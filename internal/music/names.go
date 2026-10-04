package music

import (
	"cmp"
	"slices"
	"strings"
	"unicode"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// Names for things that are not files, so that they can be served as if they
// were (#279).
//
// A playlist is a row, an artist and an album are tags, and all three are
// offered over WebDAV as a collection or a file -- and linked to from a page
// at the same address. So the name has to be generated, and it has to be
// generated once: the page and the mount agreeing about what something is
// called is the whole reason this lives below both of them.
//
// Three rules, and each is the same rule the photographs' names follow:
//
//   - A name is one path element. A slash would make it a folder, and the
//     rest are what Windows refuses in a file name -- and Windows mounts this.
//   - Two names that fold to the same thing are a collision. Finder and
//     Windows mount this on filesystems that fold case, and there the second
//     would hide the first.
//   - A name is stable. Ties are broken in an order that does not change when
//     something else arrives: the row id where there is one, the tag itself
//     where there is not.

// Segment is a tag or a title as one path element every client can hold.
//
// A name of nothing, or of dots, is not a file; what it becomes instead says
// what was missing rather than pretending the thing is nameless.
func Segment(name, whenEmpty string) string {
	name = strings.Map(func(r rune) rune {
		if strings.ContainsRune(`/\<>:"|?*`, r) || unicode.IsControl(r) {
			return '_'
		}
		return r
	}, strings.TrimSpace(name))
	if strings.Trim(name, ".") == "" {
		return whenEmpty
	}
	return name
}

// unique gives each of things a name from want, disambiguating collisions with
// db.CopyName in the order they arrive. The caller has already put them in the
// order that makes a name stable.
func unique[T any](things []T, want func(T) string) (map[string]T, []string) {
	named := make(map[string]T, len(things))
	taken := make(map[string]bool, len(things))
	order := make([]string, 0, len(things))
	for _, t := range things {
		base := want(t)
		name := base
		for n := 2; taken[strings.ToLower(name)]; n++ {
			name = db.CopyName(base, n)
		}
		taken[strings.ToLower(name)] = true
		named[name] = t
		order = append(order, name)
	}
	return named, order
}

// PlaylistNames gives every playlist a file name, in id order so that a name
// is stable: the older of two playlists called "Mix" keeps "Mix.m3u8" and the
// newer is "Mix (2).m3u8" whatever they are renamed to later.
func PlaylistNames(playlists []db.Playlist) (map[string]db.Playlist, []string) {
	byID := slices.Clone(playlists)
	slices.SortFunc(byID, func(a, b db.Playlist) int { return cmp.Compare(a.ID, b.ID) })
	return unique(byID, func(pl db.Playlist) string { return Segment(pl.Name, "Playlist") + ".m3u8" })
}

// ArtistNames names the artists. They have no id, so the tie is broken by the
// tag itself: two artists whose names differ only in a slash would otherwise
// swap places between two requests.
func ArtistNames(artists []db.Artist) (map[string]db.Artist, []string) {
	byTag := slices.Clone(artists)
	slices.SortFunc(byTag, func(a, b db.Artist) int { return cmp.Compare(a.Name, b.Name) })
	return unique(byTag, func(a db.Artist) string { return Segment(a.Name, "Unknown artist") })
}

// AlbumNames names one artist's albums, by the same rule.
func AlbumNames(albums []db.Album) (map[string]db.Album, []string) {
	byTag := slices.Clone(albums)
	slices.SortFunc(byTag, func(a, b db.Album) int { return cmp.Compare(a.Name, b.Name) })
	return unique(byTag, func(a db.Album) string { return Segment(a.Name, "Unknown album") })
}

// TrackNames names an album's tracks after their files, in id order, like the
// photographs in a month.
func TrackNames(tracks []db.Track) (map[string]db.Track, []string) {
	byID := slices.Clone(tracks)
	slices.SortFunc(byID, func(a, b db.Track) int { return cmp.Compare(a.File.ID, b.File.ID) })
	return unique(byID, func(t db.Track) string {
		base := t.File.Path[strings.LastIndex(t.File.Path, "/")+1:]
		return Segment(base, "Track")
	})
}
