package subsonic_test

import (
	"net/url"
	"strings"
	"testing"
)

func playlistEntries(t *testing.T, l *library, id string) (map[string]any, []string) {
	t.Helper()
	p, _ := mustOK(t, l, "getPlaylist", "id", id)["playlist"].(map[string]any)
	return p, names(t, p["entry"], "title")
}

func call(t *testing.T, l *library, method string, params url.Values) map[string]any {
	t.Helper()
	q := url.Values{"c": {"stratus-tests"}, "u": {username}, "p": {password}, "f": {"json"}}
	for k, v := range params {
		q[k] = v
	}
	return response(t, get(t, l, method, q.Encode()))
}

// TestAPlaylistEndToEnd is the acceptance of #196 over the protocol: made,
// listed, opened, edited by index, and a deleted track dropped out of it.
func TestAPlaylistEndToEnd(t *testing.T) {
	t.Parallel()
	l, hunter, rotar := starredLibrary(t)

	env := call(t, l, "createPlaylist", url.Values{"name": {"Mix"}, "songId": {rotar, hunter, rotar}})
	created, _ := env["playlist"].(map[string]any)
	id, _ := created["id"].(string)
	if !strings.HasPrefix(id, "pl-") {
		t.Fatalf("createPlaylist answered %v, want the playlist", env)
	}
	if got := names(t, created["entry"], "title"); !same(got, []string{"Rotar", "Hunter", "Rotar"}) {
		t.Errorf("created entries = %v", got)
	}

	list, _ := mustOK(t, l, "getPlaylists")["playlists"].(map[string]any)
	items, _ := list["playlist"].([]any)
	if len(items) != 1 {
		t.Fatalf("getPlaylists = %v", list)
	}
	p, _ := items[0].(map[string]any)
	for key, want := range map[string]any{
		"name": "Mix", "owner": username, "public": false, "songCount": float64(3), "duration": float64(764),
	} {
		if p[key] != want {
			t.Errorf("getPlaylists %s = %#v, want %#v", key, p[key], want)
		}
	}
	for _, key := range []string{"created", "changed"} {
		if s, _ := p[key].(string); s == "" {
			t.Errorf("getPlaylists %s is missing", key)
		}
	}
	if _, present := p["coverArt"]; present {
		t.Errorf("a playlist declares coverArt, which nothing fills: %v", p)
	}

	// Remove the first Rotar by index, add Hunter again, rename, make public.
	mustOK(t, l, "updatePlaylist", "playlistId", id, "songIndexToRemove", "0", "songIdToAdd", hunter,
		"name", "Mixtape", "public", "true", "comment", "side A")
	p, got := playlistEntries(t, l, id)
	if !same(got, []string{"Hunter", "Rotar", "Hunter"}) {
		t.Errorf("after the edit entries = %v", got)
	}
	if p["name"] != "Mixtape" || p["public"] != true || p["comment"] != "side A" {
		t.Errorf("after the edit = %v", p)
	}

	// An entry carries the same annotations every other row does.
	mustOK(t, l, "star", "id", hunter)
	p, _ = playlistEntries(t, l, id)
	if entries, _ := p["entry"].([]any); len(entries) > 0 {
		if _, starred := entries[0].(map[string]any)["starred"]; !starred {
			t.Errorf("a starred entry does not say so: %v", entries[0])
		}
	}

	// Deleting the file takes it out of the playlist.
	f, _ := l.files.Stat(t.Context(), username, "music/Homogenic/01 Hunter.flac")
	if err := l.files.Remove(t.Context(), username, f.Path); err != nil {
		t.Fatal(err)
	}
	if _, got := playlistEntries(t, l, id); !same(got, []string{"Rotar"}) {
		t.Errorf("after deleting a track entries = %v", got)
	}

	mustOK(t, l, "deletePlaylist", "id", id)
	if code := errorCode(t, get(t, l, "getPlaylist", query("f", "json", "id", id))); code != 70 {
		t.Errorf("getPlaylist after delete = %v, want 70", code)
	}
}

// TestCreatePlaylistWithAnIDReplaces is the specification's second use of the
// same call: what the playlist holds becomes the songs sent.
func TestCreatePlaylistWithAnIDReplaces(t *testing.T) {
	t.Parallel()
	l, hunter, rotar := starredLibrary(t)

	created, _ := call(t, l, "createPlaylist", url.Values{"name": {"Mix"}, "songId": {hunter}})["playlist"].(map[string]any)
	id, _ := created["id"].(string)

	env := call(t, l, "createPlaylist", url.Values{"playlistId": {id}, "songId": {rotar, rotar}})
	replaced, _ := env["playlist"].(map[string]any)
	if replaced["name"] != "Mix" {
		t.Errorf("name = %v, want it kept when none is sent", replaced["name"])
	}
	if got := names(t, replaced["entry"], "title"); !same(got, []string{"Rotar", "Rotar"}) {
		t.Errorf("entries = %v, want only what was sent", got)
	}
}

func TestPlaylistRefusals(t *testing.T) {
	t.Parallel()
	l, hunter, _ := starredLibrary(t)
	created, _ := call(t, l, "createPlaylist", url.Values{"name": {"Mix"}, "songId": {hunter}})["playlist"].(map[string]any)
	id, _ := created["id"].(string)

	tests := []struct {
		name   string
		method string
		params url.Values
		want   float64
	}{
		{"somebody else's playlists", "getPlaylists", url.Values{"username": {"someone-else"}}, 50},
		{"a playlist with no id", "getPlaylist", url.Values{}, 10},
		{"a playlist id that is not one", "getPlaylist", url.Values{"id": {hunter}}, 70},
		{"a playlist that is not there", "getPlaylist", url.Values{"id": {"pl-424242"}}, 70},
		{"a playlist id with no number", "getPlaylist", url.Values{"id": {"pl-first"}}, 70},
		{"creating with no name", "createPlaylist", url.Values{"songId": {hunter}}, 10},
		{"creating with a song that is not one", "createPlaylist", url.Values{"name": {"x"}, "songId": {"nonsense"}}, 70},
		{"creating with a song that is not there", "createPlaylist", url.Values{"name": {"x"}, "songId": {songIDOf(424242)}}, 70},
		{"replacing a playlist id that is not one", "createPlaylist", url.Values{"playlistId": {"nonsense"}}, 70},
		{"replacing a playlist that is not there", "createPlaylist", url.Values{"playlistId": {"pl-424242"}}, 70},
		{"updating with no id", "updatePlaylist", url.Values{}, 10},
		{"updating a playlist id that is not one", "updatePlaylist", url.Values{"playlistId": {"nonsense"}}, 70},
		{"updating a playlist that is not there", "updatePlaylist", url.Values{"playlistId": {"pl-424242"}}, 70},
		{"an index past the end", "updatePlaylist", url.Values{"playlistId": {id}, "songIndexToRemove": {"1"}}, 10},
		{"an index that is not a number", "updatePlaylist", url.Values{"playlistId": {id}, "songIndexToRemove": {"first"}}, 10},
		{"public that is not a boolean", "updatePlaylist", url.Values{"playlistId": {id}, "public": {"maybe"}}, 10},
		{"adding a song that is not one", "updatePlaylist", url.Values{"playlistId": {id}, "songIdToAdd": {"nonsense"}}, 70},
		{"adding a song that is not there", "updatePlaylist", url.Values{"playlistId": {id}, "songIdToAdd": {songIDOf(424242)}}, 70},
		{"deleting with no id", "deletePlaylist", url.Values{}, 10},
		{"deleting a playlist id that is not one", "deletePlaylist", url.Values{"id": {"nonsense"}}, 70},
		{"deleting a playlist that is not there", "deletePlaylist", url.Values{"id": {"pl-424242"}}, 70},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if code := errorCode(t, get(t, l, tt.method, func() string {
				q := url.Values{"c": {"stratus-tests"}, "u": {username}, "p": {password}, "f": {"json"}}
				for k, v := range tt.params {
					q[k] = v
				}
				return q.Encode()
			}())); code != tt.want {
				t.Errorf("code = %v, want %v", code, tt.want)
			}
		})
	}

	// None of the refused edits touched it.
	p, got := playlistEntries(t, l, id)
	if !same(got, []string{"Hunter"}) || p["public"] != false {
		t.Errorf("after the refusals = %v with %v", p, got)
	}

	// Asking for your own by name is the same as not asking.
	list, _ := mustOK(t, l, "getPlaylists", "username", username)["playlists"].(map[string]any)
	if items, _ := list["playlist"].([]any); len(items) != 1 {
		t.Errorf("getPlaylists for yourself = %v", list)
	}
}
