package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/config"
	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/db/dbtest"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/files/filestest"
	"github.com/C0piIot/stratus-backend/internal/lookup"
	"github.com/C0piIot/stratus-backend/internal/media/thumb"
)

// TestLookupLoopRunsAPassAndStopsWithTheContext: the loop is the scheduling
// and nothing else (#217), so what is asserted is that a pass ran, that what
// it could not reach was written down for later, and that the loop ends when
// the process does rather than on its own schedule.
func TestLookupLoopRunsAPassAndStopsWithTheContext(t *testing.T) {
	t.Parallel()
	blobs, meta := filestest.Backends(t)
	service := files.New(blobs, meta)
	thumbs := thumb.New(blobs, service, meta, "ffmpeg", t.TempDir()).WithLookups(meta)

	// One album, so the pass has something to ask about -- and nowhere to
	// ask: the addresses point at a port nothing listens on, which is the
	// shape of a server with no route out.
	if _, err := service.Mkdir(t.Context(), "edu", "music"); err != nil {
		t.Fatal(err)
	}
	f, err := service.Write(t.Context(), "edu", "music/01.flac", strings.NewReader("fLaC"), 4, "audio/flac")
	if err != nil {
		t.Fatal(err)
	}
	if err := meta.PutMedia(t.Context(), db.Media{
		FileID: f.ID, Kind: db.KindAudio, IndexedAt: time.Now(), Version: 1,
		AlbumArtist: "Autechre", Artist: "Autechre", Album: "Amber", Title: "Foil",
	}); err != nil {
		t.Fatal(err)
	}

	// MusicBrainz turns the client away and the other two are nowhere at
	// all, which is the shape of a server with no route out -- and the
	// refusal is what makes the loop plan to come back in a minute rather
	// than on the hour.
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(refusing.Close)
	nowhere := "http://127.0.0.1:1"
	a := New(config.Config{Values: config.Values{MusicLookup: true}}, "test", "2026-01-01T09:30:00Z")
	deps := Deps{
		Storage: blobs, Database: meta, Files: service, Thumbs: thumbs,
		Lookup: lookup.New(meta, thumbs, "test", lookup.WithEndpoints(refusing.URL, nowhere, nowhere), lookup.WithPace(0)),
	}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		a.lookupPeriodically(ctx, deps)
		close(done)
	}()

	// The first pass is armed at once, so the row appears without waiting
	// for the hour.
	deadline := time.Now().Add(2 * time.Second)
	for {
		row, rerr := meta.AlbumLookup(t.Context(), "edu", "Autechre", "Amber")
		if rerr == nil {
			if row.RetryAt.IsZero() || row.Error == "" {
				t.Errorf("an album nobody could ask about is %+v, want a retry with the reason", row)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no pass ran: %v", rerr)
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop did not stop with its context")
	}
}

// TestOpenWiresTheLookupWhenThereIsSomebody: the pass exists whenever there
// is a user to file things under, and whether it asks anything is the setting
// in force, read on every pass (#397) -- nothing leaves the machine by
// default, which TestTheLookupAsksNothingWhileOff holds.
func TestOpenWiresTheLookupWhenThereIsSomebody(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		vars map[string]string
		want bool
	}{
		{"with a user", map[string]string{"STRATUS_USERNAME": "edu", "STRATUS_PASSWORD": "example correct horse battery staple"}, true},
		{"with nobody to file under", map[string]string{"STRATUS_MUSIC_LOOKUP": "on"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			vars := map[string]string{"STRATUS_DATA_DIR": filepath.Join(t.TempDir(), "data"), "STRATUS_INDEX_INTERVAL": "0"}
			for k, v := range tc.vars {
				vars[k] = v
			}
			cfg, err := config.Load(func(key string) string { return vars[key] })
			if err != nil {
				t.Fatal(err)
			}
			deps, err := New(cfg, "test", "2026-01-01T09:30:00Z").open(t.Context())
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer func() { _ = deps.Close() }()
			if got := deps.Lookup != nil; got != tc.want {
				t.Errorf("Lookup wired = %v, want %v", got, tc.want)
			}
			if deps.Settings == nil || deps.Settings.Values().MusicLookup != (vars["STRATUS_MUSIC_LOOKUP"] == "on") {
				t.Error("the settings in force are not what the environment said")
			}
		})
	}
}

// TestLookupLoopComesStraightBackWhileABatchFills: a library larger than a
// batch is finished in passes a few seconds apart rather than one an hour,
// and a pass the index refuses is logged and waited out rather than ending
// the loop.
func TestLookupLoopComesStraightBackWhileABatchFills(t *testing.T) {
	t.Parallel()
	blobs, meta := filestest.Backends(t)
	service := files.New(blobs, meta)
	thumbs := thumb.New(blobs, service, meta, "ffmpeg", t.TempDir()).WithLookups(meta)
	if _, err := service.Mkdir(t.Context(), "edu", "music"); err != nil {
		t.Fatal(err)
	}
	// One more album than a batch holds, so the first pass says there is more.
	for i := range 9 {
		name := "music/" + string(rune('a'+i)) + ".flac"
		f, werr := service.Write(t.Context(), "edu", name, strings.NewReader("fLaC"), 4, "audio/flac")
		if werr != nil {
			t.Fatal(werr)
		}
		if perr := meta.PutMedia(t.Context(), db.Media{
			FileID: f.ID, Kind: db.KindAudio, IndexedAt: time.Now(), Version: 1,
			AlbumArtist: "Artist " + string(rune('a'+i)), Artist: "Artist", Album: "Album", Title: "Track",
		}); perr != nil {
			t.Fatal(perr)
		}
	}

	// The second pass is the one the index refuses, so what it proves is
	// that the loop came back for it, and that a refusal is a log line and a
	// wait rather than the end of the loop.
	nowhere := "http://127.0.0.1:1"
	broken := dbtest.FailAfter(t, meta, "PruneAlbumLookups", 1)
	deps := Deps{
		Storage: blobs, Database: broken, Files: service, Thumbs: thumbs,
		Lookup: lookup.New(broken, thumbs, "test", lookup.WithEndpoints(nowhere, nowhere, nowhere), lookup.WithPace(0)),
	}
	a := New(config.Config{Values: config.Values{MusicLookup: true}}, "test", "2026-01-01T09:30:00Z")
	a.lookupPause = 10 * time.Millisecond

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		a.lookupPeriodically(ctx, deps)
		close(done)
	}()

	// The first pass looks at a batch and says there is more; the second
	// comes back for the ninth within the pause and is refused before it
	// gets there.
	deadline := time.Now().Add(5 * time.Second)
	for {
		counts, cerr := meta.LookupCounts(t.Context())
		if cerr != nil {
			t.Fatal(cerr)
		}
		if counts.AlbumsTried == 8 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of 9 albums were looked at, want the first batch of 8", counts.AlbumsTried)
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("the loop ended on a pass the index refused")
	default:
	}
	counts, err := meta.LookupCounts(t.Context())
	if err != nil || counts.AlbumsTried != 8 {
		t.Errorf("after the refused pass %d albums were looked at, %v; want still 8", counts.AlbumsTried, err)
	}
	cancel()
	<-done
}
