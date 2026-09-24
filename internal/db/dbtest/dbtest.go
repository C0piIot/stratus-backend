// Package dbtest is the conformance suite every metadata driver has to pass.
//
// Its rules matter as much as its assertions: it never assumes how a driver
// stores a time, an integer or a path, and it never assumes an order beyond the
// one the port promises. What it does assume is what the port promises out
// loud -- transactions that really roll back, and listings that are an indexed
// lookup rather than a scan the caller filters.
package dbtest

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// Run executes the whole suite against the store built by newStore: every
// feature's cases, plus the ones that need a connection rather than a
// repository -- transactions, and migrating twice.
//
// newStore is called once per case and must return an empty, migrated store
// that no other case can observe, so the cases can run in parallel.
func Run(t *testing.T, newStore func(t *testing.T) db.Store) {
	t.Helper()

	t.Run("files", func(t *testing.T) {
		t.Parallel()
		RunFiles(t, func(t *testing.T) db.Files { return newStore(t) })
	})

	t.Run("media", func(t *testing.T) {
		t.Parallel()
		RunMedia(t, func(t *testing.T) db.Repo { return newStore(t) })
	})

	t.Run("music", func(t *testing.T) {
		t.Parallel()
		RunMusic(t, func(t *testing.T) db.Repo { return newStore(t) })
	})

	t.Run("annotations", func(t *testing.T) {
		t.Parallel()
		RunAnnotations(t, func(t *testing.T) db.Repo { return newStore(t) })
	})

	t.Run("uploads", func(t *testing.T) {
		t.Parallel()
		RunUploads(t, func(t *testing.T) db.Repo { return newStore(t) })
	})

	cases := []struct {
		name string
		fn   func(t *testing.T, s db.Store)
	}{
		{"a transaction commits", txCommits},
		{"a transaction rolls back on error", txRollsBack},
		{"a transaction rolls back on panic", txRollsBackOnPanic},
		{"migrating twice changes nothing", migrateIsIdempotent},
		{"a move rolled back leaves the tree as it was", moveRollsBack},
		{"two writers at once both get through", concurrentWriters},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.fn(t, newStore(t))
		})
	}
}

// concurrentWriters is the promise a database is for: two of them at the same
// time is ordinary, and the second one waits rather than failing.
//
// It is here rather than in one driver because it is a property of the seam.
// It only ever failed on one of them -- SQLite refuses to upgrade a deferred
// transaction's read lock, and busy_timeout does not apply to that -- and it
// failed as a 500 on a plain PUT, which is a phone backing up a camera roll
// with more than one upload in flight. Found by litmus (#173), which is what
// that suite is for.
func concurrentWriters(t *testing.T, s db.Store) {
	if _, err := s.CreateDir(t.Context(), owner, "busy"); err != nil {
		t.Fatal(err)
	}

	const writers = 24
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A read and then a write in one transaction, which is the shape
			// internal/files writes in and the shape that could not upgrade.
			err := s.Tx(t.Context(), func(r db.Repo) error {
				if _, err := r.FileByPath(t.Context(), owner, "busy"); err != nil {
					return err
				}
				_, err := r.PutFile(t.Context(), file(fmt.Sprintf("busy/f%02d.txt", i)))
				return err
			})
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)

	var failed int
	for err := range errs {
		if failed == 0 {
			t.Errorf("a concurrent write failed: %v", err)
		}
		failed++
	}
	if failed > 0 {
		t.Errorf("%d of %d writers were refused", failed, writers)
	}

	got, err := s.ListFiles(t.Context(), owner, "busy")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != writers {
		t.Errorf("%d of %d rows landed", len(got), writers)
	}
}

func txCommits(t *testing.T, s db.Store) {
	err := s.Tx(t.Context(), func(r db.Repo) error {
		if _, err := r.PutFile(t.Context(), file("in-a-tx.txt")); err != nil {
			return err
		}
		_, err := r.PutFile(t.Context(), file("also-in-it.txt"))
		return err
	})
	if err != nil {
		t.Fatalf("Tx: %v", err)
	}
	if got := paths(t, s, ""); !slices.Equal(got, []string{"also-in-it.txt", "in-a-tx.txt"}) {
		t.Errorf("after commit the root holds %v, want both files", got)
	}
}

func txRollsBack(t *testing.T, s db.Store) {
	boom := errors.New("boom")

	err := s.Tx(t.Context(), func(r db.Repo) error {
		if _, err := r.PutFile(t.Context(), file("should-not-survive.txt")); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Tx = %v, want the error the function returned", err)
	}
	if got := paths(t, s, ""); len(got) != 0 {
		t.Errorf("after rollback the root holds %v, want nothing", got)
	}
}

func txRollsBackOnPanic(t *testing.T, s db.Store) {
	func() {
		defer func() {
			if recover() == nil {
				t.Error("the panic was swallowed; it must reach the caller")
			}
		}()
		_ = s.Tx(t.Context(), func(r db.Repo) error {
			if _, err := r.PutFile(t.Context(), file("panic.txt")); err != nil {
				return err
			}
			panic("something went very wrong")
		})
	}()

	// The point is not the panic but the lock: a transaction left open holds
	// one, and the next writer would block on it forever.
	if got := paths(t, s, ""); len(got) != 0 {
		t.Errorf("after a panic the root holds %v, want nothing", got)
	}
}

func migrateIsIdempotent(t *testing.T, s db.Store) {
	// newStore has already migrated; every restart of the server runs this
	// again, so a second run must be a no-op rather than an error.
	if err := s.Migrate(t.Context()); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	put(t, s, file("still-works.txt"))
}

// moveRollsBack is the half of a subtree rename that cannot be checked from
// inside one: the whole tree moves or none of it does. The rewrite is one
// statement per driver, but the caller wraps it with other work -- files.Move
// checks the destination's parent in the same transaction -- so what this pins
// is that a failure after the move undoes all of it, not just the row the
// caller happened to name.
func moveRollsBack(t *testing.T, s db.Store) {
	ctx := t.Context()
	if err := s.Tx(ctx, func(r db.Repo) error {
		if _, err := r.CreateDir(ctx, owner, "inbox"); err != nil {
			return err
		}
		_, err := r.PutFile(ctx, file("inbox/photo.jpg"))
		return err
	}); err != nil {
		t.Fatal(err)
	}

	sentinel := errors.New("something after the move")
	if err := s.Tx(ctx, func(r db.Repo) error {
		if merr := r.MoveFile(ctx, owner, "inbox", "archive"); merr != nil {
			return merr
		}
		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatalf("Tx = %v, want the sentinel", err)
	}

	for _, path := range []string{"inbox", "inbox/photo.jpg"} {
		if _, err := s.FileByPath(ctx, owner, path); err != nil {
			t.Errorf("%q did not survive the rollback: %v", path, err)
		}
	}
	for _, path := range []string{"archive", "archive/photo.jpg"} {
		if _, err := s.FileByPath(ctx, owner, path); !errors.Is(err, db.ErrNotFound) {
			t.Errorf("%q survived the rollback: %v", path, err)
		}
	}
}
