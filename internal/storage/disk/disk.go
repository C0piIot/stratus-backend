// Package disk implements the storage port on a local filesystem tree.
//
// Every operation goes through an *os.Root, so a key can never reach outside
// the configured directory: the kernel refuses a traversal or a symlink that
// escapes, instead of the correctness of this package resting on somebody
// remembering to call filepath.Clean.
package disk

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"os"
	"path"
	"strings"
	"syscall"

	"github.com/C0piIot/stratus-backend/internal/storage"
)

// tmpDir holds partially written objects until they are renamed into place. It
// can never collide with a key: storage.ValidateKey rejects a first segment
// starting with a dot.
const tmpDir = ".tmp"

// Modes match internal/app: the data directory is private to the user the
// process runs as.
const (
	fileMode = 0o600
	dirMode  = 0o750
)

// Store is a storage.Storage backed by a directory tree.
type Store struct {
	root *os.Root
}

var _ storage.Storage = (*Store)(nil)

// New opens dir as a storage root, creating it if it does not exist.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return nil, fmt.Errorf("create storage root %s: %w", dir, err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open storage root %s: %w", dir, err)
	}
	for _, reserved := range []string{tmpDir, uploadDir} {
		if err := root.Mkdir(reserved, dirMode); err != nil && !errors.Is(err, fs.ErrExist) {
			_ = root.Close()
			return nil, fmt.Errorf("create %s in %s: %w", reserved, dir, err)
		}
	}

	store := &Store{root: root}
	// Anything in the reserved directory belongs to a process that is no longer
	// running: a Put that finished renamed its file out of it. No age check is
	// needed, and two processes sharing one data directory is not a thing this
	// project supports.
	if err := store.sweepTemp(); err != nil {
		_ = root.Close()
		return nil, err
	}
	return store, nil
}

// sweepTemp removes what an interrupted upload left behind.
func (s *Store) sweepTemp() error {
	entries, err := fs.ReadDir(s.root.FS(), tmpDir)
	if err != nil {
		return fmt.Errorf("read %s: %w", tmpDir, err)
	}
	for _, e := range entries {
		if err := s.root.Remove(tmpDir + "/" + e.Name()); err != nil {
			return fmt.Errorf("remove %s/%s: %w", tmpDir, e.Name(), err)
		}
	}
	return nil
}

// Close releases the directory handle. It is not part of the port: only the
// composition root, which opened the Store, closes it.
func (s *Store) Close() error { return s.root.Close() }

// Put implements storage.Storage.
func (s *Store) Put(ctx context.Context, key string, r io.Reader, size int64) (storage.ObjectInfo, error) {
	if err := storage.ValidateKey(key); err != nil {
		return storage.ObjectInfo{}, err
	}
	if err := ctx.Err(); err != nil {
		return storage.ObjectInfo{}, err
	}

	tmp, err := s.writeTemp(r, size)
	if err != nil {
		return storage.ObjectInfo{}, err
	}

	if dir := path.Dir(key); dir != "." {
		if err := s.root.MkdirAll(dir, dirMode); err != nil {
			_ = s.root.Remove(tmp)
			return storage.ObjectInfo{}, fmt.Errorf("create parents of %q: %w", key, err)
		}
	}
	if err := s.root.Rename(tmp, key); err != nil {
		_ = s.root.Remove(tmp)
		return storage.ObjectInfo{}, fmt.Errorf("publish %q: %w", key, err)
	}
	return s.Stat(ctx, key)
}

// writeTemp drains r into a file under tmpDir and returns its name. The caller
// renames it into place, which is what makes a Put atomic for readers.
func (s *Store) writeTemp(r io.Reader, size int64) (string, error) {
	name := tmpDir + "/" + rand.Text()
	f, err := s.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}

	_, err = io.Copy(f, storage.ExactReader(r, size))
	if err == nil {
		// Sync before the rename, not after: a crash between the two would
		// otherwise leave the key pointing at a file whose data never reached
		// the disk, which is worse than the upload having failed outright.
		err = f.Sync()
	}
	if cerr := f.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if err != nil {
		_ = s.root.Remove(name)
		return "", err
	}
	return name, nil
}

// Get implements storage.Storage.
func (s *Store) Get(ctx context.Context, key string, rng storage.Range) (io.ReadCloser, storage.ObjectInfo, error) {
	if err := storage.ValidateKey(key); err != nil {
		return nil, storage.ObjectInfo{}, err
	}
	if err := ctx.Err(); err != nil {
		return nil, storage.ObjectInfo{}, err
	}

	f, err := s.root.Open(key)
	if err != nil {
		return nil, storage.ObjectInfo{}, mapErr(key, err)
	}
	// Stat the open file rather than the path: Put publishes by rename, so a
	// concurrent overwrite would otherwise let the size describe one object and
	// the bytes another.
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, storage.ObjectInfo{}, err
	}
	if !fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, storage.ObjectInfo{}, fmt.Errorf("%w: %q", storage.ErrNotFound, key)
	}

	info := storage.ObjectInfo{Key: key, Size: fi.Size(), ModTime: fi.ModTime()}
	off, n, err := rng.Resolve(info.Size)
	if err != nil {
		_ = f.Close()
		return nil, storage.ObjectInfo{}, err
	}
	if off > 0 {
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			_ = f.Close()
			return nil, storage.ObjectInfo{}, err
		}
	}
	return &object{Reader: io.LimitReader(f, n), file: f}, info, nil
}

// object bounds a read to the requested range while still closing the file the
// bytes came from.
type object struct {
	io.Reader
	file *os.File
}

func (o *object) Close() error { return o.file.Close() }

// Stat implements storage.Storage.
func (s *Store) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	if err := storage.ValidateKey(key); err != nil {
		return storage.ObjectInfo{}, err
	}
	if err := ctx.Err(); err != nil {
		return storage.ObjectInfo{}, err
	}

	fi, err := s.root.Stat(key)
	if err != nil {
		return storage.ObjectInfo{}, mapErr(key, err)
	}
	if !fi.Mode().IsRegular() {
		// A directory is a container of keys, not an object under one.
		return storage.ObjectInfo{}, fmt.Errorf("%w: %q", storage.ErrNotFound, key)
	}
	return storage.ObjectInfo{Key: key, Size: fi.Size(), ModTime: fi.ModTime()}, nil
}

// Delete implements storage.Storage.
func (s *Store) Delete(ctx context.Context, key string) error {
	if err := storage.ValidateKey(key); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	if err := s.root.Remove(key); err != nil {
		if isNotFound(err) {
			return nil // documented as idempotent
		}
		return fmt.Errorf("delete %q: %w", key, err)
	}
	s.pruneParents(path.Dir(key))
	return nil
}

// pruneParents removes the directories a deleted key leaves behind, so that a
// photo library reorganised over the years does not accumulate an empty tree.
// Best effort: the first non-empty directory, or any error, stops it.
func (s *Store) pruneParents(dir string) {
	for dir != "." && dir != "/" {
		if err := s.root.Remove(dir); err != nil {
			return
		}
		dir = path.Dir(dir)
	}
}

// List implements storage.Storage.
func (s *Store) List(ctx context.Context, prefix string) iter.Seq2[storage.ObjectInfo, error] {
	return func(yield func(storage.ObjectInfo, error) bool) {
		// errStop is how a consumer's break reaches back out of WalkDir; it is
		// never returned to the caller.
		errStop := errors.New("stop")

		err := fs.WalkDir(s.root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
			if d.IsDir() {
				// Neither reserved directory is a listing: one holds writes in
				// flight and the other uploads that will be resumed, and an
				// object exists when it has been renamed out of them.
				if p == tmpDir || p == uploadDir {
					return fs.SkipDir
				}
				// A directory can only hold matching keys if it is on the
				// prefix's path or under it. Skipping the rest keeps a query
				// for one album from walking the whole library.
				if p != "." && !strings.HasPrefix(p+"/", prefix) && !strings.HasPrefix(prefix, p+"/") {
					return fs.SkipDir
				}
				return nil
			}
			// Only regular files are objects, so a symlink inside the root
			// cannot smuggle a second name for the same bytes into a listing.
			if !d.Type().IsRegular() || !strings.HasPrefix(p, prefix) {
				return nil
			}

			fi, err := d.Info()
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil // raced with a delete; it is simply not there
				}
				return err
			}
			if !yield(storage.ObjectInfo{Key: p, Size: fi.Size(), ModTime: fi.ModTime()}, nil) {
				return errStop
			}
			return nil
		})
		if err != nil && !errors.Is(err, errStop) {
			yield(storage.ObjectInfo{}, fmt.Errorf("list %q: %w", prefix, err))
		}
	}
}

// mapErr turns "the path is not there" into the port's sentinel and leaves
// every other failure alone.
func mapErr(key string, err error) error {
	if isNotFound(err) {
		return fmt.Errorf("%w: %q", storage.ErrNotFound, key)
	}
	return err
}

// isNotFound also covers ENOTDIR: a key whose parent is an object rather than a
// directory does not exist either, and saying so is more useful than leaking
// the shape of the tree.
func isNotFound(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

// uploadDir holds resumable uploads, and it is a second reserved directory
// rather than a corner of tmpDir for one reason: tmpDir is emptied when the
// store opens, on the argument that anything left in it belongs to a dead
// process. An upload in flight is the opposite -- a phone will come back to it
// tomorrow -- so a restart must leave it exactly where it was.
//
// Like tmpDir it can never collide with a key, since storage.ValidateKey
// rejects a first segment starting with a dot, and List skips it, which is what
// keeps an unfinished upload out of a listing and therefore out of reach of the
// sweep in internal/files.
const uploadDir = ".uploads"

// StartUpload implements storage.Storage.
func (s *Store) StartUpload(ctx context.Context, key string) (string, error) {
	if err := storage.ValidateKey(key); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}

	id := rand.Text()
	f, err := s.root.OpenFile(uploadPath(id), os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
	if err != nil {
		return "", fmt.Errorf("start the upload of %q: %w", key, err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("start the upload of %q: %w", key, err)
	}
	return id, nil
}

// AppendUpload implements storage.Storage.
func (s *Store) AppendUpload(ctx context.Context, key, id string, offset int64, r io.Reader) (int64, error) {
	if err := storage.ValidateKey(key); err != nil {
		return 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	at, err := s.UploadOffset(ctx, key, id)
	if err != nil {
		return 0, err
	}
	if at != offset {
		return at, fmt.Errorf("%w: the upload of %q is at %d and not %d", storage.ErrUploadOffset, key, at, offset)
	}

	f, err := s.root.OpenFile(uploadPath(id), os.O_WRONLY|os.O_APPEND, fileMode)
	if err != nil {
		return at, fmt.Errorf("append to the upload of %q: %w", key, mapErr(id, err))
	}
	n, err := io.Copy(f, r)
	if err == nil {
		// Synced per append rather than at the end: what this port promises a
		// resumed upload is that the offset it reports survives the machine
		// going away, and an unsynced tail would make that a lie.
		err = f.Sync()
	}
	if cerr := f.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if err != nil {
		// n bytes may still have landed, so the offset is read back rather than
		// assumed: what the caller needs is where the store actually is.
		if got, serr := s.UploadOffset(ctx, key, id); serr == nil {
			return got, fmt.Errorf("append to the upload of %q: %w", key, err)
		}
		return at, fmt.Errorf("append to the upload of %q: %w", key, err)
	}
	return at + n, nil
}

// UploadOffset implements storage.Storage.
func (s *Store) UploadOffset(ctx context.Context, key, id string) (int64, error) {
	if err := storage.ValidateKey(key); err != nil {
		return 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	info, err := s.root.Stat(uploadPath(id))
	if err != nil {
		return 0, fmt.Errorf("the upload of %q: %w", key, mapErr(id, err))
	}
	return info.Size(), nil
}

// CompleteUpload implements storage.Storage.
func (s *Store) CompleteUpload(ctx context.Context, key, id string) (storage.ObjectInfo, error) {
	if err := storage.ValidateKey(key); err != nil {
		return storage.ObjectInfo{}, err
	}
	if err := ctx.Err(); err != nil {
		return storage.ObjectInfo{}, err
	}

	if dir := path.Dir(key); dir != "." {
		if err := s.root.MkdirAll(dir, dirMode); err != nil {
			return storage.ObjectInfo{}, fmt.Errorf("create parents of %q: %w", key, err)
		}
	}
	// The same rename Put ends with, and atomic for the same reason: a reader
	// sees the old object or the new one, never the growing file.
	if err := s.root.Rename(uploadPath(id), key); err != nil {
		return storage.ObjectInfo{}, fmt.Errorf("publish the upload of %q: %w", key, mapErr(id, err))
	}
	return s.Stat(ctx, key)
}

// AbortUpload implements storage.Storage.
func (s *Store) AbortUpload(ctx context.Context, key, id string) error {
	if err := storage.ValidateKey(key); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	if err := s.root.Remove(uploadPath(id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("abort the upload of %q: %w", key, err)
	}
	return nil
}

// uploadPath keeps every upload in one flat reserved directory. The id is
// generated here and never comes from a caller, so it needs no validation of
// its own -- and it is not the key, because two uploads of the same key at once
// are two uploads.
func uploadPath(id string) string { return uploadDir + "/" + id }
