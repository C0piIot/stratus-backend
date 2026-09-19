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

// A link somebody without an account can open.
//
// The fourth credential adapter in this package, and the same idea as the
// third: nothing is stored, the value carries what it claims, and the signature
// is what makes the claims worth believing. Two things wanted this one
// mechanism -- sharing with a person, and casting, since a Chromecast fetches
// the film itself and cannot send an Authorization header (#169).
//
// What a link authorises is a path and a direction: read this file, or read
// inside this folder. It authorises nothing else, and it is checked against the
// path the request actually asked for rather than the one the token names,
// which is the whole of "a folder link reaches what is under it and nothing
// above it".
//
// Stateless, and the three consequences are on the record rather than
// overlooked: there is no list of what has been shared, no revoking one link
// short of changing the password -- which revokes every link and every session
// at once, since both keys come from it -- and a shared path that is renamed
// breaks its own link, because a signature names a path and not a file.

// ErrShareInvalid is what every unusable link returns: expired, tampered with,
// signed by another password, offered for a path it does not cover, or not a
// link at all. One sentinel for the same reason ErrSessionInvalid is one --
// answering which is which says something about the key.
var ErrShareInvalid = errors.New("auth: invalid share link")

// shareVersion prefixes every token and is covered by the signature, so a token
// of an older shape cannot be read as a newer one.
const shareVersion = "k1"

// The two shapes a link has, written into the token and signed with it: one
// file, or a folder and everything under it.
const (
	shareFile    = "f"
	shareSubtree = "d"
)

// Share is what a verified link authorises: whose files, which path, and
// whether it reaches inside that path.
//
// The root travels with the owner because a page rendered from a link needs it:
// a trail of breadcrumbs that climbed above the share would offer a door the
// link does not open, and one that stopped at the current folder would leave a
// visitor two levels down with no way back to what they were sent.
type Share struct {
	Owner   string
	Root    string
	Subtree bool
}

// Shares issues and verifies those links.
type Shares struct {
	key []byte
}

// NewShares derives the signing key from the credentials, the way NewSessions
// does and with a context string of its own -- so a session cookie can never be
// read as a link, nor a link as a cookie, even though one password is behind
// both.
func NewShares(c Credentials) *Shares {
	mac := hmac.New(sha256.New, []byte(c.Password))
	mac.Write([]byte("stratus share link v1\x00" + c.Username))
	return &Shares{key: mac.Sum(nil)}
}

// Issue returns the token for a link to path, owned by owner.
//
// subtree says the link covers what is under path rather than path alone, which
// is what a folder wants and what a file must not have. A zero expires is a
// link with no expiry: it lasts until the key changes, which is what the issue
// asked for and what makes a link somebody keeps usable.
func (s *Shares) Issue(owner, path string, subtree bool, expires time.Time) string {
	shape := shareFile
	if subtree {
		shape = shareSubtree
	}
	var deadline int64
	if !expires.IsZero() {
		deadline = expires.Unix()
	}

	payload := strings.Join([]string{
		shareVersion,
		base64.RawURLEncoding.EncodeToString([]byte(owner)),
		base64.RawURLEncoding.EncodeToString([]byte(path)),
		shape,
		strconv.FormatInt(deadline, 10),
	}, ".")
	return payload + "." + base64.RawURLEncoding.EncodeToString(s.sign(payload))
}

// Verify returns what the link authorises, or ErrShareInvalid.
//
// path is the one the request asked for and not the one the token names: a
// token is a claim about what may be read, and the question here is whether
// this request is inside it.
//
// The shape is parsed first and believed second. Nothing the token says -- not
// the owner, not the path, not the expiry -- is acted on until the signature
// over all of it has been checked.
func (s *Shares) Verify(token, path string, now time.Time) (Share, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 6 || parts[0] != shareVersion {
		return Share{}, ErrShareInvalid
	}
	owner, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Share{}, ErrShareInvalid
	}
	root, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Share{}, ErrShareInvalid
	}
	deadline, err := strconv.ParseInt(parts[4], 10, 64)
	if err != nil {
		return Share{}, ErrShareInvalid
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[5])
	if err != nil {
		return Share{}, ErrShareInvalid
	}

	if !hmac.Equal(sig, s.sign(strings.Join(parts[:5], "."))) {
		return Share{}, ErrShareInvalid
	}
	if deadline != 0 && !now.Before(time.Unix(deadline, 0)) {
		return Share{}, ErrShareInvalid
	}
	if !covers(string(root), parts[3], path) {
		return Share{}, ErrShareInvalid
	}
	return Share{Owner: string(owner), Root: string(root), Subtree: parts[3] == shareSubtree}, nil
}

// covers reports whether a link over shared reaches path.
//
// The separator is the point. A subtree link over "album" reaches "album/one"
// and must not reach "album2", which a plain prefix test would hand over -- and
// that is the mistake that turns one shared folder into the whole library.
func covers(shared, shape, path string) bool {
	if path == shared {
		return true
	}
	return shape == shareSubtree && strings.HasPrefix(path, shared+"/")
}

func (s *Shares) sign(payload string) []byte {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(payload))
	return mac.Sum(nil)
}
