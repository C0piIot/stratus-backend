package dbtest

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// RunUploads executes the resumable-upload cases against the repository built
// by newRepo.
//
// What these pin is the half of a resumable upload that has to survive a
// restart. The bytes are the blob store's problem; the row is what lets a phone
// come back tomorrow and be told where it got to.
func RunUploads(t *testing.T, newRepo func(t *testing.T) db.Repo) {
	t.Helper()

	cases := []struct {
		name string
		fn   func(t *testing.T, s db.Repo)
	}{
		{"an upload survives a round trip", uploadRoundTrip},
		{"progress replaces the row it was made on", uploadProgress},
		{"an upload belongs to its owner", uploadOwner},
		{"deleting one is idempotent", uploadDelete},
		{"expired uploads come back in order", uploadExpiry},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.fn(t, newRepo(t))
		})
	}
}

// upload is a plausible row, with every field set to something distinguishable:
// a column silently dropped on the way in or out is the bug these cases are for.
func upload(id string) db.Upload {
	return db.Upload{
		ID:       id,
		OwnerID:  owner,
		Path:     "holiday/" + id + ".mp4",
		Size:     4 << 30,
		Received: 1 << 20,
		BlobKey:  "video/2026/09/16/" + id,
		StoreID:  "the-store-calls-it-this",
		Digest:   []byte{0x00, 0x01, 0xfe, 0xff},
		MIMEType: "video/mp4",
		// Truncated because a driver is free to store milliseconds, and the
		// port's promise is the instant rather than its precision.
		ExpiresAt: time.Now().Add(time.Hour).UTC().Truncate(time.Millisecond),
	}
}

func uploadRoundTrip(t *testing.T, s db.Repo) {
	want := upload("one")
	if err := s.PutUpload(t.Context(), want); err != nil {
		t.Fatalf("PutUpload: %v", err)
	}

	got, err := s.UploadByID(t.Context(), owner, want.ID)
	if err != nil {
		t.Fatalf("UploadByID: %v", err)
	}
	if !got.ExpiresAt.Equal(want.ExpiresAt) {
		t.Errorf("ExpiresAt = %v, want %v", got.ExpiresAt, want.ExpiresAt)
	}
	if !bytes.Equal(got.Digest, want.Digest) {
		t.Errorf("Digest = %x, want %x", got.Digest, want.Digest)
	}
	// Field by field rather than whole: a struct carrying a byte slice is not
	// comparable, and the two fields that are not plain values are checked on
	// their own above.
	if got.ID != want.ID || got.OwnerID != want.OwnerID || got.Path != want.Path ||
		got.Size != want.Size || got.Received != want.Received ||
		got.BlobKey != want.BlobKey || got.StoreID != want.StoreID ||
		got.MIMEType != want.MIMEType {
		t.Errorf("UploadByID = %+v, want %+v", got, want)
	}

	if _, err := s.UploadByID(t.Context(), owner, "never-existed"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("UploadByID of a missing upload = %v, want ErrNotFound", err)
	}
}

// uploadProgress is what every chunk does: the same row, further along.
func uploadProgress(t *testing.T, s db.Repo) {
	u := upload("two")
	if err := s.PutUpload(t.Context(), u); err != nil {
		t.Fatalf("PutUpload: %v", err)
	}

	u.Received = 3 << 20
	u.Digest = []byte{0x11, 0x22}
	u.ExpiresAt = u.ExpiresAt.Add(time.Hour)
	if err := s.PutUpload(t.Context(), u); err != nil {
		t.Fatalf("PutUpload again: %v", err)
	}

	got, err := s.UploadByID(t.Context(), owner, u.ID)
	if err != nil {
		t.Fatalf("UploadByID: %v", err)
	}
	if got.Received != u.Received {
		t.Errorf("Received = %d, want %d", got.Received, u.Received)
	}
	if !bytes.Equal(got.Digest, u.Digest) {
		t.Errorf("Digest = %x, want %x", got.Digest, u.Digest)
	}
	if !got.ExpiresAt.Equal(u.ExpiresAt) {
		t.Errorf("ExpiresAt = %v, want %v", got.ExpiresAt, u.ExpiresAt)
	}
}

// uploadOwner: an upload id is a capability, and the owner is part of the
// lookup rather than a check somebody can forget to write.
func uploadOwner(t *testing.T, s db.Repo) {
	u := upload("three")
	if err := s.PutUpload(t.Context(), u); err != nil {
		t.Fatalf("PutUpload: %v", err)
	}

	if _, err := s.UploadByID(t.Context(), "somebody-else", u.ID); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("UploadByID as another owner = %v, want ErrNotFound", err)
	}
	if err := s.DeleteUpload(t.Context(), "somebody-else", u.ID); err != nil {
		t.Errorf("DeleteUpload as another owner = %v, want nil", err)
	}
	if _, err := s.UploadByID(t.Context(), owner, u.ID); err != nil {
		t.Errorf("another owner's delete took the upload: %v", err)
	}
}

func uploadDelete(t *testing.T, s db.Repo) {
	u := upload("four")
	if err := s.PutUpload(t.Context(), u); err != nil {
		t.Fatalf("PutUpload: %v", err)
	}
	if err := s.DeleteUpload(t.Context(), owner, u.ID); err != nil {
		t.Fatalf("DeleteUpload: %v", err)
	}
	// Completing and abandoning both end here, and a client that gave up twice
	// is not an error.
	if err := s.DeleteUpload(t.Context(), owner, u.ID); err != nil {
		t.Errorf("a second DeleteUpload = %v, want nil", err)
	}
	if _, err := s.UploadByID(t.Context(), owner, u.ID); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("UploadByID after a delete = %v, want ErrNotFound", err)
	}
}

func uploadExpiry(t *testing.T, s db.Repo) {
	now := time.Now().UTC()

	old := upload("old")
	old.ExpiresAt = now.Add(-2 * time.Hour)
	older := upload("older")
	older.ExpiresAt = now.Add(-3 * time.Hour)
	fresh := upload("fresh")
	fresh.ExpiresAt = now.Add(time.Hour)

	for _, u := range []db.Upload{old, older, fresh} {
		if err := s.PutUpload(t.Context(), u); err != nil {
			t.Fatalf("PutUpload(%s): %v", u.ID, err)
		}
	}

	var got []string
	for u, err := range s.ExpiredUploads(t.Context(), now) {
		if err != nil {
			t.Fatalf("ExpiredUploads: %v", err)
		}
		got = append(got, u.ID)
	}
	// Oldest first, because a collector that runs out of time should have
	// spent it on what has been dead longest.
	if len(got) != 2 || got[0] != "older" || got[1] != "old" {
		t.Errorf("ExpiredUploads = %v, want [older old]", got)
	}
}
