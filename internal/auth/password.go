// Package auth holds credential handling: the password hash the server is
// configured with, how `stratus hash-password` produces it, how it is
// recognised as usable at startup, and how a login is checked against it.
//
// The per-protocol adapters live here too. HTTP Basic is the first, because
// WebDAV and CalDAV both speak it; OpenSubsonic's API keys are a different
// shape and arrive with that surface.
package auth

import (
	"errors"
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

// Cost is the bcrypt work factor. bcrypt.DefaultCost is 10, which is the number
// everything else in the ecosystem uses; naming it here makes it a decision
// rather than an omission.
const Cost = bcrypt.DefaultCost

// MaxPasswordLen is bcrypt's own limit. Longer input is silently truncated by
// the algorithm, so a passphrase that differs only after byte 72 would be the
// same password -- worth an error rather than a surprise.
const MaxPasswordLen = 72

// Hash turns a plaintext password into the bcrypt hash that goes in
// STRATUS_PASSWORD_HASH.
func Hash(password string) (string, error) {
	switch {
	case password == "":
		return "", errors.New("password is empty")
	case len(password) > MaxPasswordLen:
		return "", fmt.Errorf("password is %d bytes, over bcrypt's %d byte limit", len(password), MaxPasswordLen)
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), Cost)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	return string(hash), nil
}

// compare is bcrypt's own check, wrapped so that Verify reads as two booleans
// and cannot grow an early return by accident.
func compare(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// ValidateHash reports whether hash is something bcrypt can later verify
// against. It exists so that a truncated or hand-edited hash fails at startup
// instead of at the first login attempt.
func ValidateHash(hash string) error {
	if _, err := bcrypt.Cost([]byte(hash)); err != nil {
		// The hash is not a secret, but it is not useful in a log either.
		return fmt.Errorf("not a bcrypt hash: %w", err)
	}
	return nil
}
