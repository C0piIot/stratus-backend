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
	"path/filepath"
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
	// DefaultTrashRetention is how long a deletion is kept before it is
	// destroyed for good (#274). A month, which is long enough to notice an
	// accident -- including the kind that is only noticed when somebody goes
	// looking for a photograph months later, which this does not cover and
	// nothing can. It was a constant in internal/files until somebody
	// restoring a database needed the sweep not to run meanwhile (#296).
	DefaultTrashRetention = 30 * 24 * time.Hour
	// DefaultIndexInterval is how often the indexer asks the database what is
	// missing. Hourly, because it is the safety net and not the way anything
	// ordinarily gets indexed: a write hands the file over and it is read then
	// and there. Asking is a scan of every file row, which costs the same
	// whether the answer is a file or nothing (#158).
	//
	// It is also the resolution of the retry clock: a file deferred for an
	// hour because the store would not answer waits between one and two.
	DefaultIndexInterval = time.Hour

	// DefaultIncomingInterval is how often an import directory is swept, and
	// therefore how long a file has to hold its size before it is taken. A
	// minute: short enough that dropping a file in feels like it worked, long
	// enough that an ordinary copy has finished by the time the second pass
	// looks.
	DefaultIncomingInterval = time.Minute
)

// The values STRATUS_VIDEO_TRANSCODE takes. Auto re-encodes only on a machine
// that can keep up -- libx264 on one or two cores is slower than a film plays,
// and a player that waits on every segment is worse than one given the
// original to fail on (see internal/media/encode.go).
const (
	VideoTranscodeAuto = "auto"
	VideoTranscodeOn   = "on"
	VideoTranscodeOff  = "off"
)

// The values STRATUS_THUMBS_AHEAD takes. On is the default, and the argument
// for it is that a personal cloud is judged on the first look at its photo
// grid: the pass takes a decoder only when nothing else wants one, so what it
// spends is a CPU that was idle. Off is for whoever would rather not pay the
// writes for files nobody is going to open (#341).
const (
	ThumbsAheadOn  = "on"
	ThumbsAheadOff = "off"
)

// The values STRATUS_MUSIC_LOOKUP takes. Off is the default, and that is not
// a preference: a self-hosted server must keep working with no route out, and
// on means artist, album and title tags leave the machine for MusicBrainz, the
// Cover Art Archive and LRCLIB (#217). The README says exactly what is sent.
const (
	MusicLookupOn  = "on"
	MusicLookupOff = "off"
)

// Config is the fully resolved configuration for one process.
type Config struct {
	// Addr is the listen address, in host:port form.
	Addr string
	// DataDir holds blobs, the database and any derived media.
	DataDir string
	// Storage selects and configures the blob backend.
	Storage StorageDSN
	// Database selects and configures the metadata backend.
	Database DatabaseDSN
	// IncomingDir is a directory on this machine's own filesystem whose
	// contents are moved into the library. Empty, which is the default, is the
	// feature switched off: there is no sensible default directory, and one
	// that was guessed would be one somebody's files disappeared into.
	IncomingDir string
	// Values are the settings that can also be changed from the web UI
	// (#397), as the environment set them at startup.
	Values
	// Fixed are the ones among them the environment set, as it set them:
	// what internal/settings lets nothing else change.
	Fixed map[string]string
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

	// Every editable setting goes through its own parser, the one the web
	// UI's form goes through too. A typo is refused rather than defaulted:
	// "1hour" would otherwise silently become the default and nobody would
	// know the sweep was running on a schedule they did not choose.
	cfg.Fixed = map[string]string{}
	for _, setting := range Settings {
		raw := getenv(setting.Key)
		if err := setting.Apply(&cfg.Values, raw); err != nil {
			return Config{}, fmt.Errorf("%s: %w", setting.Key, err)
		}
		if raw != "" {
			cfg.Fixed[setting.Key] = raw
		}
	}

	cfg.IncomingDir = getenv("STRATUS_INCOMING_DIR")
	// Inside the data directory it would sweep the blob store into itself, one
	// blob at a time, forever. Refused here rather than discovered there.
	if cfg.IncomingDir != "" && within(cfg.IncomingDir, cfg.DataDir) {
		return Config{}, errors.New("STRATUS_INCOMING_DIR: cannot be inside STRATUS_DATA_DIR, which is where the blobs are")
	}

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

// within reports whether dir is the same place as root or sits under it, as far
// as the names can say. Symlinks are not resolved: this is a guard against a
// configuration that is obviously wrong, not against one built to defeat it.
//
// The error from Abs is dropped on purpose rather than branched on. It is
// returned only when the working directory cannot be read, which is a process
// with larger problems than this check, and the empty string it leaves behind
// matches nothing -- so the one failure mode is that an obvious mistake is
// allowed through instead of being caught here.
func within(dir, root string) bool {
	a, _ := filepath.Abs(filepath.Clean(dir))
	b, _ := filepath.Abs(filepath.Clean(root))
	return a == b || strings.HasPrefix(a, b+string(filepath.Separator))
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
// Anything that does not parse is an error, since it can only be a mistake;
// unset never reaches here, because Setting.Apply has put the default in.
func parseLevel(raw string) (slog.Level, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(strings.ToUpper(raw))); err != nil {
		return 0, fmt.Errorf("%q is not a level, try debug, info, warn or error", raw)
	}
	return lvl, nil
}

// TrashNever is the STRATUS_TRASH_RETENTION that keeps every deletion until
// somebody empties it by hand.
const TrashNever = "never"

// trashRetention reads STRATUS_TRASH_RETENTION, with zero standing for never.
//
// Less than a day is refused: a trash that empties within the hour is a delete
// with a delay on it, and whoever typed 1h most likely meant something else.
func trashRetention(value string) (time.Duration, error) {
	if value == TrashNever {
		return 0, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil || d == 0 {
		return 0, errors.New("not a duration, try 2160h, or never to keep everything")
	}
	if d < 24*time.Hour {
		return 0, errors.New("must be at least 24h, or never to keep everything")
	}
	return d, nil
}
