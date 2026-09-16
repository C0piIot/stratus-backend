package app_test

// Every way a start can be refused.
//
// The matrix matters more than any one case: a composition root's job is to
// fail before it serves rather than after, so what is asserted throughout is an
// exit and a reason, not a server that came up half-wired.

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/app"
	"github.com/C0piIot/stratus-backend/internal/config"
	"github.com/C0piIot/stratus-backend/internal/db/sqlite"
)

func TestRunRejectsUnwritableDataDir(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root ignores mode bits, so this cannot fail as root")
	}
	dir := filepath.Join(t.TempDir(), "readonly")
	if err := os.Mkdir(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	// Port 0 would still bind; the point is that Run must refuse before it does.
	err := app.New(runConfig(t, map[string]string{"STRATUS_DATA_DIR": dir}), "test").Run(t.Context())
	if err == nil {
		t.Fatal("Run must refuse to start on an unwritable data dir")
	}
	if !strings.Contains(err.Error(), "not writable") {
		t.Errorf("error should say it is not writable, got %v", err)
	}
}

func TestEnsureDataDir(t *testing.T) {
	t.Parallel()

	t.Run("creates a missing directory", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Join(t.TempDir(), "data")
		if err := app.EnsureDataDir(dir); err != nil {
			t.Fatalf("EnsureDataDir: %v", err)
		}
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			t.Fatalf("directory not created: %v", err)
		}
	})

	t.Run("creates missing parents", func(t *testing.T) {
		t.Parallel()
		if err := app.EnsureDataDir(filepath.Join(t.TempDir(), "a", "b", "c")); err != nil {
			t.Fatalf("EnsureDataDir: %v", err)
		}
	})

	t.Run("is idempotent", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		for i := range 3 {
			if err := app.EnsureDataDir(dir); err != nil {
				t.Fatalf("call %d: %v", i, err)
			}
		}
	})

	t.Run("leaves no probe file behind", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := app.EnsureDataDir(dir); err != nil {
			t.Fatalf("EnsureDataDir: %v", err)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Errorf("directory should be empty, found %d entries", len(entries))
		}
	})

	t.Run("overwrites a stale probe file", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		stale := filepath.Join(dir, ".stratus-write-probe")
		if err := os.WriteFile(stale, []byte("left over from a crash"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := app.EnsureDataDir(dir); err != nil {
			t.Fatalf("a stale probe should not be fatal: %v", err)
		}
		if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
			t.Error("stale probe should have been removed")
		}
	})

	t.Run("path is a file", func(t *testing.T) {
		t.Parallel()
		file := filepath.Join(t.TempDir(), "notadir")
		if err := os.WriteFile(file, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := app.EnsureDataDir(file); err == nil {
			t.Error("a regular file should not be accepted as the data dir")
		}
	})

	// The regression test for the bug that shipped: a data dir the process
	// cannot write to must fail at startup, not on the first upload.
	t.Run("unwritable directory names the uid", func(t *testing.T) {
		t.Parallel()
		if os.Geteuid() == 0 {
			t.Skip("root ignores mode bits, so this cannot fail as root")
		}
		dir := filepath.Join(t.TempDir(), "readonly")
		if err := os.Mkdir(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		err := app.EnsureDataDir(dir)
		if err == nil {
			t.Fatal("an unwritable data dir must be an error")
		}
		if !strings.Contains(err.Error(), "not writable") {
			t.Errorf("error should say it is not writable, got %v", err)
		}
		if !strings.Contains(err.Error(), "uid") {
			t.Errorf("error should name the uid so the operator can fix it, got %v", err)
		}
	})
}

func TestRunRejectsHalfSetCredentials(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		vars map[string]string
		want string
	}{
		{
			name: "a hash with no username",
			vars: map[string]string{"STRATUS_PASSWORD": "an example password"},
			want: "STRATUS_USERNAME",
		},
		{
			name: "a username with no hash",
			vars: map[string]string{"STRATUS_USERNAME": "edu"},
			want: "STRATUS_PASSWORD",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := runToShutdown(t, runConfig(t, tt.vars))
			if err == nil {
				t.Fatal("Run = nil, want a refusal")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error should name %s, got %v", tt.want, err)
			}
		})
	}
}

func TestRunAcceptsWholeCredentials(t *testing.T) {
	t.Parallel()
	if err := runToShutdown(t, runConfig(t, map[string]string{
		"STRATUS_USERNAME": "edu",
		"STRATUS_PASSWORD": "an example password",
	})); err != nil {
		t.Errorf("Run = %v, want a clean start and shutdown", err)
	}
}

// TestRunProbesTheBlobStore checks the probe both ran and cleaned up after
// itself: an install that starts with a stray object in the store would be
// reporting one on every listing forever.
func TestRunProbesTheBlobStore(t *testing.T) {
	t.Parallel()
	dataDir := filepath.Join(t.TempDir(), "data")
	cfg := runConfig(t, map[string]string{"STRATUS_DATA_DIR": dataDir})

	if err := runToShutdown(t, cfg); err != nil {
		t.Fatalf("Run = %v, want a clean start", err)
	}

	entries, err := os.ReadDir(cfg.Storage.Dir)
	if err != nil {
		t.Fatalf("the blob directory was never created: %v", err)
	}
	for _, e := range entries {
		if e.Name() != ".tmp" {
			t.Errorf("the probe left %q behind", e.Name())
		}
	}
}

func TestRunRejectsAnUnreachableS3(t *testing.T) {
	t.Parallel()
	// Port 1 refuses immediately, so this is the "credentials or endpoint are
	// wrong" path without waiting on a timeout.
	cfg := runConfig(t, map[string]string{
		"STRATUS_STORAGE_DSN": "s3://key:secret@127.0.0.1:1/bucket?tls=false",
	})
	if err := runToShutdown(t, cfg); err == nil {
		t.Fatal("Run = nil, want a refusal: the server must not come up without its storage")
	}
}

// TestRunRejectsAnUnknownScheme covers a Config built by hand rather than
// parsed, which is the only way an empty scheme can reach the composition root.
func TestRunRejectsAnUnknownScheme(t *testing.T) {
	t.Parallel()
	cfg := runConfig(t, map[string]string{})
	cfg.Storage = config.StorageDSN{Scheme: "ftp"}

	err := runToShutdown(t, cfg)
	if err == nil || !strings.Contains(err.Error(), "ftp") {
		t.Errorf("Run = %v, want it to name the unsupported scheme", err)
	}
}

// TestRunRejectsAnUnwritableBlobDir is the storage-seam counterpart of the data
// directory check: the two are separate paths once a DSN can point elsewhere.
func TestRunRejectsAnUnwritableBlobDir(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root ignores mode bits, so this cannot fail as root")
	}
	parent := filepath.Join(t.TempDir(), "readonly")
	if err := os.Mkdir(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	cfg := runConfig(t, map[string]string{"STRATUS_STORAGE_DSN": "file://" + filepath.Join(parent, "blobs")})

	if err := runToShutdown(t, cfg); err == nil {
		t.Fatal("Run = nil, want a refusal on a blob directory it cannot create")
	}
}

func TestRunRejectsAnUnreachableDatabase(t *testing.T) {
	t.Parallel()
	cfg := runConfig(t, map[string]string{
		"STRATUS_DB_DSN": "postgres://nobody:secret@127.0.0.1:1/stratus?sslmode=disable",
	})
	err := runToShutdown(t, cfg)
	if err == nil {
		t.Fatal("Run = nil, want a refusal: the server must not come up without its database")
	}
	if strings.Contains(err.Error(), "secret") {
		t.Errorf("the error leaks the password: %v", err)
	}
}

// TestRunRejectsAnUnknownDatabaseScheme covers a Config built by hand, the only
// way an empty scheme reaches the composition root.
func TestRunRejectsAnUnknownDatabaseScheme(t *testing.T) {
	t.Parallel()
	cfg := runConfig(t, map[string]string{})
	cfg.Database = config.DatabaseDSN{Scheme: "oracle"}

	err := runToShutdown(t, cfg)
	if err == nil || !strings.Contains(err.Error(), "oracle") {
		t.Errorf("Run = %v, want it to name the unsupported scheme", err)
	}
}

// TestRunMigratesTheDatabase is the startup contract: by the time the server
// serves a request, the schema is there.
func TestRunMigratesTheDatabase(t *testing.T) {
	t.Parallel()
	dataDir := filepath.Join(t.TempDir(), "data")
	cfg := runConfig(t, map[string]string{"STRATUS_DATA_DIR": dataDir})

	if err := runToShutdown(t, cfg); err != nil {
		t.Fatalf("Run = %v, want a clean start", err)
	}

	store, err := sqlite.New(t.Context(), cfg.Database.Path)
	if err != nil {
		t.Fatalf("the database file was never created: %v", err)
	}
	defer func() { _ = store.Close() }()

	// A working query is the proof the migration ran; an empty listing is what
	// a fresh install should answer.
	files, err := store.ListFiles(t.Context(), "owner", "")
	if err != nil {
		t.Fatalf("the schema is not usable after startup: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("a fresh database holds %d files, want none", len(files))
	}
}

// TestRunRestartsOnAnExistingDatabase is the ordinary case that would be easy
// to break: the second start must find its schema already applied and carry on.
func TestRunRestartsOnAnExistingDatabase(t *testing.T) {
	t.Parallel()
	cfg := runConfig(t, map[string]string{})

	for i := range 2 {
		if err := runToShutdown(t, cfg); err != nil {
			t.Fatalf("start %d: %v", i+1, err)
		}
	}
}

// TestRunRejectsAnUnusableDatabasePath covers the other half of openDatabase:
// a path SQLite cannot open at all.
func TestRunRejectsAnUnusableDatabasePath(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "not-a-file.db")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	cfg := runConfig(t, map[string]string{"STRATUS_DB_DSN": "sqlite://" + dir})

	if err := runToShutdown(t, cfg); err == nil {
		t.Fatal("Run = nil, want a refusal: a directory is not a database")
	}
}

// TestRunAgainstPostgres wires the whole chain against a real server:
// STRATUS_DB_DSN, the composition root, the driver and the migration. It skips
// without one, and `make cover` runs it with one, which is how the coverage
// floor notices if this stops running.
func TestRunAgainstPostgres(t *testing.T) {
	t.Parallel()
	dsn := os.Getenv("STRATUS_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("STRATUS_TEST_POSTGRES_DSN is not set; `make test-db` or `make cover` set it")
	}

	if err := runToShutdown(t, runConfig(t, map[string]string{"STRATUS_DB_DSN": dsn})); err != nil {
		t.Errorf("Run against PostgreSQL = %v, want a clean start and shutdown", err)
	}
}

// TestRunAgainstMySQL is the same wiring against the third driver. It points at
// a database of its own -- the one mysql-up asks the image to create -- because
// Run migrates what it opens and the conformance suite is busy making and
// dropping databases beside it.
func TestRunAgainstMySQL(t *testing.T) {
	t.Parallel()
	admin := os.Getenv("STRATUS_TEST_MYSQL_DSN")
	if admin == "" {
		t.Skip("STRATUS_TEST_MYSQL_DSN is not set; `make test-db` or `make cover` set it")
	}
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatalf("parse STRATUS_TEST_MYSQL_DSN: %v", err)
	}
	u.Path = "/stratus"

	if err := runToShutdown(t, runConfig(t, map[string]string{"STRATUS_DB_DSN": u.String()})); err != nil {
		t.Errorf("Run against MySQL = %v, want a clean start and shutdown", err)
	}
}

// TestRunRejectsAReadOnlyDatabase covers the other startup failure the database
// can have: it opens, and then the schema cannot be written. A restore that got
// the file ownership wrong looks exactly like this.
func TestRunRejectsAReadOnlyDatabase(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root ignores mode bits, so this cannot fail as root")
	}
	path := filepath.Join(t.TempDir(), "stratus.db")
	if err := os.WriteFile(path, nil, 0o400); err != nil {
		t.Fatal(err)
	}
	cfg := runConfig(t, map[string]string{"STRATUS_DB_DSN": "sqlite://" + path})

	err := runToShutdown(t, cfg)
	if err == nil {
		t.Fatal("Run = nil, want a refusal on a database it cannot write to")
	}
	// It fails at open rather than at migrate, because the pragmas the driver
	// sets are themselves writes. Fine, and worth pinning: the point is that it
	// stops now and says why, rather than on the first upload.
	if !strings.Contains(err.Error(), "readonly") {
		t.Errorf("the error should name the read-only database, got %v", err)
	}
}
