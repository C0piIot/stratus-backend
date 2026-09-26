package db

import (
	"context"
	"time"
)

// Photo is an image and what the indexer read from it, listed by SortAt: when
// the camera says it was taken, or when the file arrived for one that says
// nothing. Every image counts, screenshots included.
type Photo struct {
	Track
	SortAt time.Time
}

// PhotoCursor is where a page of the timeline resumes: after the photo with
// this time and this file id, newest first. The zero value is the start.
type PhotoCursor struct {
	At     time.Time
	FileID int64
}

// AtStart reports whether the cursor names no photo.
func (c PhotoCursor) AtStart() bool { return c.FileID == 0 }

// Cursor is the cursor that resumes after p.
func (p Photo) Cursor() PhotoCursor { return PhotoCursor{At: p.SortAt, FileID: p.File.ID} }

// PhotoFilter is one page of the timeline. From and To bound it to
// [From, To) when set, which is how a month is asked for.
type PhotoFilter struct {
	After    PhotoCursor
	From, To time.Time
	Limit    int
}

// PhotoMonth is one month that has photos in it. Months are the camera's: the
// EXIF date carries no zone and is stored as read, so a month is taken in UTC
// and a photo from New Year's Eve stays in December.
type PhotoMonth struct {
	Year  int
	Month time.Month
}

// MonthOf is the month t falls in, on that clock.
func MonthOf(t time.Time) PhotoMonth {
	t = t.UTC()
	return PhotoMonth{Year: t.Year(), Month: t.Month()}
}

// Start is the first instant of the month, and End the first of the next:
// together the bounds PhotoFilter takes.
func (m PhotoMonth) Start() time.Time { return time.Date(m.Year, m.Month, 1, 0, 0, 0, 0, time.UTC) }

// End is the first instant after the month.
func (m PhotoMonth) End() time.Time { return m.Start().AddDate(0, 1, 0) }

// PhotoAround is one photo with the ones either side of it in the timeline.
// Newer is the one shown before it and Older the one after; either is nil at
// an end.
type PhotoAround struct {
	Photo        Photo
	Newer, Older *Photo
}

// Photos is the repository the gallery reads.
//
// Like Music it joins files for the owner, since media carries none. The index
// is on media, so for one owner among many it would read the others' photos
// to skip them; there is one owner, and the day there are more is the day that
// index grows an owner.
type Photos interface {
	// PhotoTimeline lists photos newest first, a keyset page at a time.
	PhotoTimeline(ctx context.Context, owner string, f PhotoFilter) ([]Photo, error)

	// PhotoAround returns one photo and its neighbours, or ErrNotFound for a
	// file that is not an image the owner has.
	PhotoAround(ctx context.Context, owner string, fileID int64) (PhotoAround, error)

	// PhotoMonths lists the months that have photos, newest first.
	PhotoMonths(ctx context.Context, owner string) ([]PhotoMonth, error)
}
