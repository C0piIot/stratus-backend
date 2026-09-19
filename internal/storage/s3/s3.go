// Package s3 implements the storage port on any S3-compatible object store.
//
// It is the second backend on purpose. A conformance suite with one
// implementation only records that implementation's habits; this package is
// what turns internal/storage/storagetest into a contract.
package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/C0piIot/stratus-backend/internal/storage"
)

// Config is everything this backend needs. It is a struct rather than a DSN
// because parsing STRATUS_STORAGE_DSN, and redacting its secrets, belongs to
// internal/config.
type Config struct {
	// Endpoint is host or host:port, with no scheme.
	Endpoint string
	// Bucket must already exist; this backend does not create it.
	Bucket string
	// AccessKey and SecretKey are the static credentials.
	AccessKey string
	SecretKey string
	// Region may be empty, in which case the server is asked.
	Region string
	// UseTLS talks https. Off is for a self-hosted S3 on a private network.
	UseTLS bool
	// SpoolDir is where a resumable upload's tail waits until it is a whole
	// part. S3 refuses a multipart part under MinPartSize unless it is the
	// last, and the port promises a caller that its chunks can be any size, so
	// the difference is held here rather than pushed back at a phone.
	//
	// Local and losable on purpose: what it holds is bytes a client can send
	// again. Losing it fails an upload in flight rather than corrupting one,
	// because UploadOffset counts what is in it -- see StartUpload.
	SpoolDir string
}

// Store is a storage.Storage backed by an S3-compatible bucket.
type Store struct {
	client *minio.Client
	core   minio.Core
	bucket string
	spool  string
}

var _ storage.Storage = (*Store)(nil)

// New connects to the object store and checks the bucket is reachable.
//
// The check is deliberate, and it is the same idea as EnsureDataDir in
// internal/app: bad credentials or a missing bucket must stop the process at
// startup, not surface on somebody's first upload.
func New(ctx context.Context, cfg Config) (*Store, error) {
	switch {
	case cfg.Endpoint == "":
		return nil, errors.New("s3: endpoint is required")
	case cfg.Bucket == "":
		return nil, errors.New("s3: bucket is required")
	case cfg.AccessKey == "" || cfg.SecretKey == "":
		return nil, errors.New("s3: access key and secret key are required")
	case cfg.SpoolDir == "":
		return nil, errors.New("s3: spool directory is required")
	}
	if err := os.MkdirAll(cfg.SpoolDir, 0o750); err != nil {
		return nil, fmt.Errorf("s3: create the spool directory %s: %w", cfg.SpoolDir, err)
	}

	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseTLS,
		Region: cfg.Region,
	})
	if err != nil {
		// Nothing here interpolates the credentials: an error message is a log
		// line waiting to happen.
		return nil, fmt.Errorf("s3: connect to %s: %w", cfg.Endpoint, err)
	}

	exists, err := client.BucketExists(ctx, cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("s3: check bucket %q on %s: %w", cfg.Bucket, cfg.Endpoint, err)
	}
	if !exists {
		return nil, fmt.Errorf("s3: bucket %q does not exist on %s", cfg.Bucket, cfg.Endpoint)
	}

	store := &Store{client: client, core: minio.Core{Client: client}, bucket: cfg.Bucket, spool: cfg.SpoolDir}
	if err := store.abortStaleUploads(ctx); err != nil {
		return nil, err
	}
	return store, nil
}

// staleUpload is how long an unfinished multipart upload has to sit before it
// is treated as abandoned. Unlike the disk backend's reserved directory, a
// bucket may be shared, and aborting somebody else's upload in progress would
// be worse than paying for a day of orphaned parts.
const staleUpload = 24 * time.Hour

// abortStaleUploads clears multipart uploads nobody completed. They are invisible
// to a listing and billed until something aborts them, so nothing else will
// notice they are there.
func (s *Store) abortStaleUploads(ctx context.Context) error {
	return s.abortUploadsBefore(ctx, time.Now().Add(-staleUpload))
}

// abortUploadsBefore takes the cutoff as an argument so the behaviour can be
// exercised without waiting a day for one to go stale.
func (s *Store) abortUploadsBefore(ctx context.Context, cutoff time.Time) error {
	for upload := range s.client.ListIncompleteUploads(ctx, s.bucket, "", true) {
		if upload.Err != nil {
			return fmt.Errorf("s3: list incomplete uploads: %w", upload.Err)
		}
		if upload.Initiated.After(cutoff) {
			continue
		}
		if err := s.client.RemoveIncompleteUpload(ctx, s.bucket, upload.Key); err != nil {
			return fmt.Errorf("s3: abort the incomplete upload of %q: %w", upload.Key, err)
		}
	}
	return nil
}

// Put implements storage.Storage.
func (s *Store) Put(ctx context.Context, key string, r io.Reader, size int64) (storage.ObjectInfo, error) {
	if err := storage.ValidateKey(key); err != nil {
		return storage.ObjectInfo{}, err
	}
	if err := ctx.Err(); err != nil {
		return storage.ObjectInfo{}, err
	}

	// storage.ExactReader is load-bearing: with a known size the client sends
	// exactly that many bytes and ignores the rest, so an over-long body would
	// otherwise be stored truncated and reported as a success.
	if _, err := s.client.PutObject(ctx, s.bucket, key, storage.ExactReader(r, size), size, minio.PutObjectOptions{}); err != nil {
		return storage.ObjectInfo{}, mapErr(key, err)
	}
	// A PUT response carries no Last-Modified, so the timestamp comes from a
	// stat rather than from an invented time.Now().
	return s.Stat(ctx, key)
}

// Get implements storage.Storage.
func (s *Store) Get(ctx context.Context, key string, rng storage.Range) (io.ReadCloser, storage.ObjectInfo, error) {
	if err := storage.ValidateKey(key); err != nil {
		return nil, storage.ObjectInfo{}, err
	}
	if err := ctx.Err(); err != nil {
		return nil, storage.ObjectInfo{}, err
	}

	// The whole object needs no size up front, so it costs one request.
	if rng == storage.All() {
		obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
		if err != nil {
			return nil, storage.ObjectInfo{}, mapErr(key, err)
		}
		// GetObject is lazy: nothing is sent until the first read or stat, so
		// this is where a missing object actually surfaces.
		oi, err := obj.Stat()
		if err != nil {
			_ = obj.Close()
			return nil, storage.ObjectInfo{}, mapErr(key, err)
		}
		return obj, objectInfo(key, oi), nil
	}

	// Any other range costs two: Resolve needs the size, and a suffix range is
	// precisely the case where the caller does not know it.
	info, err := s.Stat(ctx, key)
	if err != nil {
		return nil, storage.ObjectInfo{}, err
	}
	off, n, err := rng.Resolve(info.Size)
	if err != nil {
		return nil, storage.ObjectInfo{}, err
	}
	if n == 0 {
		// HTTP cannot express an empty range -- "bytes=5-4" is not a thing, and
		// S3 would answer 416 -- but the port says a zero-length read is legal.
		return io.NopCloser(bytes.NewReader(nil)), info, nil
	}

	var opts minio.GetObjectOptions
	if err = opts.SetRange(off, off+n-1); err != nil {
		return nil, storage.ObjectInfo{}, fmt.Errorf("%w: %w", storage.ErrInvalidRange, err)
	}
	obj, err := s.client.GetObject(ctx, s.bucket, key, opts)
	if err != nil {
		return nil, storage.ObjectInfo{}, mapErr(key, err)
	}
	return obj, info, nil
}

// Stat implements storage.Storage.
func (s *Store) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	if err := storage.ValidateKey(key); err != nil {
		return storage.ObjectInfo{}, err
	}
	if err := ctx.Err(); err != nil {
		return storage.ObjectInfo{}, err
	}

	oi, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return storage.ObjectInfo{}, mapErr(key, err)
	}
	return objectInfo(key, oi), nil
}

// Delete implements storage.Storage.
func (s *Store) Delete(ctx context.Context, key string) error {
	if err := storage.ValidateKey(key); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// S3 answers 204 whether or not the key was there, which is the idempotence
	// the port promises; the check below is for stores that are less faithful.
	if err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil && !isNotFound(err) {
		return fmt.Errorf("delete %q: %w", key, err)
	}
	return nil
}

// FreeSpace implements storage.Storage, and answers that there is no number.
//
// A bucket has no size this can see. S3 itself imposes none, and where one
// exists -- a quota on a MinIO-style server, a billing limit -- it is not
// something the API reports, so any figure here would be invented. Unlimited is
// the report: as far as anything on this side can tell, it fits.
func (s *Store) FreeSpace(_ context.Context) (int64, error) {
	return storage.Unlimited, nil
}

// List implements storage.Storage.
func (s *Store) List(ctx context.Context, prefix string) iter.Seq2[storage.ObjectInfo, error] {
	return func(yield func(storage.ObjectInfo, error) bool) {
		// ListObjects feeds the channel from a goroutine that only stops when
		// the listing is drained or the context is cancelled. A consumer that
		// breaks out of the loop does neither, so this cancel is what keeps an
		// abandoned listing from leaking it.
		listCtx, cancel := context.WithCancel(ctx)
		defer cancel()

		if err := listCtx.Err(); err != nil {
			yield(storage.ObjectInfo{}, err)
			return
		}

		// The prefix is applied server-side; filtering here would page through
		// the whole bucket to answer a question about one album.
		opts := minio.ListObjectsOptions{Prefix: prefix, Recursive: true}
		for oi := range s.client.ListObjects(listCtx, s.bucket, opts) {
			if oi.Err != nil {
				yield(storage.ObjectInfo{}, fmt.Errorf("list %q: %w", prefix, oi.Err))
				return
			}
			if !yield(objectInfo(oi.Key, oi), nil) {
				return
			}
		}
	}
}

func objectInfo(key string, oi minio.ObjectInfo) storage.ObjectInfo {
	return storage.ObjectInfo{Key: key, Size: oi.Size, ModTime: oi.LastModified}
}

// mapErr turns "no such object" into the port's sentinel and leaves every other
// failure alone, chain included.
func mapErr(key string, err error) error {
	if isNotFound(err) {
		return fmt.Errorf("%w: %q", storage.ErrNotFound, key)
	}
	return err
}

// isNotFound covers both spellings: NoSuchKey from a GET, NotFound from a HEAD,
// which is all StatObject can report since a HEAD has no body to carry a code.
//
// NoSuchBucket is deliberately absent. A bucket that has disappeared is an
// operational failure, and reporting it as a missing object would let it read
// as an empty library.
//
// errors.As rather than minio.ToErrorResponse: that helper is a bare type
// assertion and returns nothing for an error that has been wrapped even once.
func isNotFound(err error) bool {
	var resp minio.ErrorResponse
	if !errors.As(err, &resp) {
		return false
	}
	switch resp.Code {
	// NoSuchUpload is an upload id that has been completed, aborted or never
	// existed, which the port reports the same way as a missing object: the
	// caller asked about something that is not there.
	case "NoSuchKey", "NotFound", "NoSuchUpload":
		return true
	default:
		return false
	}
}

// minPartSize is S3's floor for every part but the last, and it is the whole
// reason this backend spools. A part under it is accepted when it is uploaded
// and rejected when the upload is completed, so a client sending small chunks
// would upload happily for an hour and fail at the end.
const minPartSize = 5 << 20

// StartUpload implements storage.Storage.
func (s *Store) StartUpload(ctx context.Context, key string) (string, error) {
	if err := storage.ValidateKey(key); err != nil {
		return "", err
	}
	id, err := s.core.NewMultipartUpload(ctx, s.bucket, key, minio.PutObjectOptions{})
	if err != nil {
		return "", fmt.Errorf("s3: start the upload of %q: %w", key, mapErr(key, err))
	}
	return id, nil
}

// AppendUpload implements storage.Storage.
//
// The tail that is not yet a whole part goes to SpoolDir and is counted as
// accepted, which is what lets the offset this returns be one a client can
// trust: the alternative, holding it in memory, would report progress that a
// restart silently took back.
func (s *Store) AppendUpload(ctx context.Context, key, id string, offset int64, r io.Reader) (int64, error) {
	if err := storage.ValidateKey(key); err != nil {
		return 0, err
	}

	parts, uploaded, err := s.parts(ctx, key, id)
	if err != nil {
		return 0, err
	}
	tail, err := s.tail(id)
	if err != nil {
		return 0, fmt.Errorf("s3: the upload of %q: %w", key, err)
	}
	at := uploaded + tail
	if at != offset {
		return at, fmt.Errorf("%w: the upload of %q is at %d and not %d", storage.ErrUploadOffset, key, at, offset)
	}

	// Appended to the spool first and promoted in whole parts, so a failure
	// anywhere leaves the offset exactly where the spool says it is.
	f, err := os.OpenFile(s.spoolPath(id), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return at, fmt.Errorf("s3: spool the upload of %q: %w", key, err)
	}
	n, err := io.Copy(f, r)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if err != nil {
		if got, serr := s.UploadOffset(ctx, key, id); serr == nil {
			return got, fmt.Errorf("s3: spool the upload of %q: %w", key, err)
		}
		return at, fmt.Errorf("s3: spool the upload of %q: %w", key, err)
	}

	if err := s.promote(ctx, key, id, len(parts)); err != nil {
		// The bytes are in the spool and counted either way, so the offset is
		// true; the error still goes back, because a promotion that keeps
		// failing is an upload that will never complete.
		return at + n, fmt.Errorf("s3: promote the upload of %q: %w", key, err)
	}
	return at + n, nil
}

// promote turns the spool into a part once there is enough of it to be one.
//
// The whole spool goes in a single part rather than being cut into minimum-size
// pieces: a part may be up to five gigabytes, and fewer, larger parts is both
// fewer requests and fewer things to track.
func (s *Store) promote(ctx context.Context, key, id string, done int) error {
	size, err := s.tail(id)
	if err != nil || size < minPartSize {
		return err
	}

	f, err := os.Open(s.spoolPath(id))
	if err != nil {
		return err
	}
	_, err = s.core.PutObjectPart(ctx, s.bucket, key, id, done+1, f, size,
		minio.PutObjectPartOptions{DisableContentSha256: true})
	// Best effort, and not folded into the error above: the client closes the
	// reader it was handed, so this is usually "file already closed" and never
	// news. What matters is whether the part landed.
	_ = f.Close()
	if err != nil {
		return fmt.Errorf("s3: upload part %d of %q: %w", done+1, key, err)
	}
	return os.Remove(s.spoolPath(id))
}

// UploadOffset implements storage.Storage.
func (s *Store) UploadOffset(ctx context.Context, key, id string) (int64, error) {
	if err := storage.ValidateKey(key); err != nil {
		return 0, err
	}
	_, uploaded, err := s.parts(ctx, key, id)
	if err != nil {
		return 0, err
	}
	tail, err := s.tail(id)
	if err != nil {
		return 0, fmt.Errorf("s3: the upload of %q: %w", key, err)
	}
	return uploaded + tail, nil
}

// CompleteUpload implements storage.Storage.
func (s *Store) CompleteUpload(ctx context.Context, key, id string) (storage.ObjectInfo, error) {
	if err := storage.ValidateKey(key); err != nil {
		return storage.ObjectInfo{}, err
	}

	parts, _, err := s.parts(ctx, key, id)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	size, err := s.tail(id)
	if err != nil {
		return storage.ObjectInfo{}, fmt.Errorf("s3: the upload of %q: %w", key, err)
	}
	// An upload of nothing is a file a phone will meet -- a zero-byte note, a
	// placeholder -- and S3 cannot express it as a multipart: completing with
	// no parts is an error. So it is written as an ordinary empty object and
	// the multipart is thrown away.
	if len(parts) == 0 && size == 0 {
		if aerr := s.AbortUpload(ctx, key, id); aerr != nil {
			return storage.ObjectInfo{}, aerr
		}
		return s.Put(ctx, key, bytes.NewReader(nil), 0)
	}
	// Whatever is left in the spool is the last part, and the last part is the
	// one S3 lets be short.
	if size > 0 {
		f, oerr := os.Open(s.spoolPath(id))
		if oerr != nil {
			return storage.ObjectInfo{}, fmt.Errorf("s3: the upload of %q: %w", key, oerr)
		}
		part, perr := s.core.PutObjectPart(ctx, s.bucket, key, id, len(parts)+1, f, size,
			minio.PutObjectPartOptions{DisableContentSha256: true})
		_ = f.Close()
		if perr != nil {
			return storage.ObjectInfo{}, fmt.Errorf("s3: upload the last part of %q: %w", key, perr)
		}
		parts = append(parts, minio.CompletePart{PartNumber: part.PartNumber, ETag: part.ETag})
	}

	if _, err := s.core.CompleteMultipartUpload(ctx, s.bucket, key, id, parts, minio.PutObjectOptions{}); err != nil {
		return storage.ObjectInfo{}, fmt.Errorf("s3: complete the upload of %q: %w", key, mapErr(key, err))
	}
	_ = os.Remove(s.spoolPath(id))
	return s.Stat(ctx, key)
}

// AbortUpload implements storage.Storage.
func (s *Store) AbortUpload(ctx context.Context, key, id string) error {
	if err := storage.ValidateKey(key); err != nil {
		return err
	}
	_ = os.Remove(s.spoolPath(id))
	if err := s.core.AbortMultipartUpload(ctx, s.bucket, key, id); err != nil && !isNotFound(err) {
		return fmt.Errorf("s3: abort the upload of %q: %w", key, err)
	}
	return nil
}

// parts lists what the upload has accepted, as the completion list wants it and
// as a total. Paginated, because an upload of a large video is a lot of parts.
func (s *Store) parts(ctx context.Context, key, id string) ([]minio.CompletePart, int64, error) {
	var out []minio.CompletePart
	var total int64
	marker := 0
	for {
		res, err := s.core.ListObjectParts(ctx, s.bucket, key, id, marker, 1000)
		if err != nil {
			return nil, 0, fmt.Errorf("s3: list the parts of %q: %w", key, mapErr(key, err))
		}
		for _, p := range res.ObjectParts {
			out = append(out, minio.CompletePart{PartNumber: p.PartNumber, ETag: p.ETag})
			total += p.Size
		}
		if !res.IsTruncated {
			return out, total, nil
		}
		marker = res.NextPartNumberMarker
	}
}

// tail is how much is in the spool, and zero when there is none.
func (s *Store) tail(id string) (int64, error) {
	info, err := os.Stat(s.spoolPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func (s *Store) spoolPath(id string) string {
	// The id comes from S3 and can carry anything, so it is not a file name
	// until it has been made one.
	return filepath.Join(s.spool, url.PathEscape(id))
}
