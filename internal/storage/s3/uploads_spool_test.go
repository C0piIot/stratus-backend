package s3_test

import (
	"os"
	"strings"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/storage/s3"
)

// TestUploadSpoolFailures covers the failures that belong to the spool rather
// than to the bucket: this backend keeps the tail of an upload on local disk,
// and the operating system refusing it is a real failure with a real message.
func TestUploadSpoolFailures(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root ignores mode bits, so this cannot fail as root")
	}

	cfg := testConfig(t)
	spool := t.TempDir()
	cfg.SpoolDir = spool
	cfg.Bucket = makeBucket(t, cfg)

	store, err := s3.New(t.Context(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const key = "video/clip.mp4"
	id, err := store.StartUpload(t.Context(), key)
	if err != nil {
		t.Fatalf("StartUpload: %v", err)
	}

	if err := os.Chmod(spool, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(spool, 0o750) })

	if _, err := store.AppendUpload(t.Context(), key, id, 0, strings.NewReader("a chunk")); err == nil {
		t.Error("AppendUpload with an unwritable spool succeeded, want an error")
	}

	// And the offset cannot be read either, which is the honest answer: this
	// backend counts what is in the spool, so a spool it cannot see is not an
	// offset of zero.
	if err := os.Chmod(spool, 0o000); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UploadOffset(t.Context(), key, id); err == nil {
		t.Error("UploadOffset with an unreadable spool succeeded, want an error")
	}
	if err := os.Chmod(spool, 0o750); err != nil {
		t.Fatal(err)
	}
}
