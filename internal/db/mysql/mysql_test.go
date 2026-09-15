package mysql_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"net/url"
	"os"
	"strings"
	"testing"

	_ "github.com/go-sql-driver/mysql" // registers the "mysql" driver for the maintenance connection

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/db/dbtest"
	"github.com/C0piIot/stratus-backend/internal/db/mysql"
)

// dsnEnv points at a database the test user can create others beside; every
// case makes and drops its own.
const dsnEnv = "STRATUS_TEST_MYSQL_DSN"

// TestConformance is why this package exists: the same suite the other two
// drivers pass, against the dialect that disagrees with them about the most --
// no RETURNING, no indexable path, and a subquery that may not name the table
// being written.
func TestConformance(t *testing.T) {
	t.Parallel()
	dbtest.Run(t, newStore)
}

func newStore(t *testing.T) db.Store {
	t.Helper()
	dsn := makeDatabase(t)

	store, err := mysql.New(t.Context(), dsn)
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
// the suite asks for and is as cheap on MySQL as it is on PostgreSQL.
func makeDatabase(t *testing.T) string {
	t.Helper()
	admin := os.Getenv(dsnEnv)
	if admin == "" {
		t.Skipf("%s is not set; `make test-db` starts MySQL and sets it", dsnEnv)
	}

	// A schema name is an identifier: letters, digits and underscores, and the
	// base32 the rest of this project generates lowercases into exactly that.
	name := "stratus_test_" + strings.ToLower(rand.Text()[:16])

	driverDSN, derr := mysql.DSN(admin)
	if derr != nil {
		t.Fatalf("translate %s: %v", dsnEnv, derr)
	}
	conn, err := sql.Open("mysql", driverDSN)
	if err != nil {
		t.Fatalf("open the maintenance connection: %v", err)
	}
	if _, cerr := conn.ExecContext(t.Context(), "CREATE DATABASE "+name); cerr != nil {
		_ = conn.Close()
		t.Fatalf("CREATE DATABASE %s: %v", name, cerr)
	}
	t.Cleanup(func() {
		// t.Context() is cancelled before cleanups run, and a cancelled context
		// cannot drop anything.
		ctx := context.WithoutCancel(t.Context())
		if _, derr := conn.ExecContext(ctx, "DROP DATABASE "+name); derr != nil {
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
	if _, err := mysql.New(t.Context(), ""); err == nil {
		t.Error("New = nil, want an error")
	}
}

// TestNewRejectsAnUnreachableServer covers the startup path that matters: the
// process must not come up believing it has a database.
func TestNewRejectsAnUnreachableServer(t *testing.T) {
	t.Parallel()
	_, err := mysql.New(t.Context(), "mysql://nobody:secret@127.0.0.1:1/stratus")
	if err == nil {
		t.Fatal("New = nil, want an error")
	}
	if strings.Contains(err.Error(), "secret") {
		t.Errorf("the error leaks the password: %v", err)
	}
}
