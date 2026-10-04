package music

import (
	"bytes"
	"fmt"
	"net/url"
	"strings"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// M3U8 renders a playlist in the extended format every player reads: a header,
// then for each entry a line of what it is and a line of where it is.
//
// Here rather than in the WebDAV adapter because the same file is served from
// two places now (#279) -- the mount and the page at the same address -- and a
// generated file that two halves generate differently is a file that changes
// when you look at it from the other side.
//
// Every entry is a URL rooted at the server, under filesPrefix, so a player
// that opens the playlist from here resolves it against the same host. What
// that does not serve is a copy synced to a local disk, where that is not a
// path, and it is not meant to.
func M3U8(pl db.Playlist, tracks []db.Track, filesPrefix string) []byte {
	filesPrefix = strings.TrimSuffix(filesPrefix, "/")

	var b bytes.Buffer
	b.WriteString("#EXTM3U\n")
	fmt.Fprintf(&b, "#PLAYLIST:%s\n", oneLine(pl.Name))
	for _, t := range tracks {
		title := t.Media.Title
		if title == "" {
			title = t.File.Path[strings.LastIndex(t.File.Path, "/")+1:]
		}
		if t.Media.Artist != "" {
			title = t.Media.Artist + " - " + title
		}
		fmt.Fprintf(&b, "#EXTINF:%d,%s\n", (t.Media.DurationMS+500)/1000, oneLine(title))
		b.WriteString(filesPrefix + "/" + escapePath(t.File.Path) + "\n")
	}
	return b.Bytes()
}

// oneLine keeps a tag from ending the line it is on: a newline in a title would
// otherwise be read as the next entry's URL.
func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' {
			return ' '
		}
		return r
	}, s)
}

func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}
