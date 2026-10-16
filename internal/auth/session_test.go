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

// TestSessionRenews: a session lasts a fortnight from its last use rather
// than from when it was issued, and the use is counted once a day -- a value
// signed this morning is not re-signed by every thumbnail on the page.
func TestSessionRenews(t *testing.T) {
	t.Parallel()
	s := sessions(t)
	value, expires := s.Issue(username, now)

	if _, _, ok := s.Renew(value, now.Add(23*time.Hour)); ok {
		t.Error("Renew after 23 hours re-signed a value that was good for thirteen more days")
	}
	later := now.Add(25 * time.Hour)
	renewed, until, ok := s.Renew(value, later)
	if !ok {
		t.Fatal("Renew after 25 hours = not due")
	}
	if want := later.Add(auth.DefaultSessionTTL); !until.Equal(want) {
		t.Errorf("the renewed value expires at %v, want %v", until, want)
	}
	// Past the original expiry, where the old value is refused and the new
	// one is what keeps the browser signed in.
	after := expires.Add(time.Hour)
	if _, err := s.Verify(value, after); !errors.Is(err, auth.ErrSessionInvalid) {
		t.Errorf("the original value after its expiry = %v, want ErrSessionInvalid", err)
	}
	got, err := s.Verify(renewed, after)
	if err != nil || got != username {
		t.Errorf("the renewed value after the original expiry = %q, %v; want %q", got, err, username)
	}

	// Nothing to renew: a value that is not good is not made good.
	if _, _, ok := s.Renew(value, after); ok {
		t.Error("Renew revived an expired value")
	}
	if _, _, ok := s.Renew("hello", now); ok {
		t.Error("Renew signed something that was never a session")
	}
}

// TestSessionSlidesWithoutACap, by decision (#334): a session in daily use
// is never signed out. What ends it is the password changing, which
// TestSessionDiesWithTheCredentials covers for a fresh value and this one
// covers for a value that has been renewed for a year.
func TestSessionSlidesWithoutACap(t *testing.T) {
	t.Parallel()
	s := sessions(t)
	value, _ := s.Issue(username, now)

	at := now
	for day := 1; day <= 365; day++ {
		at = now.Add(time.Duration(day) * 24 * time.Hour)
		renewed, _, ok := s.Renew(value, at)
		if !ok {
			t.Fatalf("day %d: not renewed", day)
		}
		value = renewed
	}
	if _, err := s.Verify(value, at.Add(13*24*time.Hour)); err != nil {
		t.Errorf("after a year of daily use, Verify = %v", err)
	}

	changed := auth.NewSessions(auth.Credentials{Username: username, Password: "example a different one"}, auth.DefaultSessionTTL)
	if _, err := changed.Verify(value, at); !errors.Is(err, auth.ErrSessionInvalid) {
		t.Errorf("after the password changed, Verify = %v, want ErrSessionInvalid", err)
	}
	if _, _, ok := changed.Renew(value, at); ok {
		t.Error("Renew under the new password re-signed a value the old one issued")
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
