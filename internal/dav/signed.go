package dav

import (
	"net/http"
	"strings"
	"time"

	"github.com/C0piIot/stratus-backend/internal/auth"
)

// A signed link standing in for a password, on the surface a client already
// talks to.
//
// The app speaks WebDAV and nothing else, and it needs a URL a Chromecast can
// fetch -- a receiver gets the media itself and cannot send an Authorization
// header (stratus-app#6). It can mint the signature itself, since that is a
// pure function of the credentials it already has, but the only address that
// took one was the browser surface's `/files/`.
//
// Making the app derive a second URL space would have worked and is the worse
// trade: `/files/` is the web UI, which is the thing most likely to change
// shape, while `/dav/` is a mount that will not move. A client should depend on
// the protocol surface.
//
// **This widens no authority.** The signature already authorises reading that
// path, and a GET here is exactly the bytes `/files/` would have served through
// the same ServeContent with the same ranges. What changes is the address it
// can be presented at, not what it opens.

// SignedLinks lets a valid share link authenticate a plain read, and leaves
// every other request exactly as it found it -- for those, the handler behind
// this is still HTTP Basic and still asks.
//
// GET and HEAD only. Not PROPFIND, so a folder link opens nothing here and
// stays what it is: something for a person, on the surface that renders HTML.
// Not writes, because a link is read-only wherever it is presented.
func SignedLinks(prefix string, shares *auth.Shares, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get(shareParam)
		if token == "" || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
			next.ServeHTTP(w, r)
			return
		}

		p, err := toPath(strings.TrimPrefix(r.URL.Path, prefix))
		if err != nil {
			// Not a path this server would serve, so there is nothing a
			// signature could authorise. Handed on rather than refused: what
			// answers it is the same 404 any other bad path gets.
			next.ServeHTTP(w, r)
			return
		}

		share, err := shares.Verify(token, p, time.Now())
		if err != nil {
			// A link that was offered and refused. Not handed on to Basic: a
			// receiver fetching a film needs a status code it can act on, not
			// a challenge for an account nobody has.
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r.WithContext(auth.WithUser(r.Context(), share.Owner)))
	})
}

// shareParam is where a link carries its signature, spelled the same way the
// browser surface spells it: one link has to work at both addresses.
const shareParam = "k"
