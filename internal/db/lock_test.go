package db_test

import (
	"slices"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
)

func TestLockAncestors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		want []string
	}{
		{name: "", want: nil},
		{name: "/", want: []string{"/"}},
		{name: "/notes.txt", want: []string{"/", "/notes.txt"}},
		{name: "/album/one.txt", want: []string{"/", "/album", "/album/one.txt"}},
		{name: "/a/b/c", want: []string{"/", "/a", "/a/b", "/a/b/c"}},
	}
	for _, tt := range tests {
		if got := db.LockAncestors(tt.name); !slices.Equal(got, tt.want) {
			t.Errorf("LockAncestors(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestLockRoots: the union, in order, without the duplicates two names under
// one folder would otherwise put in the IN clause twice.
func TestLockRoots(t *testing.T) {
	t.Parallel()
	got := db.LockRoots([]string{"/album/one.txt", "/album/two.txt"})
	want := []string{"/", "/album", "/album/one.txt", "/album/two.txt"}
	if !slices.Equal(got, want) {
		t.Errorf("LockRoots = %v, want %v", got, want)
	}
	if got := db.LockRoots(nil); got != nil {
		t.Errorf("LockRoots(nil) = %v, want nothing", got)
	}
}

// TestLockSubtree is the range a lock over a collection looks inside. The upper
// bound is the separator's successor, which is what keeps /album-2 out of the
// range for /album without a LIKE.
func TestLockSubtree(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, from, to string }{
		{name: "/", from: "/", to: "0"},
		{name: "/album", from: "/album/", to: "/album0"},
		{name: "/a/b", from: "/a/b/", to: "/a/b0"},
	}
	for _, tt := range tests {
		from, to := db.LockSubtree(tt.name)
		if from != tt.from || to != tt.to {
			t.Errorf("LockSubtree(%q) = %q, %q, want %q, %q", tt.name, from, to, tt.from, tt.to)
		}
	}

	// The bound is what keeps a sibling out without a LIKE: /album-2 sorts
	// after /album/ and it must sort after the end of the range too.
	from, to := db.LockSubtree("/album")
	if sibling := "/album-2"; sibling >= from && sibling < to {
		t.Errorf("LockSubtree(%q) = %q..%q, which has %q in it", "/album", from, to, sibling)
	}
}

func TestLockCovers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		root      string
		zeroDepth bool
		name      string
		want      bool
	}{
		{root: "/notes.txt", name: "/notes.txt", want: true},
		{root: "/notes.txt", name: "/other.txt", want: false},
		{root: "/album", name: "/album/one.txt", want: true},
		{root: "/album", name: "/album-2/one.txt", want: false},
		{root: "/album", zeroDepth: true, name: "/album/one.txt", want: false},
		{root: "/album", zeroDepth: true, name: "/album", want: true},
		{root: "/", name: "/anything/at/all", want: true},
		{root: "/", zeroDepth: true, name: "/anything", want: false},
	}
	for _, tt := range tests {
		lock := db.Lock{Root: tt.root, ZeroDepth: tt.zeroDepth}
		if got := lock.Covers(tt.name); got != tt.want {
			t.Errorf("Lock{%q, zeroDepth:%v}.Covers(%q) = %v, want %v",
				tt.root, tt.zeroDepth, tt.name, got, tt.want)
		}
	}
}

// TestLockHeld: a hold is a name and a lease, and either one missing means the
// lock is nobody's right now.
func TestLockHeld(t *testing.T) {
	t.Parallel()
	now := time.Now()
	tests := []struct {
		what string
		lock db.Lock
		want bool
	}{
		{what: "nobody holds it", lock: db.Lock{}},
		{what: "held with a live lease", lock: db.Lock{HeldBy: "a", HeldUntil: now.Add(time.Minute)}, want: true},
		{what: "the lease ran out", lock: db.Lock{HeldBy: "a", HeldUntil: now.Add(-time.Minute)}},
		{what: "a lease with no holder", lock: db.Lock{HeldUntil: now.Add(time.Minute)}},
	}
	for _, tt := range tests {
		if got := tt.lock.Held(now); got != tt.want {
			t.Errorf("%s: Held = %v, want %v", tt.what, got, tt.want)
		}
	}
}
