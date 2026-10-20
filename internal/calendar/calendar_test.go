package calendar_test

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-ical"

	"github.com/C0piIot/stratus-backend/internal/calendar"
	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/db/sqlite"
)

const owner = "edu"

func service(t *testing.T) (*calendar.Service, *sqlite.Store) {
	t.Helper()
	store, err := sqlite.New(t.Context(), filepath.Join(t.TempDir(), "stratus.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return calendar.New(store), store
}

func parse(t *testing.T, text string) *ical.Calendar {
	t.Helper()
	cal, err := ical.NewDecoder(strings.NewReader(strings.ReplaceAll(text, "\n", "\r\n"))).Decode()
	if err != nil {
		t.Fatal(err)
	}
	return cal
}

// event is how a client sends one: folded lines and properties in its own
// order, none of which survives being stored.
const event = `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//test//EN
BEGIN:VEVENT
UID:standup@example.com
DTSTAMP:20240101T080000Z
DTSTART:20240102T090000Z
RRULE:FREQ=WEEKLY
SUMMARY:A standup long enough that a client folds the line somewhere in the
  middle of it
END:VEVENT
END:VCALENDAR
`

func TestEveryOwnerHasACalendar(t *testing.T) {
	t.Parallel()
	s, _ := service(t)

	list, err := s.Calendars(t.Context(), owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != calendar.DefaultName {
		t.Fatalf("Calendars = %+v, want the default one", list)
	}
	again, err := s.Calendars(t.Context(), owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 1 || again[0].ID != list[0].ID {
		t.Errorf("a second look = %+v, want the same one", again)
	}
}

// TestTheDefaultCalendarIsWritableFirst is a client that writes before it ever
// lists, which is a PUT to a path it was told about by somebody else.
func TestTheDefaultCalendarIsWritableFirst(t *testing.T) {
	t.Parallel()
	s, _ := service(t)

	if _, err := s.Put(t.Context(), owner, calendar.DefaultName, "a.ics", parse(t, event), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(t.Context(), owner, "work", "a.ics", parse(t, event), nil); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("Put into a calendar nobody made = %v, want ErrNotFound", err)
	}
}

// TestWhatIsServedIsWhatIsSummarised is the reason Put encodes: the library
// serves an object by encoding what it parsed, so the stored text has to come
// out of that round trip unchanged or a GET and its ETag disagree.
func TestWhatIsServedIsWhatIsSummarised(t *testing.T) {
	t.Parallel()
	s, _ := service(t)

	o, err := s.Put(t.Context(), owner, calendar.DefaultName, "a.ics", parse(t, event), nil)
	if err != nil {
		t.Fatal(err)
	}
	cal, err := calendar.Decode(o)
	if err != nil {
		t.Fatal(err)
	}
	served, err := calendar.Encode(cal)
	if err != nil {
		t.Fatal(err)
	}
	if served != o.Data {
		t.Errorf("served:\n%q\nstored:\n%q", served, o.Data)
	}
	if o.UID != "standup@example.com" || o.Component != "VEVENT" || len(o.ETag) != 64 {
		t.Errorf("Put = %+v", o)
	}
}

func TestOnlyACalendarObjectIsStored(t *testing.T) {
	t.Parallel()
	s, _ := service(t)

	for _, tc := range []struct {
		name, text string
		want       error
	}{
		{"a task", strings.ReplaceAll(event, "VEVENT", "VTODO"), calendar.ErrUnsupported},
		{"no PRODID", strings.Replace(event, "PRODID:-//test//EN\n", "", 1), calendar.ErrInvalid},
		{"nothing in it", "BEGIN:VCALENDAR\nVERSION:2.0\nPRODID:-//test//EN\nEND:VCALENDAR\n", calendar.ErrInvalid},
		{"no UID", strings.Replace(event, "UID:standup@example.com\n", "", 1), calendar.ErrInvalid},
		{"a METHOD", strings.Replace(event, "VERSION:2.0\n", "VERSION:2.0\nMETHOD:REQUEST\n", 1), calendar.ErrInvalid},
		{"two UIDs", strings.Replace(event, "END:VCALENDAR", "BEGIN:VEVENT\nUID:other\nDTSTAMP:20240101T080000Z\nDTSTART:20240103T090000Z\nEND:VEVENT\nEND:VCALENDAR", 1), calendar.ErrInvalid},
		{"an event and a task", strings.Replace(event, "END:VCALENDAR", "BEGIN:VTODO\nUID:standup@example.com\nDTSTAMP:20240101T080000Z\nEND:VTODO\nEND:VCALENDAR", 1), calendar.ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.Put(t.Context(), owner, calendar.DefaultName, "a.ics", parse(t, tc.text), nil)
			if !errors.Is(err, tc.want) {
				t.Errorf("Put = %v, want %v", err, tc.want)
			}
		})
	}

	t.Run("a time zone beside the event", func(t *testing.T) {
		zoned := strings.Replace(event, "BEGIN:VEVENT", "BEGIN:VTIMEZONE\nTZID:Europe/Madrid\nBEGIN:STANDARD\nDTSTART:19701025T030000\nTZOFFSETFROM:+0200\nTZOFFSETTO:+0100\nEND:STANDARD\nEND:VTIMEZONE\nBEGIN:VEVENT", 1)
		if _, err := s.Put(t.Context(), owner, calendar.DefaultName, "zoned.ics", parse(t, zoned), nil); err != nil {
			t.Errorf("Put = %v", err)
		}
	})

	for _, name := range []string{"", "a/b.ics", "..", strings.Repeat("x", 256)} {
		if _, err := s.Put(t.Context(), owner, calendar.DefaultName, name, parse(t, event), nil); !errors.Is(err, db.ErrInvalidPath) {
			t.Errorf("Put(%q) = %v, want ErrInvalidPath", name, err)
		}
	}
}

// TestAPreconditionIsAskedAboutWhatIsThere is the half the adapter cannot do:
// the ETag it is handed is the one inside the write, and a refusal writes
// nothing.
func TestAPreconditionIsAskedAboutWhatIsThere(t *testing.T) {
	t.Parallel()
	s, _ := service(t)

	var sawExists bool
	first, err := s.Put(t.Context(), owner, calendar.DefaultName, "a.ics", parse(t, event),
		func(_ string, exists bool) error { sawExists = exists; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if sawExists {
		t.Error("the precondition was told a new object exists")
	}

	refused := errors.New("it has changed")
	changed := strings.Replace(event, "SUMMARY:A standup", "SUMMARY:A moved standup", 1)
	var sawETag string
	_, err = s.Put(t.Context(), owner, calendar.DefaultName, "a.ics", parse(t, changed),
		func(etag string, _ bool) error { sawETag = etag; return refused })
	if !errors.Is(err, refused) {
		t.Fatalf("Put = %v, want the precondition's refusal", err)
	}
	if sawETag != first.ETag {
		t.Errorf("the precondition saw %q, want %q", sawETag, first.ETag)
	}
	got, err := s.Object(t.Context(), owner, calendar.DefaultName, "a.ics")
	if err != nil {
		t.Fatal(err)
	}
	if got.ETag != first.ETag {
		t.Error("a refused write was stored")
	}
}

func TestDeleteAndItIsGone(t *testing.T) {
	t.Parallel()
	s, _ := service(t)

	if _, err := s.Put(t.Context(), owner, calendar.DefaultName, "a.ics", parse(t, event), nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(t.Context(), owner, calendar.DefaultName, "a.ics"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Object(t.Context(), owner, calendar.DefaultName, "a.ics"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("Object after Delete = %v, want ErrNotFound", err)
	}
	if err := s.Delete(t.Context(), owner, calendar.DefaultName, "a.ics"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("a second Delete = %v, want ErrNotFound", err)
	}
	list, err := s.Objects(t.Context(), owner, calendar.DefaultName)
	if err != nil || len(list) != 0 {
		t.Errorf("Objects = %+v, %v; want none", list, err)
	}
}

func TestACalendarIsMadeOnce(t *testing.T) {
	t.Parallel()
	s, _ := service(t)

	c, err := s.Create(t.Context(), owner, "work", "Work", "")
	if err != nil {
		t.Fatal(err)
	}
	if c.DisplayName != "Work" {
		t.Errorf("Create = %+v", c)
	}
	if _, err := s.Create(t.Context(), owner, "work", "Work", ""); !errors.Is(err, db.ErrConflict) {
		t.Errorf("a second Create = %v, want ErrConflict", err)
	}
	if _, err := s.Create(t.Context(), owner, "a/b", "", ""); !errors.Is(err, db.ErrInvalidPath) {
		t.Errorf("Create with a slash = %v, want ErrInvalidPath", err)
	}
}

func TestACalendarNobodyMadeIsNotFound(t *testing.T) {
	t.Parallel()
	s, _ := service(t)

	if _, err := s.Objects(t.Context(), owner, "work"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("Objects = %v, want ErrNotFound", err)
	}
	if _, err := s.Object(t.Context(), owner, "work", "a.ics"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("Object = %v, want ErrNotFound", err)
	}
	if err := s.Delete(t.Context(), owner, "work", "a.ics"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("Delete = %v, want ErrNotFound", err)
	}
	if _, err := calendar.Decode(db.CalendarObject{Name: "a.ics", Data: "not iCalendar"}); err == nil {
		t.Error("Decode took text that is not iCalendar")
	}
}

// TestChangesSinceAToken is what sync-collection answers from: a first sync is
// what is there, a later one is what changed including deletions, and a token
// the server never wrote or has forgotten the deletions behind is stale.
func TestChangesSinceAToken(t *testing.T) {
	t.Parallel()
	s, store := service(t)
	ctx := t.Context()
	put := func(name, uid string) {
		t.Helper()
		if _, err := s.Put(ctx, owner, calendar.DefaultName, name, parse(t, strings.ReplaceAll(event, "standup@example.com", uid)), nil); err != nil {
			t.Fatal(err)
		}
	}
	put("a.ics", "a")
	put("b.ics", "b")
	if err := s.Delete(ctx, owner, calendar.DefaultName, "a.ics"); err != nil {
		t.Fatal(err)
	}

	first, c, err := s.Changes(ctx, owner, calendar.DefaultName, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].Name != "b.ics" || c.SyncToken != 3 {
		t.Errorf("a first sync = %+v at %d, want b.ics alone at 3", first, c.SyncToken)
	}
	later, _, err := s.Changes(ctx, owner, calendar.DefaultName, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(later) != 2 || !later[1].Deleted {
		t.Errorf("changes since 1 = %+v, want b.ics and the deletion of a.ics", later)
	}

	if _, _, err := s.Changes(ctx, owner, calendar.DefaultName, 4); !errors.Is(err, calendar.ErrStaleToken) {
		t.Errorf("a token from the future = %v, want ErrStaleToken", err)
	}
	if _, _, err := s.Changes(ctx, owner, "work", 0); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("changes to a calendar nobody made = %v, want ErrNotFound", err)
	}
	if _, err := store.PruneCalendarTombstones(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Changes(ctx, owner, calendar.DefaultName, 1); !errors.Is(err, calendar.ErrStaleToken) {
		t.Errorf("a token from before a forgotten deletion = %v, want ErrStaleToken", err)
	}
	if n, err := s.Prune(ctx); err != nil || n != 0 {
		t.Errorf("Prune with nothing old = %d, %v", n, err)
	}
}
