package db

import (
	"context"
	"time"
)

// Capture is what a camera made and what the indexer read from it, listed by
// SortAt: when the camera says it was taken, or when the file arrived for one
// that says nothing. Every file of its kind counts, screenshots included.
//
// A photograph and a recording are both captures and are told apart by Kind
// alone, which is why one port serves the gallery and the video library (#215,
// #292).
type Capture struct {
	Track
	SortAt time.Time
}

// CaptureCursor is where a page of the timeline resumes: after the capture
// with this time and this file id, newest first. The zero value is the start.
type CaptureCursor struct {
	At     time.Time
	FileID int64
}

// AtStart reports whether the cursor names nothing.
func (c CaptureCursor) AtStart() bool { return c.FileID == 0 }

// Cursor is the cursor that resumes after p.
func (c Capture) Cursor() CaptureCursor { return CaptureCursor{At: c.SortAt, FileID: c.File.ID} }

// CaptureFilter is one page of the timeline. From and To bound it to
// [From, To) when set, which is how a month is asked for.
//
// Kind is which library is being read -- KindImage for the gallery, KindVideo
// for the videos (#215) -- and it is required: a timeline over both at once is
// not a thing either surface asks for, and leaving it empty would quietly
// return nothing rather than everything.
type CaptureFilter struct {
	Kind     Kind
	After    CaptureCursor
	From, To time.Time
	Limit    int
}

// Month is one month that has something in it. Months are the camera's: the
// EXIF date carries no zone and is stored as read, so a month is taken in UTC
// and a photograph from New Year's Eve stays in December.
type Month struct {
	Year  int
	Month time.Month
}

// MonthOf is the month t falls in, on that clock.
func MonthOf(t time.Time) Month {
	t = t.UTC()
	return Month{Year: t.Year(), Month: t.Month()}
}

// Start is the first instant of the month, and End the first of the next:
// together the bounds CaptureFilter takes.
func (m Month) Start() time.Time { return time.Date(m.Year, m.Month, 1, 0, 0, 0, 0, time.UTC) }

// End is the first instant after the month.
func (m Month) End() time.Time { return m.Start().AddDate(0, 1, 0) }

// Around is one capture with the ones either side of it in the timeline.
// Newer is the one shown before it and Older the one after; either is nil at
// an end.
type Around struct {
	Capture      Capture
	Newer, Older *Capture
}

// Captures is the repository the gallery and the video library read.
//
// One port for both, and a kind on every call, because the ordering column and
// the month seek are identical for the two: a second set of queries would be a
// second thing to keep in step across three drivers.
//
// Like Music it joins files for the owner, since media carries none. The index
// is on media, so for one owner among many it would read the others' captures
// to skip them; there is one owner, and the day there are more is the day that
// index grows an owner.
type Captures interface {
	// Timeline lists one kind newest first, a keyset page at a time.
	Timeline(ctx context.Context, owner string, f CaptureFilter) ([]Capture, error)

	// Around returns one file and its neighbours of the same kind, or
	// ErrNotFound for a file that is not one of them, or not the owner's.
	Around(ctx context.Context, owner string, kind Kind, fileID int64) (Around, error)

	// Months lists the months that have that kind in them, newest first.
	Months(ctx context.Context, owner string, kind Kind) ([]Month, error)
}
