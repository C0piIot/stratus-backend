package files

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/stratus-private-cloud/stratus-backend/internal/db"
)

// BackupPrefix is where the database's backups are kept in the blob store
// (#398). Each is also a file in the tree, so it can be seen and fetched like
// any other, but its key is not one newBlobKey would make: a backup is for the
// day the database is gone, and with no rows left a random key in a dated
// folder is one nobody could find again.
const BackupPrefix = "backups/"

// BackupsKept is how many backups are kept; the oldest goes when a new one
// lands.
const BackupsKept = 7

// backupLayout is a backup's name: the moment it was taken, in UTC, so that
// the names sort in the order they were taken and say when to whoever lists
// the folder.
const backupLayout = "20060102T150405Z"

// backupType is what a backup is served as.
const backupType = "application/vnd.sqlite3"

// ErrNoBackup is a database whose driver cannot write the backup format yet.
var ErrNoBackup = errors.New("this database cannot be backed up yet")

// Backup is one backup in the tree.
type Backup struct {
	Path string
	Key  string
	At   time.Time
	Size int64
}

// CanBackUp is whether this database can be backed up at all.
func (s *Service) CanBackUp() bool {
	_, ok := s.meta.(db.Backuper)
	return ok
}

// Backups are the backups in dir, newest first. What is there and was not
// written here -- a file somebody put in the folder -- is not counted.
func (s *Service) Backups(ctx context.Context, owner, dir string) ([]Backup, error) {
	rows, err := s.meta.ListFiles(ctx, owner, dir)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return nil, fmt.Errorf("list the backups: %w", err)
	}
	var all []Backup
	for _, f := range rows {
		at, ok := backupTime(f.BlobKey)
		if f.IsDir || !ok {
			continue
		}
		all = append(all, Backup{Path: f.Path, Key: f.BlobKey, At: at, Size: f.Size})
	}
	slices.SortFunc(all, func(a, b Backup) int { return b.At.Compare(a.At) })
	return all, nil
}

// BackUp copies the database into the blob store, through a file in scratch,
// files it in dir, and then destroys the backups there past BackupsKept.
func (s *Service) BackUp(ctx context.Context, owner, dir, scratch string, now time.Time) (Backup, error) {
	b, ok := s.meta.(db.Backuper)
	if !ok {
		return Backup{}, ErrNoBackup
	}
	at := now.UTC().Truncate(time.Second)
	name := at.Format(backupLayout) + ".db"
	path := dir + "/" + name
	if err := db.ValidatePath(path); err != nil {
		return Backup{}, err
	}

	local := filepath.Join(scratch, "backup-"+rand.Text()+".db")
	defer func() { _ = os.Remove(local) }()
	if err := b.BackupTo(ctx, local); err != nil {
		return Backup{}, err
	}
	f, err := os.Open(local) //nolint:gosec // a name this function made up
	if err != nil {
		return Backup{}, fmt.Errorf("open the backup: %w", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return Backup{}, fmt.Errorf("stat the backup: %w", err)
	}

	key := BackupPrefix + name
	digest := sha256.New()
	stored, err := s.blobs.Put(ctx, key, io.TeeReader(f, digest), info.Size())
	if err != nil {
		return Backup{}, fmt.Errorf("store the backup: %w", err)
	}
	row := db.File{
		OwnerID: owner, Path: path, BlobKey: key,
		Size: stored.Size, MTime: stored.ModTime, ETag: etag(digest), MIMEType: backupType,
	}
	err = s.meta.Tx(ctx, func(r db.Repo) error {
		// The folder is made if it is not there, as a restore makes one: it
		// is a setting, and nobody should have to create it first.
		if perr := restoreParents(ctx, r, owner, path); perr != nil {
			return perr
		}
		var perr error
		row, perr = r.PutFile(ctx, row)
		return perr
	})
	if err != nil {
		_ = s.blobs.Delete(ctx, key)
		return Backup{}, fmt.Errorf("file the backup: %w", err)
	}
	s.written(row)
	done := Backup{Path: path, Key: key, At: at, Size: stored.Size}

	// The new one is in before any old one goes, so a failure here costs
	// room and never a backup. They are destroyed rather than trashed: this is
	// the rotation somebody configured, not an accident to recover from.
	all, err := s.Backups(ctx, owner, dir)
	if err != nil {
		return done, err
	}
	for _, old := range all[min(len(all), BackupsKept):] {
		if err := s.meta.DeleteFile(ctx, owner, old.Path); err != nil {
			return done, fmt.Errorf("drop the backup %s: %w", old.Path, err)
		}
		if err := s.dropBlob(ctx, old.Key); err != nil {
			return done, fmt.Errorf("drop the backup %s: %w", old.Path, err)
		}
	}
	return done, nil
}

// AdoptBackups files in dir the backups in the store that no row names: the
// newest one after a restore, which was taken after the copy that was put
// back, and every one written before backups were files. One that is in the
// trash is somebody's decision and stays there.
func (s *Service) AdoptBackups(ctx context.Context, owner, dir string) (int, error) {
	claimed := make(map[string]struct{})
	for key, err := range s.meta.BlobKeys(ctx) {
		if err != nil {
			return 0, fmt.Errorf("read the referenced keys: %w", err)
		}
		claimed[key] = struct{}{}
	}
	for key, err := range s.meta.TrashKeys(ctx) {
		if err != nil {
			return 0, fmt.Errorf("read the trashed keys: %w", err)
		}
		claimed[key] = struct{}{}
	}

	var rows []db.File
	for info, err := range s.blobs.List(ctx, BackupPrefix) {
		if err != nil {
			return 0, fmt.Errorf("list the backups: %w", err)
		}
		if _, ok := claimed[info.Key]; ok {
			continue
		}
		if _, ok := backupTime(info.Key); !ok {
			continue
		}
		rows = append(rows, db.File{
			Path: dir + "/" + strings.TrimPrefix(info.Key, BackupPrefix), BlobKey: info.Key,
			Size: info.Size, MTime: info.ModTime, MIMEType: backupType,
		})
	}
	if len(rows) == 0 {
		return 0, nil
	}
	slices.SortFunc(rows, func(a, b db.File) int { return cmp.Compare(a.Path, b.Path) })
	adopted, _, err := s.Adopt(ctx, owner, rows)
	return adopted, err
}

// backupTime reads the moment out of a backup's key, and false for a key
// this package did not write.
func backupTime(key string) (time.Time, bool) {
	name, ok := strings.CutPrefix(key, BackupPrefix)
	if !ok {
		return time.Time{}, false
	}
	stamp, ok := strings.CutSuffix(name, ".db")
	if !ok {
		return time.Time{}, false
	}
	at, err := time.Parse(backupLayout, stamp)
	return at, err == nil
}
