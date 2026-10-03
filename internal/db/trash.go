package db

import (
	"context"
	"iter"
	"time"
)

// Trashed is a row that was deleted and is being kept for a while (#274).
//
// It is a table of its own and not a flag on files, and that is the decision
// worth knowing about. A flag would need every read query in every driver to
// filter -- both listings, the five buckets of a search, the gallery, the
// albums, the counts on /status -- and the one that forgot would show deleted
// files in silence, for ever. It would also collide: the unique index is
// (owner_id, path), so deleting a.txt and writing a new a.txt is two rows with
// one path. Here there is nothing to collide, two deletions of one name are
// two rows, and files keeps meaning the library.
//
// What does not move is the blob. Nothing is copied: the bytes stay exactly
// where they were written, and the only thing that changed is which table says
// who they belong to.
type Trashed struct {
	// Batch is the deletion this row arrived in. Deleting a folder of a
	// thousand photographs is a thousand rows and one deletion, and the page
	// shows deletions.
	Batch string
	// File is the row as it was, so that putting it back is an insert and not
	// a reconstruction (#275). ID is the id it had; nothing depends on it, and
	// a restored row gets a new one.
	File
	DeletedAt time.Time
}

// TrashBatch is one deletion, which is what a page of the trash lists.
//
// Root is the shortest path in it, which for a tree is the folder that was
// deleted: "holiday" sorts before "holiday/sunset.jpg", and every row in the
// batch is under it.
type TrashBatch struct {
	ID        string
	Root      string
	Files     int64
	Bytes     int64
	DeletedAt time.Time
}

// TrashCursor is where a page of the trash resumes. Newest first, because a
// trash is read from the accident backwards, and the batch id breaks a tie
// between two deletions in the same millisecond.
type TrashCursor struct {
	DeletedAt time.Time
	Batch     string
}

// AtStart reports whether c is the beginning of the listing rather than a
// position in it.
func (c TrashCursor) AtStart() bool { return c.Batch == "" }

// TrashTotals is the whole trash in two numbers, for the status page.
type TrashTotals struct {
	Files int64
	Bytes int64
}

// Trash is the repository for what has been deleted and not yet destroyed.
type Trash interface {
	// Trash records rows as deleted. The caller has already removed them from
	// files, in the same transaction: the two halves are one move, and a crash
	// between them would be a file that is in neither place.
	Trash(ctx context.Context, batch string, rows []File, at time.Time) error

	// TrashBatches is a page of deletions, newest first.
	TrashBatches(ctx context.Context, owner string, after TrashCursor, limit int) ([]TrashBatch, error)

	// TrashedIn is what one deletion holds, or an empty slice when there is no
	// such batch -- which is not an error, because a client pressing a button
	// twice is not one.
	TrashedIn(ctx context.Context, owner, batch string) ([]Trashed, error)

	// DeleteTrash forgets a whole deletion. The caller drops the blobs once
	// the transaction has committed, which is the same order every delete here
	// follows: row first, bytes second.
	DeleteTrash(ctx context.Context, owner, batch string) error

	// TrashKeys yields every blob key the trash is holding, for the sweep to
	// add to what it considers referenced. Not scoped to an owner, for the
	// reason BlobKeys is not: a sweep that saw one owner would delete another's
	// trash.
	TrashKeys(ctx context.Context) iter.Seq2[string, error]

	// ExpiredTrash yields everything deleted before a time, for the pass that
	// empties it. Not scoped to an owner either, and ordered oldest first so a
	// pass that stops halfway has made progress.
	ExpiredTrash(ctx context.Context, before time.Time) iter.Seq2[Trashed, error]

	// TrashTotals is how much room the trash is holding onto, which is the one
	// number a status page needs.
	TrashTotals(ctx context.Context, owner string) (TrashTotals, error)
}
