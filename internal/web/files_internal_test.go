package web

import (
	"errors"
	"strings"
	"testing"

	"github.com/stratus-private-cloud/stratus-backend/internal/db"
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

// humanBytes is arithmetic with a ladder of units, and a listing test only ever
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
		{n: 1024, want: "1.0 KiB"},
		{n: 1536, want: "1.5 KiB"},
		{n: 1024*1024 - 1, want: "1024.0 KiB"},
		{n: 1024 * 1024, want: "1.0 MiB"},
		{n: 3*1024*1024*1024 + 512*1024*1024, want: "3.5 GiB"},
		{n: 1024 * 1024 * 1024 * 1024, want: "1.0 TiB"},
	}
	for _, tt := range tests {
		if got := humanBytes(tt.n); got != tt.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

// TestFilePolicy: everything is opened, and what could run a script is opened
// sandboxed. An SVG is an image and is still sandboxed, because it can carry
// one; a PDF is not, because Chrome refuses to render one that is.
func TestFilePolicy(t *testing.T) {
	t.Parallel()
	for mimeType, sandboxed := range map[string]bool{
		"image/jpeg":                false,
		"image/heic":                false,
		"video/mp4":                 false,
		"audio/flac":                false,
		"application/pdf":           false,
		"text/plain; charset=utf-8": false,
		"image/svg+xml":             true,
		"text/html":                 true,
		"text/javascript":           true,
		"application/xhtml+xml":     true,
		"application/octet-stream":  true,
		"":                          true,
	} {
		got := filePolicy(mimeType)
		if strings.HasSuffix(got, "; sandbox") != sandboxed {
			t.Errorf("filePolicy(%q) = %q, sandboxed should be %v", mimeType, got, sandboxed)
		}
		if strings.Contains(got, "script-src") {
			t.Errorf("filePolicy(%q) = %q allows a script", mimeType, got)
		}
	}
}

// TestTitleOf pins what a tab, a bookmark and a history entry are named, which
// is the one place the trail above a folder is not also on the page: the
// breadcrumbs are there to be read, and a title that said only "2024" would
// leave a browserful of folders nothing tells apart.
func TestTitleOf(t *testing.T) {
	t.Parallel()

	tests := []struct{ dir, root, want string }{
		{want: "Files"},
		{dir: "holiday", want: "holiday"},
		{dir: "holiday/2024/lisbon", want: "holiday/2024/lisbon"},
		// A share starts at the share, for the reason crumbs gives: what the
		// visitor was sent is the top, and the owner's tree above it is not
		// theirs to be told about.
		{dir: "holiday/2024", root: "holiday/2024", want: "2024"},
		{dir: "holiday/2024/lisbon", root: "holiday/2024", want: "lisbon"},
		{dir: "holiday/2024/lisbon/beach", root: "holiday/2024", want: "lisbon/beach"},
	}
	for _, tt := range tests {
		if got := titleOf(tt.dir, tt.root); got != tt.want {
			t.Errorf("titleOf(%q, %q) = %q, want %q", tt.dir, tt.root, got, tt.want)
		}
	}
}
