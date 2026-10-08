package web

import (
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"time"

	"github.com/C0piIot/stratus-backend/internal/auth"
)

// shareParam is where a link carries its signature. A query parameter and not a
// path segment, so that a shared URL is the ordinary URL with something on the
// end rather than a second address for the same thing (#169).
const shareParam = "k"

// signedIn is the gate in front of every page that is not the login form. A
// browser with no usable session is sent to it rather than refused, and told
// where it was going so a bookmark deep in the UI survives signing in.
//
// HTTP Basic is the other way in (#234), and the one the app takes to /thumb/:
// #136 put a has-preview property in the PROPFIND listing, and the client that
// reads it authenticates over WebDAV. Refused Basic is a status and not the
// login form -- whoever sends it is a client that already knows what it is
// doing, and a 401 is what it can act on.
//
// The same verifier as the other surfaces, so a wrong password here counts
// against the same rate limit rather than opening an oracle beside it.
func (h *handler) signedIn(page func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if user, err := h.session(r); err == nil {
			page(w, r, user)
			return
		}
		user, sent, err := auth.BasicUser(r, h.verifier)
		switch {
		case !sent:
			// Nobody said who they are, and what to answer depends on who is
			// asking -- which since #279 is a real question, because one URL
			// now serves a browser and a WebDAV client.
			//
			// A browser navigating needs the login page; a client needs the
			// challenge, or it never sends credentials at all. The
			// discriminator is the one #234 already trusts: only a browser
			// sends Sec-Fetch-*, and it says what the request is for. A
			// request that is not a navigation -- an <img>, a fetch, a WebDAV
			// GET -- gets the challenge, which for the image means a broken
			// picture rather than a login page rendered inside one.
			next := "/login?next=" + url.QueryEscape(r.URL.RequestURI())
			if r.Header.Get("Sec-Fetch-Mode") != "navigate" {
				challenge(w, next)
				return
			}
			redirectLocal(w, r, next)
			return
		case errors.Is(err, auth.ErrTooManyAttempts):
			// 429 and not 401, for the reason auth.Basic gives: the
			// credentials were never judged, so saying "unauthorized" would be
			// a guess.
			w.Header().Set("Retry-After", "2")
			http.Error(w, "too many attempts", http.StatusTooManyRequests)
			return
		case errors.Is(err, auth.ErrCrossSite):
			http.Error(w, "cross-site request", http.StatusForbidden)
			return
		case err != nil:
			// No challenge header: this is not a surface a browser should be
			// prompted for.
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		page(w, r, user)
	}
}

// readable is signedIn plus the other way in: a signed link, which authorises
// reading one path or one subtree and nothing else.
//
// It wraps the routes that only read, and the routes that write are wrapped by
// signedIn instead -- so a link is read-only because of which gate it goes
// through, not because of a check somebody has to remember to write.
//
// A link that was offered and refused is 403 and not a redirect. Somebody sent
// a dead link needs to be told it is dead rather than shown a login form for an
// account they do not have, and a Chromecast fetching a film needs a status
// code rather than HTML.
func (h *handler) readable(page func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get(shareParam)
		if token == "" {
			h.signedIn(page)(w, r)
			return
		}

		p, err := toPath(r.PathValue("path"))
		if err != nil {
			h.forbidden(w, "That link does not point anywhere here.")
			return
		}
		share, err := h.shares.Verify(token, p, time.Now())
		if err != nil {
			h.forbidden(w, "That link has expired or is not valid any more.")
			return
		}
		page(w, r.WithContext(withShare(r.Context(), share)), share.Owner)
	}
}

// session returns who the request's cookie was issued to.
func (h *handler) session(r *http.Request) (string, error) {
	return h.sessions.FromCookie(r, time.Now())
}

func (h *handler) loginForm(w http.ResponseWriter, r *http.Request) {
	next := safeNext(r.FormValue("next"))
	// Already signed in: showing the form again would invite a pointless second
	// login, and the answer to "where was I going" is the same either way.
	if _, err := h.session(r); err == nil {
		redirectLocal(w, r, next)
		return
	}
	h.render(w, http.StatusOK, pageLogin, view{Title: "Sign in", Next: next})
}

func (h *handler) login(w http.ResponseWriter, r *http.Request) {
	next := safeNext(r.PostFormValue("next"))
	user, password := r.PostFormValue("username"), r.PostFormValue("password")

	switch err := h.verifier.Verify(r.Context(), user, password); {
	case errors.Is(err, auth.ErrTooManyAttempts):
		// The same shared budget WebDAV and OpenSubsonic guess against, so a
		// login form does not hand an attacker a third one.
		w.Header().Set("Retry-After", "2")
		h.render(w, http.StatusTooManyRequests, pageLogin, view{
			Title: "Sign in", Next: next, Username: user,
			Error: "Too many attempts. Try again in a moment.",
		})
		return
	case err != nil:
		// One message for a wrong name and a wrong password, for the reason
		// internal/auth returns one error for both.
		//
		// No WWW-Authenticate header with this 401: it would put the browser's
		// own credential dialog on top of the form the user is looking at.
		h.render(w, http.StatusUnauthorized, pageLogin, view{
			Title: "Sign in", Next: next, Username: user,
			Error: "Wrong username or password.",
		})
		return
	}

	value, expires := h.sessions.Issue(user, time.Now())
	setSession(w, r, value, expires)
	redirectLocal(w, r, next)
}

// logout clears the cookie, which is all a stateless session can be asked for:
// there is no record of it on the server to delete. See auth.Sessions.
func (h *handler) logout(w http.ResponseWriter, r *http.Request) {
	clearSession(w, r)
	redirectLocal(w, r, "/login")
}

// safeNext keeps the login form from becoming an open redirect. Only a path on
// this server survives; anything else lands on the home page.
func safeNext(raw string) string {
	const home = "/"
	// A leading "//" or "/\" is a scheme-relative URL to somebody else's host,
	// which some browsers accept in either spelling.
	if len(raw) < 2 || raw[0] != '/' || raw[1] == '/' || raw[1] == '\\' {
		return home
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "" || u.Host != "" {
		return home
	}
	return u.String()
}

// challenge refuses a request that did not look like a browser navigating. The
// header is what a WebDAV client acts on; the body is for the browser that
// should have been redirected and was not, and it is a floor rather than a
// second mechanism -- whoever is told apart correctly never reaches it.
//
// Two browsers reach it. Firefox drops Sec-Fetch-Mode when a service worker
// re-issues a navigation, which is why the worker stopped re-issuing them, and
// Safari sent none at all before 16.4. Both land on the login page from here
// instead of on the word "unauthorized". A browser shown the native dialog
// first only sees this if it cancels, which is the state this is for; a client
// ignores the markup exactly as it ignored the plain text.
func challenge(w http.ResponseWriter, login string) {
	w.Header().Set("WWW-Authenticate", auth.Challenge(auth.Realm))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	// The link is what answers a browser that honours no refresh, and the
	// escaping is template.HTMLEscapeString rather than a template because one
	// value in a fixed string is not a page.
	safe := template.HTMLEscapeString(login)
	_, _ = fmt.Fprintf(w, `<!doctype html>
<meta charset="utf-8">
<meta http-equiv="refresh" content="0; url=%s">
<title>Sign in</title>
<a href="%s">Sign in</a>
`, safe, safe)
}
