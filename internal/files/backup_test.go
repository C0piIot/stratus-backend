package files_test

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stratus-private-cloud/stratus-backend/internal/db/dbtest"
	"github.com/stratus-private-cloud/stratus-backend/internal/files"
	"github.com/stratus-private-cloud/stratus-backend/internal/files/filestest"
	"github.com/stratus-private-cloud/stratus-backend/internal/storage"
)

const backupOwner = "edu"

func TestABackupIsTheDatabaseFiledInTheTree(t *testing.T) {
	t.Parallel()
	_, _, s := filestest.Stack(t)
	if !s.CanBackUp() {
		t.Fatal("SQLite cannot be backed up")
	}
	at := time.Date(2026, 10, 22, 3, 0, 0, 0, time.UTC)
	// A folder nobody made: it is a setting, and is made when it is needed.
	done, err := s.BackUp(t.Context(), backupOwner, "admin/backups", t.TempDir(), at)
	if err != nil {
		t.Fatal(err)
	}
	if done.Key != "backups/20261022T030000Z.db" || done.Path != "admin/backups/20261022T030000Z.db" ||
		!done.At.Equal(at) || done.Size == 0 {
		t.Errorf("BackUp = %+v", done)
	}

	body, row, err := s.Open(t.Context(), backupOwner, done.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = body.Close() }()
	head := make([]byte, 16)
	if _, err := io.ReadFull(body, head); err != nil || !bytes.Equal(head, []byte("SQLite format 3\x00")) {
		t.Errorf("the backup is not a SQLite database: %q, %v", head, err)
	}
	if row.ETag == "" || row.MIMEType != "application/vnd.sqlite3" {
		t.Errorf("the backup's row = %+v", row)
	}
}

func TestOnlyTheNewestBackupsAreKept(t *testing.T) {
	t.Parallel()
	blobs, _, s := filestest.Stack(t)
	start := time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC)
	first, err := s.BackUp(t.Context(), backupOwner, "backups", t.TempDir(), start)
	if err != nil {
		t.Fatal(err)
	}
	// Somebody else's file in the folder is neither counted nor dropped.
	if _, werr := s.Write(t.Context(), backupOwner, "backups/notes.txt", strings.NewReader("mine"), 4, ""); werr != nil {
		t.Fatal(werr)
	}
	for day := 1; day < files.BackupsKept+2; day++ {
		if _, berr := s.BackUp(t.Context(), backupOwner, "backups", t.TempDir(), start.AddDate(0, 0, day)); berr != nil {
			t.Fatal(berr)
		}
	}
	all, err := s.Backups(t.Context(), backupOwner, "backups")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != files.BackupsKept {
		t.Fatalf("%d backups kept, want %d", len(all), files.BackupsKept)
	}
	if newest := start.AddDate(0, 0, files.BackupsKept+1); !all[0].At.Equal(newest) {
		t.Errorf("newest = %v, want %v", all[0].At, newest)
	}
	if oldest := start.AddDate(0, 0, 2); !all[len(all)-1].At.Equal(oldest) {
		t.Errorf("oldest = %v, want %v", all[len(all)-1].At, oldest)
	}
	// Destroyed, not trashed: the rotation is what somebody asked for.
	if _, err := blobs.Stat(t.Context(), first.Key); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("a rotated backup is still in the store: %v", err)
	}
	if totals, _ := s.TrashTotals(t.Context(), backupOwner); totals.Files != 0 {
		t.Errorf("the rotation filled the trash: %+v", totals)
	}
	if _, err := s.Stat(t.Context(), backupOwner, "backups/notes.txt"); err != nil {
		t.Errorf("a file that is not a backup was dropped: %v", err)
	}
}

// A database put back from a backup has no row for the ones taken after it,
// so the sweep keeps them by where they are until they are filed again.
func TestTheSweepLeavesBackupsAlone(t *testing.T) {
	t.Parallel()
	blobs, _, s := filestest.Stack(t)
	const key = "backups/20261001T030000Z.db"
	if _, err := blobs.Put(t.Context(), key, strings.NewReader("SQLite"), 6); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Collect(t.Context(), 0); err != nil {
		t.Fatalf("Collect over a store holding only a backup: %v", err)
	}
	if _, err := blobs.Stat(t.Context(), key); err != nil {
		t.Errorf("the sweep took the backup: %v", err)
	}
}

func TestBackupsNoRowNamesAreFiled(t *testing.T) {
	t.Parallel()
	blobs, _, s := filestest.Stack(t)
	const loose = "backups/20261001T030000Z.db"
	if _, err := blobs.Put(t.Context(), loose, strings.NewReader("SQLite"), 6); err != nil {
		t.Fatal(err)
	}
	// One somebody deleted is their decision, and stays in the trash.
	deleted, err := s.BackUp(t.Context(), backupOwner, "backups", t.TempDir(), time.Date(2026, 10, 2, 3, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if rerr := s.Remove(t.Context(), backupOwner, deleted.Path); rerr != nil {
		t.Fatal(rerr)
	}
	kept, err := s.BackUp(t.Context(), backupOwner, "backups", t.TempDir(), time.Date(2026, 10, 3, 3, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}

	n, err := s.AdoptBackups(t.Context(), backupOwner, "backups")
	if err != nil || n != 1 {
		t.Fatalf("AdoptBackups = %d, %v, want 1", n, err)
	}
	all, err := s.Backups(t.Context(), backupOwner, "backups")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].Key != kept.Key || all[1].Key != loose {
		t.Errorf("after filing, the backups are %+v", all)
	}
	if n, err := s.AdoptBackups(t.Context(), backupOwner, "backups"); err != nil || n != 0 {
		t.Errorf("a second look filed %d, %v", n, err)
	}
}

// hidden is a database whose driver cannot write the backup format.
type hidden struct{ files.Database }

func TestADatabaseWithNoBackupSaysSo(t *testing.T) {
	t.Parallel()
	blobs, meta := filestest.Backends(t)
	s := files.New(blobs, hidden{meta})
	if s.CanBackUp() {
		t.Error("a database with no backup claims one")
	}
	if _, err := s.BackUp(t.Context(), backupOwner, "backups", t.TempDir(), time.Now()); !errors.Is(err, files.ErrNoBackup) {
		t.Errorf("BackUp = %v, want ErrNoBackup", err)
	}
}

func TestABackupThatCannotBeWrittenLandsNothing(t *testing.T) {
	t.Parallel()
	_, _, s := filestest.Stack(t)
	if _, err := s.BackUp(t.Context(), backupOwner, "backups", t.TempDir()+"/not-there", time.Now()); err == nil {
		t.Fatal("a backup into a directory that is not there succeeded")
	}
	if all, err := s.Backups(t.Context(), backupOwner, "backups"); err != nil || len(all) != 0 {
		t.Errorf("Backups = %v, %v, want none", all, err)
	}
}

func TestABackupCannotBeFiledUnderAFile(t *testing.T) {
	t.Parallel()
	blobs, _, s := filestest.Stack(t)
	if _, err := s.Write(t.Context(), backupOwner, "backups", strings.NewReader("x"), 1, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BackUp(t.Context(), backupOwner, "backups", t.TempDir(), time.Now()); err == nil {
		t.Fatal("a backup was filed under a file")
	}
	for info, err := range blobs.List(t.Context(), files.BackupPrefix) {
		t.Errorf("a backup that could not be filed was left in the store: %v, %v", info.Key, err)
	}
}

func TestWhatTheIndexWillNotSayStopsTheBackups(t *testing.T) {
	t.Parallel()
	for _, method := range []string{"ListFiles", "BlobKeys"} {
		blobs, meta := filestest.Backends(t)
		if _, err := blobs.Put(t.Context(), "backups/20261001T030000Z.db", strings.NewReader("SQLite"), 6); err != nil {
			t.Fatal(err)
		}
		s := files.New(blobs, dbtest.FailOn(t, meta, method))
		_, listErr := s.Backups(t.Context(), backupOwner, "backups")
		_, adoptErr := s.AdoptBackups(t.Context(), backupOwner, "backups")
		if listErr == nil && adoptErr == nil {
			t.Errorf("with %s failing, the backups were read as if nothing had", method)
		}
	}
}

// What is under the prefix and was not written there as a backup is not one.
func TestOnlyBackupsAreFiled(t *testing.T) {
	t.Parallel()
	blobs, _, s := filestest.Stack(t)
	if _, err := blobs.Put(t.Context(), "backups/notes.txt", strings.NewReader("mine"), 4); err != nil {
		t.Fatal(err)
	}
	if n, err := s.AdoptBackups(t.Context(), backupOwner, "backups"); err != nil || n != 0 {
		t.Errorf("AdoptBackups = %d, %v, want nothing filed", n, err)
	}
}
