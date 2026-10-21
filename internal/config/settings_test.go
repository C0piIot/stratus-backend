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
		if seen[s.Key] || s.Help == "" {
			t.Errorf("%s is listed twice or says nothing about itself", s.Key)
		}
		seen[s.Key] = true
		if got, ok := config.SettingByKey(s.Key); !ok || got.Key != s.Key {
			t.Errorf("SettingByKey(%s) = %v, %v", s.Key, got.Key, ok)
		}
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
