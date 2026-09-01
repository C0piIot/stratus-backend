package config_test

import (
	"log/slog"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/config"
)

// env builds a getenv function over a map. Injecting the lookup is what lets
// every test here run in parallel: t.Setenv would panic in a parallel test.
func env(vars map[string]string) func(string) string {
	return func(key string) string { return vars[key] }
}

// load resolves a configuration and fails the test if it cannot, for the cases
// that are not about parse errors.
func load(t *testing.T, vars map[string]string) config.Config {
	t.Helper()
	cfg, err := config.Load(env(vars))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func TestLoadDefaults(t *testing.T) {
	t.Parallel()
	got := load(t, nil)

	if got.Addr != config.DefaultAddr {
		t.Errorf("Addr = %q, want %q", got.Addr, config.DefaultAddr)
	}
	if got.DataDir != config.DefaultDataDir {
		t.Errorf("DataDir = %q, want %q", got.DataDir, config.DefaultDataDir)
	}
	if got.LogLevel != config.DefaultLogLevel {
		t.Errorf("LogLevel = %v, want %v", got.LogLevel, config.DefaultLogLevel)
	}
}

func TestLoadNilGetenvYieldsDefaults(t *testing.T) {
	t.Parallel()
	// Documented contract: nil means "no environment", not a panic.
	got, err := config.Load(nil)
	if err != nil {
		t.Fatalf("Load(nil): %v", err)
	}
	if got.Addr != config.DefaultAddr || got.DataDir != config.DefaultDataDir {
		t.Errorf("Load(nil) = %+v, want defaults", got)
	}
}

func TestLoadStrings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		vars        map[string]string
		wantAddr    string
		wantDataDir string
	}{
		{
			name:        "both overridden",
			vars:        map[string]string{"STRATUS_ADDR": "127.0.0.1:9000", "STRATUS_DATA_DIR": "/srv/stratus"},
			wantAddr:    "127.0.0.1:9000",
			wantDataDir: "/srv/stratus",
		},
		{
			name:        "only addr overridden",
			vars:        map[string]string{"STRATUS_ADDR": ":9999"},
			wantAddr:    ":9999",
			wantDataDir: config.DefaultDataDir,
		},
		{
			// Compose and .env files make an empty value easy to produce, and
			// "back to the default" is almost always what it means.
			name:        "empty counts as unset",
			vars:        map[string]string{"STRATUS_ADDR": "", "STRATUS_DATA_DIR": ""},
			wantAddr:    config.DefaultAddr,
			wantDataDir: config.DefaultDataDir,
		},
		{
			name:        "whitespace is preserved, not trimmed",
			vars:        map[string]string{"STRATUS_ADDR": " "},
			wantAddr:    " ",
			wantDataDir: config.DefaultDataDir,
		},
		{
			name:        "unrelated variables are ignored",
			vars:        map[string]string{"PATH": "/usr/bin", "HOME": "/root"},
			wantAddr:    config.DefaultAddr,
			wantDataDir: config.DefaultDataDir,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := load(t, tt.vars)
			if got.Addr != tt.wantAddr {
				t.Errorf("Addr = %q, want %q", got.Addr, tt.wantAddr)
			}
			if got.DataDir != tt.wantDataDir {
				t.Errorf("DataDir = %q, want %q", got.DataDir, tt.wantDataDir)
			}
		})
	}
}

func TestLoadLogLevel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		value string
		want  slog.Level
	}{
		{name: "debug", value: "debug", want: slog.LevelDebug},
		{name: "info", value: "info", want: slog.LevelInfo},
		{name: "already uppercase", value: "WARN", want: slog.LevelWarn},
		{name: "mixed case", value: "ErRoR", want: slog.LevelError},
		{name: "offset syntax", value: "debug+2", want: slog.LevelDebug + 2},
		// Current behaviour, pinned rather than endorsed: an unparseable level
		// silently becomes the default. See the open question in the PR.
		{name: "garbage falls back", value: "nonsense", want: config.DefaultLogLevel},
		{name: "numeric falls back", value: "3", want: config.DefaultLogLevel},
		{name: "empty falls back", value: "", want: config.DefaultLogLevel},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := load(t, map[string]string{"STRATUS_LOG_LEVEL": tt.value})
			if got.LogLevel != tt.want {
				t.Errorf("LogLevel = %v, want %v", got.LogLevel, tt.want)
			}
		})
	}
}

func TestLoadIndexInterval(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		value   string
		want    time.Duration
		wantErr bool
	}{
		{name: "unset is a minute", want: config.DefaultIndexInterval},
		{name: "explicit", value: "10s", want: 10 * time.Second},
		{name: "zero disables it", value: "0", want: 0},
		{name: "a typo is an error", value: "1minute", wantErr: true},
		{name: "negative is an error", value: "-1m", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := config.Load(env(map[string]string{"STRATUS_INDEX_INTERVAL": tt.value}))
			if (err != nil) != tt.wantErr {
				t.Fatalf("Load = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && cfg.IndexInterval != tt.want {
				t.Errorf("IndexInterval = %v, want %v", cfg.IndexInterval, tt.want)
			}
		})
	}
}

func TestLoadGCGrace(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		value   string
		want    time.Duration
		wantErr bool
	}{
		{name: "unset is an hour", want: config.DefaultGCGrace},
		{name: "explicit", value: "15m", want: 15 * time.Minute},
		{name: "zero, for a test", value: "0", want: 0},
		{name: "a typo is an error", value: "1hour", wantErr: true},
		{name: "negative is an error", value: "-1h", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := config.Load(env(map[string]string{"STRATUS_GC_GRACE": tt.value}))
			if (err != nil) != tt.wantErr {
				t.Fatalf("Load = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && cfg.GCGrace != tt.want {
				t.Errorf("GCGrace = %v, want %v", cfg.GCGrace, tt.want)
			}
		})
	}
}

func TestLoadGCInterval(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		value   string
		want    time.Duration
		wantErr bool
	}{
		{name: "unset is daily", want: config.DefaultGCInterval},
		{name: "an hour", value: "1h", want: time.Hour},
		{name: "zero disables it", value: "0", want: 0},
		{name: "seconds, for a test", value: "250ms", want: 250 * time.Millisecond},
		// Unlike the log level, a typo here is refused: the alternative is a
		// sweep running on a schedule nobody chose.
		{name: "a typo is an error", value: "1hour", wantErr: true},
		{name: "a bare number is an error", value: "3600", wantErr: true},
		{name: "negative is an error", value: "-1h", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := config.Load(env(map[string]string{"STRATUS_GC_INTERVAL": tt.value}))
			if (err != nil) != tt.wantErr {
				t.Fatalf("Load = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && cfg.GCInterval != tt.want {
				t.Errorf("GCInterval = %v, want %v", cfg.GCInterval, tt.want)
			}
		})
	}
}
