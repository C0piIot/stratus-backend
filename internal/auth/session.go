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

// DefaultSessionTTL is how long a web session lasts from its last use.
//
// It used to be a hard ceiling with no renewal, because a stateless session
// has no server-side record to delete and the expiry was the only bound a
// copied cookie had. That signed out somebody using the UI every day on every
// seventh one, which is a cost paid by the person and not by the thief (#334).
// So it slides now, and it slides without a cap, by decision: a cookie that
// keeps being used keeps working, and what ends it is the password changing,
// which is the revocation this design has always had and the one that is
// actually reached for.
const DefaultSessionTTL = 14 * 24 * time.Hour

// sessionRenewEvery is how old a value has to be before a use re-signs it.
// Once a day rather than every request, so that a page of a hundred
// thumbnails does not answer with a hundred cookies.
const sessionRenewEvery = 24 * time.Hour

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
// derived from them. That is the revocation a stateless design has, and
// renewing a value signs it with the same key, so a session that has been
// kept alive for a year dies with the password exactly as a fresh one does.
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
func (s *Sessions) Verify(value string, now time.Time) (string, error) {
	username, _, err := s.parse(value, now)
	return username, err
}

// Renew is what a use of a value does to it: a fresh one for the same name,
// with the expiry moved out from now, and the moment that one stops being
// accepted. ok is false when there is nothing to send -- the value is not
// good, or it was signed less than sessionRenewEvery ago and the one the
// browser holds is as good as the one this would make.
func (s *Sessions) Renew(value string, now time.Time) (renewed string, expires time.Time, ok bool) {
	username, until, err := s.parse(value, now)
	if err != nil || now.Sub(until.Add(-s.ttl)) < sessionRenewEvery {
		return "", time.Time{}, false
	}
	renewed, expires = s.Issue(username, now)
	return renewed, expires, true
}

// parse is the shape first and the belief second: nothing the value claims --
// not the name, not the expiry -- is acted on until the signature over all of
// it has been checked.
func (s *Sessions) parse(value string, now time.Time) (username string, expires time.Time, err error) {
	parts := strings.Split(value, ".")
	if len(parts) != 4 || parts[0] != sessionVersion {
		return "", time.Time{}, ErrSessionInvalid
	}
	name, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", time.Time{}, ErrSessionInvalid
	}
	unix, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return "", time.Time{}, ErrSessionInvalid
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil {
		return "", time.Time{}, ErrSessionInvalid
	}

	if !hmac.Equal(sig, s.sign(strings.Join(parts[:3], "."))) {
		return "", time.Time{}, ErrSessionInvalid
	}
	expires = time.Unix(unix, 0)
	if !now.Before(expires) {
		return "", time.Time{}, ErrSessionInvalid
	}
	return string(name), expires, nil
}

func (s *Sessions) sign(payload string) []byte {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(payload))
	return mac.Sum(nil)
}
