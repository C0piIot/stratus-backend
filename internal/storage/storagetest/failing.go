package storagetest

import (
	"context"
	"errors"
	"io"
	"iter"
	"slices"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/storage"
)

// A blob store that fails one call, for the branches a working one will not
// take on request.
//
// It lives here, beside the conformance suite, because both are test machinery
// for the same port and this package is already outside the coverage gate. And
// it wraps a real backend rather than replacing one: what the failure cases are
// about is what happens *around* the failure -- a blob that was written before
// the row was not, a thumbnail served after it could not be stored -- so
// everything except the one call has to behave.
//
// It is deliberately not the answer to every low floor. A backend fails because
// the operating system or the network refused, and a wrapper around this port
// cannot help the packages that implement it: see internal/storage/disk, whose
// own failures are provoked with an awkward path and a chmod.

// ErrInjected is what a failing call returns. Tests match on it so that a real
// failure arriving by surprise is not mistaken for the injected one.
var ErrInjected = errors.New("storagetest: injected failure")

// storageMethods is every call Failing can be asked to break. Naming one that
// is not here is a test that would silently never fail, so FailOn refuses it.
var storageMethods = []string{"Put", "Get", "Delete", "Stat", "List"}

// Failing is a storage.Storage that fails one named method and passes the rest
// through.
type Failing struct {
	storage.Storage
	on string
}

// FailOn wraps real so that method returns ErrInjected.
func FailOn(t *testing.T, real storage.Storage, method string) *Failing {
	t.Helper()
	if !slices.Contains(storageMethods, method) {
		t.Fatalf("storagetest: cannot fail %q; it is one of %v", method, storageMethods)
	}
	return &Failing{Storage: real, on: method}
}

func (f *Failing) fails(method string) error {
	if f.on == method {
		return ErrInjected
	}
	return nil
}

// Put implements storage.Storage.
func (f *Failing) Put(ctx context.Context, key string, r io.Reader, size int64) (storage.ObjectInfo, error) {
	if err := f.fails("Put"); err != nil {
		return storage.ObjectInfo{}, err
	}
	return f.Storage.Put(ctx, key, r, size)
}

// Get implements storage.Storage.
func (f *Failing) Get(ctx context.Context, key string, rng storage.Range) (io.ReadCloser, storage.ObjectInfo, error) {
	if err := f.fails("Get"); err != nil {
		return nil, storage.ObjectInfo{}, err
	}
	return f.Storage.Get(ctx, key, rng)
}

// Delete implements storage.Storage.
func (f *Failing) Delete(ctx context.Context, key string) error {
	if err := f.fails("Delete"); err != nil {
		return err
	}
	return f.Storage.Delete(ctx, key)
}

// Stat implements storage.Storage.
func (f *Failing) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	if err := f.fails("Stat"); err != nil {
		return storage.ObjectInfo{}, err
	}
	return f.Storage.Stat(ctx, key)
}

// List implements storage.Storage.
//
// The failure is yielded rather than returned, because that is the only way an
// iterator reports one -- and a caller that ignores it and keeps going is
// exactly the bug worth having a test for.
func (f *Failing) List(ctx context.Context, prefix string) iter.Seq2[storage.ObjectInfo, error] {
	if err := f.fails("List"); err != nil {
		return func(yield func(storage.ObjectInfo, error) bool) {
			yield(storage.ObjectInfo{}, err)
		}
	}
	return f.Storage.List(ctx, prefix)
}
