package auth

import (
	"context"
	"errors"
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
	Password string
}

// Configured reports whether there is a user to authenticate against at all.
func (c Credentials) Configured() bool { return c.Username != "" && c.Password != "" }

// Validate refuses a configuration that could never authenticate anybody, so
// that half-set credentials stop the process rather than surfacing as a failed
// login weeks later.
func (c Credentials) Validate() error {
	switch {
	case c.Username == "" && c.Password == "":
		// Nothing configured is legal while no protocol needs a login.
		return nil
	case c.Username == "":
		return errors.New("a password is set but a username is not")
	case c.Password == "":
		return errors.New("a username is set but a password is not")
	}
	return nil
}

// Verify implements Verifier. It returns ErrUnauthorized for every way of
// getting the credentials wrong.
//
// Both halves are compared before either is judged. An early return on the
// username would make a wrong one measurably faster than a wrong password and
// turn the response time into an oracle for which usernames exist.
func (c Credentials) Verify(_ context.Context, username, password string) error {
	if !c.Configured() {
		return ErrUnauthorized
	}

	sameUser := sameSecret(username, c.Username)
	samePassword := sameSecret(password, c.Password)

	if !sameUser || !samePassword {
		return ErrUnauthorized
	}
	return nil
}
