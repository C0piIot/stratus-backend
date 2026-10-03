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

// MigrationLocker is the half of a driver that keeps two instances from
// migrating at the same time.
//
// Only the engine can hold this lock. What is being changed is the schema, so a
// row in a table cannot guard it -- the table would be DDL itself, and creating
// it is already part of the race. Postgres has advisory locks and MySQL has
// GET_LOCK; SQLite has neither and needs neither, so its driver passes nil here
// rather than this file growing a switch on a dialect it is written not to know
// about (#242).
type MigrationLocker interface {
	// LockMigrations blocks until this process holds the lock and returns the
	// release to call when the run is over -- which means every way it can end,
	// including a migration that failed half way through.
	LockMigrations(ctx context.Context) (release func(), err error)
}

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
//
// lock is the engine's own, or nil for a driver that has none and needs none.
// See MigrationLocker.
func Migrate(ctx context.Context, sqlDB *sql.DB, dir fs.FS, lock MigrationLocker) error {
	migrations, err := loadMigrations(dir)
	if err != nil {
		return err
	}

	// Taken before anything is read and not only before anything is applied:
	// the version table is created with DDL too, and two instances racing on
	// that statement is the same bug one line earlier.
	if lock != nil {
		release, lerr := lock.LockMigrations(ctx)
		if lerr != nil {
			return fmt.Errorf("lock migrations: %w", lerr)
		}
		defer release()
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
// Line comments are dropped before the split rather than passed through. They
// are where a schema says why it is shaped the way it is -- MySQL's needs a
// paragraph -- and a prose sentence contains a semicolon sooner or later, which
// the split would take for the end of a statement.
//
// The split is on semicolons, with one exception: a trigger's body is full of
// them and is one statement all the same, so the pieces of a CREATE TRIGGER are
// put back together until its END. SQLite needs that -- an FTS5 index is
// maintained by triggers (0011) -- and the alternative was maintaining it from
// the driver, which would turn the one statement that moves a subtree into one
// per row.
//
// It is not a SQL parser and must not become one. It knows two things: a
// statement that begins CREATE TRIGGER has a body, and that body ends at the
// END that balances its BEGIN. A semicolon inside a string literal still splits,
// and so would a CASE ... END inside a trigger. Both are limits worth having
// over a parser nobody can read.
func statements(sql string) []string {
	var body strings.Builder
	for line := range strings.SplitSeq(sql, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		body.WriteString(line)
		body.WriteByte('\n')
	}

	var out []string
	var current strings.Builder
	for piece := range strings.SplitSeq(body.String(), ";") {
		current.WriteString(piece)
		if unclosedTrigger(current.String()) {
			// The semicolon that ended this piece belongs to the body.
			current.WriteByte(';')
			continue
		}
		if trimmed := strings.TrimSpace(current.String()); trimmed != "" {
			out = append(out, trimmed)
		}
		current.Reset()
	}
	// Whatever is left is a statement with no semicolon after it, or a trigger
	// nobody closed. Either way it goes to the engine as it is: a refusal there
	// says which file and which statement, and one here would not.
	if trimmed := strings.TrimSpace(current.String()); trimmed != "" {
		out = append(out, trimmed)
	}
	return out
}

// unclosedTrigger reports whether stmt is a CREATE TRIGGER whose body is still
// open. Only a trigger, so the word BEGIN anywhere else -- a column called
// begin, a transaction nobody should be writing in a migration -- is read as
// what it is.
func unclosedTrigger(stmt string) bool {
	words := strings.Fields(strings.ToUpper(stmt))
	if len(words) == 0 || words[0] != "CREATE" {
		return false
	}
	// CREATE TRIGGER, and CREATE TEMP TRIGGER, which is as far as the forms go.
	if !slices.Contains(words[:min(len(words), 3)], "TRIGGER") {
		return false
	}
	var depth int
	for _, w := range words {
		switch w {
		case "BEGIN":
			depth++
		case "END":
			depth--
		}
	}
	return depth > 0
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
