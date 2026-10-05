package nextcloud_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/C0piIot/stratus-backend/internal/nextcloud"
)

// Nextcloud runs on SQLite, MySQL/MariaDB and PostgreSQL, so this package has
// to read all three -- and "has to" is not something a SQLite fixture can
// check. The rest of the suite proves what the survey and the import mean;
// this proves the SQL is portable, which is the only thing the other two
// engines can disagree about.
//
// Skipped unless `make test-db` is running, like every other suite that needs
// a server.
const (
	postgresEnv = "STRATUS_TEST_POSTGRES_DSN"
	mysqlEnv    = "STRATUS_TEST_MYSQL_DSN"
)

// foreignSchema is the fixture again, in the types a server engine wants.
// Deliberately the same rows as the SQLite one, so the assertions can be.
const foreignSchema = `
CREATE TABLE oc_storages (numeric_id BIGINT PRIMARY KEY, id VARCHAR(128) NOT NULL);
CREATE TABLE oc_mimetypes (id BIGINT PRIMARY KEY, mimetype VARCHAR(255) NOT NULL);
CREATE TABLE oc_filecache (
	fileid BIGINT PRIMARY KEY, storage BIGINT NOT NULL, path VARCHAR(512) NOT NULL,
	size BIGINT NOT NULL, mtime BIGINT NOT NULL DEFAULT 0,
	mimetype BIGINT, encrypted INT NOT NULL DEFAULT 0);
CREATE TABLE oc_appconfig (appid VARCHAR(64) NOT NULL, configkey VARCHAR(64) NOT NULL, configvalue TEXT);
CREATE TABLE oc_preferences (
	userid VARCHAR(64) NOT NULL, appid VARCHAR(64) NOT NULL,
	configkey VARCHAR(64) NOT NULL, configvalue TEXT);
`

const foreignRows = `
INSERT INTO oc_mimetypes VALUES (1, 'httpd/unix-directory'), (2, 'image/jpeg'), (3, 'video/quicktime'), (4, 'text/plain');
INSERT INTO oc_storages VALUES (1, 'object::user:edu');
INSERT INTO oc_filecache (fileid, storage, path, size, mtime, mimetype) VALUES
	(100, 1, '',                            0,    0,          1),
	(101, 1, 'files',                       0,    1700000000, 1),
	(102, 1, 'files/Photos',                0,    1700000001, 1),
	(103, 1, 'files/Photos/a.jpg',          1000, 1700000002, 2),
	(104, 1, 'files/Photos/b.jpg',          2000, 1700000003, 2),
	(105, 1, 'files/Photos/c.mov',          3000, 1700000004, 3),
	(106, 1, 'files_versions/Photos/a.jpg', 500,  1700000005, 2),
	(107, 1, 'files_trashbin/files/x.jpg',  700,  1700000006, 2),
	(108, 1, 'uploads/chunk',               100,  1700000007, 4),
	(109, 1, 'files/notes.txt',             -1,   1700000008, 4);
`

func TestTheSurveyAndTheImportReadPostgreSQL(t *testing.T) {
	readsEngine(t, foreignInstance(t, postgresEnv, "pgx", postgresDSN))
}

func TestTheSurveyAndTheImportReadMySQL(t *testing.T) {
	readsEngine(t, foreignInstance(t, mysqlEnv, "mysql", mysqlURL))
}

// readsEngine is the SQLite suite's two headline answers, asked of another
// engine: the survey counts the user's own tree, and the import carries the
// key the bucket already uses.
func readsEngine(t *testing.T, source string) {
	t.Helper()
	store := fullBucket(t)

	report := survey(t, source, store, nextcloud.Options{})
	if report.Files.Files != 4 {
		t.Errorf("files = %d, want 4", report.Files.Files)
	}
	// `files` itself and `files/Photos`, as the SQLite suite counts them.
	if report.Folders != 2 {
		t.Errorf("folders = %d, want 2", report.Folders)
	}
	if !report.Adoptable() {
		t.Errorf("the library is not adoptable:\n%+v", report)
	}

	dest := &recorder{}
	imported := importInto(t, dest, store, nextcloud.ImportOptions{
		Options: nextcloud.Options{Source: source},
	})
	if imported.Adopted != 5 {
		t.Errorf("adopted = %d, want 5", imported.Adopted)
	}
	f, found := dest.byPath("Photos/a.jpg")
	if !found {
		t.Fatal("Photos/a.jpg was not adopted")
	}
	if f.BlobKey != "urn:oid:103" {
		t.Errorf("blob key = %q, want urn:oid:103", f.BlobKey)
	}
}

// foreignInstance creates a database of its own on the server, fills it with
// the fixture, and answers the DSN to read it back with.
func foreignInstance(t *testing.T, env, driver string, dsnFor func(string, string) string) string {
	t.Helper()
	maintenance := os.Getenv(env)
	if maintenance == "" {
		t.Skipf("%s is not set; `make test-db` sets it", env)
	}

	admin, aerr := sql.Open(driver, dsnFor(maintenance, ""))
	if aerr != nil {
		t.Fatalf("open the maintenance database: %v", aerr)
	}

	name := "nc_" + strings.ToLower(rand.Text()[:10])
	if _, cerr := admin.ExecContext(t.Context(), "CREATE DATABASE "+name); cerr != nil {
		_ = admin.Close()
		t.Fatalf("create %s: %v", name, cerr)
	}
	// The connection it is dropped over has to outlive the test, so it is
	// closed here and not deferred above -- and t.Context() is cancelled
	// before a cleanup runs, which cannot drop anything.
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		if _, derr := admin.ExecContext(ctx, "DROP DATABASE "+name); derr != nil {
			t.Errorf("drop %s: %v", name, derr)
		}
		_ = admin.Close()
	})

	conn, err := sql.Open(driver, dsnFor(maintenance, name))
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	defer func() { _ = conn.Close() }()

	// One statement at a time: MySQL's driver does not take several in one
	// Exec unless it is asked to, and asking would be a connection setting for
	// the sake of a fixture.
	for _, stmt := range statements(foreignSchema + foreignRows) {
		if _, err := conn.ExecContext(t.Context(), stmt); err != nil {
			t.Fatalf("fixture: %v\n%s", err, stmt)
		}
	}
	return nextcloudDSN(t, env, name)
}

// postgresDSN swaps the database out of the maintenance DSN, which is the
// driver's own URL form.
func postgresDSN(maintenance, name string) string {
	if name == "" {
		return maintenance
	}
	u, err := url.Parse(maintenance)
	if err != nil {
		return maintenance
	}
	u.Path = "/" + name
	return u.String()
}

// mysqlURL does the same and then turns the URL into what the driver wants,
// since the test opens the server itself rather than going through this
// package.
func mysqlURL(maintenance, name string) string {
	u, err := url.Parse(maintenance)
	if err != nil {
		return maintenance
	}
	if name != "" {
		u.Path = "/" + name
	}
	password, _ := u.User.Password()
	return fmt.Sprintf("%s:%s@tcp(%s)%s", u.User.Username(), password, u.Host, u.Path)
}

// nextcloudDSN is what goes into Options.Source: the URL form, because reading
// a foreign database is the one thing this package does with a DSN and it
// takes the shape somebody types.
func nextcloudDSN(t *testing.T, env, name string) string {
	t.Helper()
	u, err := url.Parse(os.Getenv(env))
	if err != nil {
		t.Fatalf("parse %s: %v", env, err)
	}
	u.Path = "/" + name
	return u.String()
}

// statements splits the fixture on semicolons at the end of a line, which is
// all the structure it has.
func statements(script string) []string {
	var out []string
	for _, stmt := range strings.Split(script, ";") {
		if trimmed := strings.TrimSpace(stmt); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
