package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"
)

// ErrSessionInvalid is what every unusable session value returns: expired,
// tampered with, signed by another password, or not a session at all. One
// sentinel for all of them on purpose -- a browser is sent to the login form
// either way, and answering which is which says something about the key.
var ErrSessionInvalid = errors.New("auth: invalid session")

// DefaultSessionTTL is how long a web session lasts.
//
// A hard ceiling with no renewal, which is the trade a stateless session makes:
// there is no server-side record to delete, so logging out clears the browser's
// cookie and a stolen one stays usable until it expires. Renewing on use would
// remove the only bound there is.
const DefaultSessionTTL = 7 * 24 * time.Hour

// sessionVersion prefixes every value and is covered by the signature, so a
// value from an older format cannot be read as a newer one.
const sessionVersion = "s1"

// Sessions issues and verifies the signed cookie the web UI authenticates with.
//
// It is the third per-protocol adapter in this package -- Basic for WebDAV,
// md5+salt for OpenSubsonic, a signed cookie for a browser -- and it is here
// for the same reason as the other two: the password does not leave this
// package.
//
// Nothing is stored. The value carries who it is for and when it expires, and
// the signature is what makes both worth believing. Two consequences follow and
// the README says so: a restart logs nobody out, and changing the username or
// the password invalidates every session already issued, because the key is
// derived from them. That is the revocation a stateless design has.
type Sessions struct {
	key []byte
	ttl time.Duration
}

// NewSessions derives the signing key from the credentials.
//
// The derivation is HKDF's extract step by another name: HMAC over a context
// string, keyed by the password. crypto/hkdf computes the same thing, and its
// only failure is FIPS-140-only mode refusing a short password -- an error
// branch no test can reach in a package held at 100%. One HMAC has nothing to
// fail.
//
// The username is in there too, so renaming the user invalidates sessions just
// as changing the password does, and the context string keeps this key
// unrelated to anything else the same password is used for.
func NewSessions(c Credentials, ttl time.Duration) *Sessions {
	mac := hmac.New(sha256.New, []byte(c.Password))
	mac.Write([]byte("stratus web session v1\x00" + c.Username))
	return &Sessions{key: mac.Sum(nil), ttl: ttl}
}

// Issue returns the cookie value for username, and the moment it stops being
// accepted.
func (s *Sessions) Issue(username string, now time.Time) (value string, expires time.Time) {
	expires = now.Add(s.ttl)
	payload := strings.Join([]string{
		sessionVersion,
		base64.RawURLEncoding.EncodeToString([]byte(username)),
		strconv.FormatInt(expires.Unix(), 10),
	}, ".")
	return payload + "." + base64.RawURLEncoding.EncodeToString(s.sign(payload)), expires
}

// Verify returns who the value was issued to, or ErrSessionInvalid.
//
// The shape is parsed first and believed second: nothing the value claims --
// not the name, not the expiry -- is acted on until the signature over all of
// it has been checked.
func (s *Sessions) Verify(value string, now time.Time) (string, error) {
	parts := strings.Split(value, ".")
	if len(parts) != 4 || parts[0] != sessionVersion {
		return "", ErrSessionInvalid
	}
	username, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", ErrSessionInvalid
	}
	expires, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return "", ErrSessionInvalid
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil {
		return "", ErrSessionInvalid
	}

	if !hmac.Equal(sig, s.sign(strings.Join(parts[:3], "."))) {
		return "", ErrSessionInvalid
	}
	if !now.Before(time.Unix(expires, 0)) {
		return "", ErrSessionInvalid
	}
	return string(username), nil
}

func (s *Sessions) sign(payload string) []byte {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(payload))
	return mac.Sum(nil)
}
