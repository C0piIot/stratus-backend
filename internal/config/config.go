// Package config turns the process environment into a validated Config.
//
// Nothing here reads the environment directly: the lookup function is injected.
// That keeps Load a pure function, which matters for tests -- t.Setenv panics
// inside a parallel test, so a package that calls os.Getenv internally forces
// its whole test suite to run serially.
package config

import (
	"log/slog"
	"strings"
)

// Defaults. The container image sets ADDR and DATA_DIR explicitly, so these are
// what a bare `go run ./cmd/stratus` gets.
const (
	DefaultAddr     = ":8080"
	DefaultDataDir  = "/data"
	DefaultLogLevel = slog.LevelInfo
)

// Config is the fully resolved configuration for one process.
type Config struct {
	// Addr is the listen address, in host:port form.
	Addr string
	// DataDir holds blobs, the database and any derived media.
	DataDir string
	// LogLevel is the minimum level emitted by the JSON handler.
	LogLevel slog.Level
}

// Load resolves the configuration from getenv, which is os.Getenv in
// production. A nil getenv resolves every value to its default, which is a
// convenient way to ask for "the defaults" in a test.
func Load(getenv func(string) string) Config {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	return Config{
		Addr:     lookup(getenv, "STRATUS_ADDR", DefaultAddr),
		DataDir:  lookup(getenv, "STRATUS_DATA_DIR", DefaultDataDir),
		LogLevel: parseLevel(lookup(getenv, "STRATUS_LOG_LEVEL", "")),
	}
}

// lookup treats an empty value as absent. Compose and .env files both make it
// easy to define a variable as the empty string, and "unset it back to the
// default" is almost always what that means.
func lookup(getenv func(string) string, key, fallback string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return fallback
}

// parseLevel falls back to the default on anything it cannot parse. See the
// note in Load's tests: whether a typo should instead be a startup error is an
// open question, deliberately left as it was.
func parseLevel(raw string) slog.Level {
	if raw == "" {
		return DefaultLogLevel
	}
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(strings.ToUpper(raw))); err != nil {
		return DefaultLogLevel
	}
	return lvl
}
