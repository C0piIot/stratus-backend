// Package config turns the process environment into a validated Config.
//
// Nothing here reads the environment directly: the lookup function is injected.
// That keeps Load a pure function, which matters for tests -- t.Setenv panics
// inside a parallel test, so a package that calls os.Getenv internally forces
// its whole test suite to run serially.
package config

import (
	"fmt"
	"log/slog"
	"net/url"
	"path"
	"strings"
)

// Defaults. The container image sets ADDR and DATA_DIR explicitly, so these are
// what a bare `go run ./cmd/stratus` gets.
const (
	DefaultAddr     = ":8080"
	DefaultDataDir  = "/data"
	DefaultLogLevel = slog.LevelInfo
	// DefaultBlobDir is where blobs go under DataDir when no storage DSN is
	// given, so that an install with no configuration at all still has a
	// working backend.
	DefaultBlobDir = "blobs"
	// DefaultDBFile is the SQLite file under DataDir when no database DSN is
	// given.
	DefaultDBFile = "stratus.db"
)

// Config is the fully resolved configuration for one process.
type Config struct {
	// Addr is the listen address, in host:port form.
	Addr string
	// DataDir holds blobs, the database and any derived media.
	DataDir string
	// LogLevel is the minimum level emitted by the JSON handler.
	LogLevel slog.Level
	// Storage selects and configures the blob backend.
	Storage StorageDSN
	// Database selects and configures the metadata backend.
	Database DatabaseDSN
	// Username and PasswordHash are the single user's credentials. Both are
	// empty until a protocol needs them; nothing authenticates anything yet.
	Username     string
	PasswordHash Secret
}

// Load resolves the configuration from getenv, which is os.Getenv in
// production. A nil getenv resolves every value to its default, which is a
// convenient way to ask for "the defaults" in a test.
//
// It fails on a malformed DSN and on nothing else: everything a process cannot
// recover from should stop it here rather than at the first request.
func Load(getenv func(string) string) (Config, error) {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}

	cfg := Config{
		Addr:         lookup(getenv, "STRATUS_ADDR", DefaultAddr),
		DataDir:      lookup(getenv, "STRATUS_DATA_DIR", DefaultDataDir),
		LogLevel:     parseLevel(lookup(getenv, "STRATUS_LOG_LEVEL", "")),
		Username:     getenv("STRATUS_USERNAME"),
		PasswordHash: Secret(getenv("STRATUS_PASSWORD_HASH")),
	}

	storage, err := ParseStorageDSN(lookup(getenv, "STRATUS_STORAGE_DSN", defaultStorageDSN(cfg.DataDir)))
	if err != nil {
		return Config{}, fmt.Errorf("STRATUS_STORAGE_DSN: %w", err)
	}
	cfg.Storage = storage

	database, err := ParseDatabaseDSN(lookup(getenv, "STRATUS_DB_DSN", defaultDatabaseDSN(cfg.DataDir)))
	if err != nil {
		return Config{}, fmt.Errorf("STRATUS_DB_DSN: %w", err)
	}
	cfg.Database = database
	return cfg, nil
}

// defaultStorageDSN builds the file DSN for a data directory. It goes through
// url.URL rather than string concatenation so that a data directory with a
// space or a percent sign in it produces a DSN that parses back.
func defaultStorageDSN(dataDir string) string {
	u := url.URL{Scheme: SchemeFile, Path: path.Join(dataDir, DefaultBlobDir)}
	return u.String()
}

// defaultDatabaseDSN puts the SQLite file next to the blobs, for the same
// reason: an install with no configuration at all still has to work.
func defaultDatabaseDSN(dataDir string) string {
	u := url.URL{Scheme: SchemeSQLite, Path: path.Join(dataDir, DefaultDBFile)}
	return u.String()
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
