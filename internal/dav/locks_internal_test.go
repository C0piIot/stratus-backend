package dav

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	xnet "golang.org/x/net/webdav"

	"github.com/C0piIot/stratus-backend/internal/auth"
	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/db/sqlite"
)

// The lock system is driven by a clock the caller passes in -- every one of its
// four methods takes the time rather than reading it -- so expiry is something
// a test can watch instead of wait for. That is the whole reason these are here
// rather than through a request.

// lockStore is a migrated database of its own, since a lock is a row (#243).
func lockStore(t *testing.T) db.Locks {
	t.Helper()
	store, err := sqlite.New(t.Context(), filepath.Join(t.TempDir(), "stratus.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return store
}

// asOwner is the context every lock call arrives on: the owner is taken from
// the authenticated user, the way it is on a request.
func asOwner(t *testing.T) context.Context {
	t.Helper()
	return auth.WithUser(t.Context(), "edu")
}

func TestALockExpires(t *testing.T) {
	t.Parallel()
	ctx := asOwner(t)
	clock := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	f := &fileSystem{locks: DatabaseLocks(lockStore(t)), now: func() time.Time { return clock }}

	if _, err := f.locks.Create(ctx, f.now(), LockDetails{
		LockDetails: xnet.LockDetails{Root: "/notes.txt", Duration: lockTimeout},
	}); err != nil {
		t.Fatal(err)
	}
	write := httptest.NewRequestWithContext(ctx, http.MethodPut, "/notes.txt", nil)

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

// TestTheTokenIsAUriAndNotACounter: RFC 4918 6.5 says a lock token is a URI,
// and what mints them is an opaque string. locks.go gives them a scheme, and
// this is that claim -- including that a token from somewhere else names no
// lock.
func TestTheTokenIsAUriAndNotACounter(t *testing.T) {
	t.Parallel()
	ctx := asOwner(t)
	locks := DatabaseLocks(lockStore(t))
	now := time.Now()

	first, err := locks.Create(ctx, now, LockDetails{
		LockDetails: xnet.LockDetails{Root: "/one.txt", Duration: lockTimeout},
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := locks.Create(ctx, now, LockDetails{
		LockDetails: xnet.LockDetails{Root: "/two.txt", Duration: lockTimeout},
	})
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

	// A token this server never minted names no lock, which is the answer that
	// matters rather than an error of its own.
	if err := locks.Unlock(ctx, now, "opaquelocktoken:invented"); !errors.Is(err, xnet.ErrNoSuchLock) {
		t.Errorf("unlocking a foreign token = %v, want no such lock", err)
	}
	if err := locks.Unlock(ctx, now, first); err != nil {
		t.Errorf("unlocking our own = %v", err)
	}
}

// TestAHoldLapsesWhenNobodyRenews is what the lease is for: a request that
// stops renewing -- because its process was killed, or because its client went
// away, which from the table is the same thing -- must not leave the resource
// refusing everybody until the lock itself times out.
func TestAHoldLapsesWhenNobodyRenews(t *testing.T) {
	t.Parallel()
	ctx := withHolder(asOwner(t), "request-a")
	store := lockStore(t)
	now := time.Now()

	dead := DatabaseLocks(store)
	token, err := dead.Create(ctx, now, LockDetails{
		LockDetails:   xnet.LockDetails{Root: "/notes.txt", Duration: briefLock, ZeroDepth: true},
		ForTheRequest: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Another request, while the one that took it is still live.
	alive := DatabaseLocks(store)
	if _, err := alive.Create(ctx, now, LockDetails{
		LockDetails: xnet.LockDetails{Root: "/notes.txt", Duration: lockTimeout},
	}); !errors.Is(err, xnet.ErrLocked) {
		t.Errorf("locking what a live request holds = %v, want locked", err)
	}

	// Nothing renewed it, so a minute later the path is free again and the
	// token names nothing.
	later := now.Add(briefLock + time.Second)
	if _, err := alive.Create(ctx, later, LockDetails{
		LockDetails: xnet.LockDetails{Root: "/notes.txt", Duration: lockTimeout},
	}); err != nil {
		t.Errorf("locking after the hold lapsed = %v", err)
	}
	if err := dead.Unlock(ctx, later, token); !errors.Is(err, xnet.ErrNoSuchLock) {
		t.Errorf("unlocking a lapsed hold = %v, want no such lock", err)
	}
}

// TestARequestInFlightKeepsItsLock is the other half of the lease, and the
// reason a PUT of several gigabytes still finishes: the lock a write takes on
// itself lives for a minute, and the write renews it for as long as it runs.
//
// The clock is real here, unlike everything else in this file, because what is
// being tested is a goroutine and a ticker. The intervals are shrunk instead --
// but not as far as they will go. What fails this test is the renewal not
// running, and a goroutine that is simply not scheduled for one lifetime looks
// exactly like one that is broken: at sixty milliseconds a lifetime it failed
// on a loaded CI runner and nowhere else. Ten renewals per lifetime is the
// margin, so it takes half a second of being ignored to call this a bug.
func TestARequestInFlightKeepsItsLock(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(withHolder(asOwner(t), "request-a"))
	defer cancel()

	store := lockStore(t)
	const (
		lives  = 500 * time.Millisecond
		lease  = 2 * lives
		renews = lives / 10
	)
	locks := &dbLocks{
		store:   store,
		now:     time.Now,
		lease:   lease,
		every:   renews,
		keeping: map[string]context.CancelFunc{},
	}

	if _, err := locks.Create(ctx, time.Now(), LockDetails{
		LockDetails:   xnet.LockDetails{Root: "/notes.txt", Duration: lives, ZeroDepth: true},
		ForTheRequest: true,
	}); err != nil {
		t.Fatal(err)
	}

	// Twice over the point where an unrenewed lock would have gone.
	time.Sleep(2 * lives)
	if !stillLocked(t, store) {
		t.Fatal("the lock a request was renewing expired underneath it")
	}

	// The request ends. Nothing renews it, and it goes.
	cancel()
	time.Sleep(2 * lives)
	if stillLocked(t, store) {
		t.Error("the lock outlived the request that was holding it")
	}
}

func stillLocked(t *testing.T, store db.Locks) bool {
	t.Helper()
	got, err := store.LocksCovering(t.Context(), "edu", []string{"/notes.txt"}, time.Now())
	if err != nil {
		t.Fatalf("LocksCovering: %v", err)
	}
	return len(got) > 0
}

// TestBoundLocksCarriesTheContext: x/net's Handler takes a lock system whose
// methods have no context of their own, so PROPFIND is served one bound to its
// request (see propfind.go). Nothing on that route calls these today --
// supportedlock is a constant and lockdiscovery is a TODO in x/net's own source
// -- which is exactly why the binding is asserted here rather than left to be
// found wrong the day something does.
func TestBoundLocksCarriesTheContext(t *testing.T) {
	t.Parallel()
	ctx := withHolder(asOwner(t), "request-a")
	bound := boundLocks{locks: DatabaseLocks(lockStore(t)), ctx: ctx}
	now := time.Now()

	token, err := bound.Create(now, xnet.LockDetails{Root: "/notes.txt", Duration: lockTimeout})
	if err != nil {
		t.Fatalf("Create through the bound system: %v", err)
	}

	release, err := bound.Confirm(now, "/notes.txt", "", Condition{Token: token})
	if err != nil {
		t.Fatalf("Confirm through the bound system: %v", err)
	}
	release()

	switch details, rerr := bound.Refresh(now, token, lockTimeout); {
	case rerr != nil:
		t.Errorf("Refresh through the bound system = %v", rerr)
	case details.Root != "/notes.txt":
		t.Errorf("refreshed lock is on %q, want /notes.txt", details.Root)
	}

	if err := bound.Unlock(now, token); err != nil {
		t.Errorf("Unlock through the bound system = %v", err)
	}

	// Without a user on the context there is no owner to look a lock up for,
	// and the answer has to be a refusal rather than somebody else's locks.
	stranger := boundLocks{locks: bound.locks, ctx: t.Context()}
	if _, err := stranger.Create(now, xnet.LockDetails{Root: "/notes.txt", Duration: lockTimeout}); err == nil {
		t.Error("a lock was taken on a request with no authenticated user")
	}
}

// TestTwoRequestsCannotUseOneToken is why a hold names a request and not the
// process it runs in. Two writes from the same client, with the same token,
// must not both go through -- the in-memory lock system marked its node held
// for exactly as long, and a holder that named the instance would have let the
// second one past on the machine that granted the first.
func TestTwoRequestsCannotUseOneToken(t *testing.T) {
	t.Parallel()
	locks := DatabaseLocks(lockStore(t))
	now := time.Now()

	first := withHolder(asOwner(t), "request-a")
	token, err := locks.Create(first, now, LockDetails{
		LockDetails: xnet.LockDetails{Root: "/notes.txt", Duration: lockTimeout},
	})
	if err != nil {
		t.Fatal(err)
	}

	// A token naming some other lock claims nothing, whoever submits it.
	_, err = locks.Confirm(first, now, "/notes.txt", "", Condition{Token: "opaquelocktoken:elsewhere"})
	if !errors.Is(err, xnet.ErrConfirmationFailed) {
		t.Errorf("a write claiming a token that names no lock here = %v, want confirmation failed", err)
	}

	release, err := locks.Confirm(first, now, "/notes.txt", "", Condition{Token: token})
	if err != nil {
		t.Fatalf("the first write = %v", err)
	}

	second := withHolder(asOwner(t), "request-b")
	if _, err := locks.Confirm(second, now, "/notes.txt", "", Condition{Token: token}); !errors.Is(err, xnet.ErrConfirmationFailed) {
		t.Errorf("a second write with the same token = %v, want confirmation failed", err)
	}
	// Nor may the lock be refreshed or dropped out from under the write.
	if _, err := locks.Refresh(second, now, token, lockTimeout); !errors.Is(err, xnet.ErrLocked) {
		t.Errorf("refreshing a lock a write is using = %v, want locked", err)
	}
	if err := locks.Unlock(second, now, token); !errors.Is(err, xnet.ErrLocked) {
		t.Errorf("unlocking a lock a write is using = %v, want locked", err)
	}

	// Given back, it is the next write's.
	release()
	if _, err := locks.Confirm(second, now, "/notes.txt", "", Condition{Token: token}); err != nil {
		t.Errorf("a write after the first one finished = %v", err)
	}
}
