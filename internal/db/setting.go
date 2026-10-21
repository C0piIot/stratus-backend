package db

import (
	"context"
	"time"
)

// Settings are what the web UI stored of the settings an environment left
// unset (#397): one row per variable, its value as it would be typed into
// the environment. Nothing here knows what a key means; internal/config does,
// and internal/settings decides which of the two wins.
type Settings interface {
	// Settings returns every stored setting by its key.
	Settings(ctx context.Context) (map[string]string, error)

	// PutSetting stores a value, replacing what was there.
	PutSetting(ctx context.Context, key, value string, at time.Time) error

	// DeleteSetting forgets one, and is not an error for one never stored.
	DeleteSetting(ctx context.Context, key string) error
}
