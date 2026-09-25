package subsonic_test

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// TestBrowsingByTagFollowsItsOwnIDs is the assertion that matters most about
// the id scheme: every id a client is handed has to work when it is handed
// back. So the test never builds one -- it reads getArtists, follows the id it
// finds to getArtist, that one to getAlbum, and that one to getSong.
//
// A hash-based id passes every other test in this file and fails this one,
// which is how the design error was caught before any code was written.
func TestBrowsingByTagFollowsItsOwnIDs(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)
	l.add(t, "music/Homogenic/01 Hunter.flac", song("Björk", "Homogenic", "Hunter", 1))
	l.add(t, "music/Homogenic/02 Joga.flac", song("Björk", "Homogenic", "Joga", 2))

	env := response(t, get(t, l, "getArtists", query("f", "json")))
	artists, ok := env["artists"].(map[string]any)
	if !ok {
		t.Fatalf("no artists object in %v", env)
	}
	// Present and empty rather than absent: a client sorts by it.
	if _, isString := artists["ignoredArticles"].(string); !isString {
		t.Errorf("ignoredArticles is missing: %v", artists)
	}
	index := only(t, artists["index"])
	if got, _ := index["name"].(string); got != "B" {
		t.Errorf("index name = %v, want B", index["name"])
	}
	artist := only(t, index["artist"])
	if got, _ := artist["name"].(string); got != "Björk" {
		t.Errorf("artist name = %v", artist["name"])
	}
	if got, _ := artist["albumCount"].(float64); got != 1 {
		t.Errorf("albumCount = %v, want 1", artist["albumCount"])
	}

	env = response(t, get(t, l, "getArtist", query("f", "json", "id", str(t, artist["id"]))))
	detail, ok := env["artist"].(map[string]any)
	if !ok {
		t.Fatalf("no artist object in %v", env)
	}
	album := only(t, detail["album"])
	if got, _ := album["name"].(string); got != "Homogenic" {
		t.Errorf("album name = %v", album["name"])
	}
	if got, _ := album["songCount"].(float64); got != 2 {
		t.Errorf("songCount = %v, want 2", album["songCount"])
	}
	// Seconds, not milliseconds, and rounded: two tracks of 254.6s each.
	if got, _ := album["duration"].(float64); got != 509 {
		t.Errorf("duration = %v, want 509 seconds", album["duration"])
	}
	if got, _ := album["created"].(string); got == "" {
		t.Error("created is empty, and the schema requires it on an album")
	}

	env = response(t, get(t, l, "getAlbum", query("f", "json", "id", str(t, album["id"]))))
	full, ok := env["album"].(map[string]any)
	if !ok {
		t.Fatalf("no album object in %v", env)
	}
	songs, ok := full["song"].([]any)
	if !ok || len(songs) != 2 {
		t.Fatalf("song = %#v, want two tracks", full["song"])
	}
	first, _ := songs[0].(map[string]any)
	if got, _ := first["title"].(string); got != "Hunter" {
		t.Errorf("the tracks are out of order: %v", songs)
	}
	// isDir is false and present, because a client reads it to decide whether
	// the id it has can be played at all.
	if isDir, isBool := first["isDir"].(bool); !isBool || isDir {
		t.Errorf("isDir = %#v, want false", first["isDir"])
	}
	if got, _ := first["albumId"].(string); got != str(t, album["id"]) {
		t.Errorf("the song points at another album: %v", first["albumId"])
	}

	env = response(t, get(t, l, "getSong", query("f", "json", "id", str(t, first["id"]))))
	one, ok := env["song"].(map[string]any)
	if !ok {
		t.Fatalf("no song object in %v", env)
	}
	if got, _ := one["title"].(string); got != "Hunter" {
		t.Errorf("getSong = %v", one)
	}
}

// TestACompilationIsOneAlbum is the protocol-level version of the case that
// justified album_artist: filed under the track artist this is two albums, and
// a client shows two rows of one track each.
func TestACompilationIsOneAlbum(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)

	xtal := song("Various Artists", "Warp10", "Xtal", 1)
	xtal.Artist = "Aphex Twin"
	basscadet := song("Various Artists", "Warp10", "Basscadet", 2)
	basscadet.Artist = "Autechre"
	l.add(t, "music/Warp10/01.flac", xtal)
	l.add(t, "music/Warp10/02.flac", basscadet)

	env := response(t, get(t, l, "getArtists", query("f", "json")))
	artists, _ := env["artists"].(map[string]any)
	index := only(t, artists["index"])
	artist := only(t, index["artist"])
	if got, _ := artist["name"].(string); got != "Various Artists" {
		t.Fatalf("artist = %v, want the album artist", artist["name"])
	}

	env = response(t, get(t, l, "getArtist", query("f", "json", "id", str(t, artist["id"]))))
	detail, _ := env["artist"].(map[string]any)
	album := only(t, detail["album"])
	if got, _ := album["songCount"].(float64); got != 2 {
		t.Errorf("songCount = %v, want the compilation kept together", album["songCount"])
	}

	// And the tracks keep their own artists, which is the other half of it.
	env = response(t, get(t, l, "getAlbum", query("f", "json", "id", str(t, album["id"]))))
	full, _ := env["album"].(map[string]any)
	songs, _ := full["song"].([]any)
	if len(songs) != 2 {
		t.Fatalf("song = %#v", full["song"])
	}
	one, _ := songs[0].(map[string]any)
	two, _ := songs[1].(map[string]any)
	if one["artist"] == two["artist"] {
		t.Errorf("the tracks lost their own artists: %v", songs)
	}
}

// TestBrowsingByFolder is the other half of the client population: DSub
// browses this way unless its preferences are changed, and it is the half
// Navidrome had to fake out of tags. Here the folders are real.
func TestBrowsingByFolder(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)
	l.add(t, "music/Homogenic/01 Hunter.flac", song("Björk", "Homogenic", "Hunter", 1))
	l.add(t, "music/Homogenic/extras/demo.flac", song("Björk", "Homogenic", "Demo", 1))
	// A photo beside the music, which a music client cannot play.
	cover := song("Björk", "Homogenic", "Cover", 1)
	cover.Kind = db.KindImage
	l.add(t, "music/Homogenic/cover.jpg", cover)
	// And a file nothing has indexed yet, which is not in the library.
	l.addUnindexed(t, "music/Homogenic/02 Joga.flac")
	// One track at the top level, which has no folder to be listed under.
	l.add(t, "loose.flac", song("Loose", "Singles", "Loose", 1))

	env := response(t, get(t, l, "getIndexes", query("f", "json")))
	list, ok := env["indexes"].(map[string]any)
	if !ok {
		t.Fatalf("no indexes object in %v", env)
	}
	if got, _ := list["lastModified"].(float64); got <= 0 {
		t.Errorf("lastModified = %v, want a timestamp a client can cache against", list["lastModified"])
	}
	loose := only(t, list["child"])
	if got, _ := loose["title"].(string); got != "Loose" {
		t.Errorf("the root's own track is missing: %v", list["child"])
	}
	index := only(t, list["index"])
	folder := only(t, index["artist"])
	if got, _ := folder["name"].(string); got != "music" {
		t.Fatalf("the top level = %v, want the one directory", index)
	}
	// A folder has no albums to count, so the field is left out rather than
	// sent as a zero a client would render.
	if _, present := folder["albumCount"]; present {
		t.Errorf("a folder reports an album count: %v", folder)
	}

	// Down one level, to the folder holding the album.
	env = response(t, get(t, l, "getMusicDirectory", query("f", "json", "id", str(t, folder["id"]))))
	dir, ok := env["directory"].(map[string]any)
	if !ok {
		t.Fatalf("no directory object in %v", env)
	}
	if got, _ := dir["name"].(string); got != "music" {
		t.Errorf("name = %v", dir["name"])
	}
	album := only(t, dir["child"])
	if isDir, _ := album["isDir"].(bool); !isDir {
		t.Errorf("the album folder is not a directory: %v", album)
	}

	env = response(t, get(t, l, "getMusicDirectory", query("f", "json", "id", str(t, album["id"]))))
	dir, _ = env["directory"].(map[string]any)
	children, ok := dir["child"].([]any)
	if !ok {
		t.Fatalf("child = %#v", dir["child"])
	}
	// Directories first, then the tracks: one subfolder and one playable file.
	// The photo and the unindexed file are neither.
	var names []string
	for _, c := range children {
		m, _ := c.(map[string]any)
		names = append(names, str(t, m["title"]))
	}
	if len(names) != 2 || names[0] != "extras" || names[1] != "Hunter" {
		t.Errorf("child = %v, want the subfolder and the one indexed track", names)
	}
	// The parent leads back up, which is how a folder client walks out again.
	if got, _ := dir["parent"].(string); got != str(t, folder["id"]) {
		t.Errorf("parent = %v, want the folder above", dir["parent"])
	}
}

// TestAnEmptyDirectoryIsNotAMissingOne is why the listing is statted rather
// than inferred: WebDAV can make an empty folder, and answering "no such
// directory" for one a client can see over the other protocol is a lie.
func TestAnEmptyDirectoryIsNotAMissingOne(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)
	l.mkdirAll(t, "music/empty")

	env := response(t, get(t, l, "getIndexes", query("f", "json")))
	list, _ := env["indexes"].(map[string]any)
	index := only(t, list["index"])
	folder := only(t, index["artist"])

	env = response(t, get(t, l, "getMusicDirectory", query("f", "json", "id", str(t, folder["id"]))))
	dir, _ := env["directory"].(map[string]any)
	empty := only(t, dir["child"])

	env = response(t, get(t, l, "getMusicDirectory", query("f", "json", "id", str(t, empty["id"]))))
	if got, _ := env["status"].(string); got != "ok" {
		t.Fatalf("an empty directory = %v, want ok: %v", env["status"], env["error"])
	}
	dir, _ = env["directory"].(map[string]any)
	// An array rather than null, because a client iterates what it is given.
	if kids, ok := dir["child"].([]any); !ok || len(kids) != 0 {
		t.Errorf("child = %#v, want an empty array", dir["child"])
	}
}

// TestAnEmptyLibraryAnswersContainers is the shape a client sees on its first
// connection, and the one that crashes a naive one: a missing array is
// undefined where an empty array is iterable.
func TestAnEmptyLibraryAnswersContainers(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)

	env := response(t, get(t, l, "getArtists", query("f", "json")))
	artists, _ := env["artists"].(map[string]any)
	if idx, ok := artists["index"].([]any); !ok || len(idx) != 0 {
		t.Errorf("index = %#v, want an empty array", artists["index"])
	}

	env = response(t, get(t, l, "getIndexes", query("f", "json")))
	list, _ := env["indexes"].(map[string]any)
	if idx, ok := list["index"].([]any); !ok || len(idx) != 0 {
		t.Errorf("index = %#v, want an empty array", list["index"])
	}
	if kids, ok := list["child"].([]any); !ok || len(kids) != 0 {
		t.Errorf("child = %#v, want an empty array", list["child"])
	}
	// Nothing to cache against, and not a large negative number either.
	if got, _ := list["lastModified"].(float64); got != 0 {
		t.Errorf("lastModified = %v, want 0 for an empty library", list["lastModified"])
	}
}

// TestIDsSurviveTheQueryString is what base64url is for. Every one of these
// characters means something in a URL or in the id format itself, and a client
// sends an id back verbatim.
func TestIDsSurviveTheQueryString(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)

	const artist = "AC/DC & Friends +1"
	const album = "Live? #2 100%"
	l.add(t, "music/odd.flac", song(artist, album, "Odd", 1))

	env := response(t, get(t, l, "getArtists", query("f", "json")))
	artists, _ := env["artists"].(map[string]any)
	index := only(t, artists["index"])
	ref := only(t, index["artist"])

	env = response(t, get(t, l, "getArtist", query("f", "json", "id", str(t, ref["id"]))))
	detail, ok := env["artist"].(map[string]any)
	if !ok {
		t.Fatalf("the artist id did not survive: %v", env)
	}
	if got, _ := detail["name"].(string); got != artist {
		t.Errorf("name = %q, want %q", got, artist)
	}
	one := only(t, detail["album"])

	env = response(t, get(t, l, "getAlbum", query("f", "json", "id", str(t, one["id"]))))
	full, ok := env["album"].(map[string]any)
	if !ok {
		t.Fatalf("the album id did not survive: %v", env)
	}
	if got, _ := full["name"].(string); got != album {
		t.Errorf("name = %q, want %q", got, album)
	}
}

// TestIDsThatNameNothing covers what a client can send that this server did not
// hand out: an invented id, another owner's file, and a photo asked for as a
// song. All of them are 70, because from the client's side they are.
func TestIDsThatNameNothing(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)
	l.add(t, "music/ok.flac", song("Björk", "Homogenic", "Hunter", 1))

	// A file that exists and belongs to somebody else, so the owner filter is
	// what has to refuse it rather than the id being wrong.
	theirs, err := l.meta.PutFile(t.Context(), db.File{
		OwnerID: "someone-else", Path: "music/theirs.flac", BlobKey: "blobs/theirs", Size: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = l.meta.PutMedia(t.Context(), db.Media{FileID: theirs.ID, Kind: db.KindAudio, Version: 1}); err != nil {
		t.Fatal(err)
	}
	// A photo of this owner's, which is a row but not a song.
	photo := song("Björk", "Homogenic", "Cover", 1)
	photo.Kind = db.KindImage
	cover := l.add(t, "music/cover.jpg", photo)

	tests := []struct {
		name   string
		method string
		id     string
		want   float64
	}{
		{name: "no id at all", method: "getArtist", id: "", want: 10},
		{name: "no id on an album", method: "getAlbum", id: "", want: 10},
		{name: "no id on a song", method: "getSong", id: "", want: 10},
		{name: "no id on a directory", method: "getMusicDirectory", id: "", want: 10},
		{name: "no id on a stream", method: "stream", id: "", want: 10},
		{name: "an id with no prefix", method: "getArtist", id: "Björk", want: 70},
		{name: "an artist id that is not base64", method: "getArtist", id: "ar-!!!!", want: 70},
		{name: "an empty artist id", method: "getArtist", id: "ar-", want: 70},
		{name: "an album id with only one half", method: "getAlbum", id: "al-QmpvcmsK", want: 70},
		{name: "an album id that is not base64", method: "getAlbum", id: "al-!!!!", want: 70},
		{name: "a song id that is not a number", method: "getSong", id: "tr-abc", want: 70},
		{name: "a song id of zero", method: "getSong", id: "tr-0", want: 70},
		{name: "a song id nothing has", method: "getSong", id: "tr-99999", want: 70},
		{name: "a directory id that is not base64", method: "getMusicDirectory", id: "d-!!!!", want: 70},
		{name: "a directory id that is not a path", method: "getMusicDirectory", id: dirIDOf("../etc"), want: 70},
		{name: "a directory nothing has", method: "getMusicDirectory", id: dirIDOf("nowhere"), want: 70},
		{name: "a file used as a directory", method: "getMusicDirectory", id: dirIDOf("music/ok.flac"), want: 70},
		{name: "an artist with no albums", method: "getArtist", id: "ar-bm9ib2R5", want: 70},
		{name: "an album this artist does not have", method: "getAlbum", id: albumIDOf("Björk", "Vespertine"), want: 70},
		{name: "another owner's track", method: "getSong", id: songIDOf(theirs.ID), want: 70},
		{name: "another owner's track, streamed", method: "stream", id: songIDOf(theirs.ID), want: 70},
		{name: "a photo asked for as a song", method: "getSong", id: songIDOf(cover.ID), want: 70},
		{name: "a photo asked for as a stream", method: "stream", id: songIDOf(cover.ID), want: 70},
		{name: "an album id on a stream", method: "stream", id: "al-QmpvcmsAQQ", want: 70},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			q := url.Values{"c": {"tests"}, "u": {username}, "p": {password}, "f": {"json"}}
			if tt.id != "" {
				q.Set("id", tt.id)
			}
			rec := get(t, l, tt.method, q.Encode())
			// The binary endpoints answer XML whatever f said, so the JSON
			// decoder cannot read them.
			if tt.method == "stream" {
				assertXMLError(t, rec, tt.want)
				return
			}
			if code := errorCode(t, rec); code != tt.want {
				t.Errorf("code = %v, want %v", code, tt.want)
			}
		})
	}
}

// TestTheBitRateIsTheStreams: what the extractor read, in kilobits, and the
// stream's facts beside it. Not the file's size over its duration, which
// counts a cover as sound.
func TestTheBitRateIsTheStreams(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)

	m := song("Björk", "Homogenic", "Hunter", 1)
	m.DurationMS = 1000
	m.Bitrate, m.SampleRate, m.Channels, m.BitDepth = 2_304_400, 96_000, 2, 24
	f := l.addSized(t, "music/hunter.flac", m, 400_000)

	env := response(t, get(t, l, "getSong", query("f", "json", "id", songIDOf(f.ID))))
	one, _ := env["song"].(map[string]any)
	for field, want := range map[string]float64{
		"bitRate": 2304, "samplingRate": 96_000, "channelCount": 2, "bitDepth": 24,
	} {
		if got, _ := one[field].(float64); got != want {
			t.Errorf("%s = %v, want %v", field, one[field], want)
		}
	}
}

// TestTheBitRateFallsBackToTheFile for a row that has no stream bitrate: bytes
// times eight over milliseconds is kilobits per second, close enough to show.
func TestTheBitRateFallsBackToTheFile(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)

	m := song("Björk", "Homogenic", "Hunter", 1)
	m.DurationMS = 1000
	f := l.addSized(t, "music/one-second.flac", m, 16_000)

	env := response(t, get(t, l, "getSong", query("f", "json", "id", songIDOf(f.ID))))
	one, _ := env["song"].(map[string]any)
	if got, _ := one["bitRate"].(float64); got != 128 {
		t.Errorf("bitRate = %v, want 128: 16000 bytes over one second", one["bitRate"])
	}
}

// TestATrackWithNoTitleIsNamedAfterItsFile because a row with no title is one
// a client cannot even select, and an untagged file is the normal state of a
// library somebody is still tidying up.
func TestATrackWithNoTitleIsNamedAfterItsFile(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)

	m := song("Björk", "Homogenic", "", 1)
	f := l.add(t, "music/03 Unravel.flac", m)

	env := response(t, get(t, l, "getSong", query("f", "json", "id", songIDOf(f.ID))))
	one, _ := env["song"].(map[string]any)
	if got, _ := one["title"].(string); got != "03 Unravel.flac" {
		t.Errorf("title = %q, want the file name", got)
	}
	// The rest of what a client needs to play it is there either way.
	if got, _ := one["suffix"].(string); got != "flac" {
		t.Errorf("suffix = %v", one["suffix"])
	}
	if got, _ := one["contentType"].(string); got != "audio/flac" {
		t.Errorf("contentType = %v", one["contentType"])
	}
	if got, _ := one["path"].(string); got != "music/03 Unravel.flac" {
		t.Errorf("path = %v", one["path"])
	}
}

// TestStreamServesTheStoredBytes is the whole point of the surface. Nothing is
// transcoded, so what comes back is what went in.
func TestStreamServesTheStoredBytes(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)
	const path = "music/Homogenic/01 Hunter.flac"
	f := l.add(t, path, song("Björk", "Homogenic", "Hunter", 1))

	rec := get(t, l, "stream", query("id", songIDOf(f.ID)))
	if rec.Code != http.StatusOK {
		t.Fatalf("stream = %d, want 200", rec.Code)
	}
	if rec.Body.String() != path {
		t.Errorf("stream returned %q", rec.Body.String())
	}
	// The type the file was uploaded with, not one guessed from the bytes.
	if got := rec.Header().Get("Content-Type"); got != "audio/flac" {
		t.Errorf("Content-Type = %q", got)
	}
	// Nothing offers to save a stream.
	if got := rec.Header().Get("Content-Disposition"); got != "" {
		t.Errorf("Content-Disposition = %q, want none on a stream", got)
	}

	// Ranges come from http.ServeContent over the seekable reader, which is
	// the same path WebDAV serves a video from. Seeking inside a track is what
	// depends on it.
	ranged := get(t, l, "stream", query("id", songIDOf(f.ID)), "Range", "bytes=6-10")
	if ranged.Code != http.StatusPartialContent {
		t.Fatalf("a range request = %d, want 206", ranged.Code)
	}
	if got, want := ranged.Body.String(), path[6:11]; got != want {
		t.Errorf("the range = %q, want %q", got, want)
	}
}

// TestDownloadOffersTheFile differs from a stream in exactly one header, and
// the encoding of it is not decoration: half a music library has a name that is
// not ASCII.
func TestDownloadOffersTheFile(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)
	f := l.add(t, "music/Björk – Jóga.flac", song("Björk", "Homogenic", "Joga", 1))

	rec := get(t, l, "download", query("id", songIDOf(f.ID)))
	if rec.Code != http.StatusOK {
		t.Fatalf("download = %d, want 200", rec.Code)
	}
	got := rec.Header().Get("Content-Disposition")
	if !strings.HasPrefix(got, "attachment;") {
		t.Fatalf("Content-Disposition = %q", got)
	}
	// RFC 2231, which is what a header carrying a non-ASCII name has to use.
	if !strings.Contains(got, "filename*=utf-8''") {
		t.Errorf("Content-Disposition = %q, want the name encoded", got)
	}
}

// TestABinaryErrorIsXML is a rule of the specification and a trap: the client
// asked for audio, so it is not parsing JSON, and telling it the type is
// application/json while sending XML is worse than either.
func TestABinaryErrorIsXML(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)

	// f=json is deliberate: it must not change the answer here.
	rec := get(t, l, "stream", query("f", "json", "id", "tr-99999"))
	assertXMLError(t, rec, 70)

	// Including a refusal to authenticate, which happens before the id is read.
	q := url.Values{"c": {"tests"}, "u": {username}, "p": {"not it"}, "f": {"json"}, "id": {"tr-1"}}
	assertXMLError(t, get(t, l, "download", q.Encode()), 40)
}

func assertXMLError(t *testing.T, rec *httptest.ResponseRecorder, want float64) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d: a Subsonic error is a 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/xml; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/xml on a binary endpoint", got)
	}
	if code := "code=\"" + strconv.FormatFloat(want, 'f', -1, 64) + "\""; !strings.Contains(rec.Body.String(), code) {
		t.Errorf("%s is not in %s", code, rec.Body.String())
	}
}

// only unwraps a JSON array that has to hold exactly one object, which most of
// these cases are: one artist, one album, one folder.
func only(t *testing.T, v any) map[string]any {
	t.Helper()
	list, ok := v.([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("got %#v, want exactly one entry", v)
	}
	m, ok := list[0].(map[string]any)
	if !ok {
		t.Fatalf("got %#v, want an object", list[0])
	}
	return m
}

func str(t *testing.T, v any) string {
	t.Helper()
	s, ok := v.(string)
	if !ok {
		t.Fatalf("got %#v, want a string", v)
	}
	return s
}

// songIDOf and dirIDOf build the ids the tests that are about *bad* ids need.
// The tests about good ones never call them: they follow what the server gave.
func songIDOf(fileID int64) string { return "tr-" + strconv.FormatInt(fileID, 10) }

func dirIDOf(path string) string { return "d-" + encodeID(path) }

func artistIDOf(name string) string { return "ar-" + encodeID(name) }

func albumIDOf(artist, album string) string { return "al-" + encodeID(artist+"\x00"+album) }

func encodeID(v string) string { return base64.RawURLEncoding.EncodeToString([]byte(v)) }
