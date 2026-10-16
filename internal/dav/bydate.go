package dav

import (
	"context"
	"net/http"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/timeline"
)

// What a camera made, by date, over WebDAV (#213, #215): /photos/2024/06/
// holds every image it says was taken in June 2024 and /videos/2024/06/ every
// recording, wherever they were filed.
//
// **One handler for both, told apart by the kind it is given.** The two trees
// are identical in everything but what they hold -- the same ordering, the
// same month seek, the same generated names -- so a second copy of this would
// be a second thing to keep in step.
//
// A mount of its own rather than a folder in the tree, for the reason
// /playlists/ is one (see playlists.go): a generated tree inside the user's own
// would collide with real folders, and this URL space is the server's.
//
// **This half answers the methods a browser never sends** (#279). The same
// addresses answer HTML and bytes to a browser, and that half is
// internal/web's; which of the two a request reaches is decided in the
// composition root, by method, exactly as it is for the file surface.
//
// The names are internal/timeline's, because the page that links to a file and
// the collection that answers for it have to agree about what it is called.
// The mount itself is the generated one in generated.go (#359).

// ByDate serves the owner's files of one kind under prefix, by year and month.
func ByDate(prefix string, source timeline.Source, kind db.Kind) http.Handler {
	return mount[timeline.Node]{
		what:  "the library by date",
		open:  func(_ context.Context, owner string) tree[timeline.Node] { return timeline.New(source, owner, kind) },
		entry: func(n timeline.Node) entry { return dateEntry{n} },
	}.handler(prefix)
}

// dateEntry is a node of the tree by date as the mount reads it.
type dateEntry struct{ n timeline.Node }

func (e dateEntry) Base() string  { return e.n.Base() }
func (e dateEntry) IsDir() bool   { return e.n.Dir }
func (e dateEntry) File() db.File { return e.n.Capture.File }
