package db

import "time"

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
}

// GPS is where a photo says it was taken.
type GPS struct {
	Latitude, Longitude float64
}

// Indexed reports whether extraction succeeded.
func (m Media) Indexed() bool { return m.Error == "" }
