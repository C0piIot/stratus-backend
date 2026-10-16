package dav

import (
	"context"
	"net/http"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/music"
)

// The library by tag, over WebDAV (#279): /music/Autechre/Amber/ holds that
// album's tracks, wherever their files were filed.
//
// A mount of its own rather than a folder in the tree, for the reason
// /playlists/ is one: what is here is generated, and a generated folder inside
// somebody's own files could collide with a real one.
//
// **This half answers the methods a browser never sends.** The same addresses
// are the music pages, and which of the two a request reaches is decided in
// the composition root, by method.
//
// What this is not is a second music library. OpenSubsonic at /rest/ is what a
// music client speaks, and it serves the same tags; this is for the clients
// that speak only WebDAV, and so that the collection at the root has no dead
// end in it.
//
// The names are internal/music's, shared with the pages. They have to be
// generated rather than taken: an artist called AC/DC is a tag, and a
// collection cannot have a slash in its name. The mount itself is the
// generated one in generated.go (#359).

// Music serves the owner's library under prefix, by artist and album.
func Music(prefix string, source music.Source) http.Handler {
	return mount[music.Node]{
		what:  "the music library",
		open:  func(_ context.Context, owner string) tree[music.Node] { return music.NewTree(source, owner) },
		entry: func(n music.Node) entry { return trackEntry{n} },
	}.handler(prefix)
}

// trackEntry is a node of the tree by tag as the mount reads it.
type trackEntry struct{ n music.Node }

func (e trackEntry) Base() string  { return e.n.Base() }
func (e trackEntry) IsDir() bool   { return e.n.Dir }
func (e trackEntry) File() db.File { return e.n.Track.File }
