package dbtest

import (
	"slices"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// RunTrash executes the trash cases against the repository built by newRepo.
//
// What these pin is a table that exists so that deleting is survivable (#274):
// the row as it was, kept whole enough to be put back, grouped by the deletion
// it arrived in, and reachable by the sweep so that nothing else throws its
// bytes away.
func RunTrash(t *testing.T, newRepo func(t *testing.T) db.Repo) {
	t.Helper()

	cases := []struct {
		name string
		fn   func(t *testing.T, s db.Repo)
	}{
		{"a trashed row survives a round trip", trashRoundTrip},
		{"two deletions of one path do not collide", trashSamePath},
		{"a deletion is one entry, named by its root", trashBatches},
		{"deletions come back newest first, a page at a time", trashPaged},
		{"the trash belongs to its owner", trashOwner},
		{"a row with no owner is only seen by asking for none", trashNoOwner},
		{"its keys are what keeps the sweep off them", trashKeys},
		{"expired deletions come back oldest first", trashExpiry},
		{"deleting one is idempotent", trashDelete},
		{"the totals are files and bytes, not rows", trashTotals},
		{"a page of no rows is refused", trashLimit},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.fn(t, newRepo(t))
		})
	}
}

// deleted is a plausible row on its way out, with every field set to something
// distinguishable: a column dropped on the way in or out is the bug these
// cases are for.
func deleted(path string) db.File {
	return db.File{
		OwnerID:  owner,
		Path:     path,
		BlobKey:  "image/2026/10/03/" + path,
		Size:     4096,
		MTime:    time.Now().Add(-time.Hour).UTC().Truncate(db.TimePrecision),
		ETag:     `"abc123"`,
		MIMEType: "image/jpeg",
	}
}

// trashed puts rows in the trash and gives back the batch they share.
func trashed(t *testing.T, s db.Repo, at time.Time, rows ...db.File) string {
	t.Helper()
	batch := "batch-" + at.UTC().Format("150405.000000000")
	if err := s.Trash(t.Context(), batch, rows, at); err != nil {
		t.Fatalf("Trash: %v", err)
	}
	return batch
}

func trashRoundTrip(t *testing.T, s db.Repo) {
	want := deleted("holiday/sunset.jpg")
	at := time.Now().UTC().Truncate(db.TimePrecision)
	batch := trashed(t, s, at, want)

	rows, err := s.TrashedIn(t.Context(), owner, batch)
	if err != nil {
		t.Fatalf("TrashedIn: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("TrashedIn = %d rows, want one", len(rows))
	}
	got := rows[0]
	if got.Batch != batch || !got.DeletedAt.Equal(at) {
		t.Errorf("batch %q at %s, want %q at %s", got.Batch, got.DeletedAt, batch, at)
	}
	// The row as it was, which is what makes putting it back an insert.
	if got.Path != want.Path || got.BlobKey != want.BlobKey || got.Size != want.Size ||
		got.ETag != want.ETag || got.MIMEType != want.MIMEType || got.IsDir {
		t.Errorf("the row came back as %+v, want %+v", got.File, want)
	}
	if !got.MTime.Equal(want.MTime) {
		t.Errorf("MTime = %s, want %s", got.MTime, want.MTime)
	}
}

// trashSamePath is the reason this is a table and not a flag on files: the
// live tree has a unique index on the path, and the trash must not.
func trashSamePath(t *testing.T, s db.Repo) {
	now := time.Now().UTC().Truncate(db.TimePrecision)
	first := trashed(t, s, now.Add(-time.Hour), deleted("notes.txt"))
	second := trashed(t, s, now, deleted("notes.txt"))

	if first == second {
		t.Fatal("the fixture reused a batch, so this case is testing nothing")
	}
	for _, batch := range []string{first, second} {
		rows, err := s.TrashedIn(t.Context(), owner, batch)
		if err != nil || len(rows) != 1 {
			t.Errorf("TrashedIn(%q) = %d rows, %v; want one", batch, len(rows), err)
		}
	}
}

// trashBatches: a folder of photographs is many rows and one accident.
func trashBatches(t *testing.T, s db.Repo) {
	album := db.File{OwnerID: owner, Path: "album", IsDir: true}
	one, two := deleted("album/one.jpg"), deleted("album/two.jpg")
	one.Size, two.Size = 10, 20
	batch := trashed(t, s, time.Now().UTC(), album, one, two)

	got, err := s.TrashBatches(t.Context(), owner, db.TrashCursor{}, 50)
	if err != nil {
		t.Fatalf("TrashBatches: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("TrashBatches = %d deletions, want one", len(got))
	}
	// Named by the folder that was deleted, which is the shortest path in it,
	// and counted in files rather than rows -- the directory is in there too.
	if b := got[0]; b.ID != batch || b.Root != "album" || b.Files != 2 || b.Bytes != 30 {
		t.Errorf("the deletion is %+v, want album, two files, thirty bytes", b)
	}
}

func trashPaged(t *testing.T, s db.Repo) {
	now := time.Now().UTC().Truncate(db.TimePrecision)
	var want []string
	for i := range 5 {
		batch := trashed(t, s, now.Add(-time.Duration(i)*time.Minute),
			deleted("gone-"+string(rune('a'+i))+".txt"))
		want = append([]string{batch}, want...) // newest first, which is i = 0
	}
	want = slices.Clip(want)
	slices.Reverse(want)

	var walked []string
	after := db.TrashCursor{}
	for range 4 {
		page, err := s.TrashBatches(t.Context(), owner, after, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) > 2 {
			t.Fatalf("a page of 2 came back with %d", len(page))
		}
		if len(page) == 0 {
			break
		}
		for _, b := range page {
			walked = append(walked, b.ID)
		}
		last := page[len(page)-1]
		after = db.TrashCursor{DeletedAt: last.DeletedAt, Batch: last.ID}
	}
	if !slices.Equal(walked, want) {
		t.Errorf("walked %v, want %v", walked, want)
	}
}

func trashOwner(t *testing.T, s db.Repo) {
	mine := trashed(t, s, time.Now().UTC(), deleted("mine.txt"))
	theirs := deleted("theirs.txt")
	theirs.OwnerID = "someone-else"
	if err := s.Trash(t.Context(), "their-batch", []db.File{theirs}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	got, err := s.TrashBatches(t.Context(), owner, db.TrashCursor{}, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != mine {
		t.Errorf("the trash shows %+v, want only this owner's deletion", got)
	}
	if rows, _ := s.TrashedIn(t.Context(), owner, "their-batch"); len(rows) != 0 {
		t.Errorf("another owner's deletion was readable: %+v", rows)
	}
	if err := s.DeleteTrash(t.Context(), owner, "their-batch"); err != nil {
		t.Fatal(err)
	}
	if rows, _ := s.TrashedIn(t.Context(), "someone-else", "their-batch"); len(rows) != 1 {
		t.Error("one owner destroyed another's deletion")
	}
}

// trashNoOwner holds up the other half of #276: a blob the sweep could not
// account for is filed under no owner, because nobody can say whose it was,
// and the owner is the filter -- so a person's page cannot show one by
// accident and asking for none shows nothing else.
func trashNoOwner(t *testing.T, s db.Repo) {
	mine := trashed(t, s, time.Now().UTC(), deleted("mine.txt"))
	nobodys := db.File{BlobKey: "image/2026/10/03/NOROWHOLDSTHIS", Size: 7}
	if err := s.Trash(t.Context(), "swept", []db.File{nobodys}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	got, err := s.TrashBatches(t.Context(), "", db.TrashCursor{}, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "swept" {
		t.Fatalf("asking for no owner answered %+v, want the swept batch alone", got)
	}
	// It has no path, so the batch has no root to be named by: that is the
	// page's job, not the database's.
	if got[0].Root != "" || got[0].Files != 1 || got[0].Bytes != 7 {
		t.Errorf("the swept batch is %+v, want one unnamed object of seven bytes", got[0])
	}

	mineOnly, err := s.TrashBatches(t.Context(), owner, db.TrashCursor{}, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(mineOnly) != 1 || mineOnly[0].ID != mine {
		t.Errorf("a person's trash is %+v, want their deletion alone", mineOnly)
	}

	switch totals, terr := s.TrashTotals(t.Context(), ""); {
	case terr != nil:
		t.Fatal(terr)
	case totals.Files != 1 || totals.Bytes != 7:
		t.Errorf("the unowned totals are %+v, want one file of seven bytes", totals)
	}
}

// trashKeys is what the sweep adds to what it considers referenced, so it is
// every owner's and it leaves the directories out: they have no bytes.
func trashKeys(t *testing.T, s db.Repo) {
	trashed(t, s, time.Now().UTC(),
		db.File{OwnerID: owner, Path: "album", IsDir: true},
		deleted("album/one.jpg"))
	theirs := deleted("theirs.jpg")
	theirs.OwnerID = "someone-else"
	if err := s.Trash(t.Context(), "their-batch", []db.File{theirs}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	var keys []string
	for key, err := range s.TrashKeys(t.Context()) {
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, key)
	}
	slices.Sort(keys)
	want := []string{"image/2026/10/03/album/one.jpg", "image/2026/10/03/theirs.jpg"}
	if !slices.Equal(keys, want) {
		t.Errorf("TrashKeys = %v, want both owners' files and no directory", keys)
	}
}

func trashExpiry(t *testing.T, s db.Repo) {
	now := time.Now().UTC().Truncate(db.TimePrecision)
	old := trashed(t, s, now.Add(-2*time.Hour), deleted("old.txt"))
	trashed(t, s, now, deleted("new.txt"))

	var got []string
	for row, err := range s.ExpiredTrash(t.Context(), now.Add(-time.Hour)) {
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, row.Batch)
	}
	if !slices.Equal(got, []string{old}) {
		t.Errorf("ExpiredTrash = %v, want the old one alone", got)
	}
}

func trashDelete(t *testing.T, s db.Repo) {
	batch := trashed(t, s, time.Now().UTC(), deleted("gone.txt"))

	for range 2 {
		if err := s.DeleteTrash(t.Context(), owner, batch); err != nil {
			t.Errorf("DeleteTrash: %v", err)
		}
	}
	if rows, _ := s.TrashedIn(t.Context(), owner, batch); len(rows) != 0 {
		t.Errorf("the deletion survived: %+v", rows)
	}
	// And a batch nobody ever made is not an error either.
	if err := s.DeleteTrash(t.Context(), owner, "never-existed"); err != nil {
		t.Errorf("DeleteTrash of nothing = %v", err)
	}
}

func trashTotals(t *testing.T, s db.Repo) {
	one, two := deleted("one.jpg"), deleted("two.jpg")
	one.Size, two.Size = 100, 200
	trashed(t, s, time.Now().UTC(), db.File{OwnerID: owner, Path: "album", IsDir: true}, one)
	trashed(t, s, time.Now().UTC(), two)

	got, err := s.TrashTotals(t.Context(), owner)
	if err != nil {
		t.Fatalf("TrashTotals: %v", err)
	}
	if got.Files != 2 || got.Bytes != 300 {
		t.Errorf("TrashTotals = %+v, want two files and three hundred bytes", got)
	}
	// An empty trash is zero and not an error, which is what a status page
	// asks for on a server nobody has deleted anything on.
	if empty, terr := s.TrashTotals(t.Context(), "someone-else"); terr != nil || empty.Files != 0 {
		t.Errorf("an empty trash = %+v, %v", empty, terr)
	}
}

func trashLimit(t *testing.T, s db.Repo) {
	if _, err := s.TrashBatches(t.Context(), owner, db.TrashCursor{}, 0); err == nil {
		t.Error("a page of no deletions was allowed")
	}
}
