package web_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/db/dbtest"
	"github.com/C0piIot/stratus-backend/internal/files"
)

// rename posts the form the rename page shows.
func rename(t *testing.T, h http.Handler, target string, cookie *http.Cookie, to string) *httptest.ResponseRecorder {
	t.Helper()
	return post(t, h, "/rename/"+target, url.Values{"name": {to}}, cookie)
}

// remove posts the one the delete page shows, which carries nothing at all.
func remove(t *testing.T, h http.Handler, target string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	return post(t, h, "/delete/"+target, nil, cookies...)
}

// TestTheListingOffersBothActions: the links are how either of these is
// reached, so a row without them is a feature nobody can use.
func TestTheListingOffersBothActions(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	write(t, s, "notes.txt", "hello")
	mkdir(t, s, "photos")

	body := get(t, h, "/files/", signIn(t, h)).Body.String()
	for _, want := range []string{
		`href="/rename/notes.txt"`, `href="/delete/notes.txt"`,
		`href="/rename/photos"`, `href="/delete/photos"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the listing has no %s", want)
		}
	}
}

func TestRename(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	mkdir(t, s, "photos")
	write(t, s, "photos/img.jpg", "pixels")
	cookie := signIn(t, h)

	// The form arrives with the current name in it, so the common case is an
	// edit rather than retyping.
	form := get(t, h, "/rename/photos/img.jpg", cookie)
	if form.Code != http.StatusOK {
		t.Fatalf("the rename form = %d, want 200", form.Code)
	}
	if !strings.Contains(form.Body.String(), `value="img.jpg"`) {
		t.Error("the form does not carry the current name")
	}
	if !strings.Contains(form.Body.String(), `action="/rename/photos/img.jpg"`) {
		t.Error("the form posts somewhere else")
	}

	rec := rename(t, h, "photos/img.jpg", cookie, "holiday.jpg")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("renaming = %d, want 303", rec.Code)
	}
	// Back to the directory it is in, which is where the rename was asked for.
	if got := rec.Header().Get("Location"); got != "/files/photos" {
		t.Errorf("Location = %q", got)
	}

	if got := stored(t, s, "photos/holiday.jpg"); got != "pixels" {
		t.Errorf("the renamed file holds %q", got)
	}
	if _, err := s.Stat(t.Context(), username, "photos/img.jpg"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("the old name is still there: %v", err)
	}
}

func TestRenameAnEmptyFolder(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	mkdir(t, s, "phots")
	cookie := signIn(t, h)

	if rec := rename(t, h, "phots", cookie, "photos"); rec.Code != http.StatusSeeOther {
		t.Fatalf("renaming an empty folder = %d, want 303", rec.Code)
	}
	f, err := s.Stat(t.Context(), username, "photos")
	if err != nil || !f.IsDir {
		t.Errorf("the folder is not there under its new name: %v", err)
	}
}

// TestRenameAFolderWithSomethingInIt is what #101 was: renaming a folder is an
// ordinary thing to do from a browser, and it was refused here because the
// metadata port could not rewrite a subtree. It can, so this is now a rename
// like any other -- and what the case checks is that everything underneath came
// with it, which is the half a redirect cannot show.
func TestRenameAFolderWithSomethingInIt(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	mkdir(t, s, "photos")
	mkdir(t, s, "photos/raw")
	write(t, s, "photos/img.jpg", "pixels")
	write(t, s, "photos/raw/IMG_0001.dng", "more pixels")
	cookie := signIn(t, h)

	rec := rename(t, h, "photos", cookie, "holiday")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("renaming a folder with a file in it = %d, want 303: %s", rec.Code, excerpt(rec.Body.String(), "<p"))
	}

	for _, path := range []string{"holiday", "holiday/raw", "holiday/img.jpg", "holiday/raw/IMG_0001.dng"} {
		if _, err := s.Stat(t.Context(), username, path); err != nil {
			t.Errorf("%q is not there after the rename: %v", path, err)
		}
	}
	if _, err := s.Stat(t.Context(), username, "photos/img.jpg"); err == nil {
		t.Error("the file is still under the old folder name")
	}
}

// TestTheFormsThemselvesRefuse: the page that asks is a GET on the same path,
// so it has to answer the same way when there is nothing there to ask about.
func TestTheFormsThemselvesRefuse(t *testing.T) {
	t.Parallel()
	h, _ := browser(t)
	cookie := signIn(t, h)

	tests := []struct {
		target string
		want   int
		says   string
	}{
		{target: "/rename/nope.txt", want: http.StatusNotFound},
		{target: "/delete/nope.txt", want: http.StatusNotFound},
		{target: "/rename/", want: http.StatusBadRequest, says: "nothing there to change"},
		{target: "/delete/", want: http.StatusBadRequest, says: "nothing there to change"},
		{target: "/rename/a%01b", want: http.StatusBadRequest},
		{target: "/delete/a%01b", want: http.StatusBadRequest},
	}
	for _, tt := range tests {
		rec := get(t, h, tt.target, cookie)
		if rec.Code != tt.want {
			t.Errorf("GET %s = %d, want %d", tt.target, rec.Code, tt.want)
		}
		if tt.says != "" && !strings.Contains(rec.Body.String(), tt.says) {
			t.Errorf("GET %s says %q", tt.target, excerpt(rec.Body.String(), "<p"))
		}
	}

	// And the paths the two forms post to, which are the same ones.
	if got := rename(t, h, "a%01b", cookie, "x").Code; got != http.StatusBadRequest {
		t.Errorf("renaming a path that is not one = %d, want 400", got)
	}
	if got := remove(t, h, "a%01b", cookie).Code; got != http.StatusBadRequest {
		t.Errorf("deleting a path that is not one = %d, want 400", got)
	}
	if got := newFolder(t, h, "a%01b", cookie, "holiday").Code; got != http.StatusBadRequest {
		t.Errorf("making a folder under a path that is not one = %d, want 400", got)
	}
}

// TestWhenTheTreeWillNotAnswer: both actions end in a write, and a write that
// fails is this server's fault -- a page saying so, with the reason in the log.
func TestWhenTheTreeWillNotAnswer(t *testing.T) {
	t.Parallel()

	t.Run("a delete the database refuses", func(t *testing.T) {
		t.Parallel()
		blobs, meta := backends(t)
		working := files.New(blobs, meta)
		write(t, working, "notes.txt", "hello")

		h := handlerOver(t, files.New(blobs, dbtest.FailOn(t, meta, "DeleteFile")), blobs, meta)
		if got := remove(t, h, "notes.txt", signIn(t, h)).Code; got != http.StatusInternalServerError {
			t.Errorf("deleting through a database that refuses = %d, want 500", got)
		}
		// And it is still there, which is the half that matters.
		if _, err := working.Stat(t.Context(), username, "notes.txt"); err != nil {
			t.Errorf("the file went anyway: %v", err)
		}
	})

	t.Run("a rename the database refuses", func(t *testing.T) {
		t.Parallel()
		blobs, meta := backends(t)
		working := files.New(blobs, meta)
		mkdir(t, working, "photos")
		write(t, working, "photos/one.jpg", "bytes")

		// The folder has something in it, which used to be refused here before
		// the port could rewrite a subtree (#101). Now it is an ordinary move,
		// and what this covers is the ordinary failure of one.
		h := handlerOver(t, files.New(blobs, dbtest.FailOn(t, meta, "MoveFile")), blobs, meta)
		if got := rename(t, h, "photos", signIn(t, h), "holiday").Code; got != http.StatusInternalServerError {
			t.Errorf("renaming through a database that refuses = %d, want 500", got)
		}
		if _, err := working.Stat(t.Context(), username, "photos/one.jpg"); err != nil {
			t.Errorf("the tree moved anyway: %v", err)
		}
	})
}

func TestRenameRefuses(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	write(t, s, "notes.txt", "hello")
	write(t, s, "taken.txt", "already here")
	cookie := signIn(t, h)

	tests := []struct {
		name   string
		target string
		to     string
		code   int
	}{
		{name: "a name already taken", target: "notes.txt", to: "taken.txt", code: http.StatusConflict},
		{name: "no name at all", target: "notes.txt", code: http.StatusBadRequest},
		{name: "nothing but dots", target: "notes.txt", to: "..", code: http.StatusBadRequest},
		{name: "something that is not there", target: "nope.txt", to: "x.txt", code: http.StatusNotFound},
		{name: "the root itself", to: "x", code: http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if rec := rename(t, h, tt.target, cookie, tt.to); rec.Code != tt.code {
				t.Errorf("renaming %q to %q = %d, want %d", tt.target, tt.to, rec.Code, tt.code)
			}
		})
	}

	// The file is still there under its own name after all of that.
	if got := stored(t, s, "notes.txt"); got != "hello" {
		t.Errorf("notes.txt holds %q", got)
	}
}

// TestRenameIsNotAMove: the field is a name, so a path in it is reduced to one
// -- otherwise a text box would quietly move things across the tree.
func TestRenameIsNotAMove(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	mkdir(t, s, "photos")
	mkdir(t, s, "elsewhere")
	write(t, s, "photos/img.jpg", "pixels")
	cookie := signIn(t, h)

	if rec := rename(t, h, "photos/img.jpg", cookie, "../elsewhere/img.jpg"); rec.Code != http.StatusSeeOther {
		t.Fatalf("renaming = %d", rec.Code)
	}
	if got := stored(t, s, "photos/img.jpg"); got != "pixels" {
		t.Errorf("the file left the folder it was in: %q", got)
	}
	if _, err := s.Stat(t.Context(), username, "elsewhere/img.jpg"); !errors.Is(err, db.ErrNotFound) {
		t.Error("a rename moved a file to another directory")
	}
}

// TestRenameToTheSameName does nothing and says so by going back, rather than
// answering the conflict the port would.
func TestRenameToTheSameName(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	write(t, s, "notes.txt", "hello")
	cookie := signIn(t, h)

	rec := rename(t, h, "notes.txt", cookie, "notes.txt")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("renaming to the same name = %d, want 303", rec.Code)
	}
	if got := stored(t, s, "notes.txt"); got != "hello" {
		t.Errorf("notes.txt holds %q", got)
	}
}

func TestDelete(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	write(t, s, "notes.txt", "hello")
	cookie := signIn(t, h)

	// Asked first, still: a folder is worth a question even with a trash bin
	// behind it, and what the page says has changed from "this cannot be
	// undone" to how long you have to change your mind.
	form := get(t, h, "/delete/notes.txt", cookie)
	if form.Code != http.StatusOK {
		t.Fatalf("the delete page = %d, want 200", form.Code)
	}
	body := form.Body.String()
	if !strings.Contains(body, "trash") || !strings.Contains(body, "30 days") {
		t.Errorf("the page does not say where it goes or for how long:\n%s", body)
	}
	if !strings.Contains(body, `action="/delete/notes.txt"`) {
		t.Error("the page posts somewhere else")
	}
	// And asking did not do it.
	if _, err := s.Stat(t.Context(), username, "notes.txt"); err != nil {
		t.Fatalf("opening the delete page deleted the file: %v", err)
	}

	rec := remove(t, h, "notes.txt", cookie)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("deleting = %d, want 303", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/files/" {
		t.Errorf("Location = %q, want the directory it was in", got)
	}
	if _, err := s.Stat(t.Context(), username, "notes.txt"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("the file survived: %v", err)
	}

	// And it is in the trash, as one deletion.
	trash := get(t, h, "/trash", cookie).Body.String()
	has(t, trash, "notes.txt", "1 file", "Delete for good")
}

// TestTheTrashHoldsADeletionUntilItIsDestroyed is the page's whole job: what
// was deleted is still there, as one entry, and goes when it is told to.
func TestTheTrashHoldsADeletionUntilItIsDestroyed(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	mkdir(t, s, "album")
	write(t, s, "album/one.jpg", "one")
	write(t, s, "album/two.jpg", "two")
	cookie := signIn(t, h)

	if empty := get(t, h, "/trash", cookie).Body.String(); !strings.Contains(empty, "Nothing has been deleted") {
		t.Errorf("an empty trash does not say so:\n%s", empty)
	}

	remove(t, h, "album", cookie)
	page := get(t, h, "/trash", cookie).Body.String()
	// One accident and not three rows, named by the folder that was deleted.
	has(t, page, "album", "2 files")
	if strings.Contains(page, "one.jpg") {
		t.Errorf("the trash lists the files inside a deletion:\n%s", page)
	}

	// The status page says the room is still held, and links here.
	has(t, get(t, h, "/status", cookie).Body.String(), "In the trash", `href="/trash"`)

	// Destroying asks first, and this time it really is final.
	link := destroyLink.FindStringSubmatch(page)
	if link == nil {
		t.Fatalf("no way to destroy the deletion:\n%s", page)
	}
	has(t, get(t, h, html(link[1]), cookie).Body.String(), "cannot be undone")

	rec := post(t, h, html(link[1]), nil, cookie)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/trash" {
		t.Fatalf("destroying = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if after := get(t, h, "/trash", cookie).Body.String(); !strings.Contains(after, "Nothing has been deleted") {
		t.Errorf("the deletion outlived being destroyed:\n%s", after)
	}
}

// destroyLink finds the button that throws a deletion away for good.
var destroyLink = regexp.MustCompile(`href="(/trash/[^"]+)"`)

// TestRestoringFromTheTrash is the page's other half: what was deleted comes
// back where it was, and the browser is sent to the folder it landed in --
// which is the answer to "where did it go" when the name was taken.
func TestRestoringFromTheTrash(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	mkdir(t, s, "album")
	write(t, s, "album/one.jpg", "pixels")
	cookie := signIn(t, h)
	remove(t, h, "album", cookie)

	page := get(t, h, "/trash", cookie).Body.String()
	link := restoreForm.FindStringSubmatch(page)
	if link == nil {
		t.Fatalf("no way to restore the deletion:\n%s", page)
	}

	rec := post(t, h, html(link[1]), nil, cookie)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("restoring = %d, want 303", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/files/" {
		t.Errorf("Location = %q, want the folder it landed in", got)
	}
	if _, err := s.Stat(t.Context(), username, "album/one.jpg"); err != nil {
		t.Errorf("the file did not come back: %v", err)
	}
	if after := get(t, h, "/trash", cookie).Body.String(); !strings.Contains(after, "Nothing has been deleted") {
		t.Errorf("the deletion is still in the trash:\n%s", after)
	}

	// A deletion that is not there is a 404 rather than a silent success,
	// which is what a button pressed twice gets.
	if code := post(t, h, html(link[1]), nil, cookie).Code; code != http.StatusNotFound {
		t.Errorf("restoring it twice = %d, want 404", code)
	}
}

// restoreForm finds the form that puts a deletion back.
var restoreForm = regexp.MustCompile(`action="(/trash/[^"]+/restore)"`)

// TestTheTrashPages: fifty accidents is already more than anybody has, and
// past that it resumes by the moment and the deletion rather than by an
// offset, like every other list here.
func TestTheTrashPages(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	cookie := signIn(t, h)
	for i := range 51 {
		name := fmt.Sprintf("gone-%02d.txt", i)
		write(t, s, name, "x")
		remove(t, h, name, cookie)
	}

	first := get(t, h, "/trash", cookie).Body.String()
	next := nextLink.FindStringSubmatch(first)
	if next == nil {
		t.Fatalf("no link to the rest of the trash:\n%s", first)
	}
	has(t, next[1], "after=")

	// htmx is given the list and not a document, as everywhere else.
	fragment := htmx(t, h, html(next[1]), cookie).Body.String()
	if strings.Contains(fragment, "<!doctype") {
		t.Errorf("htmx was given a document:\n%s", fragment)
	}
	has(t, fragment, "list-group-item")

	// The fifty-first is on the second page and not on the first.
	if strings.Count(first, "Delete for good") != 50 {
		t.Errorf("the first page holds %d deletions, want fifty", strings.Count(first, "Delete for good"))
	}
}

// TestATrashCursorThatIsNotOne.
func TestATrashCursorThatIsNotOne(t *testing.T) {
	t.Parallel()
	h := newHandler(t, nil)
	cookie := signIn(t, h)

	for _, after := range []string{"nonsense", "notanumber/batch", "1700000000000"} {
		if code := get(t, h, "/trash?after="+after, cookie).Code; code != http.StatusBadRequest {
			t.Errorf("/trash?after=%s = %d, want 400", after, code)
		}
	}
}

// TestTheTrashSurvivesNothing: an index that will not answer is a page that
// says so, and a status page that cannot measure the trash still renders --
// it is one line on a page about something else.
func TestTheTrashSurvivesNothing(t *testing.T) {
	t.Parallel()
	blobs, meta := backends(t)
	for _, method := range []string{"TrashBatches", "TrashTotals"} {
		broken := files.New(blobs, dbtest.FailOn(t, meta, method))
		h := handlerOver(t, broken, blobs, meta)
		if code := get(t, h, "/trash", signIn(t, h)).Code; code != http.StatusInternalServerError {
			t.Errorf("/trash with %s broken = %d, want 500", method, code)
		}
	}

	broken := files.New(blobs, dbtest.FailOn(t, meta, "TrashTotals"))
	h := handlerOver(t, broken, blobs, meta)
	rec := get(t, h, "/status", signIn(t, h))
	if rec.Code != http.StatusOK {
		t.Fatalf("/status with the trash unmeasurable = %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "In the trash") {
		t.Error("the status page invented a number for a trash it could not measure")
	}
}

// TestDeleteAFolderTakesWhatIsInIt, which is what the page warns about.
func TestDeleteAFolderTakesWhatIsInIt(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	mkdir(t, s, "photos")
	mkdir(t, s, "photos/2026")
	write(t, s, "photos/2026/img.jpg", "pixels")
	write(t, s, "keep.txt", "still here")
	cookie := signIn(t, h)

	if !strings.Contains(get(t, h, "/delete/photos", cookie).Body.String(), "everything inside it") {
		t.Error("the page does not warn that a folder takes its contents")
	}

	rec := remove(t, h, "photos", cookie)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("deleting a folder = %d, want 303", rec.Code)
	}
	for _, gone := range []string{"photos", "photos/2026", "photos/2026/img.jpg"} {
		if _, err := s.Stat(t.Context(), username, gone); !errors.Is(err, db.ErrNotFound) {
			t.Errorf("%q survived: %v", gone, err)
		}
	}
	// And nothing outside it went with it.
	if got := stored(t, s, "keep.txt"); got != "still here" {
		t.Errorf("keep.txt holds %q", got)
	}
}

func TestDeleteRefuses(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	write(t, s, "notes.txt", "hello")
	cookie := signIn(t, h)

	t.Run("something that is not there", func(t *testing.T) {
		t.Parallel()
		if rec := remove(t, h, "nope.txt", cookie); rec.Code != http.StatusNotFound {
			t.Errorf("deleting nothing = %d, want 404", rec.Code)
		}
	})

	// The root has no row and no parent to go back to, so it is refused with
	// something a person can read rather than with the port's answer about
	// paths -- which is the same status and a worse sentence.
	t.Run("the root itself", func(t *testing.T) {
		t.Parallel()
		rec := remove(t, h, "", cookie)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("deleting the root = %d, want 400", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "nothing there to change") {
			t.Errorf("the page says %q", excerpt(rec.Body.String(), "<p"))
		}
	})

	t.Run("with no session", func(t *testing.T) {
		t.Parallel()
		rec := remove(t, h, "notes.txt")
		if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/login") {
			t.Errorf("deleting with no session = %d to %q, want the login form",
				rec.Code, rec.Header().Get("Location"))
		}
	})

	t.Run("from somebody else's page", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/delete/notes.txt", nil)
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("a cross-site delete = %d, want 403", rec.Code)
		}
	})

	// Through all of that, the file is still there.
	if got := stored(t, s, "notes.txt"); got != "hello" {
		t.Errorf("notes.txt holds %q", got)
	}
}

// TestTheTrashShowsWhatNobodyCanAccountFor: two kinds of row, and the page
// has to say which is which rather than offering a button that lies.
func TestTheTrashShowsWhatNobodyCanAccountFor(t *testing.T) {
	t.Parallel()
	h, s, blobs := browserOverStore(t)
	cookie := signIn(t, h)
	write(t, s, "notes.txt", "a file nothing is wrong with")

	// A blob in the store that no row claims, which is what a database
	// restored from last week looks like.
	if _, err := blobs.Put(t.Context(), "image/2026/01/01/NOROWHOLDSTHIS.jpg",
		strings.NewReader("irreplaceable"), -1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Collect(t.Context(), 0); err != nil {
		t.Fatal(err)
	}

	page := get(t, h, "/trash", cookie).Body.String()
	has(t, page, "Unaccounted for", "found by the sweep", "1 object")
	// It offers no way back, because there is nowhere to put it.
	if restoreForm.MatchString(page) {
		t.Errorf("the page offers to restore a blob with no path:\n%s", page)
	}
	// And it is not shown as something somebody deleted.
	if strings.Contains(page, "Restore") {
		t.Errorf("the page mixes the two kinds of row:\n%s", page)
	}

	// The status page separates the two numbers: what you deleted is a
	// decision, what the server cannot explain is a symptom.
	status := get(t, h, "/status", cookie).Body.String()
	has(t, status, "Unaccounted for")
	if strings.Contains(status, "In the trash") {
		t.Errorf("a sweep's find was counted as something somebody deleted:\n%s", status)
	}

	// Destroying it is the one thing offered, and it frees the room.
	link := destroyLink.FindStringSubmatch(page)
	if link == nil {
		t.Fatalf("no way to destroy it:\n%s", page)
	}
	if code := post(t, h, html(link[1]), nil, cookie).Code; code != http.StatusSeeOther {
		t.Fatalf("destroying = %d", code)
	}
	if _, err := blobs.Stat(t.Context(), "image/2026/01/01/NOROWHOLDSTHIS.jpg"); err == nil {
		t.Error("the blob survived being destroyed")
	}
}
