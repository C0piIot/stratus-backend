package subsonic

import (
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"github.com/C0piIot/stratus-backend/internal/auth"
)

// Subsonic error codes, the ones this adapter answers with. The rest of the
// list exists but nothing here produces it.
const (
	errGeneric      = 0
	errMissingParam = 10
	errBadLogin     = 40
	errNoTokenAuth  = 41
	errConflicting  = 43
	errNotFound     = 70
)

// Verifier is what this adapter authenticates against: both schemes a Subsonic
// client may use. Declared here rather than taking auth.Throttle whole, so the
// dependency says what it needs -- and needing both is the point, because the
// two must share one rate limit.
type Verifier interface {
	auth.Verifier
	auth.TokenVerifier
}

// authenticate checks the credentials on the query string and returns who the
// caller is.
//
// Credentials in a URL is the protocol's design, not a choice made here: they
// land in access logs and in Referer headers, and the only mitigation the
// specification offers is an extension no client is obliged to use.
func (h *handler) authenticate(r *http.Request) (string, *apiError) {
	q := r.URL.Query()
	username, password := q.Get("u"), q.Get("p")
	token, salt := q.Get("t"), q.Get("s")

	// c identifies the client and every one of them sends it. v is required
	// too, but nothing here negotiates a version, so demanding it would refuse
	// a client for no benefit.
	if q.Get("c") == "" {
		return "", &apiError{errMissingParam, "the c parameter is required"}
	}
	if username == "" {
		return "", &apiError{errMissingParam, "the u parameter is required"}
	}

	switch {
	case password != "" && token != "":
		return "", &apiError{errConflicting, "send either p, or t and s, not both"}
	case password != "":
		clear, err := decodePassword(password)
		if err != nil {
			return "", &apiError{errMissingParam, "p is not a valid enc: value"}
		}
		return username, h.classify(h.verifier.Verify(r.Context(), username, clear))
	case token != "":
		return username, h.classify(h.verifier.VerifyToken(r.Context(), username, token, salt))
	default:
		return "", &apiError{errMissingParam, "send either p, or t and s"}
	}
}

// classify turns this project's sentinels into the protocol's codes.
func (h *handler) classify(err error) *apiError {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, auth.ErrTokenUnsupported):
		// The specification's own note: the text says LDAP, but the code means
		// token authentication is unavailable for any reason at all.
		return &apiError{errNoTokenAuth, "token authentication is not available"}
	case errors.Is(err, auth.ErrTooManyAttempts):
		// Not 40. The credentials were never judged, so saying they were wrong
		// would send a client to re-prompt for a password that may be right.
		// The protocol has no code for "later", so this is the generic one with
		// a message that says which.
		return &apiError{errGeneric, "too many authentication attempts, try again shortly"}
	default:
		return &apiError{errBadLogin, "wrong username or password"}
	}
}

// decodePassword undoes the enc: form, which is hex and not encryption: the
// specification says as much and tells clients not to use it outside testing.
func decodePassword(p string) (string, error) {
	encoded, ok := strings.CutPrefix(p, "enc:")
	if !ok {
		return p, nil
	}
	decoded, err := hex.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	return string(decoded), nil
}
