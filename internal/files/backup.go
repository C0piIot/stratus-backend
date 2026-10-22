package files

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// BackupPrefix is where the database's backups are kept in the blob store
// (#398), beside the files and outside anything a row names: the sweep leaves
// it alone, since no row will ever claim one.
const BackupPrefix = "backups/"

// BackupsKept is how many backups are kept; the oldest goes when a new one
// lands.
const BackupsKept = 7

// backupLayout is a backup's name: the moment it was taken, in UTC, so that
// the names sort in the order they were taken and say when to whoever lists
// the bucket.
const backupLayout = "20060102T150405Z"

// ErrNoBackup is a database whose driver cannot write the backup format yet.
var ErrNoBackup = errors.New("this database cannot be backed up yet")

// Backup is one backup in the store.
type Backup struct {
	Key  string
	At   time.Time
	Size int64
}

// CanBackUp is whether this database can be backed up at all.
func (s *Service) CanBackUp() bool {
	_, ok := s.meta.(db.Backuper)
	return ok
}

// Backups are the backups in the store, newest first.
func (s *Service) Backups(ctx context.Context) ([]Backup, error) {
	var all []Backup
	for info, err := range s.blobs.List(ctx, BackupPrefix) {
		if err != nil {
			return nil, fmt.Errorf("list the backups: %w", err)
		}
		at, ok := backupTime(info.Key)
		if !ok {
			// Not one of ours: somebody put it there, and it is not this
			// package's to count or to delete.
			continue
		}
		all = append(all, Backup{Key: info.Key, At: at, Size: info.Size})
	}
	slices.SortFunc(all, func(a, b Backup) int { return cmp.Compare(b.Key, a.Key) })
	return all, nil
}

// BackUp copies the database into the blob store, through a file in scratch,
// and then drops the backups past BackupsKept.
func (s *Service) BackUp(ctx context.Context, scratch string, now time.Time) (Backup, error) {
	b, ok := s.meta.(db.Backuper)
	if !ok {
		return Backup{}, ErrNoBackup
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

	at := now.UTC().Truncate(time.Second)
	key := BackupPrefix + at.Format(backupLayout) + ".db"
	if _, err = s.blobs.Put(ctx, key, f, info.Size()); err != nil {
		return Backup{}, fmt.Errorf("store the backup: %w", err)
	}
	done := Backup{Key: key, At: at, Size: info.Size()}

	// The new one is in before any old one goes, so a failure here costs
	// room and never a backup.
	all, err := s.Backups(ctx)
	if err != nil {
		return done, err
	}
	for _, old := range all[min(len(all), BackupsKept):] {
		if err := s.blobs.Delete(ctx, old.Key); err != nil {
			return done, fmt.Errorf("drop the backup %s: %w", old.Key, err)
		}
	}
	return done, nil
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
