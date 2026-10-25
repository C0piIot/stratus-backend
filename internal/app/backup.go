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
	v := a.values(deps)
	// With no user there is no tree to file a backup in, and nothing written
	// into the database worth one.
	if v.BackupInterval <= 0 || a.cfg.Username == "" {
		return true
	}
	owner, dir := a.cfg.Username, v.BackupDir

	last := a.backedUp.Load()
	if last == nil || last.Dir != dir {
		// The first look in a folder reads the tree, so the clock survives a
		// restart and a folder that moved is backed up into at once.
		if last == nil {
			if n, err := service.AdoptBackups(ctx, owner, dir); err != nil {
				if errors.Is(err, context.Canceled) {
					return false
				}
				slog.Error("filing the backups no row names", "err", err)
			} else if n > 0 {
				slog.Info("filed the backups no row named", "dir", dir, "backups", n)
			}
		}
		all, err := service.Backups(ctx, owner, dir)
		switch {
		case errors.Is(err, context.Canceled):
			return false
		case err != nil:
			slog.Error("listing the database backups", "err", err)
			a.backedUp.Store(&web.Backup{Dir: dir, Err: err.Error()})
			return true
		}
		last = &web.Backup{Dir: dir}
		if len(all) > 0 {
			last = &web.Backup{Dir: dir, At: all[0].At, Size: all[0].Size}
		}
		a.backedUp.Store(last)
	}
	if !last.At.IsZero() && time.Since(last.At) < v.BackupInterval {
		return true
	}

	done, err := service.BackUp(ctx, owner, dir, deps.Scratch, time.Now())
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
		a.backedUp.Store(&web.Backup{Dir: dir, At: done.At, Size: done.Size, Err: err.Error()})
	default:
		slog.Info("backed up the database", "path", done.Path, "bytes", done.Size, "kept", files.BackupsKept)
		a.backedUp.Store(&web.Backup{Dir: dir, At: done.At, Size: done.Size})
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
