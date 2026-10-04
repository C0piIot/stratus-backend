package web_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/db/dbtest"
	"github.com/C0piIot/stratus-backend/internal/files"
)

func TestSearchNeedsASession(t *testing.T) {
	t.Parallel()
	h := newHandler(t, nil)

	rec := get(t, h, "/search?q=holiday")
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/login") {
		t.Errorf("/search with no session = %d %q, want the login form",
			rec.Code, rec.Header().Get("Location"))
	}
}

// TestTheBoxIsOnEveryPageAndKeepsWhatWasTyped: it is in the layout, so every
// page behind the login carries it, and a result page shows the term back.
func TestTheBoxIsOnEveryPageAndKeepsWhatWasTyped(t *testing.T) {
	t.Parallel()
	h := newHandler(t, nil)
	cookie := signIn(t, h)

	for _, target := range []string{"/files/", "/status", "/gallery/photos", "/music"} {
		body := get(t, h, target, cookie).Body.String()
		has(t, body, `action="/search"`, `name="q"`)
	}

	// The sign-out form is still there, outside the search form: a form does
	// not nest, and the links used to live inside that one.
	listing := get(t, h, "/files/", cookie).Body.String()
	has(t, listing, `action="/logout"`, `href="/music"`)

	result := get(t, h, "/search?q=holiday", cookie).Body.String()
	has(t, result, `value="holiday"`)
}

// TestTheBoxHasSomethingToPress: Enter has always submitted it, which is
// nothing to rely on with a touch keyboard, so the field is an input group
// with its own submit beside it. Still no script -- it is the form's submit --
// and still one field, so what is sent is what was already sent.
func TestTheBoxHasSomethingToPress(t *testing.T) {
	t.Parallel()
	h := newHandler(t, nil)
	cookie := signIn(t, h)

	body := get(t, h, "/files/", cookie).Body.String()
	has(t, body, `class="input-group input-group-sm"`, `type="submit" aria-label="Search"`,
		// The emoji rather than an icon set: see the template.
		"🔍")
}

// TestAnEmptyBoxIsAPageAndNotAnError.
func TestAnEmptyBoxIsAPageAndNotAnError(t *testing.T) {
	t.Parallel()
	h := newHandler(t, nil)

	rec := get(t, h, "/search?q=%20%20", signIn(t, h))
	if rec.Code != http.StatusOK {
		t.Fatalf("an empty search = %d", rec.Code)
	}
	has(t, rec.Body.String(), "Type into the box")
}

// TestASearchFindsNamesAndTags, which is the whole feature: one box, every
// shape in the library.
func TestASearchFindsNamesAndTags(t *testing.T) {
	t.Parallel()
	h, s, meta := browserOver(t)
	cookie := signIn(t, h)
	mkdir(t, s, "holiday")
	write(t, s, "holiday/sunset-beach.txt", "where we went")
	write(t, s, "notes.txt", "nothing to do with it")
	addTrack(t, s, meta, "sunset.flac", db.Media{
		AlbumArtist: "Autechre", Artist: "Autechre", Album: "Amber",
		Title: "Montreal", DurationMS: 254_000,
	})

	body := get(t, h, "/search?q=sunset", cookie).Body.String()
	has(t, body, ">sunset-beach.txt")
	if strings.Contains(body, ">notes.txt") {
		t.Errorf("a file nothing matched is in the results:\n%s", body)
	}

	// The music half is the tags and not the names: a word in a filename finds
	// the file above, and a track's own title finds the track here.
	tagged := get(t, h, "/search?q=montreal", cookie).Body.String()
	has(t, tagged, ">Montreal<", "Autechre", "/music/Autechre/Amber")

	// A folder is found by its own name, and what is inside it is not dragged
	// along: that is what the port promises and what the page shows.
	folder := get(t, h, "/search?q=holiday", cookie).Body.String()
	has(t, folder, ">holiday/<")
	if strings.Contains(folder, ">sunset-beach.txt") {
		t.Errorf("a folder's match brought its contents with it:\n%s", folder)
	}
}

// TestAnArtistIsOneLineAndNotTheirDiscography is #262's decision seen from the
// page: a word that is somebody's name answers with the name, and their tracks
// stay where they are, which is under it.
func TestAnArtistIsOneLineAndNotTheirDiscography(t *testing.T) {
	t.Parallel()
	h, s, meta := browserOver(t)
	cookie := signIn(t, h)
	for i, title := range []string{"Montreal", "Nine", "Silverside"} {
		addTrack(t, s, meta, fmt.Sprintf("autechre-%d.flac", i), db.Media{
			AlbumArtist: "Autechre", Artist: "Autechre", Album: "Amber", Title: title,
		})
	}

	body := get(t, h, "/search?q=autechre", cookie).Body.String()
	has(t, body, `href="/music/Autechre"`, "1 album")
	for _, title := range []string{">Montreal<", ">Nine<", ">Silverside<"} {
		if strings.Contains(body, title) {
			t.Errorf("an artist's tracks came back with them (%s):\n%s", title, body)
		}
	}

	// And an album is its own line too, with who it is by under it.
	album := get(t, h, "/search?q=amber", cookie).Body.String()
	has(t, album, `href="/music/Autechre/Amber"`, "3 tracks")
}

// TestTheTwoNameBucketsPage: artists and albums resume by name, which is a
// different cursor from the one the three made of files use.
func TestTheTwoNameBucketsPage(t *testing.T) {
	t.Parallel()
	h, s, meta := browserOver(t)
	cookie := signIn(t, h)
	for i := range 60 {
		addTrack(t, s, meta, fmt.Sprintf("lumen-%02d.flac", i), db.Media{
			AlbumArtist: fmt.Sprintf("Lumen %02d", i),
			Artist:      fmt.Sprintf("Lumen %02d", i),
			Album:       fmt.Sprintf("Lumen %02d", i),
			Title:       "Untitled",
		})
	}

	first := get(t, h, "/search?q=lumen&in=artists", cookie).Body.String()
	if strings.Contains(first, ">Lumen 59<") {
		t.Error("the first page holds the last artist, so nothing was paged")
	}
	next := nextLink.FindStringSubmatch(first)
	if next == nil {
		t.Fatalf("no link to the rest of the artists:\n%s", first)
	}
	has(t, next[1], "in=artists", "after=Lumen")

	second := get(t, h, html(next[1]), cookie).Body.String()
	has(t, second, ">Lumen 59<")
	if strings.Contains(second, ">Lumen 00<") {
		t.Errorf("the second page repeats the first:\n%s", second)
	}

	// An album resumes by two names, so its cursor carries both.
	albums := get(t, h, "/search?q=lumen&in=albums", cookie).Body.String()
	link := nextLink.FindStringSubmatch(albums)
	if link == nil {
		t.Fatalf("no link to the rest of the albums:\n%s", albums)
	}
	has(t, link[1], "in=albums", "%2F")
	has(t, get(t, h, html(link[1]), cookie).Body.String(), ">Lumen 59<")
}

func TestASearchThatFindsNothingSaysSo(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	write(t, s, "notes.txt", "x")

	body := get(t, h, "/search?q=nothinghere", signIn(t, h)).Body.String()
	has(t, body, "Nothing is called that", "No music is called that")
}

// TestOneHalfAtATime: the "more" link narrows the page to the half somebody is
// reading, because two cursors in one URL is a URL nobody can read.
func TestOneHalfAtATime(t *testing.T) {
	t.Parallel()
	h, s, meta := browserOver(t)
	cookie := signIn(t, h)
	write(t, s, "sunset.txt", "a file")
	addTrack(t, s, meta, "track.flac", db.Media{Artist: "Boards", Title: "Sunset"})

	files := get(t, h, "/search?q=sunset&in=files", cookie).Body.String()
	has(t, files, ">sunset.txt")
	if strings.Contains(files, ">Sunset<") {
		t.Errorf("a files-only page showed tracks:\n%s", files)
	}

	tracks := get(t, h, "/search?q=sunset&in=tracks", cookie).Body.String()
	has(t, tracks, ">Sunset<")
	if strings.Contains(tracks, ">sunset.txt") {
		t.Errorf("a tracks-only page showed files:\n%s", tracks)
	}
}

// TestASearchPages walks a result bigger than a page, which is the property a
// cursor buys: every row once, and the next page resumes exactly.
func TestASearchPages(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	cookie := signIn(t, h)
	for i := range 60 {
		write(t, s, fmt.Sprintf("sunset-%02d.txt", i), "x")
	}

	first := get(t, h, "/search?q=sunset", cookie).Body.String()
	if got := strings.Count(first, "sunset-"); got < 50 {
		t.Fatalf("the first page holds %d mentions, want fifty rows' worth", got)
	}
	if strings.Contains(first, ">sunset-59.txt") {
		t.Error("the first page holds the last row, so nothing was paged")
	}

	next := nextLink.FindStringSubmatch(first)
	if next == nil {
		t.Fatalf("no link to the rest of the files:\n%s", first)
	}
	if !strings.Contains(next[1], "in=files") {
		t.Errorf("the next page is %q, want it narrowed to one half", next[1])
	}

	second := get(t, h, html(next[1]), cookie).Body.String()
	has(t, second, ">sunset-59.txt")
	if strings.Contains(second, ">sunset-00.txt") {
		t.Errorf("the second page repeats the first:\n%s", second)
	}
}

// TestHtmxGetsOneHalfAndNothingElse: the same URL, a piece of the same HTML.
func TestHtmxGetsOneHalfAndNothingElse(t *testing.T) {
	t.Parallel()
	h, s, meta := browserOver(t)
	cookie := signIn(t, h)
	write(t, s, "sunset.txt", "a file")
	addTrack(t, s, meta, "track.flac", db.Media{Artist: "Boards", Title: "Sunset"})

	files := htmx(t, h, "/search?q=sunset&in=files", cookie).Body.String()
	if !strings.HasPrefix(strings.TrimSpace(files), "<tr") || strings.Contains(files, "<!doctype") {
		t.Errorf("htmx was given a document rather than rows:\n%s", files)
	}
	tracks := htmx(t, h, "/search?q=sunset&in=tracks", cookie).Body.String()
	if strings.Contains(tracks, "<!doctype") || strings.Contains(tracks, "<tr") {
		t.Errorf("htmx was given a document rather than the track list:\n%s", tracks)
	}
	has(t, tracks, "list-group-item", ">Sunset<")
}

func TestASearchThatIsNotOne(t *testing.T) {
	t.Parallel()
	h := newHandler(t, nil)
	cookie := signIn(t, h)

	for _, query := range []string{
		"?q=sunset&in=elsewhere",
		"?q=sunset&after=f%2F..%2Fetc%2Fpasswd",
		"?q=sunset&after=nokind",
	} {
		if code := get(t, h, "/search"+query, cookie).Code; code != http.StatusBadRequest {
			t.Errorf("/search%s = %d, want 400", query, code)
		}
	}
}

// TestASearchIsNotSharedReading: a link to one folder does not authorise a
// search over everything, and there is no path in this URL to check it against.
func TestASearchIsNotSharedReading(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	mkdir(t, s, "holiday")
	write(t, s, "holiday/notes.txt", "where we went")
	link := linkTo(t, h, "holiday", "7d")

	token := link[strings.Index(link, "?"):]
	rec := get(t, h, "/search"+token+"&q=notes")
	if rec.Code != http.StatusSeeOther {
		t.Errorf("a search on a share's token = %d, want the login form", rec.Code)
	}
}

// TestATrackWithNoTagsIsStillFound: a track nothing has read is found by its
// name, in the files bucket, which is the state every track is in for the
// minute after it arrives. The music bucket matches titles, and a row with no
// tags has none -- so the two buckets divide this between them rather than one
// of them inventing a title out of the path.
func TestATrackWithNoTagsIsStillFound(t *testing.T) {
	t.Parallel()
	h, s, meta := browserOver(t)
	cookie := signIn(t, h)
	addTrack(t, s, meta, "sunset.flac", db.Media{})

	body := get(t, h, "/search?q=sunset", cookie).Body.String()
	has(t, body, ">sunset.flac", "No music is called that")

	// And one that has been read shows its tags and not its name, with no link
	// to an album it is not on.
	addTrack(t, s, meta, "02.flac", db.Media{Artist: "Collective", Title: "Harbour"})
	tagged := get(t, h, "/search?q=harbour&in=tracks", cookie).Body.String()
	has(t, tagged, ">Harbour<", "Collective")
	if strings.Contains(tagged, ">Album<") {
		t.Errorf("a track with no album offered a link to one:\n%s", tagged)
	}
}

// TestASearchSurvivesNothing: an index that will not answer is a page that says
// so, not a half-rendered result.
func TestASearchSurvivesNothing(t *testing.T) {
	t.Parallel()
	blobs, meta := backends(t)
	broken := dbtest.FailOn(t, meta, "Find")
	s := files.New(blobs, meta)
	h := handlerOverIndex(t, s, blobs, broken)

	rec := get(t, h, "/search?q=sunset", signIn(t, h))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("a search over a broken index = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), dbtest.ErrInjected.Error()) {
		t.Errorf("the page tells the reader what the database said:\n%s", rec.Body)
	}
}

// TestASearchFindsAPhotographByItsCamera: a camera calls everything IMG_0042,
// so the name half finds nothing anybody meant and this half is why.
func TestASearchFindsAPhotographByItsCamera(t *testing.T) {
	t.Parallel()
	h, s, meta := browserOver(t)
	cookie := signIn(t, h)
	addPhoto(t, s, meta, "IMG_0042.jpg", time.Date(2024, 6, 2, 10, 0, 0, 0, time.UTC), "Olympus OM-1")
	addPhoto(t, s, meta, "IMG_0043.jpg", time.Date(2019, 8, 9, 10, 0, 0, 0, time.UTC), "Canon EOS R6")

	body := get(t, h, "/search?q=olympus", cookie).Body.String()
	has(t, body, "Photos", `/gallery/photos/IMG_0042.jpg`, "/thumb/IMG_0042.jpg?size=300")
	if strings.Contains(body, "IMG_0043") {
		t.Errorf("a photograph from another camera is in the results:\n%s", body)
	}

	// And by the year the camera recorded, which is the other half of what a
	// photograph says about itself.
	if year := get(t, h, "/search?q=2019", cookie).Body.String(); !strings.Contains(year, "IMG_0043.jpg") {
		t.Errorf("a year found nothing:\n%s", year)
	}
}

// TestOnePhotoHalfAtATime: the photographs page on their own like the other two.
func TestOnePhotoHalfAtATime(t *testing.T) {
	t.Parallel()
	h, s, meta := browserOver(t)
	cookie := signIn(t, h)
	addPhoto(t, s, meta, "IMG_0042.jpg", time.Date(2024, 6, 2, 10, 0, 0, 0, time.UTC), "Olympus OM-1")
	write(t, s, "olympus-notes.txt", "about the camera")

	only := get(t, h, "/search?q=olympus&in=photos", cookie).Body.String()
	has(t, only, "IMG_0042.jpg")
	if strings.Contains(only, "olympus-notes.txt") {
		t.Errorf("a photographs-only page showed files:\n%s", only)
	}

	fragment := htmx(t, h, "/search?q=olympus&in=photos", cookie).Body.String()
	if strings.Contains(fragment, "<!doctype") || strings.Contains(fragment, "<tr") {
		t.Errorf("htmx was given a document rather than the grid:\n%s", fragment)
	}
	has(t, fragment, "ratio-1x1")
}
