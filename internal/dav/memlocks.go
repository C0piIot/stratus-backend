package dav

import (
	"context"
	"time"

	xnet "golang.org/x/net/webdav"
)

// The lock system, in this process and nowhere else (#192).
//
// It was a table for a while (#243), and the argument for that was one thing:
// the server advertises class 2 to everybody, so with two instances on one
// database `423 Locked` would have been true of one of them and false of the
// other. That is not a promise this server has to keep any more -- it runs as
// one process, by design, and the workspace's CLAUDE.md says so.
//
// What goes back to being true is the older argument, which was never wrong:
// a lock is a claim with a timeout measured in minutes, and a restart
// forgetting one costs a client a retry. What goes away with the table is the
// lease it needed -- a row cannot know that the process holding it has died,
// so a write interrupted by a crash left its path refusing everybody for a
// minute. A held node in memory cannot outlive the process that held it.
//
// x/net's memLS does the storage; everything that decides whether a write is
// allowed is still in locks.go, which is where the `If` header, the tagged
// lists and the ETag conditions live. That half came from #174 and is what
// litmus measures.
type memLocks struct {
	ls xnet.LockSystem
}

// MemoryLocks is the lock system the WebDAV surface runs on.
func MemoryLocks() lockSystem {
	return withOpaqueTokens(&memLocks{ls: xnet.NewMemLS()})
}

// Create implements lockSystem.
//
// The context is unused and stays in the signature: it is the enforcement
// layer's interface, and the one thing it is not is a property of where the
// locks are kept.
func (m *memLocks) Create(_ context.Context, now time.Time, details LockDetails) (string, error) {
	// RFC 4918 allows a lock with no timeout and this server asks for none: a
	// lock nothing can outlast is a resource nobody can ever write again.
	if details.Duration <= 0 {
		return "", xnet.ErrForbidden
	}
	return m.ls.Create(now, details.LockDetails)
}

// Refresh implements lockSystem.
func (m *memLocks) Refresh(_ context.Context, now time.Time, token string, duration time.Duration) (xnet.LockDetails, error) {
	return m.ls.Refresh(now, token, duration)
}

// Unlock implements lockSystem.
func (m *memLocks) Unlock(_ context.Context, now time.Time, token string) error {
	return m.ls.Unlock(now, token)
}

// Confirm implements lockSystem. memLS already answers exactly what this layer
// asks of it: every name has to be covered by a lock one of the conditions
// names, a covering lock nobody claimed is somebody else's, and a name with no
// lock at all is a refusal too -- which is why a write to an unlocked path
// takes a lock on itself first. See lockForTheRequest.
func (m *memLocks) Confirm(_ context.Context, now time.Time, name0, name1 string, conditions ...Condition) (func(), error) {
	return m.ls.Confirm(now, name0, name1, conditions...)
}

// Covers implements lockSystem.
//
// A question rather than a claim, and in memory the only way to ask it is to
// make the claim and give it straight back. Against a table that was two
// statements and a heartbeat for something one SELECT knew, which is why the
// method exists at all; here it costs a map lookup.
func (m *memLocks) Covers(_ context.Context, now time.Time, token, name string) bool {
	if token == "" || name == "" {
		return false
	}
	release, err := m.ls.Confirm(now, name, "", xnet.Condition{Token: token})
	if err != nil {
		return false
	}
	release()
	return true
}
