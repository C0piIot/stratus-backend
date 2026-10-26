package web

import (
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/stratus-private-cloud/stratus-backend/internal/db"
)

// Paths, links and the trail back to the root: what turns a request into a
// place in the tree and a place into something a page can link (#365).

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

// targetOf resolves what a page about one thing is about: the path in the URL
// and the row it names. The root is not one of them -- nothing in the tree
// names it, and there is nowhere above it to go back to.
func (h *handler) targetOf(w http.ResponseWriter, r *http.Request, user string) (string, db.File, bool) {
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

func pageTitle(dir string) string {
	if dir == "" {
		return "Files"
	}
	return path.Base(dir)
}

// titleOf is what the document title says, which is the whole trail and not the
// folder at the end of it: a browser's tabs, its history and a bookmark are all
// the title alone, and a dozen called "2024" tell nobody which library they are
// in. It is the same trail the breadcrumbs draw, so a shared page starts at the
// share for the reason crumbs gives.
func titleOf(dir, root string) string {
	if rel := below(dir, root); rel != "" {
		return rel
	}
	return pageTitle(root)
}

// below is dir relative to root, and dir itself when there is no root.
func below(dir, root string) string {
	return strings.TrimPrefix(strings.TrimPrefix(dir, root), "/")
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
	segments := below(dir, root)

	if token == "" {
		trail = append(trail, crumb{Name: "Files", Href: filesPrefix})
	} else {
		trail = append(trail, crumb{Name: pageTitle(root), Href: shared(href(root), token)})
		walked = root
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
