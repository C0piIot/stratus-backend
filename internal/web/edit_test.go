package web_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// TestRenameAFolderWithSomethingInIt is the limitation worth being honest
// about: the metadata port refuses to move a directory that still has anything
// in it, because that is a rewrite of every descendant. The page has to say
// what actually happened rather than "something is in the way".
func TestRenameAFolderWithSomethingInIt(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	mkdir(t, s, "photos")
	write(t, s, "photos/img.jpg", "pixels")
	cookie := signIn(t, h)

	rec := rename(t, h, "photos", cookie, "holiday")
	if rec.Code != http.StatusConflict {
		t.Fatalf("renaming a folder with a file in it = %d, want 409", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "cannot be renamed yet") {
		t.Errorf("the page does not say why: %s", excerpt(rec.Body.String(), "<p"))
	}
	// And nothing moved.
	if _, err := s.Stat(t.Context(), username, "photos/img.jpg"); err != nil {
		t.Errorf("the folder did not survive the refusal: %v", err)
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

		h := handlerOver(t, files.New(blobs, dbtest.FailOn(t, meta, "DeleteFile")))
		if got := remove(t, h, "notes.txt", signIn(t, h)).Code; got != http.StatusInternalServerError {
			t.Errorf("deleting through a database that refuses = %d, want 500", got)
		}
		// And it is still there, which is the half that matters.
		if _, err := working.Stat(t.Context(), username, "notes.txt"); err != nil {
			t.Errorf("the file went anyway: %v", err)
		}
	})

	t.Run("a rename that cannot look inside", func(t *testing.T) {
		t.Parallel()
		blobs, meta := backends(t)
		working := files.New(blobs, meta)
		mkdir(t, working, "photos")

		h := handlerOver(t, files.New(blobs, dbtest.FailOn(t, meta, "ListFiles")))
		if got := rename(t, h, "photos", signIn(t, h), "holiday").Code; got != http.StatusInternalServerError {
			t.Errorf("renaming a folder it cannot read = %d, want 500", got)
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

	// Asked first: there is no trash bin, so the page in between is the only
	// chance to have not meant it.
	form := get(t, h, "/delete/notes.txt", cookie)
	if form.Code != http.StatusOK {
		t.Fatalf("the delete page = %d, want 200", form.Code)
	}
	body := form.Body.String()
	if !strings.Contains(body, "cannot be undone") {
		t.Error("the page does not say that it is permanent")
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
