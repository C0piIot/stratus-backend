package subsonic

import (
	"encoding/base64"
	"strconv"
	"strings"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// The API has one id namespace for artists, albums, songs and folders, and a
// client hands an id back without saying what it had asked for. A typed prefix
// is what lets a handler tell them apart.
//
// The value after the prefix is **encoded, not hashed**. Artists and albums are
// not rows here -- an album is a pair of tags -- so the id has to carry them:
// there is no derived table to look a hash up in. That was the first instinct
// and it does not work.
//
// base64url without padding, because an id travels in a query string and the
// tags it encodes contain spaces, slashes, ampersands, plus signs and worse.
const (
	artistPrefix = "ar-"
	albumPrefix  = "al-"
	songPrefix   = "tr-"
	dirPrefix    = "d-"
)

// idSeparator joins the two tags of an album id. NUL rather than a printable
// character because a tag may contain any of those: an album "b" by "a-b"
// must not encode to the same id as an album "a-b" by "a".
const idSeparator = "\x00"

var idEncoding = base64.RawURLEncoding

// songID is the one id built from a row, because a track is one: the client
// sends it back to stream, and a file id is what names the bytes.
func songID(fileID int64) string { return songPrefix + strconv.FormatInt(fileID, 10) }

func artistID(name string) string { return artistPrefix + idEncoding.EncodeToString([]byte(name)) }

func albumID(artist, album string) string {
	return albumPrefix + idEncoding.EncodeToString([]byte(artist+idSeparator+album))
}

func dirID(path string) string { return dirPrefix + idEncoding.EncodeToString([]byte(path)) }

// isSongID and isAlbumID say what kind of thing an id names without decoding
// it, which is what getCoverArt needs: one endpoint takes ids of four kinds and
// has to resolve each differently.
func isSongID(id string) bool { return strings.HasPrefix(id, songPrefix) }

func isAlbumID(id string) bool { return strings.HasPrefix(id, albumPrefix) }

func parseSongID(id string) (int64, bool) {
	rest, ok := strings.CutPrefix(id, songPrefix)
	if !ok {
		return 0, false
	}
	fileID, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || fileID <= 0 {
		return 0, false
	}
	return fileID, true
}

func parseArtistID(id string) (string, bool) {
	rest, ok := strings.CutPrefix(id, artistPrefix)
	if !ok {
		return "", false
	}
	name, err := idEncoding.DecodeString(rest)
	if err != nil || len(name) == 0 {
		return "", false
	}
	return string(name), true
}

func parseAlbumID(id string) (artist, album string, ok bool) {
	rest, ok := strings.CutPrefix(id, albumPrefix)
	if !ok {
		return "", "", false
	}
	decoded, err := idEncoding.DecodeString(rest)
	if err != nil {
		return "", "", false
	}
	artist, album, ok = strings.Cut(string(decoded), idSeparator)
	if !ok || album == "" {
		return "", "", false
	}
	return artist, album, true
}

// parseDirID validates the path it decoded, because the value came from a query
// string: an id a client invented is a path this server would otherwise put
// into a query. The owner filter would keep it harmless, but a listing is not
// the place to find that out.
func parseDirID(id string) (string, bool) {
	rest, ok := strings.CutPrefix(id, dirPrefix)
	if !ok {
		return "", false
	}
	decoded, err := idEncoding.DecodeString(rest)
	if err != nil {
		return "", false
	}
	path := string(decoded)
	if db.ValidateDir(path) != nil {
		return "", false
	}
	return path, true
}
