package dbtest

import (
	"sync"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// RunMigrationLock executes the cases that keep two instances from migrating at
// the same time (#242), against the engine's own lock.
//
// open returns a new, unmigrated store on one database that every call shares,
// because two instances starting together is exactly what that is. The cases
// run in order rather than in parallel: they are about what one of them does
// while the other is holding something.
func RunMigrationLock(t *testing.T, open func(t *testing.T) db.Store) {
	t.Helper()

	t.Run("two instances started together both come up", func(t *testing.T) {
		stores := []db.Store{open(t), open(t)}
		errs := make(chan error, len(stores))

		var wg sync.WaitGroup
		for _, store := range stores {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs <- store.Migrate(t.Context())
			}()
		}
		wg.Wait()
		close(errs)

		// Without the lock this is where one of them fails: both read the same
		// MAX(version) and both apply the same migration.
		for err := range errs {
			if err != nil {
				t.Errorf("Migrate: %v", err)
			}
		}
	})

	t.Run("the second waits rather than fails", func(t *testing.T) {
		locker, ok := open(t).(db.MigrationLocker)
		if !ok {
			t.Fatal("this store takes no migration lock, so this suite is not for it")
		}
		release, err := locker.LockMigrations(t.Context())
		if err != nil {
			t.Fatalf("LockMigrations: %v", err)
		}

		waiter := open(t)
		done := make(chan error, 1)
		go func() { done <- waiter.Migrate(t.Context()) }()

		select {
		case err := <-done:
			t.Fatalf("a migration ran while another instance held the lock: %v", err)
		case <-time.After(heldLongEnough):
		}

		release()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("the migration that waited its turn = %v", err)
			}
		case <-time.After(waitForTheLock):
			t.Error("the waiting instance never got the lock")
		}
	})

	t.Run("the lock comes back", func(t *testing.T) {
		locker, ok := open(t).(db.MigrationLocker)
		if !ok {
			t.Fatal("this store takes no migration lock, so this suite is not for it")
		}

		// Twice over, which is the assertion: the second take is the first
		// release having actually happened, on the engine rather than in Go.
		for i := range 2 {
			release, err := locker.LockMigrations(t.Context())
			if err != nil {
				t.Fatalf("LockMigrations %d: %v", i+1, err)
			}
			release()
		}
	})
}

const (
	// heldLongEnough is how long the waiting instance is watched for before it
	// is believed to be waiting. Long enough to mean something, short enough
	// that the suite does not pay for it twice over.
	heldLongEnough = 300 * time.Millisecond

	// waitForTheLock is how long it gets to finish once the lock is free. It is
	// generous because what it bounds is a test that hangs, not a wait anybody
	// is measuring.
	waitForTheLock = 30 * time.Second
)
