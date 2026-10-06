package web_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db/dbtest"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/storage/storagetest"
)

// shared posts what a share sheet posts: one file part, named as the manifest
// says, with the filename and the type a phone would declare.
func shared(t *testing.T, h http.Handler, cookie *http.Cookie, filename, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	head := make(map[string][]string)
	head["Content-Disposition"] = []string{`form-data; name="file"`}
	if filename != "" {
		// Escaped the way multipart.Writer escapes it -- backslash and quote
		// and nothing else -- so that a name a MIME header cannot carry stays
		// in the header and makes the body unreadable, which is a case here.
		escaped := strings.NewReplacer("\\", "\\\\", `"`, `\"`).Replace(filename)
		head["Content-Disposition"] = []string{`form-data; name="file"; filename="` + escaped + `"`}
	}
	if contentType != "" {
		head["Content-Type"] = []string{contentType}
	}
	part, err := w.CreatePart(head)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(part, body); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/share-target", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// month is where a share lands today, which is the folder the handler makes.
func month() string {
	now := time.Now().UTC()
	return fmt.Sprintf("shared/%04d/%02d", now.Year(), int(now.Month()))
}

// TestASharedFileLandsSomewhereAndSaysWhere: the destination is the server's,
// because a share carries none, and the answer is the folder it landed in --
// which is the page that already holds the rename this feature would otherwise
// have had to invent.
func TestASharedFileLandsSomewhereAndSaysWhere(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	cookie := signIn(t, h)

	rec := shared(t, h, cookie, "holiday.jpg", "image/jpeg", "the bytes")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("sharing a file = %d: %s", rec.Code, rec.Body)
	}
	want := "/files/" + month() + "?added=1"
	if got := rec.Header().Get("Location"); got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}
	if got := stored(t, s, month()+"/holiday.jpg"); got != "the bytes" {
		t.Errorf("what landed = %q", got)
	}
}

// TestASecondShareOfTheSameNameLandsBesideTheFirst: two photographs shared a
// minute apart are both called image.jpg, and a PUT's replace would destroy
// the first. Nobody said "replace that" here.
func TestASecondShareOfTheSameNameLandsBesideTheFirst(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	cookie := signIn(t, h)

	if rec := shared(t, h, cookie, "image.jpg", "image/jpeg", "the first"); rec.Code != http.StatusSeeOther {
		t.Fatalf("the first share = %d: %s", rec.Code, rec.Body)
	}
	if rec := shared(t, h, cookie, "image.jpg", "image/jpeg", "the second"); rec.Code != http.StatusSeeOther {
		t.Fatalf("the second share = %d: %s", rec.Code, rec.Body)
	}
	if got := stored(t, s, month()+"/image.jpg"); got != "the first" {
		t.Errorf("the first share was replaced: %q", got)
	}
	if got := stored(t, s, month()+"/image (2).jpg"); got != "the second" {
		t.Errorf("the second share did not land beside it: %q", got)
	}
}

// TestAShareWithNoNameGetsOne: a share sheet can hand over a stream and no
// filename, and the moment it arrived is the one thing that tells it from the
// next one.
func TestAShareWithNoNameGetsOne(t *testing.T) {
	t.Parallel()
	h, _ := browser(t)
	cookie := signIn(t, h)

	rec := shared(t, h, cookie, "", "image/png", "the bytes")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("sharing a nameless file = %d: %s", rec.Code, rec.Body)
	}
	listing := get(t, h, "/files/"+month(), cookie).Body.String()
	if !strings.Contains(listing, time.Now().UTC().Format("2006-01-02")) ||
		!strings.Contains(listing, ".png") {
		t.Errorf("the folder holds nothing named after when it arrived:\n%s", listing)
	}
}

// TestSharingToTheTargetNeedsASession: it is a write, so it goes through the
// gate every other write goes through. Whether a phone's share carries the
// cookie is the measurement on #312; what this pins is which gate it meets.
func TestSharingToTheTargetNeedsASession(t *testing.T) {
	t.Parallel()
	h, _ := browser(t)

	rec := shared(t, h, nil, "holiday.jpg", "image/jpeg", "the bytes")
	if rec.Code == http.StatusSeeOther && strings.Contains(rec.Header().Get("Location"), "shared/") {
		t.Fatalf("an unauthenticated share was stored: %d %s", rec.Code, rec.Header().Get("Location"))
	}
}

// TestTheManifestPointsAtTheHandler: a share target naming a path that moved
// fails on a phone and nowhere else, so the two are held together here.
func TestTheManifestPointsAtTheHandler(t *testing.T) {
	t.Parallel()
	h, _ := browser(t)
	cookie := signIn(t, h)

	var m struct {
		ShareTarget struct {
			Action  string `json:"action"`
			Method  string `json:"method"`
			Enctype string `json:"enctype"`
			Params  struct {
				Files []struct {
					Name string `json:"name"`
				} `json:"files"`
			} `json:"params"`
		} `json:"share_target"`
	}
	if err := json.Unmarshal(get(t, h, "/manifest.webmanifest").Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	target := m.ShareTarget
	if target.Method != http.MethodPost || target.Enctype != "multipart/form-data" {
		t.Errorf("share_target = %+v, want a multipart POST", target)
	}
	if len(target.Params.Files) != 1 || target.Params.Files[0].Name != "file" {
		t.Fatalf("share_target names %+v, want the field the upload form uses", target.Params.Files)
	}

	// The address it names has to be the one the handler is registered at, and
	// the only honest way to ask is to post to it.
	rec := shared(t, h, cookie, "holiday.jpg", "image/jpeg", "the bytes")
	if target.Action != "/share-target" || rec.Code != http.StatusSeeOther {
		t.Errorf("the manifest points at %q, which answered %d", target.Action, rec.Code)
	}
}

// TestWhatIsSharedHasToBeAFile: the manifest asks for multipart, and anything
// else posted at this address is the sender's mistake rather than the tree's.
func TestWhatIsSharedHasToBeAFile(t *testing.T) {
	t.Parallel()
	h, _ := browser(t)
	cookie := signIn(t, h)

	rec := post(t, h, "/share-target", url.Values{"text": {"look at this"}}, cookie)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("sharing a form with no file in it = %d, want 400", rec.Code)
	}
}

// TestSharingWhenTheStoreRefuses: it is the same write an upload makes, so a
// store that says no stops it rather than leaving a name with nothing behind
// it.
func TestSharingWhenTheStoreRefuses(t *testing.T) {
	t.Parallel()
	blobs, meta := backends(t)
	broken := files.New(storagetest.FailOn(t, blobs, "Put"), meta)
	h := handlerOver(t, broken, blobs, meta)
	cookie := signIn(t, h)

	rec := shared(t, h, cookie, "holiday.jpg", "image/jpeg", "the bytes")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("sharing into a store that refuses = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), storagetest.ErrInjected.Error()) {
		t.Error("the page carries the error, which belongs in the log")
	}
}

// TestSharingWhenTheFolderCannotBeMade: the dated folder is made on the way,
// and a database that will not say whether it is there stops the share before
// any bytes move.
func TestSharingWhenTheFolderCannotBeMade(t *testing.T) {
	t.Parallel()
	blobs, meta := backends(t)
	broken := files.New(blobs, dbtest.FailOn(t, meta, "FileByPath"))
	h := handlerOver(t, broken, blobs, meta)
	cookie := signIn(t, h)

	if rec := shared(t, h, cookie, "holiday.jpg", "image/jpeg", "the bytes"); rec.Code != http.StatusInternalServerError {
		t.Errorf("sharing where the folder cannot be made = %d, want 500", rec.Code)
	}
}

// TestAShareThatDoesNotArriveWhole: a MIME header cannot carry a control
// character, so a filename with one in it stops the body being readable before
// any of it reaches the tree — the same refusal an upload gives.
func TestAShareThatDoesNotArriveWhole(t *testing.T) {
	t.Parallel()
	h, _ := browser(t)
	cookie := signIn(t, h)

	rec := shared(t, h, cookie, "img\x01.jpg", "image/jpeg", "pixels")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a share with an unreadable header = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "did not arrive whole") {
		t.Errorf("refused, but not for that reason:\n%s", rec.Body)
	}
}
