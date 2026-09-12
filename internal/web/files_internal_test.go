package web

import (
	"errors"
	"strings"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// TestToPath is where the URL becomes a path in somebody's tree, so it is
// tested as the pure function it is rather than only through a request. The
// port validates too -- FileByPath refuses the same strings -- which is exactly
// why this needs its own test: a request would still be refused if this stopped
// checking, and nothing would say so.
//
// internal/dav tests its twin through requests instead. The two functions stay
// separate for the reason toPath's comment gives, so this table is the only
// place the cleaning itself is pinned down.
func TestToPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want string
		ok   bool
	}{
		{name: "the root", ok: true},
		{name: "a file", raw: "notes.txt", want: "notes.txt", ok: true},
		{name: "deeper", raw: "photos/2026/img.jpg", want: "photos/2026/img.jpg", ok: true},
		{name: "a trailing slash", raw: "photos/", want: "photos", ok: true},
		// Cleaned rather than refused: a browser sends these by accident.
		{name: "a doubled slash", raw: "photos//2026", want: "photos/2026", ok: true},
		{name: "a dot", raw: "photos/./2026", want: "photos/2026", ok: true},
		{name: "climbing out", raw: "../../etc/passwd", want: "etc/passwd", ok: true},
		{name: "climbing out from inside", raw: "photos/../../../etc/passwd", want: "etc/passwd", ok: true},
		// Refused, because no amount of cleaning makes them a path.
		{name: "a control character", raw: "a\x01b"},
		{name: "invalid UTF-8", raw: "a\xffb"},
		{name: "longer than the tree allows", raw: strings.Repeat("x", db.MaxPathLen+1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := toPath(tt.raw)

			if !tt.ok {
				if !errors.Is(err, db.ErrInvalidPath) {
					t.Fatalf("toPath(%q) = %q, %v; want ErrInvalidPath", tt.raw, got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("toPath(%q) = %v", tt.raw, err)
			}
			if got != tt.want {
				t.Errorf("toPath(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

// TestBaseName is the other half of "a name is not a path". Three forms feed
// it -- an upload, a new folder, a rename -- and each of them would be a way
// out of the directory it was shown in if this returned anything else.
func TestBaseName(t *testing.T) {
	t.Parallel()

	tests := []struct{ sent, want string }{
		{sent: "img.jpg", want: "img.jpg"},
		{sent: "holiday/img.jpg", want: "img.jpg"},
		{sent: `C:\Users\edu\img.jpg`, want: "img.jpg"},
		{sent: "../../img.jpg", want: "img.jpg"},
		{sent: "/etc/passwd", want: "passwd"},
		{sent: `..\..\img.jpg`, want: "img.jpg"},
		// What is left is not a name, and db.ValidatePath is what refuses it.
		{sent: "", want: "."},
		{sent: "..", want: ".."},
		{sent: "/", want: "/"},
	}
	for _, tt := range tests {
		if got := baseName(tt.sent); got != tt.want {
			t.Errorf("baseName(%q) = %q, want %q", tt.sent, got, tt.want)
		}
	}
}

// humanSize is arithmetic with a ladder of units, and a listing test only ever
// exercises the rung the fixture happens to sit on. Tested where it lives, with
// the boundaries that matter: the first byte of each unit, and the last one
// before it.
func TestHumanSize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		n    int64
		want string
	}{
		{n: 0, want: "0 B"},
		{n: 1, want: "1 B"},
		{n: 1023, want: "1023 B"},
		{n: 1024, want: "1.0 KB"},
		{n: 1536, want: "1.5 KB"},
		{n: 1024*1024 - 1, want: "1024.0 KB"},
		{n: 1024 * 1024, want: "1.0 MB"},
		{n: 3*1024*1024*1024 + 512*1024*1024, want: "3.5 GB"},
		{n: 1024 * 1024 * 1024 * 1024, want: "1.0 TB"},
	}
	for _, tt := range tests {
		if got := humanSize(tt.n); got != tt.want {
			t.Errorf("humanSize(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}
