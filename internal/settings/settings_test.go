package settings_test

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/config"
	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/db/dbtest"
	"github.com/C0piIot/stratus-backend/internal/db/sqlite"
	"github.com/C0piIot/stratus-backend/internal/settings"
)

func store(t *testing.T) db.Store {
	t.Helper()
	s, err := sqlite.New(t.Context(), filepath.Join(t.TempDir(), "stratus.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return s
}

func env(vars map[string]string) func(string) string {
	return func(key string) string { return vars[key] }
}

func entry(t *testing.T, s *settings.Service, key string) settings.Entry {
	t.Helper()
	for _, e := range s.Entries() {
		if e.Key == key {
			return e
		}
	}
	t.Fatalf("no entry for %s", key)
	return settings.Entry{}
}

// TestTheEnvironmentWins is the decision #397 took: the environment's value
// when it has one, what was stored when it does not, the default otherwise --
// and a row stored before the variable was set is kept, ignored, and back the
// day the variable goes.
func TestTheEnvironmentWins(t *testing.T) {
	t.Parallel()
	st := store(t)
	ctx := t.Context()
	for key, value := range map[string]string{"STRATUS_GC_INTERVAL": "6h", "STRATUS_TRASH_RETENTION": "48h"} {
		if err := st.PutSetting(ctx, key, value, time.Now()); err != nil {
			t.Fatal(err)
		}
	}

	s, err := settings.New(ctx, env(map[string]string{"STRATUS_GC_INTERVAL": "2h"}), st)
	if err != nil {
		t.Fatal(err)
	}
	v := s.Values()
	if v.GCInterval != 2*time.Hour || v.TrashRetention != 48*time.Hour || v.IndexInterval != config.DefaultIndexInterval {
		t.Errorf("Values = %+v", v)
	}
	for key, want := range map[string]settings.Source{
		"STRATUS_GC_INTERVAL": settings.FromEnvironment, "STRATUS_TRASH_RETENTION": settings.FromStore,
		"STRATUS_INDEX_INTERVAL": settings.FromDefault,
	} {
		if got := entry(t, s, key).Source; got != want {
			t.Errorf("%s comes from %s, want %s", key, got, want)
		}
	}
	if len(s.Entries()) != len(config.Settings) {
		t.Errorf("%d entries, want one per setting", len(s.Entries()))
	}

	if err = s.Set(ctx, "STRATUS_GC_INTERVAL", "1h"); !errors.Is(err, settings.ErrFixed) {
		t.Errorf("Set of a setting the environment fixes = %v, want ErrFixed", err)
	}
	unset, err := settings.New(ctx, env(nil), st)
	if err != nil {
		t.Fatal(err)
	}
	if got := unset.Values().GCInterval; got != 6*time.Hour {
		t.Errorf("with the variable gone the stored value is %v, want 6h back", got)
	}
}

func TestASettingIsChangedAndPutBack(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, err := settings.New(ctx, env(nil), store(t))
	if err != nil {
		t.Fatal(err)
	}
	changed := s.Changed()

	if err = s.Set(ctx, "TZ", "Europe/Madrid"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
	default:
		t.Error("a change did not close the channel")
	}
	if got := s.Values().Zone.String(); got != "Europe/Madrid" {
		t.Errorf("Zone = %s", got)
	}
	if e := entry(t, s, "TZ"); e.Value != "Europe/Madrid" || e.Source != settings.FromStore {
		t.Errorf("the entry = %+v", e)
	}

	again := s.Changed()
	if again == changed {
		t.Error("Changed handed back the channel it had already closed")
	}
	if err = s.Set(ctx, "TZ", ""); err != nil {
		t.Fatal(err)
	}
	if got := s.Values().Zone.String(); got != "UTC" || entry(t, s, "TZ").Source != settings.FromDefault {
		t.Errorf("after setting nothing the zone is %s from %s, want the default", got, entry(t, s, "TZ").Source)
	}
	select {
	case <-again:
	default:
		t.Error("a reset did not close the channel")
	}
}

// TestAValueIsReadAsTheEnvironmentWouldReadIt is the same parser both ways:
// what config.Load would refuse at startup, the page cannot store.
func TestAValueIsReadAsTheEnvironmentWouldReadIt(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, err := settings.New(ctx, env(nil), store(t))
	if err != nil {
		t.Fatal(err)
	}
	for key, raw := range map[string]string{
		"STRATUS_GC_GRACE": "0s", "STRATUS_TRASH_RETENTION": "1h", "STRATUS_VIDEO_TRANSCODE": "maybe",
		"STRATUS_MUSIC_LOOKUP": "yes", "TZ": "Mars/Olympus", "STRATUS_LOG_LEVEL": "loud", "STRATUS_INDEX_INTERVAL": "-1m",
	} {
		if err := s.Set(ctx, key, raw); err == nil {
			t.Errorf("Set(%s, %q) was taken", key, raw)
		}
	}
	for _, key := range []string{"STRATUS_DB_DSN", "STRATUS_INCOMING_DIR", "STRATUS_PASSWORD"} {
		if err := s.Set(ctx, key, "x"); !errors.Is(err, settings.ErrUnknown) {
			t.Errorf("Set(%s) = %v, want ErrUnknown", key, err)
		}
		if err := s.Reset(ctx, key); !errors.Is(err, settings.ErrUnknown) {
			t.Errorf("Reset(%s) = %v, want ErrUnknown", key, err)
		}
	}
}

func TestAStoredValueThatNoLongerParsesIsPassedOver(t *testing.T) {
	t.Parallel()
	st := store(t)
	if err := st.PutSetting(t.Context(), "STRATUS_VIDEO_TRANSCODE", "sometimes", time.Now()); err != nil {
		t.Fatal(err)
	}
	s, err := settings.New(t.Context(), env(nil), st)
	if err != nil {
		t.Fatal(err)
	}
	if e := entry(t, s, "STRATUS_VIDEO_TRANSCODE"); e.Value != config.VideoTranscodeAuto || e.Source != settings.FromDefault {
		t.Errorf("the entry = %+v, want the default", e)
	}

	if _, err := settings.New(t.Context(), env(map[string]string{"STRATUS_GC_GRACE": "0s"}), st); err == nil {
		t.Error("New took an environment config.Load would refuse")
	}
}

func TestTheStoreFailing(t *testing.T) {
	t.Parallel()
	st := store(t)
	ctx := t.Context()
	if _, err := settings.New(ctx, env(nil), dbtest.FailOn(t, st, "Settings")); !errors.Is(err, dbtest.ErrInjected) {
		t.Errorf("New over a store that cannot read = %v", err)
	}
	for method, call := range map[string]func(*settings.Service) error{
		"PutSetting":    func(s *settings.Service) error { return s.Set(ctx, "TZ", "UTC") },
		"DeleteSetting": func(s *settings.Service) error { return s.Reset(ctx, "TZ") },
	} {
		s, err := settings.New(ctx, env(nil), dbtest.FailOn(t, st, method))
		if err != nil {
			t.Fatal(err)
		}
		before := s.Changed()
		if err := call(s); !errors.Is(err, dbtest.ErrInjected) {
			t.Errorf("%s failing = %v", method, err)
		}
		select {
		case <-before:
			t.Errorf("%s failing still announced a change", method)
		default:
		}
	}
	s, err := settings.New(ctx, env(nil), dbtest.FailAfter(t, st, "Settings", 1))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set(ctx, "TZ", "UTC"); !errors.Is(err, dbtest.ErrInjected) {
		t.Errorf("a change whose reread fails = %v", err)
	}
}
