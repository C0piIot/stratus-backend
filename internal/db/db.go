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
)

// Sentinel errors crossing the port. Drivers return these, wrapped with
// whatever detail they have, so a caller can classify a failure without knowing
// which one produced it.
var (
	ErrNotFound    = errors.New("db: not found")
	ErrConflict    = errors.New("db: already exists")
	ErrInvalidPath = errors.New("db: invalid path")
)

// Repo is the set of operations a connection and a transaction can both serve.
// Splitting it from Store is what keeps Migrate and Close out of reach inside a
// transaction, where neither means anything.
type Repo interface {
	// PutFile inserts f, or replaces the file already at its path. The returned
	// File carries the stored row, including its ID and the MTime as persisted.
	PutFile(ctx context.Context, f File) (File, error)

	// FileByPath returns the file at path, or ErrNotFound.
	FileByPath(ctx context.Context, owner, path string) (File, error)

	// ListFiles returns the direct children of dir, which is "" for the root.
	// Direct children only: this is PROPFIND with Depth 1, and a recursive walk
	// is a different query that arrives when something needs it.
	//
	// The order is by path, because a listing that changes order between calls
	// is useless to a sync client.
	ListFiles(ctx context.Context, owner, dir string) ([]File, error)

	// MoveFile renames from to to. It returns ErrNotFound if there is nothing
	// at from, and ErrConflict if something already sits at to.
	MoveFile(ctx context.Context, owner, from, to string) error

	// DeleteFile removes the file at path, or returns ErrNotFound.
	//
	// Unlike a blob delete, this is not idempotent: the caller is asking about
	// a row it believes exists, and "it was already gone" is information worth
	// keeping rather than a detail to smooth over.
	DeleteFile(ctx context.Context, owner, path string) error
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
