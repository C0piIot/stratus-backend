package db

import "context"

// Backuper is the half of a driver that can copy its whole database into one
// file, in the backup format -- which is a SQLite database with this schema,
// whatever the engine (#398). Optional, like MigrationLocker: only SQLite has
// it for now, where it is VACUUM INTO, and the other two are #425.
type Backuper interface {
	// BackupTo writes a consistent copy of the database to path, which must
	// not exist yet.
	BackupTo(ctx context.Context, path string) error
}
