package web_test

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// row writes a file row with a size and a time of its own. The service cannot:
// it stamps a write with the clock, and three writes a millisecond apart are
// not an ordering a test can assert.
func row(t *testing.T, meta db.Store, name string, size int64, mtime time.Time) {
	t.Helper()
	_, err := meta.PutFile(t.Context(), db.File{
		OwnerID: username, Path: name, BlobKey: "blobs/" + name,
		Size: size, MTime: mtime, ETag: `"` + name + `"`, MIMEType: "application/octet-stream",
	}.Normalize())
	if err != nil {
		t.Fatal(err)
	}
}

// listed is the names in the order the page printed them.
func listed(body string, names ...string) []string {
	out := slices.Clone(names)
	slices.SortFunc(out, func(a, b string) int {
		return strings.Index(body, ">"+a) - strings.Index(body, ">"+b)
	})
	return out
}

// day is a time far enough apart to be an ordering rather than a coincidence.
func day(d int) time.Time { return time.Date(2024, 6, d, 12, 0, 0, 0, time.UTC) }

// threeFiles is a folder whose three orderings disagree with each other, which
// is the only way a test can tell them apart.
func threeFiles(t *testing.T, meta db.Store) {
	t.Helper()
	row(t, meta, "big.bin", 900, day(1))
	row(t, meta, "middle.jpg", 300, day(2))
	row(t, meta, "small.txt", 10, day(3))
}

func TestAListingIsOrderedByWhatTheURLAsksFor(t *testing.T) {
	t.Parallel()
	h, _, meta := browserOver(t)
	cookie := signIn(t, h)
	threeFiles(t, meta)

	tests := []struct {
		query string
		want  []string
	}{
		{"", []string{"big.bin", "middle.jpg", "small.txt"}},
		{"?sort=name&order=desc", []string{"small.txt", "middle.jpg", "big.bin"}},
		{"?sort=size", []string{"small.txt", "middle.jpg", "big.bin"}},
		{"?sort=size&order=desc", []string{"big.bin", "middle.jpg", "small.txt"}},
		{"?sort=modified", []string{"big.bin", "middle.jpg", "small.txt"}},
		{"?sort=modified&order=desc", []string{"small.txt", "middle.jpg", "big.bin"}},
	}
	for _, tt := range tests {
		body := get(t, h, "/files/"+tt.query, cookie).Body.String()
		got := listed(body, "big.bin", "middle.jpg", "small.txt")
		if !slices.Equal(got, tt.want) {
			t.Errorf("/files/%s listed %v, want %v", tt.query, got, tt.want)
		}
	}
}

// TestFoldersStayOnTopHoweverItIsOrdered: the grouping is not a preference, so
// reversing a listing must not send the folders to the bottom of it.
func TestFoldersStayOnTopHoweverItIsOrdered(t *testing.T) {
	t.Parallel()
	h, s, meta := browserOver(t)
	cookie := signIn(t, h)
	mkdir(t, s, "holiday")
	threeFiles(t, meta)

	for _, query := range []string{"?sort=size&order=desc", "?sort=modified&order=desc", "?sort=name&order=desc"} {
		body := get(t, h, "/files/"+query, cookie).Body.String()
		if got := listed(body, "holiday", "big.bin", "small.txt"); got[0] != "holiday" {
			t.Errorf("/files/%s listed %v, want the folder first", query, got)
		}
	}
}

// TestAnOrderingSurvivesThePageBoundary is what the cursor's new half is for: a
// position in an ordering by size is a size and a path, and a cursor carrying
// only the path would resume in the wrong place.
func TestAnOrderingSurvivesThePageBoundary(t *testing.T) {
	t.Parallel()
	h, _, meta := browserOver(t)
	cookie := signIn(t, h)
	for i := range 60 {
		// Sizes descend as names ascend, so a page resumed by path would come
		// back in a visibly different order.
		row(t, meta, "f"+string(rune('a'+i/26))+string(rune('a'+i%26))+".txt", int64(1000-i), day(1))
	}

	first := get(t, h, "/files/?sort=size&rows=50", cookie).Body.String()
	next := nextLink.FindStringSubmatch(first)
	if next == nil {
		t.Fatalf("a folder of 60 rows offered no next page:\n%s", first)
	}
	if !strings.Contains(next[1], "sort=size") || !strings.Contains(next[1], "rows=50") {
		t.Errorf("the next page is %q, want it to carry the ordering and the row count", next[1])
	}

	second := get(t, h, html(next[1]), cookie).Body.String()
	// The ten largest sizes are the ten lowest letters, so the last page is
	// exactly the first ten names.
	for _, want := range []string{"faa.txt", "faj.txt"} {
		if !strings.Contains(second, ">"+want) {
			t.Errorf("the last page does not hold %s:\n%s", want, second)
		}
	}
	if strings.Contains(second, ">fak.txt") {
		t.Errorf("the last page repeats a row the first one had:\n%s", second)
	}
}

func TestHowManyRowsAPageHolds(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	cookie := signIn(t, h)
	for i := range 60 {
		write(t, s, "f"+string(rune('a'+i/26))+string(rune('a'+i%26))+".txt", "x")
	}

	if got := strings.Count(get(t, h, "/files/?rows=50", cookie).Body.String(), ".txt<"); got != 50 {
		t.Errorf("a page of 50 held %d rows", got)
	}
	if got := strings.Count(get(t, h, "/files/", cookie).Body.String(), ".txt<"); got != 60 {
		t.Errorf("a page with nothing asked for held %d rows, want all 60 of them", got)
	}
}

// TestTheHeadingsSayWhereTheyGo: an arrow on the column in use, the opposite
// direction behind it, and the direction somebody means on the ones beside it.
func TestTheHeadingsSayWhereTheyGo(t *testing.T) {
	t.Parallel()
	h, _, meta := browserOver(t)
	cookie := signIn(t, h)
	threeFiles(t, meta)

	body := get(t, h, "/files/?sort=size&order=asc", cookie).Body.String()
	has(t, body, `aria-sort="ascending"`, "Size ↑",
		// Clicking the column in use turns it around.
		"order=desc&amp;rows=100&amp;sort=size",
		// A date starts at the most recent, which is what somebody means by it.
		"order=desc&amp;rows=100&amp;sort=modified",
		// A name starts at A.
		"order=asc&amp;rows=100&amp;sort=name")
	if strings.Count(body, `aria-sort="none"`) != 2 {
		t.Errorf("the columns not in use are not marked unsorted:\n%s", body)
	}
}

func TestAnArrangementThatIsNotOne(t *testing.T) {
	t.Parallel()
	h := newHandler(t, nil)
	cookie := signIn(t, h)

	for _, query := range []string{"?sort=colour", "?order=sideways", "?rows=100000", "?rows=three"} {
		if code := get(t, h, "/files/"+query, cookie).Code; code != http.StatusBadRequest {
			t.Errorf("/files/%s = %d, want 400", query, code)
		}
	}
}

// TestACursorFromAnotherOrdering: a position in one ordering is not a position
// in another, so a cursor is read in the shape the sort names or not at all.
func TestACursorFromAnotherOrdering(t *testing.T) {
	t.Parallel()
	h := newHandler(t, nil)
	cookie := signIn(t, h)

	for _, query := range []string{
		"?sort=size&after=f%2Fnotes.txt",          // no sort value in it
		"?sort=size&after=f%2Fbig%2Fnotes.txt",    // one that is not a number
		"?sort=modified&after=f%2F..%2Fetc%2Fpwd", // and the old guard still holds
	} {
		if code := get(t, h, "/files/"+query, cookie).Code; code != http.StatusBadRequest {
			t.Errorf("/files/%s = %d, want 400", query, code)
		}
	}
}

// TestTheLastArrangementIsRemembered: the cookie carries a preference and
// nothing else, so it is written only when somebody expressed one and read only
// when the URL does not.
func TestTheLastArrangementIsRemembered(t *testing.T) {
	t.Parallel()
	h, _, meta := browserOver(t)
	session := signIn(t, h)
	threeFiles(t, meta)

	rec := get(t, h, "/files/?sort=size&order=desc&rows=50", session)
	saved := namedCookie(rec, "stratus_list")
	if saved == nil {
		t.Fatalf("asking for an ordering saved nothing: %v", rec.Header())
	}
	if saved.HttpOnly {
		t.Error("the preference cookie is HttpOnly, which it has no credential to protect")
	}

	body := get(t, h, "/files/", session, saved).Body.String()
	if got := listed(body, "big.bin", "middle.jpg", "small.txt"); got[0] != "big.bin" {
		t.Errorf("a folder opened with no opinion listed %v, want the saved one", got)
	}

	// And a link somebody was sent opens the way it was written, whatever the
	// receiver last chose.
	body = get(t, h, "/files/?sort=size&order=asc", session, saved).Body.String()
	if got := listed(body, "big.bin", "middle.jpg", "small.txt"); got[0] != "small.txt" {
		t.Errorf("a URL that names an ordering listed %v, want its own", got)
	}

	// Opening a folder with no opinion of your own changes nothing.
	if c := namedCookie(get(t, h, "/files/", session, saved), "stratus_list"); c != nil {
		t.Errorf("browsing rewrote the preference to %q", c.Value)
	}
}

// TestAVisitorsArrangementIsNotRemembered: a share is somebody else's browser,
// and what they do to a folder that is not theirs is not a preference to keep.
func TestAVisitorsArrangementIsNotRemembered(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	mkdir(t, s, "holiday")
	write(t, s, "holiday/notes.txt", "where we went")
	link := linkTo(t, h, "holiday", "7d")

	rec := get(t, h, link+"&sort=size")
	if rec.Code != http.StatusOK {
		t.Fatalf("a shared folder ordered by size = %d: %s", rec.Code, rec.Body)
	}
	if c := namedCookie(rec, "stratus_list"); c != nil {
		t.Errorf("a visitor's ordering was saved as %q", c.Value)
	}
	// Every link the page emits still carries the signature, or the second
	// click is a login form.
	for _, want := range []string{"sort=modified", "sort=size", "rows=500"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("a shared listing offers no %s", want)
		}
	}
	if strings.Count(rec.Body.String(), "k=") < 6 {
		t.Errorf("the headings and row counts do not carry the token:\n%s", rec.Body)
	}
}

// namedCookie is sessionCookie for any of them.
func namedCookie(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range (&http.Response{Header: rec.Header()}).Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}
