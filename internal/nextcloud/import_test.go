package nextcloud_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/nextcloud"
	"github.com/C0piIot/stratus-backend/internal/storage"
)

// recorder is a destination that remembers what it was handed. The real one is
// files.Service.Adopt, which has tests of its own in its own package; what is
// under test here is what comes out of the filecache and in what order.
type recorder struct {
	rows   []db.File
	owners []string
	taken  map[string]bool
}

func (r *recorder) Adopt(_ context.Context, owner string, rows []db.File) (int, int, error) {
	r.owners = append(r.owners, owner)
	var adopted, skipped int
	for _, f := range rows {
		if r.taken[f.Path] {
			skipped++
			continue
		}
		r.rows = append(r.rows, f)
		adopted++
	}
	return adopted, skipped, nil
}

func (r *recorder) byPath(path string) (db.File, bool) {
	for _, f := range r.rows {
		if f.Path == path {
			return f, true
		}
	}
	return db.File{}, false
}

func importInto(
	t *testing.T,
	dest nextcloud.Destination,
	store storage.Storage,
	opts nextcloud.ImportOptions,
) *nextcloud.ImportReport {
	t.Helper()
	if opts.Source == "" {
		opts.Source = instance(t)
	}
	report, err := nextcloud.Import(t.Context(), store, dest, "edu", opts)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	return report
}

func fullBucket(t *testing.T) storage.Storage {
	t.Helper()
	return bucket(t, map[string]int{
		"urn:oid:103": 1000, "urn:oid:104": 2000, "urn:oid:105": 3000, "urn:oid:109": 42,
	})
}

func TestTheImportAdoptsTheUsersOwnTreeAndNothingElse(t *testing.T) {
	dest := &recorder{}
	report := importInto(t, dest, fullBucket(t), nextcloud.ImportOptions{})

	if report.Files != 4 {
		t.Errorf("files = %d, want 4", report.Files)
	}
	// `files` itself is the root of the user's tree and is not a folder
	// anybody made, so only Photos comes across.
	if report.Folders != 1 {
		t.Errorf("folders = %d, want 1", report.Folders)
	}
	if report.Adopted != 5 {
		t.Errorf("adopted = %d, want 5", report.Adopted)
	}

	for _, path := range []string{
		"files_versions/Photos/a.jpg", "files_trashbin/files/x.jpg", "uploads/chunk", "files",
	} {
		if _, found := dest.byPath(path); found {
			t.Errorf("%q was adopted and should not have been", path)
		}
	}
}

func TestTheImportCarriesTheKeyTheBucketAlreadyUses(t *testing.T) {
	dest := &recorder{}
	importInto(t, dest, fullBucket(t), nextcloud.ImportOptions{})

	f, found := dest.byPath("Photos/a.jpg")
	if !found {
		t.Fatal("Photos/a.jpg was not adopted")
	}
	if f.BlobKey != "urn:oid:103" {
		t.Errorf("blob key = %q, want urn:oid:103", f.BlobKey)
	}
	if f.Size != 1000 {
		t.Errorf("size = %d, want 1000", f.Size)
	}
	if f.MIMEType != "image/jpeg" {
		t.Errorf("type = %q", f.MIMEType)
	}
	if want := time.Unix(1700000002, 0).UTC(); !f.MTime.Equal(want) {
		t.Errorf("mtime = %s, want %s", f.MTime, want)
	}
	// Nextcloud has no hash of the content to adopt, so the honest import
	// writes no validator rather than inventing one.
	if f.ETag != "" {
		t.Errorf("etag = %q, want none", f.ETag)
	}
}

func TestTheImportWritesAParentBeforeWhatIsUnderIt(t *testing.T) {
	dest := &recorder{}
	importInto(t, dest, fullBucket(t), nextcloud.ImportOptions{})

	var seenFolder bool
	for _, f := range dest.rows {
		if f.Path == "Photos" {
			seenFolder = true
			continue
		}
		if strings.HasPrefix(f.Path, "Photos/") && !seenFolder {
			t.Fatalf("%q came before its folder", f.Path)
		}
	}
}

func TestTheImportCanHangTheLibraryUnderAPath(t *testing.T) {
	dest := &recorder{}
	report := importInto(t, dest, fullBucket(t), nextcloud.ImportOptions{Into: "nextcloud"})

	if report.Into != "nextcloud" {
		t.Errorf("into = %q", report.Into)
	}
	if _, found := dest.byPath("nextcloud/Photos/a.jpg"); !found {
		t.Error("the tree did not land under the path it was given")
	}
	if _, found := dest.byPath("Photos/a.jpg"); found {
		t.Error("the tree landed at the root as well")
	}
}

func TestTheImportSkipsWhatIsAlreadyThereRatherThanReplacingIt(t *testing.T) {
	dest := &recorder{taken: map[string]bool{"Photos/a.jpg": true}}
	report := importInto(t, dest, fullBucket(t), nextcloud.ImportOptions{})

	if report.Skipped != 1 {
		t.Errorf("skipped = %d, want 1", report.Skipped)
	}
	if report.Adopted != 4 {
		t.Errorf("adopted = %d, want 4", report.Adopted)
	}
}

func TestTheImportRefusesAPathThisServerCannotStoreAndCarriesOn(t *testing.T) {
	// A name with a control character in it is legal on Nextcloud's side and
	// is not storable here. One of those in a hundred thousand files is not a
	// reason to abandon a migration, so it is counted and named.
	odd := instance(t, "INSERT INTO oc_filecache (fileid, storage, path, size, mtime, mimetype) VALUES"+
		" (110, 1, 'files/ba\x01d.jpg', 10, 1700000009, 2);")
	dest := &recorder{}
	report := importInto(t, dest, bucket(t, map[string]int{
		"urn:oid:103": 1000, "urn:oid:104": 2000, "urn:oid:105": 3000,
		"urn:oid:109": 42, "urn:oid:110": 10,
	}), nextcloud.ImportOptions{Options: nextcloud.Options{Source: odd}})

	if report.RefusedCount != 1 {
		t.Fatalf("refused = %d, want 1", report.RefusedCount)
	}
	if len(report.Refused) != 1 || !strings.Contains(report.Refused[0].Err, "control character") {
		t.Errorf("refusal = %+v", report.Refused)
	}
	if report.Adopted != 5 {
		t.Errorf("adopted = %d, want the rest of the library", report.Adopted)
	}
}

func TestTheImportComputesTheRealETagWhenItIsAskedTo(t *testing.T) {
	dest := &recorder{}
	report := importInto(t, dest, fullBucket(t), nextcloud.ImportOptions{ETag: true})

	if report.Hashed != 4 {
		t.Errorf("hashed = %d, want 4", report.Hashed)
	}
	f, found := dest.byPath("Photos/a.jpg")
	if !found {
		t.Fatal("Photos/a.jpg was not adopted")
	}
	// The bucket holds a thousand x's under that key, and this server's ETag
	// is the SHA-256 of the bytes it serves.
	sum := sha256.Sum256([]byte(strings.Repeat("x", 1000)))
	if want := hex.EncodeToString(sum[:]); f.ETag != want {
		t.Errorf("etag = %q, want %q", f.ETag, want)
	}
}

func TestTheImportNeedsAnOwnerAndADatabase(t *testing.T) {
	store := fullBucket(t)
	dest := &recorder{}

	if _, err := nextcloud.Import(t.Context(), store, dest, "",
		nextcloud.ImportOptions{Options: nextcloud.Options{Source: instance(t)}}); err == nil {
		t.Error("an import with no owner was accepted")
	}
	if _, err := nextcloud.Import(t.Context(), store, dest, "edu", nextcloud.ImportOptions{}); err == nil {
		t.Error("an import with no database was accepted")
	}
}

func TestTheImportRefusesAnIntoThatIsNotAPath(t *testing.T) {
	if _, err := nextcloud.Import(t.Context(), fullBucket(t), &recorder{}, "edu",
		nextcloud.ImportOptions{
			Options: nextcloud.Options{Source: instance(t)},
			Into:    "../elsewhere",
		}); err == nil {
		t.Error("--into escaped the tree and was accepted")
	}
}

func TestTheImportReportSaysWhatItDidAndWhatItDidNot(t *testing.T) {
	report := importInto(t, &recorder{}, fullBucket(t), nextcloud.ImportOptions{})

	var out strings.Builder
	if err := report.Render(&out); err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, want := range []string{"Not a byte was moved", "carry no ETag", "rows written"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the report does not say %q:\n%s", want, out.String())
		}
	}
}

// A failing destination has to stop the import rather than carry on reporting
// numbers nothing wrote.
type refusing struct{}

func (refusing) Adopt(context.Context, string, []db.File) (int, int, error) {
	return 0, 0, errors.New("the database said no")
}

func TestTheImportStopsWhenTheRowsCannotBeWritten(t *testing.T) {
	_, err := nextcloud.Import(t.Context(), fullBucket(t), refusing{}, "edu",
		nextcloud.ImportOptions{Options: nextcloud.Options{Source: instance(t)}})
	if err == nil {
		t.Fatal("an import whose rows were refused reported success")
	}
	if !strings.Contains(err.Error(), "adopt") {
		t.Errorf("err = %v, want it to say which half failed", err)
	}
}

// --etag reads the library, and a bucket that lost an object between the survey
// and the read has to be an error rather than a row with no validator.
func TestHashingAnObjectThatIsNotThereFails(t *testing.T) {
	store := bucket(t, map[string]int{"urn:oid:103": 1000, "urn:oid:104": 2000})

	_, err := nextcloud.Import(t.Context(), store, &recorder{}, "edu", nextcloud.ImportOptions{
		Options: nextcloud.Options{Source: instance(t)},
		ETag:    true,
	})
	if err == nil {
		t.Fatal("hashing an object that is not there was reported as a success")
	}
}
