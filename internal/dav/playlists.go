package dav

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	xnet "golang.org/x/net/webdav"

	"github.com/C0piIot/stratus-backend/internal/auth"
	"github.com/C0piIot/stratus-backend/internal/db"
)

// Playlists as .m3u8 files, over a WebDAV mount of their own (#203).
//
// **A mount of its own rather than a folder in the tree, and that is the whole
// design.** A generated file inside somebody's files can collide with a real
// one: a folder called ".playlists" they already have, or a "Mix.m3u8" they
// uploaded next to the tracks. Reserving a name would mean refusing a path that
// has always been legal, including in a library adopted from elsewhere. This
// URL space is the server's and not the user's, so nothing of theirs can ever
// be at it, by construction rather than by a rule.
//
// Read-only, and it says so: OPTIONS answers class 1 with no LOCK, which is
// what makes Finder mount it read-only instead of offering writes that would
// fail. The database is what a playlist is; these files are a view of it, so
// there is nothing for a write to mean. Importing an .m3u8 is a separate
// question, and a harder one.
//
// Every entry is a URL rooted at the server -- /dav/music/... -- so a player
// that opens the playlist from here resolves it against the same host. What
// that does not serve is a copy synced to a local disk, where /dav/ is not a
// path; that is the same import-and-export question, not this one.

// PlaylistSource is what this mount needs from internal/music.
type PlaylistSource interface {
	Playlists(ctx context.Context, owner string) ([]db.Playlist, error)
	Playlist(ctx context.Context, owner string, id int64) (db.Playlist, []db.Track, error)
}

// playlistMIME is the type players recognise an .m3u8 by.
const playlistMIME = "audio/x-mpegurl"

// playlistMethods is everything this mount answers.
const playlistMethods = "OPTIONS, GET, HEAD, PROPFIND"

// Playlists serves the owner's playlists under prefix as .m3u8 files whose
// entries point into the file surface at davPrefix.
func Playlists(prefix, davPrefix string, source PlaylistSource) http.Handler {
	prefix = strings.TrimSuffix(prefix, "/")
	davPrefix = strings.TrimSuffix(davPrefix, "/")
	locks := xnet.NewMemLS()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		owner, ok := auth.User(r.Context())
		if !ok {
			// The wrapper in front of this handler is what puts a user on the
			// request; getting here without one is a routing mistake.
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		switch r.Method {
		case http.MethodOptions:
			w.Header().Set("DAV", "1")
			w.Header().Set("Allow", playlistMethods)
			w.WriteHeader(http.StatusOK)
			return
		case http.MethodGet, http.MethodHead, "PROPFIND":
		default:
			w.Header().Set("Allow", playlistMethods)
			http.Error(w, "playlists are read-only here", http.StatusMethodNotAllowed)
			return
		}
		if refuseInfiniteDepth(w, r) {
			return
		}

		// Resolved here before the library sees it, for two things x/net gets
		// wrong for us. It answers 404 for any error opening a file, so a
		// database that is down would tell a client its playlist is gone. And
		// it leaves a GET's type to ServeContent, which does not know .m3u8
		// and sniffs it as text. The filesystem remembers what this read, so
		// the library's own reads are answered from memory.
		pfs := &playlistFS{ctx: r.Context(), source: source, owner: owner, davPrefix: davPrefix}
		f, err := pfs.open(r.Context(), strings.TrimPrefix(r.URL.Path, prefix))
		if err == nil && f.dir && r.Method == "PROPFIND" && r.Header.Get("Depth") == "1" {
			_, err = f.Readdir(0)
		}
		switch {
		case errors.Is(err, os.ErrNotExist):
			http.NotFound(w, r)
			return
		case err != nil:
			slog.ErrorContext(r.Context(), "dav: cannot read playlists", "err", err)
			http.Error(w, "the server could not read the playlists", http.StatusInternalServerError)
			return
		case !f.dir:
			w.Header().Set("Content-Type", playlistMIME)
		}

		(&xnet.Handler{
			Prefix:     prefix,
			FileSystem: pfs,
			// Never consulted for a read, but the handler will not start
			// without one.
			LockSystem: locks,
		}).ServeHTTP(w, r)
	})
}

// playlistFS is one request's view of the owner's playlists. Built per request,
// like readOnlyFS, so a listing reads the playlists once and an .m3u8 is
// generated once however many times the library asks about it.
type playlistFS struct {
	// ctx is the request's, for Readdir: x/net's File has no context to hand
	// it, and a listing that outlived its request would be reading for nobody.
	ctx       context.Context
	source    PlaylistSource
	owner     string
	davPrefix string

	// named is every playlist by the file name it is served as, once listed.
	named map[string]db.Playlist
	order []string
	// bodies are the generated files, by name.
	bodies map[string][]byte
}

func (p *playlistFS) Mkdir(context.Context, string, os.FileMode) error { return errReadOnly }
func (p *playlistFS) RemoveAll(context.Context, string) error          { return errReadOnly }
func (p *playlistFS) Rename(context.Context, string, string) error     { return errReadOnly }

func (p *playlistFS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	f, err := p.open(ctx, name)
	if err != nil {
		return nil, err
	}
	return f.Stat()
}

func (p *playlistFS) OpenFile(ctx context.Context, name string, flag int, _ os.FileMode) (xnet.File, error) {
	if flag != os.O_RDONLY {
		return nil, errReadOnly
	}
	return p.open(ctx, name)
}

func (p *playlistFS) open(ctx context.Context, name string) (*playlistFile, error) {
	name = strings.Trim(name, "/")
	if err := p.list(ctx); err != nil {
		return nil, err
	}
	if name == "" {
		return &playlistFile{fs: p, dir: true}, nil
	}
	pl, ok := p.named[name]
	if !ok {
		return nil, os.ErrNotExist
	}
	body, err := p.body(ctx, name, pl)
	if err != nil {
		return nil, err
	}
	return &playlistFile{fs: p, name: name, playlist: pl, Reader: bytes.NewReader(body), body: body}, nil
}

// list reads the playlists and names them, once per request.
func (p *playlistFS) list(ctx context.Context) error {
	if p.named != nil {
		return nil
	}
	playlists, err := p.source.Playlists(ctx, p.owner)
	if err != nil {
		return err
	}
	p.named, p.order = fileNames(playlists)
	p.bodies = map[string][]byte{}
	return nil
}

func (p *playlistFS) body(ctx context.Context, name string, pl db.Playlist) ([]byte, error) {
	if body, ok := p.bodies[name]; ok {
		return body, nil
	}
	pl, tracks, err := p.source.Playlist(ctx, p.owner, pl.ID)
	if errors.Is(err, db.ErrNotFound) {
		// Deleted between the listing and this read.
		return nil, os.ErrNotExist
	}
	if err != nil {
		return nil, err
	}
	body := m3u8(pl, tracks, p.davPrefix)
	p.bodies[name] = body
	return body, nil
}

// fileNames gives every playlist a file name, in id order so that a name is
// stable: the older of two playlists called "Mix" keeps "Mix.m3u8" and the
// newer is "Mix (2).m3u8" whatever they are renamed to later.
//
// Two names that differ only in case are a collision too. Finder and Windows
// mount this on filesystems that fold case, and there the second would hide
// the first.
func fileNames(playlists []db.Playlist) (map[string]db.Playlist, []string) {
	byID := slices.Clone(playlists)
	slices.SortFunc(byID, func(a, b db.Playlist) int { return cmp.Compare(a.ID, b.ID) })

	named := make(map[string]db.Playlist, len(byID))
	taken := make(map[string]bool, len(byID))
	order := make([]string, 0, len(byID))
	for _, pl := range byID {
		base := safeName(pl.Name)
		name := base + ".m3u8"
		for n := 2; taken[strings.ToLower(name)]; n++ {
			name = base + " (" + strconv.Itoa(n) + ").m3u8"
		}
		taken[strings.ToLower(name)] = true
		named[name] = pl
		order = append(order, name)
	}
	return named, order
}

// safeName is a playlist's name as one path element that every client can
// hold: a slash would make it a folder, a control character is not something
// any client can show, the rest are what Windows refuses in a file name -- and
// Windows mounts WebDAV -- and a name of nothing or of dots is not a file.
func safeName(name string) string {
	name = strings.Map(func(r rune) rune {
		if strings.ContainsRune(`/\<>:"|?*`, r) || unicode.IsControl(r) {
			return '_'
		}
		return r
	}, strings.TrimSpace(name))
	if strings.Trim(name, ".") == "" {
		return "Playlist"
	}
	return name
}

// m3u8 renders a playlist in the extended format every player reads: a header,
// then for each entry a line of what it is and a line of where it is.
func m3u8(pl db.Playlist, tracks []db.Track, davPrefix string) []byte {
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
		b.WriteString(davPrefix + "/" + escapePath(t.File.Path) + "\n")
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

// playlistFile is the root collection or one generated .m3u8.
type playlistFile struct {
	*bytes.Reader
	fs       *playlistFS
	dir      bool
	name     string
	playlist db.Playlist
	body     []byte
	read     bool
}

func (f *playlistFile) Close() error              { return nil }
func (f *playlistFile) Write([]byte) (int, error) { return 0, errReadOnly }

func (f *playlistFile) Read(b []byte) (int, error) {
	if f.dir {
		return 0, errReadOnly
	}
	return f.Reader.Read(b)
}

func (f *playlistFile) Seek(offset int64, whence int) (int64, error) {
	if f.dir {
		return 0, errReadOnly
	}
	return f.Reader.Seek(offset, whence)
}

func (f *playlistFile) Readdir(count int) ([]os.FileInfo, error) {
	if !f.dir {
		return nil, errReadOnly
	}
	if f.read && count > 0 {
		return nil, nil
	}
	f.read = true
	out := make([]os.FileInfo, 0, len(f.fs.order))
	for _, name := range f.fs.order {
		child, err := f.fs.open(f.fs.ctx, name)
		if err != nil {
			return nil, err
		}
		info, _ := child.Stat()
		out = append(out, info)
	}
	return out, nil
}

func (f *playlistFile) Stat() (os.FileInfo, error) {
	if f.dir {
		return playlistInfo{dir: true}, nil
	}
	return playlistInfo{name: f.name, playlist: f.playlist, body: f.body}, nil
}

// playlistInfo is x/net's FileInfo, ETager and ContentTyper for one file --
// the two optional interfaces that are not optional, for the reasons rowInfo
// gives.
type playlistInfo struct {
	dir      bool
	name     string
	playlist db.Playlist
	body     []byte
}

func (i playlistInfo) Name() string { return i.name }
func (i playlistInfo) Size() int64  { return int64(len(i.body)) }
func (i playlistInfo) IsDir() bool  { return i.dir }
func (i playlistInfo) Sys() any     { return nil }

// ModTime is when the playlist was last edited. It does not move when a track
// in it is renamed, which changes the file; the ETag is what does.
func (i playlistInfo) ModTime() time.Time { return i.playlist.Changed }

func (i playlistInfo) Mode() os.FileMode {
	if i.dir {
		return fs.ModeDir | 0o555
	}
	return 0o444
}

func (i playlistInfo) ContentType(context.Context) (string, error) {
	if i.dir {
		return "httpd/unix-directory", nil
	}
	return playlistMIME, nil
}

// ETag is a digest of what is served, because nothing else changes exactly when
// it does: a rename of a track, a retag and a deleted file all change the file
// without touching the playlist's row.
func (i playlistInfo) ETag(context.Context) (string, error) {
	if i.dir {
		return "", xnet.ErrNotImplemented
	}
	sum := sha256.Sum256(i.body)
	return strconv.Quote(hex.EncodeToString(sum[:16])), nil
}
