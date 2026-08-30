// Package storage is the blob storage port: the interface every backend
// implements, the sentinel errors that cross it, and the key validation that
// has to look identical from every backend.
//
// It is one of exactly two pluggable seams in Stratus. Nothing here knows about
// HTTP, and nothing outside internal/storage/<driver> knows how a key becomes a
// path on a disk or an object name in a bucket.
package storage

import (
	"context"
	"errors"
	"io"
	"iter"
	"time"
)

// Sentinel errors crossing the port. Backends return these, wrapped with
// whatever detail they have, so a caller can classify a failure without knowing
// which driver produced it.
var (
	ErrNotFound     = errors.New("storage: object not found")
	ErrInvalidKey   = errors.New("storage: invalid key")
	ErrInvalidRange = errors.New("storage: invalid range")
	ErrSizeMismatch = errors.New("storage: size mismatch")
)

// ObjectInfo is everything a backend can report about an object without reading
// it. Deliberately minimal: an ETag or a content hash is a decision for
// internal/files, which owns the naming, not for the port.
type ObjectInfo struct {
	// Key identifies the object. It is the key that was asked for, not a
	// backend-specific path.
	Key string
	// Size is the object length in bytes.
	Size int64
	// ModTime is when the object was last written.
	ModTime time.Time
}

// Storage is the blob seam. Implementations live in internal/storage/<driver>
// and must pass the conformance suite in internal/storage/storagetest.
//
// Every method validates its key with [ValidateKey] before touching anything.
// That is the chokepoint against path traversal, and it is the reason no
// adapter ever needs to suppress gosec's G304.
type Storage interface {
	// Put writes r under key, replacing any existing object. The replacement is
	// atomic: a concurrent Get sees either the old bytes or the new ones, never
	// a partial write.
	//
	// size is a hint. A negative size means "unknown"; a non-negative one that
	// does not match what r delivers fails with ErrSizeMismatch and leaves the
	// existing object untouched.
	Put(ctx context.Context, key string, r io.Reader, size int64) (ObjectInfo, error)

	// Get opens the part of the object selected by rng. The zero Range reads the
	// whole object. The caller closes the reader.
	Get(ctx context.Context, key string, rng Range) (io.ReadCloser, ObjectInfo, error)

	// Stat reports on an object without reading it, and returns ErrNotFound if
	// there is none under key.
	Stat(ctx context.Context, key string) (ObjectInfo, error)

	// Delete removes the object under key. It is idempotent: a key that is not
	// there is not an error. S3 answers 204 either way, and demanding
	// ErrNotFound would turn every delete into a HEAD plus a DELETE.
	Delete(ctx context.Context, key string) error

	// List yields every object whose key starts with prefix, where prefix is a
	// plain string prefix and not a directory path: "a/b" also matches "a/bc".
	// An empty prefix lists everything.
	//
	// The order is unspecified. A backend that walks a directory tree cannot
	// cheaply produce S3's flat lexical order -- "a!b" sorts before "a/b", but a
	// tree walk descends into "a/" first -- so callers that need an order sort
	// for themselves.
	//
	// A non-nil error ends the sequence; there is at most one, and it is the
	// last pair yielded.
	List(ctx context.Context, prefix string) iter.Seq2[ObjectInfo, error]
}
