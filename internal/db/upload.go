package db

import (
	"context"
	"iter"
	"time"
)

// Upload is a write that arrives over several requests and has to survive
// between them.
//
// It is a row rather than something held in memory for one reason: the point of
// a resumable upload is that a phone can come back to it after a tunnel, a
// restart or a night, and a server that forgot where it was would make the
// client start again -- which is the thing being fixed (#122).
//
// What it does not hold is the bytes. Those are in the blob store under BlobKey,
// invisible to a listing until the upload completes, which is what keeps the
// sweep in internal/files from treating them as garbage.
type Upload struct {
	// ID is the name the client knows this upload by.
	ID string
	// OwnerID is who it belongs to, and is checked on every call: an upload id
	// is a capability, and one owner must not be able to resume another's.
	OwnerID string
	// Path is where the file lands when the upload completes.
	Path string
	// Size is how long the client said the whole thing would be, or -1 when it
	// declined to say.
	Size int64
	// Received is what the blob store has accepted and can be resumed from. It
	// is the store's number, not a count of what arrived: a chunk that failed
	// halfway leaves the two disagreeing and only the store's is true.
	Received int64
	// BlobKey is where the bytes are going, and StoreID is what the blob store
	// calls the upload in progress.
	BlobKey string
	StoreID string
	// Digest is the running SHA-256 of what has been received, marshalled
	// between requests. Carried rather than recomputed because the alternative
	// is reading a four-gigabyte object back to hash it at the end.
	Digest []byte
	// MIMEType is what the completed file will be recorded as.
	MIMEType string
	// ExpiresAt is when an untouched upload may be collected. It is told to the
	// client, so it cannot be a guess, and it cannot outlive what the blob
	// store will keep either.
	ExpiresAt time.Time
}

// Uploads is the repository for uploads in progress.
type Uploads interface {
	// PutUpload records an upload or replaces the record of one, which is how
	// progress is saved: the row is small and rewriting it whole is one
	// statement rather than a column-by-column update per chunk.
	PutUpload(ctx context.Context, u Upload) error

	// UploadByID returns the upload, or ErrNotFound. The owner is part of the
	// lookup rather than checked afterwards, so a wrong one is indistinguishable
	// from an upload that does not exist.
	UploadByID(ctx context.Context, owner, id string) (Upload, error)

	// DeleteUpload forgets one. Like a blob delete it is idempotent: completing
	// and abandoning both end here, and so does a client that gave up twice.
	DeleteUpload(ctx context.Context, owner, id string) error

	// ExpiredUploads yields every upload past its deadline, for whoever is
	// collecting them. Not scoped to an owner, for the same reason BlobKeys is
	// not: a collector that saw one owner would leak another's.
	ExpiredUploads(ctx context.Context, now time.Time) iter.Seq2[Upload, error]
}
