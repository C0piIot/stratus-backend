package db_test

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
)

func TestParentOf(t *testing.T) {
	t.Parallel()
	tests := []struct {
		path string
		want string
	}{
		{path: "file.txt", want: ""},
		{path: "dir/file.txt", want: "dir"},
		{path: "a/b/c/file.txt", want: "a/b/c"},
		{path: "fotos/ñandú.jpg", want: "fotos"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			t.Parallel()
			if got := db.ParentOf(tt.path); got != tt.want {
				t.Errorf("ParentOf(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func TestValidatePathAccepts(t *testing.T) {
	t.Parallel()
	paths := []string{
		"file.txt",
		"photos/2024/06/IMG_0001.jpg",
		"music/Björk/Homogenic/01 Hunter.flac",
		"fotos/ñandú 🌞.jpg",
		"a.file.with.dots",
		".hidden",
		"dir/.hidden",
		strings.Repeat("x", db.MaxPathLen),
	}
	for _, path := range paths {
		t.Run(strconv.Quote(path), func(t *testing.T) {
			t.Parallel()
			if err := db.ValidatePath(path); err != nil {
				t.Errorf("ValidatePath(%q) = %v, want nil", path, err)
			}
		})
	}
}

func TestValidatePathRejects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		path string
	}{
		{"empty", ""},
		{"leading slash", "/etc/passwd"},
		{"trailing slash", "photos/"},
		{"empty segment", "photos//img.jpg"},
		{"dot segment", "photos/./img.jpg"},
		{"parent segment", "photos/../../etc/passwd"},
		{"bare parent", ".."},
		{"nul byte", "photos/img\x00.jpg"},
		{"newline", "photos/img\n.jpg"},
		{"invalid utf-8", "photos/\xff.jpg"},
		{"too long", strings.Repeat("x", db.MaxPathLen+1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := db.ValidatePath(tt.path); !errors.Is(err, db.ErrInvalidPath) {
				t.Errorf("ValidatePath(%q) = %v, want ErrInvalidPath", tt.path, err)
			}
		})
	}
}

// TestValidateDir differs from ValidatePath in exactly one place, and that
// place is the root.
func TestValidateDir(t *testing.T) {
	t.Parallel()
	if err := db.ValidateDir(""); err != nil {
		t.Errorf(`ValidateDir("") = %v, want nil: "" is the root`, err)
	}
	if err := db.ValidateDir("/absolute"); !errors.Is(err, db.ErrInvalidPath) {
		t.Errorf("ValidateDir = %v, want ErrInvalidPath", err)
	}
}

func TestNormalize(t *testing.T) {
	t.Parallel()
	zone := time.FixedZone("CEST", 2*60*60)
	f := db.File{MTime: time.Date(2024, 6, 1, 12, 0, 0, 999_999_999, zone)}.Normalize()

	if f.MTime.Location() != time.UTC {
		t.Errorf("MTime is in %v, want UTC", f.MTime.Location())
	}
	// Truncated, not rounded: 999999999ns must not become the next second.
	if got := f.MTime.Nanosecond(); got != 999_000_000 {
		t.Errorf("nanoseconds = %d, want it truncated to milliseconds", got)
	}
}
