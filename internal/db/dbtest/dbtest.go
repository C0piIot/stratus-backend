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
	"slices"
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

	cases := []struct {
		name string
		fn   func(t *testing.T, s db.Store)
	}{
		{"a transaction commits", txCommits},
		{"a transaction rolls back on error", txRollsBack},
		{"a transaction rolls back on panic", txRollsBackOnPanic},
		{"migrating twice changes nothing", migrateIsIdempotent},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.fn(t, newStore(t))
		})
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
