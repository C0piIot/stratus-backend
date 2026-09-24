package web

import (
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/media"
	"github.com/C0piIot/stratus-backend/internal/storage"
)

// filesPrefix is where the tree lives. Every directory has one URL and every
// file has the same one, because to somebody typing it they are the same thing.
const filesPrefix = "/files/"

// listPageSize is how much of a directory one page holds. A folder with a
// hundred thousand photographs in it is an ordinary size for this project, and
// rendering all of it would be one query, one enormous document and, since
// #135, an offer of a thumbnail per row.
//
// Not configurable: it is a property of what a browser can lay out and of how
// many pictures it is fair to ask this server for at once, neither of which an
// operator is better placed to judge than the page is.
const listPageSize = 100

// listFragment is the part of the page htmx asks for when it extends a
// listing: the rows and the link to the next page, without the document around
// them.
const listFragment = "rows"

// browse answers both halves of "open this": a directory is a page, a file is
// its bytes.
func (h *handler) browse(w http.ResponseWriter, r *http.Request, user string) {
	// Empty for an ordinary request, and what makes the page read-only for a
	// shared one: every link below carries it, and the template drops
	// everything that writes.
	token := r.URL.Query().Get(shareParam)

	// Who the page says is signed in, which for a shared one is nobody. The
	// owner authorises the read and is not on show for it: a visitor with a
	// link has no account here, and a navbar naming somebody would be both a
	// door that does not open and a name they did not mean to publish.
	display := user
	if token != "" {
		display = ""
	}

	p, err := toPath(r.PathValue("path"))
	if err != nil {
		h.fail(w, r, display, err)
		return
	}

	// The root is the one directory with no row of its own -- an empty server
	// has nothing in it and still has a root.
	if p != "" {
		f, statErr := h.files.Stat(r.Context(), user, p)
		if statErr != nil {
			h.fail(w, r, display, statErr)
			return
		}
		if !f.IsDir {
			h.download(w, r, display, f)
			return
		}
	}

	after, err := parseCursor(r.URL.Query().Get("after"))
	if err != nil {
		h.badRequest(w, display, "That is not a place in this folder to carry on from.")
		return
	}
	children, more, err := h.files.ListPage(r.Context(), user, p, after, listPageSize)
	if err != nil {
		h.fail(w, r, display, err)
		return
	}

	v := view{
		Title:   pageTitle(p),
		User:    display,
		Shared:  token,
		Notice:  uploaded(r.URL.Query().Get("added")),
		Crumbs:  crumbs(p, token, sharedRoot(r)),
		Entries: entries(children, h.indexingOf(r, children), token),
		// Where this page's two forms post: into the directory being listed.
		Here:    href(p),
		Folders: link(folderPrefix, p),
	}
	if more {
		v.NextPage = shared(href(p)+"?after="+
			url.QueryEscape(encodeCursor(db.After(children[len(children)-1]))), token)
	}

	// htmx asked for the rows to append; anything else asked for the page. The
	// answer is not cached either way -- render sets no-store on both -- so
	// there is no Vary header to get wrong.
	if r.Header.Get("HX-Request") == "true" {
		h.renderTemplate(w, http.StatusOK, pageFiles, listFragment, v)
		return
	}
	h.render(w, http.StatusOK, pageFiles, v)
}

// encodeCursor and parseCursor carry a position in a listing through a URL.
//
// Not opaque and not signed: it is a path the browser already has and is
// allowed to see. The letter in front of it is the half a path cannot say --
// which of the two groups the ordering had reached -- and without it a page
// boundary that falls between the directories and the files could not be
// resumed.
func encodeCursor(c db.Cursor) string {
	if c.IsDir {
		return "d/" + c.Path
	}
	return "f/" + c.Path
}

func parseCursor(v string) (db.Cursor, error) {
	if v == "" {
		return db.Cursor{}, nil
	}
	kind, p, ok := strings.Cut(v, "/")
	if !ok {
		return db.Cursor{}, fmt.Errorf("cursor %q: no kind", v)
	}
	if err := db.ValidatePath(p); err != nil {
		return db.Cursor{}, err
	}
	switch kind {
	case "d":
		return db.Cursor{IsDir: true, Path: p}, nil
	case "f":
		return db.Cursor{Path: p}, nil
	default:
		return db.Cursor{}, fmt.Errorf("cursor %q: unknown kind %q", v, kind)
	}
}

// download streams a file.
func (h *handler) download(w http.ResponseWriter, r *http.Request, user string, f db.File) {
	body, err := h.files.OpenFile(r.Context(), f)
	if err != nil {
		h.fail(w, r, user, err)
		return
	}
	defer func() { _ = body.Close() }()

	disp := disposition(f.MIMEType)
	w.Header().Set("Content-Disposition",
		mime.FormatMediaType(disp, map[string]string{"filename": path.Base(f.Path)}))
	if disp == "inline" {
		w.Header().Set("Content-Security-Policy", fileContentSecurityPolicy)
	}
	if f.MIMEType != "" {
		w.Header().Set("Content-Type", f.MIMEType)
	}
	if f.ETag != "" {
		// Quoted here because that is HTTP's framing; internal/files stores the
		// validator itself, without quotes, for every surface to frame its own
		// way.
		w.Header().Set("ETag", strconv.Quote(f.ETag))
	}
	// Private and revalidated: it is one person's file, and the ETag makes
	// asking again cheap.
	w.Header().Set("Cache-Control", "private, no-cache")

	// ServeContent implements RFC 7233 over the seeker internal/files returns,
	// which is where ranges, conditional requests and resuming a download come
	// from -- none of it written here.
	http.ServeContent(w, r, path.Base(f.Path), f.MTime, body)
}

// fail turns what the layers below return into the page a browser should see.
func (h *handler) fail(w http.ResponseWriter, r *http.Request, user string, err error) {
	switch {
	case errors.Is(err, db.ErrNotFound), errors.Is(err, storage.ErrNotFound):
		h.render(w, http.StatusNotFound, pageError, view{
			Title: "Not found", User: user,
			Message: "There is nothing at " + r.URL.Path + ".",
		})
	case errors.Is(err, db.ErrInvalidPath), errors.Is(err, storage.ErrInvalidKey):
		h.render(w, http.StatusBadRequest, pageError, view{
			Title: "Bad request", User: user,
			Message: "That is not a path this server can answer.",
		})
	// 409 rather than 400, for the reason WebDAV gives it: the request is
	// inconsistent with the tree rather than malformed.
	case errors.Is(err, db.ErrConflict):
		h.render(w, http.StatusConflict, pageError, view{
			Title: "Not possible here", User: user,
			Message: "Something is already in the way of that.",
		})
	default:
		// Anything else is this server's fault, so it is logged here and
		// described in no detail there.
		slog.Error("serving a page", "path", r.URL.Path, "err", err)
		h.render(w, http.StatusInternalServerError, pageError, view{
			Title: "Something went wrong", User: user,
			Message: "The server could not answer that. The log has the detail.",
		})
	}
}

// toPath maps what came after /files/ onto a path in the tree, through the same
// validation every other surface uses -- there is no second implementation of
// what a path may contain, which is what keeps traversal a solved problem
// rather than a solved-twice one.
//
// internal/dav has the same four lines, and they stay duplicated on purpose:
// db.ValidatePath rejects rather than cleans, deliberately, because silently
// rewriting a stored path is how two names come to point at one row. Tidying a
// URL before it becomes a path is therefore the job of whoever speaks HTTP, and
// each adapter answers a bad one in its own shape -- a page here, an
// HTTPError there.
func toPath(name string) (string, error) {
	p := strings.Trim(path.Clean("/"+name), "/")
	if p == "" {
		return "", nil
	}
	return p, db.ValidatePath(p)
}

// href is the URL of a path in the tree, and link is the same for the other
// prefixes -- the forms and the two pages that act on one thing. Escaped, so a
// browser can follow what it is given.
func href(p string) string { return link(filesPrefix, p) }

func link(prefix, p string) string {
	u := url.URL{Path: prefix + p}
	return u.String()
}

// baseName is the last element of whatever a client sent, and nothing else.
//
// Every form here takes a name from somebody: an upload sends a filename, a new
// folder and a rename send a typed one. A directory upload sends a relative
// path, an old browser a whole Windows one, and a hostile client whatever it
// likes -- so what is used is the last element, and db.ValidatePath refuses it
// downstream if even that is not a name.
func baseName(sent string) string {
	return path.Base(strings.ReplaceAll(sent, `\`, "/"))
}

// editing resolves what a form is about: the path in the URL and the row it
// names. The root is not one of them -- nothing in the tree names it, and there
// is nowhere above it to go back to.
func (h *handler) editing(w http.ResponseWriter, r *http.Request, user string) (string, db.File, bool) {
	target, err := toPath(r.PathValue("path"))
	if err != nil {
		h.fail(w, r, user, err)
		return "", db.File{}, false
	}
	if target == "" {
		h.badRequest(w, user, "There is nothing there to change.")
		return "", db.File{}, false
	}
	f, err := h.files.Stat(r.Context(), user, target)
	if err != nil {
		h.fail(w, r, user, err)
		return "", db.File{}, false
	}
	return target, f, true
}

// redirectLocal sends the browser somewhere else on this server.
//
// One function so there is one line to audit: gosec's taint analysis cannot see
// through safeNext or href -- both of which build a path under this server --
// and seven identical nolint comments scattered about would be seven places for
// a real open redirect to hide.
func redirectLocal(w http.ResponseWriter, r *http.Request, target string) {
	//nolint:gosec // G710: every caller passes href's or safeNext's output.
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// badRequest is the answer to a form that was never going to work. It is the
// client's mistake, so it says what it can and no more.
func (h *handler) badRequest(w http.ResponseWriter, user, message string) {
	h.render(w, http.StatusBadRequest, pageError, view{
		Title: "Bad request", User: user, Message: message,
	})
}

// forbidden is the answer to a link that is not good any more. No User on the
// view, because whoever is reading this has no account here.
func (h *handler) forbidden(w http.ResponseWriter, message string) {
	h.render(w, http.StatusForbidden, pageError, view{
		Title: "Not available", Message: message,
	})
}

func pageTitle(dir string) string {
	if dir == "" {
		return "Files"
	}
	return path.Base(dir)
}

// crumb is one step of the trail back to the root.
type crumb struct {
	Name string
	Href string
	Last bool
}

// crumbs is the trail above the directory being listed.
//
// A shared one starts at the share and not at the root: climbing to "Files"
// would offer a door the link does not open, and stopping at the current folder
// would strand a visitor two levels down with no way back to what they were
// sent. So the trail is the share, then what is under it, and every step of it
// carries the signature.
func crumbs(dir, token, root string) []crumb {
	var trail []crumb
	walked := ""
	segments := dir

	if token == "" {
		trail = append(trail, crumb{Name: "Files", Href: filesPrefix})
	} else {
		trail = append(trail, crumb{Name: pageTitle(root), Href: shared(href(root), token)})
		walked = root
		segments = strings.TrimPrefix(strings.TrimPrefix(dir, root), "/")
	}

	if segments != "" {
		for _, seg := range strings.Split(segments, "/") {
			walked = path.Join(walked, seg)
			trail = append(trail, crumb{Name: seg, Href: shared(href(walked), token)})
		}
	}
	trail[len(trail)-1].Last = true
	return trail
}

// sharedRoot is where the link this request arrived on starts, or "" when it
// did not arrive on one.
func sharedRoot(r *http.Request) string {
	share, ok := shareOf(r.Context())
	if !ok {
		return ""
	}
	return share.Root
}

// entry is one row of the listing, with everything already in the shape the
// template prints: no formatting logic in the markup.
type entry struct {
	Name     string
	Href     string
	IsDir    bool
	Size     string
	Modified string
	// Thumb is where a picture of this file lives, and empty when this build
	// cannot make one. The ETag is in the URL so the answer can be cached for a
	// year: a new write takes a new blob key and therefore a new ETag.
	Thumb string
	// Indexing is what the extractor has made of this file, and empty when it
	// has been read -- which is the ordinary case, and a mark on every row
	// would be noise. It says something during a first pass over a library, a
	// re-index, or an import, which is when somebody is looking.
	Indexing string
	// Where the two things that can be done to it are asked for. Both are
	// pages: a rename needs a name, and a delete cannot be undone.
	Rename string
	Delete string
	Share  string
}

// entries renders what the port returned, in the order it returned it.
// Directories first and then by path is the ordering ListFilesPage promises,
// and it is the query's job rather than this function's: regrouping a page
// afterwards would only group that page, so a listing scrolled through would
// show folders, then files, then folders again.
func entries(children []db.File, indexing map[int64]string, token string) []entry {
	out := make([]entry, 0, len(children))
	for _, c := range children {
		e := entry{
			Name:     path.Base(c.Path),
			Href:     shared(href(c.Path), token),
			IsDir:    c.IsDir,
			Modified: c.MTime.Format("2006-01-02 15:04"),
			Rename:   link(renamePrefix, c.Path),
			Delete:   link(deletePrefix, c.Path),
			Share:    link(sharePrefix, c.Path),
		}
		if !c.IsDir {
			e.Size = humanSize(c.Size)
			e.Indexing = indexing[c.ID]
			if media.CanThumbnail(c.Path, c.Size) {
				// The thumbnail carries the signature too. Without it a shared
				// listing is a grid of broken images, which is the half of this
				// that fails quietly.
				e.Thumb = shared(link(thumbPrefix, c.Path)+"?size="+strconv.Itoa(listThumb)+
					"&v="+url.QueryEscape(c.ETag), token)
			}
		}
		out = append(out, e)
	}
	return out
}

// humanSize is the size a person reads rather than the one a machine counts.
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return strconv.FormatFloat(float64(n)/float64(div), 'f', 1, 64) + " " + string("KMGTPE"[exp]) + "B"
}

// disposition lets the browser open what cannot carry a script and hands
// everything else over as an attachment.
//
// This origin serves the UI and holds the session cookie, so an uploaded HTML
// page or SVG rendered here would run as the owner. The content security policy
// does not stop that on its own: script-src 'self' is satisfied by a .js file
// uploaded beside the page. The list is of types that are passive by
// construction, and a type nobody recorded is not one of them.
func disposition(mimeType string) string {
	mediaType, _, err := mime.ParseMediaType(mimeType)
	if err != nil {
		return "attachment"
	}
	switch top, _, _ := strings.Cut(mediaType, "/"); {
	case mediaType == "image/svg+xml":
		return "attachment"
	case top == "image", top == "video", top == "audio",
		mediaType == "application/pdf", mediaType == "text/plain":
		return "inline"
	}
	return "attachment"
}
