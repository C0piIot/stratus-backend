package auth_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/auth"
)

func shares(t *testing.T) *auth.Shares {
	t.Helper()
	return auth.NewShares(auth.Credentials{Username: "edu", Password: "an example password"})
}

// TestShareRoundTrip is the ordinary case, both shapes.
func TestShareRoundTrip(t *testing.T) {
	t.Parallel()
	s := shares(t)
	now := time.Now()

	file := s.Issue("edu", "photos/IMG_0001.jpg", false, now.Add(time.Hour))
	got, err := s.Verify(file, "photos/IMG_0001.jpg", now)
	if err != nil || got.Owner != "edu" || got.Root != "photos/IMG_0001.jpg" || got.Subtree {
		t.Errorf("a file link = %+v, %v", got, err)
	}

	folder := s.Issue("edu", "photos", true, now.Add(time.Hour))
	// The root comes back with the owner because a page rendered from a link
	// needs to know where the link starts.
	got, err = s.Verify(folder, "photos/IMG_0001.jpg", now)
	if err != nil || got.Owner != "edu" || got.Root != "photos" || !got.Subtree {
		t.Errorf("a folder link over a file inside it = %+v, %v", got, err)
	}
}

// TestShareCoversItsSubtreeAndNoMore is the one that leaks the library if it is
// wrong, so every way of being just outside is here.
func TestShareCoversItsSubtreeAndNoMore(t *testing.T) {
	t.Parallel()
	s := shares(t)
	now := time.Now()
	folder := s.Issue("edu", "album", true, time.Time{})

	for _, path := range []string{"album", "album/one.jpg", "album/raw/two.dng", "album/a/b/c/d.jpg"} {
		if _, err := s.Verify(folder, path, now); err != nil {
			t.Errorf("a link over album does not reach %q: %v", path, err)
		}
	}
	for _, path := range []string{
		"",          // the root, which is everything
		"other.jpg", // a sibling
		"album2",    // the mistake a plain prefix test makes
		"album2/one.jpg",
		"albumsomething/deep.jpg",
	} {
		if _, err := s.Verify(folder, path, now); !errors.Is(err, auth.ErrShareInvalid) {
			t.Errorf("a link over album reached %q", path)
		}
	}

	// And a file link is one file, not a prefix of the tree.
	file := s.Issue("edu", "album", false, time.Time{})
	if _, err := s.Verify(file, "album/one.jpg", now); !errors.Is(err, auth.ErrShareInvalid) {
		t.Error("a file link reached inside the path it names")
	}
}

// TestShareExpiry: a deadline is honoured, and none means none.
func TestShareExpiry(t *testing.T) {
	t.Parallel()
	s := shares(t)
	now := time.Now()

	soon := s.Issue("edu", "notes.txt", false, now.Add(time.Minute))
	if _, err := s.Verify(soon, "notes.txt", now.Add(time.Hour)); !errors.Is(err, auth.ErrShareInvalid) {
		t.Error("an expired link still opens")
	}
	if _, err := s.Verify(soon, "notes.txt", now); err != nil {
		t.Errorf("a link inside its own life = %v", err)
	}

	// No expiry is the shape the issue asked for: it lasts until the key
	// changes, which is the only revocation a stateless link has.
	forever := s.Issue("edu", "notes.txt", false, time.Time{})
	if _, err := s.Verify(forever, "notes.txt", now.AddDate(10, 0, 0)); err != nil {
		t.Errorf("a link with no expiry expired anyway: %v", err)
	}
}

// TestShareRefusesWhatItDidNotSign covers the ways a token arrives wrong, and
// the one that matters most: another password signed it.
func TestShareRefusesWhatItDidNotSign(t *testing.T) {
	t.Parallel()
	s := shares(t)
	now := time.Now()
	token := s.Issue("edu", "album", true, time.Time{})

	other := auth.NewShares(auth.Credentials{Username: "edu", Password: "a different password"})
	if _, err := other.Verify(token, "album", now); !errors.Is(err, auth.ErrShareInvalid) {
		t.Error("a link signed by one password opened under another")
	}
	renamed := auth.NewShares(auth.Credentials{Username: "someone", Password: "an example password"})
	if _, err := renamed.Verify(token, "album", now); !errors.Is(err, auth.ErrShareInvalid) {
		t.Error("renaming the user left the links open")
	}

	// A cookie is not a link, and the derivation is what says so.
	cookie, _ := auth.NewSessions(auth.Credentials{Username: "edu", Password: "an example password"},
		auth.DefaultSessionTTL).Issue("edu", now)
	if _, err := s.Verify(cookie, "album", now); !errors.Is(err, auth.ErrShareInvalid) {
		t.Error("a session cookie was accepted as a share link")
	}

	parts := strings.Split(token, ".")
	broken := map[string]string{
		"nothing at all":   "",
		"not a token":      "hello",
		"a newer shape":    "k2." + strings.Join(parts[1:], "."),
		"a turned shape":   strings.Join([]string{parts[0], parts[1], parts[2], "f", parts[4], parts[5]}, "."),
		"another path":     strings.Join([]string{parts[0], parts[1], "b3RoZXI", parts[3], parts[4], parts[5]}, "."),
		"another owner":    strings.Join([]string{parts[0], "c29tZWJvZHk", parts[2], parts[3], parts[4], parts[5]}, "."),
		"a later deadline": strings.Join([]string{parts[0], parts[1], parts[2], parts[3], "99999999999", parts[5]}, "."),
		"a bad signature":  strings.Join(parts[:5], ".") + ".bm90YXNpZ25hdHVyZQ",
		"unreadable owner": strings.Join([]string{parts[0], "!!!", parts[2], parts[3], parts[4], parts[5]}, "."),
		"unreadable path":  strings.Join([]string{parts[0], parts[1], "!!!", parts[3], parts[4], parts[5]}, "."),
		"unreadable time":  strings.Join([]string{parts[0], parts[1], parts[2], parts[3], "soon", parts[5]}, "."),
		"unreadable sig":   strings.Join(parts[:5], ".") + ".!!!",
	}
	for name, token := range broken {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := s.Verify(token, "album", now); !errors.Is(err, auth.ErrShareInvalid) {
				t.Errorf("%s was accepted", name)
			}
		})
	}
}
