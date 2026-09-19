// Package files owns the invariants that must look identical from every
// protocol, because a file here is a database row *plus* a blob.
//
// Nothing else may write that pair. If dav and web each wired storage and db
// themselves they would drift on how an ETag is computed and, worse, on what
// happens when one of the two writes succeeds and the other fails.
//
// The two writes cannot be made atomic across two independent seams, so
// ordering is the mitigation, in both directions:
//
//   - Writing: blob first, row second. A failure leaves a blob nothing points
//     at, which is collectable garbage (#17).
//   - Deleting: row first, blob second. Same reason read backwards -- a row
//     pointing at a blob that is gone would be a 500 on every read.
package files

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/sniff"
	"github.com/C0piIot/stratus-backend/internal/storage"
)

// Database is what this package needs from the metadata seam: the file
// repository, plus a unit of work. Declared here rather than taken as a whole
// db.Store so that the dependency says what it uses.
type Database interface {
	db.Files
	db.Uploads
	Tx(ctx context.Context, fn func(db.Repo) error) error
}

// Service is the file layer.
type Service struct {
	blobs storage.Storage
	meta  Database
	// watch is told about a file that has just been written, and is nil when
	// nobody is listening. See WithWatcher.
	watch func(db.File)
}

// New wires the two seams together.
func New(blobs storage.Storage, meta Database, opts ...Option) *Service {
	s := &Service{blobs: blobs, meta: meta}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// An Option configures a Service at construction. Variadic so that the callers
// that want none -- which is most of them, and every test -- say nothing.
type Option func(*Service)

// WithWatcher registers a function called once a write has landed: the blob is
// stored and the row is committed. It exists so the media indexer can start on
// a file the moment it arrives instead of finding it on its next pass, and it
// is wired by the composition root because this package cannot import
// internal/media -- that package imports this one.
//
// The watcher takes no context on purpose. What it starts outlives the request
// that caused it, and handing it the request's context would cancel the work as
// soon as the response was written. It must not block for the same reason: the
// caller is still holding a client connection.
//
// Nothing here depends on it arriving. A watcher that is never called, or a
// process that dies before it runs, costs latency and not correctness -- what
// is pending is a query over the rows, and the row is already committed.
func WithWatcher(fn func(db.File)) Option {
	return func(s *Service) { s.watch = fn }
}

// written tells the watcher, if there is one.
func (s *Service) written(f db.File) {
	if s.watch != nil {
		s.watch(f)
	}
}

// Stat returns the row for path.
func (s *Service) Stat(ctx context.Context, owner, path string) (db.File, error) {
	return s.meta.FileByPath(ctx, owner, path)
}

// List returns the direct children of dir, files and directories alike.
func (s *Service) List(ctx context.Context, owner, dir string) ([]db.File, error) {
	return s.meta.ListFiles(ctx, owner, dir)
}

// ListPage returns at most limit children of dir, resuming after the cursor,
// and reports whether there is another page behind it.
//
// The "is there more" is why this is here rather than in each caller: the way
// to answer it is to ask for one row more than is wanted and throw it away, and
// a driver that had to do that would be three drivers doing it. The cursor for
// the next page is db.After of the last row returned.
func (s *Service) ListPage(ctx context.Context, owner, dir string, after db.Cursor, limit int) ([]db.File, bool, error) {
	if err := db.ValidateLimit(limit); err != nil {
		return nil, false, err
	}
	rows, err := s.meta.ListFilesPage(ctx, owner, dir, after, limit+1)
	if err != nil {
		return nil, false, err
	}
	if len(rows) > limit {
		return rows[:limit], true, nil
	}
	return rows, false, nil
}

// Open returns the bytes of a file.
//
// The reader seeks, which is not decoration: http.ServeContent switches on
// io.Seeker to implement RFC 7233, so returning one is what gives every
// protocol surface range requests, conditional requests and video seeking for
// free instead of hand-rolled range parsing.
func (s *Service) Open(ctx context.Context, owner, path string) (io.ReadSeekCloser, db.File, error) {
	f, err := s.meta.FileByPath(ctx, owner, path)
	if err != nil {
		return nil, db.File{}, err
	}
	if f.IsDir {
		return nil, db.File{}, fmt.Errorf("%w: %q is a directory", db.ErrConflict, path)
	}
	body, err := s.OpenFile(ctx, f)
	return body, f, err
}

// OpenFile is Open for a row the caller already has, which saves the lookup.
// The indexer walks rows, so it would otherwise pay a query per file to reach
// bytes it can already address.
func (s *Service) OpenFile(ctx context.Context, f db.File) (io.ReadSeekCloser, error) {
	if f.IsDir {
		return nil, fmt.Errorf("%w: %q is a directory", db.ErrConflict, f.Path)
	}
	return &blobReader{ctx: ctx, blobs: s.blobs, key: f.BlobKey, size: f.Size}, nil
}

// Write stores body at path, replacing whatever was there.
//
// size may be negative when the client did not say. The ETag is a digest of
// what was actually stored, computed on the way past, so it is a strong
// validator rather than a guess from a size and a timestamp.
func (s *Service) Write(ctx context.Context, owner, path string, body io.Reader, size int64, mimeType string) (db.File, error) {
	f, err := s.storeBlob(ctx, owner, path, body, size, mimeType)
	if err != nil {
		return db.File{}, err
	}

	err = s.meta.Tx(ctx, func(r db.Repo) error {
		if perr := s.requireParent(ctx, r, owner, path); perr != nil {
			return perr
		}
		stored, perr := r.PutFile(ctx, f)
		if perr != nil {
			return perr
		}
		f = stored
		return nil
	})
	if err != nil {
		// The blob is already written and now points at nothing. Removing it is
		// best effort: if this fails too, #17 collects it.
		_ = s.blobs.Delete(ctx, f.BlobKey)
		return db.File{}, err
	}
	s.written(f)
	return f, nil
}

// storeBlob writes the bytes and returns the row that would describe them,
// without committing it.
//
// Split out of Write for Copy, which writes a tree of blobs and then commits
// every row at once: the ordering this package relies on -- blob first, row
// second, a blob with no row being collectable garbage -- is what lets a copy be
// all or nothing, and it only works if the rows wait.
//
// It is one function and not two copies of one, which is the whole reason this
// package exists: a second opinion here about what a blob key looks like or how
// an ETag is computed is exactly the drift internal/files was created to stop.
func (s *Service) storeBlob(ctx context.Context, owner, path string, body io.Reader, size int64, mimeType string) (db.File, error) {
	if err := db.ValidatePath(path); err != nil {
		return db.File{}, err
	}

	// The first bytes are read before anything else happens to them, because
	// two of the things about to be decided are decided better by the file than
	// by its name: what it is filed under, and what it is served as (#146).
	// They are handed to the store in front of the rest, so nothing is read
	// twice and no request is made twice.
	head := make([]byte, sniff.HeadSize)
	read, err := io.ReadFull(body, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return db.File{}, fmt.Errorf("read %q: %w", path, err)
	}
	head = head[:read]
	sniffedType, sniffedKind := sniff.Sniff(head)

	// A fresh key every time, never derived from the path: it is what lets an
	// import adopt somebody else's bucket (#24), and it means an overwrite that
	// fails halfway has not destroyed the previous content. The cost is an
	// orphaned blob per overwrite, which is #17's job.
	key := newBlobKey(path, sniffedKind)
	digest := sha256.New()

	whole := io.MultiReader(bytes.NewReader(head), body)
	info, err := s.blobs.Put(ctx, key, io.TeeReader(whole, digest), size)
	if err != nil {
		return db.File{}, fmt.Errorf("store %q: %w", path, err)
	}

	return db.File{
		OwnerID:  owner,
		Path:     path,
		BlobKey:  key,
		Size:     info.Size,
		MTime:    info.ModTime,
		ETag:     etag(digest),
		MIMEType: contentType(mimeType, sniffedType),
	}, nil
}

// contentType keeps what the client declared unless it declared nothing worth
// keeping.
//
// A client that named a type gets to keep it, even when the bytes disagree:
// being told is better information than being guessed at, and a server that
// overrode it would be arguing with the only party that saw the file whole.
// What this replaces is silence -- and application/octet-stream, which is what
// every WebDAV client sends for everything and what dav.mimeType falls back to
// for an extension nothing knows.
func contentType(declared, sniffed string) string {
	if sniffed == "" {
		return declared
	}
	if declared == "" || declared == "application/octet-stream" {
		return sniffed
	}
	return declared
}

// Mkdir records an empty directory.
func (s *Service) Mkdir(ctx context.Context, owner, path string) (db.File, error) {
	if err := db.ValidatePath(path); err != nil {
		return db.File{}, err
	}

	var dir db.File
	err := s.meta.Tx(ctx, func(r db.Repo) error {
		if err := s.requireParent(ctx, r, owner, path); err != nil {
			return err
		}
		created, err := r.CreateDir(ctx, owner, path)
		dir = created
		return err
	})
	return dir, err
}

// Move renames a file, or a directory with everything under it.
//
// Only rows move: a blob has no idea what it is called, so renaming a folder of
// ten thousand photos is one statement and no bytes. The transaction is what
// makes it safe -- a rewrite that stopped halfway would leave the rest of the
// tree pointing at a parent that no longer exists.
func (s *Service) Move(ctx context.Context, owner, from, to string) error {
	return s.meta.Tx(ctx, func(r db.Repo) error {
		if err := s.requireParent(ctx, r, owner, to); err != nil {
			return err
		}
		return r.MoveFile(ctx, owner, from, to)
	})
}

// Remove deletes path and, if it is a directory, everything under it.
//
// Every row goes in one transaction, so a failure halfway leaves the tree as it
// was. The blobs are deleted afterwards, outside it: they are the half that is
// safe to lose.
func (s *Service) Remove(ctx context.Context, owner, path string) error {
	var orphaned []string

	err := s.meta.Tx(ctx, func(r db.Repo) error {
		keys, err := removeTree(ctx, r, owner, path)
		orphaned = keys
		return err
	})
	if err != nil {
		return err
	}

	for _, key := range orphaned {
		if err := s.blobs.Delete(ctx, key); err != nil {
			// The rows are already gone, so the caller's delete did succeed.
			// What is left is garbage with an owner: #17.
			return fmt.Errorf("delete blob %q: %w", key, err)
		}
	}
	return nil
}

// removeTree deletes depth first, so a directory is only removed once it is
// empty, which is the one order the database will accept.
func removeTree(ctx context.Context, r db.Repo, owner, path string) ([]string, error) {
	f, err := r.FileByPath(ctx, owner, path)
	if err != nil {
		return nil, err
	}

	var keys []string
	if f.IsDir {
		children, err := r.ListFiles(ctx, owner, path)
		if err != nil {
			return nil, err
		}
		for _, child := range children {
			childKeys, err := removeTree(ctx, r, owner, child.Path)
			if err != nil {
				return nil, err
			}
			keys = append(keys, childKeys...)
		}
	} else {
		keys = append(keys, f.BlobKey)
	}

	if err := r.DeleteFile(ctx, owner, path); err != nil {
		return nil, err
	}
	return keys, nil
}

// requireParent is the half of the tree invariant that stays in Go: a row whose
// parent is missing is unreachable, and WebDAV answers 409 for it.
//
// A foreign key from (owner_id, parent_path) was weighed against this and
// refused: it sees existence and nothing of is_dir, so the second check below
// would stay regardless. CLAUDE.md carries the rest of the price.
func (s *Service) requireParent(ctx context.Context, r db.Repo, owner, path string) error {
	parent := db.ParentOf(path)
	if parent == "" {
		return nil
	}

	f, err := r.FileByPath(ctx, owner, parent)
	if errors.Is(err, db.ErrNotFound) {
		return fmt.Errorf("%w: %q has no parent directory", db.ErrNotFound, path)
	}
	if err != nil {
		return err
	}
	if !f.IsDir {
		return fmt.Errorf("%w: %q is not a directory", db.ErrConflict, parent)
	}
	return nil
}

// etag is unquoted: the quotes are HTTP framing, and the adapters that speak
// HTTP add them. Storing them would mean every comparison had to strip them.
func etag(h hash.Hash) string {
	return hex.EncodeToString(h.Sum(nil))
}

// blobReader turns the storage port's ranged reads into an io.ReadSeekCloser,
// which is the shape http.ServeContent needs. Nothing is fetched until the
// first Read, so a Seek to the end -- which is how ServeContent asks for the
// size -- costs no request at all.
type blobReader struct {
	// ctx is held rather than passed because io.Reader has nowhere to put it.
	// It is the request's context, so an abandoned download stops fetching.
	ctx   context.Context
	blobs storage.Storage
	key   string
	size  int64

	offset int64
	body   io.ReadCloser
}

func (b *blobReader) Read(p []byte) (int, error) {
	if b.offset >= b.size {
		return 0, io.EOF
	}
	if b.body == nil {
		body, _, err := b.blobs.Get(b.ctx, b.key, storage.From(b.offset))
		if err != nil {
			// A row whose blob is missing is corruption, not a 404: reporting
			// it as "not found" would hide it behind something ordinary.
			return 0, fmt.Errorf("blob %q: %w", b.key, err)
		}
		b.body = body
	}

	n, err := b.body.Read(p)
	b.offset += int64(n)
	return n, err
}

func (b *blobReader) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = b.offset + offset
	case io.SeekEnd:
		abs = b.size + offset
	default:
		return 0, fmt.Errorf("blob %q: invalid whence %d", b.key, whence)
	}
	if abs < 0 {
		return 0, fmt.Errorf("blob %q: negative position %d", b.key, abs)
	}

	if abs != b.offset {
		// The open range no longer starts where the next read does, so it is
		// dropped and reopened lazily.
		if err := b.closeBody(); err != nil {
			return 0, err
		}
		b.offset = abs
	}
	return abs, nil
}

func (b *blobReader) Close() error { return b.closeBody() }

func (b *blobReader) closeBody() error {
	if b.body == nil {
		return nil
	}
	body := b.body
	b.body = nil
	return body.Close()
}
