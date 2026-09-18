package db

import (
	"strings"
	"time"
)

// Kind is what an extractor decided a file is.
type Kind string

// The kinds of file that carry metadata worth indexing.
const (
	KindImage Kind = "image"
	KindAudio Kind = "audio"
	KindVideo Kind = "video"
	// KindOther is a file with nothing to extract. It still gets a row, or the
	// queue would offer it again on every pass.
	KindOther Kind = "other"
)

// Media is what an indexer extracted from a file: one row per file, whatever
// its kind, with the fields the other kinds do not use left at their zero value.
//
// A wide table with unused columns rather than one table per kind, which would
// be three migrations per driver, three sets of hand-written SQL and a union for
// any listing that mixes them. It splits when OpenSubsonic wants real queries
// over artists and albums.
type Media struct {
	// FileID is the file this describes, and the primary key: a file has one
	// set of metadata or none.
	FileID int64
	Kind   Kind

	// IndexedAt and Version say when this was extracted and by which extractor.
	// Raising the version is what puts everything back in the queue when an
	// extractor improves, with no migration and no script.
	IndexedAt time.Time
	Version   int
	// ETag is the file's validator as it was when this was extracted, and it is
	// what makes the queue understand an overwrite. A replaced file keeps its
	// row and therefore its id, so without this the metadata of the bytes that
	// are gone would describe the bytes that are there, for as long as the
	// extractor version did not move.
	ETag string
	// Error is why extraction failed, empty when it did not. A file that cannot
	// be parsed still gets a row, or it would be retried on every pass forever.
	Error string

	// TakenAt is when the camera says the photo was taken, zero when unknown.
	TakenAt time.Time
	// Width, Height and Orientation are pixels and the EXIF rotation.
	Width, Height, Orientation int
	// GPS is nil when unknown, because zero is a real place in the Atlantic.
	GPS *GPS
	// Camera is the make and model as recorded.
	Camera string

	// DurationMS and Codec describe audio and video.
	DurationMS int64
	Codec      string

	// The rest is what a music library needs.
	Artist, Album, Title, Genre string
	TrackNo, DiscNo, Year       int
	// AlbumArtist is what the album is filed under, which is not always the
	// track's artist: without it a compilation breaks into one album per
	// track. It falls back to Artist when the tag is absent, which is the
	// common case for a record by one artist.
	AlbumArtist string
}

// Folded is the lower-cased text a driver stores so that a search can match it.
//
// It exists because the two drivers do not agree on what lower() means: SQLite
// folds ASCII and nothing else, while PostgreSQL folds Unicode, so the same
// search for "BJÖRK" would find "Björk" on one and not the other. Folding here,
// in Go, is what makes the two answer identically -- they compare bytes this
// package produced rather than calling a function of their own.
//
// Three fields and not one concatenation, because a search answers three
// separate questions. An artist must not match because the words were in the
// title of one of its tracks.
type Folded struct {
	// Song is the track's own text: its title and the artist credited on it.
	Song string
	// Album and AlbumArtist are the two names an album is filed under, and the
	// keys the album and artist listings group by.
	Album, AlbumArtist string
}

// Fold returns the folded text for m. Drivers must store exactly this.
func (m Media) Fold() Folded {
	return Folded{
		Song:        fold(m.Title, m.Artist),
		Album:       fold(m.Album),
		AlbumArtist: fold(m.AlbumArtist),
	}
}

// FoldQuery folds a search term the same way, which is the other half of the
// guarantee: both sides of the comparison go through the same function.
func FoldQuery(text string) string { return fold(text) }

func fold(parts ...string) string {
	return strings.ToLower(strings.TrimSpace(strings.Join(parts, " ")))
}

// GPS is where a photo says it was taken.
type GPS struct {
	Latitude, Longitude float64
}

// Indexed reports whether extraction succeeded.
func (m Media) Indexed() bool { return m.Error == "" }

// Normalize returns m with the fields drivers must not store verbatim already
// fixed: UTC times at the agreed precision, the same job File.Normalize does.
//
// A zero TakenAt is left alone rather than truncated. It is not a time: it says
// the extractor found none, and every driver stores that as NULL.
func (m Media) Normalize() Media {
	m.IndexedAt = m.IndexedAt.UTC().Truncate(TimePrecision)
	if !m.TakenAt.IsZero() {
		m.TakenAt = m.TakenAt.UTC().Truncate(TimePrecision)
	}
	return m
}
