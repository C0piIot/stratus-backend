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

	m := db.Media{IndexedAt: when, TakenAt: when}.Normalize()

	for _, tc := range []struct {
		name string
		got  time.Time
	}{
		{"IndexedAt", m.IndexedAt},
		{"TakenAt", m.TakenAt},
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

// TestMediaNormalizeKeepsAnUnknownTakenAtZero pins the branch: a zero TakenAt
// means the extractor found no date, and the drivers store that as NULL. A
// normalisation that turned it into year 1 at millisecond precision would still
// be zero today, but only by accident of how Truncate counts.
func TestMediaNormalizeKeepsAnUnknownTakenAtZero(t *testing.T) {
	t.Parallel()

	m := db.Media{IndexedAt: time.Now()}.Normalize()

	if !m.TakenAt.IsZero() {
		t.Errorf("TakenAt = %v, want the zero time", m.TakenAt)
	}
}
