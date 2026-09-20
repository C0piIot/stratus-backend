package web

import (
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
)

const sharePrefix = "/share/"

// Making a link somebody without an account can open.
//
// The same shape as rename and delete: a GET that asks and a POST that does.
// What it asks is how long the link should last, because that is the only
// control a stateless link has -- there is nothing to revoke afterwards short
// of changing the password, which revokes every link and every session at once.
//
// What comes back is the ordinary URL with a signature on it. There is no share
// page and no share surface: /files/<path> already serves a file's bytes with
// ranges and renders a folder as HTML, so the link is that URL and a browser
// needs to be told nothing new (#169).

// shareLife is one of the lifetimes on offer: what the form posts, what it
// reads as, and how long it is. None is one of them.
type shareLifetime struct {
	Value string
	Label string
	For   time.Duration
}

// shareLives are the expiries offered.
var shareLives = []shareLifetime{
	{"1d", "a day", 24 * time.Hour},
	{"7d", "a week", 7 * 24 * time.Hour},
	{"30d", "a month", 30 * 24 * time.Hour},
	{"never", "until the password changes", 0},
}

func (h *handler) shareForm(w http.ResponseWriter, r *http.Request, user string) {
	target, f, ok := h.editing(w, r, user)
	if !ok {
		return
	}
	h.render(w, http.StatusOK, pageShare, view{
		Title: "Share", User: user,
		Name: path.Base(target), IsDir: f.IsDir,
		Action: link(sharePrefix, target), Back: href(db.ParentOf(target)),
		Lives: shareLives,
	})
}

// share issues the link and shows it.
//
// Shown rather than redirected to: the link is the answer to the question, and
// sending the browser to it would open the share instead of handing it over.
func (h *handler) share(w http.ResponseWriter, r *http.Request, user string) {
	target, f, ok := h.editing(w, r, user)
	if !ok {
		return
	}

	life, ok := shareLife(r.PostFormValue("life"))
	if !ok {
		h.badRequest(w, user, "That is not one of the lifetimes on offer.")
		return
	}
	var expires time.Time
	if life > 0 {
		expires = time.Now().Add(life)
	}

	// A folder link covers what is under it, or every file inside would need a
	// link of its own; a file link is one file.
	token := h.shares.Issue(user, target, f.IsDir, expires)

	h.render(w, http.StatusOK, pageShared, view{
		Title: "Share", User: user,
		Name: path.Base(target), IsDir: f.IsDir,
		Back:    href(db.ParentOf(target)),
		Link:    absolute(r, shared(href(target), token)),
		Expires: expiryLabel(expires),
	})
}

// shared puts the signature on a URL this UI already knows how to build.
//
// Appended rather than parsed and rebuilt: every target comes from href or
// link, which build a path, so the only question is whether there is a query on
// it already -- and a parse here would add an error branch nothing can reach.
func shared(target, token string) string {
	if token == "" {
		return target
	}
	separator := "?"
	if strings.Contains(target, "?") {
		separator = "&"
	}
	return target + separator + shareParam + "=" + url.QueryEscape(token)
}

// absolute turns a path on this server into the URL somebody can be sent.
//
// It was relative at first, on the argument that behind a reverse proxy the
// Host header is a claim rather than a fact. That was true and beside the
// point: a link you have to assemble by hand is not a link, which is the whole
// of what this page produces.
//
// So it is built from the request, the way every self-hosted server builds
// one. What it costs is a wrong link when a proxy does not pass the name it is
// reached by -- shown to the owner, who can see that it is wrong, on a page
// that is never cached. A configured external URL is the fix for that, and it
// waits until somebody's proxy is actually wrong.
func absolute(r *http.Request, path string) string {
	scheme := "http"
	switch {
	case r.TLS != nil:
		scheme = "https"
	case r.Header.Get("X-Forwarded-Proto") == "https":
		// A claim, and believed for the same reason the host is: getting it
		// wrong shows somebody a link they can see is wrong.
		scheme = "https"
	}
	return scheme + "://" + r.Host + path
}

func shareLife(value string) (time.Duration, bool) {
	for _, life := range shareLives {
		if life.Value == value {
			return life.For, true
		}
	}
	return 0, false
}

func expiryLabel(expires time.Time) string {
	if expires.IsZero() {
		return ""
	}
	return expires.UTC().Format("2 January 2006, 15:04 MST")
}
