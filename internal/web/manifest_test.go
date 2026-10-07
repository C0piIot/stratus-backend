package web_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// readManifest is what a browser reads out of it: enough to install, and every
// address it would go on to fetch.
type readManifest struct {
	Name            string   `json:"name"`
	StartURL        string   `json:"start_url"`
	Display         string   `json:"display"`
	DisplayOverride []string `json:"display_override"`
	Lang            string   `json:"lang"`
	Dir             string   `json:"dir"`
	Orientation     string   `json:"orientation"`
	Categories      []string `json:"categories"`
	LaunchHandler   struct {
		ClientMode string `json:"client_mode"`
	} `json:"launch_handler"`
	Icons []struct {
		Src     string `json:"src"`
		Sizes   string `json:"sizes"`
		Purpose string `json:"purpose"`
	} `json:"icons"`
	Screenshots []struct {
		Src        string `json:"src"`
		FormFactor string `json:"form_factor"`
		Label      string `json:"label"`
	} `json:"screenshots"`
	Shortcuts []struct {
		Name  string `json:"name"`
		URL   string `json:"url"`
		Icons []struct {
			Src   string `json:"src"`
			Sizes string `json:"sizes"`
		} `json:"icons"`
	} `json:"shortcuts"`
}

func (m readManifest) sources() []string {
	var out []string
	for _, i := range m.Icons {
		out = append(out, i.Src)
	}
	for _, s := range m.Screenshots {
		out = append(out, s.Src)
	}
	for _, s := range m.Shortcuts {
		for _, i := range s.Icons {
			out = append(out, i.Src)
		}
	}
	return out
}

// TestTheManifestAnswersWithoutASession is the assertion it exists for: a
// browser fetches a manifest with credentials omitted, so behind the login this
// would answer a redirect and the browser would quietly have no manifest at
// all. Everything it names has to answer the same way, which is why those are
// fetched here rather than trusted to be in the binary.
func TestTheManifestAnswersWithoutASession(t *testing.T) {
	t.Parallel()
	h := newHandler(t, nil)

	rec := get(t, h, "/manifest.webmanifest")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET the manifest with no session = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/manifest+json" {
		t.Errorf("Content-Type = %q, want the registered one", got)
	}

	var m readManifest
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("the manifest is not JSON: %v\n%s", err, rec.Body)
	}
	if m.Name == "" || m.StartURL != "/" || m.Display != "standalone" {
		t.Errorf("manifest = %+v, want a name, the root and a standalone display", m)
	}

	if len(m.Icons) != 2 {
		t.Fatalf("the manifest names %d icons, want 192 and 512", len(m.Icons))
	}
	for _, i := range m.Icons {
		// Both purposes, because the source is drawn for it: a launcher that
		// masks the picture and one that does not take the same file.
		if i.Purpose != "any maskable" {
			t.Errorf("the %s icon is %q, want it offered for both", i.Sizes, i.Purpose)
		}
	}

	// A set with one form factor in it is a set half of the browsers that read
	// it ignore: the wide ones are a desktop's and the narrow ones a phone's.
	wide, narrow := 0, 0
	for _, s := range m.Screenshots {
		switch s.FormFactor {
		case "wide":
			wide++
		case "narrow":
			narrow++
		default:
			t.Errorf("screenshot %s has form factor %q", s.Src, s.FormFactor)
		}
		if s.Label == "" {
			t.Errorf("screenshot %s has nothing to read out", s.Src)
		}
	}
	if wide == 0 || narrow == 0 {
		t.Errorf("screenshots: %d wide and %d narrow, want both kinds", wide, narrow)
	}

	for _, src := range m.sources() {
		rec := get(t, h, src)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s with no session = %d, which is what a browser would get", src, rec.Code)
			continue
		}
		if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "image/") {
			t.Errorf("GET %s answered %q, want a picture", src, got)
		}
	}
}

// TestTheShortcutsGoSomewhere: a launcher offers the four libraries, and a
// shortcut to an address that moved is the failure nobody sees -- so each one
// is followed here, signed in, and has to be a page rather than a 404 (#311).
func TestTheShortcutsGoSomewhere(t *testing.T) {
	t.Parallel()
	h := newHandler(t, nil)
	cookie := signIn(t, h)

	var m readManifest
	if err := json.Unmarshal(get(t, h, "/manifest.webmanifest").Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Shortcuts) != 4 {
		t.Fatalf("the manifest offers %d shortcuts, want the four libraries", len(m.Shortcuts))
	}
	for _, s := range m.Shortcuts {
		if s.Name == "" {
			t.Errorf("the shortcut to %s has no name", s.URL)
		}
		if len(s.Icons) != 1 {
			t.Errorf("the shortcut to %s offers %d glyphs, want the one drawn for it", s.URL, len(s.Icons))
		}
		if rec := get(t, h, s.URL, cookie); rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want the library it names", s.URL, rec.Code)
		}
	}
}

// TestWhatAnInstallerIsTold: the members that say what this is and how it
// should open. The one with an argument behind it is the order of
// display_override -- standalone first, because it is the window this UI is
// laid out for, and the two behind it are declared rather than used.
func TestWhatAnInstallerIsTold(t *testing.T) {
	t.Parallel()
	h := newHandler(t, nil)

	var m readManifest
	if err := json.Unmarshal(get(t, h, "/manifest.webmanifest").Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m.Lang != "en" || m.Dir != "ltr" {
		t.Errorf("lang/dir = %q/%q, want the language this UI is written in", m.Lang, m.Dir)
	}
	if m.Orientation != "any" {
		t.Errorf("orientation = %q: a file browser reads both ways round", m.Orientation)
	}
	if len(m.Categories) == 0 {
		t.Error("the manifest files this under nothing")
	}
	if m.LaunchHandler.ClientMode != "navigate-existing" {
		t.Errorf("client_mode = %q, want one window", m.LaunchHandler.ClientMode)
	}
	if len(m.DisplayOverride) == 0 || m.DisplayOverride[0] != "standalone" {
		t.Errorf("display_override = %v, want the mode this UI is laid out for first", m.DisplayOverride)
	}
	if m.DisplayOverride[0] != m.Display {
		t.Errorf("display_override starts at %q and display is %q, which are two answers to one question",
			m.DisplayOverride[0], m.Display)
	}
}

// TestThePagesLinkTheManifest, and the icon iOS reads instead of one.
func TestThePagesLinkTheManifest(t *testing.T) {
	t.Parallel()
	h := newHandler(t, nil)

	body := get(t, h, "/login").Body.String()
	for _, want := range []string{
		`<link rel="manifest" href="/manifest.webmanifest">`,
		`<link rel="apple-touch-icon" href="/static/stratus/apple-touch-icon.png?v=`,
		`<meta name="theme-color" content="#ffffff">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the layout lacks %s", want)
		}
	}
	if rec := get(t, h, "/static/stratus/apple-touch-icon.png?v="+version); rec.Code != http.StatusOK {
		t.Errorf("GET the touch icon = %d, want 200", rec.Code)
	}
}
