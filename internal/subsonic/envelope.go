package subsonic

import (
	"encoding/json"
	"encoding/xml"
	"log/slog"
	"net/http"
)

// apiVersion is the Subsonic version this server answers as. Three dotted
// integers because the schema's own pattern says so: "1.16" is not a version a
// strict client will accept.
const apiVersion = "1.16.1"

// serverName is the envelope's type field, which OpenSubsonic made mandatory so
// that a client can adapt to how much of the API a server really implements.
const serverName = "Stratus"

// envelope is a subsonic-response, in both serialisations from one value.
//
// The scalars are XML *attributes* and JSON keys, which is the shape of the
// whole API and the easiest tag in it to get wrong: `xml:"status"` silently
// produces a child element instead, and nothing complains.
//
// At most one payload is set. The schema says so -- its Response is a choice of
// zero or one child -- so every payload here is a pointer that omits itself.
type envelope struct {
	XMLName xml.Name `xml:"http://subsonic.org/restapi subsonic-response" json:"-"`

	Status        string `xml:"status,attr" json:"status"`
	Version       string `xml:"version,attr" json:"version"`
	Type          string `xml:"type,attr" json:"type"`
	ServerVersion string `xml:"serverVersion,attr" json:"serverVersion"`
	OpenSubsonic  bool   `xml:"openSubsonic,attr" json:"openSubsonic"`

	Error        *apiError     `xml:"error,omitempty" json:"error,omitempty"`
	License      *license      `xml:"license,omitempty" json:"license,omitempty"`
	MusicFolders *musicFolders `xml:"musicFolders,omitempty" json:"musicFolders,omitempty"`
	User         *user         `xml:"user,omitempty" json:"user,omitempty"`
	Artists      *artistsList  `xml:"artists,omitempty" json:"artists,omitempty"`
	Artist       *artistDetail `xml:"artist,omitempty" json:"artist,omitempty"`
	Album        *albumDetail  `xml:"album,omitempty" json:"album,omitempty"`
	Song         *child        `xml:"song,omitempty" json:"song,omitempty"`
	Indexes      *indexes      `xml:"indexes,omitempty" json:"indexes,omitempty"`
	Directory    *directory    `xml:"directory,omitempty" json:"directory,omitempty"`

	AlbumList2    *albumList2    `xml:"albumList2,omitempty" json:"albumList2,omitempty"`
	AlbumList     *albumList     `xml:"albumList,omitempty" json:"albumList,omitempty"`
	SearchResult3 *searchResult3 `xml:"searchResult3,omitempty" json:"searchResult3,omitempty"`
	SearchResult2 *searchResult2 `xml:"searchResult2,omitempty" json:"searchResult2,omitempty"`
	Genres        *genres        `xml:"genres,omitempty" json:"genres,omitempty"`
	SongsByGenre  *songList      `xml:"songsByGenre,omitempty" json:"songsByGenre,omitempty"`
	RandomSongs   *songList      `xml:"randomSongs,omitempty" json:"randomSongs,omitempty"`
	Starred2      *starred2      `xml:"starred2,omitempty" json:"starred2,omitempty"`
	Starred       *starred       `xml:"starred,omitempty" json:"starred,omitempty"`

	// Extensions is the one payload that is a bare array on the response rather
	// than the container-and-element pair every legacy payload uses. It has to
	// be present even when empty, so it is a pointer: nil leaves it out, and a
	// pointer to an empty slice renders [].
	Extensions *[]extension `xml:"openSubsonicExtensions,omitempty" json:"openSubsonicExtensions,omitempty"`
}

// apiError is why status is "failed". message is optional in the schema and
// always sent here, because a code alone is not a diagnosis.
type apiError struct {
	Code    int    `xml:"code,attr" json:"code"`
	Message string `xml:"message,attr" json:"message,omitempty"`
}

type license struct {
	Valid bool `xml:"valid,attr" json:"valid"`
}

type musicFolders struct {
	Folders []musicFolder `xml:"musicFolder" json:"musicFolder"`
}

type musicFolder struct {
	// An int, unlike every other id in the API, which are strings. The schema
	// types this one differently and a strict client parses it as a number.
	ID   int    `xml:"id,attr" json:"id"`
	Name string `xml:"name,attr" json:"name"`
}

type extension struct {
	Name     string `xml:"name,attr" json:"name"`
	Versions []int  `xml:"versions" json:"versions"`
}

// user is what a client reads to decide which of its own features to show. Every
// role is answered honestly, including the ones that are false: a client told it
// may make playlists and then refused is a worse experience than one that never
// offers.
type user struct {
	Username            string `xml:"username,attr" json:"username"`
	ScrobblingEnabled   bool   `xml:"scrobblingEnabled,attr" json:"scrobblingEnabled"`
	AdminRole           bool   `xml:"adminRole,attr" json:"adminRole"`
	SettingsRole        bool   `xml:"settingsRole,attr" json:"settingsRole"`
	DownloadRole        bool   `xml:"downloadRole,attr" json:"downloadRole"`
	UploadRole          bool   `xml:"uploadRole,attr" json:"uploadRole"`
	PlaylistRole        bool   `xml:"playlistRole,attr" json:"playlistRole"`
	CoverArtRole        bool   `xml:"coverArtRole,attr" json:"coverArtRole"`
	CommentRole         bool   `xml:"commentRole,attr" json:"commentRole"`
	PodcastRole         bool   `xml:"podcastRole,attr" json:"podcastRole"`
	StreamRole          bool   `xml:"streamRole,attr" json:"streamRole"`
	JukeboxRole         bool   `xml:"jukeboxRole,attr" json:"jukeboxRole"`
	ShareRole           bool   `xml:"shareRole,attr" json:"shareRole"`
	VideoConversionRole bool   `xml:"videoConversionRole,attr" json:"videoConversionRole"`
}

// ok is an envelope with nothing in it yet.
func (h *handler) ok() envelope {
	return envelope{
		Status:        "ok",
		Version:       apiVersion,
		Type:          serverName,
		ServerVersion: h.serverVersion,
		OpenSubsonic:  true,
	}
}

// write renders one envelope in the format the request asked for.
//
// XML is the default, and that is the specification's default rather than a
// preference: f is optional, so a client that omits it is asking for XML, and a
// server that only spoke JSON would not be conformant.
//
// Everything here answers HTTP 200, errors included. The status lives in the
// envelope, and a client that gets a transport error instead cannot read it.
func (h *handler) write(w http.ResponseWriter, r *http.Request, env envelope) {
	if r.URL.Query().Get("f") != "json" {
		h.writeXML(w, r, env, "application/xml; charset=utf-8")
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	body := struct {
		Response envelope `json:"subsonic-response"`
	}{env}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.WarnContext(r.Context(), "writing a subsonic response", "err", err)
	}
}

func (h *handler) writeXML(w http.ResponseWriter, r *http.Request, env envelope, contentType string) {
	w.Header().Set("Content-Type", contentType)
	if _, err := w.Write([]byte(xml.Header)); err != nil {
		return
	}
	if err := xml.NewEncoder(w).Encode(env); err != nil {
		slog.WarnContext(r.Context(), "writing a subsonic response", "err", err)
	}
}

// fail renders an error envelope. It is the only place status becomes "failed".
func (h *handler) fail(w http.ResponseWriter, r *http.Request, e apiError) {
	h.write(w, r, failed(h.ok(), e))
}

// failXML is fail for an endpoint that answers bytes, where the specification
// requires text/xml whatever f said. A client asking for a stream is not
// parsing JSON, and the type it is told to expect is the one it gets.
func (h *handler) failXML(w http.ResponseWriter, r *http.Request, e apiError) {
	h.writeXML(w, r, failed(h.ok(), e), "text/xml; charset=utf-8")
}

func failed(env envelope, e apiError) envelope {
	env.Status = "failed"
	env.Error = &e
	return env
}
