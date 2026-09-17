package dbtest

import (
	"context"
	"errors"
	"iter"
	"slices"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// A metadata store that fails one call, for the branches a working database
// will not take on request.
//
// It wraps a real store rather than replacing one, for the reason its blob-side
// twin gives: what these cases are about is what happens *around* the failure,
// so everything except the one call has to behave. The most load-bearing of
// them is the half of "blob first, row second" where the row does not land --
// the ordering internal/files exists to guarantee, whose failure path had no
// test at all.
//
// The failure reaches a repository inside a transaction too, which is where it
// has to: internal/files does every write through Tx, so a wrapper that only
// broke the direct calls would break nothing that matters.

// ErrInjected is what a failing call returns.
var ErrInjected = errors.New("dbtest: injected failure")

// repoMethods is every call Failing can be asked to break. The list grows with
// the tests that need it -- there is no value in wrapping a method nothing
// fails on purpose -- and FailOn refuses a name that is not here, because a
// test asking for one would silently never fail.
var repoMethods = []string{
	"PutFile", "CreateDir", "FileByPath", "ListFiles", "MoveFile", "DeleteFile",
	"BlobKeys", "PutMedia",
	"PutUpload", "UploadByID", "DeleteUpload", "ExpiredUploads",
}

// Failing is a db.Store that fails one named method and passes the rest
// through, including inside a transaction.
type Failing struct {
	failingRepo
	store db.Store
}

// FailOn wraps store so that method returns ErrInjected.
func FailOn(t *testing.T, store db.Store, method string) *Failing {
	t.Helper()
	if !slices.Contains(repoMethods, method) {
		t.Fatalf("dbtest: cannot fail %q; it is one of %v", method, repoMethods)
	}
	return &Failing{failingRepo: failingRepo{Repo: store, on: method}, store: store}
}

// Tx implements db.Store. The repository handed to fn fails the same call the
// store does, which is the whole point: every write internal/files makes goes
// through one.
func (f *Failing) Tx(ctx context.Context, fn func(db.Repo) error) error {
	return f.store.Tx(ctx, func(r db.Repo) error {
		return fn(&failingRepo{Repo: r, on: f.on})
	})
}

// Migrate implements db.Store.
func (f *Failing) Migrate(ctx context.Context) error { return f.store.Migrate(ctx) }

// Ping implements db.Store.
func (f *Failing) Ping(ctx context.Context) error { return f.store.Ping(ctx) }

// Close implements db.Store.
func (f *Failing) Close() error { return f.store.Close() }

// failingRepo is the half that can sit inside a transaction.
type failingRepo struct {
	db.Repo
	on string
}

func (f *failingRepo) fails(method string) error {
	if f.on == method {
		return ErrInjected
	}
	return nil
}

// PutFile implements db.Repo.
func (f *failingRepo) PutFile(ctx context.Context, file db.File) (db.File, error) {
	if err := f.fails("PutFile"); err != nil {
		return db.File{}, err
	}
	return f.Repo.PutFile(ctx, file)
}

// CreateDir implements db.Repo.
func (f *failingRepo) CreateDir(ctx context.Context, owner, path string) (db.File, error) {
	if err := f.fails("CreateDir"); err != nil {
		return db.File{}, err
	}
	return f.Repo.CreateDir(ctx, owner, path)
}

// FileByPath implements db.Repo.
func (f *failingRepo) FileByPath(ctx context.Context, owner, path string) (db.File, error) {
	if err := f.fails("FileByPath"); err != nil {
		return db.File{}, err
	}
	return f.Repo.FileByPath(ctx, owner, path)
}

// ListFiles implements db.Repo.
func (f *failingRepo) ListFiles(ctx context.Context, owner, dir string) ([]db.File, error) {
	if err := f.fails("ListFiles"); err != nil {
		return nil, err
	}
	return f.Repo.ListFiles(ctx, owner, dir)
}

// MoveFile implements db.Repo.
func (f *failingRepo) MoveFile(ctx context.Context, owner, from, to string) error {
	if err := f.fails("MoveFile"); err != nil {
		return err
	}
	return f.Repo.MoveFile(ctx, owner, from, to)
}

// DeleteFile implements db.Repo.
func (f *failingRepo) DeleteFile(ctx context.Context, owner, path string) error {
	if err := f.fails("DeleteFile"); err != nil {
		return err
	}
	return f.Repo.DeleteFile(ctx, owner, path)
}

// BlobKeys implements db.Repo, yielding the failure the way an iterator has to.
func (f *failingRepo) BlobKeys(ctx context.Context) iter.Seq2[string, error] {
	if err := f.fails("BlobKeys"); err != nil {
		return func(yield func(string, error) bool) {
			yield("", err)
		}
	}
	return f.Repo.BlobKeys(ctx)
}

// PutMedia implements db.Repo.
func (f *failingRepo) PutMedia(ctx context.Context, m db.Media) error {
	if err := f.fails("PutMedia"); err != nil {
		return err
	}
	return f.Repo.PutMedia(ctx, m)
}

// PutUpload implements db.Repo.
func (f *failingRepo) PutUpload(ctx context.Context, u db.Upload) error {
	if err := f.fails("PutUpload"); err != nil {
		return err
	}
	return f.Repo.PutUpload(ctx, u)
}

// UploadByID implements db.Repo.
func (f *failingRepo) UploadByID(ctx context.Context, owner, id string) (db.Upload, error) {
	if err := f.fails("UploadByID"); err != nil {
		return db.Upload{}, err
	}
	return f.Repo.UploadByID(ctx, owner, id)
}

// DeleteUpload implements db.Repo.
func (f *failingRepo) DeleteUpload(ctx context.Context, owner, id string) error {
	if err := f.fails("DeleteUpload"); err != nil {
		return err
	}
	return f.Repo.DeleteUpload(ctx, owner, id)
}

// ExpiredUploads implements db.Repo.
func (f *failingRepo) ExpiredUploads(ctx context.Context, now time.Time) iter.Seq2[db.Upload, error] {
	if err := f.fails("ExpiredUploads"); err != nil {
		return func(yield func(db.Upload, error) bool) { yield(db.Upload{}, err) }
	}
	return f.Repo.ExpiredUploads(ctx, now)
}
