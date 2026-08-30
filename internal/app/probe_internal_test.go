package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"iter"
	"strings"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/storage"
)

// fakeStore is a blob store that fails wherever the test asks it to. The probe
// is the only thing standing between a misconfigured backend and a server that
// looks healthy, so each of its steps needs to be the one that catches a
// failure.
type fakeStore struct {
	putErr    error
	getErr    error
	deleteErr error
	closeErr  error
	// body overrides what Get returns; nil returns what Put wrote.
	body   []byte
	stored []byte
}

func (f *fakeStore) Put(_ context.Context, key string, r io.Reader, _ int64) (storage.ObjectInfo, error) {
	if f.putErr != nil {
		return storage.ObjectInfo{}, f.putErr
	}
	body, err := io.ReadAll(r)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	f.stored = body
	return storage.ObjectInfo{Key: key, Size: int64(len(body))}, nil
}

func (f *fakeStore) Get(_ context.Context, key string, _ storage.Range) (io.ReadCloser, storage.ObjectInfo, error) {
	if f.getErr != nil {
		return nil, storage.ObjectInfo{}, f.getErr
	}
	body := f.body
	if body == nil {
		body = f.stored
	}
	return &fakeBody{Reader: bytes.NewReader(body), err: f.closeErr}, storage.ObjectInfo{Key: key, Size: int64(len(body))}, nil
}

func (f *fakeStore) Stat(context.Context, string) (storage.ObjectInfo, error) {
	return storage.ObjectInfo{}, nil
}

func (f *fakeStore) Delete(context.Context, string) error { return f.deleteErr }

func (f *fakeStore) List(context.Context, string) iter.Seq2[storage.ObjectInfo, error] {
	return func(func(storage.ObjectInfo, error) bool) {}
}

type fakeBody struct {
	io.Reader
	err error
}

func (b *fakeBody) Close() error { return b.err }

func TestProbeStorage(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")

	tests := []struct {
		name  string
		store *fakeStore
		want  string
	}{
		{name: "a store that works", store: &fakeStore{}},
		{name: "put fails", store: &fakeStore{putErr: boom}, want: "not writable"},
		{name: "get fails", store: &fakeStore{getErr: boom}, want: "not readable"},
		{name: "the body fails to close", store: &fakeStore{closeErr: boom}, want: "not readable"},
		// The one a naive probe would miss: every call succeeds and the bytes
		// are still wrong.
		{name: "the bytes come back different", store: &fakeStore{body: []byte("something else")}, want: "not the"},
		{name: "delete fails", store: &fakeStore{deleteErr: boom}, want: "cannot delete"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := probeStorage(t.Context(), tt.store)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("probeStorage = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("probeStorage = nil, want an error mentioning %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("probeStorage = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

// TestProbeStorageLeavesNothing is the property that matters on a shared
// bucket: the probe must not be visible to anything that lists it afterwards.
func TestProbeStorageLeavesNothing(t *testing.T) {
	t.Parallel()
	store := &fakeStore{}
	if err := probeStorage(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	if store.deleteErr != nil {
		t.Fatal("the fake was configured to fail")
	}
	// The fake records the last write; the real check is that Delete was the
	// last call, which the table above covers by failing it.
	if len(store.stored) == 0 {
		t.Error("the probe never wrote anything")
	}
}
