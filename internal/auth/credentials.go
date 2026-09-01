package auth

import (
	"crypto/subtle"
	"errors"
	"fmt"
)

// ErrUnauthorized is what every failed verification returns, whatever went
// wrong. Telling a caller that the username was right but the password was not
// hands an attacker half the answer.
var ErrUnauthorized = errors.New("auth: unauthorized")

// Credentials is the single user Stratus is configured with. Sharing will make
// this a lookup; today it is one pair, and the protocol adapters do not need to
// know which it is.
type Credentials struct {
	Username string
	// Hash is a bcrypt hash, as produced by Hash and stored in
	// STRATUS_PASSWORD_HASH.
	Hash string
}

// Configured reports whether there is a user to authenticate against at all.
func (c Credentials) Configured() bool { return c.Username != "" && c.Hash != "" }

// Validate refuses a configuration that could never authenticate anybody. It is
// called at startup so a hash pasted with a byte missing stops the process
// rather than surfacing as a failed login weeks later.
func (c Credentials) Validate() error {
	switch {
	case c.Username == "" && c.Hash == "":
		// Nothing configured is legal while no protocol needs a login.
		return nil
	case c.Username == "":
		return errors.New("a password hash is set but a username is not")
	case c.Hash == "":
		return errors.New("a username is set but a password hash is not")
	}
	if err := ValidateHash(c.Hash); err != nil {
		return fmt.Errorf("the password hash is unusable: %w", err)
	}
	return nil
}

// Verify checks a username and password, and returns ErrUnauthorized for every
// way of getting them wrong.
//
// There is no early return when the username does not match, on purpose. bcrypt
// is deliberately slow, so skipping it would make a wrong username measurably
// faster than a wrong password and turn the response time into an oracle for
// which usernames exist. Both halves are computed, then combined.
func (c Credentials) Verify(username, password string) error {
	if !c.Configured() {
		return ErrUnauthorized
	}

	// ConstantTimeCompare still returns early on a length mismatch, so the
	// length of the configured username is not hidden. Everything else is.
	sameUser := subtle.ConstantTimeCompare([]byte(username), []byte(c.Username)) == 1
	samePassword := compare(c.Hash, password)

	if !sameUser || !samePassword {
		return ErrUnauthorized
	}
	return nil
}
