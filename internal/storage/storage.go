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
	"math"
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
	// ErrUploadOffset is an append that does not start where the store left
	// off. It is what a client learns from, so it is a sentinel rather than a
	// driver's own words: tus answers 409 to it and the client asks again.
	ErrUploadOffset = errors.New("storage: upload offset mismatch")
)

// Unlimited is what FreeSpace answers when the backend cannot say how much room
// is left -- which for an object store is the truth rather than an evasion.
//
// A number and not a second return value or a sentinel error, because the only
// thing a caller does with free space is compare it against what it is about to
// write, and this makes the store that cannot say need no special case at all.
const Unlimited = math.MaxInt64

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

	// FreeSpace reports how many bytes can still be written, or Unlimited when
	// this backend has no number to give.
	//
	// It exists because a copy of a collection is the first thing this server
	// does that can predictably fill a disk, and the worst moment to find that
	// out is thirty gigabytes in, with the database on the same disk (#43). A
	// caller asks before it writes.
	//
	// It is a best answer and not a reservation: nothing stops another process
	// filling the disk between this call and the write. What it rules out is
	// starting something that never had room.
	FreeSpace(ctx context.Context) (int64, error)

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
	//
	// An iterator rather than a slice, and that is the rule for every query
	// method written after this one: a result the caller has bounded comes back
	// as a slice, a result bounded only by how much is stored comes back as an
	// iterator. A bucket holds a hundred thousand photos; a listing of it must
	// not have to fit in memory first.
	List(ctx context.Context, prefix string) iter.Seq2[ObjectInfo, error]

	// StartUpload begins a write that arrives over several calls and returns
	// the id naming it until it is completed or abandoned.
	//
	// This half of the port exists because a PUT is all or nothing: a phone
	// uploading a four-gigabyte video over mobile data restarts from zero on
	// every drop, and no retry loop above the port can change that (#122). Both
	// backends can honour it truthfully -- a file grows, a multipart upload
	// gains parts -- which is why it is here rather than an optional interface
	// a caller has to test for.
	//
	// Nothing an upload has accepted is visible to Get, Stat or List until
	// CompleteUpload. That is what keeps an upload in flight out of reach of
	// the sweep in internal/files, which deletes what no row points at.
	StartUpload(ctx context.Context, key string) (string, error)

	// AppendUpload writes r at offset and returns the offset after it.
	//
	// offset is a precondition and not a seek: it must be what the store has
	// already accepted, and anything else is ErrUploadOffset. That is what
	// makes a retried append -- which is what a timeout produces -- a refusal
	// rather than duplicated bytes, and it is the reason this port can be
	// honest on S3, where there is no way to fill a hole after the fact.
	//
	// The size of r is the caller's business. A backend that wants its writes a
	// certain size arranges that for itself rather than making a phone learn
	// its rules.
	AppendUpload(ctx context.Context, key, id string, offset int64, r io.Reader) (int64, error)

	// UploadOffset reports how much of the upload the store has accepted, which
	// is what a resumed upload asks after anything has restarted.
	UploadOffset(ctx context.Context, key, id string) (int64, error)

	// CompleteUpload publishes what was accepted as the object at key,
	// replacing whatever was there, and ends the upload.
	CompleteUpload(ctx context.Context, key, id string) (ObjectInfo, error)

	// AbortUpload discards an upload and everything it accepted. Like Delete it
	// is idempotent: an upload that is already gone is not an error.
	AbortUpload(ctx context.Context, key, id string) error
}
