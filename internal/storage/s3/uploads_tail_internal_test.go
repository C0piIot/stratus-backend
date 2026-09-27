package s3

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// TestAnUploadIsResumedThroughAnotherProcess is what having no spool is for:
// every byte an upload has accepted is in the bucket, so a second process --
// another instance, or the same one after a restart with no volume -- sees the
// same offset and finishes the upload the first one started.
func TestAnUploadIsResumedThroughAnotherProcess(t *testing.T) {
	t.Parallel()
	client, bucket := tailBucket(t)
	first := &Store{client: client, core: minio.Core{Client: client}, bucket: bucket, partSize: minPartSize}
	second := &Store{client: client, core: minio.Core{Client: client}, bucket: bucket, partSize: minPartSize}

	const key = "video/moved.mp4"
	id, err := first.StartUpload(t.Context(), key)
	if err != nil {
		t.Fatalf("StartUpload: %v", err)
	}

	// Twelve megabytes in one request over five-megabyte parts: two parts, and
	// two megabytes left as the tail.
	body := bytes.Repeat([]byte{1}, 12<<20)
	at, err := first.AppendUpload(t.Context(), key, id, 0, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("AppendUpload: %v", err)
	}
	if at != int64(len(body)) {
		t.Fatalf("offset = %d, want %d", at, len(body))
	}
	if parts, _, perr := first.parts(t.Context(), key, id); perr != nil || len(parts) != 2 {
		t.Errorf("parts = %d, %v, want 2: one request is not one part", len(parts), perr)
	}

	if got, oerr := second.UploadOffset(t.Context(), key, id); oerr != nil || got != at {
		t.Fatalf("UploadOffset through the other process = %d, %v, want %d", got, oerr, at)
	}

	// The other process carries on over a connection that drops, and keeps
	// what it read.
	rest := bytes.Repeat([]byte{2}, 1<<20)
	broken := errors.New("the connection went away")
	end, err := second.AppendUpload(t.Context(), key, id, at, io.MultiReader(bytes.NewReader(rest), failing{broken}))
	if !errors.Is(err, broken) {
		t.Errorf("AppendUpload = %v, want the reader's error", err)
	}
	if want := at + int64(len(rest)); end != want {
		t.Errorf("offset after a broken append = %d, want %d", end, want)
	}

	if _, err = second.CompleteUpload(t.Context(), key, id); err != nil {
		t.Fatalf("CompleteUpload: %v", err)
	}
	obj, err := client.GetObject(t.Context(), bucket, key, minio.GetObjectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(obj)
	_ = obj.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, append(bytes.Clone(body), rest...)) {
		t.Errorf("the completed object is %d bytes and not what was appended", len(got))
	}
	if tails := listed(t, client, bucket, tailPrefix); len(tails) != 0 {
		t.Errorf("a completed upload left its tails behind: %v", tails)
	}
}

// TestAbortUploadsBeforeRemovesTails: an upload the sweep aborts leaves its
// tail, which is billed like any other object and which nothing else will
// ever look for.
func TestAbortUploadsBeforeRemovesTails(t *testing.T) {
	t.Parallel()
	client, bucket := tailBucket(t)
	store := &Store{client: client, core: minio.Core{Client: client}, bucket: bucket, partSize: minPartSize}

	tail := tailKey("abandoned", 1)
	if _, err := client.PutObject(t.Context(), bucket, tail, strings.NewReader("half a part"), 11, minio.PutObjectOptions{}); err != nil {
		t.Fatal(err)
	}

	if err := store.abortUploadsBefore(t.Context(), time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("abortUploadsBefore: %v", err)
	}
	if got := listed(t, client, bucket, tailPrefix); len(got) != 1 {
		t.Fatalf("a tail from a moment ago was removed: %v", got)
	}

	if err := store.abortUploadsBefore(t.Context(), time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("abortUploadsBefore: %v", err)
	}
	if got := listed(t, client, bucket, tailPrefix); len(got) != 0 {
		t.Errorf("the stale tail survived the sweep: %v", got)
	}
}

type failing struct{ err error }

func (f failing) Read([]byte) (int, error) { return 0, f.err }

func tailBucket(t *testing.T) (*minio.Client, string) {
	t.Helper()
	endpoint := os.Getenv("STRATUS_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("STRATUS_TEST_S3_ENDPOINT is not set; `make test-s3` starts Silo and sets it")
	}
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(os.Getenv("STRATUS_TEST_S3_ACCESS_KEY"), os.Getenv("STRATUS_TEST_S3_SECRET_KEY"), ""),
		Region: "us-east-1",
	})
	if err != nil {
		t.Fatal(err)
	}

	bucket := "stratus-test-tails-" + strings.ToLower(rand.Text()[:16])
	if err := client.MakeBucket(t.Context(), bucket, minio.MakeBucketOptions{Region: "us-east-1"}); err != nil {
		t.Fatalf("MakeBucket: %v", err)
	}
	t.Cleanup(func() {
		// t.Context() is already cancelled by the time cleanup runs.
		ctx := context.WithoutCancel(t.Context())
		for upload := range client.ListIncompleteUploads(ctx, bucket, "", true) {
			_ = client.RemoveIncompleteUpload(ctx, bucket, upload.Key)
		}
		for oi := range client.ListObjects(ctx, bucket, minio.ListObjectsOptions{Recursive: true}) {
			_ = client.RemoveObject(ctx, bucket, oi.Key, minio.RemoveObjectOptions{})
		}
		_ = client.RemoveBucket(ctx, bucket)
	})
	return client, bucket
}

func listed(t *testing.T, client *minio.Client, bucket, prefix string) []string {
	t.Helper()
	var out []string
	for oi := range client.ListObjects(t.Context(), bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if oi.Err != nil {
			t.Fatalf("ListObjects: %v", oi.Err)
		}
		out = append(out, oi.Key)
	}
	return out
}
