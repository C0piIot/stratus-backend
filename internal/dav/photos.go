package dav

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	xnet "golang.org/x/net/webdav"

	"github.com/C0piIot/stratus-backend/internal/auth"
	"github.com/C0piIot/stratus-backend/internal/db"
)

// Photos by date, as a WebDAV mount of their own (#213): /photos/2024/06/ holds
// every image the camera says was taken in June 2024, wherever it was filed.
//
// The same design as /playlists/ (see playlists.go), for the same reason: a
// generated tree inside the user's own would collide with real folders, and
// this URL space is the server's. Read-only and class 1, so Finder mounts it
// read-only; the one thing this mount does that that one does not is serve
// bytes, which are the originals, ranges and all.
//
// Three things are easy to get wrong here, and each is handled below:
//
//   - x/net opens every resource a PROPFIND lists, to ask it for dead
//     properties. A month of a thousand photographs would then be a thousand
//     reads from the blob store to answer a listing, so a photo's bytes are
//     opened on its first Read or Seek and not before.
//   - x/net answers 404 for any error opening a file, so the request is
//     resolved before it sees it and a broken database or blob store is a
//     500 -- the same fix as the playlists.
//   - Two photos in one month can share a name. They are told apart the way
//     playlists are: in id order, case-insensitively, " (2)" before the
//     extension, so a name never changes when another photo arrives.

// PhotoSource is what this mount reads.
type PhotoSource interface {
	PhotoMonths(ctx context.Context, owner string) ([]db.PhotoMonth, error)
	PhotoTimeline(ctx context.Context, owner string, f db.PhotoFilter) ([]db.Photo, error)
}

// Opener is where the bytes come from: internal/files, so the blob store is
// read the same way every other surface reads it.
type Opener interface {
	OpenFile(ctx context.Context, f db.File) (io.ReadSeekCloser, error)
}

const photoMethods = "OPTIONS, GET, HEAD, PROPFIND"

// monthBatch is how many photos one query reads while listing a month. A
// month is listed whole, so this only bounds a single round trip.
const monthBatch = 1000

// Photos serves the owner's photos under prefix, by year and month.
func Photos(prefix string, source PhotoSource, blobs Opener) http.Handler {
	prefix = strings.TrimSuffix(prefix, "/")
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
		case http.MethodGet, http.MethodHead, "PROPFIND":
		default:
			w.Header().Set("Allow", photoMethods)
			http.Error(w, "photos are read-only here", http.StatusMethodNotAllowed)
			return
		}
		if refuseInfiniteDepth(w, r) {
			return
		}

		pfs := &photoFS{ctx: r.Context(), source: source, blobs: blobs, owner: owner,
			months: nil, named: map[db.PhotoMonth]map[string]db.Photo{}, opened: map[string]io.ReadSeekCloser{}}
		name := strings.TrimPrefix(r.URL.Path, prefix)
		n, err := pfs.resolve(name)
		if err == nil && n.dir && r.Method == "PROPFIND" && r.Header.Get("Depth") == "1" {
			_, err = pfs.children(n)
		}
		if err == nil && !n.dir && r.Method != "PROPFIND" {
			// Opened here so that a blob store that fails is a 500, and handed
			// to the library through opened so it is not read twice.
			var rsc io.ReadSeekCloser
			if rsc, err = blobs.OpenFile(r.Context(), n.photo.File); err == nil {
				pfs.opened[n.key()] = rsc
				defer func() { _ = rsc.Close() }()
				w.Header().Set("Content-Type", cmp.Or(n.photo.File.MIMEType, "application/octet-stream"))
			}
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

// photoFS is one request's view of the owner's photos by date. It remembers
// the months and every month it has listed, so x/net's walk -- a Stat, then an
// open per resource -- is answered from memory after the first query.
type photoFS struct {
	// ctx is the request's, for the reads x/net's File interface gives no
	// context to.
	ctx    context.Context
	source PhotoSource
	blobs  Opener
	owner  string

	months []db.PhotoMonth
	named  map[db.PhotoMonth]map[string]db.Photo
	// opened is a photo's bytes, opened before the library asked for them.
	opened map[string]io.ReadSeekCloser
}

// photoNode is one resource: the root, a year, a month or a photo.
type photoNode struct {
	year  int
	month time.Month
	name  string
	dir   bool
	photo db.Photo
}

func (n photoNode) key() string { return fmt.Sprintf("%04d/%02d/%s", n.year, n.month, n.name) }

func (n photoNode) base() string {
	switch {
	case n.name != "":
		return n.name
	case n.month != 0:
		return fmt.Sprintf("%02d", n.month)
	case n.year != 0:
		return strconv.Itoa(n.year)
	}
	return ""
}

func (p *photoFS) Mkdir(context.Context, string, os.FileMode) error { return errReadOnly }
func (p *photoFS) RemoveAll(context.Context, string) error          { return errReadOnly }
func (p *photoFS) Rename(context.Context, string, string) error     { return errReadOnly }

func (p *photoFS) Stat(_ context.Context, name string) (os.FileInfo, error) {
	n, err := p.resolve(name)
	if err != nil {
		return nil, err
	}
	return photoInfo{n}, nil
}

func (p *photoFS) OpenFile(_ context.Context, name string, flag int, _ os.FileMode) (xnet.File, error) {
	if flag != os.O_RDONLY {
		return nil, errReadOnly
	}
	n, err := p.resolve(name)
	if err != nil {
		return nil, err
	}
	return &photoFile{fs: p, node: n}, nil
}

// resolve turns a path under the mount into what is at it, or os.ErrNotExist.
func (p *photoFS) resolve(name string) (photoNode, error) {
	parts := strings.Split(strings.Trim(name, "/"), "/")
	if parts[0] == "" {
		return photoNode{dir: true}, nil
	}
	if len(parts) > 3 {
		return photoNode{}, os.ErrNotExist
	}

	year, err := strconv.Atoi(parts[0])
	if err != nil || parts[0] != strconv.Itoa(year) {
		return photoNode{}, os.ErrNotExist
	}
	months, err := p.listMonths()
	if err != nil {
		return photoNode{}, err
	}
	if !slices.ContainsFunc(months, func(m db.PhotoMonth) bool { return m.Year == year }) {
		return photoNode{}, os.ErrNotExist
	}
	if len(parts) == 1 {
		return photoNode{year: year, dir: true}, nil
	}

	mo, err := strconv.Atoi(parts[1])
	if err != nil || parts[1] != fmt.Sprintf("%02d", mo) {
		return photoNode{}, os.ErrNotExist
	}
	month := db.PhotoMonth{Year: year, Month: time.Month(mo)}
	if !slices.Contains(months, month) {
		return photoNode{}, os.ErrNotExist
	}
	if len(parts) == 2 {
		return photoNode{year: year, month: month.Month, dir: true}, nil
	}

	named, err := p.listMonth(month)
	if err != nil {
		return photoNode{}, err
	}
	ph, ok := named[parts[2]]
	if !ok {
		return photoNode{}, os.ErrNotExist
	}
	return photoNode{year: year, month: month.Month, name: parts[2], photo: ph}, nil
}

func (p *photoFS) listMonths() ([]db.PhotoMonth, error) {
	if p.months != nil {
		return p.months, nil
	}
	months, err := p.source.PhotoMonths(p.ctx, p.owner)
	if err != nil {
		return nil, err
	}
	p.months = append([]db.PhotoMonth{}, months...)
	return p.months, nil
}

// listMonth reads one month whole and names its photos.
func (p *photoFS) listMonth(m db.PhotoMonth) (map[string]db.Photo, error) {
	if named, ok := p.named[m]; ok {
		return named, nil
	}
	var all []db.Photo
	f := db.PhotoFilter{From: m.Start(), To: m.End(), Limit: monthBatch}
	for {
		page, err := p.source.PhotoTimeline(p.ctx, p.owner, f)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if len(page) < monthBatch {
			break
		}
		f.After = page[len(page)-1].Cursor()
	}
	named := photoNames(all)
	p.named[m] = named
	return named, nil
}

// photoNames names a month's photos after their files, in id order so that a
// name is stable, and without two that a case-folding client would take for
// one.
func photoNames(photos []db.Photo) map[string]db.Photo {
	byID := slices.Clone(photos)
	slices.SortFunc(byID, func(a, b db.Photo) int { return cmp.Compare(a.File.ID, b.File.ID) })

	named := make(map[string]db.Photo, len(byID))
	taken := make(map[string]bool, len(byID))
	for _, ph := range byID {
		base := path.Base(ph.File.Path)
		ext := path.Ext(base)
		stem := strings.TrimSuffix(base, ext)
		name := base
		for n := 2; taken[strings.ToLower(name)]; n++ {
			name = stem + " (" + strconv.Itoa(n) + ")" + ext
		}
		taken[strings.ToLower(name)] = true
		named[name] = ph
	}
	return named
}

// children lists a directory.
func (p *photoFS) children(n photoNode) ([]os.FileInfo, error) {
	months, err := p.listMonths()
	if err != nil {
		return nil, err
	}
	var out []os.FileInfo
	switch {
	case n.year == 0:
		seen := map[int]bool{}
		for _, m := range months {
			if !seen[m.Year] {
				seen[m.Year] = true
				out = append(out, photoInfo{photoNode{year: m.Year, dir: true}})
			}
		}
	case n.month == 0:
		for _, m := range months {
			if m.Year == n.year {
				out = append(out, photoInfo{photoNode{year: m.Year, month: m.Month, dir: true}})
			}
		}
	default:
		named, err := p.listMonth(db.PhotoMonth{Year: n.year, Month: n.month})
		if err != nil {
			return nil, err
		}
		for name, ph := range named {
			out = append(out, photoInfo{photoNode{year: n.year, month: n.month, name: name, photo: ph}})
		}
	}
	return out, nil
}

// photoFile is what OpenFile hands back. A photo's bytes are opened on the
// first read, not here: see the top of this file.
type photoFile struct {
	fs   *photoFS
	node photoNode
	rsc  io.ReadSeekCloser
	read bool
}

func (f *photoFile) body() (io.ReadSeekCloser, error) {
	if f.node.dir {
		return nil, errReadOnly
	}
	if f.rsc != nil {
		return f.rsc, nil
	}
	if rsc, ok := f.fs.opened[f.node.key()]; ok {
		// Closed by the handler that opened it, not here.
		delete(f.fs.opened, f.node.key())
		f.rsc = nopCloser{rsc}
		return f.rsc, nil
	}
	rsc, err := f.fs.blobs.OpenFile(f.fs.ctx, f.node.photo.File)
	if err != nil {
		return nil, err
	}
	f.rsc = rsc
	return rsc, nil
}

func (f *photoFile) Read(b []byte) (int, error) {
	rsc, err := f.body()
	if err != nil {
		return 0, err
	}
	return rsc.Read(b)
}

func (f *photoFile) Seek(offset int64, whence int) (int64, error) {
	rsc, err := f.body()
	if err != nil {
		return 0, err
	}
	return rsc.Seek(offset, whence)
}

func (f *photoFile) Close() error {
	if f.rsc == nil {
		return nil
	}
	return f.rsc.Close()
}

func (f *photoFile) Write([]byte) (int, error) { return 0, errReadOnly }

func (f *photoFile) Readdir(count int) ([]os.FileInfo, error) {
	if !f.node.dir {
		return nil, errReadOnly
	}
	if f.read && count > 0 {
		return nil, nil
	}
	f.read = true
	return f.fs.children(f.node)
}

func (f *photoFile) Stat() (os.FileInfo, error) { return photoInfo{f.node}, nil }

// nopCloser leaves closing to whoever opened the reader.
type nopCloser struct{ io.ReadSeeker }

func (nopCloser) Close() error { return nil }

// photoInfo is x/net's FileInfo, ETager and ContentTyper for one resource: the
// original's own validator and type, for the reasons rowInfo gives.
type photoInfo struct{ n photoNode }

func (i photoInfo) Name() string { return i.n.base() }
func (i photoInfo) IsDir() bool  { return i.n.dir }
func (i photoInfo) Sys() any     { return nil }

func (i photoInfo) Size() int64 {
	if i.n.dir {
		return 0
	}
	return i.n.photo.File.Size
}

func (i photoInfo) ModTime() time.Time {
	if i.n.dir {
		return time.Time{}
	}
	return i.n.photo.File.MTime
}

func (i photoInfo) Mode() os.FileMode {
	if i.n.dir {
		return fs.ModeDir | 0o555
	}
	return 0o444
}

func (i photoInfo) ContentType(context.Context) (string, error) {
	if i.n.dir {
		return "httpd/unix-directory", nil
	}
	return cmp.Or(i.n.photo.File.MIMEType, "application/octet-stream"), nil
}

func (i photoInfo) ETag(context.Context) (string, error) {
	if i.n.dir || i.n.photo.File.ETag == "" {
		return "", xnet.ErrNotImplemented
	}
	return strconv.Quote(i.n.photo.File.ETag), nil
}
