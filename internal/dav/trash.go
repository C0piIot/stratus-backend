package dav

import (
	"cmp"
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	xnet "golang.org/x/net/webdav"

	"github.com/C0piIot/stratus-backend/internal/auth"
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
// folder.

// trashMethods is what the address answers, which is not what this handler
// answers: the two reads are the browser half's.
const trashMethods = "OPTIONS, GET, HEAD, PROPFIND"

// Trash serves the owner's deletions under prefix.
func Trash(prefix string, source trash.Source) http.Handler {
	prefix = strings.TrimSuffix(prefix, "/")
	// x/net's own and not the table the file surface uses (#243): nothing here
	// writes, so no lock taken on this mount can ever refuse anything.
	locks := xnet.NewMemLS()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		owner, ok := auth.User(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		switch r.Method {
		case http.MethodOptions:
			w.Header().Set("DAV", "1")
			w.Header().Set("Allow", trashMethods)
			w.WriteHeader(http.StatusOK)
			return
		case "PROPFIND":
		default:
			w.Header().Set("Allow", trashMethods)
			http.Error(w, "the trash is read-only here", http.StatusMethodNotAllowed)
			return
		}
		if refuseInfiniteDepth(w, r) {
			return
		}

		// Resolved before the library sees it, because x/net answers 404 for
		// any error opening a file: a database that is down would tell a
		// client the trash is empty.
		tfs := &trashFS{ctx: r.Context(), tree: trash.New(source, owner)}
		n, err := tfs.tree.Resolve(r.Context(), strings.TrimPrefix(r.URL.Path, prefix))
		if err == nil && n.Dir && r.Header.Get("Depth") == "1" {
			_, err = tfs.tree.Children(r.Context(), n)
		}
		switch {
		case errors.Is(err, os.ErrNotExist):
			http.NotFound(w, r)
			return
		case err != nil:
			slog.ErrorContext(r.Context(), "dav: cannot read the trash", "err", err)
			http.Error(w, "the server could not read the trash", http.StatusInternalServerError)
			return
		}

		(&xnet.Handler{Prefix: prefix, FileSystem: tfs, LockSystem: locks}).ServeHTTP(w, r)
	})
}

// trashFS is x/net's FileSystem over one request's view of the trash. What is
// where, and what it is called, is internal/trash's.
type trashFS struct {
	refusesWrites
	// ctx is the request's, for the calls x/net's File interface gives no
	// context to.
	ctx  context.Context
	tree *trash.Tree
}

func (t *trashFS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	n, err := t.tree.Resolve(ctx, name)
	if err != nil {
		return nil, err
	}
	return trashInfo{n: n}, nil
}

func (t *trashFS) OpenFile(ctx context.Context, name string, flag int, _ os.FileMode) (xnet.File, error) {
	if flag != os.O_RDONLY {
		return nil, errReadOnly
	}
	n, err := t.tree.Resolve(ctx, name)
	if err != nil {
		return nil, err
	}
	return &trashFile{fs: t, node: n}, nil
}

// trashFile is what OpenFile hands back, with no bytes in it: a PROPFIND opens
// every resource it lists and reads none of them, and a GET is the other
// half's.
type trashFile struct {
	noBytes
	fs   *trashFS
	node trash.Node
	read bool
}

func (f *trashFile) Readdir(count int) ([]os.FileInfo, error) {
	if !f.node.Dir {
		return nil, errReadOnly
	}
	if f.read && count > 0 {
		return nil, nil
	}
	f.read = true
	children, err := f.fs.tree.Children(f.fs.ctx, f.node)
	if err != nil {
		return nil, err
	}
	out := make([]os.FileInfo, 0, len(children))
	for _, c := range children {
		out = append(out, trashInfo{n: c})
	}
	return out, nil
}

func (f *trashFile) Stat() (os.FileInfo, error) { return trashInfo{n: f.node}, nil }

// trashInfo is x/net's FileInfo, ETager and ContentTyper for one deleted
// resource: the row exactly as it was, since nothing about the file changed
// when it was deleted.
type trashInfo struct {
	noSys
	n trash.Node
}

func (i trashInfo) Name() string { return i.n.Name }
func (i trashInfo) IsDir() bool  { return i.n.Dir }

func (i trashInfo) Size() int64 {
	if i.n.Dir {
		return 0
	}
	return i.n.Row.Size
}

// ModTime is the file's own, not the moment it was deleted: a copy taken out
// of here is the file that was deleted, and when the trash took it is on the
// page.
func (i trashInfo) ModTime() time.Time {
	if i.n.Dir {
		return time.Time{}
	}
	return i.n.Row.MTime
}

func (i trashInfo) Mode() os.FileMode {
	if i.n.Dir {
		return fs.ModeDir | 0o555
	}
	return 0o444
}

func (i trashInfo) ContentType(context.Context) (string, error) {
	if i.n.Dir {
		return "httpd/unix-directory", nil
	}
	return cmp.Or(i.n.Row.MIMEType, "application/octet-stream"), nil
}

func (i trashInfo) ETag(context.Context) (string, error) {
	if i.n.Dir || i.n.Row.ETag == "" {
		return "", xnet.ErrNotImplemented
	}
	return strconv.Quote(i.n.Row.ETag), nil
}
