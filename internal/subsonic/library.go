package subsonic

import (
	"mime"
	"path"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// The browse payloads.
//
// Two rules shape every struct here, and both are easy to get wrong silently:
//
//   - Only fields this server actually fills are declared. OpenSubsonic treats
//     the presence of a field as the claim that the server supports it, so a
//     declared-and-always-empty field is a lie a client acts on.
//   - Every slice is allocated even when empty. In XML an empty slice writes
//     nothing either way, but a nil slice is `null` in JSON and clients iterate
//     what they are given.
//
// durations are **seconds** here and milliseconds everywhere else in this
// project, because that is what the schema says.

// artistsList is getArtists: the whole library grouped by initial.
type artistsList struct {
	// IgnoredArticles is the list of leading words a client should ignore when
	// it sorts. Empty because this server ignores none -- present because its
	// absence would mean something else.
	IgnoredArticles string       `xml:"ignoredArticles,attr" json:"ignoredArticles"`
	Indexes         []indexEntry `xml:"index" json:"index"`
}

// indexes is getIndexes, the folder-browsing counterpart of artistsList. The
// entries in it are directories rather than artists, which is the protocol's
// design and not an approximation of ours: the element is called artist.
type indexes struct {
	// LastModified is milliseconds since the epoch, and a client caches
	// against it.
	LastModified    int64        `xml:"lastModified,attr" json:"lastModified"`
	IgnoredArticles string       `xml:"ignoredArticles,attr" json:"ignoredArticles"`
	Indexes         []indexEntry `xml:"index" json:"index"`
	// Children are the playable files at the top level, which have no folder
	// to be listed under.
	Children []child `xml:"child" json:"child"`
}

type indexEntry struct {
	Name    string      `xml:"name,attr" json:"name"`
	Artists []artistRef `xml:"artist" json:"artist"`
}

// artistRef is one entry in an index, and it serves both indexes: an artist
// counts its albums, a folder has none to count, and the field is omitted
// rather than sent as zero.
type artistRef struct {
	ID         string `xml:"id,attr" json:"id"`
	Name       string `xml:"name,attr" json:"name"`
	AlbumCount int    `xml:"albumCount,attr,omitempty" json:"albumCount,omitempty"`
}

// artistDetail is getArtist: one artist and its albums.
type artistDetail struct {
	ID         string     `xml:"id,attr" json:"id"`
	Name       string     `xml:"name,attr" json:"name"`
	AlbumCount int        `xml:"albumCount,attr" json:"albumCount"`
	Albums     []albumRef `xml:"album" json:"album"`
}

// albumRef is an album without its tracks, which is what a listing shows.
type albumRef struct {
	ID        string `xml:"id,attr" json:"id"`
	Name      string `xml:"name,attr" json:"name"`
	Artist    string `xml:"artist,attr" json:"artist"`
	ArtistID  string `xml:"artistId,attr" json:"artistId"`
	SongCount int    `xml:"songCount,attr" json:"songCount"`
	Duration  int    `xml:"duration,attr" json:"duration"`
	Created   string `xml:"created,attr" json:"created"`
	Year      int    `xml:"year,attr,omitempty" json:"year,omitempty"`
	Genre     string `xml:"genre,attr,omitempty" json:"genre,omitempty"`
}

// albumDetail is getAlbum: the same album with its tracks.
type albumDetail struct {
	albumRef
	Songs []child `xml:"song" json:"song"`
}

// directory is getMusicDirectory: one folder of the real file tree.
type directory struct {
	ID       string  `xml:"id,attr" json:"id"`
	Name     string  `xml:"name,attr" json:"name"`
	Parent   string  `xml:"parent,attr,omitempty" json:"parent,omitempty"`
	Children []child `xml:"child" json:"child"`
}

// child is the schema's Child, which is one type for a song and for a folder
// and is why isDir exists. It is serialised under two different element names,
// so the name lives on the field that holds it and not here.
type child struct {
	ID     string `xml:"id,attr" json:"id"`
	Parent string `xml:"parent,attr,omitempty" json:"parent,omitempty"`
	// IsDir carries no omitempty: false is the answer for every song, and a
	// client reads it to decide whether the id can be played.
	IsDir bool   `xml:"isDir,attr" json:"isDir"`
	Title string `xml:"title,attr" json:"title"`

	Album       string `xml:"album,attr,omitempty" json:"album,omitempty"`
	Artist      string `xml:"artist,attr,omitempty" json:"artist,omitempty"`
	AlbumID     string `xml:"albumId,attr,omitempty" json:"albumId,omitempty"`
	ArtistID    string `xml:"artistId,attr,omitempty" json:"artistId,omitempty"`
	Track       int    `xml:"track,attr,omitempty" json:"track,omitempty"`
	DiscNumber  int    `xml:"discNumber,attr,omitempty" json:"discNumber,omitempty"`
	Year        int    `xml:"year,attr,omitempty" json:"year,omitempty"`
	Genre       string `xml:"genre,attr,omitempty" json:"genre,omitempty"`
	Size        int64  `xml:"size,attr,omitempty" json:"size,omitempty"`
	ContentType string `xml:"contentType,attr,omitempty" json:"contentType,omitempty"`
	Suffix      string `xml:"suffix,attr,omitempty" json:"suffix,omitempty"`
	Duration    int    `xml:"duration,attr,omitempty" json:"duration,omitempty"`
	BitRate     int    `xml:"bitRate,attr,omitempty" json:"bitRate,omitempty"`
	Path        string `xml:"path,attr,omitempty" json:"path,omitempty"`
	Created     string `xml:"created,attr,omitempty" json:"created,omitempty"`
	// Type is "music" for a track. The other values in the schema are for
	// surfaces this server does not have.
	Type string `xml:"type,attr,omitempty" json:"type,omitempty"`
}

// songOf renders a track as a Child.
//
// The bit rate is computed rather than read: the extractor does not record one,
// and bytes times eight over milliseconds is kilobits per second exactly. A
// client shows it, and some decide their buffer from it.
func songOf(t db.Track) child {
	c := child{
		ID:          songID(t.File.ID),
		Parent:      dirID(db.ParentOf(t.File.Path)),
		Title:       titleOf(t),
		Album:       t.Media.Album,
		Artist:      t.Media.Artist,
		Track:       t.Media.TrackNo,
		DiscNumber:  t.Media.DiscNo,
		Year:        t.Media.Year,
		Genre:       t.Media.Genre,
		Size:        t.File.Size,
		ContentType: t.File.MIMEType,
		Suffix:      suffixOf(t.File.Path),
		Duration:    seconds(t.Media.DurationMS),
		Path:        t.File.Path,
		Created:     stamp(t.File.MTime),
		Type:        "music",
	}
	if t.Media.AlbumArtist != "" && t.Media.Album != "" {
		c.AlbumID = albumID(t.Media.AlbumArtist, t.Media.Album)
		c.ArtistID = artistID(t.Media.AlbumArtist)
	}
	if t.Media.DurationMS > 0 {
		c.BitRate = int(t.File.Size * 8 / t.Media.DurationMS)
	}
	return c
}

// folderOf renders a directory row as a Child.
func folderOf(f db.File) child {
	return child{
		ID:      dirID(f.Path),
		Parent:  dirID(db.ParentOf(f.Path)),
		IsDir:   true,
		Title:   path.Base(f.Path),
		Created: stamp(f.MTime),
	}
}

func albumOf(a db.Album) albumRef {
	return albumRef{
		ID:        albumID(a.Artist, a.Name),
		Name:      a.Name,
		Artist:    a.Artist,
		ArtistID:  artistID(a.Artist),
		SongCount: a.SongCount,
		Duration:  seconds(a.DurationMS),
		Created:   stamp(a.Created),
		Year:      a.Year,
		Genre:     a.Genre,
	}
}

// titleOf falls back to the file name. A track with no title tag would
// otherwise be a blank row a client cannot even select.
func titleOf(t db.Track) string {
	if t.Media.Title != "" {
		return t.Media.Title
	}
	return path.Base(t.File.Path)
}

func suffixOf(p string) string {
	return strings.TrimPrefix(strings.ToLower(path.Ext(p)), ".")
}

// seconds is what the schema wants everywhere a duration appears. Rounded
// rather than truncated, so a track of 254.6 seconds is not 254.
func seconds(ms int64) int { return int((ms + 500) / 1000) }

// stamp is the created format the schema asks for. Every time reaching it came
// out of a NOT NULL column, so there is no missing case to handle.
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// indexArtists groups a name-ordered list under its initials, which is what
// both index endpoints answer with. Anything not starting with a letter goes
// under "#": clients render the initials as a jump bar, and one entry per
// punctuation mark is not a jump bar.
func indexArtists(entries []artistRef) []indexEntry {
	out := make([]indexEntry, 0, len(entries))
	for _, e := range entries {
		name := initialOf(e.Name)
		if len(out) == 0 || out[len(out)-1].Name != name {
			out = append(out, indexEntry{Name: name, Artists: make([]artistRef, 0, 1)})
		}
		last := &out[len(out)-1]
		last.Artists = append(last.Artists, e)
	}
	return out
}

func initialOf(name string) string {
	r, size := utf8.DecodeRuneInString(name)
	if size == 0 || !unicode.IsLetter(r) {
		return "#"
	}
	return string(unicode.ToUpper(r))
}

// attachment is the header that makes a browser save a download rather than
// play it. mime.FormatMediaType encodes a name that is not plain ASCII, which
// a hand-built header does not and half a music library needs.
func attachment(name string) string {
	return mime.FormatMediaType("attachment", map[string]string{"filename": name})
}
