package app_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/app"
	"github.com/C0piIot/stratus-backend/internal/config"
	"github.com/C0piIot/stratus-backend/internal/nextcloud"
	"github.com/C0piIot/stratus-backend/internal/storage/disk"
)

// The fixture is a real Nextcloud schema with two files in it, one of which
// this test puts in the bucket and the other it does not.
//
// A file rather than a database built here, because building one means opening
// a SQLite file directly and only internal/db/sqlite and internal/nextcloud may
// do that (see .golangci.yml). It is the smoke suite's copy rather than a
// second one under testdata/: the same fixture is fed to the real container by
// scripts/smoke.sh, and two binary files that have to stay identical are one
// more than this needs.
const fixture = "../../scripts/testdata/nextcloud.db"

// bucket is a disk blob store holding the one object the fixture's `held.jpg`
// claims, under the name Nextcloud already gave it.
func bucket(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	store, err := disk.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	if _, err := store.Put(t.Context(), "urn:oid:3", strings.NewReader("held.js"), 7); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestTheSurveyReadsTheConfiguredBucket(t *testing.T) {
	cfg := config.Config{Storage: config.StorageDSN{Scheme: config.SchemeFile, Dir: bucket(t)}}

	var out strings.Builder
	if err := app.SurveyNextcloud(t.Context(), cfg, nextcloud.Options{Source: fixture}, &out); err != nil {
		t.Fatalf("survey: %v", err)
	}

	// One file is in the bucket and the other is not, so the survey has to say
	// the library cannot be adopted and name the object that is missing.
	if !strings.Contains(out.String(), "urn:oid:4") {
		t.Errorf("the missing object is not named:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "do not agree") {
		t.Errorf("a drifted bucket is not reported as such:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "Nothing was written") {
		t.Errorf("the report does not say it wrote nothing:\n%s", out.String())
	}
}

func TestTheSurveyNeedsABlobStoreItCanOpen(t *testing.T) {
	cfg := config.Config{Storage: config.StorageDSN{Scheme: "nonesuch"}}

	var out strings.Builder
	err := app.SurveyNextcloud(t.Context(), cfg, nextcloud.Options{Source: fixture}, &out)
	if err == nil {
		t.Fatal("a storage DSN with no backend behind it was accepted")
	}
	if out.Len() != 0 {
		t.Errorf("a failed survey still wrote a report:\n%s", out.String())
	}
}

func TestTheSurveyReportsADatabaseItCannotRead(t *testing.T) {
	cfg := config.Config{Storage: config.StorageDSN{Scheme: config.SchemeFile, Dir: bucket(t)}}
	missing := filepath.Join(t.TempDir(), "not-an-instance.db")

	var out strings.Builder
	if err := app.SurveyNextcloud(t.Context(), cfg, nextcloud.Options{Source: missing}, &out); err == nil {
		t.Fatal("a database that is not there was accepted")
	}
}
