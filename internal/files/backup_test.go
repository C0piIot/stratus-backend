package files_test

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/files/filestest"
	"github.com/C0piIot/stratus-backend/internal/storage"
)

func TestABackupIsTheDatabaseInTheStore(t *testing.T) {
	t.Parallel()
	blobs, _, s := filestest.Stack(t)
	if !s.CanBackUp() {
		t.Fatal("SQLite cannot be backed up")
	}
	at := time.Date(2026, 10, 22, 3, 0, 0, 0, time.UTC)
	done, err := s.BackUp(t.Context(), t.TempDir(), at)
	if err != nil {
		t.Fatal(err)
	}
	if done.Key != "backups/20261022T030000Z.db" || !done.At.Equal(at) || done.Size == 0 {
		t.Errorf("BackUp = %+v", done)
	}
	body, _, err := blobs.Get(t.Context(), done.Key, storage.All())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = body.Close() }()
	head := make([]byte, 16)
	if _, err := io.ReadFull(body, head); err != nil || !bytes.Equal(head, []byte("SQLite format 3\x00")) {
		t.Errorf("the backup is not a SQLite database: %q, %v", head, err)
	}
}

func TestOnlyTheNewestBackupsAreKept(t *testing.T) {
	t.Parallel()
	blobs, _, s := filestest.Stack(t)
	// Somebody else's object under the prefix is neither counted nor dropped.
	if _, err := blobs.Put(t.Context(), "backups/notes.txt", strings.NewReader("mine"), 4); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC)
	for day := range files.BackupsKept + 2 {
		if _, err := s.BackUp(t.Context(), t.TempDir(), start.AddDate(0, 0, day)); err != nil {
			t.Fatal(err)
		}
	}
	all, err := s.Backups(t.Context())
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
	if _, err := blobs.Stat(t.Context(), "backups/notes.txt"); err != nil {
		t.Errorf("an object that is not a backup was dropped: %v", err)
	}
}

// No row claims a backup, so the sweep has to know them by where they are --
// and a store holding nothing else is a fresh install, not an index pointed
// somewhere new.
func TestTheSweepLeavesBackupsAlone(t *testing.T) {
	t.Parallel()
	blobs, _, s := filestest.Stack(t)
	done, err := s.BackUp(t.Context(), t.TempDir(), time.Now().Add(-48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Collect(t.Context(), 0); err != nil {
		t.Fatalf("Collect over a store holding only a backup: %v", err)
	}
	if _, err := blobs.Stat(t.Context(), done.Key); err != nil {
		t.Errorf("the sweep took the backup: %v", err)
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
	if _, err := s.BackUp(t.Context(), t.TempDir(), time.Now()); !errors.Is(err, files.ErrNoBackup) {
		t.Errorf("BackUp = %v, want ErrNoBackup", err)
	}
}

func TestABackupThatCannotBeWrittenLandsNothing(t *testing.T) {
	t.Parallel()
	_, _, s := filestest.Stack(t)
	if _, err := s.BackUp(t.Context(), t.TempDir()+"/not-there", time.Now()); err == nil {
		t.Fatal("a backup into a directory that is not there succeeded")
	}
	if all, err := s.Backups(t.Context()); err != nil || len(all) != 0 {
		t.Errorf("Backups = %v, %v, want none", all, err)
	}
}
