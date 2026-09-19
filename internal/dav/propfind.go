package dav

import (
	"context"
	"encoding/xml"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strconv"
	"time"

	xnet "golang.org/x/net/webdav"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/media"
	"github.com/C0piIot/stratus-backend/internal/storage"
)

// PROPFIND is answered by a second WebDAV library, and this file is the whole
// of why.
//
// emersion/go-webdav builds every response from a fixed set of properties and
// gives a backend no way to add one: `webdav.FileInfo` has six fields and
// `propFindFile` has no hook. That has cost us three things -- telling a client
// which files have a preview (#136), reporting free space over RFC 4331 (#154),
// and a multistatus that is built whole in memory, which is why Depth: infinity
// is refused (#160).
//
// golang.org/x/net/webdav has exactly what is missing: DeadPropsHolder, which
// lets a File contribute properties of its own, and a writer that emits each
// response as it is produced. What made it look like the wrong trade is its
// FileSystem -- a PUT is OpenFile plus io.Copy, which fits a blob store badly
// and would need a pipe between the library and files.Write.
//
// **That objection is entirely about writing, and PROPFIND does not write.** It
// touches Stat, OpenFile with O_RDONLY, and Readdir. So the two libraries split
// by method rather than by surface: the one that can express a property answers
// PROPFIND, everything else stays where it was, and the write path is untouched.

// propfindHandler answers one PROPFIND, over a filesystem built for that
// request alone. See readOnlyFS for why it cannot be shared.
func (f *fileSystem) propfindHandler(owner string) http.Handler {
	return &xnet.Handler{
		Prefix:     f.prefix,
		FileSystem: &readOnlyFS{files: f.files, owner: owner, listed: map[string]db.File{}},
		// A lock system is required for the supportedlock and lockdiscovery
		// properties, which this reports honestly: nothing holds a lock here,
		// because LOCK is answered in lock.go with a token nothing records.
		// Making it real is #174.
		LockSystem: xnet.NewMemLS(),
	}
}

// readOnlyFS is x/net/webdav's FileSystem over internal/files, with everything
// that writes refused.
//
// **Built per request, because of the cache.** x/net asks Stat once while
// walking and then opens every resource again to read its properties, throwing
// away the os.FileInfo it already had. Without somewhere to put what a listing
// just returned, a directory of a hundred children would be one ListFiles plus
// a hundred lookups -- the N+1 shape #160 removed from this very surface. So a
// Readdir remembers its rows for the life of the request, and the reopen is
// answered from memory.
type readOnlyFS struct {
	files  *files.Service
	owner  string
	listed map[string]db.File
}

// errReadOnly is what the methods PROPFIND cannot reach return. They are on the
// interface and unreachable: only PROPFIND is routed here, and every other
// method is answered by the other library.
var errReadOnly = errors.New("dav: this filesystem only answers PROPFIND")

func (r *readOnlyFS) Mkdir(context.Context, string, os.FileMode) error { return errReadOnly }
func (r *readOnlyFS) RemoveAll(context.Context, string) error          { return errReadOnly }
func (r *readOnlyFS) Rename(context.Context, string, string) error     { return errReadOnly }

func (r *readOnlyFS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	row, err := r.row(ctx, name)
	if err != nil {
		return nil, err
	}
	return rowInfo{row}, nil
}

func (r *readOnlyFS) OpenFile(ctx context.Context, name string, flag int, _ os.FileMode) (xnet.File, error) {
	if flag != os.O_RDONLY {
		return nil, errReadOnly
	}
	row, err := r.row(ctx, name)
	if err != nil {
		return nil, err
	}
	return &readOnlyFile{fs: r, row: row}, nil
}

// row finds one row, from what a listing already returned when it can.
//
// os.ErrNotExist and not our own sentinel: the library tests with os.IsNotExist
// to tell a 404 from a 405, and a wrapped db.ErrNotFound would come back as the
// wrong status.
func (r *readOnlyFS) row(ctx context.Context, name string) (db.File, error) {
	p, err := toPath(name)
	if err != nil {
		return db.File{}, err
	}
	if p == "" {
		// The root is the one directory with no row of its own.
		return db.File{OwnerID: r.owner, IsDir: true}, nil
	}
	if row, ok := r.listed[p]; ok {
		return row, nil
	}

	row, err := r.files.Stat(ctx, r.owner, p)
	if errors.Is(err, db.ErrNotFound) {
		return db.File{}, os.ErrNotExist
	}
	if err != nil {
		return db.File{}, err
	}
	r.listed[p] = row
	return row, nil
}

// readOnlyFile is what OpenFile hands back: enough to be walked and described,
// and nothing that reads bytes. PROPFIND never asks for any.
type readOnlyFile struct {
	fs  *readOnlyFS
	row db.File
	// read is where Readdir has got to, since the library may ask in batches.
	read int
}

func (f *readOnlyFile) Close() error                   { return nil }
func (f *readOnlyFile) Read([]byte) (int, error)       { return 0, errReadOnly }
func (f *readOnlyFile) Write([]byte) (int, error)      { return 0, errReadOnly }
func (f *readOnlyFile) Seek(int64, int) (int64, error) { return 0, errReadOnly }
func (f *readOnlyFile) Stat() (os.FileInfo, error)     { return rowInfo{f.row}, nil }

func (f *readOnlyFile) Readdir(count int) ([]os.FileInfo, error) {
	if !f.row.IsDir {
		return nil, errReadOnly
	}
	children, err := f.fs.files.List(context.Background(), f.fs.owner, f.row.Path)
	if err != nil {
		return nil, err
	}

	out := make([]os.FileInfo, 0, len(children))
	for _, child := range children {
		// What the walk is about to ask for again, one open per child. This is
		// the cache earning its place.
		f.fs.listed[child.Path] = child
		out = append(out, rowInfo{child})
	}
	if count <= 0 {
		return out, nil
	}
	return out[min(f.read, len(out)):], nil
}

// rowInfo is a db.File wearing os.FileInfo, which is the shape x/net speaks.
type rowInfo struct{ row db.File }

func (i rowInfo) Name() string       { return path.Base(i.row.Path) }
func (i rowInfo) Size() int64        { return i.row.Size }
func (i rowInfo) ModTime() time.Time { return i.row.MTime }
func (i rowInfo) IsDir() bool        { return i.row.IsDir }
func (i rowInfo) Sys() any           { return i.row }

// ETag is x/net's ETager, and it is not optional for us.
//
// Without it the library computes one from the modification time and the size,
// which would be a different string from the one every other surface sends --
// the same file would have two validators depending on which request asked, and
// a client comparing them would decide it had changed. Caught by the app's
// conformance suite rather than by anything here, which is what that suite is
// for.
//
// Quoted, because that is the form the interface asks for and the framing HTTP
// wants; internal/files stores the validator itself, unquoted, for every
// surface to frame its own way.
func (i rowInfo) ETag(context.Context) (string, error) {
	if i.row.ETag == "" {
		// Nothing to say, so the library's own is better than an empty header.
		return "", xnet.ErrNotImplemented
	}
	return strconv.Quote(i.row.ETag), nil
}

func (i rowInfo) Mode() os.FileMode {
	if i.row.IsDir {
		return fs.ModeDir | 0o555
	}
	return 0o444
}

// The properties this server answers that the protocol does not define.
//
// Our own namespace and not Nextcloud's `nc:`, which is the only convention
// with any deployment behind it: speaking somebody else's vocabulary would tell
// a client it is talking to Nextcloud and invite it to expect the rest of that
// surface (#136).
const stratusNamespace = "https://github.com/C0piIot/stratus"

var (
	hasPreviewName = xml.Name{Space: stratusNamespace, Local: "has-preview"}
	quotaAvailable = xml.Name{Space: "DAV:", Local: "quota-available-bytes"}
	quotaUsed      = xml.Name{Space: "DAV:", Local: "quota-used-bytes"}
)

// DeadProps is the hook this whole file exists for.
//
// They are not dead properties in the RFC's sense -- nothing writes them and
// Patch refuses -- but it is the one door a FileSystem is given, and what comes
// out of it is indistinguishable to a client from a live property.
func (f *readOnlyFile) DeadProps() (map[xml.Name]xnet.Property, error) {
	props := map[xml.Name]xnet.Property{}

	if !f.row.IsDir {
		// Whether a picture of this file can be made, which is the question a
		// client drawing a grid of a few hundred rows would otherwise answer by
		// asking for each one and counting the 404s (#136).
		props[hasPreviewName] = property(hasPreviewName, strconv.FormatBool(
			media.CanThumbnail(f.row.Path, f.row.Size)))
		return props, nil
	}

	// RFC 4331, on a collection: what a mounted volume draws its bar from.
	free, err := f.fs.files.FreeSpace(context.Background())
	if err == nil && free != storage.Unlimited {
		props[quotaAvailable] = property(quotaAvailable, strconv.FormatInt(free, 10))
	}
	used, err := f.fs.files.SubtreeSize(context.Background(), f.fs.owner, f.row.Path)
	if err == nil {
		props[quotaUsed] = property(quotaUsed, strconv.FormatInt(used, 10))
	}
	return props, nil
}

// Patch refuses every write. These are facts about a file, not notes a client
// may leave on one, and x/net turns a refusal here into the 403 the RFC wants.
func (f *readOnlyFile) Patch([]xnet.Proppatch) ([]xnet.Propstat, error) {
	return nil, errReadOnly
}

func property(name xml.Name, value string) xnet.Property {
	return xnet.Property{XMLName: name, InnerXML: []byte(value)}
}
