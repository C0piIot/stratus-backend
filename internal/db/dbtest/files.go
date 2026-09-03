package dbtest

import (
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
)

const owner = "edu"

// RunFiles executes the file cases against the repository built by newFiles.
//
// It takes a db.Files rather than a db.Store on purpose: these cases have no
// business reaching Migrate or Tx, and a driver's file repository can be
// exercised without one. Every feature added to the port gets a Run of its own
// beside this, and [Run] calls all of them.
func RunFiles(t *testing.T, newFiles func(t *testing.T) db.Files) {
	t.Helper()

	cases := []struct {
		name string
		fn   func(t *testing.T, s db.Files)
	}{
		{"put and get round trip", putGetRoundTrip},
		{"every column of a file survives a round trip", fileEveryColumn},
		{"put replaces the file at that path", putReplaces},
		{"a missing file is ErrNotFound", missingFile},
		{"delete removes exactly once", deleteOnce},
		{"list returns direct children only", listDirectChildren},
		{"list is ordered by path", listOrdered},
		{"list of an empty directory is empty", listEmpty},
		{"move renames", moveRenames},
		{"move onto an occupied path conflicts", moveConflict},
		{"move of a missing file is ErrNotFound", moveMissing},
		{"owners do not see each other", ownersAreSeparate},
		{"paths are opaque unicode", unicodePaths},
		{"invalid paths are rejected", invalidPaths},
		{"a size larger than 32 bits survives", largeFiles},
		{"mtime survives the round trip", timeRoundTrip},
		{"directories are rows of their own", emptyDirectories},
		{"a directory listing holds both kinds", listMixed},
		{"nothing may be created twice", createDirConflicts},
		{"a file may not replace a directory", fileOverDirectory},
		{"a directory in use is not deleted", deleteBusyDirectory},
		{"a directory in use is not moved", moveBusyDirectory},
		{"every blob key is reachable", blobKeys},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.fn(t, newFiles(t))
		})
	}
}

func putGetRoundTrip(t *testing.T, s db.Files) {
	want := file("photos/2024/06/IMG_0001.jpg")
	stored := put(t, s, want)

	if stored.ID == 0 {
		t.Error("PutFile returned no ID")
	}

	got, err := s.FileByPath(t.Context(), owner, want.Path)
	if err != nil {
		t.Fatalf("FileByPath: %v", err)
	}
	if got.ID != stored.ID {
		t.Errorf("ID = %d, want %d", got.ID, stored.ID)
	}
	if got.Path != want.Path || got.BlobKey != want.BlobKey || got.Size != want.Size {
		t.Errorf("got %+v, want the file that was written", got)
	}
	if got.ETag != want.ETag || got.MIMEType != want.MIMEType || got.OwnerID != owner {
		t.Errorf("got %+v, want the file that was written", got)
	}
}

func putReplaces(t *testing.T, s db.Files) {
	first := put(t, s, file("notes.txt"))

	second := file("notes.txt")
	second.Size = 999
	second.BlobKey = "a-different-blob"
	replaced := put(t, s, second)

	// The row is updated, not duplicated: the identity of a path is the path.
	if replaced.ID != first.ID {
		t.Errorf("ID = %d, want the original %d: the row was replaced, not updated", replaced.ID, first.ID)
	}
	got, err := s.FileByPath(t.Context(), owner, "notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got.Size != 999 || got.BlobKey != "a-different-blob" {
		t.Errorf("got %+v, want the second write", got)
	}
	if list := paths(t, s, ""); len(list) != 1 {
		t.Errorf("root holds %v, want one file", list)
	}
}

func missingFile(t *testing.T, s db.Files) {
	if _, err := s.FileByPath(t.Context(), owner, "nothing/here.txt"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("FileByPath = %v, want ErrNotFound", err)
	}
}

func deleteOnce(t *testing.T, s db.Files) {
	put(t, s, file("doomed.txt"))

	if err := s.DeleteFile(t.Context(), owner, "doomed.txt"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	if _, err := s.FileByPath(t.Context(), owner, "doomed.txt"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("FileByPath after delete = %v, want ErrNotFound", err)
	}
	// Unlike a blob delete, this one is not idempotent: the caller asked about
	// a row it believed existed.
	if err := s.DeleteFile(t.Context(), owner, "doomed.txt"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("second DeleteFile = %v, want ErrNotFound", err)
	}
}

func listDirectChildren(t *testing.T, s db.Files) {
	for _, p := range []string{"a.txt", "dir/b.txt", "dir/c.txt", "dir/deeper/d.txt", "other/e.txt"} {
		put(t, s, file(p))
	}

	if got := paths(t, s, "dir"); !slices.Equal(got, []string{"dir/b.txt", "dir/c.txt"}) {
		t.Errorf("children of dir = %v, want only its direct ones", got)
	}
	if got := paths(t, s, ""); !slices.Equal(got, []string{"a.txt"}) {
		t.Errorf("root = %v, want only the file at the top level", got)
	}
	if got := paths(t, s, "dir/deeper"); !slices.Equal(got, []string{"dir/deeper/d.txt"}) {
		t.Errorf("children of dir/deeper = %v", got)
	}
}

func listOrdered(t *testing.T, s db.Files) {
	for _, p := range []string{"dir/c", "dir/a", "dir/b"} {
		put(t, s, file(p))
	}
	// A listing that changes order between calls is useless to a sync client,
	// so the port promises path order and this pins it.
	if got := paths(t, s, "dir"); !slices.Equal(got, []string{"dir/a", "dir/b", "dir/c"}) {
		t.Errorf("listing = %v, want it sorted by path", got)
	}
}

func listEmpty(t *testing.T, s db.Files) {
	put(t, s, file("dir/only.txt"))

	if got := paths(t, s, "dir/empty"); len(got) != 0 {
		t.Errorf("listing a directory with nothing in it = %v, want nothing", got)
	}
}

func moveRenames(t *testing.T, s db.Files) {
	before := put(t, s, file("inbox/photo.jpg"))

	if err := s.MoveFile(t.Context(), owner, "inbox/photo.jpg", "albums/2024/photo.jpg"); err != nil {
		t.Fatalf("MoveFile: %v", err)
	}

	if _, err := s.FileByPath(t.Context(), owner, "inbox/photo.jpg"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("the old path still resolves: %v", err)
	}
	after, err := s.FileByPath(t.Context(), owner, "albums/2024/photo.jpg")
	if err != nil {
		t.Fatalf("the new path does not resolve: %v", err)
	}
	if after.ID != before.ID {
		t.Errorf("ID = %d, want the same row %d: a move is not a copy", after.ID, before.ID)
	}
	if after.BlobKey != before.BlobKey {
		t.Errorf("BlobKey = %q, want it untouched: moving a file must not move a blob", after.BlobKey)
	}
	// The parent has to follow the path, or the file disappears from listings.
	if got := paths(t, s, "albums/2024"); !slices.Equal(got, []string{"albums/2024/photo.jpg"}) {
		t.Errorf("listing the new parent = %v, want the moved file", got)
	}
}

func moveConflict(t *testing.T, s db.Files) {
	put(t, s, file("a.txt"))
	victim := put(t, s, file("b.txt"))

	err := s.MoveFile(t.Context(), owner, "a.txt", "b.txt")
	if !errors.Is(err, db.ErrConflict) {
		t.Fatalf("MoveFile onto an occupied path = %v, want ErrConflict", err)
	}
	// And it must not have clobbered the file that was already there.
	got, err := s.FileByPath(t.Context(), owner, "b.txt")
	if err != nil {
		t.Fatalf("the target is gone after a failed move: %v", err)
	}
	if got.ID != victim.ID {
		t.Errorf("the target was overwritten by the failed move")
	}
	if _, err := s.FileByPath(t.Context(), owner, "a.txt"); err != nil {
		t.Errorf("the source is gone after a failed move: %v", err)
	}
}

func moveMissing(t *testing.T, s db.Files) {
	if err := s.MoveFile(t.Context(), owner, "ghost.txt", "elsewhere.txt"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("MoveFile = %v, want ErrNotFound", err)
	}
}

func ownersAreSeparate(t *testing.T, s db.Files) {
	const other = "someone-else"
	mine := file("shared-name.txt")
	theirs := file("shared-name.txt")
	theirs.OwnerID = other
	theirs.BlobKey = "their-blob"

	put(t, s, mine)
	if _, err := s.PutFile(t.Context(), theirs); err != nil {
		t.Fatalf("the same path under another owner: %v", err)
	}

	got, err := s.FileByPath(t.Context(), other, "shared-name.txt")
	if err != nil {
		t.Fatalf("FileByPath for the other owner: %v", err)
	}
	if got.BlobKey != "their-blob" {
		t.Errorf("BlobKey = %q, want the other owner's", got.BlobKey)
	}
	if got := paths(t, s, ""); !slices.Equal(got, []string{"shared-name.txt"}) {
		t.Errorf("listing = %v, want only this owner's file", got)
	}
}

func unicodePaths(t *testing.T, s db.Files) {
	const path = "fotos/verano/ñandú en la playa 🌞.jpg"
	put(t, s, file(path))

	if _, err := s.FileByPath(t.Context(), owner, path); err != nil {
		t.Errorf("FileByPath: %v", err)
	}
	if got := paths(t, s, "fotos/verano"); !slices.Equal(got, []string{path}) {
		t.Errorf("listing = %v", got)
	}
}

func invalidPaths(t *testing.T, s db.Files) {
	bad := []string{
		"",
		"/leading",
		"trailing/",
		"double//slash",
		"dot/./segment",
		"dot/../segment",
		"..",
		"nul\x00byte",
		strings.Repeat("x", db.MaxPathLen+1),
	}
	for _, path := range bad {
		// Quoted and clipped: one of these is four kilobytes long and another
		// contains a NUL, and neither reads well as a test name.
		name := strconv.Quote(path)
		t.Run(name[:min(len(name), 32)], func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()

			f := file(path)
			if _, err := s.PutFile(ctx, f); !errors.Is(err, db.ErrInvalidPath) {
				t.Errorf("PutFile = %v, want ErrInvalidPath", err)
			}
			if _, err := s.FileByPath(ctx, owner, path); !errors.Is(err, db.ErrInvalidPath) {
				t.Errorf("FileByPath = %v, want ErrInvalidPath", err)
			}
			if err := s.DeleteFile(ctx, owner, path); !errors.Is(err, db.ErrInvalidPath) {
				t.Errorf("DeleteFile = %v, want ErrInvalidPath", err)
			}
			if err := s.MoveFile(ctx, owner, "somewhere.txt", path); !errors.Is(err, db.ErrInvalidPath) {
				t.Errorf("MoveFile to it = %v, want ErrInvalidPath", err)
			}
		})
	}
}

// largeFiles is a regression test with a name: PostgreSQL's INTEGER is 32 bits,
// so a column declared with it silently caps a library at 2 GB per file.
func largeFiles(t *testing.T, s db.Files) {
	const size = 8 << 30 // 8 GiB, an ordinary video

	f := file("videos/holiday.mkv")
	f.Size = size
	put(t, s, f)

	got, err := s.FileByPath(t.Context(), owner, f.Path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Size != size {
		t.Errorf("Size = %d, want %d", got.Size, int64(size))
	}
}

func timeRoundTrip(t *testing.T, s db.Files) {
	// Deliberately awkward: a non-UTC zone and sub-millisecond precision, which
	// is what an upload from a phone actually carries.
	zone := time.FixedZone("CEST", 2*60*60)
	when := time.Date(2024, 6, 1, 12, 30, 15, 123_456_789, zone)

	f := file("clock.txt")
	f.MTime = when
	put(t, s, f)

	got, err := s.FileByPath(t.Context(), owner, f.Path)
	if err != nil {
		t.Fatal(err)
	}
	want := when.UTC().Truncate(db.TimePrecision)
	if !got.MTime.Equal(want) {
		t.Errorf("MTime = %v, want %v", got.MTime, want)
	}
}

// fileEveryColumn is the generalisation of largeFiles and timeRoundTrip: rather
// than pinning the one column somebody was burned by, it writes every column
// and reads every column, and assertEveryFieldSet makes it a failure to add a
// field to db.File that the fixture leaves empty.
//
// Two fields are exempt, and neither is an oversight. ID is assigned by the
// database. IsDir cannot be true on a row that also carries a blob, a size and
// an ETag -- a row is a file or a collection, never both -- so it is covered by
// emptyDirectories and listMixed instead.
//
// One column no struct comparison can reach: files.parent_path has no field on
// db.File. It stays covered indirectly, by the listing cases, because
// ListFiles selects on it.
func fileEveryColumn(t *testing.T, s db.Files) {
	want := file("photos/every-column.jpg").Normalize()
	assertEveryFieldSet(t, want, "ID", "IsDir")

	want.ID = put(t, s, want).ID

	got, err := s.FileByPath(t.Context(), owner, want.Path)
	if err != nil {
		t.Fatalf("FileByPath: %v", err)
	}
	compareFields(t, got, want)
}

// assertEveryFieldSet names any exported field the fixture leaves at its zero
// value, which is what replaces remembering the rule. A column added to a
// migration arrives with a field on the entity; a field nothing writes cannot
// be covered by a round trip, and disc_no sat in the schema unwritten by any
// case until this existed.
func assertEveryFieldSet(t *testing.T, v any, exempt ...string) {
	t.Helper()

	rv := reflect.ValueOf(v)
	for i := range rv.NumField() {
		field := rv.Type().Field(i)
		if !field.IsExported() || slices.Contains(exempt, field.Name) {
			continue
		}
		if rv.Field(i).IsZero() {
			t.Errorf("%T.%s is unset in the fixture: a round trip cannot cover a column nothing writes",
				v, field.Name)
		}
	}
}

// compareFields names every exported field that differs. Reflection rather than
// a list of comparisons so that a field added to the entity is compared without
// anyone editing the case -- which is the whole point of the two cases that use
// it.
//
// It knows two things == gets wrong here: a time.Time compares equal instants
// rather than wall clock and location, and a pointer field like db.Media.GPS
// compares what it points at rather than where it points.
func compareFields(t *testing.T, got, want any) {
	t.Helper()

	g, w := reflect.ValueOf(got), reflect.ValueOf(want)
	for i := range g.NumField() {
		field := g.Type().Field(i)
		if !field.IsExported() {
			continue
		}
		if !equalField(g.Field(i), w.Field(i)) {
			t.Errorf("%s = %v, want %v", field.Name, g.Field(i), w.Field(i))
		}
	}
}

func equalField(got, want reflect.Value) bool {
	if when, ok := got.Interface().(time.Time); ok {
		return when.Equal(want.Interface().(time.Time))
	}
	if got.Kind() == reflect.Pointer {
		if got.IsNil() || want.IsNil() {
			return got.IsNil() == want.IsNil()
		}
		return got.Elem().Interface() == want.Elem().Interface()
	}
	return got.Interface() == want.Interface()
}

// file builds a plausible row, so each case only states what it cares about.
func file(path string) db.File {
	return db.File{
		OwnerID:  owner,
		Path:     path,
		BlobKey:  "blobs/" + strings.ReplaceAll(path, "/", "-"),
		Size:     1234,
		MTime:    time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC),
		ETag:     `"abc123"`,
		MIMEType: "application/octet-stream",
	}
}

func put(t *testing.T, s db.Files, f db.File) db.File {
	t.Helper()
	stored, err := s.PutFile(t.Context(), f)
	if err != nil {
		t.Fatalf("PutFile(%q): %v", f.Path, err)
	}
	return stored
}

func paths(t *testing.T, s db.Files, dir string) []string {
	t.Helper()
	files, err := s.ListFiles(t.Context(), owner, dir)
	if err != nil {
		t.Fatalf("ListFiles(%q): %v", dir, err)
	}
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Path)
	}
	return out
}

// emptyDirectories is the point of the whole feature: a collection with nothing
// in it has to exist, because MKCOL creates one and PROPFIND has to find it.
func emptyDirectories(t *testing.T, s db.Files) {
	dir, err := s.CreateDir(t.Context(), owner, "photos")
	if err != nil {
		t.Fatalf("CreateDir: %v", err)
	}
	if !dir.IsDir {
		t.Error("CreateDir returned something that is not a directory")
	}
	if dir.ID == 0 {
		t.Error("CreateDir returned no ID")
	}

	got, err := s.FileByPath(t.Context(), owner, "photos")
	if err != nil {
		t.Fatalf("FileByPath on a directory: %v", err)
	}
	if !got.IsDir {
		t.Error("the row came back without IsDir")
	}
	// A directory carries no blob, and inventing one for it would be a lie the
	// storage seam would then have to keep.
	if got.BlobKey != "" || got.Size != 0 {
		t.Errorf("got %+v, want a row with no blob", got)
	}
	if list := paths(t, s, ""); !slices.Equal(list, []string{"photos"}) {
		t.Errorf("root = %v, want the empty directory to be listed", list)
	}
}

func listMixed(t *testing.T, s db.Files) {
	if _, err := s.CreateDir(t.Context(), owner, "album"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateDir(t.Context(), owner, "album/raw"); err != nil {
		t.Fatal(err)
	}
	put(t, s, file("album/one.jpg"))

	got, err := s.ListFiles(t.Context(), owner, "album")
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("listing = %+v, want the file and the subdirectory", got)
	}
	// PROPFIND wants both kinds in one listing, told apart by a flag rather
	// than by two queries.
	if got[0].Path != "album/one.jpg" || got[0].IsDir {
		t.Errorf("first entry = %+v, want the file", got[0])
	}
	if got[1].Path != "album/raw" || !got[1].IsDir {
		t.Errorf("second entry = %+v, want the directory", got[1])
	}
}

func createDirConflicts(t *testing.T, s db.Files) {
	if _, err := s.CreateDir(t.Context(), owner, "photos"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateDir(t.Context(), owner, "photos"); !errors.Is(err, db.ErrConflict) {
		t.Errorf("CreateDir twice = %v, want ErrConflict", err)
	}

	put(t, s, file("notes.txt"))
	if _, err := s.CreateDir(t.Context(), owner, "notes.txt"); !errors.Is(err, db.ErrConflict) {
		t.Errorf("CreateDir over a file = %v, want ErrConflict", err)
	}
}

// fileOverDirectory is the upsert's dangerous edge: replacing a collection with
// a file would orphan everything under it.
func fileOverDirectory(t *testing.T, s db.Files) {
	if _, err := s.CreateDir(t.Context(), owner, "album"); err != nil {
		t.Fatal(err)
	}
	put(t, s, file("album/photo.jpg"))

	if _, err := s.PutFile(t.Context(), file("album")); !errors.Is(err, db.ErrConflict) {
		t.Fatalf("PutFile over a directory = %v, want ErrConflict", err)
	}
	// And the contents are still reachable.
	if list := paths(t, s, "album"); !slices.Equal(list, []string{"album/photo.jpg"}) {
		t.Errorf("album holds %v, want its file untouched", list)
	}
}

func deleteBusyDirectory(t *testing.T, s db.Files) {
	if _, err := s.CreateDir(t.Context(), owner, "album"); err != nil {
		t.Fatal(err)
	}
	put(t, s, file("album/photo.jpg"))

	if err := s.DeleteFile(t.Context(), owner, "album"); !errors.Is(err, db.ErrConflict) {
		t.Fatalf("deleting a directory with a file in it = %v, want ErrConflict", err)
	}

	// Empty it and the directory goes.
	if err := s.DeleteFile(t.Context(), owner, "album/photo.jpg"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteFile(t.Context(), owner, "album"); err != nil {
		t.Errorf("deleting an emptied directory = %v, want nil", err)
	}
}

func moveBusyDirectory(t *testing.T, s db.Files) {
	if _, err := s.CreateDir(t.Context(), owner, "inbox"); err != nil {
		t.Fatal(err)
	}
	put(t, s, file("inbox/photo.jpg"))

	// Renaming it would leave the file pointing at a parent that no longer
	// exists. Rewriting the subtree is the protocol adapter's job, later.
	if err := s.MoveFile(t.Context(), owner, "inbox", "archive"); !errors.Is(err, db.ErrConflict) {
		t.Fatalf("moving a directory with a file in it = %v, want ErrConflict", err)
	}
	if _, err := s.FileByPath(t.Context(), owner, "inbox/photo.jpg"); err != nil {
		t.Errorf("the file was disturbed by the refused move: %v", err)
	}

	// An empty one moves like anything else.
	if _, err := s.CreateDir(t.Context(), owner, "empty"); err != nil {
		t.Fatal(err)
	}
	if err := s.MoveFile(t.Context(), owner, "empty", "renamed"); err != nil {
		t.Errorf("moving an empty directory = %v, want nil", err)
	}
}

// blobKeys is what the orphan collector subtracts from the blob store, so a
// driver that misses one would have it delete a live file.
func blobKeys(t *testing.T, s db.Files) {
	if _, err := s.CreateDir(t.Context(), owner, "album"); err != nil {
		t.Fatal(err)
	}
	one := put(t, s, file("album/one.jpg"))
	two := put(t, s, file("two.txt"))

	// Another owner's rows count too: a collector that only saw one owner's
	// would delete the other's blobs.
	theirs := file("theirs.txt")
	theirs.OwnerID = "someone-else"
	theirs.BlobKey = "blobs/theirs"
	if _, err := s.PutFile(t.Context(), theirs); err != nil {
		t.Fatal(err)
	}

	got := map[string]int{}
	for key, err := range s.BlobKeys(t.Context()) {
		if err != nil {
			t.Fatalf("BlobKeys: %v", err)
		}
		got[key]++
	}

	for _, want := range []string{one.BlobKey, two.BlobKey, theirs.BlobKey} {
		if got[want] != 1 {
			t.Errorf("key %q appears %d times, want once", want, got[want])
		}
	}
	// A directory has no blob, so it must not contribute an empty key: the
	// collector would then treat "" as referenced and never notice.
	if _, ok := got[""]; ok {
		t.Error("a directory contributed an empty blob key")
	}
	if len(got) != 3 {
		t.Errorf("BlobKeys returned %d keys, want 3", len(got))
	}

	// The collector may stop early -- on an error, or because its context went
	// away -- and abandoning the iteration must not leak the open rows.
	for range s.BlobKeys(t.Context()) {
		break
	}
	// Still usable afterwards, which is what proves the rows were closed.
	if _, err := s.FileByPath(t.Context(), owner, "two.txt"); err != nil {
		t.Errorf("the repository is unusable after an abandoned iteration: %v", err)
	}

	// An overwrite replaces the row, so the old key stops being referenced --
	// which is exactly what makes its blob collectable.
	replaced := file("two.txt")
	replaced.BlobKey = "blobs/replacement"
	put(t, s, replaced)

	got = map[string]int{}
	for key, err := range s.BlobKeys(t.Context()) {
		if err != nil {
			t.Fatalf("BlobKeys: %v", err)
		}
		got[key]++
	}
	if _, stale := got[two.BlobKey]; stale {
		t.Error("the replaced blob key is still referenced")
	}
	if got["blobs/replacement"] != 1 {
		t.Error("the new blob key is not referenced")
	}
}
