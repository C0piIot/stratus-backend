package storage_test

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/storage"
)

func TestExactReader(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		size int64
		want error
	}{
		{name: "exact", body: "twelve bytes", size: 12},
		{name: "empty", body: "", size: 0},
		{name: "unknown size passes anything through", body: "whatever", size: -1},
		{name: "short", body: "short", size: 100, want: storage.ErrSizeMismatch},
		{name: "long", body: "much longer than declared", size: 4, want: storage.ErrSizeMismatch},
		{name: "one byte long", body: "abcde", size: 4, want: storage.ErrSizeMismatch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := io.ReadAll(storage.ExactReader(strings.NewReader(tt.body), tt.size))
			if !errors.Is(err, tt.want) {
				t.Fatalf("ReadAll = %v, want %v", err, tt.want)
			}
			if tt.want == nil && string(got) != tt.body {
				t.Errorf("read %q, want %q", got, tt.body)
			}
		})
	}
}

// TestExactReaderStopsAtTheLimit pins the property S3 depends on: the wrapper
// never hands more than the declared number of bytes to the writer, so a
// truncating client cannot turn an over-long body into a successful short
// upload.
func TestExactReaderStopsAtTheLimit(t *testing.T) {
	t.Parallel()
	var sink strings.Builder
	_, err := io.Copy(&sink, storage.ExactReader(strings.NewReader("abcdefgh"), 3))

	if !errors.Is(err, storage.ErrSizeMismatch) {
		t.Fatalf("Copy = %v, want ErrSizeMismatch", err)
	}
	if sink.String() != "abc" {
		t.Errorf("wrote %q, want the first three bytes and nothing more", sink.String())
	}
}

// TestExactReaderPropagatesReadErrors makes sure a real failure is not
// relabelled as a size problem.
func TestExactReaderPropagatesReadErrors(t *testing.T) {
	t.Parallel()
	want := errors.New("disk on fire")
	_, err := io.ReadAll(storage.ExactReader(errReader{want}, 10))
	if !errors.Is(err, want) {
		t.Errorf("ReadAll = %v, want %v", err, want)
	}
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }
