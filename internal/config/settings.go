package config

import (
	"errors"
	"log/slog"
	"time"
)

// Values are the settings that can be changed while the server runs (#397):
// from the environment like everything else, and from the web UI when the
// environment leaves one unset. What opens the database and the store, the
// credentials, the Sentry DSN and the import folder's path are not among
// them, and stay in Config.
type Values struct {
	// LogLevel is the minimum level emitted by the JSON handler.
	LogLevel slog.Level
	// GCInterval is how often orphaned blobs are collected. Zero disables it.
	GCInterval time.Duration
	// GCGrace is how long a blob is left alone before it can be collected.
	GCGrace time.Duration
	// TrashRetention is how long a deletion is kept before the sweep destroys
	// it, and zero means never (#296). The variable spells that "never"
	// rather than 0, because 0 turns the other intervals off and here it would
	// read as the opposite of what it does.
	TrashRetention time.Duration
	// IndexInterval is how often the media indexer looks for work nobody told
	// it about. Zero disables indexing altogether, notices included.
	IndexInterval time.Duration
	// IncomingInterval is how often the import folder is swept. It is also how
	// long a file has to sit at the same size before it counts as finished, so
	// it is the one number that trades "imported sooner" against "imported
	// half-written". Zero disables the sweep.
	IncomingInterval time.Duration
	// VideoTranscode is whether a film is also offered re-encoded to H.264:
	// VideoTranscodeAuto, VideoTranscodeOn or VideoTranscodeOff (#50).
	VideoTranscode string
	// ThumbsAhead is whether the indexer's second half makes the pictures a
	// grid will ask for before it asks (#341). It follows IndexInterval,
	// since it is that loop's pass: with the indexer off there is nothing to
	// carry it.
	ThumbsAhead bool
	// MusicLookup is whether outside sources are asked about the music
	// library (#217): album identity and covers from MusicBrainz and the
	// Cover Art Archive, lyrics from LRCLIB. Off by default, because on
	// means tags leave the machine.
	MusicLookup bool
	// Zone is what the calendar page draws times in (#111). Unset is UTC: a
	// page with no script cannot ask the browser for its own.
	Zone *time.Location
	// WeekStart is the day the calendar page's weeks begin on: Monday, or
	// Sunday.
	WeekStart time.Weekday
	// BackupInterval is how often the database is copied into the blob store
	// (#398). Zero disables it.
	BackupInterval time.Duration
}

// Setting is one of Values as a variable: its name, what it is when nobody
// set it, and how a value is read. The environment, the stored settings and
// the web UI's form all go through the same one, so a value one of them
// refuses the others refuse too.
type Setting struct {
	// Key is the environment variable, and the key it is stored under.
	Key string
	// Default is the value when neither the environment nor the stored
	// settings have one, written the way somebody would type it.
	Default string
	// Choices are the only values it takes, for one that is a choice; empty
	// for one that is typed.
	Choices []string
	// Help says what it does, in a line, for the page that offers it, and
	// Section is the heading it is under there.
	Help    string
	Section string
	apply   func(v *Values, raw string) error
}

// Apply reads raw into v, or says what is wrong with it. Empty is the
// default, which is what an unset variable means everywhere here.
func (s Setting) Apply(v *Values, raw string) error {
	if raw == "" {
		raw = s.Default
	}
	return s.apply(v, raw)
}

// Settings are every editable setting, in the order a page shows them.
var Settings = []Setting{
	{
		Key: "STRATUS_TRASH_RETENTION", Section: "Trash and sweep", Default: "720h",
		Help: "How long a deletion stays in the trash: at least 24h, or never.",
		apply: func(v *Values, raw string) (err error) {
			v.TrashRetention, err = trashRetention(raw)
			return err
		},
	},
	{
		Key: "STRATUS_GC_INTERVAL", Section: "Trash and sweep", Default: "24h",
		Help: "How often the blob store is swept for what no row claims; 0 never sweeps.",
		apply: func(v *Values, raw string) (err error) {
			v.GCInterval, err = interval(raw, "not a duration, try 24h or 0 to disable")
			return err
		},
	},
	{
		Key: "STRATUS_GC_GRACE", Section: "Trash and sweep", Default: "1h",
		Help: "How old a blob has to be before the sweep may touch it.",
		apply: func(v *Values, raw string) error {
			grace, err := time.ParseDuration(raw)
			if err != nil {
				return errors.New("not a duration, try 1h")
			}
			// Zero is refused rather than taken as "collect immediately".
			// Writes go blob first and row second, so with no grace a sweep
			// landing between an upload's blob and its row deletes a blob
			// whose row is about to exist, and the upload is gone with it.
			// That is a correctness requirement rather than an operator
			// preference, the same argument the SQLite pragmas get.
			if grace <= 0 {
				return errors.New("must be greater than zero, or a sweep can delete an upload still in flight")
			}
			v.GCGrace = grace
			return nil
		},
	},
	{
		Key: "STRATUS_INDEX_INTERVAL", Section: "Indexing", Default: "1h",
		Help: "How often the indexer asks what nobody told it about; 0 turns indexing off.",
		apply: func(v *Values, raw string) (err error) {
			v.IndexInterval, err = interval(raw, "not a duration, try 1m or 0 to disable")
			return err
		},
	},
	{
		Key: "STRATUS_THUMBS_AHEAD", Section: "Indexing", Default: ThumbsAheadOn, Choices: []string{ThumbsAheadOn, ThumbsAheadOff},
		Help: "Whether pictures are made before a grid asks for them, with a CPU that was idle.",
		apply: func(v *Values, raw string) (err error) {
			v.ThumbsAhead, err = onOff(raw)
			return err
		},
	},
	{
		Key: "STRATUS_INCOMING_INTERVAL", Section: "Import folder", Default: "1m",
		Help: "How often the import folder is swept, and how long a file must hold its size; 0 stops it.",
		apply: func(v *Values, raw string) (err error) {
			v.IncomingInterval, err = interval(raw, "not a duration, try 1m or 0 to disable")
			return err
		},
	},
	{
		Key: "STRATUS_VIDEO_TRANSCODE", Section: "Video", Default: VideoTranscodeAuto,
		Choices: []string{VideoTranscodeAuto, VideoTranscodeOn, VideoTranscodeOff},
		Help:    "Whether a film a player cannot decode is also offered re-encoded; auto is from four CPUs.",
		apply: func(v *Values, raw string) error {
			switch raw {
			case VideoTranscodeAuto, VideoTranscodeOn, VideoTranscodeOff:
				v.VideoTranscode = raw
				return nil
			}
			return errors.New("must be auto, on or off")
		},
	},
	{
		Key: "STRATUS_MUSIC_LOOKUP", Section: "Music", Default: MusicLookupOff, Choices: []string{MusicLookupOn, MusicLookupOff},
		Help: "Whether MusicBrainz, the Cover Art Archive and LRCLIB are asked about the music; on sends tags out.",
		apply: func(v *Values, raw string) (err error) {
			v.MusicLookup, err = onOff(raw)
			return err
		},
	},
	{
		Key: "TZ", Section: "Calendar", Default: "UTC",
		Help: "The zone the calendar page draws times in, such as Europe/Madrid.",
		apply: func(v *Values, raw string) error {
			zone, err := time.LoadLocation(raw)
			if err != nil {
				return errors.New("not a zone, try Europe/Madrid or UTC")
			}
			v.Zone = zone
			return nil
		},
	},
	{
		Key: "STRATUS_WEEK_START", Section: "Calendar", Default: "monday", Choices: []string{"monday", "sunday"},
		Help: "The day the calendar page's weeks begin on.",
		apply: func(v *Values, raw string) error {
			switch raw {
			case "monday":
				v.WeekStart = time.Monday
			case "sunday":
				v.WeekStart = time.Sunday
			default:
				return errors.New("must be monday or sunday")
			}
			return nil
		},
	},
	{
		Key: "STRATUS_BACKUP_INTERVAL", Section: "Backup", Default: "24h",
		Help: "How often the database is copied into the blob store, keeping the last seven; 0 stops it.",
		apply: func(v *Values, raw string) (err error) {
			v.BackupInterval, err = interval(raw, "not a duration, try 24h or 0 to disable")
			return err
		},
	},
	{
		Key: "STRATUS_LOG_LEVEL", Section: "Logs", Default: "info", Choices: []string{"debug", "info", "warn", "error"},
		Help: "The least a log line has to be to be written.",
		apply: func(v *Values, raw string) (err error) {
			v.LogLevel, err = parseLevel(raw)
			return err
		},
	},
}

// SettingByKey returns the setting a key names, and false for one that is not
// editable.
func SettingByKey(key string) (Setting, bool) {
	for _, s := range Settings {
		if s.Key == key {
			return s, true
		}
	}
	return Setting{}, false
}

// interval is a duration that may be zero, which is off, and not negative.
func interval(raw, notADuration string) (time.Duration, error) {
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, errors.New(notADuration)
	}
	if d < 0 {
		return 0, errors.New("cannot be negative, use 0 to disable")
	}
	return d, nil
}

func onOff(raw string) (bool, error) {
	switch raw {
	case "on":
		return true, nil
	case "off":
		return false, nil
	}
	return false, errors.New("must be on or off")
}
