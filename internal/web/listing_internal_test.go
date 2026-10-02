package web

import (
	"net/url"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// A cursor is written into a URL and read back out of one, so what matters is
// that the round trip is exact: a size or a time that came back rounded would
// resume a listing a row early or a row late.
func TestACursorSurvivesAURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		key  db.FileSortKey
		of   db.File
		want string
	}{
		{
			name: "a file by name",
			of:   db.File{Path: "holiday/notes.txt"},
			want: "f/holiday/notes.txt",
		},
		{
			name: "a folder by name",
			of:   db.File{Path: "holiday", IsDir: true},
			want: "d/holiday",
		},
		{
			name: "by size",
			key:  db.SortSize,
			of:   db.File{Path: "holiday/film.mp4", Size: 8_589_934_592},
			want: "f/8589934592/holiday/film.mp4",
		},
		{
			name: "by when it changed",
			key:  db.SortMTime,
			of:   db.File{Path: "holiday/notes.txt", MTime: time.UnixMilli(1_717_243_200_123).UTC()},
			want: "f/1717243200123/holiday/notes.txt",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := encodeCursor(tt.key, db.After(tt.of))
			if got != tt.want {
				t.Fatalf("encodeCursor = %q, want %q", got, tt.want)
			}
			back, err := parseCursor(tt.key, got)
			if err != nil {
				t.Fatalf("parseCursor(%q): %v", got, err)
			}
			if want := db.After(tt.of); back != want {
				t.Errorf("round trip = %+v, want %+v", back, want)
			}
		})
	}
}

// The cookie is written by the same function that writes a link, so a
// preference read back is the one that was saved.
func TestAnArrangementSurvivesTheCookie(t *testing.T) {
	t.Parallel()

	for _, want := range []listing{
		defaultListing,
		{Key: "size", Desc: true, Rows: 50},
		{Key: "modified", Rows: 500},
	} {
		saved, err := url.ParseQuery(want.query())
		if err != nil {
			t.Fatalf("%+v: %v", want, err)
		}
		got, err := parseListing(saved, defaultListing)
		if err != nil {
			t.Fatalf("%+v: %v", want, err)
		}
		if got != want {
			t.Errorf("round trip = %+v, want %+v", got, want)
		}
	}
}
