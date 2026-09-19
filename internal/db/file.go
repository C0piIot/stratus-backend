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

// Cursor is where a page of a listing resumes: the last row of the page before
// it, in the ordering ListFilesPage promises. The zero value is the start of
// the listing, which no row can name -- a path is never empty.
//
// Deliberately not opaque. It is a path the caller already has and is allowed
// to see, so a surface can put it in a URL without encoding a secret, and IsDir
// is there because the ordering groups collections first: the pair is the sort
// key, and a cursor that carried half of it could not resume across the seam
// between the two groups.
type Cursor struct {
	IsDir bool
	Path  string
}

// After returns the cursor that resumes a listing after f.
func After(f File) Cursor { return Cursor{IsDir: f.IsDir, Path: f.Path} }

// AtStart reports whether c is the beginning of a listing rather than a
// position in one.
func (c Cursor) AtStart() bool { return c.Path == "" }

// ValidateLimit refuses a page size the dialects would not agree on. SQLite
// reads a negative LIMIT as no limit at all and PostgreSQL refuses one, so a
// caller's bug would be a full listing on one driver and an error on another.
// Here rather than in each of them, for the reason ValidateMove is.
func ValidateLimit(limit int) error {
	if limit <= 0 {
		return fmt.Errorf("db: a page has to ask for at least one row, not %d", limit)
	}
	return nil
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

// ValidateMove reports whether from can be renamed to to, for the two ways that
// question has an answer before any row is read.
//
// Moving a directory into itself is the one that matters: `a` to `a/b` would
// rewrite the subtree into a place inside the subtree, and the statement doing
// the rewriting would be reading rows it had already written. Refused here,
// where every driver shares it, rather than in each of them.
func ValidateMove(from, to string) error {
	if err := ValidatePath(from); err != nil {
		return err
	}
	if err := ValidatePath(to); err != nil {
		return err
	}
	if from == to {
		return fmt.Errorf("%w: %q is already where it is", ErrConflict, from)
	}
	if strings.HasPrefix(to, from+"/") {
		return fmt.Errorf("%w: %q is inside %q", ErrConflict, to, from)
	}
	return nil
}

// SubtreeRange is the half-open range of paths under dir, for the queries that
// have to add something up over a whole branch.
//
// A range and not a prefix match, and that is the whole of why it is cheap: a
// LIKE cannot seek, while `path >= "album/" AND path < "album0"` walks the
// unique index on (owner_id, path) from where the branch starts to where it
// ends -- 0.10 ms for a folder of a hundred on a library of a hundred thousand,
// against a scan of all of it.
//
// The upper bound is the separator incremented, which works because '/' is 0x2F
// and every path under "album/" sorts below "album0". whole is true for the
// root, which has no prefix to bound and is every row this owner has.
//
// It lives here rather than in a driver for the reason ValidateMove does: the
// three would otherwise get it subtly different in three ways.
func SubtreeRange(dir string) (from, to string, whole bool) {
	if dir == "" {
		return "", "", true
	}
	from = dir + "/"
	return from, dir + string(rune('/'+1)), false
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
