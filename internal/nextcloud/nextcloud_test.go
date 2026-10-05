package nextcloud_test

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/C0piIot/stratus-backend/internal/nextcloud"
	"github.com/C0piIot/stratus-backend/internal/storage"
	"github.com/C0piIot/stratus-backend/internal/storage/disk"
	"github.com/C0piIot/stratus-backend/internal/storage/storagetest"
)

// The fixture is a real Nextcloud schema, cut down to the columns this reads.
// A survey tested against a hand-rolled fake of the filecache would be testing
// the fake -- and the shape of that table is the whole thing being relied on.
const schema = `
CREATE TABLE oc_storages (numeric_id INTEGER PRIMARY KEY, id TEXT NOT NULL);
CREATE TABLE oc_mimetypes (id INTEGER PRIMARY KEY, mimetype TEXT NOT NULL);
CREATE TABLE oc_filecache (
	fileid INTEGER PRIMARY KEY, storage INTEGER NOT NULL, path TEXT NOT NULL,
	size INTEGER NOT NULL, mtime INTEGER NOT NULL DEFAULT 0,
	mimetype INTEGER, encrypted INTEGER NOT NULL DEFAULT 0);
CREATE TABLE oc_appconfig (appid TEXT NOT NULL, configkey TEXT NOT NULL, configvalue TEXT);
CREATE TABLE oc_preferences (
	userid TEXT NOT NULL, appid TEXT NOT NULL, configkey TEXT NOT NULL, configvalue TEXT);

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

// instance writes a Nextcloud database and returns its path.
func instance(t *testing.T, extra ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nextcloud.db")

	conn, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	for _, stmt := range append([]string{schema}, extra...) {
		if _, err := conn.ExecContext(t.Context(), stmt); err != nil {
			t.Fatalf("fixture: %v", err)
		}
	}
	return path
}

// bucket is a real blob store holding the objects named here, so that what the
// survey asks is a real Stat against a real backend.
func bucket(t *testing.T, objects map[string]int) storage.Storage {
	t.Helper()
	store, err := disk.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	for key, size := range objects {
		body := strings.Repeat("x", size)
		if _, err := store.Put(t.Context(), key, strings.NewReader(body), int64(size)); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	return store
}

// survey is the fixture above against the bucket above.
func survey(t *testing.T, db string, store storage.Storage, opts nextcloud.Options) *nextcloud.Report {
	t.Helper()
	opts.Source = db
	report, err := nextcloud.Survey(t.Context(), store, opts)
	if err != nil {
		t.Fatalf("survey: %v", err)
	}
	return report
}

func TestTheSurveyCountsOnlyTheUsersOwnFiles(t *testing.T) {
	report := survey(t, instance(t), bucket(t, map[string]int{
		"urn:oid:103": 1000, "urn:oid:105": 3000, "urn:oid:109": 42,
	}), nextcloud.Options{})

	if report.Files.Files != 4 {
		t.Errorf("files = %d, want 4", report.Files.Files)
	}
	// Folders are `files` itself and `files/Photos`. The storage's own root
	// row has an empty path and is nobody's file.
	if report.Folders != 2 {
		t.Errorf("folders = %d, want 2", report.Folders)
	}
	// The sizes of the four, with the unscanned one contributing nothing.
	if report.Files.Bytes != 6000 {
		t.Errorf("bytes = %d, want 6000", report.Files.Bytes)
	}
	for _, area := range []string{"files_versions", "files_trashbin", "uploads"} {
		if got := report.Skipped[area].Files; got != 1 {
			t.Errorf("skipped[%s] = %d, want 1", area, got)
		}
	}
	if report.Unscanned != 1 {
		t.Errorf("unscanned = %d, want 1", report.Unscanned)
	}
}

func TestTheSurveyNamesWhatTheBucketDoesNotHave(t *testing.T) {
	report := survey(t, instance(t), bucket(t, map[string]int{
		"urn:oid:103": 1000,
		"urn:oid:105": 2999, // the database says 3000
		"urn:oid:109": 42,
	}), nextcloud.Options{})

	if report.MissingCount != 1 || len(report.Missing) != 1 {
		t.Fatalf("missing = %d (%d named), want 1", report.MissingCount, len(report.Missing))
	}
	if report.Missing[0].Key != "urn:oid:104" {
		t.Errorf("missing key = %q, want urn:oid:104", report.Missing[0].Key)
	}
	if report.Missing[0].Path != "Photos/b.jpg" {
		t.Errorf("missing path = %q, want Photos/b.jpg", report.Missing[0].Path)
	}
	if report.MissingBytes != 2000 {
		t.Errorf("missing bytes = %d, want 2000", report.MissingBytes)
	}
	if report.MismatchCount != 1 || len(report.Mismatch) != 1 {
		t.Fatalf("mismatch = %d (%d named), want 1", report.MismatchCount, len(report.Mismatch))
	}
	if m := report.Mismatch[0]; m.Size != 3000 || m.Actual != 2999 {
		t.Errorf("mismatch = database %d, bucket %d; want 3000 and 2999", m.Size, m.Actual)
	}
	// Present is the one that matches plus the unscanned one, which is taken
	// as found: the database never had a size to disagree with.
	if report.Present.Files != 2 {
		t.Errorf("present = %d, want 2", report.Present.Files)
	}
	if report.Adoptable() {
		t.Error("a library with a missing object reports as adoptable")
	}
}

func TestAnIntactLibraryIsAdoptable(t *testing.T) {
	report := survey(t, instance(t), bucket(t, map[string]int{
		"urn:oid:103": 1000, "urn:oid:104": 2000, "urn:oid:105": 3000, "urn:oid:109": 42,
	}), nextcloud.Options{})

	if !report.Adoptable() {
		t.Fatalf("not adoptable: %d missing, %d wrong size, %d unanswered",
			report.MissingCount, report.MismatchCount, report.FailedCount)
	}
	if report.Present.Files != 4 {
		t.Errorf("present = %d, want 4", report.Present.Files)
	}
}

func TestEncryptionStopsTheSurveyBeforeItWalksAnything(t *testing.T) {
	db := instance(t, `INSERT INTO oc_appconfig VALUES ('core', 'encryption_enabled', 'yes');`)
	report := survey(t, db, bucket(t, nil), nextcloud.Options{})

	if !report.Blocked() {
		t.Fatal("an encrypted instance is not reported as blocked")
	}
	// Nothing was walked: counting a hundred thousand files as present would
	// be true and completely misleading, since every one of them is ciphertext.
	if report.Files.Files != 0 || report.Present.Files != 0 {
		t.Errorf("walked anyway: %d files, %d present", report.Files.Files, report.Present.Files)
	}
	if report.Adoptable() {
		t.Error("an encrypted instance reports as adoptable")
	}
}

func TestAFileMarkedEncryptedBlocksEvenWithTheSettingOff(t *testing.T) {
	db := instance(t, `UPDATE oc_filecache SET encrypted = 1 WHERE fileid = 103;`)
	report := survey(t, db, bucket(t, nil), nextcloud.Options{})

	if report.Encryption.Rows != 1 {
		t.Errorf("encrypted rows = %d, want 1", report.Encryption.Rows)
	}
	if !report.Blocked() {
		t.Error("a file marked encrypted does not block the import")
	}
}

func TestMoreThanOneUserHasToBeNamed(t *testing.T) {
	db := instance(t, `
		INSERT INTO oc_storages VALUES (2, 'object::user:ana');
		INSERT INTO oc_filecache (fileid, storage, path, size, mimetype)
			VALUES (200, 2, 'files/hers.jpg', 10, 2);`)

	_, err := nextcloud.Survey(t.Context(), bucket(t, nil), nextcloud.Options{Source: db})
	if err == nil {
		t.Fatal("two users and no --user was accepted")
	}
	for _, want := range []string{"edu", "ana"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %q: %v", want, err)
		}
	}

	report := survey(t, db, bucket(t, map[string]int{"urn:oid:200": 10}), nextcloud.Options{User: "ana"})
	if report.Storage.NumericID != 2 {
		t.Errorf("storage = %d, want 2", report.Storage.NumericID)
	}
	if report.Files.Files != 1 {
		t.Errorf("files = %d, want 1 -- the other user's tree was counted", report.Files.Files)
	}
}

func TestAMultibucketInstanceIsReportedAndNotAdoptable(t *testing.T) {
	db := instance(t, `
		INSERT INTO oc_preferences VALUES ('edu', 'homeobjectstore', 'bucket', 'nextcloud-7');`)
	report := survey(t, db, bucket(t, map[string]int{
		"urn:oid:103": 1000, "urn:oid:104": 2000, "urn:oid:105": 3000, "urn:oid:109": 42,
	}), nextcloud.Options{})

	if len(report.Buckets) != 1 || report.Buckets[0].Bucket != "nextcloud-7" {
		t.Fatalf("buckets = %+v, want one mapping to nextcloud-7", report.Buckets)
	}
	// Every object is where it should be, and it still cannot be adopted: this
	// server is configured with one bucket and the instance has several.
	if report.Adoptable() {
		t.Error("a multibucket instance reports as adoptable")
	}
}

func TestTheObjectPrefixIsTheOneTheInstanceUses(t *testing.T) {
	report := survey(t, instance(t), bucket(t, map[string]int{
		"blobs/103": 1000, "blobs/104": 2000, "blobs/105": 3000, "blobs/109": 42,
	}), nextcloud.Options{ObjectPrefix: "blobs/"})

	if !report.Adoptable() {
		t.Fatalf("a non-default prefix was not honoured: %d missing", report.MissingCount)
	}
}

func TestTheReportSaysNothingWasWritten(t *testing.T) {
	report := survey(t, instance(t), bucket(t, nil), nextcloud.Options{})

	var out strings.Builder
	if err := report.Render(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Nothing was written") {
		t.Errorf("the report does not say it wrote nothing:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "urn:oid:103") {
		t.Errorf("the report does not name a missing object:\n%s", out.String())
	}
}

func TestATablePrefixThatIsNotAnIdentifierIsRefused(t *testing.T) {
	// The prefix is concatenated into every query, because SQL has no
	// placeholder for a table name. This is the check that stands in for one.
	_, err := nextcloud.Survey(t.Context(), bucket(t, nil), nextcloud.Options{
		Source:      instance(t),
		TablePrefix: "oc_; DROP TABLE oc_filecache; --",
	})
	if err == nil {
		t.Fatal("a table prefix with punctuation in it was accepted")
	}
	if !strings.Contains(err.Error(), "identifier") {
		t.Errorf("error does not say why: %v", err)
	}
}

func TestADatabaseThatIsNotNextcloudsIsAnError(t *testing.T) {
	empty := filepath.Join(t.TempDir(), "empty.db")
	conn, err := sql.Open("sqlite", "file:"+empty)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(t.Context(), "CREATE TABLE unrelated (x INTEGER)"); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()

	if _, err := nextcloud.Survey(t.Context(), bucket(t, nil), nextcloud.Options{Source: empty}); err == nil {
		t.Fatal("a database with none of Nextcloud's tables was accepted")
	}
}

func TestAMissingDatabaseIsAnError(t *testing.T) {
	_, err := nextcloud.Survey(t.Context(), bucket(t, nil), nextcloud.Options{
		Source: filepath.Join(t.TempDir(), "nothing-here.db"),
	})
	if err == nil {
		t.Fatal("a database that is not there was accepted")
	}
}

func TestThePathToTheDatabaseIsRequired(t *testing.T) {
	if _, err := nextcloud.Survey(t.Context(), bucket(t, nil), nextcloud.Options{}); err == nil {
		t.Fatal("an empty path was accepted")
	}
}

func TestAnAdoptableReportSaysSo(t *testing.T) {
	report := survey(t, instance(t), bucket(t, map[string]int{
		"urn:oid:103": 1000, "urn:oid:104": 2000, "urn:oid:105": 3000, "urn:oid:109": 42,
	}), nextcloud.Options{})

	var out strings.Builder
	if err := report.Render(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "without moving a byte") {
		t.Errorf("an intact library is not reported as adoptable:\n%s", out.String())
	}
}

func TestAMultibucketReportNamesTheBuckets(t *testing.T) {
	db := instance(t, `INSERT INTO oc_preferences VALUES ('edu', 'homeobjectstore', 'bucket', 'nextcloud-7');`)
	report := survey(t, db, bucket(t, nil), nextcloud.Options{})

	var out strings.Builder
	if err := report.Render(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "nextcloud-7") {
		t.Errorf("the report does not name the bucket:\n%s", out.String())
	}
}

func TestAnEncryptedReportStopsAtTheReason(t *testing.T) {
	db := instance(t, `INSERT INTO oc_appconfig VALUES ('encryption', 'enabled', 'yes');`)
	report := survey(t, db, bucket(t, nil), nextcloud.Options{})

	var out strings.Builder
	if err := report.Render(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "ciphertext") {
		t.Errorf("the report does not say why it stopped:\n%s", out.String())
	}
	if strings.Contains(out.String(), "The bucket, asked about") {
		t.Errorf("the report walked the library anyway:\n%s", out.String())
	}
}

// The sweep lists the whole bucket and puts what no row claims into the trash,
// so what an import would leave behind is a number to have before the import
// rather than a month after it.
func TestTheSurveyCountsWhatTheImportWouldLeaveInTheBucket(t *testing.T) {
	report := survey(t, instance(t), bucket(t, map[string]int{
		"urn:oid:103": 1000, "urn:oid:104": 2000, "urn:oid:105": 3000, "urn:oid:109": 42,
		// A version and a trashed file: Nextcloud's, in the same bucket, and
		// adopted by nothing.
		"urn:oid:106": 500, "urn:oid:107": 700,
	}), nextcloud.Options{})

	if report.UnclaimedCount != 2 {
		t.Errorf("unclaimed = %d, want 2", report.UnclaimedCount)
	}
	if report.UnclaimedBytes != 1200 {
		t.Errorf("unclaimed bytes = %d, want 1200", report.UnclaimedBytes)
	}
	if report.UnclaimedErr != "" {
		t.Errorf("unclaimed err = %q", report.UnclaimedErr)
	}

	var out strings.Builder
	if err := report.Render(&out); err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, want := range []string{"The rest of the bucket", "urn:oid:106", "thirty days"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the report does not say %q:\n%s", want, out.String())
		}
	}
}

func TestASurveyOfABucketWithNothingElseInItSaysSo(t *testing.T) {
	report := survey(t, instance(t), bucket(t, map[string]int{
		"urn:oid:103": 1000, "urn:oid:104": 2000, "urn:oid:105": 3000, "urn:oid:109": 42,
	}), nextcloud.Options{})

	if report.UnclaimedCount != 0 {
		t.Fatalf("unclaimed = %d, want 0", report.UnclaimedCount)
	}
	var out strings.Builder
	if err := report.Render(&out); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(out.String(), "nothing else is in it") {
		t.Errorf("the report does not say the bucket is clean:\n%s", out.String())
	}
}

// A bucket that cannot be listed is a worse report, not a failed survey: the
// drift question has already been answered by then, and a zero that means "not
// asked" would read as "nothing there".
func TestASurveyReportsABucketItCannotList(t *testing.T) {
	store := storagetest.FailOn(t, bucket(t, map[string]int{
		"urn:oid:103": 1000, "urn:oid:104": 2000, "urn:oid:105": 3000, "urn:oid:109": 42,
	}), "List")

	report := survey(t, instance(t), store, nextcloud.Options{})
	if report.UnclaimedErr == "" {
		t.Fatal("the survey did not say the bucket could not be listed")
	}
	if !report.Adoptable() {
		t.Error("a bucket that could not be listed stopped an adoption it should not have")
	}

	var out strings.Builder
	if err := report.Render(&out); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(out.String(), "could not be listed") {
		t.Errorf("the report does not say so:\n%s", out.String())
	}
}
