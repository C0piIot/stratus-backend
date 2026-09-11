package dav

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/emersion/go-webdav"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/storage"
)

// hasStatus reports whether err is the HTTP error go-webdav builds for code.
//
// By its rendering rather than its type: the library keeps that type in an
// internal package and exposes no accessor for the status. It does write the
// status first, and the prefix expected here is produced by the library itself
// -- so a change to that format moves both sides of the comparison together.
func hasStatus(err error, code int) bool {
	return err != nil && strings.HasPrefix(err.Error(), webdav.NewHTTPError(code, nil).Error())
}

// Two pure functions, tested where they live rather than through a request.
//
// It is the honest way round for both. checkConditions has a dozen
// combinations of two headers and a file that may or may not exist, and
// reaching each through a PUT would be a dozen fixtures saying the same thing.
// mapErr is a translation table whose job is to be complete -- two of its arms
// cannot arrive through this surface at all, because toPath rejects an invalid
// path before internal/files ever sees one and http.ServeContent answers a bad
// range itself. Deleting those arms would leave the table incomplete the day a
// feature does return one; asserting them here says what the mapping is.

func TestCheckConditions(t *testing.T) {
	t.Parallel()

	// The stored ETag carries no quotes and the header does: quoting is HTTP
	// framing, which internal/files deliberately leaves to whoever speaks HTTP.
	// checkConditions is where the two meet, so a case that quoted both sides
	// would agree with itself and prove nothing.
	const stored = "abc123"
	const header = `"abc123"`
	existing := db.File{ETag: stored}

	tests := []struct {
		name        string
		ifMatch     webdav.ConditionalMatch
		ifNoneMatch webdav.ConditionalMatch
		exists      bool
		want        int // 0 for "allowed"
	}{
		{name: "no conditions at all", exists: true},

		// If-None-Match is "only if it is not there", which a client uses to
		// create without racing somebody else.
		{name: "* over something that exists", ifNoneMatch: "*", exists: true, want: http.StatusPreconditionFailed},
		{name: "* over nothing", ifNoneMatch: "*"},
		{
			name: "an etag that matches", ifNoneMatch: header, exists: true,
			want: http.StatusPreconditionFailed,
		},
		// It does not match, so the write is somebody else's to make.
		{name: "an etag that does not match", ifNoneMatch: `"other"`, exists: true},
		// Absent, so there is nothing to compare and nothing to refuse.
		{name: "an etag over nothing", ifNoneMatch: header},
		{
			name: "a malformed etag", ifNoneMatch: "not-quoted", exists: true,
			want: http.StatusBadRequest,
		},

		// If-Match is the other half: "only if it is still what I read".
		{name: "if-match over nothing", ifMatch: header, want: http.StatusPreconditionFailed},
		{name: "if-match *, which exists", ifMatch: "*", exists: true},
		{name: "if-match * over nothing", ifMatch: "*", want: http.StatusPreconditionFailed},
		{name: "if-match that matches", ifMatch: header, exists: true},
		{
			name: "if-match that is stale", ifMatch: `"older"`, exists: true,
			want: http.StatusPreconditionFailed,
		},
		{
			name: "a malformed if-match", ifMatch: "not-quoted", exists: true,
			want: http.StatusBadRequest,
		},

		// Both at once, which a cautious client sends.
		{
			name: "both, and the none-match refuses first", ifMatch: header, ifNoneMatch: "*", exists: true,
			want: http.StatusPreconditionFailed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := checkConditions(tt.ifMatch, tt.ifNoneMatch, existing, tt.exists)

			if tt.want == 0 {
				if err != nil {
					t.Fatalf("checkConditions = %v, want it allowed", err)
				}
				return
			}
			if !hasStatus(err, tt.want) {
				t.Errorf("checkConditions = %v, want %d", err, tt.want)
			}
		})
	}
}

func TestMapErr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want int // 0 for "not an HTTP error"
	}{
		{name: "nothing went wrong"},
		{name: "a missing row", err: db.ErrNotFound, want: http.StatusNotFound},
		{name: "a missing object", err: storage.ErrNotFound, want: http.StatusNotFound},
		// 409 rather than 412: the request is inconsistent with the tree, which
		// is what a client fixes by creating the parent first.
		{name: "something already there", err: db.ErrConflict, want: http.StatusConflict},
		{name: "a path the tree refuses", err: db.ErrInvalidPath, want: http.StatusBadRequest},
		{name: "a key the store refuses", err: storage.ErrInvalidKey, want: http.StatusBadRequest},
		{name: "a range that cannot be served", err: storage.ErrInvalidRange, want: http.StatusRequestedRangeNotSatisfiable},
		// Wrapped, because that is how they arrive: every layer adds context.
		{
			name: "wrapped in whatever the caller added",
			err:  fmt.Errorf("put %q: %w", "notes.txt", db.ErrConflict),
			want: http.StatusConflict,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := mapErr(tt.err)

			if tt.err == nil {
				if got != nil {
					t.Fatalf("mapErr(nil) = %v", got)
				}
				return
			}
			if !hasStatus(got, tt.want) {
				t.Errorf("mapErr = %v, want %d", got, tt.want)
			}
			// The cause survives, which is what a log line is read for.
			if !errors.Is(got, tt.err) {
				t.Errorf("mapErr lost the cause: %v", got)
			}
		})
	}
}

// TestMapErrKeepsWhatItDoesNotKnow: anything that is not one of this project's
// sentinels is a bug rather than a client's mistake, so it becomes a 500 by
// being left alone rather than being guessed at.
func TestMapErrKeepsWhatItDoesNotKnow(t *testing.T) {
	t.Parallel()

	cause := errors.New("the database is on fire")
	got := mapErr(cause)

	// Not turned into any status: the library's handler makes an unclassified
	// error a 500, which is the right answer for a bug in this server.
	for _, code := range []int{http.StatusNotFound, http.StatusConflict, http.StatusBadRequest} {
		if hasStatus(got, code) {
			t.Errorf("mapErr turned an unknown failure into %d", code)
		}
	}
	if !strings.HasPrefix(got.Error(), "dav: ") {
		t.Errorf("mapErr = %q, want it wrapped and passed on", got)
	}
	if !errors.Is(got, cause) {
		t.Errorf("mapErr lost the cause: %v", got)
	}
}
