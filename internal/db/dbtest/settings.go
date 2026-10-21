package dbtest

import (
	"maps"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// RunSettings executes the settings cases against the repository built by
// newRepo.
func RunSettings(t *testing.T, newRepo func(t *testing.T) db.Repo) {
	t.Helper()

	cases := []struct {
		name string
		fn   func(t *testing.T, s db.Repo)
	}{
		{"nothing stored is an empty map", settingsEmpty},
		{"a setting comes back as it was stored, and a second write replaces it", settingsRoundTrip},
		{"forgetting a setting forgets that one, and one never stored is no error", settingsDelete},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.fn(t, newRepo(t))
		})
	}
}

var settingAt = time.Date(2024, 9, 1, 20, 0, 0, 0, time.UTC)

func settingsOf(t *testing.T, s db.Repo) map[string]string {
	t.Helper()
	got, err := s.Settings(t.Context())
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	return got
}

func settingsEmpty(t *testing.T, s db.Repo) {
	if got := settingsOf(t, s); len(got) != 0 {
		t.Errorf("Settings = %v, want none", got)
	}
}

func settingsRoundTrip(t *testing.T, s db.Repo) {
	for key, value := range map[string]string{"STRATUS_GC_INTERVAL": "12h", "TZ": "Europe/Madrid", "LONG": "a value with spaces, commas, and ünïcödé"} {
		if err := s.PutSetting(t.Context(), key, value, settingAt); err != nil {
			t.Fatalf("PutSetting(%s): %v", key, err)
		}
	}
	if err := s.PutSetting(t.Context(), "TZ", "UTC", settingAt.Add(time.Hour)); err != nil {
		t.Fatalf("PutSetting again: %v", err)
	}
	want := map[string]string{"STRATUS_GC_INTERVAL": "12h", "TZ": "UTC", "LONG": "a value with spaces, commas, and ünïcödé"}
	if got := settingsOf(t, s); !maps.Equal(got, want) {
		t.Errorf("Settings = %v, want %v", got, want)
	}
}

func settingsDelete(t *testing.T, s db.Repo) {
	for _, key := range []string{"A", "B"} {
		if err := s.PutSetting(t.Context(), key, "x", settingAt); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeleteSetting(t.Context(), "A"); err != nil {
		t.Fatalf("DeleteSetting: %v", err)
	}
	if err := s.DeleteSetting(t.Context(), "never stored"); err != nil {
		t.Errorf("DeleteSetting of nothing = %v", err)
	}
	if got := settingsOf(t, s); !maps.Equal(got, map[string]string{"B": "x"}) {
		t.Errorf("Settings = %v, want B alone", got)
	}
}
