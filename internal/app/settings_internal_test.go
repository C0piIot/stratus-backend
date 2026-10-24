package app

import (
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/config"
	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/files/filestest"
	"github.com/C0piIot/stratus-backend/internal/lookup"
	"github.com/C0piIot/stratus-backend/internal/media"
	"github.com/C0piIot/stratus-backend/internal/media/stream"
	"github.com/C0piIot/stratus-backend/internal/media/thumb"
	"github.com/C0piIot/stratus-backend/internal/settings"
)

func liveSettings(t *testing.T, meta db.Store) *settings.Service {
	t.Helper()
	s, err := settings.New(t.Context(), func(string) string { return "" }, meta)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestALoopFollowsItsInterval is #397 for every loop built on every: off until
// a setting turns it on, faster when it is shortened, untouched when another
// setting changes, and off again at zero.
func TestALoopFollowsItsInterval(t *testing.T) {
	t.Parallel()
	_, meta := filestest.Backends(t)
	s := liveSettings(t, meta)
	if err := s.Set(t.Context(), "STRATUS_INCOMING_INTERVAL", "0"); err != nil {
		t.Fatal(err)
	}
	a := New(config.Config{}, "test", "2026-01-01T09:30:00Z")
	deps := Deps{Settings: s}

	ctx, cancel := context.WithCancel(t.Context())
	var passes atomic.Int64
	done := make(chan struct{})
	go func() {
		a.every(ctx, deps, "test", func(v config.Values) time.Duration { return v.IncomingInterval }, func() bool {
			passes.Add(1)
			return true
		})
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	if n := passes.Load(); n != 0 {
		t.Fatalf("%d passes while the interval was zero", n)
	}
	if err := s.Set(t.Context(), "STRATUS_INCOMING_INTERVAL", "5ms"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a pass once the interval was set", func() bool { return passes.Load() >= 3 })

	if err := s.Set(t.Context(), "TZ", "Europe/Madrid"); err != nil {
		t.Fatal(err)
	}
	before := passes.Load()
	eventually(t, "passes going on through an unrelated change", func() bool { return passes.Load() > before+2 })

	if err := s.Set(t.Context(), "STRATUS_INCOMING_INTERVAL", "0"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	stopped := passes.Load()
	time.Sleep(50 * time.Millisecond)
	if passes.Load() != stopped {
		t.Error("passes went on after the interval went back to zero")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop did not end with its context")
	}

	// And a pass that says stop is the end of the loop.
	if err := s.Set(t.Context(), "STRATUS_INCOMING_INTERVAL", "1ms"); err != nil {
		t.Fatal(err)
	}
	ended := make(chan struct{})
	go func() {
		a.every(t.Context(), deps, "test", func(v config.Values) time.Duration { return v.IncomingInterval }, func() bool { return false })
		close(ended)
	}()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("a pass that said stop did not end the loop")
	}
}

func TestTheLogLevelFollowsTheSetting(t *testing.T) {
	t.Parallel()
	_, meta := filestest.Backends(t)
	s := liveSettings(t, meta)
	level := new(slog.LevelVar)
	a := New(config.Config{}, "test", "2026-01-01T09:30:00Z").WithLogLevel(level)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go a.followLogLevel(ctx, Deps{Settings: s})

	if err := s.Set(t.Context(), "STRATUS_LOG_LEVEL", "debug"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the level moving to debug", func() bool { return level.Level() == slog.LevelDebug })
	if err := s.Reset(t.Context(), "STRATUS_LOG_LEVEL"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the level back at info", func() bool { return level.Level() == slog.LevelInfo })
}

// TestTheLookupAsksNothingWhileOff: the pass is there, and nothing leaves the
// machine until the setting is turned on -- and then it asks at once.
func TestTheLookupAsksNothingWhileOff(t *testing.T) {
	t.Parallel()
	blobs, meta := filestest.Backends(t)
	service := files.New(blobs, meta)
	thumbs := thumb.New(blobs, service, meta, "ffmpeg", t.TempDir()).WithLookups(meta)
	if _, err := service.Mkdir(t.Context(), "edu", "music"); err != nil {
		t.Fatal(err)
	}
	f, err := service.Write(t.Context(), "edu", "music/a.flac", strings.NewReader("fLaC"), 4, "audio/flac")
	if err != nil {
		t.Fatal(err)
	}
	if err = meta.PutMedia(t.Context(), db.Media{
		FileID: f.ID, Kind: db.KindAudio, IndexedAt: time.Now(), Version: 1,
		AlbumArtist: "Artist", Artist: "Artist", Album: "Album", Title: "Track",
	}); err != nil {
		t.Fatal(err)
	}
	s := liveSettings(t, meta)
	nowhere := "http://127.0.0.1:1"
	deps := Deps{
		Storage: blobs, Database: meta, Files: service, Thumbs: thumbs, Settings: s,
		Lookup: lookup.New(meta, thumbs, "test", lookup.WithEndpoints(nowhere, nowhere, nowhere), lookup.WithPace(0)),
	}
	a := New(config.Config{}, "test", "2026-01-01T09:30:00Z")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go a.lookupPeriodically(ctx, deps)

	tried := func() int64 {
		counts, cerr := meta.LookupCounts(t.Context())
		if cerr != nil {
			t.Fatal(cerr)
		}
		return counts.AlbumsTried
	}
	time.Sleep(100 * time.Millisecond)
	if n := tried(); n != 0 {
		t.Fatalf("%d albums were asked about with the setting off", n)
	}
	if err = s.Set(t.Context(), "STRATUS_MUSIC_LOOKUP", "on"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "an album asked about once the setting was on", func() bool { return tried() == 1 })
	if err = s.Set(t.Context(), "TZ", "UTC"); err != nil {
		t.Fatal(err)
	}
}

// TestTheIndexerIsSwitchedOnAndOff is STRATUS_INDEX_INTERVAL from the web UI:
// nothing is read while it is zero, what landed meanwhile is found the moment
// it is set, a write is read as it lands, and a new interval goes on reading.
func TestTheIndexerIsSwitchedOnAndOff(t *testing.T) {
	t.Parallel()
	blobs, meta := filestest.Backends(t)
	var ix *media.Indexer
	service := files.New(blobs, meta, files.WithWatcher(func(f db.File) {
		if ix != nil {
			ix.Notice(f)
		}
	}))
	ix = media.NewIndexer(service, meta, t.TempDir(), "ffprobe")
	s := liveSettings(t, meta)
	for key, value := range map[string]string{"STRATUS_INDEX_INTERVAL": "0", "STRATUS_THUMBS_AHEAD": "off"} {
		if err := s.Set(t.Context(), key, value); err != nil {
			t.Fatal(err)
		}
	}
	deps := Deps{Storage: blobs, Database: meta, Files: service, Indexer: ix, Settings: s}
	a := New(config.Config{}, "test", "2026-01-01T09:30:00Z")

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		a.indexPeriodically(ctx, deps)
		close(done)
	}()

	write := func(name string) db.File {
		t.Helper()
		f, err := service.Write(t.Context(), "edu", name, strings.NewReader("words"), 5, "text/plain")
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	indexed := func(f db.File) func() bool {
		return func() bool {
			_, err := meta.MediaByFile(t.Context(), f.ID)
			return err == nil
		}
	}

	early := write("early.txt")
	time.Sleep(50 * time.Millisecond)
	if indexed(early)() {
		t.Fatal("a file was read with indexing off")
	}
	if err := s.Set(t.Context(), "STRATUS_INDEX_INTERVAL", "1h"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "what landed while indexing was off being read", indexed(early))
	eventually(t, "a write being read as it lands", indexed(write("landed.txt")))

	if err := s.Set(t.Context(), "STRATUS_INDEX_INTERVAL", "2h"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(t.Context(), "TZ", "Europe/Madrid"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a write being read after the interval moved", indexed(write("later.txt")))

	if err := s.Set(t.Context(), "STRATUS_INDEX_INTERVAL", "0"); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the indexer did not end with its context")
	}
}

// TestAFilmIsReencodedAsTheSettingSays is STRATUS_VIDEO_TRANSCODE read per
// request rather than once at startup.
func TestAFilmIsReencodedAsTheSettingSays(t *testing.T) {
	t.Parallel()
	_, meta := filestest.Backends(t)
	s := liveSettings(t, meta)
	a := New(config.Config{}, "test", "2026-01-01T09:30:00Z")
	v := a.playback(Deps{Database: meta, Transcoder: &stream.Transcoder{}, Encoder: &stream.Encoder{}, Settings: s})
	if v.Encodes == nil {
		t.Fatal("no switch on the encoder")
	}
	for _, mode := range []string{"on", "off", "on"} {
		if err := s.Set(t.Context(), "STRATUS_VIDEO_TRANSCODE", mode); err != nil {
			t.Fatal(err)
		}
		if got := v.Encodes(); got != (mode == "on") {
			t.Errorf("with %s a film is re-encoded = %v", mode, got)
		}
	}
}
