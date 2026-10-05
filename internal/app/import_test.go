package app_test

import (
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/app"
	"github.com/C0piIot/stratus-backend/internal/config"
	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/db/sqlite"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/nextcloud"
	"github.com/C0piIot/stratus-backend/internal/storage/disk"
)

// wholeBucket holds every object the fixture claims, which is the state the
// survey has to find before an import is allowed to start.
func wholeBucket(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	store, err := disk.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	for key, body := range map[string]string{
		"urn:oid:3": "held.js",
		"urn:oid:4": "gone.jpeg",
		"urn:oid:7": "inner",
	} {
		if _, err := store.Put(t.Context(), key, strings.NewReader(body), int64(len(body))); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// adoptable is a configuration pointed at a bucket that agrees with the
// fixture, with somewhere of its own to keep the rows.
func adoptable(t *testing.T) config.Config {
	t.Helper()
	return config.Config{
		Username: "edu",
		Storage:  config.StorageDSN{Scheme: config.SchemeFile, Dir: wholeBucket(t)},
		Database: config.DatabaseDSN{
			Scheme: config.SchemeSQLite,
			Path:   filepath.Join(t.TempDir(), "stratus.db"),
		},
	}
}

// opened is the library as the server would see it afterwards, which is the
// only assertion that matters: a row an import wrote is a file like any other.
func opened(t *testing.T, cfg config.Config, path string) (db.File, string) {
	t.Helper()
	blobs, err := disk.New(cfg.Storage.Dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blobs.Close() })

	meta, err := sqlite.New(t.Context(), cfg.Database.Path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = meta.Close() })

	body, f, err := files.New(blobs, meta).Open(t.Context(), cfg.Username, path)
	if err != nil {
		t.Fatalf("open %q: %v", path, err)
	}
	defer func() { _ = body.Close() }()

	bytes, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	return f, string(bytes)
}

func TestTheImportWritesRowsThatAreOrdinaryFiles(t *testing.T) {
	cfg := adoptable(t)

	var out strings.Builder
	if err := app.ImportNextcloud(t.Context(), cfg,
		nextcloud.ImportOptions{Options: nextcloud.Options{Source: fixture}}, &out); err != nil {
		t.Fatalf("import: %v", err)
	}
	if !strings.Contains(out.String(), "Not a byte was moved") {
		t.Errorf("the report does not say what it did:\n%s", out.String())
	}

	// The folder came across, and so did the file under it -- read back
	// through the same service a WebDAV GET goes through.
	f, body := opened(t, cfg, "album/inner.jpg")
	if body != "inner" {
		t.Errorf("read back %q", body)
	}
	if f.BlobKey != "urn:oid:7" {
		t.Errorf("blob key = %q, want the object Nextcloud already wrote", f.BlobKey)
	}
}

func TestTheImportRunsTheSurveyAndRefusesADriftedBucket(t *testing.T) {
	cfg := adoptable(t)
	// The bucket the survey test uses holds one of the two files, which is
	// the drift that makes an import a data loss event.
	cfg.Storage.Dir = bucket(t)

	var out strings.Builder
	err := app.ImportNextcloud(t.Context(), cfg,
		nextcloud.ImportOptions{Options: nextcloud.Options{Source: fixture}}, &out)
	if err == nil {
		t.Fatal("an import against a bucket that does not agree was allowed")
	}
	if !strings.Contains(out.String(), "do not agree") {
		t.Errorf("the survey was not printed before the refusal:\n%s", out.String())
	}
}

func TestTheImportNeedsTheUserToFileThingsUnder(t *testing.T) {
	cfg := adoptable(t)
	cfg.Username = ""

	var out strings.Builder
	if err := app.ImportNextcloud(t.Context(), cfg,
		nextcloud.ImportOptions{Options: nextcloud.Options{Source: fixture}}, &out); err == nil {
		t.Fatal("an import with no configured user was allowed")
	}
}

func TestRunningTheImportTwiceChangesNothingTheSecondTime(t *testing.T) {
	cfg := adoptable(t)
	opts := nextcloud.ImportOptions{Options: nextcloud.Options{Source: fixture}}

	var first, second strings.Builder
	if err := app.ImportNextcloud(t.Context(), cfg, opts, &first); err != nil {
		t.Fatalf("first import: %v", err)
	}
	if err := app.ImportNextcloud(t.Context(), cfg, opts, &second); err != nil {
		t.Fatalf("second import: %v", err)
	}

	// An interrupted migration has to be finishable by running it again, so
	// the paths that are already there are skipped rather than replaced.
	if !strings.Contains(second.String(), "already there") {
		t.Errorf("the second run did not skip what the first wrote:\n%s", second.String())
	}
	if _, body := opened(t, cfg, "album/inner.jpg"); body != "inner" {
		t.Errorf("read back %q", body)
	}
}
