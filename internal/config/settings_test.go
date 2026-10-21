package config_test

import (
	"testing"

	"github.com/C0piIot/stratus-backend/internal/config"
)

// TestEverySettingHasADefaultThatParses is the list held to itself: a default
// that its own parser refused would be a server that will not start with
// nothing configured.
func TestEverySettingHasADefaultThatParses(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, s := range config.Settings {
		var v config.Values
		if err := s.Apply(&v, ""); err != nil {
			t.Errorf("%s: the default %q does not parse: %v", s.Key, s.Default, err)
		}
		for _, choice := range s.Choices {
			if err := s.Apply(&v, choice); err != nil {
				t.Errorf("%s: the choice %q does not parse: %v", s.Key, choice, err)
			}
		}
		if seen[s.Key] || s.Help == "" || s.Section == "" {
			t.Errorf("%s is listed twice, says nothing about itself or has no section", s.Key)
		}
		seen[s.Key] = true
		if got, ok := config.SettingByKey(s.Key); !ok || got.Key != s.Key {
			t.Errorf("SettingByKey(%s) = %v, %v", s.Key, got.Key, ok)
		}
	}
	// The defaults are written the way somebody would type them, and are
	// still the constants the rest of the code is written against.
	defaults, err := config.Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	if defaults.TrashRetention != config.DefaultTrashRetention || defaults.GCInterval != config.DefaultGCInterval ||
		defaults.GCGrace != config.DefaultGCGrace || defaults.IndexInterval != config.DefaultIndexInterval ||
		defaults.IncomingInterval != config.DefaultIncomingInterval || defaults.LogLevel != config.DefaultLogLevel {
		t.Errorf("a default written in the list is not its constant: %+v", defaults.Values)
	}
	for _, key := range []string{"STRATUS_DB_DSN", "STRATUS_INCOMING_DIR", "STRATUS_PASSWORD", "STRATUS_SENTRY_DSN"} {
		if _, ok := config.SettingByKey(key); ok {
			t.Errorf("%s is editable, and is meant to stay in the environment", key)
		}
	}
}

func TestAZoneThatIsNotOneIsRefused(t *testing.T) {
	t.Parallel()
	_, err := config.Load(func(key string) string {
		if key == "TZ" {
			return "Mars/Olympus"
		}
		return ""
	})
	if err == nil {
		t.Error("Load took a zone that is not one")
	}
}

func TestWhatTheEnvironmentFixedIsKept(t *testing.T) {
	t.Parallel()
	cfg, err := config.Load(func(key string) string {
		return map[string]string{"STRATUS_GC_INTERVAL": "2h", "STRATUS_DB_DSN": "sqlite:///x.db"}[key]
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Fixed) != 1 || cfg.Fixed["STRATUS_GC_INTERVAL"] != "2h" {
		t.Errorf("Fixed = %v, want the one editable setting the environment set", cfg.Fixed)
	}
}

func TestAWeekThatStartsOnAnotherDayIsRefused(t *testing.T) {
	t.Parallel()
	_, err := config.Load(func(key string) string {
		if key == "STRATUS_WEEK_START" {
			return "tuesday"
		}
		return ""
	})
	if err == nil {
		t.Error("Load took a week that starts on Tuesday")
	}
}
