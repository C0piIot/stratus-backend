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
	// The order is by path, because a listing that changes order between calls
	// is useless to a sync client.
	//
	// A slice rather than an iterator, on the same rule storage.Storage.List
	// states from the other side: one directory is bounded by what the caller
	// asked for, so it comes back whole. A recursive walk or a library-wide
	// scan is not, and will be an iterator when it arrives.
	ListFiles(ctx context.Context, owner, dir string) ([]File, error)

	// MoveFile renames from to to. It returns ErrNotFound if there is nothing
	// at from, and ErrConflict if something already sits at to or if from is a
	// directory that still has something in it.
	//
	// Moving a whole subtree is a rewrite of every descendant and arrives with
	// the protocol that needs it; refusing is what keeps this from silently
	// orphaning them in the meantime.
	MoveFile(ctx context.Context, owner, from, to string) error

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

// Repo is every repository at once, which is what a transaction hands out: a
// unit of work may well span features -- deleting a collection is file rows and
// calendar objects together.
//
// Splitting it from Store is what keeps Migrate and Close out of reach inside a
// transaction, where neither means anything.
type Repo interface {
	Files
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
