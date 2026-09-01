package auth_test

import (
	"testing"
	"time"
)

// TestWrongUsernameCostsTheSame is the oracle guard: a wrong username must not
// be measurably faster than a wrong password, or the response time tells an
// attacker which usernames exist.
func TestWrongUsernameCostsTheSame(t *testing.T) {
	creds := credentials(t)

	measure := func(user, pass string) time.Duration {
		start := time.Now()
		_ = creds.Verify(user, pass)
		return time.Since(start)
	}
	badPassword := measure(username, "not it")
	badUsername := measure("someone", examplePassword)

	// Generous on purpose: bcrypt is ~65ms and the gap this catches is the
	// whole of it, so anything above a quarter is a real early return.
	if badUsername < badPassword/4 {
		t.Errorf("a wrong username took %v against %v for a wrong password: the check returns early", badUsername, badPassword)
	}
}
