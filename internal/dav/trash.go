package dav

import (
	"context"
	"net/http"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/trash"
)

// What has been deleted, as a collection of its own (#323).
//
// The trash had two doors and both were inside the UI -- the page that asks
// before deleting, and the line on /status that counts what it holds -- so a
// client that mounts this server could not see it at all. It is a mount beside
// /photos/ and /playlists/ now, for the same reason those are mounts: this URL
// space is the server's, so nothing generated can collide with a real file.
//
// **One collection per deletion**, named by what was deleted, which is
// internal/trash's rule and shared with the pages. Read-only, and class 1 with
// no LOCK: restoring and destroying are the page's, and a write here would
// have nothing to mean.
//
// **This half answers the methods a browser never sends** (#279), like the
// other generated mounts: a GET of a deleted file is internal/web's and serves
// the blob that was never moved, which is what makes pulling one photograph
// out of an accident a drag in Finder rather than a restore of the whole
// folder. The mount itself is the generated one in generated.go (#359); what
// it says about a deleted file is the row exactly as it was, since nothing
// about the file changed when it was deleted.

// Trash serves the owner's deletions under prefix.
func Trash(prefix string, source trash.Source) http.Handler {
	return mount[trash.Node]{
		what:  "the trash",
		open:  func(_ context.Context, owner string) tree[trash.Node] { return trash.New(source, owner) },
		entry: func(n trash.Node) entry { return trashEntry{n} },
	}.handler(prefix)
}

// trashEntry is a node of the trash as the mount reads it.
type trashEntry struct{ n trash.Node }

func (e trashEntry) Base() string  { return e.n.Name }
func (e trashEntry) IsDir() bool   { return e.n.Dir }
func (e trashEntry) File() db.File { return e.n.Row.File }
