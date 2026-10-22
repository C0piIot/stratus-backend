package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/C0piIot/stratus-backend/internal/config"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/web"
)

// backupCheck is how often the loop asks whether a backup is due. Shorter
// than the interval, because the clock that matters is the newest backup's
// and not this process's: a server restarted every day on a daily interval
// would otherwise never back up at all.
const backupCheck = time.Hour

// backupPeriodically copies the database into the blob store whenever the
// newest copy there is older than the interval (#398), for as long as ctx
// lives. Once at the start, so a fresh install has one the moment it runs.
func (a *App) backupPeriodically(ctx context.Context, deps Deps) {
	if !a.backUpIfDue(ctx, deps) {
		return
	}
	a.every(ctx, deps, "backing up the database", func(v config.Values) time.Duration {
		return min(v.BackupInterval, backupCheck)
	}, func() bool { return a.backUpIfDue(ctx, deps) })
}

// backUpIfDue takes a backup when the newest one is older than the interval,
// and is false when the context has gone.
func (a *App) backUpIfDue(ctx context.Context, deps Deps) bool {
	service := deps.Files
	if !service.CanBackUp() {
		a.backedUp.Store(&web.Backup{Unsupported: true})
		return true
	}
	every := a.values(deps).BackupInterval
	if every <= 0 {
		return true
	}

	last := a.backedUp.Load()
	if last == nil {
		// The first look reads the store, so the clock survives a restart.
		all, err := service.Backups(ctx)
		switch {
		case errors.Is(err, context.Canceled):
			return false
		case err != nil:
			slog.Error("listing the database backups", "err", err)
			a.backedUp.Store(&web.Backup{Err: err.Error()})
			return true
		}
		last = &web.Backup{}
		if len(all) > 0 {
			last = &web.Backup{At: all[0].At, Size: all[0].Size}
		}
		a.backedUp.Store(last)
	}
	if !last.At.IsZero() && time.Since(last.At) < every {
		return true
	}

	done, err := service.BackUp(ctx, deps.Scratch, time.Now())
	switch {
	case errors.Is(err, context.Canceled):
		return false
	case err != nil && done.Key == "":
		// Nothing landed: the newest is still the one before.
		slog.Error("backing up the database", "err", err)
		failedBackup := *last
		failedBackup.Err = err.Error()
		a.backedUp.Store(&failedBackup)
	case err != nil:
		// It landed and an old one would not go, which costs room.
		slog.Error("dropping an old database backup", "err", err)
		a.backedUp.Store(&web.Backup{At: done.At, Size: done.Size, Err: err.Error()})
	default:
		slog.Info("backed up the database", "key", done.Key, "bytes", done.Size, "kept", files.BackupsKept)
		a.backedUp.Store(&web.Backup{At: done.At, Size: done.Size})
	}
	return true
}

// lastBackup is what the status page shows, and a zero Backup before the
// loop has looked.
func (a *App) lastBackup() web.Backup {
	if b := a.backedUp.Load(); b != nil {
		return *b
	}
	return web.Backup{}
}
