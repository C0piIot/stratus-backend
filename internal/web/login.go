package web

import (
	"errors"
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
func (h *handler) signedIn(page func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, err := h.session(r)
		if err != nil {
			redirectLocal(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()))
			return
		}
		page(w, r, user)
	}
}

// withPicture is readable plus HTTP Basic, and it wraps exactly one route.
//
// A thumbnail is the one thing this surface holds that another surface needs.
// #136 put a has-preview property in the PROPFIND listing so a client drawing a
// grid knows which tiles to ask for -- and the client that asks is the app,
// which authenticates over WebDAV with Basic and could not reach /thumb/ at
// all. A property that points at nothing is worse than no property.
//
// **This is the extension principle 2 warns about, taken knowingly.** There is
// no standard way to ask a WebDAV server for a preview -- #136 checked, and the
// only convention with deployment behind it is one vendor's. What keeps this on
// the right side of the line is that nothing depends on it: a client that does
// not know this URL renders no thumbnails and works, which is what the app does
// against any other WebDAV server today.
//
// The same verifier as the other surfaces, so a wrong password here counts
// against the same rate limit rather than opening an oracle beside it.
func (h *handler) withPicture(page func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		if !ok {
			h.readable(page)(w, r)
			return
		}
		switch err := h.verifier.Verify(r.Context(), username, password); {
		case errors.Is(err, auth.ErrTooManyAttempts):
			// 429 and not 401, for the reason auth.Basic gives: the
			// credentials were never judged, so saying "unauthorized" would be
			// a guess.
			w.Header().Set("Retry-After", "2")
			http.Error(w, "too many attempts", http.StatusTooManyRequests)
			return
		case err != nil:
			// No challenge header: this is not a surface a browser should be
			// prompted for, and the client that sends Basic here already knows
			// what it is doing.
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		page(w, r, username)
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
	c, err := r.Cookie(cookieName)
	if err != nil {
		return "", auth.ErrSessionInvalid
	}
	return h.sessions.Verify(c.Value, time.Now())
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
