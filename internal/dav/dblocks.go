package dav

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	xnet "golang.org/x/net/webdav"

	"github.com/C0piIot/stratus-backend/internal/auth"
	"github.com/C0piIot/stratus-backend/internal/db"
)

// The lock system, over the metadata database (#243).
//
// It used to be x/net's memLS, held for the process, and the argument for that
// was written down here: a lock is a claim with a timeout measured in minutes,
// and a restart forgetting one costs a client a retry. What the argument left
// out is that the server advertises class 2 to everybody, and that two
// instances on one database would each honour only their own locks -- so
// `423 Locked` would be true of one of them and false of the other for the
// same resource. A promise that depends on which machine answered is not one.
//
// What a table costs is a few round trips on every write, and a second thing
// to keep alive: memLS marks a node held while a request runs and a held node
// cannot expire, which a row cannot copy. The row carries a lease instead, and
// a request in flight renews it -- see keepAlive.

const (
	// heldLease is how long a hold on a lock outlives the instance that took
	// it. It is the whole cost of moving locks out of memory: an instance that
	// dies mid-write leaves its resource refusing everybody for this long.
	heldLease = 2 * time.Minute

	// heartbeatEvery is how often a request in flight renews. Well inside the
	// lease, so a single slow round trip does not cost the hold.
	heartbeatEvery = 40 * time.Second

	// releaseTimeout bounds the write that gives a lock back. Small: it is one
	// statement, and the request it belongs to has already finished.
	releaseTimeout = 5 * time.Second
)

// dbLocks is the lock system over db.Locks.
type dbLocks struct {
	store db.Locks
	// now is this system's own clock, and not the fileSystem's, because the
	// heartbeat runs between requests and nobody is passing it a time.
	now func() time.Time
	// lease and every are heldLease and heartbeatEvery, on the struct so a test
	// can watch a lease lapse rather than wait two minutes for one.
	lease time.Duration
	every time.Duration

	mu sync.Mutex
	// keeping is the heartbeat running for each token this process holds,
	// so that giving a lock back also stops renewing it.
	keeping map[string]context.CancelFunc
}

// DatabaseLocks is the lock system the WebDAV surface runs on.
func DatabaseLocks(store db.Locks) lockSystem {
	return withOpaqueTokens(&dbLocks{
		store:   store,
		now:     time.Now,
		lease:   heldLease,
		every:   heartbeatEvery,
		keeping: map[string]context.CancelFunc{},
	})
}

// Create implements lockSystem.
func (d *dbLocks) Create(ctx context.Context, now time.Time, details LockDetails) (string, error) {
	owner, ok := auth.User(ctx)
	if !ok {
		return "", xnet.ErrForbidden
	}
	// RFC 4918 allows a lock with no timeout and this server asks for none:
	// both durations above are finite constants, and a lock nothing can outlast
	// is a resource nobody can ever write again.
	if details.Duration <= 0 {
		return "", xnet.ErrForbidden
	}

	lock := db.Lock{
		Token:     newLockToken(),
		OwnerID:   owner,
		Root:      details.Root,
		ZeroDepth: details.ZeroDepth,
		OwnerXML:  details.OwnerXML,
		ExpiresAt: now.Add(details.Duration),
	}
	// A lock the request took on its own behalf is held from the moment it
	// exists: there is no gap between creating it and claiming it for somebody
	// else to fit a Confirm into.
	if details.ForTheRequest {
		lock.HeldBy, lock.HeldUntil = holderOf(ctx), now.Add(d.lease)
	}

	switch err := d.store.CreateLock(ctx, lock, now); {
	case errors.Is(err, db.ErrConflict):
		return "", xnet.ErrLocked
	case err != nil:
		return "", err
	}

	if details.ForTheRequest {
		d.keepAlive(ctx, owner, []string{lock.Token}, details.Duration)
	}
	return lock.Token, nil
}

// Refresh implements lockSystem.
func (d *dbLocks) Refresh(ctx context.Context, now time.Time, token string, duration time.Duration) (xnet.LockDetails, error) {
	owner, ok := auth.User(ctx)
	if !ok {
		return xnet.LockDetails{}, xnet.ErrForbidden
	}

	lock, err := d.store.RefreshLock(ctx, owner, token, holderOf(ctx), now.Add(duration), now)
	switch {
	case errors.Is(err, db.ErrNotFound):
		return xnet.LockDetails{}, xnet.ErrNoSuchLock
	case errors.Is(err, db.ErrConflict):
		return xnet.LockDetails{}, xnet.ErrLocked
	case err != nil:
		return xnet.LockDetails{}, err
	}
	return xnet.LockDetails{
		Root:      lock.Root,
		Duration:  lock.ExpiresAt.Sub(now),
		OwnerXML:  lock.OwnerXML,
		ZeroDepth: lock.ZeroDepth,
	}, nil
}

// Unlock implements lockSystem.
func (d *dbLocks) Unlock(ctx context.Context, now time.Time, token string) error {
	owner, ok := auth.User(ctx)
	if !ok {
		return xnet.ErrForbidden
	}
	holder := holderOf(ctx)
	d.stop(token)

	// Detached from the request, and this is the one place that is right rather
	// than a shortcut: most of these unlocks are a write giving back the lock
	// it took on itself, and the client hanging up is exactly when that runs.
	// Abandoning it would leave the path refusing everybody until the lease
	// lapsed, which is the failure this whole file is trying not to have.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()

	switch err := d.store.DeleteLock(ctx, owner, token, holder, now); {
	case errors.Is(err, db.ErrNotFound):
		return xnet.ErrNoSuchLock
	case errors.Is(err, db.ErrConflict):
		return xnet.ErrLocked
	case err != nil:
		return err
	}
	return nil
}

// Covers implements lockSystem.
func (d *dbLocks) Covers(ctx context.Context, now time.Time, token, name string) bool {
	owner, ok := auth.User(ctx)
	if !ok || token == "" || name == "" {
		return false
	}

	covering, err := d.store.LocksCovering(ctx, owner, []string{name}, now)
	if err != nil {
		// A question the database could not answer is not a yes. The write
		// behind it is refused, which is the safe direction.
		slog.Error("reading the locks over a resource", "err", err)
		return false
	}
	for _, lock := range covering {
		if lock.Token == token && lock.Covers(name) {
			return true
		}
	}
	return false
}

// Confirm implements lockSystem: it reports whether these conditions let this
// request write to name0 and name1, and takes what they claim for as long as it
// runs.
//
// Every name has to be covered by a lock one of the conditions names. Both
// halves of that matter and the in-memory lock system had them too: a covering
// lock nobody claimed is somebody else's, and **a name with no lock on it at
// all is a refusal as well** -- a client submitting a token for an unlocked
// resource is saying something untrue about it, which is 412 and not a write
// that goes through. What makes a write to an unlocked path possible is the
// lock the request takes on itself first; see lockForTheRequest.
func (d *dbLocks) Confirm(ctx context.Context, now time.Time, name0, name1 string, conditions ...Condition) (func(), error) {
	owner, ok := auth.User(ctx)
	if !ok {
		return nil, xnet.ErrForbidden
	}

	names := make([]string, 0, 2)
	for _, name := range []string{name0, name1} {
		if name != "" {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return func() {}, nil
	}

	covering, err := d.store.LocksCovering(ctx, owner, names, now)
	if err != nil {
		return nil, err
	}
	claims := map[string]struct{}{}
	for _, name := range names {
		covered := false
		for _, lock := range covering {
			if !lock.Covers(name) {
				continue
			}
			if !claimedBy(conditions, lock.Token) {
				return nil, xnet.ErrConfirmationFailed
			}
			claims[lock.Token] = struct{}{}
			covered = true
		}
		if !covered {
			return nil, xnet.ErrConfirmationFailed
		}
	}
	// Sorted so that two requests claiming the same pair take them in the same
	// order, and a test reading the query sees a stable one.
	tokens := slices.Sorted(maps.Keys(claims))

	hold := db.Hold{Holder: holderOf(ctx), Expires: now.Add(lockTimeout), Until: now.Add(d.lease)}
	switch got, herr := d.store.HoldLocks(ctx, owner, tokens, hold, now); {
	case herr != nil:
		return nil, herr
	case got != len(tokens):
		// Another request has one of them in hand. Give back whatever this one
		// did take, or it would sit there until the lease ran out.
		d.release(ctx, owner, tokens)
		return nil, xnet.ErrConfirmationFailed
	}

	d.keepAlive(ctx, owner, tokens, lockTimeout)
	return func() {
		d.stop(tokens...)
		d.release(ctx, owner, tokens)
	}, nil
}

// release gives the locks back, on a context of its own for the reason Unlock
// gives: this runs when the request is over, including when it is over because
// the client went away.
func (d *dbLocks) release(ctx context.Context, owner string, tokens []string) {
	holder := holderOf(ctx)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()

	if err := d.store.ReleaseLocks(ctx, owner, tokens, holder); err != nil {
		// Nothing to do about it here, and nothing is broken: the lease is what
		// frees these if this fails.
		slog.Error("releasing webdav locks", "err", err)
	}
}

// keepAlive renews the hold on tokens until the request ends.
//
// It hangs off the request's own context rather than a detached one, which is
// the point: a client that disappears mid-PUT stops renewing, and two minutes
// later the resource is somebody else's to write. That is the same answer an
// instance that died gets, because from the database there is no difference.
func (d *dbLocks) keepAlive(ctx context.Context, owner string, tokens []string, duration time.Duration) {
	holder := holderOf(ctx)
	ctx, cancel := context.WithCancel(ctx)

	d.mu.Lock()
	for _, token := range tokens {
		d.keeping[token] = cancel
	}
	d.mu.Unlock()

	go func() {
		// Twice over, and neither is redundant: stop cancels this from the
		// release, and this cancels it when the request ended without one.
		defer cancel()
		defer d.forget(tokens...)
		ticker := time.NewTicker(d.every)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}

			now := d.now()
			hold := db.Hold{Holder: holder, Expires: now.Add(duration), Until: now.Add(d.lease)}
			switch got, err := d.store.HoldLocks(ctx, owner, tokens, hold, now); {
			case errors.Is(err, context.Canceled):
				return
			case err != nil:
				slog.Warn("renewing a webdav lock", "err", err)
			case got != len(tokens):
				// The lease ran out and somebody else took it, which means this
				// write is going to finish over a lock it no longer holds.
				// There is nothing to do but say so: the request is already
				// streaming bytes, and the ETag is what a client compares.
				slog.Warn("lost a webdav lock while the request was running",
					"tokens", len(tokens), "kept", got)
				return
			}
		}
	}()
}

// stop ends the renewal for these tokens, which is what a release does before
// it gives them back.
func (d *dbLocks) stop(tokens ...string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	for _, token := range tokens {
		if cancel, ok := d.keeping[token]; ok {
			delete(d.keeping, token)
			cancel()
		}
	}
}

// forget drops what stop would have dropped, for the heartbeat that ended
// because the request did rather than because anything released it.
func (d *dbLocks) forget(tokens ...string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	for _, token := range tokens {
		delete(d.keeping, token)
	}
}

// claimedBy reports whether one of the conditions claims that token. A negative
// condition names a lock to say it is not held, which claims nothing.
func claimedBy(conditions []Condition, token string) bool {
	for _, c := range conditions {
		if c.Token == token && !c.Not {
			return true
		}
	}
	return false
}

// holderKey carries the name of the request a hold belongs to.
//
// **One request and not one process**, which is the scope the in-memory lock
// system's held flag had: two writes from the same client, on the same
// instance, submitting the same token must not both get through, and a holder
// that named the process would let them. What a request that stopped renewing
// loses, it loses whether its process died or its client hung up -- from the
// table there is no difference, and there does not need to be one.
type holderKey struct{}

// withHolder names the request that is about to take locks. enforceLocks is
// what puts one on, because "the length of the request" is the scope it owns.
func withHolder(ctx context.Context, holder string) context.Context {
	return context.WithValue(ctx, holderKey{}, holder)
}

// holderOf is that name, or "" for a request that takes no locks on its own
// behalf -- a LOCK or an UNLOCK. Empty is the right fallback rather than a
// missing case: the column is ” when nothing holds the lock, so a caller with
// no name of its own matches exactly the rows nobody is using.
func holderOf(ctx context.Context) string {
	holder, _ := ctx.Value(holderKey{}).(string)
	return holder
}

// newHolder names one request, for as long as it runs.
func newHolder() string { return randomHex(8) }

// newLockToken is the opaque half of a lock token, which opaqueTokens gives a
// scheme. Random rather than a counter because two instances mint them
// independently and a counter would collide; see opaqueTokens for why being
// unguessable is not what this is for.
//
// **Short on purpose, and the reason is a client and not taste.** A token goes
// into an If header beside one or two ETags, and this server's ETag is a
// SHA-256 in hex -- 64 characters each. litmus builds that header into a fixed
// buffer of about two hundred bytes and silently truncates, so a longer token
// turns `complex_cond_put` into a 400 from a header the client mangled. It is
// a 2011 C program, which is exactly the kind of client this surface exists
// for, so the buffer is evidence rather than a quirk to code around. Twelve
// base64url characters is 72 bits, which is more than the memLS counter this
// replaced had and forty fewer characters than hex would have cost.
func newLockToken() string {
	b := make([]byte, 9)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func randomHex(n int) string {
	b := make([]byte, n)
	// crypto/rand.Read cannot fail: since Go 1.24 it panics rather than
	// returning an error a caller would have to invent a fallback for.
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
