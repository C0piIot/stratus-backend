package db

import (
	"context"
	"strings"
	"time"
)

// Lock is a WebDAV write lock: a claim on a path, with a timeout, that refuses
// somebody else's write until it expires or is released.
//
// It is a row rather than something held in the process for the reason the
// protocol surface advertises class 2 and the operator restarts the binary:
// a lock nothing records is a promise kept only until the next deploy, and a
// lock one instance records is a promise kept only for the clients that land
// on it (#243).
//
// What it is not is a general mutex. Nothing outside internal/dav takes one,
// and nothing about the file layer's own invariants depends on one: the strong
// ETag and If-Match are still what a client without a lock relies on.
type Lock struct {
	// Token is what a client submits in an If header to say the lock is its
	// own. It is the primary key: the scheme a URI needs is put on and taken
	// off by the adapter, since that is the protocol's spelling and not this
	// port's.
	Token string
	// OwnerID is who took it. Locks are scoped to an owner everywhere, so the
	// day sharing arrives one owner's lock cannot refuse another's write by
	// accident.
	OwnerID string
	// Root is the resource locked, in the URL form the If header carries: "/"
	// for the top of the tree, "/notes.txt" for a file. Not the storage path,
	// because the only thing that compares these is the adapter, and it has
	// the URL form in its hand.
	Root string
	// ZeroDepth is a lock on that one resource. The other kind covers
	// everything under it, which is how a client locks a folder it is about to
	// write several files into.
	ZeroDepth bool
	// OwnerXML is whatever the client put in the <owner> element, echoed back
	// untouched on a refresh and in lockdiscovery. Arbitrary XML, stored as
	// the bytes it arrived as.
	OwnerXML string
	// ExpiresAt is when the lock stops refusing anything. Every read filters
	// on it, so an expired lock is invisible whether or not the sweep has got
	// to it yet.
	ExpiresAt time.Time
	// HeldBy names the request that has this lock taken, and is "" when none
	// has. It is what stops two writes using one token at the same time -- the
	// in-memory lock system marked a node held for exactly as long, and the
	// difference is that this one has to say *which* request did it, because
	// there is no longer one process to take it for granted.
	HeldBy string
	// HeldUntil is the lease on that hold, extended while the request runs.
	// Without it a request that stops -- because its process was killed
	// mid-PUT, or because its client hung up, which from here is the same
	// thing -- would leave the resource refusing everybody until ExpiresAt,
	// which for a client's own lock is an hour.
	HeldUntil time.Time
}

// Held reports whether some request has this lock taken right now.
func (l Lock) Held(now time.Time) bool {
	return l.HeldBy != "" && l.HeldUntil.After(now)
}

// Covers reports whether this lock applies to name: the resource itself, or
// anything under it when the lock is not zero-depth.
func (l Lock) Covers(name string) bool {
	if l.Root == name {
		return true
	}
	if l.ZeroDepth {
		return false
	}
	under, _ := LockSubtree(l.Root)
	return strings.HasPrefix(name, under)
}

// LockAncestors returns name and every collection above it, in the same form,
// with "/" first.
//
// This is how a covering lock is found without a LIKE: the caller computes the
// handful of names that could hold one and the query is an indexed IN. A path
// ten levels deep is eleven strings, which is cheaper than asking the database
// to match a prefix over every row it has.
func LockAncestors(name string) []string {
	if name == "" {
		return nil
	}
	out := []string{"/"}
	if name == "/" {
		return out
	}
	for i := 1; i < len(name); i++ {
		if name[i] == '/' {
			out = append(out, name[:i])
		}
	}
	return append(out, name)
}

// LockRoots is every name a lock covering one of names could be rooted at:
// each name and every collection above it, with no duplicates.
//
// It is here rather than in each driver because it is not SQL -- it is the
// shape of the question, and all three ask it the same way.
func LockRoots(names []string) []string {
	seen := make(map[string]struct{}, len(names)*4)
	var out []string
	for _, name := range names {
		for _, root := range LockAncestors(name) {
			if _, done := seen[root]; done {
				continue
			}
			seen[root] = struct{}{}
			out = append(out, root)
		}
	}
	return out
}

// LockSubtree is the half-open range of lock names strictly under name, for the
// one question ancestors cannot answer: whether taking a lock over a whole
// collection would swallow one somebody else already holds inside it.
//
// A range and not a prefix match, the same trick SubtreeRange plays on the
// files table, so it is an index seek in the two drivers that can index a path.
func LockSubtree(name string) (from, to string) {
	from = name
	if !strings.HasSuffix(from, "/") {
		from += "/"
	}
	return from, from[:len(from)-1] + string(rune('/'+1))
}

// Hold is one request's claim on a set of locks for as long as it runs.
//
// Holder names the request and not the process, which is the scope the
// in-memory lock system's held flag had: two writes from one client, on one
// instance, submitting the same token must not both get through. A release has
// to be able to say "the hold that was mine", and a lease has to be able to
// expire somebody else's.
type Hold struct {
	Holder string
	// Expires is the lock's own new expiry. A request holding a lock pushes it
	// out as it goes, so a write slower than the timeout does not finish
	// against a lock that has lapsed underneath it.
	Expires time.Time
	// Until is the lease on the hold itself, and is short. It is what an
	// instance that died mid-write loses, and the only thing that frees the
	// resource afterwards.
	Until time.Time
}

// Locks is the repository for WebDAV locks.
//
// Every method takes the caller's clock rather than reading the database's, for
// the reason PendingMedia gives: the two are not the same one, and a test that
// has to wait an hour is a test nobody runs.
//
// Several of them also take the caller's own holder id and treat a lock it
// already holds as free, which is what lets a request give back the lock it
// took on its own behalf and renew the one it is using. A caller with no holder
// of its own passes "", which matches exactly the rows nobody is holding.
type Locks interface {
	// CreateLock records l, or returns ErrConflict when a live lock already
	// covers its root -- one on the resource itself, one on a collection above
	// it, or, when l is not zero-depth, one on anything below it.
	//
	// The check is inside the statement rather than a read the caller makes
	// first, on the same rule the tree invariant follows: one round trip, and
	// no window between asking and inserting for a concurrent LOCK to fit in.
	CreateLock(ctx context.Context, l Lock, now time.Time) error

	// LocksCovering returns every live lock rooted at one of names or at a
	// collection above it. Whether such a lock actually applies is Lock.Covers,
	// which the caller asks: a zero-depth lock on an ancestor comes back here
	// and covers nothing, and sorting that out over a handful of rows in Go is
	// cheaper than expressing it once per name in SQL.
	LocksCovering(ctx context.Context, owner string, names []string, now time.Time) ([]Lock, error)

	// HoldLocks takes the named locks for a request in flight, pushing their
	// expiry out with it, and returns how many it got.
	//
	// It is a compare-and-set: a lock held by somebody else whose lease has not
	// run out is not taken and is not counted, so a caller that asked for two
	// and got one knows it lost. Called again on the same tokens by the same
	// holder it is the heartbeat -- a PUT of several gigabytes outlives any
	// lease worth setting, and renewing is what tells a slow write apart from
	// an instance that died holding the path.
	HoldLocks(ctx context.Context, owner string, tokens []string, h Hold, now time.Time) (int, error)

	// ReleaseLocks gives back what holder took, leaving the locks themselves in
	// place. Locks held by somebody else are untouched, so a release arriving
	// after the lease was lost cannot free the hold that replaced it.
	ReleaseLocks(ctx context.Context, owner string, tokens []string, holder string) error

	// RefreshLock pushes a lock's expiry out and returns it. ErrNotFound if
	// there is no live lock with that token, ErrConflict if another holder has
	// it: the client is asking about state that is being changed underneath it.
	RefreshLock(ctx context.Context, owner, token, holder string, expires, now time.Time) (Lock, error)

	// DeleteLock releases a lock for good. Same two refusals as RefreshLock,
	// for the same reasons.
	DeleteLock(ctx context.Context, owner, token, holder string, now time.Time) error

	// DeleteExpiredLocks removes what has timed out, and returns how many.
	//
	// Nothing depends on it running: every read already filters on the expiry,
	// so this reclaims rows rather than enforcing anything. That is what makes
	// it safe to run from every instance at once.
	DeleteExpiredLocks(ctx context.Context, now time.Time) (int, error)
}
