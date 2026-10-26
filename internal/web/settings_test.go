package web_test

import (
	htmlstd "html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stratus-private-cloud/stratus-backend/internal/auth"
	"github.com/stratus-private-cloud/stratus-backend/internal/calendar"
	"github.com/stratus-private-cloud/stratus-backend/internal/db"
	"github.com/stratus-private-cloud/stratus-backend/internal/db/dbtest"
	"github.com/stratus-private-cloud/stratus-backend/internal/settings"
	"github.com/stratus-private-cloud/stratus-backend/internal/web"
)

// settingsHandler is the UI with a settings page over a real service, the
// environment fixing what fixed says.
func settingsHandler(t *testing.T, fixed map[string]string, failing ...string) (http.Handler, *settings.Service) {
	t.Helper()
	return settingsHandlerWith(t, fixed, nil, failing...)
}

func settingsHandlerWith(t *testing.T, fixed map[string]string, imports web.Imports, failing ...string) (http.Handler, *settings.Service) {
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
		Playback: web.Playback{}, TrashRetention: aMonth, Settings: editor.Values, Editor: editor, Imports: imports,
		Zones:     []string{"America/New_York", "Europe/Madrid", "UTC"},
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
		"Trash and sweep", "Calendar", `name="TZ"`, `<option value="Europe/Madrid">Europe/Madrid</option>`,
		`name="STRATUS_WEEK_START"`, `<option value="" selected>default (monday)</option>`,
		`id="STRATUS_GC_INTERVAL" type="text" value="2h" placeholder="not set" readonly disabled`, "set by the environment",
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
	for _, want := range []string{"Settings saved.", `<option value="Europe/Madrid" selected>Europe/Madrid</option>`, `<option value="on" selected>on</option>`} {
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
	for _, want := range []string{"Nothing was saved", "must be at least 24h", `value="1h"`, `<option value="Europe/Madrid" selected>`, "is-invalid"} {
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

// TestThePasswordIsShownAndNotGivenAway: the page says where the password is
// changed, and neither its value nor its length is in what it sends.
func TestThePasswordIsShownAndNotGivenAway(t *testing.T) {
	t.Parallel()
	h, _ := settingsHandler(t, nil)
	cookie := signIn(t, h)
	body := get(t, h, "/settings", cookie).Body.String()

	if !strings.Contains(body, `id="STRATUS_PASSWORD" type="password" value="************" readonly disabled`) {
		t.Errorf("the page does not show the password as fixed:\n%s", body)
	}
	if strings.Contains(body, examplePassword) {
		t.Error("the page carries the password")
	}
	if strings.Contains(body, `name="STRATUS_PASSWORD"`) {
		t.Error("the password is a field the form would post")
	}
}

// TestTheImportFolderIsShownAndNotOffered: its path is on the page, read-only,
// and not a field the form posts -- set or not.
func TestTheImportFolderIsShownAndNotOffered(t *testing.T) {
	t.Parallel()
	h, _ := settingsHandler(t, nil)
	cookie := signIn(t, h)
	body := get(t, h, "/settings", cookie).Body.String()

	for _, want := range []string{`id="STRATUS_INCOMING_DIR" type="text" value="" placeholder="not set" readonly disabled`} {
		if !strings.Contains(body, want) {
			t.Errorf("the page has no %q", want)
		}
	}
	if strings.Contains(body, `name="STRATUS_INCOMING_DIR"`) {
		t.Error("the import folder is a field the form would post")
	}
	if strings.Index(body, "STRATUS_INCOMING_DIR") > strings.Index(body, "STRATUS_INCOMING_INTERVAL") {
		t.Error("the folder is not above its interval")
	}
}

func TestASetImportFolderIsShownAsItIs(t *testing.T) {
	t.Parallel()
	h, _ := settingsHandlerWith(t, nil, stuck{dir: "/srv/scanner"})
	cookie := signIn(t, h)
	if body := get(t, h, "/settings", cookie).Body.String(); !strings.Contains(body, `id="STRATUS_INCOMING_DIR" type="text" value="/srv/scanner"`) {
		t.Errorf("the page does not show the import folder:\n%s", body)
	}
}

// TestAZoneThisMachineDoesNotKnowIsStillOffered: a stored zone missing from
// the list is added to it, or the next save would put the default back.
func TestAZoneThisMachineDoesNotKnowIsStillOffered(t *testing.T) {
	t.Parallel()
	h, s := settingsHandler(t, nil)
	cookie := signIn(t, h)
	if err := s.Set(t.Context(), "TZ", "Asia/Tokyo"); err != nil {
		t.Fatal(err)
	}
	if body := get(t, h, "/settings", cookie).Body.String(); !strings.Contains(body, `<option value="Asia/Tokyo" selected>Asia/Tokyo</option>`) {
		t.Error("a stored zone the list does not have is not offered")
	}
}

// TestTheWeekStartsWhereTheSettingSays: Sunday first, the grid and its
// headings both.
func TestTheWeekStartsWhereTheSettingSays(t *testing.T) {
	t.Parallel()
	h, s := settingsHandler(t, nil)
	cookie := signIn(t, h)
	month := func() string { return get(t, h, "/calendars/edu/default/?month=2024-03", cookie).Body.String() }

	// March 2024 begins on a Friday: Monday-first draws from the 26th of
	// February, Sunday-first from the 25th.
	// The first 25 drawn is February's only when the grid reaches back to it.
	startsOn25th := func(body string) bool {
		return strings.Index(body, ">25</a>") < strings.Index(body, ">1</a>")
	}
	if body := month(); !strings.Contains(body, ">Mon</div>") ||
		strings.Index(body, ">Mon</div>") > strings.Index(body, ">Sun</div>") || startsOn25th(body) {
		t.Error("the week does not start on Monday by default")
	}
	if err := s.Set(t.Context(), "STRATUS_WEEK_START", "sunday"); err != nil {
		t.Fatal(err)
	}
	body := month()
	if strings.Index(body, ">Sun</div>") > strings.Index(body, ">Mon</div>") {
		t.Error("the headings do not start on Sunday")
	}
	if !startsOn25th(body) {
		t.Error("the grid does not start on Sunday the 25th of February")
	}
}

// A path inside the tree is written after /files/, which the page shows in
// front of the field so nobody types it twice.
func TestTheBackupFolderIsWrittenUnderFiles(t *testing.T) {
	t.Parallel()
	h, _ := settingsHandler(t, nil)
	body := get(t, h, "/settings", signIn(t, h)).Body.String()
	want := `<span class="input-group-text">/files/</span>
              <input class="form-control" id="STRATUS_BACKUP_DIR"`
	if !strings.Contains(body, want) {
		t.Errorf("the backup folder is not shown under /files/:\n%s", between(body, "STRATUS_BACKUP_DIR"))
	}
}
