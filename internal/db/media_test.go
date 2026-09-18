package db_test

import (
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
)

func TestMediaNormalize(t *testing.T) {
	t.Parallel()
	zone := time.FixedZone("CEST", 2*60*60)
	when := time.Date(2024, 6, 1, 12, 0, 0, 999_999_999, zone)

	m := db.Media{IndexedAt: when, TakenAt: when, RetryAt: when}.Normalize()

	for _, tc := range []struct {
		name string
		got  time.Time
	}{
		{"IndexedAt", m.IndexedAt},
		{"TakenAt", m.TakenAt},
		{"RetryAt", m.RetryAt},
	} {
		if tc.got.Location() != time.UTC {
			t.Errorf("%s is in %v, want UTC", tc.name, tc.got.Location())
		}
		// Truncated, not rounded: 999999999ns must not become the next second.
		if got := tc.got.Nanosecond(); got != 999_000_000 {
			t.Errorf("%s nanoseconds = %d, want it truncated to milliseconds", tc.name, got)
		}
	}
}

// TestMediaNormalizeKeepsTheUnknownTimesZero pins the branch: a zero TakenAt
// means the extractor found no date, and the drivers store that as NULL. A
// normalisation that turned it into year 1 at millisecond precision would still
// be zero today, but only by accident of how Truncate counts.
func TestMediaNormalizeKeepsTheUnknownTimesZero(t *testing.T) {
	t.Parallel()

	m := db.Media{IndexedAt: time.Now()}.Normalize()

	if !m.TakenAt.IsZero() {
		t.Errorf("TakenAt = %v, want the zero time", m.TakenAt)
	}
	// And the same for RetryAt, where zero is what says the row is the last
	// word on the file rather than a wait.
	if !m.RetryAt.IsZero() {
		t.Errorf("RetryAt = %v, want the zero time", m.RetryAt)
	}
}

// TestMediaFold is where the guarantee lives, so it is tested where it lives:
// the two drivers store what this returns, and a search compares it to what
// FoldQuery returns. If the two ever stopped agreeing, a search would answer
// differently on SQLite and on PostgreSQL, which is what the whole arrangement
// exists to prevent.
func TestMediaFold(t *testing.T) {
	t.Parallel()

	got := db.Media{
		Title:       "JÓGA",
		Artist:      "Björk",
		Album:       "HOMOGENIC",
		AlbumArtist: "BJÖRK",
	}.Fold()

	// Case-folded past ASCII, which is the whole point: neither engine's own
	// lower() would agree with the other about Ó.
	if want := "jóga björk"; got.Song != want {
		t.Errorf("Song = %q, want %q", got.Song, want)
	}
	if want := "homogenic"; got.Album != want {
		t.Errorf("Album = %q, want %q", got.Album, want)
	}
	// Separate fields and not one string: an artist must not match because the
	// term appeared in the title of one of its tracks.
	if want := "björk"; got.AlbumArtist != want {
		t.Errorf("AlbumArtist = %q, want %q", got.AlbumArtist, want)
	}

	// A missing tag leaves no stray space to match on.
	if bare := (db.Media{Title: "Untagged"}).Fold(); bare.Song != "untagged" || bare.Album != "" {
		t.Errorf("a row with only a title folded to %+v", bare)
	}
	// And both sides of the comparison go through the same folding.
	if got := db.FoldQuery("  BJÖRK  "); got != "björk" {
		t.Errorf("FoldQuery = %q", got)
	}
}
