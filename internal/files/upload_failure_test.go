package files_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/db/dbtest"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/storage"
	"github.com/C0piIot/stratus-backend/internal/storage/storagetest"
)

// What this package does when half of a resumable upload succeeds.
//
// The same argument as failure_test.go, over more moving parts: an upload is a
// row and a growing object across two seams, and it lives for hours rather than
// for one request, so every one of these halves is reachable in practice.

// upload is a service and its two backends, with a directory to upload into.
func uploadFixture(t *testing.T) (*files.Service, storage.Storage, db.Store) {
	t.Helper()
	blobs, meta := breakable(t)
	s := files.New(blobs, meta)
	if _, err := s.Mkdir(t.Context(), owner, "holiday"); err != nil {
		t.Fatal(err)
	}
	return s, blobs, meta
}

// TestBeginUploadCleansUpWhenTheRowFails: the store has an upload nothing will
// ever name, so it goes now rather than waiting for a sweep that cannot see it.
func TestBeginUploadCleansUpWhenTheRowFails(t *testing.T) {
	t.Parallel()
	s, blobs, meta := uploadFixture(t)

	broken := files.New(blobs, dbtest.FailOn(t, meta, "PutUpload"))
	if _, err := broken.BeginUpload(t.Context(), owner, "holiday/clip.mp4", -1, "video/mp4"); !errors.Is(err, dbtest.ErrInjected) {
		t.Fatalf("BeginUpload = %v, want the injected failure", err)
	}

	for range blobs.List(t.Context(), "") {
		t.Error("the store holds an object after an upload whose row failed")
	}
	_ = s
}

// TestBeginUploadWhenTheStoreRefuses: nothing is recorded, so nothing has to be
// cleaned up.
func TestBeginUploadWhenTheStoreRefuses(t *testing.T) {
	t.Parallel()
	_, blobs, meta := uploadFixture(t)

	broken := files.New(storagetest.FailOn(t, blobs, "StartUpload"), meta)
	if _, err := broken.BeginUpload(t.Context(), owner, "holiday/clip.mp4", -1, "video/mp4"); !errors.Is(err, storagetest.ErrInjected) {
		t.Errorf("BeginUpload = %v, want the injected failure", err)
	}
	if _, err := files.New(blobs, meta).BeginUpload(t.Context(), owner, "x/y", -1, ""); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("BeginUpload into a missing directory = %v, want ErrNotFound", err)
	}
}

// TestUploadOfAnUnknownID: every call takes the id from a client, so every one
// of them has to answer for an id that is not there.
func TestUploadOfAnUnknownID(t *testing.T) {
	t.Parallel()
	s, _, _ := uploadFixture(t)

	if _, err := s.Upload(t.Context(), owner, "nope"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("Upload = %v, want ErrNotFound", err)
	}
	if _, err := s.AppendUpload(t.Context(), owner, "nope", 0, strings.NewReader("x")); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("AppendUpload = %v, want ErrNotFound", err)
	}
	if _, err := s.CompleteUpload(t.Context(), owner, "nope"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("CompleteUpload = %v, want ErrNotFound", err)
	}
	// Except the one a client calls when it has given up, which is idempotent.
	if err := s.AbortUpload(t.Context(), owner, "nope"); err != nil {
		t.Errorf("AbortUpload = %v, want nil", err)
	}
}

// TestCompleteUploadCleansUpWhenTheRowFails is the same bargain Write makes:
// the object is published and the row is not, so the object is removed by hand
// and the sweep is the backstop.
func TestCompleteUploadCleansUpWhenTheRowFails(t *testing.T) {
	t.Parallel()
	s, blobs, meta := uploadFixture(t)

	u, err := s.BeginUpload(t.Context(), owner, "holiday/clip.mp4", -1, "video/mp4")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendUpload(t.Context(), owner, u.ID, 0, bytes.NewReader([]byte("a clip"))); err != nil {
		t.Fatal(err)
	}

	broken := files.New(blobs, dbtest.FailOn(t, meta, "PutFile"))
	if _, err := broken.CompleteUpload(t.Context(), owner, u.ID); !errors.Is(err, dbtest.ErrInjected) {
		t.Fatalf("CompleteUpload = %v, want the injected failure", err)
	}
	if _, err := s.Stat(t.Context(), owner, "holiday/clip.mp4"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("Stat after a failed completion = %v, want ErrNotFound", err)
	}
	for range blobs.List(t.Context(), "") {
		t.Error("the store holds an object after a completion whose row failed")
	}
}

// TestCompleteUploadWhenTheStoreRefuses leaves the upload where it was, which is
// what lets a client try again rather than start again.
func TestCompleteUploadWhenTheStoreRefuses(t *testing.T) {
	t.Parallel()
	s, blobs, meta := uploadFixture(t)

	u, err := s.BeginUpload(t.Context(), owner, "holiday/clip.mp4", -1, "video/mp4")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendUpload(t.Context(), owner, u.ID, 0, bytes.NewReader([]byte("a clip"))); err != nil {
		t.Fatal(err)
	}

	broken := files.New(storagetest.FailOn(t, blobs, "CompleteUpload"), meta)
	if _, err := broken.CompleteUpload(t.Context(), owner, u.ID); !errors.Is(err, storagetest.ErrInjected) {
		t.Fatalf("CompleteUpload = %v, want the injected failure", err)
	}
	if _, err := s.Upload(t.Context(), owner, u.ID); err != nil {
		t.Errorf("the upload was forgotten when its completion failed: %v", err)
	}
}

// TestCompleteUploadWhenTheHashHasToBeReadBack covers the expensive path's own
// failure: the object cannot be read, so there is no ETag and no file.
func TestCompleteUploadWhenTheHashHasToBeReadBack(t *testing.T) {
	t.Parallel()
	s, blobs, meta := uploadFixture(t)

	u, err := s.BeginUpload(t.Context(), owner, "holiday/clip.mp4", -1, "video/mp4")
	if err != nil {
		t.Fatal(err)
	}
	// Break the running hash the way a store that keeps less than it read does.
	lossy := files.New(storagetest.FailOn(t, blobs, "AppendUpload"), meta)
	if _, err := lossy.AppendUpload(t.Context(), owner, u.ID, 0, bytes.NewReader([]byte("gone"))); err == nil {
		t.Fatal("AppendUpload = nil, want the injected failure")
	}
	if _, err := s.AppendUpload(t.Context(), owner, u.ID, 0, bytes.NewReader([]byte("a clip"))); err != nil {
		t.Fatal(err)
	}

	broken := files.New(storagetest.FailOn(t, blobs, "Get"), meta)
	if _, err := broken.CompleteUpload(t.Context(), owner, u.ID); !errors.Is(err, storagetest.ErrInjected) {
		t.Errorf("CompleteUpload = %v, want the injected failure from reading it back", err)
	}
}

// TestAbortUploadWhenTheStoreRefuses keeps the row: forgetting it would leave
// bytes nothing can ever name, which is the one leak this design cannot recover
// from.
func TestAbortUploadWhenTheStoreRefuses(t *testing.T) {
	t.Parallel()
	s, blobs, meta := uploadFixture(t)

	u, err := s.BeginUpload(t.Context(), owner, "holiday/clip.mp4", -1, "video/mp4")
	if err != nil {
		t.Fatal(err)
	}

	broken := files.New(storagetest.FailOn(t, blobs, "AbortUpload"), meta)
	if err := broken.AbortUpload(t.Context(), owner, u.ID); !errors.Is(err, storagetest.ErrInjected) {
		t.Fatalf("AbortUpload = %v, want the injected failure", err)
	}
	if _, err := s.Upload(t.Context(), owner, u.ID); err != nil {
		t.Errorf("the row was dropped while its bytes are still there: %v", err)
	}
}

// TestCollectUploadsStopsAtTheFirstRefusal, rather than carrying on and leaving
// a row whose bytes it could not remove.
func TestCollectUploadsStopsAtTheFirstRefusal(t *testing.T) {
	t.Parallel()
	s, blobs, meta := uploadFixture(t)

	if _, err := s.BeginUpload(t.Context(), owner, "holiday/clip.mp4", -1, "video/mp4"); err != nil {
		t.Fatal(err)
	}

	broken := files.New(storagetest.FailOn(t, blobs, "AbortUpload"), meta)
	if _, err := broken.CollectUploads(t.Context(), time.Now().Add(2*files.DefaultUploadTTL)); !errors.Is(err, storagetest.ErrInjected) {
		t.Errorf("CollectUploads = %v, want the injected failure", err)
	}
}

// TestAppendUploadWhenTheRowCannotBeSaved: the bytes are in the store and the
// row still says otherwise, which is the one disagreement the design cannot
// paper over -- so it is reported rather than swallowed, and the next request
// asks the store where it really is.
func TestAppendUploadWhenTheRowCannotBeSaved(t *testing.T) {
	t.Parallel()
	s, blobs, meta := uploadFixture(t)

	u, err := s.BeginUpload(t.Context(), owner, "holiday/clip.mp4", -1, "video/mp4")
	if err != nil {
		t.Fatal(err)
	}

	broken := files.New(blobs, dbtest.FailOn(t, meta, "PutUpload"))
	if _, err := broken.AppendUpload(t.Context(), owner, u.ID, 0, strings.NewReader("a chunk")); !errors.Is(err, dbtest.ErrInjected) {
		t.Errorf("AppendUpload = %v, want the injected failure", err)
	}
}

// TestAbortUploadWhenTheRowCannotBeDeleted leaves the row, which the collector
// will come back to.
func TestAbortUploadWhenTheRowCannotBeDeleted(t *testing.T) {
	t.Parallel()
	s, blobs, meta := uploadFixture(t)

	u, err := s.BeginUpload(t.Context(), owner, "holiday/clip.mp4", -1, "video/mp4")
	if err != nil {
		t.Fatal(err)
	}

	broken := files.New(blobs, dbtest.FailOn(t, meta, "DeleteUpload"))
	if err := broken.AbortUpload(t.Context(), owner, u.ID); !errors.Is(err, dbtest.ErrInjected) {
		t.Errorf("AbortUpload = %v, want the injected failure", err)
	}
}

// TestCollectUploadsReportsAListingThatFails, rather than treating an error
// halfway through as the end of the list and calling the sweep a success.
func TestCollectUploadsReportsAListingThatFails(t *testing.T) {
	t.Parallel()
	_, blobs, meta := uploadFixture(t)

	broken := files.New(blobs, dbtest.FailOn(t, meta, "ExpiredUploads"))
	if _, err := broken.CollectUploads(t.Context(), time.Now()); !errors.Is(err, dbtest.ErrInjected) {
		t.Errorf("CollectUploads = %v, want the injected failure", err)
	}
}

// TestCollectUploadsWhenTheRowCannotBeDeleted: the bytes are gone and the row
// is not, so it is reported and the next sweep finishes the job.
func TestCollectUploadsWhenTheRowCannotBeDeleted(t *testing.T) {
	t.Parallel()
	s, blobs, meta := uploadFixture(t)

	if _, err := s.BeginUpload(t.Context(), owner, "holiday/clip.mp4", -1, "video/mp4"); err != nil {
		t.Fatal(err)
	}

	broken := files.New(blobs, dbtest.FailOn(t, meta, "DeleteUpload"))
	if _, err := broken.CollectUploads(t.Context(), time.Now().Add(2*files.DefaultUploadTTL)); !errors.Is(err, dbtest.ErrInjected) {
		t.Errorf("CollectUploads = %v, want the injected failure", err)
	}
}

// TestBeginUploadValidatesThePath: the same chokepoint every other write goes
// through, because an upload is a write that has not happened yet.
func TestBeginUploadValidatesThePath(t *testing.T) {
	t.Parallel()
	s, _, _ := uploadFixture(t)

	if _, err := s.BeginUpload(t.Context(), owner, "../escape", -1, "video/mp4"); !errors.Is(err, db.ErrInvalidPath) {
		t.Errorf("BeginUpload = %v, want ErrInvalidPath", err)
	}
}
