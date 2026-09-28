package auth

import (
	"errors"
	"net/http"
	"time"
)

// SessionCookie is where a browser keeps the value Sessions issues.
const SessionCookie = "stratus_session"

// ErrCrossSite means a credential the browser attaches on its own -- a cookie,
// or Basic it has cached -- arrived on a request another site started.
var ErrCrossSite = errors.New("auth: credential sent from another site")

// FromCookie returns who the request's session cookie was issued to.
func (s *Sessions) FromCookie(r *http.Request, now time.Time) (string, error) {
	c, err := r.Cookie(SessionCookie)
	if err != nil {
		return "", ErrSessionInvalid
	}
	return s.Verify(c.Value, now)
}

// Session lets a signed-in browser through a protocol surface, so a page can
// use the protocol rather than grow an endpoint of its own (#234). A request
// with no usable cookie passes through untouched, for whatever authenticates
// behind this to judge.
//
// OpenSubsonic changes state over GET, and SameSite=Lax sends the cookie on a
// top-level navigation from anywhere, so a link on somebody else's page to
// /rest/deletePlaylist would be authenticated. Hence the cookie counts only
// when the browser says the request is its own, and a browser that says
// nothing is not given the benefit of the doubt: every one that still gets
// updates sends Sec-Fetch-Site, and nothing that is not a browser has this
// cookie to send.
func Session(s *Sessions, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := User(r.Context()); !ok && sameOrigin(r) {
			if username, err := s.FromCookie(r, time.Now()); err == nil {
				r = r.WithContext(WithUser(r.Context(), username))
			}
		}
		h.ServeHTTP(w, r)
	})
}

// BasicUser verifies the request's Basic credentials, and reports whether it
// carried any.
//
// A browser caches Basic and attaches it the way it does a cookie, so the same
// rule applies with one difference: a request that does not say where it comes
// from is a client, not a browser, and is judged on its password. The check
// runs before the password is, so a cross-site request spends nothing from the
// throttle.
func BasicUser(r *http.Request, v Verifier) (username string, sent bool, err error) {
	username, password, ok := r.BasicAuth()
	if !ok {
		return "", false, nil
	}
	if crossSite(r) {
		return "", true, ErrCrossSite
	}
	if err := v.Verify(r.Context(), username, password); err != nil {
		return "", true, err
	}
	return username, true, nil
}

// sameOrigin is true only when the browser says it started the request
// itself: from one of this origin's pages, or from the address bar.
func sameOrigin(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none":
		return true
	}
	return false
}

// crossSite is true when the browser says another site started the request.
// same-site counts as another site: a sibling subdomain is somebody else's.
func crossSite(r *http.Request) bool {
	return r.Header.Get("Sec-Fetch-Site") != "" && !sameOrigin(r)
}
