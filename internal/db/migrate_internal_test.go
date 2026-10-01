package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"testing/fstest"
)

func TestLoadMigrations(t *testing.T) {
	t.Parallel()
	dir := fstest.MapFS{
		"migrations/0002_second.sql": {Data: []byte("SELECT 2")},
		"migrations/0001_first.sql":  {Data: []byte("SELECT 1")},
		"migrations/0010_tenth.sql":  {Data: []byte("SELECT 10")},
	}

	got, err := loadMigrations(dir)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	// Numeric order, not lexical: 10 comes after 2 even though "0010" < "0002"
	// is false only because of the zero padding somebody may forget.
	want := []int64{1, 2, 10}
	if len(got) != len(want) {
		t.Fatalf("loaded %d migrations, want %d", len(got), len(want))
	}
	for i, m := range got {
		if m.Version != want[i] {
			t.Errorf("migration %d has version %d, want %d", i, m.Version, want[i])
		}
	}
	if got[0].Name != "first" {
		t.Errorf("Name = %q, want %q", got[0].Name, "first")
	}
}

func TestLoadMigrationsRejects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		dir  fstest.MapFS
	}{
		{name: "an empty embed", dir: fstest.MapFS{}},
		{
			name: "a file with no version",
			dir:  fstest.MapFS{"migrations/files.sql": {Data: []byte("SELECT 1")}},
		},
		{
			name: "a version that is not a number",
			dir:  fstest.MapFS{"migrations/first_files.sql": {Data: []byte("SELECT 1")}},
		},
		{
			name: "version zero, which would always look applied",
			dir:  fstest.MapFS{"migrations/0000_files.sql": {Data: []byte("SELECT 1")}},
		},
		{
			// Two people adding a migration in parallel branches is how this
			// happens, and applying only one of them silently is the worst
			// possible outcome.
			name: "two migrations sharing a version",
			dir: fstest.MapFS{
				"migrations/0001_files.sql":  {Data: []byte("SELECT 1")},
				"migrations/0001_events.sql": {Data: []byte("SELECT 2")},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := loadMigrations(tt.dir); err == nil {
				t.Error("loadMigrations = nil, want an error")
			}
		})
	}
}

func TestStatements(t *testing.T) {
	t.Parallel()
	const migration = `
CREATE TABLE a (id INTEGER);

CREATE INDEX a_id ON a (id);
`
	got := statements(migration)
	if len(got) != 2 {
		t.Fatalf("split into %d statements, want 2: %q", len(got), got)
	}
	if !strings.HasPrefix(got[0], "CREATE TABLE") || !strings.HasPrefix(got[1], "CREATE INDEX") {
		t.Errorf("statements = %q", got)
	}

	// Trailing semicolons and blank lines must not produce empty statements:
	// pgx rejects those.
	for _, stmt := range statements("SELECT 1;\n\n;\n") {
		if strings.TrimSpace(stmt) == "" {
			t.Error("an empty statement survived the split")
		}
	}

	// A comment is prose, and prose has semicolons in it. Before they were
	// stripped, the sentence below ended a statement halfway through and the
	// rest of the file arrived at the server as a syntax error.
	const commented = `
-- Why this table is shaped like this; the reason has a semicolon in it.
CREATE TABLE a (
    id INTEGER -- and so does the column; here it is again.
);
`
	only := statements(commented)
	if len(only) != 1 {
		t.Fatalf("split into %d statements, want 1: %q", len(only), only)
	}
	if strings.Contains(only[0], "--") {
		t.Errorf("a comment survived into the statement: %q", only[0])
	}
}

// The migration lock, from the side a real engine cannot show: a run that fails
// has to give it back, and it has to be taken before the first statement rather
// than before the first migration. Both are assertions about order, so they are
// made against a driver that fails everything instead of against PostgreSQL.

func init() { sql.Register("migratefake", failingDriver{}) }

var errFake = errors.New("migratefake: everything fails here")

type failingDriver struct{}

func (failingDriver) Open(string) (driver.Conn, error) { return failingConn{}, nil }

type failingConn struct{}

func (failingConn) Prepare(string) (driver.Stmt, error) { return nil, errFake }
func (failingConn) Close() error                        { return nil }
func (failingConn) Begin() (driver.Tx, error)           { return nil, errFake }

// countingLocker is a MigrationLocker that records what was asked of it.
type countingLocker struct {
	err      error
	taken    int
	released int
}

func (c *countingLocker) LockMigrations(context.Context) (func(), error) {
	if c.err != nil {
		return nil, c.err
	}
	c.taken++
	return func() { c.released++ }, nil
}

func fakeDB(t *testing.T) *sql.DB {
	t.Helper()
	sqlDB, err := sql.Open("migratefake", "")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() {
		if err := sqlDB.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return sqlDB
}

// oneMigration is enough for loadMigrations to have something to do; none of it
// is ever executed, because the first statement fails.
var oneMigration = fstest.MapFS{
	"migrations/0001_schema.sql": &fstest.MapFile{Data: []byte("CREATE TABLE files (id INTEGER);")},
}

// TestMigrateTakesTheLockBeforeTheFirstStatement: the very first thing Migrate
// does to the database is create the version table, which is DDL, and two
// instances racing on that statement is the bug one line earlier than the one
// anybody thinks of. The driver here fails that statement, so a lock taken any
// later would never have been taken at all.
func TestMigrateTakesTheLockBeforeTheFirstStatement(t *testing.T) {
	t.Parallel()
	lock := &countingLocker{}

	if err := Migrate(t.Context(), fakeDB(t), oneMigration, lock); !errors.Is(err, errFake) {
		t.Fatalf("Migrate = %v, want the injected failure", err)
	}
	if lock.taken != 1 {
		t.Errorf("the lock was taken %d times, want 1 and before the first statement", lock.taken)
	}
	if lock.released != 1 {
		t.Errorf("a run that failed gave the lock back %d times, want 1", lock.released)
	}
}

// TestMigrateWithoutALock is the SQLite shape: a driver that has no lock and
// needs none passes nil, and nothing here dereferences it.
func TestMigrateWithoutALock(t *testing.T) {
	t.Parallel()
	if err := Migrate(t.Context(), fakeDB(t), oneMigration, nil); !errors.Is(err, errFake) {
		t.Fatalf("Migrate = %v, want the injected failure", err)
	}
}

// TestMigrateStopsWhenTheLockDoes: a lock that cannot be taken is not a
// migration to attempt anyway, and the database is never touched.
func TestMigrateStopsWhenTheLockDoes(t *testing.T) {
	t.Parallel()
	lock := &countingLocker{err: errors.New("another instance is migrating")}

	err := Migrate(t.Context(), fakeDB(t), oneMigration, lock)
	switch {
	case err == nil:
		t.Fatal("Migrate = nil, want the lock's own error")
	case errors.Is(err, errFake):
		t.Error("Migrate reached the database without the lock")
	case !strings.Contains(err.Error(), "another instance is migrating"):
		t.Errorf("Migrate = %v, want the lock's own reason", err)
	}
	if lock.released != 0 {
		t.Error("a lock that was never taken was released")
	}
}
