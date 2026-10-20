package db

import (
	"context"
	"strings"
	"time"
)

// Calendars over CalDAV (#75). A calendar is a row and so is every object in
// it: the iCalendar text lives in the database rather than in the blob store,
// because it is a few kilobytes, it is what a time-range query has to read
// whole, and nothing else about it is a file.
//
// Every write moves a counter on the calendar and stamps the object with it,
// and every delete leaves a tombstone stamped the same way. That is what
// sync-collection (RFC 6578) answers from: "what changed since token n" is the
// objects and the tombstones above n, and a token is only that number.

// Calendar is one calendar collection.
type Calendar struct {
	ID      int64
	OwnerID string
	// Name is the calendar's segment in the URL, which a client never shows;
	// DisplayName is what it does show.
	Name        string
	DisplayName string
	Description string
	// Color is what a client paints the calendar with, #RRGGBB or #RRGGBBAA,
	// and empty for none chosen.
	Color string
	// Components is what the calendar holds, comma-separated -- "VTODO" for
	// a task list, "VEVENT,VTODO,VJOURNAL" for one that takes everything. A
	// string rather than a slice so that a Calendar stays comparable.
	Components string
	// SyncToken is the counter every write to the calendar moves.
	SyncToken int64
	// PrunedToken is the highest token whose tombstones have been forgotten.
	// A client asking for changes since a token below it cannot be told what
	// was deleted in between, and has to list the calendar again.
	PrunedToken int64
	Created     time.Time
}

// Holds reports whether the calendar takes a component of that name.
func (c Calendar) Holds(component string) bool {
	for _, held := range strings.Split(c.Components, ",") {
		if held == component {
			return true
		}
	}
	return false
}

// Normalize is Calendar the way every driver stores it: its time in UTC and to
// the millisecond.
func (c Calendar) Normalize() Calendar {
	c.Created = c.Created.UTC().Truncate(time.Millisecond)
	return c
}

// CalendarObject is one resource in a calendar: an event with every override
// of its recurrence, which is what one UID is.
type CalendarObject struct {
	CalendarID int64
	// Name is the object's segment in the URL, chosen by the client.
	Name string
	UID  string
	// Component is the iCalendar component it holds, VEVENT for one.
	Component string
	// Data is the iCalendar text as stored, which is what an ETag summarises.
	Data      string
	ETag      string
	SyncToken int64
	Changed   time.Time
}

// Normalize is CalendarObject the way every driver stores it: its time in UTC
// and to the millisecond.
func (o CalendarObject) Normalize() CalendarObject {
	o.Changed = o.Changed.UTC().Truncate(time.Millisecond)
	return o
}

// CalendarChange is one name that changed after a token: written, or deleted
// when Deleted says so.
type CalendarChange struct {
	Name      string
	SyncToken int64
	Deleted   bool
}

// Calendars is the repository for calendars. Writes to an object are several
// statements -- the counter, the row, the tombstone -- and have to run inside
// Tx; reads need not.
//
// The object calls take a calendar id and no owner: an id is only ever had by
// reading the calendar by its owner first, which is where somebody else's
// calendar is not found.
type Calendars interface {
	// CreateCalendar stores c and returns it with its id, or ErrConflict when
	// the owner already has a calendar by that name.
	CreateCalendar(ctx context.Context, c Calendar) (Calendar, error)

	// Calendars lists an owner's calendars by name.
	Calendars(ctx context.Context, owner string) ([]Calendar, error)

	// CalendarByName returns one of an owner's calendars, or ErrNotFound.
	CalendarByName(ctx context.Context, owner, name string) (Calendar, error)

	// UpdateCalendar writes c's display name, description and color, found
	// by its owner and name, or answers ErrNotFound. The name is the URL and
	// is not changed by this.
	UpdateCalendar(ctx context.Context, c Calendar) error

	// DeleteCalendar removes one of an owner's calendars with everything in
	// it and every deletion it remembers, or answers ErrNotFound.
	DeleteCalendar(ctx context.Context, owner, name string) error

	// CalendarObjects lists everything in a calendar by name.
	CalendarObjects(ctx context.Context, calendarID int64) ([]CalendarObject, error)

	// CalendarObject returns one object by name, or ErrNotFound.
	CalendarObject(ctx context.Context, calendarID int64, name string) (CalendarObject, error)

	// PutCalendarObject writes o under its name, replacing what was there,
	// and returns it stamped with the calendar's next token. Another object in
	// the calendar with the same UID is ErrConflict, which is CalDAV's
	// no-uid-conflict; a calendar that is not there is ErrNotFound.
	PutCalendarObject(ctx context.Context, o CalendarObject) (CalendarObject, error)

	// DeleteCalendarObject removes one object and leaves a tombstone, or
	// answers ErrNotFound.
	DeleteCalendarObject(ctx context.Context, calendarID int64, name string, at time.Time) error

	// CalendarChanges returns every name written or deleted after the token
	// since, in token order. Whether since is still answerable is the
	// caller's to check against PrunedToken.
	CalendarChanges(ctx context.Context, calendarID, since int64) ([]CalendarChange, error)

	// PruneCalendarTombstones forgets the deletions made before the time
	// given, moves each calendar's PrunedToken past them, and reports how
	// many it forgot.
	PruneCalendarTombstones(ctx context.Context, before time.Time) (int64, error)
}
