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
	"strconv"
	"strings"
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
}

// Store is a storage.Storage backed by an S3-compatible bucket.
type Store struct {
	client *minio.Client
	core   minio.Core
	bucket string
	// partSize is defaultPartSize, and a field so a test can see parts form
	// without sending sixteen megabytes for each.
	partSize int
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

	store := &Store{client: client, core: minio.Core{Client: client}, bucket: cfg.Bucket, partSize: defaultPartSize}
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

	// And the tails, which an upload aborted above leaves and which are
	// billed like anything else. Last written before the cutoff means the
	// upload has not been touched since, whatever became of it.
	for oi := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: tailPrefix, Recursive: true}) {
		if oi.Err != nil {
			return fmt.Errorf("s3: list the tails of incomplete uploads: %w", oi.Err)
		}
		if oi.LastModified.After(cutoff) {
			continue
		}
		if err := s.client.RemoveObject(ctx, s.bucket, oi.Key, minio.RemoveObjectOptions{}); err != nil {
			return fmt.Errorf("s3: remove the tail %q: %w", oi.Key, err)
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
			// A tail is bytes of an upload in flight, which no listing shows.
			if strings.HasPrefix(oi.Key, tailPrefix) {
				continue
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

// minPartSize is S3's floor for every part but the last. A part under it is
// accepted when it is uploaded and rejected when the upload is completed, so a
// client sending small chunks would upload happily for an hour and fail at the
// end.
const minPartSize = 5 << 20

// defaultPartSize is how much of an append is held in memory before it goes
// to the bucket as a part: the whole of this backend's footprint per upload in
// flight, and, times S3's ten thousand parts, the largest object a client
// sending everything in one request can upload -- 156 GiB.
const defaultPartSize = 16 << 20

// tailPrefix is where the part-to-be of an upload waits between appends. It
// cannot collide with a key, since storage.ValidateKey rejects a first segment
// starting with a dot, and it is named for this project because a bucket may
// be shared and the stale sweep deletes what is under it.
const tailPrefix = ".stratus-uploads/"

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
// Nothing touches the local disk, so an upload can be resumed through any
// process pointed at the bucket. What arrives is held in memory and goes out as
// a part each time there is a part's worth. What is left when the request
// ends -- because it ended, or because the connection went -- is either big
// enough to be a part, or is written to the bucket as the tail: an object named
// for the part it will become, and counted in the offset only while that part
// does not exist. That numbering is what makes promoting a tail safe: the
// moment the part lands, the tail it came from stops counting, with no second
// write that could fail in between.
//
// Everything this reads is kept unless the bucket refuses it, which is what
// lets internal/files carry its running hash across a dropped connection.
func (s *Store) AppendUpload(ctx context.Context, key, id string, offset int64, r io.Reader) (int64, error) {
	if err := storage.ValidateKey(key); err != nil {
		return 0, err
	}

	parts, uploaded, err := s.parts(ctx, key, id)
	if err != nil {
		return 0, err
	}
	next := len(parts) + 1

	buf := make([]byte, s.partSize)
	kept, err := s.readTail(ctx, id, next, buf)
	if err != nil {
		return 0, fmt.Errorf("s3: the upload of %q: %w", key, err)
	}
	if at := uploaded + int64(kept); at != offset {
		return at, fmt.Errorf("%w: the upload of %q is at %d and not %d", storage.ErrUploadOffset, key, at, offset)
	}

	// held is what is in memory, kept how much of it the bucket already has as
	// the tail, and the offset at any moment is uploaded + kept.
	held := kept
	for {
		n, rerr := fill(r, buf[held:])
		held += n
		if rerr != nil {
			return s.flush(ctx, key, id, next, uploaded, buf[:held], kept, rerr)
		}
		if err := s.putPart(ctx, key, id, next, buf[:held]); err != nil {
			return uploaded + int64(kept), err
		}
		s.dropTail(ctx, id, next)
		uploaded += int64(held)
		held, kept = 0, 0
		next++
	}
}

// flush keeps what an append was holding when its body ended, and reports the
// reader's error unless that was the end of the body.
//
// Not under the request's context: the likeliest reason to be here is a client
// that went away, which cancels it, and the bytes it did send are exactly what
// the next attempt should not have to send again.
func (s *Store) flush(ctx context.Context, key, id string, next int, uploaded int64, held []byte, kept int, rerr error) (int64, error) {
	if errors.Is(rerr, io.EOF) {
		rerr = nil
	}
	if len(held) == kept {
		return uploaded + int64(kept), rerr
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), flushTimeout)
	defer cancel()

	var err error
	if len(held) >= minPartSize {
		if err = s.putPart(ctx, key, id, next, held); err == nil {
			s.dropTail(ctx, id, next)
		}
	} else if _, err = s.client.PutObject(ctx, s.bucket, tailKey(id, next), bytes.NewReader(held), int64(len(held)),
		minio.PutObjectOptions{}); err != nil {
		err = fmt.Errorf("s3: keep the tail of %q: %w", key, err)
	}
	if err != nil {
		return uploaded + int64(kept), errors.Join(rerr, err)
	}
	return uploaded + int64(len(held)), rerr
}

// flushTimeout bounds the write that outlives its request: a tail is under
// five megabytes, and a bucket that cannot take that in a minute is not going
// to.
const flushTimeout = time.Minute

// fill reads until buf is full, returning io.EOF only for a body that ended
// cleanly and any other error as the reader gave it.
func fill(r io.Reader, buf []byte) (int, error) {
	var n int
	for n < len(buf) {
		m, err := r.Read(buf[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func (s *Store) putPart(ctx context.Context, key, id string, number int, b []byte) error {
	_, err := s.core.PutObjectPart(ctx, s.bucket, key, id, number, bytes.NewReader(b), int64(len(b)),
		minio.PutObjectPartOptions{DisableContentSha256: true})
	if err != nil {
		return fmt.Errorf("s3: upload part %d of %q: %w", number, key, mapErr(key, err))
	}
	return nil
}

// UploadOffset implements storage.Storage.
func (s *Store) UploadOffset(ctx context.Context, key, id string) (int64, error) {
	if err := storage.ValidateKey(key); err != nil {
		return 0, err
	}
	parts, uploaded, err := s.parts(ctx, key, id)
	if err != nil {
		return 0, err
	}
	tail, err := s.tailSize(ctx, id, len(parts)+1)
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
	next := len(parts) + 1
	size, err := s.tailSize(ctx, id, next)
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
	// The tail is the last part, and the last part is the one S3 lets be short.
	if size > 0 {
		buf := make([]byte, size)
		if _, err := s.readTail(ctx, id, next, buf); err != nil {
			return storage.ObjectInfo{}, fmt.Errorf("s3: the upload of %q: %w", key, err)
		}
		part, perr := s.core.PutObjectPart(ctx, s.bucket, key, id, next, bytes.NewReader(buf), size,
			minio.PutObjectPartOptions{DisableContentSha256: true})
		if perr != nil {
			return storage.ObjectInfo{}, fmt.Errorf("s3: upload the last part of %q: %w", key, perr)
		}
		parts = append(parts, minio.CompletePart{PartNumber: part.PartNumber, ETag: part.ETag})
	}

	if _, err := s.core.CompleteMultipartUpload(ctx, s.bucket, key, id, parts, minio.PutObjectOptions{}); err != nil {
		return storage.ObjectInfo{}, fmt.Errorf("s3: complete the upload of %q: %w", key, mapErr(key, err))
	}
	s.dropTails(ctx, id)
	return s.Stat(ctx, key)
}

// AbortUpload implements storage.Storage.
func (s *Store) AbortUpload(ctx context.Context, key, id string) error {
	if err := storage.ValidateKey(key); err != nil {
		return err
	}
	if err := s.core.AbortMultipartUpload(ctx, s.bucket, key, id); err != nil && !isNotFound(err) {
		return fmt.Errorf("s3: abort the upload of %q: %w", key, err)
	}
	s.dropTails(ctx, id)
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

// tailSize is how much the tail for part number next holds, and zero when
// there is none.
func (s *Store) tailSize(ctx context.Context, id string, next int) (int64, error) {
	oi, err := s.client.StatObject(ctx, s.bucket, tailKey(id, next), minio.StatObjectOptions{})
	if isNotFound(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return oi.Size, nil
}

// readTail reads the tail for part number next into the start of buf and
// reports how much it held, zero when there is none.
func (s *Store) readTail(ctx context.Context, id string, next int, buf []byte) (int, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, tailKey(id, next), minio.GetObjectOptions{})
	if err != nil {
		return 0, err
	}
	defer func() { _ = obj.Close() }()

	n, err := fill(obj, buf)
	switch {
	case isNotFound(err):
		return 0, nil
	case errors.Is(err, io.EOF):
		return n, nil
	case err != nil:
		return 0, err
	}
	// A tail is under a part by construction; one that fills the buffer is not
	// something this backend wrote.
	return 0, fmt.Errorf("the tail of part %d is larger than a part", next)
}

// dropTail deletes a tail whose part has landed. Best effort: once the part
// exists the tail no longer counts, and the tails of an upload all go when it
// completes or is aborted.
func (s *Store) dropTail(ctx context.Context, id string, next int) {
	_ = s.client.RemoveObject(ctx, s.bucket, tailKey(id, next), minio.RemoveObjectOptions{})
}

// dropTails deletes every tail an upload left, which is the ones dropTail
// could not.
func (s *Store) dropTails(ctx context.Context, id string) {
	for oi := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: tailDir(id), Recursive: true}) {
		if oi.Err != nil {
			return
		}
		_ = s.client.RemoveObject(ctx, s.bucket, oi.Key, minio.RemoveObjectOptions{})
	}
}

func tailDir(id string) string {
	// The id comes from S3 and can carry anything, so it is not a key segment
	// until it has been made one.
	return tailPrefix + url.PathEscape(id) + "/"
}

func tailKey(id string, next int) string { return tailDir(id) + strconv.Itoa(next) }
