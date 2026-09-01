package auth

import (
	"net/http"
	"strings"
)

// Basic wraps h with HTTP Basic authentication.
//
// Basic rather than anything cleverer because it is what the clients speak:
// rclone, DAVx5, Finder and Thunderbird all send it, and inventing a login flow
// they do not implement would make the server unusable from the applications
// this project exists to serve. It is only ever safe over TLS, which is the
// deployment's job.
func Basic(realm string, creds Credentials, h http.Handler) http.Handler {
	// A realm reaches the client inside a quoted string, so a quote in it would
	// produce a header the client cannot parse. It comes from us, not from a
	// request, and stripping is enough to keep that true.
	challenge := `Basic realm="` + strings.NewReplacer(`"`, "", `\`, "").Replace(realm) + `", charset="UTF-8"`

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		if !ok {
			// No credentials at all, or a header we cannot parse. Both are the
			// same answer: here is how to authenticate.
			unauthorized(w, challenge)
			return
		}
		if err := creds.Verify(username, password); err != nil {
			unauthorized(w, challenge)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func unauthorized(w http.ResponseWriter, challenge string) {
	w.Header().Set("WWW-Authenticate", challenge)
	// charset=UTF-8 on the body too: the status text is ASCII, but saying so
	// keeps a client from guessing.
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}
