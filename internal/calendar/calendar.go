// Package calendar owns what a calendar object is (#75): which iCalendar a
// collection accepts, the one form it is stored in, and the write that checks a
// client's precondition and stores in one transaction.
//
// It exists for the reason internal/music does. A conditional PUT is a read and
// a write -- the ETag as it is, then the new object -- and a client that said
// If-Match has to lose to another client that wrote in between, which only a
// transaction can promise. An inbound adapter has none and must not grow one.
package calendar

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/emersion/go-ical"

	"github.com/C0piIot/stratus-backend/internal/db"
)

var (
	// ErrInvalid is iCalendar a calendar collection cannot hold: no UID, two
	// of them, a METHOD, or text that does not encode.
	ErrInvalid = errors.New("calendar: not a calendar object")
	// ErrUnsupported is a component this server does not store. Events only,
	// for now: a time-range query cannot filter tasks yet (#79), and a task
	// list that silently returned nothing would be worse than one refused.
	ErrUnsupported = errors.New("calendar: only events are stored")
	// ErrStaleToken is a sync token this server cannot answer from: older than
	// the deletions it still remembers, or newer than anything it wrote. The
	// client's answer to it is to list the calendar again.
	ErrStaleToken = errors.New("calendar: sync token no longer answerable")
)

// TombstoneRetention is how long a deletion is remembered for a client that
// syncs by token. A client away for longer is told its token is stale and
// lists the calendar again, which is correct and only costs a listing.
const TombstoneRetention = 30 * 24 * time.Hour

// Component is the one kind of calendar object stored.
const Component = ical.CompEvent

// DefaultName is the calendar every owner has: CalDAV has no standard way for a
// client to create one that the library here answers (MKCALENDAR), so the
// first look at somebody's calendars makes it.
const DefaultName = "default"

// maxName bounds an object's name to what every driver can hold as a key.
const maxName = 255

// Database is what this package needs from the metadata seam.
type Database interface {
	db.Calendars
	Tx(ctx context.Context, fn func(db.Repo) error) error
}

// Precondition is a client's If-Match and If-None-Match, asked about the object
// as it is inside the write's transaction: its ETag, and whether it exists.
type Precondition func(etag string, exists bool) error

// Service is the calendar layer.
type Service struct {
	meta Database
	now  func() time.Time
}

// New wires it to the database.
func New(meta Database) *Service { return &Service{meta: meta, now: time.Now} }

// Calendars lists an owner's calendars, making the default one first if there
// are none.
func (s *Service) Calendars(ctx context.Context, owner string) ([]db.Calendar, error) {
	list, err := s.meta.Calendars(ctx, owner)
	if err != nil || len(list) > 0 {
		return list, err
	}
	if _, err := s.ensureDefault(ctx, owner); err != nil {
		return nil, err
	}
	return s.meta.Calendars(ctx, owner)
}

// Calendar returns one of an owner's calendars, or db.ErrNotFound. The default
// one is never not found.
func (s *Service) Calendar(ctx context.Context, owner, name string) (db.Calendar, error) {
	c, err := s.meta.CalendarByName(ctx, owner, name)
	if errors.Is(err, db.ErrNotFound) && name == DefaultName {
		return s.ensureDefault(ctx, owner)
	}
	return c, err
}

// Create makes a calendar, or answers db.ErrConflict when the name is taken.
func (s *Service) Create(ctx context.Context, owner, name, displayName, description string) (db.Calendar, error) {
	if err := validName(name); err != nil {
		return db.Calendar{}, err
	}
	return s.meta.CreateCalendar(ctx, db.Calendar{
		OwnerID: owner, Name: name, DisplayName: displayName, Description: description, Created: s.now(),
	})
}

// Objects lists what is in one of an owner's calendars.
func (s *Service) Objects(ctx context.Context, owner, calendar string) ([]db.CalendarObject, error) {
	c, err := s.Calendar(ctx, owner, calendar)
	if err != nil {
		return nil, err
	}
	return s.meta.CalendarObjects(ctx, c.ID)
}

// Object returns one object, or db.ErrNotFound.
func (s *Service) Object(ctx context.Context, owner, calendar, name string) (db.CalendarObject, error) {
	c, err := s.Calendar(ctx, owner, calendar)
	if err != nil {
		return db.CalendarObject{}, err
	}
	return s.meta.CalendarObject(ctx, c.ID, name)
}

// Put stores cal under name in one of an owner's calendars, if check agrees
// with what is there. What is stored is cal encoded again rather than the bytes
// a client sent, because the library serves an object by encoding what it
// parsed: storing the client's octets would make the ETag summarise something
// no GET returns.
func (s *Service) Put(ctx context.Context, owner, calendar, name string, cal *ical.Calendar, check Precondition) (db.CalendarObject, error) {
	if err := validName(name); err != nil {
		return db.CalendarObject{}, err
	}
	component, uid, err := validate(cal)
	if err != nil {
		return db.CalendarObject{}, err
	}
	data, err := Encode(cal)
	if err != nil {
		return db.CalendarObject{}, err
	}
	sum := sha256.Sum256([]byte(data))
	if err = s.ensure(ctx, owner, calendar); err != nil {
		return db.CalendarObject{}, err
	}

	var out db.CalendarObject
	err = s.meta.Tx(ctx, func(r db.Repo) error {
		c, txErr := r.CalendarByName(ctx, owner, calendar)
		if txErr != nil {
			return txErr
		}
		existing, txErr := r.CalendarObject(ctx, c.ID, name)
		exists := txErr == nil
		if txErr != nil && !errors.Is(txErr, db.ErrNotFound) {
			return txErr
		}
		if check != nil {
			if txErr = check(existing.ETag, exists); txErr != nil {
				return txErr
			}
		}
		out, txErr = r.PutCalendarObject(ctx, db.CalendarObject{
			CalendarID: c.ID, Name: name, UID: uid, Component: component,
			Data: data, ETag: hex.EncodeToString(sum[:]), Changed: s.now(),
		})
		return txErr
	})
	if err != nil {
		return db.CalendarObject{}, err
	}
	return out, nil
}

// Delete removes one object, or answers db.ErrNotFound.
func (s *Service) Delete(ctx context.Context, owner, calendar, name string) error {
	if err := s.ensure(ctx, owner, calendar); err != nil {
		return err
	}
	return s.meta.Tx(ctx, func(r db.Repo) error {
		c, err := r.CalendarByName(ctx, owner, calendar)
		if err != nil {
			return err
		}
		return r.DeleteCalendarObject(ctx, c.ID, name, s.now())
	})
}

// Changes returns what changed in one of an owner's calendars after the token
// since -- written, or deleted -- and the token to come back with. A since of
// zero is a first sync, which wants what is there and not what used to be.
//
// The token handed back is the highest of the calendar's and of the changes',
// because the two are separate reads: a write landing between them is in the
// changes and would otherwise be sent again next time.
func (s *Service) Changes(ctx context.Context, owner, calendar string, since int64) ([]db.CalendarChange, db.Calendar, error) {
	c, err := s.Calendar(ctx, owner, calendar)
	if err != nil {
		return nil, db.Calendar{}, err
	}
	if since < 0 || since > c.SyncToken || (since > 0 && since < c.PrunedToken) {
		return nil, c, ErrStaleToken
	}
	changes, err := s.meta.CalendarChanges(ctx, c.ID, since)
	if err != nil {
		return nil, c, err
	}
	out := changes[:0]
	for _, ch := range changes {
		c.SyncToken = max(c.SyncToken, ch.SyncToken)
		if since == 0 && ch.Deleted {
			continue
		}
		out = append(out, ch)
	}
	return out, c, nil
}

// Prune forgets the deletions older than TombstoneRetention, and says how
// many.
func (s *Service) Prune(ctx context.Context) (int64, error) {
	var n int64
	err := s.meta.Tx(ctx, func(r db.Repo) error {
		var err error
		n, err = r.PruneCalendarTombstones(ctx, s.now().Add(-TombstoneRetention))
		return err
	})
	return n, err
}

// Decode parses what Put stored.
func Decode(o db.CalendarObject) (*ical.Calendar, error) {
	cal, err := ical.NewDecoder(strings.NewReader(o.Data)).Decode()
	if err != nil {
		return nil, fmt.Errorf("calendar: decode %q: %w", o.Name, err)
	}
	return cal, nil
}

// Encode is the one form an object is stored and served in.
func Encode(cal *ical.Calendar) (string, error) {
	var buf bytes.Buffer
	if err := ical.NewEncoder(&buf).Encode(cal); err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return buf.String(), nil
}

// ensure makes the default calendar if that is the one named, before a write
// opens its transaction: inside one, a create that lost a race is a conflict
// that aborts the transaction on PostgreSQL, and on SQLite a second connection
// would wait on the write lock the transaction already holds.
func (s *Service) ensure(ctx context.Context, owner, name string) error {
	if name != DefaultName {
		return nil
	}
	_, err := s.Calendar(ctx, owner, name)
	return err
}

// ensureDefault makes the default calendar, and takes one somebody else made a
// moment earlier as made.
func (s *Service) ensureDefault(ctx context.Context, owner string) (db.Calendar, error) {
	c, err := s.meta.CreateCalendar(ctx, db.Calendar{
		OwnerID: owner, Name: DefaultName, DisplayName: "Calendar", Created: s.now(),
	})
	if errors.Is(err, db.ErrConflict) {
		return s.meta.CalendarByName(ctx, owner, DefaultName)
	}
	return c, err
}

// validate applies RFC 4791 4.1 -- one kind of component besides the time
// zones, one UID across all of them, no METHOD -- and returns the kind and
// the UID.
func validate(cal *ical.Calendar) (component, uid string, err error) {
	if cal == nil || cal.Component == nil {
		return "", "", ErrInvalid
	}
	if cal.Props.Get(ical.PropMethod) != nil {
		return "", "", fmt.Errorf("%w: it carries a METHOD", ErrInvalid)
	}
	for _, child := range cal.Children {
		if child.Name == ical.CompTimezone {
			continue
		}
		if component != "" && child.Name != component {
			return "", "", fmt.Errorf("%w: it holds both %s and %s", ErrInvalid, component, child.Name)
		}
		component = child.Name
		id, err := child.Props.Text(ical.PropUID)
		if err != nil {
			return "", "", fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		if id == "" || (uid != "" && id != uid) {
			return "", "", fmt.Errorf("%w: every component needs the one UID", ErrInvalid)
		}
		uid = id
	}
	if component == "" {
		return "", "", fmt.Errorf("%w: it holds no component", ErrInvalid)
	}
	if component != Component {
		return "", "", fmt.Errorf("%w: %s", ErrUnsupported, component)
	}
	return component, uid, nil
}

func validName(name string) error {
	if name == "" || len(name) > maxName || strings.ContainsAny(name, "/\x00") || name == "." || name == ".." {
		return fmt.Errorf("calendar: name %q: %w", name, db.ErrInvalidPath)
	}
	return nil
}
