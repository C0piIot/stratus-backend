package web_test

import (
	"strings"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/auth"
	"github.com/C0piIot/stratus-backend/internal/calendar"
	"github.com/C0piIot/stratus-backend/internal/config"
	"github.com/C0piIot/stratus-backend/internal/web"
)

// TestAPageAsksTheSettingsEachTime is #397 from the pages' side: what they say
// about the indexer, the trash and the calendar's zone is the setting in force
// when the page is drawn, not what the handler was built with.
func TestAPageAsksTheSettingsEachTime(t *testing.T) {
	t.Parallel()
	s, thumbs, meta := pieces(t)
	creds := credentials()
	madrid, err := time.LoadLocation("Europe/Madrid")
	if err != nil {
		t.Fatal(err)
	}
	now := config.Values{IndexInterval: time.Hour, ThumbsAhead: true, TrashRetention: aMonth, Zone: time.UTC}
	h := web.Handler(web.Deps{
		Version: version, BuildDate: buildDate, Verifier: creds,
		Sessions: auth.NewSessions(creds, auth.DefaultSessionTTL), Shares: auth.NewShares(creds),
		Files: s, Thumbs: thumbs, Index: meta, Playlists: playlistsOf(meta), Indexing: indexing(meta),
		Playback: web.Playback{}, TrashRetention: aMonth, Calendars: calendar.New(meta),
		Settings: func() config.Values { return now },
	})
	cookie := signIn(t, h)

	status := get(t, h, "/status", cookie).Body.String()
	if strings.Contains(status, "Indexing is off") || strings.Contains(status, "data-trash-held") {
		t.Fatal("the status page says something is off before anything changed")
	}
	if !strings.Contains(get(t, h, "/calendars/edu/default/?month=2024-01", cookie).Body.String(), "times in UTC") {
		t.Error("the calendar is not drawn in the zone in force")
	}

	now = config.Values{IndexInterval: 0, TrashRetention: 0, Zone: madrid}
	status = get(t, h, "/status", cookie).Body.String()
	for _, want := range []string{"Indexing is off", "data-trash-held"} {
		if !strings.Contains(status, want) {
			t.Errorf("after the change the status page has no %q", want)
		}
	}
	if !strings.Contains(get(t, h, "/calendars/edu/default/?month=2024-01", cookie).Body.String(), "times in CE") {
		t.Error("the calendar did not follow the zone to Madrid")
	}
}
