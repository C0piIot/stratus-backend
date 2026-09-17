package files_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/storage"
	"github.com/C0piIot/stratus-backend/internal/storage/storagetest"
)

// begin is the upload every case here starts from.
func begin(t *testing.T, s *files.Service, path string, size int64) db.Upload {
	t.Helper()
	if dir, _, ok := strings.Cut(path, "/"); ok {
		if _, err := s.Mkdir(t.Context(), owner, dir); err != nil && !errors.Is(err, db.ErrConflict) {
			t.Fatalf("Mkdir(%q): %v", dir, err)
		}
	}
	u, err := s.BeginUpload(t.Context(), owner, path, size, "video/mp4")
	if err != nil {
		t.Fatalf("BeginUpload(%q): %v", path, err)
	}
	return u
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// TestUploadInPieces is the whole point: a file that arrives over several
// requests is the same file as one that arrives in a single PUT, ETag included.
func TestUploadInPieces(t *testing.T) {
	t.Parallel()
	s, _ := service(t)
	body := bytes.Repeat([]byte("a video, allegedly. "), 1000)

	u := begin(t, s, "holiday/clip.mp4", int64(len(body)))
	half := len(body) / 2
	if u = appendAt(t, s, u.ID, 0, body[:half]); u.Received != int64(half) {
		t.Fatalf("Received = %d, want %d", u.Received, half)
	}
	if u = appendAt(t, s, u.ID, int64(half), body[half:]); u.Received != int64(len(body)) {
		t.Fatalf("Received = %d, want %d", u.Received, len(body))
	}

	f, err := s.CompleteUpload(t.Context(), owner, u.ID)
	if err != nil {
		t.Fatalf("CompleteUpload: %v", err)
	}
	if f.Size != int64(len(body)) {
		t.Errorf("Size = %d, want %d", f.Size, len(body))
	}
	// The strong validator, and the reason the hash is carried between
	// requests: it is the digest of the content, exactly as a single PUT would
	// have produced.
	if f.ETag != sha256Hex(body) {
		t.Errorf("ETag = %q, want the sha256 of the content", f.ETag)
	}
	if got := read(t, s, "holiday/clip.mp4"); got != string(body) {
		t.Errorf("the stored file is %d bytes, want %d", len(got), len(body))
	}
	// The upload is over, so nothing should still be holding it.
	if _, err := s.Upload(t.Context(), owner, u.ID); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("the upload outlived its file: %v", err)
	}
}

// appendAt fails the test rather than returning an error: every caller here
// expects the append to land, and the ones that do not call the service directly.
func appendAt(t *testing.T, s *files.Service, id string, at int64, body []byte) db.Upload {
	t.Helper()
	u, err := s.AppendUpload(t.Context(), owner, id, at, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("AppendUpload at %d: %v", at, err)
	}
	return u
}

// TestUploadResumesAfterEverythingForgets covers the case the feature exists
// for: nothing is held in memory between requests, so the upload can be picked
// up from the row alone.
func TestUploadResumesAfterEverythingForgets(t *testing.T) {
	t.Parallel()
	s, _ := service(t)
	body := []byte("one two three four five")

	u := begin(t, s, "notes.txt", int64(len(body)))
	appendAt(t, s, u.ID, 0, body[:7])

	// What a second request knows: an id and nothing else.
	resumed, err := s.Upload(t.Context(), owner, u.ID)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if resumed.Received != 7 {
		t.Fatalf("Received = %d, want 7", resumed.Received)
	}
	appendAt(t, s, resumed.ID, resumed.Received, body[7:])

	f, err := s.CompleteUpload(t.Context(), owner, u.ID)
	if err != nil {
		t.Fatalf("CompleteUpload: %v", err)
	}
	if f.ETag != sha256Hex(body) {
		t.Errorf("ETag = %q, want the sha256 of the content", f.ETag)
	}
}

// TestUploadRefusesTheWrongOffset: a client that resends a chunk it already
// sent must be told, not obeyed.
func TestUploadRefusesTheWrongOffset(t *testing.T) {
	t.Parallel()
	s, _ := service(t)

	u := begin(t, s, "notes.txt", 10)
	appendAt(t, s, u.ID, 0, []byte("12345"))

	if _, err := s.AppendUpload(t.Context(), owner, u.ID, 0, strings.NewReader("12345")); !errors.Is(err, storage.ErrUploadOffset) {
		t.Errorf("a repeated append = %v, want ErrUploadOffset", err)
	}
	if _, err := s.AppendUpload(t.Context(), owner, u.ID, 99, strings.NewReader("x")); !errors.Is(err, storage.ErrUploadOffset) {
		t.Errorf("an append past the end = %v, want ErrUploadOffset", err)
	}
}

// TestUploadRefusesToCompleteShort: the declared length is a promise, and a
// file that is short is a broken upload rather than a small file.
func TestUploadRefusesToCompleteShort(t *testing.T) {
	t.Parallel()
	s, _ := service(t)

	u := begin(t, s, "notes.txt", 100)
	appendAt(t, s, u.ID, 0, []byte("only this much"))

	if _, err := s.CompleteUpload(t.Context(), owner, u.ID); !errors.Is(err, db.ErrConflict) {
		t.Errorf("CompleteUpload of a short upload = %v, want ErrConflict", err)
	}
}

// TestUploadWithNoDeclaredLength is the client that does not know how big the
// file is until it stops, which tus allows.
func TestUploadWithNoDeclaredLength(t *testing.T) {
	t.Parallel()
	s, _ := service(t)

	u := begin(t, s, "notes.txt", -1)
	appendAt(t, s, u.ID, 0, []byte("as much as there was"))

	f, err := s.CompleteUpload(t.Context(), owner, u.ID)
	if err != nil {
		t.Fatalf("CompleteUpload: %v", err)
	}
	if f.Size != 20 {
		t.Errorf("Size = %d, want 20", f.Size)
	}
}

// TestUploadAfterABrokenConnection pins the common case, which turns out to be
// the cheap one: io.Copy writes what it reads, so a client that disappears
// mid-chunk leaves the store holding exactly the bytes the hash covers. The
// running hash is still good and the ETag is the digest of the content.
func TestUploadAfterABrokenConnection(t *testing.T) {
	t.Parallel()
	s, _ := service(t)

	u := begin(t, s, "holiday/clip.mp4", -1)
	broken := errors.New("the connection went away")
	sent := bytes.Repeat([]byte{7}, 4096)
	if _, err := s.AppendUpload(t.Context(), owner, u.ID, 0,
		&halfReader{body: bytes.NewReader(sent), err: broken}); err == nil {
		t.Fatal("AppendUpload = nil, want the reader's error")
	}

	u, err := s.Upload(t.Context(), owner, u.ID)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if u.Received != int64(len(sent)) {
		t.Errorf("Received = %d, want all %d bytes: the copy wrote what it read", u.Received, len(sent))
	}
	if len(u.Digest) == 0 {
		t.Error("the running hash was dropped when it did not have to be")
	}

	rest := []byte("the rest of it")
	appendAt(t, s, u.ID, u.Received, rest)

	f, err := s.CompleteUpload(t.Context(), owner, u.ID)
	if err != nil {
		t.Fatalf("CompleteUpload: %v", err)
	}
	if f.ETag != sha256Hex(append(sent, rest...)) {
		t.Errorf("ETag = %q, which is not the digest of what was stored", f.ETag)
	}
}

// halfReader delivers its body and then fails, which is a client walking out of
// range with the request still open.
type halfReader struct {
	body io.Reader
	err  error
}

func (r *halfReader) Read(p []byte) (int, error) {
	n, err := r.body.Read(p)
	if errors.Is(err, io.EOF) {
		return n, r.err
	}
	return n, err
}

// TestUploadWhenTheStoreKeepsLess is the expensive path, and it takes a store
// that fails to reach: the hash covers bytes the store did not keep, and a hash
// cannot be wound back, so it is dropped and the object is read back to be
// hashed at the end. The ETag has to come out the same either way, or an
// interrupted upload would give identical bytes a different validator.
func TestUploadWhenTheStoreKeepsLess(t *testing.T) {
	t.Parallel()
	blobs, meta := breakable(t)
	body := []byte("the bytes that did not land")

	working := files.New(blobs, meta)
	if _, err := working.Mkdir(t.Context(), owner, "holiday"); err != nil {
		t.Fatal(err)
	}
	u, err := working.BeginUpload(t.Context(), owner, "holiday/clip.mp4", -1, "video/mp4")
	if err != nil {
		t.Fatalf("BeginUpload: %v", err)
	}

	broken := files.New(storagetest.FailOn(t, blobs, "AppendUpload"), meta)
	if _, aerr := broken.AppendUpload(t.Context(), owner, u.ID, 0, bytes.NewReader(body)); !errors.Is(aerr, storagetest.ErrInjected) {
		t.Fatalf("AppendUpload = %v, want the injected failure", aerr)
	}

	after, err := working.Upload(t.Context(), owner, u.ID)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if len(after.Digest) != 0 {
		t.Error("the running hash survived a chunk the store did not keep")
	}

	// It carries on from where the store really is, and the ETag is computed
	// the slow way.
	if _, aerr := working.AppendUpload(t.Context(), owner, u.ID, after.Received, bytes.NewReader(body)); aerr != nil {
		t.Fatalf("AppendUpload: %v", aerr)
	}
	f, err := working.CompleteUpload(t.Context(), owner, u.ID)
	if err != nil {
		t.Fatalf("CompleteUpload: %v", err)
	}
	if f.ETag != sha256Hex(body) {
		t.Errorf("ETag = %q, want the sha256 of what was stored", f.ETag)
	}
}

// TestAbortUpload leaves nothing: not the row, not the bytes, and not an
// error when it is done twice.
func TestAbortUpload(t *testing.T) {
	t.Parallel()
	s, blobs := service(t)

	u := begin(t, s, "holiday/clip.mp4", -1)
	appendAt(t, s, u.ID, 0, []byte("half a video"))

	if err := s.AbortUpload(t.Context(), owner, u.ID); err != nil {
		t.Fatalf("AbortUpload: %v", err)
	}
	if err := s.AbortUpload(t.Context(), owner, u.ID); err != nil {
		t.Errorf("a second AbortUpload = %v, want nil", err)
	}
	if _, err := s.Upload(t.Context(), owner, u.ID); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("Upload after an abort = %v, want ErrNotFound", err)
	}
	var found int
	for range blobs.List(t.Context(), "") {
		found++
	}
	if found != 0 {
		t.Errorf("an aborted upload left %d objects behind", found)
	}
}

// TestBeginUploadNeedsItsParent: a client that mistyped a directory learns now
// rather than after an hour of uploading into it.
func TestBeginUploadNeedsItsParent(t *testing.T) {
	t.Parallel()
	s, _ := service(t)

	if _, err := s.BeginUpload(t.Context(), owner, "missing/clip.mp4", 10, "video/mp4"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("BeginUpload into a missing directory = %v, want ErrNotFound", err)
	}
}

// TestCollectUploads is the only thing that ever cleans these up: an upload in
// flight is invisible to a listing, which keeps the blob sweep off it and means
// the blob sweep will never tidy it away either.
func TestCollectUploads(t *testing.T) {
	t.Parallel()
	s, blobs := service(t)

	stale := begin(t, s, "holiday/old.mp4", -1)
	appendAt(t, s, stale.ID, 0, []byte("abandoned"))
	fresh := begin(t, s, "holiday/new.mp4", -1)

	// Everything whose deadline has passed, which for an upload made now means
	// asking about a moment after the TTL.
	done, err := s.CollectUploads(t.Context(), time.Now().Add(files.DefaultUploadTTL+time.Minute))
	if err != nil {
		t.Fatalf("CollectUploads: %v", err)
	}
	if done != 2 {
		t.Errorf("collected %d uploads, want 2", done)
	}
	if _, err := s.Upload(t.Context(), owner, stale.ID); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("a collected upload is still there: %v", err)
	}
	if _, err := s.Upload(t.Context(), owner, fresh.ID); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("a collected upload is still there: %v", err)
	}
	var found int
	for range blobs.List(t.Context(), "") {
		found++
	}
	if found != 0 {
		t.Errorf("collecting left %d objects behind", found)
	}

	// And one that is not due is left alone.
	keep := begin(t, s, "holiday/keep.mp4", -1)
	if done, err := s.CollectUploads(t.Context(), time.Now()); err != nil || done != 0 {
		t.Errorf("CollectUploads = %d, %v, want 0, nil", done, err)
	}
	if _, err := s.Upload(t.Context(), owner, keep.ID); err != nil {
		t.Errorf("a live upload was collected: %v", err)
	}
}
