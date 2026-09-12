package web

import (
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/C0piIot/stratus-backend/internal/auth"
)

// authenticated is the gate in front of every page that is not the login form.
// A browser with no usable session is sent to it rather than refused, and told
// where it was going so a bookmark deep in the UI survives signing in.
func (h *handler) authenticated(page func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, err := h.session(r)
		if err != nil {
			redirectLocal(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()))
			return
		}
		page(w, r, user)
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
