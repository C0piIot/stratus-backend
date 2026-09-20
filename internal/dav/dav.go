// Package dav is the inbound WebDAV adapter: it translates protocol bytes into
// calls on internal/files and back.
//
// The XML, the multistatus responses and the method dispatch come from
// github.com/emersion/go-webdav. What is written here is the backend it drives
// and, mostly, the mapping from this project's sentinel errors onto status
// codes -- which is the part a library cannot guess.
//
// **This file and propfind.go are the only two that know which libraries those
// are** -- there are two now, split by method, and propfind.go says why.
// Everything else in the package -- locking, paths, MIME types -- is written
// against net/http and this project's own types, so replacing either means
// rewriting one file rather than the package. That is deliberate: the choice
// has been questioned twice now (#3, #136) and may be again.
package dav

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/emersion/go-webdav"
	xnet "golang.org/x/net/webdav"

	"github.com/C0piIot/stratus-backend/internal/auth"
	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/storage"
)

// Handler serves WebDAV under prefix, for whoever the request was
// authenticated as.
//
// The prefix is stripped here rather than by the caller because the backend
// speaks in storage paths and the handler speaks in URLs, and exactly one place
// should know the difference.
func Handler(prefix string, service *files.Service) http.Handler {
	prefix = strings.TrimSuffix(prefix, "/")
	fs := &fileSystem{files: service, prefix: prefix, locks: memoryLocks(), now: time.Now}
	dav := &webdav.Handler{FileSystem: fs}
	// Everything that writes goes through the lock gate first. See locks.go.
	guarded := fs.enforceLocks(dav)

	return http.StripPrefix(prefix, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A PROPFIND over the whole tree is refused before the library sees it,
		// with the precondition the RFC has for saying so. See depth.go.
		if refuseInfiniteDepth(w, r) {
			return
		}

		// PROPFIND is answered by the other library, which can express a
		// property. See propfind.go for why there are two.
		if r.Method == "PROPFIND" {
			owner, err := fs.owner(r.Context())
			if err != nil {
				// The only way here is no authenticated user, and the wrapper
				// in front of this handler is what puts one on the request.
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			// The prefix goes back on before it comes off again: this
			// handler is inside a StripPrefix, and x/net strips the prefix
			// itself and joins it back to build every href. Handing it a
			// stripped path would cost the hrefs their /dav.
			restored := r.Clone(r.Context())
			restored.URL.Path = prefix + r.URL.Path
			fs.propfindHandler(owner).ServeHTTP(w, restored)
			return
		}

		// LOCK and UNLOCK never reach the library: it is class 1 and answers
		// 405 to both. See lock.go for what these do and what they do not.
		switch r.Method {
		case "LOCK":
			fs.handleLock(w, r)
		case "UNLOCK":
			fs.handleUnlock(w, r)
		case http.MethodOptions:
			dav.ServeHTTP(&advertiseLocking{ResponseWriter: w}, r)
		default:
			guarded.ServeHTTP(w, r)
		}
	}))
}

type fileSystem struct {
	files *files.Service
	// prefix is stripped from the request path by the handler above, but it is
	// still on the Destination header, and it has to be put back on every href
	// in a multistatus or the client follows a link to nowhere.
	prefix string
	// locks is what makes LOCK mean something. One for the process, not one
	// per request: a lock nobody else can see is not a lock.
	//
	// The interface is x/net's rather than one invented here, so the day a
	// lock has to outlive a restart it is a type satisfying the same four
	// methods and a line in the composition root. See locks.go.
	locks xnet.LockSystem
	// now is the clock the lock system is driven by. Every one of its four
	// methods takes the time as an argument rather than reading it, which is
	// the library saying the caller owns the clock -- and it is what lets a
	// test watch a lock expire instead of waiting an hour for one.
	now func() time.Time
}

// owner is whoever authenticated. go-webdav hands the request's context to
// every backend method, so it arrives here with no plumbing of our own.
//
// There is no fallback: a request that reached this far without a user is a
// routing mistake, and serving somebody's files on a guess is the wrong way to
// find out about it.
func (f *fileSystem) owner(ctx context.Context) (string, error) {
	username, ok := auth.User(ctx)
	if !ok {
		return "", webdav.NewHTTPError(http.StatusUnauthorized, errors.New("no authenticated user on the request"))
	}
	return username, nil
}

// Open implements webdav.FileSystem. The reader it returns seeks, which is what
// makes go-webdav answer with http.ServeContent: ranges, conditional requests
// and video seeking, none of it written here.
func (f *fileSystem) Open(ctx context.Context, name string) (io.ReadCloser, error) {
	p, err := toPath(name)
	if err != nil {
		return nil, err
	}
	owner, err := f.owner(ctx)
	if err != nil {
		return nil, err
	}

	body, _, err := f.files.Open(ctx, owner, p)
	return body, mapErr(err)
}

// Stat implements webdav.FileSystem.
func (f *fileSystem) Stat(ctx context.Context, name string) (*webdav.FileInfo, error) {
	p, err := toPath(name)
	if err != nil {
		return nil, err
	}
	// The root is not a row: nothing creates it and nothing may delete it, so
	// it is reported as the directory it behaves like.
	if p == "" {
		return &webdav.FileInfo{Path: f.prefix + "/", IsDir: true}, nil
	}

	owner, err := f.owner(ctx)
	if err != nil {
		return nil, err
	}

	file, err := f.files.Stat(ctx, owner, p)
	if err != nil {
		return nil, mapErr(err)
	}
	return f.toFileInfo(file), nil
}

// ReadDir implements webdav.FileSystem and is unreachable.
//
// The library calls it from one place, building a PROPFIND response, and
// PROPFIND is answered by the other library now (see propfind.go). Left as a
// refusal rather than deleted because the interface requires it, and a refusal
// is what should happen if it is ever reached again.
//
// What it used to carry was #126: a Depth 1 listing has to include the
// collection itself, and this library throws away the Stat it made and never
// adds it back, so the entry had to be prepended here. x/net walks from the
// requested resource, so it includes it natively -- and the test that pins it
// is still there, now watching a different implementation keep the promise.
func (f *fileSystem) ReadDir(context.Context, string, bool) ([]webdav.FileInfo, error) {
	return nil, errNotThisLibrary
}

// errNotThisLibrary marks the corner of the emersion adapter that PROPFIND used
// to reach.
var errNotThisLibrary = errors.New("dav: PROPFIND is answered by golang.org/x/net/webdav")

// Create implements webdav.FileSystem, which is PUT.
func (f *fileSystem) Create(ctx context.Context, name string, body io.ReadCloser, opts *webdav.CreateOptions) (*webdav.FileInfo, bool, error) {
	p, err := toPath(name)
	if err != nil {
		return nil, false, err
	}

	owner, err := f.owner(ctx)
	if err != nil {
		return nil, false, err
	}

	existing, statErr := f.files.Stat(ctx, owner, p)
	switch {
	case statErr != nil && !errors.Is(statErr, db.ErrNotFound):
		return nil, false, mapErr(statErr)
	case opts != nil:
		if cerr := checkConditions(opts.IfMatch, opts.IfNoneMatch, existing, statErr == nil); cerr != nil {
			return nil, false, cerr
		}
	}

	// Size is unknown here: WebDAV clients may send chunked, and the port takes
	// -1 for that.
	file, err := f.files.Write(ctx, owner, p, body, -1, mimeType(p))
	if err != nil {
		// RFC 4918 9.7.1: a PUT whose parent collection does not exist is 409,
		// not 404. The resource being created is not the one that is missing.
		if errors.Is(err, db.ErrNotFound) {
			return nil, false, webdav.NewHTTPError(http.StatusConflict, err)
		}
		return nil, false, mapErr(err)
	}
	return f.toFileInfo(file), statErr != nil, nil
}

// RemoveAll implements webdav.FileSystem, which is DELETE. It is recursive by
// definition of the method: DELETE on a collection takes the collection.
func (f *fileSystem) RemoveAll(ctx context.Context, name string, opts *webdav.RemoveAllOptions) error {
	p, err := toPath(name)
	if err != nil {
		return err
	}
	if p == "" {
		return webdav.NewHTTPError(http.StatusForbidden, errors.New("the root cannot be deleted"))
	}

	owner, err := f.owner(ctx)
	if err != nil {
		return err
	}

	if opts != nil && (opts.IfMatch.IsSet() || opts.IfNoneMatch.IsSet()) {
		existing, statErr := f.files.Stat(ctx, owner, p)
		if statErr != nil && !errors.Is(statErr, db.ErrNotFound) {
			return mapErr(statErr)
		}
		if err := checkConditions(opts.IfMatch, opts.IfNoneMatch, existing, statErr == nil); err != nil {
			return err
		}
	}
	return mapErr(f.files.Remove(ctx, owner, p))
}

// Mkdir implements webdav.FileSystem, which is MKCOL.
func (f *fileSystem) Mkdir(ctx context.Context, name string) error {
	p, err := toPath(name)
	if err != nil {
		return err
	}
	owner, err := f.owner(ctx)
	if err != nil {
		return err
	}

	_, err = f.files.Mkdir(ctx, owner, p)
	switch {
	case errors.Is(err, db.ErrConflict):
		// RFC 4918 9.3.1: MKCOL on something that already exists is 405.
		return webdav.NewHTTPError(http.StatusMethodNotAllowed, err)
	case errors.Is(err, db.ErrNotFound):
		// And a missing parent is 409, for the same reason as PUT.
		return webdav.NewHTTPError(http.StatusConflict, err)
	default:
		return mapErr(err)
	}
}

// Move implements webdav.FileSystem.
func (f *fileSystem) Move(ctx context.Context, name, dest string, opts *webdav.MoveOptions) (bool, error) {
	from, err := toPath(name)
	if err != nil {
		return false, err
	}
	to, err := f.destPath(dest)
	if err != nil {
		return false, err
	}

	owner, err := f.owner(ctx)
	if err != nil {
		return false, err
	}

	_, statErr := f.files.Stat(ctx, owner, to)
	switch {
	case statErr == nil && opts != nil && opts.NoOverwrite:
		return false, webdav.NewHTTPError(http.StatusPreconditionFailed, errors.New("the destination exists"))
	case statErr == nil:
		// The port refuses to move onto an occupied path, so the destination is
		// cleared first. Not atomic with the move: a failure between the two
		// loses the destination, which is what Overwrite: T asked for anyway.
		if err := f.files.Remove(ctx, owner, to); err != nil {
			return false, mapErr(err)
		}
	case !errors.Is(statErr, db.ErrNotFound):
		return false, mapErr(statErr)
	}

	if err := f.files.Move(ctx, owner, from, to); err != nil {
		return false, mapErr(err)
	}
	return statErr != nil, nil
}

// Copy implements webdav.FileSystem, for a single file.
//
// Copying a collection is refused rather than half-implemented: it is a
// recursive walk that has to decide what to do when it fails halfway, and no
// client this project targets needs it. See the issue linked from the PR.
func (f *fileSystem) Copy(ctx context.Context, name, dest string, opts *webdav.CopyOptions) (bool, error) {
	from, err := toPath(name)
	if err != nil {
		return false, err
	}
	to, err := f.destPath(dest)
	if err != nil {
		return false, err
	}

	owner, err := f.owner(ctx)
	if err != nil {
		return false, err
	}

	if _, statErr := f.files.Stat(ctx, owner, to); statErr == nil {
		if opts != nil && opts.NoOverwrite {
			return false, webdav.NewHTTPError(http.StatusPreconditionFailed, errors.New("the destination exists"))
		}
	} else if !errors.Is(statErr, db.ErrNotFound) {
		return false, mapErr(statErr)
	}

	// NoRecursive is the library's reading of Depth: 0, which on a collection
	// means the collection and not its members (RFC 4918 9.8.3).
	created, err := f.files.Copy(ctx, owner, from, to, opts == nil || !opts.NoRecursive)
	if err != nil {
		return false, mapErr(err)
	}
	return created, nil
}

// checkConditions applies If-Match and If-None-Match, which is how a client
// says "only if I am not overwriting somebody else's change".
func checkConditions(ifMatch, ifNoneMatch webdav.ConditionalMatch, existing db.File, exists bool) error {
	if ifNoneMatch.IsSet() {
		if ifNoneMatch.IsWildcard() && exists {
			return webdav.NewHTTPError(http.StatusPreconditionFailed, errors.New("it already exists"))
		}
		if !ifNoneMatch.IsWildcard() && exists {
			match, err := ifNoneMatch.MatchETag(existing.ETag)
			if err != nil {
				return webdav.NewHTTPError(http.StatusBadRequest, err)
			}
			if match {
				return webdav.NewHTTPError(http.StatusPreconditionFailed, errors.New("it matches"))
			}
		}
	}

	if ifMatch.IsSet() {
		if !exists {
			return webdav.NewHTTPError(http.StatusPreconditionFailed, errors.New("it does not exist"))
		}
		if !ifMatch.IsWildcard() {
			match, err := ifMatch.MatchETag(existing.ETag)
			if err != nil {
				return webdav.NewHTTPError(http.StatusBadRequest, err)
			}
			if !match {
				return webdav.NewHTTPError(http.StatusPreconditionFailed, errors.New("it has changed"))
			}
		}
	}
	return nil
}

// toPath turns a URL path into a storage path.
//
// Clean bounds the result at the root, so "/a/../../etc/passwd" becomes
// "/etc/passwd" rather than escaping. Whatever survives that still has to
// satisfy db.ValidatePath, which is where the rest is refused.
func toPath(name string) (string, error) {
	p := strings.Trim(path.Clean("/"+name), "/")
	if p == "" {
		return "", nil
	}
	if err := db.ValidatePath(p); err != nil {
		return "", webdav.NewHTTPError(http.StatusBadRequest, err)
	}
	return p, nil
}

// destPath is toPath for the Destination header, which the handler does not
// strip: it arrives as a URL or an absolute path, prefix and all.
func (f *fileSystem) destPath(dest string) (string, error) {
	p := dest
	if u, err := url.Parse(dest); err == nil {
		p = u.Path
	}
	trimmed := strings.TrimPrefix(path.Clean("/"+p), f.prefix)
	if trimmed == p && f.prefix != "" {
		// A destination outside this collection is somebody else's server as
		// far as we are concerned. RFC 4918 9.9.4 calls that 502.
		return "", webdav.NewHTTPError(http.StatusBadGateway, errors.New("the destination is outside this collection"))
	}
	return toPath(trimmed)
}

func (f *fileSystem) toFileInfo(file db.File) *webdav.FileInfo {
	info := toFileInfo(file)
	info.Path = f.prefix + info.Path
	return info
}

func toFileInfo(f db.File) *webdav.FileInfo {
	return &webdav.FileInfo{
		Path:     "/" + f.Path,
		Size:     f.Size,
		ModTime:  f.MTime,
		IsDir:    f.IsDir,
		MIMEType: f.MIMEType,
		ETag:     f.ETag,
	}
}

// mimeType guesses from the extension, which is all a WebDAV PUT gives us that
// is worth trusting: clients send application/octet-stream for everything.
func mimeType(p string) string {
	if t := typeByExtension(path.Ext(p)); t != "" {
		return t
	}
	return "application/octet-stream"
}

// mapErr turns this project's sentinels into the status codes WebDAV clients
// expect. It is the one piece of translation a library cannot do for us.
func mapErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, db.ErrNotFound), errors.Is(err, storage.ErrNotFound):
		return webdav.NewHTTPError(http.StatusNotFound, err)
	case errors.Is(err, db.ErrConflict):
		// 409 rather than 412: the request is inconsistent with the tree, which
		// is what a client fixes by creating the parent first.
		return webdav.NewHTTPError(http.StatusConflict, err)
	case errors.Is(err, db.ErrInvalidPath), errors.Is(err, storage.ErrInvalidKey):
		return webdav.NewHTTPError(http.StatusBadRequest, err)
	case errors.Is(err, storage.ErrInvalidRange):
		return webdav.NewHTTPError(http.StatusRequestedRangeNotSatisfiable, err)
	case errors.Is(err, files.ErrNoSpace):
		// RFC 4918 9.8.5 has this one for a copy with nowhere to land, and it
		// is the answer a client can act on rather than retry.
		return webdav.NewHTTPError(http.StatusInsufficientStorage, err)
	default:
		return fmt.Errorf("dav: %w", err)
	}
}
