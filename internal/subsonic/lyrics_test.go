package subsonic_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stratus-private-cloud/stratus-backend/internal/db"
)

// What this file is about: what outside sources said about the library
// (#217), answered the way the protocol asks for it. The rows are written
// here by hand, since the pass that fills them is internal/lookup's and what
// is under test is the shape a client reads.

func TestGetLyricsBySongIdAnswersBothForms(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)
	f := l.add(t, "music/Homogenic/01 Hunter.flac", song("Björk", "Homogenic", "Hunter", 1))
	if err := l.meta.PutLyrics(t.Context(), db.Lyrics{
		FileID: f.ID, ETag: f.ETag, FetchedAt: time.Now(),
		Plain:  "If travel is searching\nAnd home what's been found",
		Synced: "[00:12.50] If travel is searching\n[00:17.025] And home what's been found\n[ar: Björk]",
	}); err != nil {
		t.Fatal(err)
	}

	env := mustOK(t, l, "getLyricsBySongId", "id", songIDOf(f.ID))
	list, _ := env["lyricsList"].(map[string]any)
	forms, _ := list["structuredLyrics"].([]any)
	if len(forms) != 2 {
		t.Fatalf("structuredLyrics = %v, want the synced and the plain form", list)
	}

	synced, _ := forms[0].(map[string]any)
	if synced["synced"] != true || synced["displayTitle"] != "Hunter" || synced["displayArtist"] != "Björk" || synced["lang"] != "und" {
		t.Errorf("the synced form is %v", synced)
	}
	lines, _ := synced["line"].([]any)
	if len(lines) != 2 {
		t.Fatalf("the synced form has %d lines, want 2 with the tag line dropped: %v", len(lines), lines)
	}
	first, _ := lines[0].(map[string]any)
	second, _ := lines[1].(map[string]any)
	// Milliseconds, from hundredths on one line and thousandths on the other.
	if first["start"] != 12500.0 || first["value"] != "If travel is searching" {
		t.Errorf("the first line is %v", first)
	}
	if second["start"] != 17025.0 {
		t.Errorf("the second line is %v", second)
	}

	plain, _ := forms[1].(map[string]any)
	if plain["synced"] != false {
		t.Errorf("the plain form says it is synced: %v", plain)
	}
	plainLines, _ := plain["line"].([]any)
	if len(plainLines) != 2 {
		t.Fatalf("the plain form has %d lines: %v", len(plainLines), plainLines)
	}
	if line, _ := plainLines[0].(map[string]any); line["value"] != "If travel is searching" || line["start"] != nil {
		t.Errorf("a plain line carries a start, or the wrong words: %v", line)
	}
}

// TestGetLyricsBySongIdWithNothingKnown: an empty list and not an error, which
// is what a client draws nothing for -- and what every track answers with the
// setting off.
func TestGetLyricsBySongIdWithNothingKnown(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)
	f := l.add(t, "music/Homogenic/01 Hunter.flac", song("Björk", "Homogenic", "Hunter", 1))

	env := mustOK(t, l, "getLyricsBySongId", "id", songIDOf(f.ID))
	list, ok := env["lyricsList"].(map[string]any)
	if !ok {
		t.Fatalf("lyricsList = %v, want an object", env["lyricsList"])
	}
	if forms, ok := list["structuredLyrics"].([]any); !ok || len(forms) != 0 {
		t.Errorf("structuredLyrics = %#v, want an empty array", list["structuredLyrics"])
	}

	// And the extension is advertised, since this is it.
	ext := response(t, get(t, l, "getOpenSubsonicExtensions", "f=json"))
	if !strings.Contains(fmt.Sprint(ext["openSubsonicExtensions"]), "songLyrics") {
		t.Errorf("songLyrics is not advertised: %v", ext["openSubsonicExtensions"])
	}
}

func TestGetLyricsBySongIdForWhatIsNotASong(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)
	for _, id := range []string{songIDOf(424242), albumIDOf("Björk", "Homogenic"), "nonsense"} {
		env := response(t, get(t, l, "getLyricsBySongId", query("f", "json", "id", id)))
		apiErr, _ := env["error"].(map[string]any)
		if apiErr["code"] != 70.0 {
			t.Errorf("getLyricsBySongId(%q) = %v, want error 70", id, env)
		}
	}
	env := response(t, get(t, l, "getLyricsBySongId", query("f", "json")))
	if apiErr, _ := env["error"].(map[string]any); apiErr["code"] != 10.0 {
		t.Errorf("getLyricsBySongId with no id = %v, want error 10", env)
	}
}

func TestGetAlbumInfo2(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)
	f := l.add(t, "music/Homogenic/01 Hunter.flac", song("Björk", "Homogenic", "Hunter", 1))
	l.add(t, "music/Post/01 Army of Me.flac", song("Björk", "Post", "Army of Me", 1))
	const group = "4cf8ba42-5d0b-3a4d-8e8f-4c8f2f3d1a1b"
	if err := l.meta.PutAlbumLookup(t.Context(), db.AlbumLookup{
		Owner: username, Artist: "Björk", Album: "Homogenic", LookedUpAt: time.Now(), ReleaseGroup: group,
	}); err != nil {
		t.Fatal(err)
	}

	// By the album's id, and by one of its songs, which the specification
	// allows either of.
	for _, id := range []string{albumIDOf("Björk", "Homogenic"), songIDOf(f.ID)} {
		env := mustOK(t, l, "getAlbumInfo2", "id", id)
		info, _ := env["albumInfo"].(map[string]any)
		if info["musicBrainzId"] != group {
			t.Errorf("getAlbumInfo2(%q) = %v, want the release group", id, env["albumInfo"])
		}
	}

	// An album nobody has looked up answers an empty albumInfo: the element
	// is required, and there is nothing to put in it.
	env := mustOK(t, l, "getAlbumInfo2", "id", albumIDOf("Björk", "Post"))
	info, ok := env["albumInfo"].(map[string]any)
	if !ok || len(info) != 0 {
		t.Errorf("getAlbumInfo2 of an album not looked up = %#v, want an empty albumInfo", env["albumInfo"])
	}

	// No id at all is the parameter error, like everywhere else.
	env = response(t, get(t, l, "getAlbumInfo2", query("f", "json")))
	if apiErr, _ := env["error"].(map[string]any); apiErr["code"] != 10.0 {
		t.Errorf("getAlbumInfo2 with no id = %v, want error 10", env)
	}

	// And one the library does not have is not found, like getAlbum.
	for _, id := range []string{albumIDOf("Björk", "Vespertine"), artistIDOf("Björk"), songIDOf(424242), "al-%%%"} {
		env := response(t, get(t, l, "getAlbumInfo2", query("f", "json", "id", id)))
		if apiErr, _ := env["error"].(map[string]any); apiErr["code"] != 70.0 {
			t.Errorf("getAlbumInfo2(%q) = %v, want error 70", id, env)
		}
	}
}

// TestCoverArtOfAnAlbumInAFlatFolderIsItsOwn is #217's flat-library bug on
// this surface: every album in one folder, a fetched cover for one of them,
// and getCoverArt for the other has to say there is none rather than hand
// over the neighbour's.
func TestCoverArtOfAnAlbumInAFlatFolderIsItsOwn(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)
	// Path order puts this one first in the folder.
	l.add(t, "music/80KIDZ - This Is My Shit.flac", song("80KIDZ", "This Is My Shit", "This Is My Shit", 1))
	obel := l.add(t, "music/Agnes Obel - Familiar.flac", song("Agnes Obel", "Aventine", "Familiar", 1))
	if err := l.art.StoreArt(t.Context(), obel, jpegBytes(t, 500, 500)); err != nil {
		t.Fatal(err)
	}
	if err := l.meta.PutAlbumLookup(t.Context(), db.AlbumLookup{
		Owner: username, Artist: "Agnes Obel", Album: "Aventine", LookedUpAt: time.Now(),
		ReleaseGroup: "4cf8ba42-5d0b-3a4d-8e8f-4c8f2f3d1a1b", CoverBlob: obel.BlobKey,
	}); err != nil {
		t.Fatal(err)
	}

	if rec := get(t, l, "getCoverArt", query("id", albumIDOf("Agnes Obel", "Aventine"), "size", "96")); rec.Code != 200 || rec.Header().Get("Content-Type") != "image/jpeg" {
		t.Errorf("the album with a fetched cover = %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	if rec := get(t, l, "getCoverArt", query("id", songIDOf(obel.ID), "size", "96")); rec.Code != 200 || rec.Header().Get("Content-Type") != "image/jpeg" {
		t.Errorf("a song of that album = %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	// A binary endpoint's errors are XML whatever f says.
	rec := get(t, l, "getCoverArt", query("id", albumIDOf("80KIDZ", "This Is My Shit"), "size", "96"))
	if !strings.Contains(rec.Body.String(), `code="70"`) {
		t.Errorf("the album with no cover of its own = %d %q, want error 70 rather than its neighbour's picture", rec.Code, rec.Body.String())
	}
}
