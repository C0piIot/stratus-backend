package db

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// MaxPathLen bounds a stored path. It is generous: the limit exists to keep a
// runaway client from writing a megabyte into an indexed column, not to express
// a rule about names.
const MaxPathLen = 4096

// File is a database row that points at a blob. The pair is what makes
// internal/files necessary: neither half is a file on its own.
type File struct {
	// ID is assigned by the database.
	ID int64
	// OwnerID is always the same value today. It is on the record from the
	// first migration so that sharing later is a feature and not a rewrite.
	OwnerID string
	// Path is slash-separated with no leading slash: "photos/2024/img.jpg".
	Path string
	// BlobKey names the object in blob storage. It is **opaque**: never derived
	// from Path, never from a hash of the content. That is what lets an import
	// adopt somebody else's bucket without moving a byte -- see issue #24.
	BlobKey string
	// Size is the blob length in bytes.
	Size int64
	// MTime is the modification time the client claims. Stored to millisecond
	// precision, in UTC, so that two drivers with different time types agree.
	MTime time.Time
	// ETag is the validator serialised for conditional requests. The port does
	// not compute it; internal/files owns that.
	ETag string
	// MIMEType is what was declared at upload time.
	MIMEType string
	// IsDir marks a collection rather than a file. A directory row carries no
	// blob: BlobKey, Size, ETag and MIMEType are empty on one, and it exists
	// only so that an empty directory can, which WebDAV's MKCOL requires.
	IsDir bool
}

// TimePrecision is what every driver rounds MTime to. Postgres keeps
// microseconds and SQLite keeps whatever it is handed, so without a common
// resolution a value would not survive a round trip identically on both.
const TimePrecision = time.Millisecond

// Normalize returns f with the fields drivers must not store verbatim already
// fixed: UTC time at the agreed precision.
func (f File) Normalize() File {
	f.MTime = f.MTime.UTC().Truncate(TimePrecision)
	return f
}

// ParentOf returns the directory holding path, or "" for a path at the root.
// Both drivers derive their parent column with this, so a listing cannot
// disagree between them.
func ParentOf(path string) string {
	i := strings.LastIndexByte(path, '/')
	if i < 0 {
		return ""
	}
	return path[:i]
}

// ValidatePath reports whether path can be stored.
//
// It rejects rather than cleans, for the same reason storage.ValidateKey does:
// silently rewriting a path is how two names come to point at one row.
func ValidatePath(path string) error {
	switch {
	case path == "":
		return fmt.Errorf("%w: empty", ErrInvalidPath)
	case len(path) > MaxPathLen:
		return fmt.Errorf("%w: %d bytes exceeds the %d byte limit", ErrInvalidPath, len(path), MaxPathLen)
	case !utf8.ValidString(path):
		return fmt.Errorf("%w: not valid UTF-8", ErrInvalidPath)
	case strings.HasPrefix(path, "/"), strings.HasSuffix(path, "/"):
		return fmt.Errorf("%w: %q has a leading or trailing slash", ErrInvalidPath, path)
	}

	for _, r := range path {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: %q contains a control character", ErrInvalidPath, path)
		}
	}
	for seg := range strings.SplitSeq(path, "/") {
		switch seg {
		case "":
			return fmt.Errorf("%w: %q has an empty segment", ErrInvalidPath, path)
		case ".", "..":
			return fmt.Errorf("%w: %q has a %q segment", ErrInvalidPath, path, seg)
		}
	}
	return nil
}

// ValidateDir is ValidatePath for a listing, where "" means the root.
func ValidateDir(dir string) error {
	if dir == "" {
		return nil
	}
	return ValidatePath(dir)
}
