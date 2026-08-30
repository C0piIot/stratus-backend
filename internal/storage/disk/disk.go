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
	if err := root.Mkdir(tmpDir, dirMode); err != nil && !errors.Is(err, fs.ErrExist) {
		_ = root.Close()
		return nil, fmt.Errorf("create %s in %s: %w", tmpDir, dir, err)
	}
	return &Store{root: root}, nil
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

	written, err := io.Copy(f, r)
	if err == nil && size >= 0 && written != size {
		err = fmt.Errorf("%w: wrote %d bytes, expected %d", storage.ErrSizeMismatch, written, size)
	}
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
				if p == tmpDir {
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
