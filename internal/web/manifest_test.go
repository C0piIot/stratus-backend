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
	Name     string `json:"name"`
	StartURL string `json:"start_url"`
	Display  string `json:"display"`
	Icons    []struct {
		Src     string `json:"src"`
		Sizes   string `json:"sizes"`
		Purpose string `json:"purpose"`
	} `json:"icons"`
	Screenshots []struct {
		Src        string `json:"src"`
		FormFactor string `json:"form_factor"`
		Label      string `json:"label"`
	} `json:"screenshots"`
}

func (m readManifest) sources() []string {
	var out []string
	for _, i := range m.Icons {
		out = append(out, i.Src)
	}
	for _, s := range m.Screenshots {
		out = append(out, s.Src)
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
