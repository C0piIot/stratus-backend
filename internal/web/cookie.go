package web

import (
	"net/http"
	"strings"
	"time"
)

// cookieName is the browser's half of the session. The value is signed rather
// than looked up, so what is in here is the whole credential -- hence HttpOnly,
// which keeps it out of reach of any script that ever ends up on the page.
const cookieName = "stratus_session"

func setSession(w http.ResponseWriter, r *http.Request, value string, expires time.Time) {
	http.SetCookie(w, cookie(r, value, int(time.Until(expires).Seconds())))
}

// clearSession is what signing out does. A stateless session has nothing on the
// server to delete, so this is the whole of it.
func clearSession(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, cookie(r, "", -1))
}

func cookie(r *http.Request, value string, maxAge int) *http.Cookie {
	//nolint:gosec // G124 wants Secure unconditionally; see overTLS for why it follows the request instead.
	return &http.Cookie{
		Name:   cookieName,
		Value:  value,
		Path:   "/",
		MaxAge: maxAge,
		// SameSite=Lax is the first half of the CSRF defence: a cross-site POST
		// carries no cookie, so it cannot be authenticated in the first place.
		// http.CrossOriginProtection in Handler is the second.
		SameSite: http.SameSiteLaxMode,
		HttpOnly: true,
		Secure:   overTLS(r),
	}
}

// overTLS reports whether the request arrived encrypted, directly or through a
// proxy that terminated it.
//
// Trusting X-Forwarded-Proto from an unauthenticated client is safe here in the
// one direction that matters: a lie can only add the Secure attribute, never
// remove it, so the worst a forger achieves is a cookie their own browser
// refuses to send back over plain HTTP. Requiring Secure unconditionally would
// be the real hazard -- every self-hoster on http://box.lan:8080 would get a
// cookie the browser stores and never returns.
func overTLS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}
