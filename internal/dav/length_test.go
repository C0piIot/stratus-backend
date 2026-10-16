package dav_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/dav"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/files/filestest"
	"github.com/C0piIot/stratus-backend/internal/storage"
)

// sizes records what the store is told about each body's length, which is the
// one thing about a PUT the library does not pass on (#348).
type sizes struct {
	storage.Storage
	told []int64
}

func (s *sizes) Put(ctx context.Context, key string, r io.Reader, size int64) (storage.ObjectInfo, error) {
	s.told = append(s.told, size)
	return s.Storage.Put(ctx, key, r, size)
}

// TestAPutReachesTheStoreWithItsLength: a client that sent a Content-Length
// is believed, one that sent a chunked body is an unknown length, and an
// empty body is a length of nothing rather than of unknown. On S3 the first is
// one request with no buffer and the second is a part held in memory, so the
// difference is the whole of #348.
func TestAPutReachesTheStoreWithItsLength(t *testing.T) {
	t.Parallel()
	blobs, meta := filestest.Backends(t)
	store := &sizes{Storage: blobs}
	h := withUser(dav.Handler(prefix, files.New(store, meta), meta), "edu")

	put := func(target string, body io.Reader, length int64) {
		t.Helper()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, target, body)
		req.ContentLength = length
		if length < 0 {
			req.TransferEncoding = []string{"chunked"}
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("PUT %s: %d %s", target, rec.Code, rec.Body)
		}
	}

	put("/files/declared.txt", strings.NewReader("twelve bytes"), 12)
	put("/files/chunked.txt", strings.NewReader("sent in pieces"), -1)
	put("/files/empty.txt", http.NoBody, 0)

	want := []int64{12, -1, 0}
	if len(store.told) != len(want) {
		t.Fatalf("the store was told %v, want %v", store.told, want)
	}
	for i, size := range want {
		if store.told[i] != size {
			t.Errorf("PUT %d told the store %d, want %d", i, store.told[i], size)
		}
	}
}
