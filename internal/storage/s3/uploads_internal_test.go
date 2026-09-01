package s3

import (
	"crypto/rand"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// TestAbortUploadsBefore covers the sweep that runs on every connect. An
// abandoned multipart upload is invisible to a listing and billed until
// something aborts it, so nothing else would ever notice it was there.
func TestAbortUploadsBefore(t *testing.T) {
	t.Parallel()
	endpoint := os.Getenv("STRATUS_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("STRATUS_TEST_S3_ENDPOINT is not set; `make test-s3` starts MinIO and sets it")
	}

	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(os.Getenv("STRATUS_TEST_S3_ACCESS_KEY"), os.Getenv("STRATUS_TEST_S3_SECRET_KEY"), ""),
		Region: "us-east-1",
	})
	if err != nil {
		t.Fatal(err)
	}

	bucket := "stratus-test-uploads-" + strings.ToLower(rand.Text()[:16])
	if merr := client.MakeBucket(t.Context(), bucket, minio.MakeBucketOptions{Region: "us-east-1"}); merr != nil {
		t.Fatalf("MakeBucket: %v", merr)
	}
	t.Cleanup(func() { _ = client.RemoveBucket(t.Context(), bucket) })

	// Start a multipart upload and walk away from it, which is what a killed
	// process leaves behind.
	core := minio.Core{Client: client}
	id, err := core.NewMultipartUpload(t.Context(), bucket, "abandoned", minio.PutObjectOptions{})
	if err != nil {
		t.Fatalf("NewMultipartUpload: %v", err)
	}
	if id == "" {
		t.Fatal("no upload id")
	}

	store := &Store{client: client, bucket: bucket}

	// A cutoff in the past leaves it alone: aborting somebody else's upload in
	// progress would be worse than paying for a day of orphaned parts.
	if err := store.abortUploadsBefore(t.Context(), time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("abortUploadsBefore: %v", err)
	}
	if !hasIncompleteUpload(t, client, bucket) {
		t.Fatal("an upload from a moment ago was aborted; the cutoff is not being respected")
	}

	if err := store.abortUploadsBefore(t.Context(), time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("abortUploadsBefore: %v", err)
	}
	if hasIncompleteUpload(t, client, bucket) {
		t.Error("the abandoned upload survived the sweep")
	}
}

func hasIncompleteUpload(t *testing.T, client *minio.Client, bucket string) bool {
	t.Helper()
	var found bool
	for upload := range client.ListIncompleteUploads(t.Context(), bucket, "", true) {
		if upload.Err != nil {
			t.Fatalf("ListIncompleteUploads: %v", upload.Err)
		}
		found = true
	}
	return found
}
