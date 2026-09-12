package web_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// newFolder posts the form the listing shows, into the directory named by the
// URL.
func newFolder(t *testing.T, h http.Handler, in string, cookie *http.Cookie, name string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"name": {name}}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/folders/"+in,
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestNewFolder(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	cookie := signIn(t, h)

	rec := newFolder(t, h, "", cookie, "photos")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("making a folder = %d, want 303", rec.Code)
	}
	// Into it, which is where somebody who just made one is going.
	if got := rec.Header().Get("Location"); got != "/files/photos" {
		t.Errorf("Location = %q, want the new folder", got)
	}

	f, err := s.Stat(t.Context(), username, "photos")
	if err != nil {
		t.Fatalf("the folder is not in the tree: %v", err)
	}
	if !f.IsDir {
		t.Error("what was made is not a directory")
	}
	if !strings.Contains(get(t, h, "/files/", cookie).Body.String(), ">photos/<") {
		t.Error("the new folder is not in the listing it was made from")
	}
}

func TestNewFolderDeeper(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	mkdir(t, s, "photos")
	cookie := signIn(t, h)

	rec := newFolder(t, h, "photos", cookie, "2026")
	if got := rec.Header().Get("Location"); got != "/files/photos/2026" {
		t.Fatalf("Location = %q", got)
	}
	if _, err := s.Stat(t.Context(), username, "photos/2026"); err != nil {
		t.Errorf("photos/2026 is not there: %v", err)
	}

	// And it is somewhere to upload into, which is the point of having it.
	if rec := upload(t, h, "/files/photos/2026", cookie, "img.jpg", "pixels"); rec.Code != http.StatusSeeOther {
		t.Errorf("uploading into the new folder = %d", rec.Code)
	}
	if got := stored(t, s, "photos/2026/img.jpg"); got != "pixels" {
		t.Errorf("the file in the new folder holds %q", got)
	}
}

// TestNewFolderNamesThatAreNotNames: the name is whatever was typed into a text
// box, so it is one element of a path at most.
func TestNewFolderNamesThatAreNotNames(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	mkdir(t, s, "photos")
	cookie := signIn(t, h)

	tests := []struct {
		name string
		sent string
		want string // where it must land, or "" when it must be refused
		code int
	}{
		{name: "a plain name", sent: "holiday", want: "photos/holiday", code: http.StatusSeeOther},
		{name: "a name with spaces", sent: "the holiday", want: "photos/the holiday", code: http.StatusSeeOther},
		{name: "a path", sent: "a/b/c", want: "photos/c", code: http.StatusSeeOther},
		{name: "climbing out", sent: "../../elsewhere", want: "photos/elsewhere", code: http.StatusSeeOther},
		{name: "nothing at all", code: http.StatusBadRequest},
		{name: "nothing but dots", sent: "..", code: http.StatusBadRequest},
		{name: "a slash", sent: "/", code: http.StatusBadRequest},
		{name: "a control character", sent: "a\x01b", code: http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := newFolder(t, h, "photos", cookie, tt.sent)
			if rec.Code != tt.code {
				t.Fatalf("a folder called %q = %d, want %d", tt.sent, rec.Code, tt.code)
			}
			if tt.want == "" {
				return
			}
			if _, err := s.Stat(t.Context(), username, tt.want); err != nil {
				t.Errorf("%q did not land at %q: %v", tt.sent, tt.want, err)
			}
		})
	}
}

func TestNewFolderRefuses(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	mkdir(t, s, "photos")
	write(t, s, "notes.txt", "hello")
	cookie := signIn(t, h)

	t.Run("a name already taken", func(t *testing.T) {
		t.Parallel()
		if rec := newFolder(t, h, "", cookie, "photos"); rec.Code != http.StatusConflict {
			t.Errorf("making a folder that is already there = %d, want 409", rec.Code)
		}
		// Including by a file, which is the case that would otherwise leave two
		// rows at one path.
		if rec := newFolder(t, h, "", cookie, "notes.txt"); rec.Code != http.StatusConflict {
			t.Errorf("making a folder over a file = %d, want 409", rec.Code)
		}
	})

	// 404 rather than 409: nothing is in the way, there is simply no such page
	// to have posted the form from.
	t.Run("inside somewhere that is not there", func(t *testing.T) {
		t.Parallel()
		if rec := newFolder(t, h, "no/such/place", cookie, "holiday"); rec.Code != http.StatusNotFound {
			t.Errorf("making a folder with no parent = %d, want 404", rec.Code)
		}
	})

	t.Run("with no session", func(t *testing.T) {
		t.Parallel()
		rec := newFolder(t, h, "", nil, "holiday")
		if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/login") {
			t.Errorf("making a folder with no session = %d to %q, want the login form",
				rec.Code, rec.Header().Get("Location"))
		}
	})

	t.Run("from somebody else's page", func(t *testing.T) {
		t.Parallel()
		form := url.Values{"name": {"holiday"}}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/folders/",
			strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("a cross-site folder = %d, want 403", rec.Code)
		}
	})
}

// TestTheListingOffersBothForms: the page has to post to the directory it is
// showing, or the folder and the files land somewhere else entirely.
func TestTheListingOffersBothForms(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	mkdir(t, s, "odd names")
	cookie := signIn(t, h)

	body := get(t, h, "/files/odd%20names", cookie).Body.String()
	for _, want := range []string{`action="/files/odd%20names"`, `action="/folders/odd%20names"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the listing has no form posting to %s", want)
		}
	}
}
