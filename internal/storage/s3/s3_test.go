package s3_test

import (
	"context"
	"crypto/rand"
	"os"
	"strings"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/C0piIot/stratus-backend/internal/storage"
	"github.com/C0piIot/stratus-backend/internal/storage/s3"
	"github.com/C0piIot/stratus-backend/internal/storage/storagetest"
)

const (
	endpointEnv = "STRATUS_TEST_S3_ENDPOINT"
	accessEnv   = "STRATUS_TEST_S3_ACCESS_KEY"
	secretEnv   = "STRATUS_TEST_S3_SECRET_KEY"
)

// TestConformance is why this package exists: the same suite the disk backend
// passes, against a real S3 API.
func TestConformance(t *testing.T) {
	t.Parallel()
	storagetest.Run(t, newStore)
}

// newStore gives every case its own bucket. That is the isolation the suite
// asks for, and it is why this backend needs no notion of a prefix inside a
// bucket to be testable.
func newStore(t *testing.T) storage.Storage {
	t.Helper()
	cfg := testConfig(t)
	cfg.Bucket = makeBucket(t, cfg)

	store, err := s3.New(t.Context(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return store
}

// testConfig skips the test when there is nothing to talk to. The message is
// explicit on purpose: a suite that skips silently stops being a suite.
func testConfig(t *testing.T) s3.Config {
	t.Helper()
	endpoint := os.Getenv(endpointEnv)
	if endpoint == "" {
		t.Skipf("%s is not set; `make test-s3` starts MinIO and sets it", endpointEnv)
	}
	return s3.Config{
		Endpoint:  endpoint,
		AccessKey: os.Getenv(accessEnv),
		SecretKey: os.Getenv(secretEnv),
		Region:    "us-east-1",
	}
}

func makeBucket(t *testing.T, cfg s3.Config) string {
	t.Helper()
	client := rawClient(t, cfg)

	// Bucket names are lowercase; base32 from crypto/rand lowercases into
	// exactly the allowed alphabet.
	name := "stratus-test-" + strings.ToLower(rand.Text()[:16])
	if err := client.MakeBucket(t.Context(), name, minio.MakeBucketOptions{Region: cfg.Region}); err != nil {
		t.Fatalf("MakeBucket(%s): %v", name, err)
	}

	t.Cleanup(func() {
		// t.Context() is already cancelled by the time cleanup runs, and a
		// cancelled context cannot delete anything.
		ctx := context.WithoutCancel(t.Context())
		for oi := range client.ListObjects(ctx, name, minio.ListObjectsOptions{Recursive: true}) {
			if oi.Err != nil {
				t.Errorf("drain %s: %v", name, oi.Err)
				return
			}
			if err := client.RemoveObject(ctx, name, oi.Key, minio.RemoveObjectOptions{}); err != nil {
				t.Errorf("remove %s/%s: %v", name, oi.Key, err)
			}
		}
		if err := client.RemoveBucket(ctx, name); err != nil {
			t.Errorf("RemoveBucket(%s): %v", name, err)
		}
	})
	return name
}

func rawClient(t *testing.T, cfg s3.Config) *minio.Client {
	t.Helper()
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Region: cfg.Region,
	})
	if err != nil {
		t.Fatalf("minio.New: %v", err)
	}
	return client
}

// TestNewValidatesConfig needs no server: New has to reject an unusable config
// before it dials anything.
func TestNewValidatesConfig(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cfg  s3.Config
	}{
		{"no endpoint", s3.Config{Bucket: "b", AccessKey: "k", SecretKey: "s"}},
		{"no bucket", s3.Config{Endpoint: "localhost:9000", AccessKey: "k", SecretKey: "s"}},
		{"no access key", s3.Config{Endpoint: "localhost:9000", Bucket: "b", SecretKey: "s"}},
		{"no secret key", s3.Config{Endpoint: "localhost:9000", Bucket: "b", AccessKey: "k"}},
		{"nothing at all", s3.Config{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := s3.New(t.Context(), tt.cfg); err == nil {
				t.Error("New = nil, want an error")
			}
		})
	}
}

// TestNewRejectsAMissingBucket is the startup check that keeps a
// misconfiguration from surfacing on somebody's first upload.
func TestNewRejectsAMissingBucket(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	cfg.Bucket = "stratus-test-" + strings.ToLower(rand.Text()[:16])

	if _, err := s3.New(t.Context(), cfg); err == nil {
		t.Error("New on a bucket that does not exist = nil, want an error")
	}
}
