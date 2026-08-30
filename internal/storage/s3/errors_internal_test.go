package s3

import (
	"errors"
	"fmt"
	"testing"

	"github.com/minio/minio-go/v7"

	"github.com/C0piIot/stratus-backend/internal/storage"
)

// TestIsNotFound covers the mapping without a server, including the case that
// matters most: a bucket that has vanished must not read as an empty library.
func TestIsNotFound(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"no such key, from a GET", minio.ErrorResponse{Code: "NoSuchKey"}, true},
		{"not found, from a HEAD", minio.ErrorResponse{Code: "NotFound"}, true},
		{"wrapped", fmt.Errorf("get: %w", minio.ErrorResponse{Code: "NoSuchKey"}), true},
		{"a missing bucket is not a missing object", minio.ErrorResponse{Code: "NoSuchBucket"}, false},
		{"access denied", minio.ErrorResponse{Code: "AccessDenied"}, false},
		{"a plain error", errors.New("connection refused"), false},
		{"no error", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isNotFound(tt.err); got != tt.want {
				t.Errorf("isNotFound(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestMapErr(t *testing.T) {
	t.Parallel()

	if err := mapErr("k", minio.ErrorResponse{Code: "NoSuchKey"}); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("mapErr = %v, want ErrNotFound", err)
	}

	// Anything else has to come back with its chain intact, so a caller can
	// still tell a timeout from a permission problem.
	want := errors.New("boom")
	if err := mapErr("k", fmt.Errorf("put: %w", want)); !errors.Is(err, want) {
		t.Errorf("mapErr = %v, want the original error", err)
	}
}
