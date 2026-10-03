package dbtest

import (
	"fmt"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// RunFind executes the search cases against the repository built by newRepo.
//
// It takes a db.Repo because a search answers across both halves of the
// library: a file is a row and a track is a row plus its tags.
//
// **Every word these cases search for is four letters or more, and none of them
// is a common English word.** The floor is MySQL's: a FULLTEXT index does not
// hold a token shorter than innodb_ft_min_token_size, three by default, nor a
// word on its stopword list. A case that searched for "the" would pass on two
// drivers and fail on the third for a reason nobody would find in an afternoon.
func RunFind(t *testing.T, newRepo func(t *testing.T) db.Repo) {
	t.Helper()

	cases := []struct {
		name string
		fn   func(t *testing.T, s db.Repo)
	}{
		{"a word of a name finds the file", findName},
		{"a word of a folder's name finds the folder and not what is in it", findFolder},
		{"the separators in a name are word boundaries", findSeparators},
		{"case does not matter", findCase},
		{"several words are a phrase", findPhrase},
		{"a tag finds its track", findTags},
		{"a camera and a year find a photograph", findPhotos},
		{"a photograph with nothing recorded is not found by nothing", findPhotosBlank},
		{"a search sees one owner", findOwner},
		{"an empty term finds nothing", findEmpty},
		{"a half nobody asked for is not answered", findUnwanted},
		{"a search is walked a page at a time", findPaged},
		{"a cursor still resumes after its row is deleted", findPagedDeletedCursor},
		{"a page of no rows is refused", findLimit},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.fn(t, newRepo(t))
		})
	}
}

// found is the paths a search answers, in the order it answered them.
func found(t *testing.T, s db.Repo, text string) []string {
	t.Helper()
	return foundIn(t, s, db.FindFilter{Text: text, Files: db.Window{Limit: 50}})
}

func foundIn(t *testing.T, s db.Repo, f db.FindFilter) []string {
	t.Helper()
	result, err := s.Find(t.Context(), owner, f)
	if err != nil {
		t.Fatalf("Find(%+v): %v", f, err)
	}
	out := make([]string, 0, len(result.Files)+len(result.Tracks)+len(result.Photos))
	for _, file := range result.Files {
		out = append(out, file.Path)
	}
	for _, track := range result.Tracks {
		out = append(out, track.File.Path)
	}
	for _, p := range result.Photos {
		out = append(out, p.Path)
	}
	return out
}

func findName(t *testing.T, s db.Repo) {
	put(t, s, file("holiday/sunset.jpg"))
	put(t, s, file("holiday/beach.jpg"))

	if got := found(t, s, "sunset"); !slices.Equal(got, []string{"holiday/sunset.jpg"}) {
		t.Errorf("sunset = %v", got)
	}
	if got := found(t, s, "nothinghere"); len(got) != 0 {
		t.Errorf("a word nothing is called = %v, want nothing", got)
	}
}

// findFolder is the shape of the whole feature: the name and not the path. A
// word in a folder finds that folder, and the thousand photographs under it
// stay out of the way.
func findFolder(t *testing.T, s db.Repo) {
	if _, err := s.CreateDir(t.Context(), owner, "holiday"); err != nil {
		t.Fatal(err)
	}
	put(t, s, file("holiday/sunset.jpg"))

	if got := found(t, s, "holiday"); !slices.Equal(got, []string{"holiday"}) {
		t.Errorf("holiday = %v, want the folder alone", got)
	}
}

// findSeparators: a filename is not prose, and the punctuation it is made of
// has to be read as spaces or the words inside it are unreachable.
func findSeparators(t *testing.T, s db.Repo) {
	put(t, s, file("camera/IMG_0042.JPEG"))
	put(t, s, file("camera/beach-sunset (2).JPEG"))

	for _, term := range []string{"0042", "jpeg", "sunset", "beach"} {
		if got := found(t, s, term); len(got) == 0 {
			t.Errorf("%q found nothing", term)
		}
	}
}

func findCase(t *testing.T, s db.Repo) {
	put(t, s, file("holiday/Sunset.JPEG"))

	for _, term := range []string{"sunset", "SUNSET", "SunSet"} {
		if got := found(t, s, term); !slices.Equal(got, []string{"holiday/Sunset.JPEG"}) {
			t.Errorf("%q = %v", term, got)
		}
	}
}

// findPhrase: two words mean those two words, in that order and together.
func findPhrase(t *testing.T, s db.Repo) {
	put(t, s, file("holiday/beach sunset.jpg"))
	put(t, s, file("holiday/sunset beach.jpg"))
	put(t, s, file("holiday/beach storm sunset.jpg"))

	if got := found(t, s, "beach sunset"); !slices.Equal(got, []string{"holiday/beach sunset.jpg"}) {
		t.Errorf("beach sunset = %v, want the one that reads that way", got)
	}
}

func findTags(t *testing.T, s db.Repo) {
	catalogue(t, s,
		record{artist: "Autechre", album: "Amber"},
		record{artist: "Burial", album: "Untrue"},
	)

	tracks := db.FindFilter{Text: "autechre", Tracks: db.Window{Limit: 50}}
	if got := foundIn(t, s, tracks); len(got) != 1 {
		t.Errorf("autechre = %v, want its one track", got)
	}
	album := db.FindFilter{Text: "untrue", Tracks: db.Window{Limit: 50}}
	if got := foundIn(t, s, album); len(got) != 1 {
		t.Errorf("untrue = %v, want the track on it", got)
	}
}

// shot stores an image and the row an extractor would have left beside it.
// Not photo, which this package already has for the gallery's own cases.
func shot(t *testing.T, s db.Repo, name, camera string, taken time.Time) {
	t.Helper()
	stored := put(t, s, file(name))
	if err := s.PutMedia(t.Context(), db.Media{
		FileID: stored.ID, Kind: db.KindImage, IndexedAt: time.Now(), Version: 1,
		Camera: camera, TakenAt: taken,
	}); err != nil {
		t.Fatalf("PutMedia(%q): %v", name, err)
	}
}

// findPhotos: a camera calls everything IMG_0042, so what a photograph is found
// by is what the camera recorded rather than what the file is called.
func findPhotos(t *testing.T, s db.Repo) {
	shot(t, s, "camera/IMG_0042.JPG", "Olympus OM-1", time.Date(2024, 6, 2, 10, 0, 0, 0, time.UTC))
	shot(t, s, "camera/IMG_0043.JPG", "Canon EOS R6", time.Date(2019, 8, 9, 10, 0, 0, 0, time.UTC))

	window := db.Window{Limit: 50}
	for term, want := range map[string]string{
		"olympus": "camera/IMG_0042.JPG",
		"OLYMPUS": "camera/IMG_0042.JPG",
		"2024":    "camera/IMG_0042.JPG",
		"canon":   "camera/IMG_0043.JPG",
		"2019":    "camera/IMG_0043.JPG",
	} {
		got := foundIn(t, s, db.FindFilter{Text: term, Photos: window})
		if !slices.Equal(got, []string{want}) {
			t.Errorf("%q found %v, want [%s]", term, got, want)
		}
	}

	// And the half nobody asked for stays empty, as it does for the others.
	if got := foundIn(t, s, db.FindFilter{Text: "olympus", Files: window}); len(got) != 0 {
		t.Errorf("a camera matched a file name: %v", got)
	}
}

// findPhotosBlank: a photograph nothing was recorded about has no text, and an
// empty column must not be something every search matches.
func findPhotosBlank(t *testing.T, s db.Repo) {
	shot(t, s, "camera/IMG_0044.JPG", "", time.Time{})
	shot(t, s, "camera/IMG_0045.JPG", "Olympus OM-1", time.Time{})

	window := db.Window{Limit: 50}
	if got := foundIn(t, s, db.FindFilter{Text: "olympus", Photos: window}); !slices.Equal(got, []string{"camera/IMG_0045.JPG"}) {
		t.Errorf("olympus found %v, want the one with a camera", got)
	}
	// A year nothing was taken in. Zero is not a date, so neither row has one.
	if got := foundIn(t, s, db.FindFilter{Text: "0001", Photos: window}); len(got) != 0 {
		t.Errorf("a photograph with no date was filed under year one: %v", got)
	}
}

func findOwner(t *testing.T, s db.Repo) {
	put(t, s, file("holiday/sunset.jpg"))
	theirs := file("holiday/sunset.jpg")
	theirs.OwnerID = "someone-else"
	theirs.BlobKey = "their-blob"
	if _, err := s.PutFile(t.Context(), theirs); err != nil {
		t.Fatal(err)
	}

	result, err := s.Find(t.Context(), "someone-else",
		db.FindFilter{Text: "sunset", Files: db.Window{Limit: 50}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Files) != 1 || result.Files[0].BlobKey != "their-blob" {
		t.Errorf("the other owner's search = %+v, want their row alone", result.Files)
	}
}

// findEmpty: a person looking at an empty box is asking for nothing, which is
// the opposite of what Music.Search does with an empty query.
func findEmpty(t *testing.T, s db.Repo) {
	put(t, s, file("holiday/sunset.jpg"))
	catalogue(t, s, record{artist: "Autechre", album: "Amber"})

	if got := foundIn(t, s, db.FindFilter{
		Files:  db.Window{Limit: 50},
		Tracks: db.Window{Limit: 50},
	}); len(got) != 0 {
		t.Errorf("an empty term = %v, want nothing", got)
	}
}

func findUnwanted(t *testing.T, s db.Repo) {
	put(t, s, file("holiday/amber.jpg"))
	catalogue(t, s, record{artist: "Autechre", album: "Amber"})

	result, err := s.Find(t.Context(), owner,
		db.FindFilter{Text: "amber", Files: db.Window{Limit: 50}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Files) == 0 {
		t.Error("the half that was asked for came back empty")
	}
	if len(result.Tracks) != 0 {
		t.Errorf("tracks = %+v, want none: nothing asked for them", result.Tracks)
	}
}

// findPaged walks a search a page at a time and reassembles it: every row once,
// in one order, which is the property a reader scrolling a result depends on.
func findPaged(t *testing.T, s db.Repo) {
	for i := range 7 {
		put(t, s, file("holiday/sunset-"+strconv.Itoa(i)+".jpg"))
	}

	var walked []string
	after := db.Cursor{}
	for range 4 {
		result, err := s.Find(t.Context(), owner, db.FindFilter{
			Text:  "sunset",
			Files: db.Window{After: after, Limit: 2},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Files) > 2 {
			t.Fatalf("a page of 2 came back with %d", len(result.Files))
		}
		for _, f := range result.Files {
			walked = append(walked, f.Path)
		}
		if len(result.Files) == 0 {
			break
		}
		after = db.After(result.Files[len(result.Files)-1])
	}

	want := make([]string, 0, 7)
	for i := range 7 {
		want = append(want, fmt.Sprintf("holiday/sunset-%d.jpg", i))
	}
	if !slices.Equal(walked, want) {
		t.Errorf("walked %v, want %v", walked, want)
	}
}

// findPagedDeletedCursor: a cursor is a position in an ordering, not a row.
func findPagedDeletedCursor(t *testing.T, s db.Repo) {
	for _, p := range []string{"sunset-a.jpg", "sunset-b.jpg", "sunset-c.jpg"} {
		put(t, s, file(p))
	}

	first, err := s.Find(t.Context(), owner,
		db.FindFilter{Text: "sunset", Files: db.Window{Limit: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Files) != 1 || first.Files[0].Path != "sunset-a.jpg" {
		t.Fatalf("first page = %+v", first.Files)
	}
	if err = s.DeleteFile(t.Context(), owner, "sunset-a.jpg"); err != nil {
		t.Fatal(err)
	}

	rest, err := s.Find(t.Context(), owner, db.FindFilter{
		Text:  "sunset",
		Files: db.Window{After: db.After(first.Files[0]), Limit: 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(rest.Files))
	for _, f := range rest.Files {
		got = append(got, f.Path)
	}
	if !slices.Equal(got, []string{"sunset-b.jpg", "sunset-c.jpg"}) {
		t.Errorf("after a deleted cursor = %v", got)
	}
}

func findLimit(t *testing.T, s db.Repo) {
	put(t, s, file("holiday/sunset.jpg"))

	if _, err := s.Find(t.Context(), owner,
		db.FindFilter{Text: "sunset", Files: db.Window{Limit: -1}}); err == nil {
		t.Error("a page of -1 rows was allowed")
	}
}
