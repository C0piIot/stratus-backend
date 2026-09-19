// Package db is the metadata database port: the repository interface every
// driver implements, the sentinel errors that cross it, and the entities that
// live next to it.
//
// It is one of exactly two pluggable seams in Stratus. Two guarantees are part
// of the contract rather than an accident of the first driver:
//
//   - **Transactions.** A driver that cannot roll back a group of writes cannot
//     implement this port. Moving a collection, deleting one, and bumping a sync
//     token are each several rows that must land together or not at all.
//   - **Relational queries.** Listing a directory is an indexed lookup, not a
//     scan the caller filters.
//
// That is a deliberate door closed on key-value and document stores. A port
// that also had to satisfy them would be the lowest common denominator, and
// every feature above it would reimplement joins and transactions in Go.
package db

import (
	"context"
	"errors"
	"iter"
	"time"
)

// Sentinel errors crossing the port. Drivers return these, wrapped with
// whatever detail they have, so a caller can classify a failure without knowing
// which one produced it.
var (
	ErrNotFound    = errors.New("db: not found")
	ErrConflict    = errors.New("db: already exists")
	ErrInvalidPath = errors.New("db: invalid path")
)

// Files is the repository for file rows.
//
// Feature packages take this, not Store: internal/files has no business being
// handed something that can also search music, and a fake for it in a test is
// five methods rather than everything the database can do.
//
// Calendar objects and tracks get interfaces of their own beside this one as
// they arrive, and Repo composes them. Growing a single interface instead would
// end with every feature depending on all of them.
type Files interface {
	// PutFile inserts f, or replaces the file already at its path. The returned
	// File carries the stored row, including its ID and the MTime as persisted.
	//
	// Writing over a directory is ErrConflict: a file replacing a collection
	// would orphan everything under it.
	PutFile(ctx context.Context, f File) (File, error)

	// CreateDir records an empty collection, and returns ErrConflict if
	// anything already sits at path.
	//
	// Directories are rows rather than something inferred from the paths of the
	// files under them, because an empty one has no files under it and MKCOL
	// has to be able to make one.
	CreateDir(ctx context.Context, owner, path string) (File, error)

	// FileByPath returns the file at path, or ErrNotFound.
	FileByPath(ctx context.Context, owner, path string) (File, error)

	// ListFiles returns the direct children of dir, which is "" for the root.
	// Direct children only: this is PROPFIND with Depth 1, and a recursive walk
	// is a different query that arrives when something needs it.
	//
	// Directories first and then by path. Stable, because a listing that
	// changed order between calls would be useless to a sync client -- and in
	// that order rather than by path alone because it is the one the index is
	// built in, which is the difference between reading a folder and scanning
	// the library (#160). ListFilesPage has always answered in this order, so
	// the two halves of "list a directory" now agree.
	//
	// A slice rather than an iterator, on the same rule storage.Storage.List
	// states from the other side: one directory is bounded by what the caller
	// asked for, so it comes back whole. A recursive walk or a library-wide
	// scan is not, and will be an iterator when it arrives.
	//
	// "Bounded by what the caller asked for" is exactly as true as it was and
	// no longer the whole story: PROPFIND, the two Subsonic browse calls and
	// the folder-cover lookup each need a directory entire and take it here,
	// while a browser rendering one takes ListFilesPage instead. Which of the
	// two a caller wants is a property of the caller, which is why this is two
	// methods rather than one with a limit nobody would pass.
	ListFiles(ctx context.Context, owner, dir string) ([]File, error)

	// ListFilesPage returns at most limit children of dir, resuming after the
	// row named by the cursor -- a keyset page rather than LIMIT with an
	// OFFSET, which would make the database count past everything it skips and
	// would repeat or drop a row when the tree changes underneath a reader.
	//
	// The order is directories first and then by path, which is what a file
	// manager shows and what the whole-listing caller above gets by sorting
	// what it was given. Over a page it cannot be done afterwards: the grouping
	// would hold inside each page and break at every boundary, so it is the
	// query's job and the cursor carries both halves of the key.
	//
	// A cursor whose row has since been deleted still resumes in the right
	// place: it is a position in an ordering, not a row that has to exist.
	ListFilesPage(ctx context.Context, owner, dir string, after Cursor, limit int) ([]File, error)

	// MoveFile renames from to to, and everything under it when from is a
	// directory. It returns ErrNotFound if there is nothing at from, and
	// ErrConflict if something already sits at to or if to is inside from.
	//
	// The whole subtree moves in one statement and therefore in one
	// transaction: a rename that rewrote half of a tree would leave the other
	// half pointing at a parent that no longer exists, which no surface could
	// recover from. Nothing is copied and no blob is touched -- a path is a
	// column, and what it names does not know what it is called.
	MoveFile(ctx context.Context, owner, from, to string) error

	// SubtreeSize is the number of bytes the files under dir add up to, dir
	// itself included, and 0 for a directory with nothing in it. An empty dir
	// is the whole tree.
	//
	// It exists for RFC 4331's quota-used-bytes, which is what a mounted volume
	// draws its bar from (#136). Computed and not kept: a running total on
	// directory rows would mean updating every ancestor on every write and
	// moving totals between two chains on every rename, three times over, and
	// lying the first time any of that went wrong.
	//
	// Cheap because it is a range and not a prefix match: on a hundred thousand
	// files it is 0.10 ms for a folder of a hundred and 63 ms for the root,
	// which is the one case that walks everything because it was asked about
	// everything.
	SubtreeSize(ctx context.Context, owner, dir string) (int64, error)

	// BlobKeys yields the blob key of every file row, which is what the
	// collector subtracts from what the blob store actually holds.
	//
	// Not scoped to an owner, unlike everything else here: a collector that
	// only saw one owner's rows would delete another owner's blobs the day
	// sharing arrives.
	//
	// An iterator rather than a slice: the result is bounded by how much is
	// stored, not by what the caller asked for.
	BlobKeys(ctx context.Context) iter.Seq2[string, error]

	// DeleteFile removes the file at path, or returns ErrNotFound.
	//
	// Unlike a blob delete, this is not idempotent: the caller is asking about
	// a row it believes exists, and "it was already gone" is information worth
	// keeping rather than a detail to smooth over.
	//
	// A directory that still has something in it is ErrConflict, for the same
	// reason a move of one is.
	DeleteFile(ctx context.Context, owner, path string) error
}

// MediaIndex is the repository for extracted metadata. Separate from Files for
// the reason in #29: a feature takes the interface it uses, and the indexer has
// no business being handed something that can move files around.
type MediaIndex interface {
	// PutMedia stores what an extractor found, replacing any earlier attempt.
	PutMedia(ctx context.Context, m Media) error

	// MediaByFile returns the metadata for a file, or ErrNotFound.
	MediaByFile(ctx context.Context, fileID int64) (Media, error)

	// PendingMedia returns files that have never been indexed, were indexed by
	// an extractor older than version, were indexed from different bytes than
	// the ones the row now points at, or failed in a way worth trying again by
	// now.
	//
	// The queue is this query rather than a table: nothing is enqueued, nothing
	// is dequeued, a restart loses nothing, and a row that appears -- written by
	// an upload or inserted by an import -- turns up on its own. What the query
	// cannot see is a rename that changes an extension, since the bytes and
	// therefore the validator are the same while the kind is not.
	//
	// now is the caller's, not the database's: the two clocks are not the same
	// one, and a test that has to wait an hour is a test nobody runs.
	PendingMedia(ctx context.Context, version int, now time.Time, limit int) ([]File, error)

	// MediaCounts says how much of the library is indexed, for the page that
	// reports it. One query, and it walks every file row: there is no way to
	// count an absence from an index, and this is asked for by somebody looking
	// at a page rather than by a loop.
	MediaCounts(ctx context.Context, version int) (MediaCounts, error)

	// MediaStates returns what is known about the metadata of the files named,
	// for the ones that have a row at all. It exists for a listing that wants
	// to mark what has not been indexed yet, so the ids are one page of one and
	// the caller is what bounds them.
	MediaStates(ctx context.Context, fileIDs []int64) (map[int64]MediaState, error)
}

// MediaCounts is the state of the library in four numbers. Pending is every
// file that is not Indexed and did not Fail, which is what the queue would
// hand out next.
//
// A file whose last attempt failed on the way to the bytes counts as pending
// and not as failed: it is going to be tried again, and reporting it as
// unreadable would send somebody looking at a file that is fine.
type MediaCounts struct {
	Files   int64
	Indexed int64
	Failed  int64
}

// Pending is what is left to do.
func (c MediaCounts) Pending() int64 { return c.Files - c.Indexed - c.Failed }

// MediaState is the part of a media row a listing needs to say whether a file
// has been looked at, without reading everything an extractor found.
type MediaState struct {
	Version int
	ETag    string
	Failed  bool
}

// Repo is every repository at once, which is what a transaction hands out: a
// unit of work may well span features -- deleting a collection is file rows and
// calendar objects together.
//
// Splitting it from Store is what keeps Migrate and Close out of reach inside a
// transaction, where neither means anything.
type Repo interface {
	Files
	MediaIndex
	Music
	Uploads
}

// Store is a database connection.
type Store interface {
	Repo

	// Tx runs fn inside a transaction, committing when it returns nil and
	// rolling back on an error or a panic.
	Tx(ctx context.Context, fn func(Repo) error) error

	// Migrate brings the schema up to the version this binary knows. It runs at
	// startup, and it doubles as the write probe: a database the process cannot
	// create tables in has to fail there rather than on the first upload.
	Migrate(ctx context.Context) error

	// Ping checks the connection is usable.
	Ping(ctx context.Context) error

	// Close releases the connection pool.
	Close() error
}
