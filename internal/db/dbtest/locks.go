package dbtest

import (
	"errors"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// RunLocks executes the WebDAV lock cases against the repository built by
// newRepo.
//
// What these pin is a lock that means the same thing to a process that did not
// grant it: the covering rules, which are the protocol's, and the hold, which
// is this port's own answer to a thing the in-memory lock system got for free
// -- a node it marked held could not expire, and a row can.
func RunLocks(t *testing.T, newRepo func(t *testing.T) db.Repo) {
	t.Helper()

	cases := []struct {
		name string
		fn   func(t *testing.T, s db.Repo)
	}{
		{"a lock survives a round trip", lockRoundTrip},
		{"a collection covers what is under it", lockCovers},
		{"two locks cannot share a root", lockConflict},
		{"a lock may not swallow one inside it", lockNesting},
		{"an expired lock covers nothing", lockExpiry},
		{"a hold is a compare-and-set", lockHold},
		{"a hold is given back by whoever took it", lockRelease},
		{"a refresh moves the expiry", lockRefresh},
		{"deleting says why it could not", lockDelete},
		{"the sweep reclaims what timed out", lockSweep},
		{"a lock belongs to its owner", lockOwner},
		{"a call with nothing in it asks nothing", lockNothing},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.fn(t, newRepo(t))
		})
	}
}

// lockAt is a plausible lock, with every field set to something
// distinguishable: a driver that dropped one, or swapped two, fails the round
// trip rather than passing by coincidence.
func lockAt(token, root string, now time.Time) db.Lock {
	return db.Lock{
		Token:     token,
		OwnerID:   owner,
		Root:      root,
		ZeroDepth: false,
		OwnerXML:  `<D:href>mailto:edu@example.com</D:href>`,
		ExpiresAt: now.Add(time.Hour),
	}
}

// lockClock is truncated to the millisecond because that is the resolution two
// of the three drivers store, and UTC because a time that comes back in another
// zone is the same instant and a confusing failure.
func lockClock() time.Time { return time.Now().UTC().Truncate(time.Millisecond) }

func lockRoundTrip(t *testing.T, s db.Repo) {
	now := lockClock()
	want := lockAt("tok-1", "/notes.txt", now)
	if err := s.CreateLock(t.Context(), want, now); err != nil {
		t.Fatalf("CreateLock: %v", err)
	}

	got := onlyLock(t, s, "/notes.txt", now)
	switch {
	case got.Token != want.Token:
		t.Errorf("token = %q, want %q", got.Token, want.Token)
	case got.OwnerID != want.OwnerID:
		t.Errorf("owner = %q, want %q", got.OwnerID, want.OwnerID)
	case got.Root != want.Root:
		t.Errorf("root = %q, want %q", got.Root, want.Root)
	case got.ZeroDepth != want.ZeroDepth:
		t.Errorf("zero depth = %v, want %v", got.ZeroDepth, want.ZeroDepth)
	case got.OwnerXML != want.OwnerXML:
		t.Errorf("owner XML = %q, want %q", got.OwnerXML, want.OwnerXML)
	case !got.ExpiresAt.Equal(want.ExpiresAt):
		t.Errorf("expiry = %v, want %v", got.ExpiresAt, want.ExpiresAt)
	case got.Held(now):
		t.Error("a lock nobody claimed came back held")
	}
}

// lockCovers is the depth rule, which is the protocol's: a lock on a collection
// applies to everything under it unless it was taken with Depth: 0.
func lockCovers(t *testing.T, s db.Repo) {
	now := lockClock()
	deep := lockAt("tok-deep", "/album", now)
	if err := s.CreateLock(t.Context(), deep, now); err != nil {
		t.Fatalf("CreateLock: %v", err)
	}

	inside := onlyLock(t, s, "/album/one.txt", now)
	if !inside.Covers("/album/one.txt") {
		t.Error("a lock on a collection does not cover a file in it")
	}
	// A name that merely starts the same way is not under it.
	if inside.Covers("/album-2/one.txt") {
		t.Error("a lock on /album covers /album-2, which is a different folder")
	}

	shallow := lockAt("tok-flat", "/notes.txt", now)
	shallow.ZeroDepth = true
	if err := s.CreateLock(t.Context(), shallow, now); err != nil {
		t.Fatalf("CreateLock: %v", err)
	}
	flat := onlyLock(t, s, "/notes.txt", now)
	if flat.Covers("/notes.txt/inner") {
		t.Error("a zero-depth lock covers something under it")
	}
}

func lockConflict(t *testing.T, s db.Repo) {
	now := lockClock()
	if err := s.CreateLock(t.Context(), lockAt("tok-1", "/notes.txt", now), now); err != nil {
		t.Fatalf("CreateLock: %v", err)
	}
	if err := s.CreateLock(t.Context(), lockAt("tok-2", "/notes.txt", now), now); !errors.Is(err, db.ErrConflict) {
		t.Errorf("locking what is already locked = %v, want ErrConflict", err)
	}
	// And through a collection above it, which is the same refusal one level up.
	if err := s.CreateLock(t.Context(), lockAt("tok-3", "/notes.txt/inner", now), now); !errors.Is(err, db.ErrConflict) {
		t.Errorf("locking under a locked resource = %v, want ErrConflict", err)
	}
}

// lockNesting is the half of the rule an ancestor lookup cannot answer: a lock
// over a whole collection has to find nothing locked inside it.
func lockNesting(t *testing.T, s db.Repo) {
	now := lockClock()
	if err := s.CreateLock(t.Context(), lockAt("tok-in", "/album/one.txt", now), now); err != nil {
		t.Fatalf("CreateLock: %v", err)
	}
	if err := s.CreateLock(t.Context(), lockAt("tok-over", "/album", now), now); !errors.Is(err, db.ErrConflict) {
		t.Errorf("locking a collection over a locked file = %v, want ErrConflict", err)
	}

	// Zero depth claims nothing below, so it is allowed over the same folder.
	over := lockAt("tok-flat", "/album", now)
	over.ZeroDepth = true
	if err := s.CreateLock(t.Context(), over, now); err != nil {
		t.Errorf("a zero-depth lock over a folder with a locked file in it = %v", err)
	}
	// Nor may one be taken over the root while anything is locked at all.
	if err := s.CreateLock(t.Context(), lockAt("tok-root", "/", now), now); !errors.Is(err, db.ErrConflict) {
		t.Errorf("locking the root over a locked file = %v, want ErrConflict", err)
	}
}

func lockExpiry(t *testing.T, s db.Repo) {
	now := lockClock()
	if err := s.CreateLock(t.Context(), lockAt("tok-old", "/notes.txt", now), now); err != nil {
		t.Fatalf("CreateLock: %v", err)
	}

	later := now.Add(2 * time.Hour)
	got, err := s.LocksCovering(t.Context(), owner, []string{"/notes.txt"}, later)
	if err != nil {
		t.Fatalf("LocksCovering: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("an expired lock still covers its root: %v", got)
	}
	// And the path is free, which is the thing that matters to a client.
	if err := s.CreateLock(t.Context(), lockAt("tok-new", "/notes.txt", later), later); err != nil {
		t.Errorf("locking over an expired lock = %v", err)
	}
}

// lockHold is what replaces the held flag the in-memory lock system kept: two
// requests must not use one lock at the same time, and the second has to be
// told so rather than told a number it can ignore.
func lockHold(t *testing.T, s db.Repo) {
	now := lockClock()
	if err := s.CreateLock(t.Context(), lockAt("tok-1", "/notes.txt", now), now); err != nil {
		t.Fatalf("CreateLock: %v", err)
	}

	mine := db.Hold{Holder: "request-a", Expires: now.Add(time.Hour), Until: now.Add(time.Minute)}
	if got, err := s.HoldLocks(t.Context(), owner, []string{"tok-1"}, mine, now); err != nil || got != 1 {
		t.Fatalf("HoldLocks = %d, %v, want 1", got, err)
	}
	if held := onlyLock(t, s, "/notes.txt", now); !held.Held(now) {
		t.Error("a lock that was taken does not say it is held")
	}

	theirs := db.Hold{Holder: "request-b", Expires: now.Add(time.Hour), Until: now.Add(time.Minute)}
	if got, err := s.HoldLocks(t.Context(), owner, []string{"tok-1"}, theirs, now); err != nil || got != 0 {
		t.Errorf("holding what somebody else has = %d, %v, want 0", got, err)
	}

	// Once the lease has run out it is anybody's, which is the whole point of
	// having one: the request that took it may never come back.
	lapsed := now.Add(2 * time.Minute)
	if got, err := s.HoldLocks(t.Context(), owner, []string{"tok-1"}, theirs, lapsed); err != nil || got != 1 {
		t.Errorf("holding after the lease lapsed = %d, %v, want 1", got, err)
	}

	// The same holder asking again is the heartbeat, and it has to work.
	if got, err := s.HoldLocks(t.Context(), owner, []string{"tok-1"}, theirs, lapsed); err != nil || got != 1 {
		t.Errorf("renewing a hold = %d, %v, want 1", got, err)
	}
}

func lockRelease(t *testing.T, s db.Repo) {
	now := lockClock()
	if err := s.CreateLock(t.Context(), lockAt("tok-1", "/notes.txt", now), now); err != nil {
		t.Fatalf("CreateLock: %v", err)
	}
	mine := db.Hold{Holder: "request-a", Expires: now.Add(time.Hour), Until: now.Add(time.Minute)}
	if _, err := s.HoldLocks(t.Context(), owner, []string{"tok-1"}, mine, now); err != nil {
		t.Fatalf("HoldLocks: %v", err)
	}

	// Somebody else's release does nothing, or a request coming back from the
	// dead would free the hold that replaced its own.
	if err := s.ReleaseLocks(t.Context(), owner, []string{"tok-1"}, "request-b"); err != nil {
		t.Fatalf("ReleaseLocks: %v", err)
	}
	if held := onlyLock(t, s, "/notes.txt", now); !held.Held(now) {
		t.Error("another request's release freed a hold that was not its own")
	}

	if err := s.ReleaseLocks(t.Context(), owner, []string{"tok-1"}, "request-a"); err != nil {
		t.Fatalf("ReleaseLocks: %v", err)
	}
	free := onlyLock(t, s, "/notes.txt", now)
	if free.Held(now) {
		t.Error("a released lock still says it is held")
	}
	// Released, not deleted: the client still holds it.
	if free.Token != "tok-1" {
		t.Errorf("releasing a hold removed the lock: %v", free)
	}
}

func lockRefresh(t *testing.T, s db.Repo) {
	now := lockClock()
	if err := s.CreateLock(t.Context(), lockAt("tok-1", "/notes.txt", now), now); err != nil {
		t.Fatalf("CreateLock: %v", err)
	}

	want := now.Add(3 * time.Hour)
	got, err := s.RefreshLock(t.Context(), owner, "tok-1", "request-a", want, now)
	if err != nil {
		t.Fatalf("RefreshLock: %v", err)
	}
	if !got.ExpiresAt.Equal(want) {
		t.Errorf("refreshed expiry = %v, want %v", got.ExpiresAt, want)
	}
	if got.OwnerXML == "" {
		t.Error("a refresh came back without the owner element a client expects to see again")
	}

	// A request in flight somewhere else is a reason to refuse: the client is
	// asking about state that is being changed underneath it.
	busy := db.Hold{Holder: "request-b", Expires: want, Until: now.Add(time.Minute)}
	if _, err := s.HoldLocks(t.Context(), owner, []string{"tok-1"}, busy, now); err != nil {
		t.Fatalf("HoldLocks: %v", err)
	}
	if _, err := s.RefreshLock(t.Context(), owner, "tok-1", "request-a", want, now); !errors.Is(err, db.ErrConflict) {
		t.Errorf("refreshing what another request holds = %v, want ErrConflict", err)
	}
	// The instance holding it is not somebody else.
	if _, err := s.RefreshLock(t.Context(), owner, "tok-1", "request-b", want, now); err != nil {
		t.Errorf("refreshing our own held lock = %v", err)
	}

	if _, err := s.RefreshLock(t.Context(), owner, "tok-nobody", "request-a", want, now); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("refreshing a token nobody minted = %v, want ErrNotFound", err)
	}

	// Refreshing to the expiry the row already carries changes nothing, and
	// "nothing changed" is not by itself a refusal: the client asked to keep
	// its lock and still has it.
	if got, err := s.RefreshLock(t.Context(), owner, "tok-1", "request-b", want, now); err != nil {
		t.Errorf("refreshing to the expiry it already had = %v", err)
	} else if !got.ExpiresAt.Equal(want) {
		t.Errorf("expiry after a refresh that changed nothing = %v, want %v", got.ExpiresAt, want)
	}

	// A lock that has timed out is gone, whatever its row still says.
	lapsed := want.Add(time.Hour)
	if _, err := s.RefreshLock(t.Context(), owner, "tok-1", "request-b", lapsed.Add(time.Hour), lapsed); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("refreshing an expired lock = %v, want ErrNotFound", err)
	}
	if err := s.DeleteLock(t.Context(), owner, "tok-1", "request-b", lapsed); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("deleting an expired lock = %v, want ErrNotFound", err)
	}
}

func lockDelete(t *testing.T, s db.Repo) {
	now := lockClock()
	if err := s.CreateLock(t.Context(), lockAt("tok-1", "/notes.txt", now), now); err != nil {
		t.Fatalf("CreateLock: %v", err)
	}

	if err := s.DeleteLock(t.Context(), owner, "tok-nobody", "request-a", now); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("deleting a token nobody minted = %v, want ErrNotFound", err)
	}

	busy := db.Hold{Holder: "request-b", Expires: now.Add(time.Hour), Until: now.Add(time.Minute)}
	if _, err := s.HoldLocks(t.Context(), owner, []string{"tok-1"}, busy, now); err != nil {
		t.Fatalf("HoldLocks: %v", err)
	}
	if err := s.DeleteLock(t.Context(), owner, "tok-1", "request-a", now); !errors.Is(err, db.ErrConflict) {
		t.Errorf("deleting what another request holds = %v, want ErrConflict", err)
	}

	// The request that took it can give it back, which is what a write does
	// with the lock it took on its own behalf.
	if err := s.DeleteLock(t.Context(), owner, "tok-1", "request-b", now); err != nil {
		t.Fatalf("DeleteLock: %v", err)
	}
	if got, err := s.LocksCovering(t.Context(), owner, []string{"/notes.txt"}, now); err != nil || len(got) != 0 {
		t.Errorf("after deleting, LocksCovering = %v, %v, want none", got, err)
	}
	if err := s.DeleteLock(t.Context(), owner, "tok-1", "request-b", now); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("deleting twice = %v, want ErrNotFound", err)
	}
}

func lockSweep(t *testing.T, s db.Repo) {
	now := lockClock()
	if err := s.CreateLock(t.Context(), lockAt("tok-old", "/old.txt", now), now); err != nil {
		t.Fatalf("CreateLock: %v", err)
	}
	fresh := lockAt("tok-new", "/new.txt", now.Add(4*time.Hour))
	if err := s.CreateLock(t.Context(), fresh, now); err != nil {
		t.Fatalf("CreateLock: %v", err)
	}

	later := now.Add(2 * time.Hour)
	switch done, err := s.DeleteExpiredLocks(t.Context(), later); {
	case err != nil:
		t.Fatalf("DeleteExpiredLocks: %v", err)
	case done != 1:
		t.Errorf("swept %d locks, want 1", done)
	}
	if got := onlyLock(t, s, "/new.txt", later); got.Token != "tok-new" {
		t.Errorf("the sweep took a lock that had not expired: %v", got)
	}
}

func lockOwner(t *testing.T, s db.Repo) {
	now := lockClock()
	if err := s.CreateLock(t.Context(), lockAt("tok-1", "/notes.txt", now), now); err != nil {
		t.Fatalf("CreateLock: %v", err)
	}

	if got, err := s.LocksCovering(t.Context(), "someone-else", []string{"/notes.txt"}, now); err != nil || len(got) != 0 {
		t.Errorf("another owner sees %v, %v, want no locks", got, err)
	}
	if err := s.DeleteLock(t.Context(), "someone-else", "tok-1", "request-a", now); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("another owner deleting it = %v, want ErrNotFound", err)
	}
	// And the same root is theirs to lock, since sharing is not here yet and
	// the day it is, one owner's lock must not refuse another's write by
	// accident.
	theirs := lockAt("tok-2", "/notes.txt", now)
	theirs.OwnerID = "someone-else"
	if err := s.CreateLock(t.Context(), theirs, now); err != nil {
		t.Errorf("another owner locking the same path = %v", err)
	}
}

// lockNothing is the empty call every one of these has to answer without a
// round trip: an If header with no tokens in it, a request that touches no
// resource. A query built from an empty IN list is a syntax error in two of the
// three drivers, so "nothing" has to be answered before the SQL is written.
func lockNothing(t *testing.T, s db.Repo) {
	now := lockClock()

	if err := s.CreateLock(t.Context(), lockAt("tok-1", "", now), now); !errors.Is(err, db.ErrInvalidPath) {
		t.Errorf("a lock with no root = %v, want ErrInvalidPath", err)
	}
	if got, err := s.LocksCovering(t.Context(), owner, nil, now); err != nil || len(got) != 0 {
		t.Errorf("LocksCovering with no names = %v, %v, want none", got, err)
	}
	hold := db.Hold{Holder: "request-a", Expires: now.Add(time.Hour), Until: now.Add(time.Minute)}
	if got, err := s.HoldLocks(t.Context(), owner, nil, hold, now); err != nil || got != 0 {
		t.Errorf("HoldLocks with no tokens = %d, %v, want 0", got, err)
	}
	if err := s.ReleaseLocks(t.Context(), owner, nil, "request-a"); err != nil {
		t.Errorf("ReleaseLocks with no tokens = %v", err)
	}
}

// onlyLock is the one lock covering name, and a failure when there is any other
// number of them.
func onlyLock(t *testing.T, s db.Repo, name string, now time.Time) db.Lock {
	t.Helper()
	got, err := s.LocksCovering(t.Context(), owner, []string{name}, now)
	if err != nil {
		t.Fatalf("LocksCovering(%q): %v", name, err)
	}
	if len(got) != 1 {
		t.Fatalf("LocksCovering(%q) returned %d locks, want 1", name, len(got))
	}
	return got[0]
}
