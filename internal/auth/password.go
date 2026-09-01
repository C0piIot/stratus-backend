// Package auth holds credential handling: the single user the server is
// configured with, and the per-protocol adapters that check a login against it.
//
// The password is stored in memory as configured, in the clear. That is a
// deliberate trade rather than an oversight: OpenSubsonic's token
// authentication is md5(password + salt), which a server that only holds a hash
// cannot compute. Keeping the plaintext means every protocol behaves the same
// way whatever the deployment looks like, instead of one of them working only
// when the operator happened to configure the password one way.
//
// The exposure this accepts is the process environment, which already carries
// the S3 secret key and the database password. It is not written anywhere.
package auth

import (
	"crypto/sha256"
	"crypto/subtle"
)

// sameSecret compares two secrets without leaking which characters matched, and
// without leaking their lengths either: ConstantTimeCompare returns early when
// the lengths differ, so both sides are hashed to a fixed size first.
func sameSecret(given, configured string) bool {
	a := sha256.Sum256([]byte(given))
	b := sha256.Sum256([]byte(configured))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}
