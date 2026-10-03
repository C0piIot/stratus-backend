package dav

import (
	"errors"
	"testing"
	"time"

	xnet "golang.org/x/net/webdav"
)

func lockedFor(t *testing.T, ls lockSystem, root string, d time.Duration) string {
	t.Helper()
	token, err := ls.Create(t.Context(), time.Now(), LockDetails{
		LockDetails: xnet.LockDetails{Root: root, Duration: d, ZeroDepth: false},
	})
	if err != nil {
		t.Fatalf("Create(%q): %v", root, err)
	}
	return token
}

// TestALockWithNoTimeoutIsRefused: RFC 4918 allows one and this server asks for
// none, because a lock nothing can outlast is a resource nobody can ever write
// again.
func TestALockWithNoTimeoutIsRefused(t *testing.T) {
	t.Parallel()
	ls := MemoryLocks()

	if _, err := ls.Create(t.Context(), time.Now(), LockDetails{
		LockDetails: xnet.LockDetails{Root: "/notes.txt"},
	}); !errors.Is(err, xnet.ErrForbidden) {
		t.Errorf("a lock with no duration = %v, want ErrForbidden", err)
	}
}

// TestCoversAnswersWithoutTakingAnything is the fifth method's whole point: it
// is a question, and asking it must leave the lock exactly as claimable as it
// was.
func TestCoversAnswersWithoutTakingAnything(t *testing.T) {
	t.Parallel()
	ls := MemoryLocks()
	token := lockedFor(t, ls, "/album", time.Hour)
	now := time.Now()

	if !ls.Covers(t.Context(), now, token, "/album/one.txt") {
		t.Error("a lock on a collection does not cover what is inside it")
	}
	// Twice, because an implementation that claimed the lock to answer would
	// fail the second time.
	if !ls.Covers(t.Context(), now, token, "/album") {
		t.Error("asking twice took the lock the first time")
	}
	if ls.Covers(t.Context(), now, token, "/elsewhere.txt") {
		t.Error("a lock covers a path it has nothing to do with")
	}
	if ls.Covers(t.Context(), now, "opaquelocktoken:nobody", "/album") {
		t.Error("a token nobody minted covers something")
	}
	for _, empty := range [][2]string{{"", "/album"}, {token, ""}} {
		if ls.Covers(t.Context(), now, empty[0], empty[1]) {
			t.Errorf("Covers(%q, %q) is true", empty[0], empty[1])
		}
	}
}

// TestALockIsGivenBackAndRefreshed, through the interface x/net's own handler
// is bound to -- which is the one PROPFIND is served by.
func TestALockIsGivenBackAndRefreshed(t *testing.T) {
	t.Parallel()
	bound := boundLocks{locks: MemoryLocks(), ctx: t.Context()}
	now := time.Now()

	token, err := bound.Create(now, xnet.LockDetails{Root: "/notes.txt", Duration: time.Minute})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err = bound.Refresh(now, token, time.Hour); err != nil {
		t.Errorf("Refresh: %v", err)
	}
	release, err := bound.Confirm(now, "/notes.txt", "", Condition{Token: token})
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	release()
	if err := bound.Unlock(now, token); err != nil {
		t.Errorf("Unlock: %v", err)
	}
	if _, err := bound.Refresh(now, token, time.Hour); err == nil {
		t.Error("a token that was unlocked still refreshes")
	}
}
