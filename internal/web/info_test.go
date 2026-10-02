package web_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/media"
)

// indexed writes a file and the row an extractor of this build would have left
// beside it: the validator of the bytes that are there and this build's
// version, which is what makes the page show what was found rather than say it
// is waiting to be read.
func indexed(t *testing.T, s *files.Service, meta db.Store, name, body string, m db.Media) db.File {
	t.Helper()
	types := map[db.Kind]string{
		db.KindImage: "image/jpeg", db.KindAudio: "audio/flac", db.KindVideo: "video/mp4",
	}
	f, err := s.Write(t.Context(), username, name, strings.NewReader(body), int64(len(body)), types[m.Kind])
	if err != nil {
		t.Fatal(err)
	}
	m.FileID = f.ID
	if m.ETag == "" {
		m.ETag = f.ETag
	}
	if m.Version == 0 {
		m.Version = media.Version
	}
	if m.IndexedAt.IsZero() {
		m.IndexedAt = time.Now()
	}
	if err := meta.PutMedia(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	return f
}

func details(t *testing.T, h http.Handler, path string, cookie *http.Cookie) string {
	t.Helper()
	rec := get(t, h, "/info/"+path, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /info/%s = %d: %s", path, rec.Code, rec.Body)
	}
	return rec.Body.String()
}

// has fails with the whole page when one of the strings is not on it, which is
// what makes a formatting change readable rather than a bare false.
func has(t *testing.T, body string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Errorf("the page does not say %q:\n%s", w, body)
		}
	}
}

func TestDetailsNeedASession(t *testing.T) {
	t.Parallel()
	h := newHandler(t, nil)

	rec := get(t, h, "/info/a.jpg")
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/login") {
		t.Errorf("/info/a.jpg with no session = %d %q, want the login form",
			rec.Code, rec.Header().Get("Location"))
	}
}

// TestAPhotographSaysWhatTheCameraSaid: the file row's half and the indexer's,
// on one page.
func TestAPhotographSaysWhatTheCameraSaid(t *testing.T) {
	t.Parallel()
	h, s, meta := browserOver(t)
	f := indexed(t, s, meta, "holiday.jpg", "pixels", db.Media{
		Kind:    db.KindImage,
		TakenAt: time.Date(2024, 6, 2, 18, 45, 0, 0, time.UTC),
		Width:   4032, Height: 3024,
		Camera: "Canon EOS R6",
		GPS:    &db.GPS{Latitude: 41.3879, Longitude: 2.1699},
	})

	body := details(t, h, "holiday.jpg", signIn(t, h))
	has(t, body, "holiday.jpg", "image/jpeg", "6 B (6 bytes)", f.ETag,
		"2 June 2024, 18:45", "4032 × 3024", "Canon EOS R6", "41.38790, 2.16990")
}

func TestATrackSaysWhatTheTagsSaid(t *testing.T) {
	t.Parallel()
	h, s, meta := browserOver(t)
	indexed(t, s, meta, "song.flac", "sound", db.Media{
		Kind: db.KindAudio, DurationMS: 225_000,
		Codec: "flac", Bitrate: 960_000, SampleRate: 44_100, Channels: 2, BitDepth: 16,
		Title: "Hyperballad", Artist: "Björk", Album: "Post", AlbumArtist: "Björk",
		Genre: "Electronic", TrackNo: 3, Year: 1995,
	})

	body := details(t, h, "song.flac", signIn(t, h))
	has(t, body, "3:45", "flac", "960 kbps", "44.1 kHz", "stereo", "16-bit",
		"Hyperballad", "Post", "Electronic", "1995")
	// The album artist is the track's artist, which the line above already
	// said: a second identical line is noise rather than information.
	if strings.Contains(body, "Album artist") {
		t.Errorf("an album artist equal to the artist took a line of its own:\n%s", body)
	}
}

// TestAFilmSaysWhatAPlayerWouldNeed: the stream facts a transcode decision is
// made from are the ones this page exists to show (#197, #207, #50).
func TestAFilmSaysWhatAPlayerWouldNeed(t *testing.T) {
	t.Parallel()
	h, s, meta := browserOver(t)
	indexed(t, s, meta, "film.mp4", "frames", db.Media{
		Kind: db.KindVideo, DurationMS: 6_750_000, Width: 3840, Height: 2160,
		Codec: "hevc", CodecProfile: "Main 10", Level: 51, FrameRate: 23_976,
		Bitrate: 42_000_000, BitDepth: 10,
		ColorPrimaries: "bt2020", ColorTransfer: "smpte2084", ColorSpace: "bt2020nc",
		DoViProfile: 8,
		AudioCodec:  "eac3", Channels: 6, SampleRate: 48_000,
	})

	body := details(t, h, "film.mp4", signIn(t, h))
	has(t, body, "1:52:30", "3840 × 2160", "hevc Main 10 level 5.1", "23.976 fps",
		"42.0 Mbps", "10-bit", "smpte2084", "Dolby Vision profile 8",
		"eac3, 6 channels, 48 kHz")
}

func TestAFileNothingHasReadYetSaysSo(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	write(t, s, "notes.txt", "nothing in here")

	body := details(t, h, "notes.txt", signIn(t, h))
	has(t, body, "Nothing has read this file yet", "text/plain")
}

// TestAFileWithNothingInItToReadSaysThatInstead: the difference between a file
// nothing has been near and one there was nothing in.
func TestAFileWithNothingInItToReadSaysThatInstead(t *testing.T) {
	t.Parallel()
	h, s, meta := browserOver(t)
	indexed(t, s, meta, "notes.txt", "where we went", db.Media{Kind: db.KindOther})

	has(t, details(t, h, "notes.txt", signIn(t, h)), "nothing in a file of this kind")
}

// TestFactsAboutBytesThatAreGoneAreNotShown is the reason the row carries the
// validator: a replaced file keeps its id, so metadata that outlived its bytes
// would otherwise describe the wrong picture.
func TestFactsAboutBytesThatAreGoneAreNotShown(t *testing.T) {
	t.Parallel()
	h, s, meta := browserOver(t)
	indexed(t, s, meta, "replaced.jpg", "pixels", db.Media{
		Kind: db.KindImage, ETag: "the-bytes-before", Camera: "Canon EOS R6",
	})

	body := details(t, h, "replaced.jpg", signIn(t, h))
	has(t, body, "has been replaced since it was read")
	if strings.Contains(body, "Canon EOS R6") {
		t.Errorf("the page describes bytes that are gone:\n%s", body)
	}
}

func TestAFileNothingCouldParseSaysWhy(t *testing.T) {
	t.Parallel()
	h, s, meta := browserOver(t)
	cookie := signIn(t, h)
	indexed(t, s, meta, "broken.mp4", "junk", db.Media{
		Kind: db.KindVideo, Error: "ffprobe found no streams",
	})
	indexed(t, s, meta, "unreached.mp4", "junk", db.Media{
		Kind: db.KindVideo, Error: "the store timed out",
		RetryAt: time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC),
	})

	// A verdict on the file: nothing will look at it again, so nothing is
	// promised.
	body := details(t, h, "broken.mp4", cookie)
	has(t, body, "ffprobe found no streams")
	if strings.Contains(body, "tried again") {
		t.Errorf("a file nothing can parse was promised another attempt:\n%s", body)
	}

	// A verdict on our reach, which says nothing about the bytes (#157).
	has(t, details(t, h, "unreached.mp4", cookie),
		"the store timed out", "tried again after 4 March 2026, 09:00")
}

// TestAnOlderExtractorStillSaysWhatItFound: the facts are about the bytes that
// are there, unlike a stale validator's, so they are shown and labelled.
func TestAnOlderExtractorStillSaysWhatItFound(t *testing.T) {
	t.Parallel()
	h, s, meta := browserOver(t)
	indexed(t, s, meta, "old.jpg", "pixels", db.Media{
		Kind: db.KindImage, Version: 1, Camera: "Canon EOS R6",
	})

	has(t, details(t, h, "old.jpg", signIn(t, h)), "Canon EOS R6", "queued to be read again")
}

// TestTheRowAndThePageAreTheSameFacts is the condition htmx was let in under:
// one URL, a piece of the same HTML for the listing and the whole document for
// a browser that followed the link.
func TestTheRowAndThePageAreTheSameFacts(t *testing.T) {
	t.Parallel()
	h, s, meta := browserOver(t)
	cookie := signIn(t, h)
	indexed(t, s, meta, "holiday.jpg", "pixels", db.Media{
		Kind: db.KindImage, Camera: "Canon EOS R6",
	})

	row := htmx(t, h, "/info/holiday.jpg", cookie).Body.String()
	if !strings.HasPrefix(strings.TrimSpace(row), "<tr") || strings.Contains(row, "<!doctype") {
		t.Errorf("htmx was given a document rather than a row:\n%s", row)
	}
	has(t, row, "Canon EOS R6", "<details open")

	page := details(t, h, "holiday.jpg", cookie)
	if !strings.Contains(page, "<!doctype html>") || strings.Contains(page, "<tr") {
		t.Errorf("the page is not a whole document:\n%s", page)
	}
	has(t, page, "Canon EOS R6")
}

// TestTheListingOffersDetailsOnFilesOnly: a directory is a row and nothing
// else, and a shared listing offers nothing that writes or inspects.
func TestTheListingOffersDetailsOnFilesOnly(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	mkdir(t, s, "holiday")
	write(t, s, "notes.txt", "where we went")

	body := get(t, h, "/files/", signIn(t, h)).Body.String()
	has(t, body, `hx-get="/info/notes.txt"`, `href="/info/notes.txt"`)
	if strings.Contains(body, "/info/holiday") {
		t.Errorf("a folder was offered details:\n%s", body)
	}

	if shared := get(t, h, linkTo(t, h, "holiday", "7d")).Body.String(); strings.Contains(shared, "/info/") {
		t.Errorf("a shared listing offers details:\n%s", shared)
	}
}

// TestDetailsOfAFolder is the URL nothing links to: it is reachable by hand and
// answers rather than falling over.
func TestDetailsOfAFolder(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	mkdir(t, s, "holiday")

	has(t, details(t, h, "holiday", signIn(t, h)), "no bytes here for the indexer to read")
}
