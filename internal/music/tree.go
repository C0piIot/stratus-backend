package music

import (
	"context"
	"os"
	"strings"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// The library as folders by tag: /Autechre/Amber/ holds that album's tracks,
// wherever their files were filed (#279).
//
// It exists for the reason internal/photos does: these addresses are served
// twice, as a WebDAV collection and as the pages at the same URLs, and the two
// have to agree about what everything is called. The names are generated --
// a tag is not a filename -- so the generator lives below both adapters.
//
// What it is not is a second music library. OpenSubsonic at /rest/ is what a
// music client speaks; this is for the clients that speak only WebDAV, and for
// a root collection with no dead end in it.

// Source is what the tree reads: the index by tag, and nothing else.
type Source interface {
	Artists(ctx context.Context, owner string) ([]db.Artist, error)
	Albums(ctx context.Context, owner, artist string) ([]db.Album, error)
	Tracks(ctx context.Context, owner, artist, album string) ([]db.Track, error)
}

// Node is one place in the tree: the root, an artist, an album or a track.
//
// It carries both halves of every level -- the tag the index knows and the
// segment this tree serves it as -- because a caller needs the first to ask
// another question and the second to build a link.
type Node struct {
	Artist     db.Artist
	Album      db.Album
	Track      db.Track
	ArtistName string
	AlbumName  string
	TrackName  string
	Dir        bool
}

// Base is what this node is called inside its parent.
func (n Node) Base() string {
	switch {
	case n.TrackName != "":
		return n.TrackName
	case n.AlbumName != "":
		return n.AlbumName
	}
	return n.ArtistName
}

// Path is the node's address under the mount, with no leading slash.
func (n Node) Path() string {
	parts := make([]string, 0, 3)
	for _, p := range []string{n.ArtistName, n.AlbumName, n.TrackName} {
		if p == "" {
			break
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, "/")
}

// level is one listing: the things by the name they are served as, the name
// each tag is served as, and the order to show them in -- which is the
// index's and not the naming's. Tracks are named in file-id order so a name
// is stable, and listed in disc and track order because that is an album.
type level[T any] struct {
	named map[string]T
	name  map[string]string
	order []T
}

// Tree is one owner's library by tag, for the length of one request.
//
// It caches what it reads for the reason photos.Tree does: resolving a path
// and then listing what is in it asks the same questions twice, and each
// answer is a query.
type Tree struct {
	source Source
	owner  string

	artists *level[db.Artist]
	albums  map[string]*level[db.Album]
	tracks  map[string]*level[db.Track]
}

// NewTree makes the tree for owner. Not New: that is the playlist service,
// which this package had first.
func NewTree(source Source, owner string) *Tree {
	return &Tree{
		source: source, owner: owner,
		albums: map[string]*level[db.Album]{},
		tracks: map[string]*level[db.Track]{},
	}
}

// Resolve turns a path under the mount into what is at it, or os.ErrNotExist.
func (t *Tree) Resolve(ctx context.Context, p string) (Node, error) {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	if parts[0] == "" {
		return Node{Dir: true}, nil
	}
	if len(parts) > 3 {
		return Node{}, os.ErrNotExist
	}

	artists, err := t.artistLevel(ctx)
	if err != nil {
		return Node{}, err
	}
	artist, ok := artists.named[parts[0]]
	if !ok {
		return Node{}, os.ErrNotExist
	}
	n := Node{Artist: artist, ArtistName: parts[0], Dir: true}
	if len(parts) == 1 {
		return n, nil
	}

	albums, err := t.albumLevel(ctx, artist.Name)
	if err != nil {
		return Node{}, err
	}
	album, ok := albums.named[parts[1]]
	if !ok {
		return Node{}, os.ErrNotExist
	}
	n.Album, n.AlbumName = album, parts[1]
	if len(parts) == 2 {
		return n, nil
	}

	tracks, err := t.trackLevel(ctx, artist.Name, album.Name)
	if err != nil {
		return Node{}, err
	}
	track, ok := tracks.named[parts[2]]
	if !ok {
		return Node{}, os.ErrNotExist
	}
	n.Track, n.TrackName, n.Dir = track, parts[2], false
	return n, nil
}

// Children lists what is inside a node, in the order the index gives: artists
// and albums by name, an album's tracks by disc and track number.
func (t *Tree) Children(ctx context.Context, n Node) ([]Node, error) {
	switch {
	case n.ArtistName == "":
		artists, err := t.artistLevel(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]Node, 0, len(artists.order))
		for _, a := range artists.order {
			out = append(out, Node{Artist: a, ArtistName: artists.name[a.Name], Dir: true})
		}
		return out, nil
	case n.AlbumName == "":
		albums, err := t.albumLevel(ctx, n.Artist.Name)
		if err != nil {
			return nil, err
		}
		out := make([]Node, 0, len(albums.order))
		for _, a := range albums.order {
			out = append(out, Node{
				Artist: n.Artist, ArtistName: n.ArtistName,
				Album: a, AlbumName: albums.name[a.Name], Dir: true,
			})
		}
		return out, nil
	case n.TrackName == "":
		tracks, err := t.trackLevel(ctx, n.Artist.Name, n.Album.Name)
		if err != nil {
			return nil, err
		}
		out := make([]Node, 0, len(tracks.order))
		for _, tr := range tracks.order {
			out = append(out, Node{
				Artist: n.Artist, ArtistName: n.ArtistName,
				Album: n.Album, AlbumName: n.AlbumName,
				Track: tr, TrackName: tracks.name[tr.File.Path],
			})
		}
		return out, nil
	}
	return nil, os.ErrNotExist
}

// artistLevel is every album artist, named.
func (t *Tree) artistLevel(ctx context.Context) (*level[db.Artist], error) {
	if t.artists != nil {
		return t.artists, nil
	}
	artists, err := t.source.Artists(ctx, t.owner)
	if err != nil {
		return nil, err
	}
	named, _ := ArtistNames(artists)
	t.artists = &level[db.Artist]{named: named, name: reverse(named, func(a db.Artist) string { return a.Name }), order: artists}
	return t.artists, nil
}

// albumLevel is one artist's albums, named.
func (t *Tree) albumLevel(ctx context.Context, artist string) (*level[db.Album], error) {
	if l, ok := t.albums[artist]; ok {
		return l, nil
	}
	albums, err := t.source.Albums(ctx, t.owner, artist)
	if err != nil {
		return nil, err
	}
	named, _ := AlbumNames(albums)
	l := &level[db.Album]{named: named, name: reverse(named, func(a db.Album) string { return a.Name }), order: albums}
	t.albums[artist] = l
	return l, nil
}

// trackLevel is one album's tracks, named. Keyed by both tags, because two
// artists can have an album of the same name.
func (t *Tree) trackLevel(ctx context.Context, artist, album string) (*level[db.Track], error) {
	key := artist + "\x00" + album
	if l, ok := t.tracks[key]; ok {
		return l, nil
	}
	tracks, err := t.source.Tracks(ctx, t.owner, artist, album)
	if err != nil {
		return nil, err
	}
	named, _ := TrackNames(tracks)
	l := &level[db.Track]{named: named, name: reverse(named, func(tr db.Track) string { return tr.File.Path }), order: tracks}
	t.tracks[key] = l
	return l, nil
}

// PathOf is where a track lives in this tree, which is what a page links to.
func (t *Tree) PathOf(ctx context.Context, track db.Track) (string, error) {
	artist, album := track.Media.AlbumArtist, track.Media.Album
	if artist == "" {
		artist = track.Media.Artist
	}
	at, err := t.PathOfAlbum(ctx, artist, album)
	if err != nil {
		return "", err
	}
	tracks, err := t.trackLevel(ctx, artist, album)
	if err != nil {
		return "", err
	}
	name, ok := tracks.name[track.File.Path]
	if !ok {
		return "", os.ErrNotExist
	}
	return at + "/" + name, nil
}

// PathOfArtist and PathOfAlbum are where a tag lives in this tree.
//
// They exist for the one caller that has a tag and no node: a search result is
// a row of the index, and the address it should link to is the generated one.
func (t *Tree) PathOfArtist(ctx context.Context, artist string) (string, error) {
	artists, err := t.artistLevel(ctx)
	if err != nil {
		return "", err
	}
	name, ok := artists.name[artist]
	if !ok {
		return "", os.ErrNotExist
	}
	return name, nil
}

// PathOfAlbum is where an album lives, which is its artist's segment and its
// own.
func (t *Tree) PathOfAlbum(ctx context.Context, artist, album string) (string, error) {
	artistName, err := t.PathOfArtist(ctx, artist)
	if err != nil {
		return "", err
	}
	albums, err := t.albumLevel(ctx, artist)
	if err != nil {
		return "", err
	}
	name, ok := albums.name[album]
	if !ok {
		return "", os.ErrNotExist
	}
	return Node{ArtistName: artistName, AlbumName: name}.Path(), nil
}

// reverse is the name each thing is served as, by whatever identifies it.
func reverse[T any](named map[string]T, key func(T) string) map[string]string {
	out := make(map[string]string, len(named))
	for name, thing := range named {
		out[key(thing)] = name
	}
	return out
}
