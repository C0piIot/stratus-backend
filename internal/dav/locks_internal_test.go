package dav

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	xnet "golang.org/x/net/webdav"
)

// The lock system is driven by a clock the caller passes in -- every one of its
// four methods takes the time rather than reading it -- so expiry is something
// a test can watch instead of wait for. That is the whole reason these two are
// here rather than through a request.

func TestALockExpires(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	f := &fileSystem{locks: memoryLocks(), now: func() time.Time { return clock }}

	if _, err := f.locks.Create(f.now(), xnet.LockDetails{Root: "/notes.txt", Duration: lockTimeout}); err != nil {
		t.Fatal(err)
	}
	write := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/notes.txt", nil)

	if _, status, err := f.confirmLocks(write, "/notes.txt", ""); err == nil {
		t.Error("a write with no token got past a live lock")
	} else if status != StatusLocked {
		t.Errorf("a write over a live lock = %d, want 423", status)
	}

	clock = clock.Add(lockTimeout + time.Minute)

	release, status, err := f.confirmLocks(write, "/notes.txt", "")
	if err != nil {
		t.Fatalf("a write after the lock expired = %d, %v", status, err)
	}
	release()
}

// TestTheTokenIsAUriAndNotACounter: x/net's memLS numbers its locks, and RFC
// 4918 6.5 says a lock token is a URI. locks.go gives them a scheme, and this
// is that claim -- including that a token from somewhere else names no lock.
func TestTheTokenIsAUriAndNotACounter(t *testing.T) {
	t.Parallel()
	locks := memoryLocks()
	now := time.Now()

	first, err := locks.Create(now, xnet.LockDetails{Root: "/one.txt", Duration: lockTimeout})
	if err != nil {
		t.Fatal(err)
	}
	second, err := locks.Create(now, xnet.LockDetails{Root: "/two.txt", Duration: lockTimeout})
	if err != nil {
		t.Fatal(err)
	}

	for _, token := range []string{first, second} {
		if !strings.HasPrefix(token, "opaquelocktoken:") {
			t.Errorf("token %q is not an absolute URI", token)
		}
		if strings.ContainsAny(token, " \t") {
			t.Errorf("token %q has whitespace in it, which no Coded-URL may", token)
		}
	}
	if first == second {
		t.Errorf("two locks got the same token %q", first)
	}

	// A token this process never minted names no lock, which is the answer
	// that matters rather than an error of its own.
	if err := locks.Unlock(now, "opaquelocktoken:invented"); !errors.Is(err, xnet.ErrNoSuchLock) {
		t.Errorf("unlocking a foreign token = %v, want no such lock", err)
	}
	if err := locks.Unlock(now, first); err != nil {
		t.Errorf("unlocking our own = %v", err)
	}
}
