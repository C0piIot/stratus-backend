package db

import (
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
}
