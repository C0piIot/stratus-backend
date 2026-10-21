package web_test

import (
	htmlstd "html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/auth"
	"github.com/C0piIot/stratus-backend/internal/calendar"
	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/db/dbtest"
	"github.com/C0piIot/stratus-backend/internal/settings"
	"github.com/C0piIot/stratus-backend/internal/web"
)

// settingsHandler is the UI with a settings page over a real service, the
// environment fixing what fixed says.
func settingsHandler(t *testing.T, fixed map[string]string, failing ...string) (http.Handler, *settings.Service) {
	t.Helper()
	s, thumbs, meta := pieces(t)
	var store db.Settings = meta
	for _, method := range failing {
		store = dbtest.FailOn(t, meta, method)
	}
	editor, err := settings.New(t.Context(), func(key string) string { return fixed[key] }, store)
	if err != nil {
		t.Fatal(err)
	}
	creds := credentials()
	return web.Handler(web.Deps{
		Version: version, BuildDate: buildDate, Verifier: creds,
		Sessions: auth.NewSessions(creds, auth.DefaultSessionTTL), Shares: auth.NewShares(creds),
		Files: s, Thumbs: thumbs, Index: meta, Playlists: playlistsOf(meta), Indexing: indexing(meta),
		Video: web.Video{}, TrashRetention: aMonth, Settings: editor.Values, Editor: editor,
		Calendars: calendar.New(meta),
	}), editor
}

func TestTheSettingsNeedASession(t *testing.T) {
	t.Parallel()
	h, _ := settingsHandler(t, nil)
	if rec := get(t, h, "/settings"); rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/login") {
		t.Errorf("/settings with no session = %d", rec.Code)
	}
}

// TestTheSettingsPage is #397's page: every setting under its heading, what
// the environment fixes shown and not offered, a value saved and in force at
// once, and an empty field putting the default back.
func TestTheSettingsPage(t *testing.T) {
	t.Parallel()
	h, s := settingsHandler(t, map[string]string{"STRATUS_GC_INTERVAL": "2h"})
	cookie := signIn(t, h)

	page := get(t, h, "/settings", cookie)
	if page.Code != http.StatusOK {
		t.Fatalf("/settings = %d", page.Code)
	}
	body := htmlstd.UnescapeString(page.Body.String())
	for _, want := range []string{
		"Trash and sweep", "Calendar", `name="TZ"`, `placeholder="UTC"`,
		`id="STRATUS_GC_INTERVAL" type="text" value="2h" disabled`, "set by the environment",
		`<option value="" selected>default (off)</option>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page has no %q", want)
		}
	}
	if strings.Contains(body, `name="STRATUS_GC_INTERVAL"`) {
		t.Error("a setting the environment fixes is offered as a field")
	}

	rec := post(t, h, "/settings", url.Values{
		"TZ": {"Europe/Madrid"}, "STRATUS_MUSIC_LOOKUP": {"on"}, "STRATUS_GC_INTERVAL": {"1m"}, "NOT_A_SETTING": {"x"},
	}, cookie)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/settings?saved" {
		t.Fatalf("saving = %d to %q", rec.Code, rec.Header().Get("Location"))
	}
	v := s.Values()
	if v.Zone.String() != "Europe/Madrid" || !v.MusicLookup || v.GCInterval != 2*time.Hour {
		t.Errorf("after saving the values are %+v", v)
	}
	saved := htmlstd.UnescapeString(get(t, h, "/settings?saved", cookie).Body.String())
	for _, want := range []string{"Saved.", `value="Europe/Madrid"`, `<option value="on" selected>on</option>`} {
		if !strings.Contains(saved, want) {
			t.Errorf("the saved page has no %q", want)
		}
	}

	// Clearing a field is the default again.
	if rec := post(t, h, "/settings", url.Values{"TZ": {""}, "STRATUS_MUSIC_LOOKUP": {"on"}}, cookie); rec.Code != http.StatusSeeOther {
		t.Fatalf("clearing = %d", rec.Code)
	}
	if got := s.Values().Zone.String(); got != "UTC" || !s.Values().MusicLookup {
		t.Errorf("after clearing the zone is %s and lookup %v", got, s.Values().MusicLookup)
	}
}

// TestAFormWithAMistakeChangesNothing: every value is read before any is
// stored, so one that does not parse leaves the rest where they were.
func TestAFormWithAMistakeChangesNothing(t *testing.T) {
	t.Parallel()
	h, s := settingsHandler(t, nil)
	cookie := signIn(t, h)

	rec := post(t, h, "/settings", url.Values{"TZ": {"Europe/Madrid"}, "STRATUS_TRASH_RETENTION": {"1h"}}, cookie)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a form with a mistake = %d, want 400", rec.Code)
	}
	body := htmlstd.UnescapeString(rec.Body.String())
	for _, want := range []string{"Nothing was saved", "must be at least 24h", `value="1h"`, `value="Europe/Madrid"`, "is-invalid"} {
		if !strings.Contains(body, want) {
			t.Errorf("the refusal has no %q", want)
		}
	}
	if got := s.Values().Zone.String(); got != "UTC" {
		t.Errorf("a refused form stored the zone %s", got)
	}
}

// TestAPageReadsTheSettingInForce: a change on the settings page is on the
// next page, with no restart -- the status page's switches and the trash's
// retention among them.
func TestAPageReadsTheSettingInForce(t *testing.T) {
	t.Parallel()
	h, s := settingsHandler(t, nil)
	cookie := signIn(t, h)

	status := func() string { return get(t, h, "/status", cookie).Body.String() }
	if strings.Contains(status(), "Indexing is off") || strings.Contains(status(), "data-trash-held") {
		t.Fatal("the status page says something is off before anything was changed")
	}
	for key, value := range map[string]string{
		"STRATUS_INDEX_INTERVAL": "0", "STRATUS_TRASH_RETENTION": "never", "TZ": "Europe/Madrid",
	} {
		if err := s.Set(t.Context(), key, value); err != nil {
			t.Fatal(err)
		}
	}
	body := status()
	for _, want := range []string{"Indexing is off", "data-trash-held"} {
		if !strings.Contains(body, want) {
			t.Errorf("after the change the status page has no %q", want)
		}
	}
	if month := get(t, h, "/calendars/edu/default/?month=2024-01", cookie).Body.String(); !strings.Contains(month, "times in CE") {
		t.Error("the calendar is not drawn in the zone the setting names")
	}
}

func TestASettingThatCannotBeStored(t *testing.T) {
	t.Parallel()
	h, _ := settingsHandler(t, nil, "PutSetting")
	cookie := signIn(t, h)
	if rec := post(t, h, "/settings", url.Values{"TZ": {"Europe/Madrid"}}, cookie); rec.Code != http.StatusInternalServerError {
		t.Errorf("saving to a store that refuses = %d, want 500", rec.Code)
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/settings", strings.NewReader("TZ=%zz"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a form that does not parse = %d, want 400", rec.Code)
	}
}
