package postgres_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/db/dbtest"
	"github.com/C0piIot/stratus-backend/internal/db/postgres"
)

// dsnEnv points at a maintenance database; every case creates and drops its own
// beside it.
const dsnEnv = "STRATUS_TEST_POSTGRES_DSN"

// TestConformance is why this package exists: the same suite SQLite passes,
// against a database that disagrees with it about types, placeholders and error
// codes.
func TestConformance(t *testing.T) {
	t.Parallel()
	dbtest.Run(t, newStore)
}

func newStore(t *testing.T) db.Store {
	t.Helper()
	dsn := makeDatabase(t)

	store, err := postgres.New(t.Context(), dsn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	if err := store.Migrate(t.Context()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return store
}

// makeDatabase gives every case a database of its own, which is the isolation
// the suite asks for and is cheap on PostgreSQL.
func makeDatabase(t *testing.T) string {
	t.Helper()
	admin := os.Getenv(dsnEnv)
	if admin == "" {
		t.Skipf("%s is not set; `make test-db` starts PostgreSQL and sets it", dsnEnv)
	}

	// Database names are lowercase and must start with a letter.
	name := "stratus_test_" + strings.ToLower(rand.Text()[:16])

	conn, err := sql.Open("pgx", admin)
	if err != nil {
		t.Fatalf("open the maintenance database: %v", err)
	}
	if _, cerr := conn.ExecContext(t.Context(), "CREATE DATABASE "+name); cerr != nil {
		_ = conn.Close()
		t.Fatalf("CREATE DATABASE %s: %v", name, cerr)
	}
	t.Cleanup(func() {
		// t.Context() is cancelled before cleanups run, and a cancelled context
		// cannot drop anything.
		ctx := context.WithoutCancel(t.Context())
		if _, derr := conn.ExecContext(ctx, "DROP DATABASE "+name+" WITH (FORCE)"); derr != nil {
			t.Errorf("DROP DATABASE %s: %v", name, derr)
		}
		// Closed here rather than deferred: a defer would fire when this
		// function returns, long before the cleanup that needs the connection.
		if cerr := conn.Close(); cerr != nil {
			t.Errorf("close the maintenance connection: %v", cerr)
		}
	})

	u, perr := url.Parse(admin)
	if perr != nil {
		t.Fatalf("parse %s: %v", dsnEnv, perr)
	}
	u.Path = "/" + name
	return u.String()
}

func TestNewRejectsAnEmptyDSN(t *testing.T) {
	t.Parallel()
	if _, err := postgres.New(t.Context(), ""); err == nil {
		t.Error("New = nil, want an error")
	}
}

// TestNewRejectsAnUnreachableServer covers the startup path that matters: the
// process must not come up believing it has a database.
func TestNewRejectsAnUnreachableServer(t *testing.T) {
	t.Parallel()
	_, err := postgres.New(t.Context(), "postgres://nobody:secret@127.0.0.1:1/stratus?sslmode=disable")
	if err == nil {
		t.Fatal("New = nil, want an error")
	}
	if strings.Contains(err.Error(), "secret") {
		t.Errorf("the error leaks the password: %v", err)
	}
}

// TestBlobKeysOnAClosedStore covers the error path the collector would hit if
// it kept iterating through a shutdown: it reports rather than panics.
func TestBlobKeysOnAClosedStore(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// The cleanup closes it again, which database/sql tolerates.

	var got error
	for _, err := range store.BlobKeys(t.Context()) {
		got = err
		break
	}
	if got == nil {
		t.Error("iterating a closed store reported no error")
	}
}
