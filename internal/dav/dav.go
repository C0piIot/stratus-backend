// Package dav is the inbound WebDAV adapter: it translates protocol bytes into
// calls on internal/files and back.
//
// The XML, the multistatus responses and the method dispatch come from
// github.com/emersion/go-webdav. What is written here is the backend it drives
// and, mostly, the mapping from this project's sentinel errors onto status
// codes -- which is the part a library cannot guess.
//
// **This file is the only one that knows which library that is.** Everything
// else in the package -- locking, paths, MIME types -- is written against
// net/http and this project's own types, so replacing the library means
// rewriting one file rather than the package. That is deliberate: the choice
// has been questioned once already (#3) and may be again.
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

	"github.com/emersion/go-webdav"

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
	fs := &fileSystem{files: service, prefix: prefix}
	dav := &webdav.Handler{FileSystem: fs}

	return http.StripPrefix(prefix, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// LOCK and UNLOCK never reach the library: it is class 1 and answers
		// 405 to both. See lock.go for what these do and what they do not.
		switch r.Method {
		case "LOCK":
			fs.handleLock(w, r)
		case "UNLOCK":
			handleUnlock(w, r)
		case http.MethodOptions:
			dav.ServeHTTP(&advertiseLocking{ResponseWriter: w}, r)
		default:
			dav.ServeHTTP(w, r)
		}
	}))
}

type fileSystem struct {
	files *files.Service
	// prefix is stripped from the request path by the handler above, but it is
	// still on the Destination header, and it has to be put back on every href
	// in a multistatus or the client follows a link to nowhere.
	prefix string
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

// ReadDir implements webdav.FileSystem.
func (f *fileSystem) ReadDir(ctx context.Context, name string, recursive bool) ([]webdav.FileInfo, error) {
	p, err := toPath(name)
	if err != nil {
		return nil, err
	}

	owner, err := f.owner(ctx)
	if err != nil {
		return nil, err
	}

	var listing []db.File
	if recursive {
		listing, err = f.files.Walk(ctx, owner, p)
	} else {
		listing, err = f.files.List(ctx, owner, p)
	}
	if err != nil {
		return nil, mapErr(err)
	}

	out := make([]webdav.FileInfo, 0, len(listing))
	for _, file := range listing {
		out = append(out, *f.toFileInfo(file))
	}
	return out, nil
}

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

	source, err := f.files.Stat(ctx, owner, from)
	if err != nil {
		return false, mapErr(err)
	}
	if source.IsDir {
		return false, webdav.NewHTTPError(http.StatusNotImplemented, errors.New("copying a collection is not supported"))
	}

	_, statErr := f.files.Stat(ctx, owner, to)
	if statErr == nil && opts != nil && opts.NoOverwrite {
		return false, webdav.NewHTTPError(http.StatusPreconditionFailed, errors.New("the destination exists"))
	}
	if statErr != nil && !errors.Is(statErr, db.ErrNotFound) {
		return false, mapErr(statErr)
	}

	body, _, err := f.files.Open(ctx, owner, from)
	if err != nil {
		return false, mapErr(err)
	}
	defer func() { _ = body.Close() }()

	if _, err := f.files.Write(ctx, owner, to, body, source.Size, source.MIMEType); err != nil {
		return false, mapErr(err)
	}
	return statErr != nil, nil
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
	default:
		return fmt.Errorf("dav: %w", err)
	}
}
