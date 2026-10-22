package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/config"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/files/filestest"
)

func backupApp(every time.Duration) *App {
	cfg := config.Config{}
	cfg.BackupInterval = every
	return &App{cfg: cfg}
}

func TestABackupIsTakenWhenDueAndNotBefore(t *testing.T) {
	t.Parallel()
	_, _, service := filestest.Stack(t)
	deps := Deps{Files: service, Scratch: t.TempDir()}
	a := backupApp(24 * time.Hour)

	if !a.backUpIfDue(t.Context(), deps) {
		t.Fatal("the pass stopped")
	}
	first := a.lastBackup()
	if first.At.IsZero() || first.Size == 0 || first.Err != "" {
		t.Fatalf("a fresh install was not backed up: %+v", first)
	}
	a.backUpIfDue(t.Context(), deps)
	if all, _ := service.Backups(t.Context()); len(all) != 1 {
		t.Errorf("%d backups after a second look inside the interval, want 1", len(all))
	}

	// A restart remembers the newest backup from the store, not from memory.
	again := backupApp(24 * time.Hour)
	again.backUpIfDue(t.Context(), deps)
	if got := again.lastBackup(); !got.At.Equal(first.At) {
		t.Errorf("after a restart the newest backup is %v, want %v", got.At, first.At)
	}
}

func TestBackupsOffTakeNone(t *testing.T) {
	t.Parallel()
	_, _, service := filestest.Stack(t)
	a := backupApp(0)
	a.backUpIfDue(t.Context(), Deps{Files: service, Scratch: t.TempDir()})
	if all, _ := service.Backups(t.Context()); len(all) != 0 {
		t.Errorf("%d backups with the interval at zero", len(all))
	}
}

func TestAFailedBackupKeepsTheLastOne(t *testing.T) {
	t.Parallel()
	_, _, service := filestest.Stack(t)
	a := backupApp(time.Nanosecond)
	a.backUpIfDue(t.Context(), Deps{Files: service, Scratch: t.TempDir()})
	good := a.lastBackup()

	a.backUpIfDue(t.Context(), Deps{Files: service, Scratch: t.TempDir() + "/not-there"})
	got := a.lastBackup()
	if !got.At.Equal(good.At) || !strings.Contains(got.Err, "back up") {
		t.Errorf("after a failure: %+v, want the last good one with the error", got)
	}
}

type noBackups struct{ files.Database }

func TestADatabaseWithNoBackupIsSaid(t *testing.T) {
	t.Parallel()
	blobs, meta := filestest.Backends(t)
	a := backupApp(24 * time.Hour)
	a.backUpIfDue(t.Context(), Deps{Files: files.New(blobs, noBackups{meta}), Scratch: t.TempDir()})
	if !a.lastBackup().Unsupported {
		t.Error("a database with no backup is not said to have none")
	}
}

func TestACancelledBackupStopsTheLoop(t *testing.T) {
	t.Parallel()
	_, _, service := filestest.Stack(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if backupApp(24*time.Hour).backUpIfDue(ctx, Deps{Files: service, Scratch: t.TempDir()}) {
		t.Error("a cancelled pass did not stop the loop")
	}
}
