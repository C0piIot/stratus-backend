package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"
)

// versionTable is the only piece of SQL shared by the drivers. It is portable
// on purpose: every driver needs the same bookkeeping, and having two copies of
// it would be two chances to disagree about what "applied" means.
const versionTable = `CREATE TABLE IF NOT EXISTS schema_migrations (
	version    INTEGER   NOT NULL PRIMARY KEY,
	applied_at TIMESTAMP NOT NULL
)`

// Migration is one file from a driver's migrations directory.
type Migration struct {
	Version int64
	Name    string
	SQL     string
}

// Migrate applies every migration in dir that the database has not seen yet,
// each one in its own transaction together with the row recording it. A
// migration that fails leaves the database at the last version that worked.
//
// It is called at startup, which makes it the write probe as well: a database
// user that cannot create a table fails here rather than on the first upload.
func Migrate(ctx context.Context, sqlDB *sql.DB, dir fs.FS) error {
	migrations, err := loadMigrations(dir)
	if err != nil {
		return err
	}
	if _, err := sqlDB.ExecContext(ctx, versionTable); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	var current int64
	// No placeholders anywhere in this file: their syntax is the one thing
	// SQLite and Postgres cannot agree on, and every value here is ours.
	if err := sqlDB.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}

	// Refusing to run against a newer schema is not pedantry: rolling the image
	// back is the first thing a self-hoster does when something breaks, and a
	// binary that quietly runs against a schema from the future corrupts data
	// instead of printing an error.
	if known := latest(migrations); current > known {
		return fmt.Errorf("database is at schema version %d but this build only knows %d: it was written by a newer Stratus", current, known)
	}

	for _, m := range migrations {
		if m.Version <= current {
			continue
		}
		if err := apply(ctx, sqlDB, m); err != nil {
			return err
		}
	}
	return nil
}

func apply(ctx context.Context, sqlDB *sql.DB, m Migration) error {
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migration %d: %w", m.Version, err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed

	for _, stmt := range statements(m.SQL) {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migration %d (%s): %w", m.Version, m.Name, err)
		}
	}
	record := fmt.Sprintf(`INSERT INTO schema_migrations (version, applied_at) VALUES (%d, CURRENT_TIMESTAMP)`, m.Version)
	if _, err := tx.ExecContext(ctx, record); err != nil {
		return fmt.Errorf("record migration %d: %w", m.Version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migration %d: %w", m.Version, err)
	}
	return nil
}

// statements splits a migration into single statements, because pgx speaks the
// extended protocol and will not accept several in one Exec.
//
// The split is on semicolons, so a migration may not contain one inside a
// string literal or a trigger body. That is a real limit and a cheap one: it is
// checked by the fact that every migration has to run on both drivers.
func statements(sql string) []string {
	var out []string
	for stmt := range strings.SplitSeq(sql, ";") {
		if trimmed := strings.TrimSpace(stmt); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// loadMigrations reads NNNN_name.sql files and returns them in version order.
func loadMigrations(dir fs.FS) ([]Migration, error) {
	entries, err := fs.Glob(dir, "migrations/*.sql")
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, errors.New("no migrations found: the driver's embed is empty")
	}

	migrations := make([]Migration, 0, len(entries))
	seen := map[int64]string{}
	for _, name := range entries {
		base := path.Base(name)
		number, rest, ok := strings.Cut(base, "_")
		if !ok {
			return nil, fmt.Errorf("migration %q is not named NNNN_name.sql", base)
		}
		version, err := strconv.ParseInt(number, 10, 64)
		if err != nil || version <= 0 {
			return nil, fmt.Errorf("migration %q does not start with a version number", base)
		}
		if other, dup := seen[version]; dup {
			return nil, fmt.Errorf("migrations %q and %q share version %d", other, base, version)
		}
		seen[version] = base

		body, err := fs.ReadFile(dir, name)
		if err != nil {
			return nil, err
		}
		migrations = append(migrations, Migration{
			Version: version,
			Name:    strings.TrimSuffix(rest, ".sql"),
			SQL:     string(body),
		})
	}

	slices.SortFunc(migrations, func(a, b Migration) int { return int(a.Version - b.Version) })
	return migrations, nil
}

func latest(migrations []Migration) int64 {
	if len(migrations) == 0 {
		return 0
	}
	return migrations[len(migrations)-1].Version
}
