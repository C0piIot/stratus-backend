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
	// UseTLS talks https. Off is for MinIO on a private network.
	UseTLS bool
}

// Store is a storage.Storage backed by an S3-compatible bucket.
type Store struct {
	client *minio.Client
	bucket string
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
	return &Store{client: client, bucket: cfg.Bucket}, nil
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
	case "NoSuchKey", "NotFound":
		return true
	default:
		return false
	}
}
