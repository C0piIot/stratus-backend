package files

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding"
	"errors"
	"fmt"
	"hash"
	"io"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/storage"
)

// DefaultUploadTTL is how long an untouched upload survives.
//
// It is one number and not two on purpose: it is told to the client, and it
// cannot outlive what the blob store will keep -- the S3 backend abandons a
// multipart upload it has not heard about in a day -- so a longer deadline here
// would be a promise the storage quietly breaks.
const DefaultUploadTTL = 12 * time.Hour

// BeginUpload reserves a blob and records an upload that will arrive in pieces.
//
// The parent is checked here as well as at completion. Once, because a client
// that has mistyped a directory should learn before it spends an hour uploading
// into it; and again at the end, because the tree can change while it does.
func (s *Service) BeginUpload(ctx context.Context, owner, path string, size int64, mimeType string) (db.Upload, error) {
	if err := db.ValidatePath(path); err != nil {
		return db.Upload{}, err
	}
	if err := s.meta.Tx(ctx, func(r db.Repo) error {
		return s.requireParent(ctx, r, owner, path)
	}); err != nil {
		return db.Upload{}, err
	}

	key := newBlobKey(path)
	storeID, err := s.blobs.StartUpload(ctx, key)
	if err != nil {
		return db.Upload{}, fmt.Errorf("start the upload of %q: %w", path, err)
	}

	digest, err := marshalDigest(sha256.New())
	if err != nil {
		return db.Upload{}, err
	}

	u := db.Upload{
		ID:        rand.Text(),
		OwnerID:   owner,
		Path:      path,
		Size:      size,
		BlobKey:   key,
		StoreID:   storeID,
		Digest:    digest,
		MIMEType:  mimeType,
		ExpiresAt: time.Now().Add(DefaultUploadTTL).UTC(),
	}
	if err := s.meta.PutUpload(ctx, u); err != nil {
		// Nothing points at the upload the store just started, and nothing ever
		// will, so it goes now rather than waiting for the sweep.
		_ = s.blobs.AbortUpload(ctx, key, storeID)
		return db.Upload{}, err
	}
	return u, nil
}

// Upload returns an upload in progress, or db.ErrNotFound.
func (s *Service) Upload(ctx context.Context, owner, id string) (db.Upload, error) {
	return s.meta.UploadByID(ctx, owner, id)
}

// AppendUpload writes the next piece and returns the upload as it now stands.
//
// offset is the client's belief about where it got to, and it is refused rather
// than trusted: the store is the only thing that knows, and a retried chunk
// must be a refusal rather than bytes written twice.
func (s *Service) AppendUpload(ctx context.Context, owner, id string, offset int64, body io.Reader) (db.Upload, error) {
	u, err := s.meta.UploadByID(ctx, owner, id)
	if err != nil {
		return db.Upload{}, err
	}

	// An empty digest means an earlier chunk already broke the running hash, so
	// there is nothing to carry on and nothing to feed.
	var digest hash.Hash
	if len(u.Digest) > 0 {
		var derr error
		if digest, derr = unmarshalDigest(u.Digest); derr != nil {
			return db.Upload{}, derr
		}
	}

	counted := &countingReader{r: body}
	if digest != nil {
		counted.r = io.TeeReader(body, digest)
	}
	at, appendErr := s.blobs.AppendUpload(ctx, u.BlobKey, u.StoreID, offset, counted)

	// The hash covers what was read; the store reports what it accepted. They
	// agree even when a client disappears mid-chunk, because a copy writes what
	// it reads -- what breaks them apart is a store that read the bytes and
	// could not keep them, a full disk or a bucket that stopped answering. The
	// hash is then ahead of the object and cannot be wound back, so it is
	// dropped and CompleteUpload reads the object to hash it instead. That is
	// the expensive path and it is the rare one.
	//
	// Empty rather than null: "no hash" is a state of the upload, not a missing
	// column, and the schema says so on all three drivers.
	switch {
	case digest == nil, at != u.Received+counted.n:
		u.Digest = []byte{}
	default:
		var merr error
		if u.Digest, merr = marshalDigest(digest); merr != nil {
			return db.Upload{}, merr
		}
	}
	u.Received = at
	u.ExpiresAt = time.Now().Add(DefaultUploadTTL).UTC()

	// Saved even when the append failed: the offset it reported is where the
	// store actually is, and forgetting it would make the client resend what
	// did land.
	if err := s.meta.PutUpload(ctx, u); err != nil {
		return db.Upload{}, err
	}
	if appendErr != nil {
		return u, appendErr
	}
	return u, nil
}

// CompleteUpload publishes the upload as a file and forgets it.
func (s *Service) CompleteUpload(ctx context.Context, owner, id string) (db.File, error) {
	u, err := s.meta.UploadByID(ctx, owner, id)
	if err != nil {
		return db.File{}, err
	}
	if u.Size >= 0 && u.Received != u.Size {
		return db.File{}, fmt.Errorf("%w: the upload of %q has %d bytes of %d",
			db.ErrConflict, u.Path, u.Received, u.Size)
	}

	info, err := s.blobs.CompleteUpload(ctx, u.BlobKey, u.StoreID)
	if err != nil {
		return db.File{}, fmt.Errorf("complete the upload of %q: %w", u.Path, err)
	}

	tag, err := s.uploadETag(ctx, u)
	if err != nil {
		return db.File{}, err
	}

	f := db.File{
		OwnerID:  owner,
		Path:     u.Path,
		BlobKey:  u.BlobKey,
		Size:     info.Size,
		MTime:    info.ModTime,
		ETag:     tag,
		MIMEType: u.MIMEType,
	}
	err = s.meta.Tx(ctx, func(r db.Repo) error {
		if perr := s.requireParent(ctx, r, owner, u.Path); perr != nil {
			return perr
		}
		stored, perr := r.PutFile(ctx, f)
		if perr != nil {
			return perr
		}
		f = stored
		return r.DeleteUpload(ctx, owner, id)
	})
	if err != nil {
		// Same bargain as Write: the blob is there and no row points at it, so
		// the sweep takes it.
		_ = s.blobs.Delete(ctx, u.BlobKey)
		return db.File{}, err
	}
	return f, nil
}

// AbortUpload throws an upload away. Idempotent, like everything else a client
// can retry.
func (s *Service) AbortUpload(ctx context.Context, owner, id string) error {
	u, err := s.meta.UploadByID(ctx, owner, id)
	if errors.Is(err, db.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := s.blobs.AbortUpload(ctx, u.BlobKey, u.StoreID); err != nil {
		return err
	}
	return s.meta.DeleteUpload(ctx, owner, id)
}

// CollectUploads throws away the uploads nobody came back to.
//
// Nothing else will: an upload in progress is invisible to a listing, which is
// what keeps the blob sweep from eating it, and the same invisibility means the
// blob sweep will never clean up after it either.
func (s *Service) CollectUploads(ctx context.Context, now time.Time) (int, error) {
	var expired []db.Upload
	for u, err := range s.meta.ExpiredUploads(ctx, now) {
		if err != nil {
			return 0, err
		}
		expired = append(expired, u)
	}

	var done int
	for _, u := range expired {
		if err := s.blobs.AbortUpload(ctx, u.BlobKey, u.StoreID); err != nil {
			return done, err
		}
		if err := s.meta.DeleteUpload(ctx, u.OwnerID, u.ID); err != nil {
			return done, err
		}
		done++
	}
	return done, nil
}

// uploadETag is the running hash when it survived, and the object read back
// when it did not.
func (s *Service) uploadETag(ctx context.Context, u db.Upload) (string, error) {
	if len(u.Digest) > 0 {
		digest, err := unmarshalDigest(u.Digest)
		if err != nil {
			return "", err
		}
		return etag(digest), nil
	}

	body, _, err := s.blobs.Get(ctx, u.BlobKey, storage.All())
	if err != nil {
		return "", fmt.Errorf("read back the upload of %q: %w", u.Path, err)
	}
	defer func() { _ = body.Close() }()

	digest := sha256.New()
	if _, err := io.Copy(digest, body); err != nil {
		return "", fmt.Errorf("hash the upload of %q: %w", u.Path, err)
	}
	return etag(digest), nil
}

// countingReader is how much of a chunk was read, which is compared with how
// much the store says it accepted.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// marshalDigest and unmarshalDigest carry a half-finished SHA-256 between
// requests, which is the only reason a resumable upload does not have to read
// everything back to hash it at the end.
//
// The type assertions are unchecked deliberately: crypto/sha256's hash has
// marshalled its state since Go 1.8, so a failure here is the standard library
// having changed under us rather than anything a caller can cause or handle.
func marshalDigest(h hash.Hash) ([]byte, error) {
	b, err := h.(encoding.BinaryMarshaler).MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("files: save the upload's hash: %w", err)
	}
	return b, nil
}

func unmarshalDigest(b []byte) (hash.Hash, error) {
	h := sha256.New()
	if err := h.(encoding.BinaryUnmarshaler).UnmarshalBinary(b); err != nil {
		return nil, fmt.Errorf("files: restore the upload's hash: %w", err)
	}
	return h, nil
}
