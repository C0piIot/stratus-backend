package auth_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/auth"
)

// A fixed instant, so an expiry is arithmetic rather than a race with the clock.
var now = time.Date(2026, time.September, 11, 12, 0, 0, 0, time.UTC)

func sessions(t *testing.T) *auth.Sessions {
	t.Helper()
	return auth.NewSessions(credentials(t), auth.DefaultSessionTTL)
}

func TestSessionRoundTrip(t *testing.T) {
	t.Parallel()
	s := sessions(t)

	value, expires := s.Issue(username, now)
	if want := now.Add(auth.DefaultSessionTTL); !expires.Equal(want) {
		t.Errorf("expires = %v, want %v", expires, want)
	}

	got, err := s.Verify(value, now)
	if err != nil {
		t.Fatalf("Verify = %v", err)
	}
	if got != username {
		t.Errorf("Verify = %q, want %q", got, username)
	}

	// A second before it runs out, and the moment it does. The boundary is
	// closed against the cookie: at the expiry it is already gone.
	if _, err := s.Verify(value, expires.Add(-time.Second)); err != nil {
		t.Errorf("Verify a second early = %v, want nil", err)
	}
	if _, err := s.Verify(value, expires); !errors.Is(err, auth.ErrSessionInvalid) {
		t.Errorf("Verify at the expiry = %v, want ErrSessionInvalid", err)
	}
}

// TestSessionIsNotACredential: the password is what the key is made of, so
// changing it -- or the username -- is what revokes a session that is already
// out there. It is the whole reason the derivation is not a random key.
func TestSessionDiesWithTheCredentials(t *testing.T) {
	t.Parallel()
	value, _ := sessions(t).Issue(username, now)

	tests := []struct {
		name  string
		creds auth.Credentials
	}{
		{
			name:  "the password changed",
			creds: auth.Credentials{Username: username, Password: "example a different one"},
		},
		{
			name:  "the user was renamed",
			creds: auth.Credentials{Username: "someone", Password: examplePassword},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			after := auth.NewSessions(tt.creds, auth.DefaultSessionTTL)
			if _, err := after.Verify(value, now); !errors.Is(err, auth.ErrSessionInvalid) {
				t.Errorf("Verify = %v, want ErrSessionInvalid", err)
			}
		})
	}
}

// TestSessionRefuses walks every way a value can be wrong. They all answer the
// same error, and the point of the table is that every one of them answers at
// all rather than returning a username nobody signed.
func TestSessionRefuses(t *testing.T) {
	t.Parallel()
	s := sessions(t)
	valid, _ := s.Issue(username, now)
	parts := strings.Split(valid, ".")

	// Signed by this key, so it is only the name inside that was swapped: the
	// signature covers the payload, which is what makes this fail.
	forged := strings.Join([]string{parts[0], "cm9vdA", parts[2], parts[3]}, ".")

	tests := []struct {
		name  string
		value string
	}{
		{name: "nothing at all"},
		{name: "not a session", value: "hello"},
		{name: "too few fields", value: strings.Join(parts[:3], ".")},
		{name: "too many fields", value: valid + ".extra"},
		{name: "another format version", value: "s2." + strings.Join(parts[1:], ".")},
		{name: "a name that is not base64", value: strings.Join([]string{parts[0], "not base64!", parts[2], parts[3]}, ".")},
		{name: "an expiry that is not a number", value: strings.Join([]string{parts[0], parts[1], "soon", parts[3]}, ".")},
		{name: "a signature that is not base64", value: strings.Join(append(parts[:3:3], "not base64!"), ".")},
		{name: "a signature that is somebody else's", value: strings.Join(append(parts[:3:3], "AAAA"), ".")},
		{name: "a name swapped under a real signature", value: forged},
		{name: "an expiry pushed out under a real signature", value: strings.Join(
			[]string{parts[0], parts[1], "99999999999", parts[3]}, ".")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := s.Verify(tt.value, now)
			if !errors.Is(err, auth.ErrSessionInvalid) {
				t.Errorf("Verify = %v, want ErrSessionInvalid", err)
			}
			if got != "" {
				t.Errorf("Verify returned %q for a value it refused", got)
			}
		})
	}
}

// TestSessionCarriesTheName rather than assuming the configured one: a second
// user is the point of keeping an owner on every record, and a session that
// hardcoded the answer would be the first thing to rewrite.
func TestSessionCarriesTheName(t *testing.T) {
	t.Parallel()
	s := sessions(t)

	// Dots and slashes, because the value is split on dots and travels in a
	// cookie: a name that could break either has to survive the round trip.
	const odd = "a.name/with=awkward characters"
	value, _ := s.Issue(odd, now)

	got, err := s.Verify(value, now)
	if err != nil {
		t.Fatalf("Verify = %v", err)
	}
	if got != odd {
		t.Errorf("Verify = %q, want %q", got, odd)
	}
}
