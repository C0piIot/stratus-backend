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
	"github.com/C0piIot/stratus-backend/internal/photos"
)

// Photos by date, over WebDAV (#213): /photos/2024/06/ holds every image the
// camera says was taken in June 2024, wherever it was filed.
//
// A mount of its own rather than a folder in the tree, for the reason
// /playlists/ is one (see playlists.go): a generated tree inside the user's own
// would collide with real folders, and this URL space is the server's.
//
// **This half answers the methods a browser never sends** (#279). The same
// addresses answer HTML and bytes to a browser, and that half is
// internal/web's; which of the two a request reaches is decided in the
// composition root, by method, exactly as it is for the file surface. So the
// `Allow` below still names GET and HEAD -- they are answered, and saying
// otherwise would make a client believe the collection is listing-only --
// while nothing here serves them.
//
// The names are internal/photos', because the page that links to a photograph
// and the collection that answers for it have to agree about what it is
// called.
//
// Two things are easy to get wrong here, and both are handled below:
//
//   - x/net opens every resource a PROPFIND lists, to ask it for dead
//     properties. A month of a thousand photographs would then be a thousand
//     reads from the blob store to answer a listing -- which is why nothing
//     under this handler reads bytes at all, and why the tree below caches
//     what it has already looked up.
//   - x/net answers 404 for any error opening a file, so the request is
//     resolved before it sees it and a broken database is a 500 -- the same
//     fix as the playlists.

// photoMethods is what the address answers, which is not the same as what this
// handler answers: see above.
const photoMethods = "OPTIONS, GET, HEAD, PROPFIND"

// Photos serves the owner's photos under prefix, by year and month.
func Photos(prefix string, source photos.Source) http.Handler {
	prefix = strings.TrimSuffix(prefix, "/")
	// x/net's own and not the table the file surface uses (#243): nothing here
	// writes, so no lock taken on this mount can ever refuse anything, and one
	// that outlived the process would be state kept for a surface that has
	// none.
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
			w.Header().Set("Allow", photoMethods)
			w.WriteHeader(http.StatusOK)
			return
		case "PROPFIND":
		default:
			w.Header().Set("Allow", photoMethods)
			http.Error(w, "photos are read-only here", http.StatusMethodNotAllowed)
			return
		}
		if refuseInfiniteDepth(w, r) {
			return
		}

		pfs := &photoFS{ctx: r.Context(), tree: photos.New(source, owner)}
		name := strings.TrimPrefix(r.URL.Path, prefix)
		n, err := pfs.tree.Resolve(r.Context(), name)
		if err == nil && n.Dir && r.Header.Get("Depth") == "1" {
			_, err = pfs.tree.Children(r.Context(), n)
		}
		switch {
		case errors.Is(err, os.ErrNotExist):
			http.NotFound(w, r)
			return
		case err != nil:
			slog.ErrorContext(r.Context(), "dav: cannot read photos", "err", err)
			http.Error(w, "the server could not read the photos", http.StatusInternalServerError)
			return
		}

		(&xnet.Handler{Prefix: prefix, FileSystem: pfs, LockSystem: locks}).ServeHTTP(w, r)
	})
}

// photoFS is x/net's FileSystem over one request's view of the tree. The view
// itself -- what is where, and what it is called -- is internal/photos'; what
// is here is the shape x/net asks for.
type photoFS struct {
	refusesWrites
	// ctx is the request's, for the calls x/net's File interface gives no
	// context to.
	ctx  context.Context
	tree *photos.Tree
}

func (p *photoFS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	n, err := p.tree.Resolve(ctx, name)
	if err != nil {
		return nil, err
	}
	return photoInfo{n: n}, nil
}

func (p *photoFS) OpenFile(ctx context.Context, name string, flag int, _ os.FileMode) (xnet.File, error) {
	if flag != os.O_RDONLY {
		return nil, errReadOnly
	}
	n, err := p.tree.Resolve(ctx, name)
	if err != nil {
		return nil, err
	}
	return &photoFile{fs: p, node: n}, nil
}

// photoFile is what OpenFile hands back, and it has no bytes in it: a PROPFIND
// opens every resource it lists and reads none of them, and a GET is the other
// half's. A read here would mean this handler had been mounted without that
// half, which is a wiring mistake and says so.
type photoFile struct {
	noBytes
	fs   *photoFS
	node photos.Node
	read bool
}

func (f *photoFile) Readdir(count int) ([]os.FileInfo, error) {
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
		out = append(out, photoInfo{n: c})
	}
	return out, nil
}

func (f *photoFile) Stat() (os.FileInfo, error) { return photoInfo{n: f.node}, nil }

// photoInfo is x/net's FileInfo, ETager and ContentTyper for one resource: the
// original's own validator and type, for the reasons rowInfo gives.
type photoInfo struct {
	noSys
	n photos.Node
}

func (i photoInfo) Name() string { return i.n.Base() }
func (i photoInfo) IsDir() bool  { return i.n.Dir }

func (i photoInfo) Size() int64 {
	if i.n.Dir {
		return 0
	}
	return i.n.Photo.File.Size
}

func (i photoInfo) ModTime() time.Time {
	if i.n.Dir {
		return time.Time{}
	}
	return i.n.Photo.File.MTime
}

func (i photoInfo) Mode() os.FileMode {
	if i.n.Dir {
		return fs.ModeDir | 0o555
	}
	return 0o444
}

func (i photoInfo) ContentType(context.Context) (string, error) {
	if i.n.Dir {
		return "httpd/unix-directory", nil
	}
	return cmp.Or(i.n.Photo.File.MIMEType, "application/octet-stream"), nil
}

func (i photoInfo) ETag(context.Context) (string, error) {
	if i.n.Dir || i.n.Photo.File.ETag == "" {
		return "", xnet.ErrNotImplemented
	}
	return strconv.Quote(i.n.Photo.File.ETag), nil
}
