// Package config turns the process environment into a validated Config.
//
// Nothing here reads the environment directly: the lookup function is injected.
// That keeps Load a pure function, which matters for tests -- t.Setenv panics
// inside a parallel test, so a package that calls os.Getenv internally forces
// its whole test suite to run serially.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/getsentry/sentry-go"
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
	// DefaultGCInterval is how often orphaned blobs are collected. Daily: the
	// sweep reads the whole store, which on S3 is billed per thousand keys.
	DefaultGCInterval = 24 * time.Hour
	// DefaultGCGrace is how old a blob must be before the collector will touch
	// it. Writes go blob first and row second, so a blob with no row may be a
	// write still in flight rather than one that failed.
	DefaultGCGrace = time.Hour
	// DefaultIndexInterval is how often the indexer asks the database what is
	// missing. Hourly, because it is the safety net and not the way anything
	// ordinarily gets indexed: a write hands the file over and it is read then
	// and there. Asking is a scan of every file row, which costs the same
	// whether the answer is a file or nothing (#158).
	//
	// It is also the resolution of the retry clock: a file deferred for an
	// hour because the store would not answer waits between one and two.
	DefaultIndexInterval = time.Hour
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
	// GCInterval is how often orphaned blobs are collected. Zero disables it.
	GCInterval time.Duration
	// GCGrace is how long a blob is left alone before it can be collected.
	GCGrace time.Duration
	// IndexInterval is how often the media indexer looks for work nobody told
	// it about. Zero disables indexing altogether, notices included.
	IndexInterval time.Duration
	// Username and Password are the single user's credentials, and the
	// password is held as configured rather than hashed: OpenSubsonic's token
	// auth is md5(password + salt), which a hash cannot produce. See
	// internal/auth.
	Username string
	Password Secret
	// SentryDSN is where errors are reported, and empty reports nothing. Held
	// as a secret although Sentry calls the key public: anybody holding it can
	// spend the project's quota.
	SentryDSN Secret
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
		Addr:     lookup(getenv, "STRATUS_ADDR", DefaultAddr),
		DataDir:  lookup(getenv, "STRATUS_DATA_DIR", DefaultDataDir),
		Username: getenv("STRATUS_USERNAME"),
		Password: Secret(getenv("STRATUS_PASSWORD")),
	}

	// Refused rather than defaulted, which is the rule every other setting in
	// this function already follows: a typo would otherwise start the server at
	// info and leave whoever set it debugging blind, wondering why the lines
	// they asked for never appear.
	level, err := parseLevel(lookup(getenv, "STRATUS_LOG_LEVEL", ""))
	if err != nil {
		return Config{}, fmt.Errorf("STRATUS_LOG_LEVEL: %w", err)
	}
	cfg.LogLevel = level

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

	// Unlike the log level, a typo here is not shrugged off: "1hour" would
	// otherwise silently become the default and nobody would know the sweep was
	// running on a schedule they did not choose.
	interval, err := time.ParseDuration(lookup(getenv, "STRATUS_GC_INTERVAL", DefaultGCInterval.String()))
	if err != nil {
		return Config{}, errors.New("STRATUS_GC_INTERVAL: not a duration, try 24h or 0 to disable")
	}
	if interval < 0 {
		return Config{}, errors.New("STRATUS_GC_INTERVAL: cannot be negative, use 0 to disable")
	}
	cfg.GCInterval = interval

	grace, err := time.ParseDuration(lookup(getenv, "STRATUS_GC_GRACE", DefaultGCGrace.String()))
	if err != nil {
		return Config{}, errors.New("STRATUS_GC_GRACE: not a duration, try 1h")
	}
	// Zero is refused rather than taken as "collect immediately". Writes go
	// blob first and row second, so with no grace a sweep landing between an
	// upload's blob and its row deletes a blob whose row is about to exist, and
	// the upload is gone with it. That is a correctness requirement rather than
	// an operator preference, the same argument the SQLite pragmas get.
	if grace <= 0 {
		return Config{}, errors.New("STRATUS_GC_GRACE: must be greater than zero, or a sweep can delete an upload still in flight")
	}
	cfg.GCGrace = grace

	index, err := time.ParseDuration(lookup(getenv, "STRATUS_INDEX_INTERVAL", DefaultIndexInterval.String()))
	if err != nil {
		return Config{}, errors.New("STRATUS_INDEX_INTERVAL: not a duration, try 1m or 0 to disable")
	}
	if index < 0 {
		return Config{}, errors.New("STRATUS_INDEX_INTERVAL: cannot be negative, use 0 to disable")
	}
	cfg.IndexInterval = index

	// Checked here rather than when the client is made, so that a DSN pasted
	// with a character missing stops the process instead of an install
	// believing it is reporting while every event is refused.
	if dsn := getenv("STRATUS_SENTRY_DSN"); dsn != "" {
		if _, err := sentry.NewDsn(dsn); err != nil {
			return Config{}, errors.New("STRATUS_SENTRY_DSN: not a Sentry DSN, copy it from the project's Client Keys")
		}
		cfg.SentryDSN = Secret(dsn)
	}

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
// parseLevel reads a level name, case-insensitively and with slog's offset
// syntax -- "debug+2" is a level below debug, which is how a client library
// asks for more than this program names.
//
// Unset is the default and not an error: the variable is optional. Anything
// else that does not parse is, since it can only be a mistake.
func parseLevel(raw string) (slog.Level, error) {
	if raw == "" {
		return DefaultLogLevel, nil
	}
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(strings.ToUpper(raw))); err != nil {
		return 0, fmt.Errorf("%q is not a level, try debug, info, warn or error", raw)
	}
	return lvl, nil
}
