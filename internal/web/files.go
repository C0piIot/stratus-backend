package web

import (
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/storage"
)

// filesPrefix is where the tree lives. Every directory has one URL and every
// file has the same one, because to somebody typing it they are the same thing.
const filesPrefix = "/files/"

// browse answers both halves of "open this": a directory is a page, a file is
// its bytes.
func (h *handler) browse(w http.ResponseWriter, r *http.Request, user string) {
	p, err := toPath(r.PathValue("path"))
	if err != nil {
		h.fail(w, r, user, err)
		return
	}

	// The root is the one directory with no row of its own -- an empty server
	// has nothing in it and still has a root.
	if p != "" {
		f, statErr := h.files.Stat(r.Context(), user, p)
		if statErr != nil {
			h.fail(w, r, user, statErr)
			return
		}
		if !f.IsDir {
			h.download(w, r, user, f)
			return
		}
	}

	children, err := h.files.List(r.Context(), user, p)
	if err != nil {
		h.fail(w, r, user, err)
		return
	}
	h.render(w, http.StatusOK, pageFiles, view{
		Title:   pageTitle(p),
		User:    user,
		Notice:  uploaded(r.URL.Query().Get("added")),
		Crumbs:  crumbs(p),
		Entries: entries(children),
		// Where this page's two forms post: into the directory being listed.
		Here:    href(p),
		Folders: (&url.URL{Path: folderPrefix + p}).String(),
	})
}

// download streams a file.
func (h *handler) download(w http.ResponseWriter, r *http.Request, user string, f db.File) {
	body, err := h.files.OpenFile(r.Context(), f)
	if err != nil {
		h.fail(w, r, user, err)
		return
	}
	defer func() { _ = body.Close() }()

	// An attachment rather than something the browser renders. This origin
	// serves the UI, and a file somebody uploaded is not the UI's to display in
	// it -- the content security policy would already stop a script inside one,
	// and the disposition is what stops the question from arising at all.
	w.Header().Set("Content-Disposition",
		mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(f.Path)}))
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
func toPath(name string) (string, error) {
	p := strings.Trim(path.Clean("/"+name), "/")
	if p == "" {
		return "", nil
	}
	return p, db.ValidatePath(p)
}

// href is the URL of a path in the tree, escaped for a browser to follow.
func href(p string) string {
	u := url.URL{Path: filesPrefix + p}
	return u.String()
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

func crumbs(dir string) []crumb {
	trail := []crumb{{Name: "Files", Href: filesPrefix}}
	if dir != "" {
		var walked string
		for _, seg := range strings.Split(dir, "/") {
			walked = path.Join(walked, seg)
			trail = append(trail, crumb{Name: seg, Href: href(walked)})
		}
	}
	trail[len(trail)-1].Last = true
	return trail
}

// entry is one row of the listing, with everything already in the shape the
// template prints: no formatting logic in the markup.
type entry struct {
	Name     string
	Href     string
	IsDir    bool
	Size     string
	Modified string
}

func entries(children []db.File) []entry {
	// Directories first, which is what every file manager does and what makes a
	// deep tree navigable; within each group, the order the port returned,
	// which is by path. Two slices rather than a sort: the port has already
	// done the ordering, and all that is left is which group a row is in.
	dirs := make([]entry, 0, len(children))
	rest := make([]entry, 0, len(children))
	for _, c := range children {
		e := entry{
			Name:     path.Base(c.Path),
			Href:     href(c.Path),
			IsDir:    c.IsDir,
			Modified: c.MTime.Format("2006-01-02 15:04"),
		}
		if c.IsDir {
			dirs = append(dirs, e)
			continue
		}
		e.Size = humanSize(c.Size)
		rest = append(rest, e)
	}
	return append(dirs, rest...)
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
