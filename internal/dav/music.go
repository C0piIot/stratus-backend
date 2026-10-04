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
// the composition root, by method. So the `Allow` below still names GET and
// HEAD -- they are answered, by the other half -- while nothing here serves
// bytes.
//
// What this is not is a second music library. OpenSubsonic at /rest/ is what a
// music client speaks, and it serves the same tags; this is for the clients
// that speak only WebDAV, and so that the collection at the root has no dead
// end in it.
//
// The names are internal/music's, shared with the pages. They have to be
// generated rather than taken: an artist called AC/DC is a tag, and a
// collection cannot have a slash in its name.

// musicMethods is what the address answers, which is not the same as what this
// handler answers: see above.
const musicMethods = "OPTIONS, GET, HEAD, PROPFIND"

// Music serves the owner's library under prefix, by artist and album.
func Music(prefix string, source music.Source) http.Handler {
	prefix = strings.TrimSuffix(prefix, "/")
	// x/net's own, for the reason the photographs' mount gives: nothing here
	// writes, so no lock taken on it could ever refuse anything.
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
			w.Header().Set("Allow", musicMethods)
			w.WriteHeader(http.StatusOK)
			return
		case "PROPFIND":
		default:
			w.Header().Set("Allow", musicMethods)
			http.Error(w, "the music library is read-only here", http.StatusMethodNotAllowed)
			return
		}
		if refuseInfiniteDepth(w, r) {
			return
		}

		mfs := &musicFS{ctx: r.Context(), tree: music.NewTree(source, owner)}
		name := strings.TrimPrefix(r.URL.Path, prefix)
		n, err := mfs.tree.Resolve(r.Context(), name)
		if err == nil && n.Dir && r.Header.Get("Depth") == "1" {
			_, err = mfs.tree.Children(r.Context(), n)
		}
		switch {
		case errors.Is(err, os.ErrNotExist):
			http.NotFound(w, r)
			return
		case err != nil:
			slog.ErrorContext(r.Context(), "dav: cannot read the music library", "err", err)
			http.Error(w, "the server could not read the music library", http.StatusInternalServerError)
			return
		}

		(&xnet.Handler{Prefix: prefix, FileSystem: mfs, LockSystem: locks}).ServeHTTP(w, r)
	})
}

// musicFS is x/net's FileSystem over one request's view of the tree. What is
// where, and what it is called, is internal/music's.
type musicFS struct {
	refusesWrites
	// ctx is the request's, for the calls x/net's File interface gives no
	// context to.
	ctx  context.Context
	tree *music.Tree
}

func (m *musicFS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	n, err := m.tree.Resolve(ctx, name)
	if err != nil {
		return nil, err
	}
	return trackInfo{n: n}, nil
}

func (m *musicFS) OpenFile(ctx context.Context, name string, flag int, _ os.FileMode) (xnet.File, error) {
	if flag != os.O_RDONLY {
		return nil, errReadOnly
	}
	n, err := m.tree.Resolve(ctx, name)
	if err != nil {
		return nil, err
	}
	return &trackFile{fs: m, node: n}, nil
}

// trackFile is what OpenFile hands back, and it has no bytes in it: a PROPFIND
// opens every resource it lists and reads none of them, and a GET is the other
// half's -- the same shape as the photographs' mount, and for the same reason.
type trackFile struct {
	noBytes
	fs   *musicFS
	node music.Node
	read bool
}

func (f *trackFile) Readdir(count int) ([]os.FileInfo, error) {
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
		out = append(out, trackInfo{n: c})
	}
	return out, nil
}

func (f *trackFile) Stat() (os.FileInfo, error) { return trackInfo{n: f.node}, nil }

// trackInfo is x/net's FileInfo, ETager and ContentTyper for one resource: the
// original's own validator and type, for the reasons rowInfo gives.
type trackInfo struct {
	noSys
	n music.Node
}

func (i trackInfo) Name() string { return i.n.Base() }
func (i trackInfo) IsDir() bool  { return i.n.Dir }

func (i trackInfo) Size() int64 {
	if i.n.Dir {
		return 0
	}
	return i.n.Track.File.Size
}

func (i trackInfo) ModTime() time.Time {
	if i.n.Dir {
		return time.Time{}
	}
	return i.n.Track.File.MTime
}

func (i trackInfo) Mode() os.FileMode {
	if i.n.Dir {
		return fs.ModeDir | 0o555
	}
	return 0o444
}

func (i trackInfo) ContentType(context.Context) (string, error) {
	if i.n.Dir {
		return "httpd/unix-directory", nil
	}
	return cmp.Or(i.n.Track.File.MIMEType, "application/octet-stream"), nil
}

func (i trackInfo) ETag(context.Context) (string, error) {
	if i.n.Dir || i.n.Track.File.ETag == "" {
		return "", xnet.ErrNotImplemented
	}
	return strconv.Quote(i.n.Track.File.ETag), nil
}
