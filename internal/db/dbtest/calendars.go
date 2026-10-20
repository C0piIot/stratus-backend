package dbtest

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// RunCalendars executes the calendar cases against the repository built by
// newRepo.
func RunCalendars(t *testing.T, newRepo func(t *testing.T) db.Repo) {
	t.Helper()

	cases := []struct {
		name string
		fn   func(t *testing.T, s db.Repo)
	}{
		{"a calendar comes back as it was stored", calendarRoundTrip},
		{"an owner cannot have two calendars by one name", calendarNameTaken},
		{"owners do not see each other's calendars", calendarOwnersAreSeparate},
		{"a missing calendar is ErrNotFound", calendarMissing},
		{"an object comes back as it was stored", calendarObjectRoundTrip},
		{"a large object is stored whole", calendarObjectLarge},
		{"writing a name again replaces it and moves the token", calendarObjectReplace},
		{"one UID cannot be two objects in a calendar", calendarUIDTaken},
		{"one UID can be in two calendars", calendarUIDPerCalendar},
		{"a missing object is ErrNotFound", calendarObjectMissing},
		{"changes since a token are writes and deletions in order", calendarChanges},
		{"a name written again after a delete is one change", calendarChangesRevive},
		{"pruning forgets old deletions and says how far", calendarPrune},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.fn(t, newRepo(t))
		})
	}
}

var calendarAt = time.Date(2024, 9, 1, 20, 0, 0, 125_000_000, time.UTC)

func newCalendar(t *testing.T, s db.Repo, ownerID, name string) db.Calendar {
	t.Helper()
	c, err := s.CreateCalendar(t.Context(), db.Calendar{
		OwnerID: ownerID, Name: name, DisplayName: "Shown " + name, Description: "About " + name, Created: calendarAt,
	})
	if err != nil {
		t.Fatalf("CreateCalendar(%q): %v", name, err)
	}
	return c
}

func calendarEvent(uid string) string {
	return "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//dbtest//EN\r\nBEGIN:VEVENT\r\nUID:" + uid +
		"\r\nDTSTAMP:20240901T200000Z\r\nDTSTART:20240902T090000Z\r\nSUMMARY:Standup\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
}

func putObject(t *testing.T, s db.Repo, c db.Calendar, name, uid string) db.CalendarObject {
	t.Helper()
	o, err := s.PutCalendarObject(t.Context(), db.CalendarObject{
		CalendarID: c.ID, Name: name, UID: uid, Component: "VEVENT",
		Data: calendarEvent(uid), ETag: `"` + uid + `"`, Changed: calendarAt,
	})
	if err != nil {
		t.Fatalf("PutCalendarObject(%q): %v", name, err)
	}
	return o
}

func calendarByName(t *testing.T, s db.Repo, c db.Calendar) db.Calendar {
	t.Helper()
	got, err := s.CalendarByName(t.Context(), c.OwnerID, c.Name)
	if err != nil {
		t.Fatalf("CalendarByName(%q): %v", c.Name, err)
	}
	return got
}

func calendarRoundTrip(t *testing.T, s db.Repo) {
	c := newCalendar(t, s, "alice", "work")
	if c.ID == 0 {
		t.Fatal("CreateCalendar returned no id")
	}
	want := db.Calendar{
		ID: c.ID, OwnerID: "alice", Name: "work", DisplayName: "Shown work", Description: "About work",
		Created: calendarAt.Truncate(time.Millisecond),
	}
	if got := calendarByName(t, s, c); got != want {
		t.Errorf("CalendarByName = %+v, want %+v", got, want)
	}

	newCalendar(t, s, "alice", "home")
	list, err := s.Calendars(t.Context(), "alice")
	if err != nil {
		t.Fatalf("Calendars: %v", err)
	}
	names := make([]string, 0, len(list))
	for _, c := range list {
		names = append(names, c.Name)
	}
	if !slices.Equal(names, []string{"home", "work"}) {
		t.Errorf("Calendars = %v, want [home work]", names)
	}
}

func calendarNameTaken(t *testing.T, s db.Repo) {
	newCalendar(t, s, "alice", "work")
	_, err := s.CreateCalendar(t.Context(), db.Calendar{OwnerID: "alice", Name: "work", Created: calendarAt})
	if !errors.Is(err, db.ErrConflict) {
		t.Errorf("second CreateCalendar = %v, want ErrConflict", err)
	}
}

func calendarOwnersAreSeparate(t *testing.T, s db.Repo) {
	newCalendar(t, s, "alice", "work")
	newCalendar(t, s, "bob", "work")

	if _, err := s.CalendarByName(t.Context(), "carol", "work"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("CalendarByName for another owner = %v, want ErrNotFound", err)
	}
	list, err := s.Calendars(t.Context(), "bob")
	if err != nil {
		t.Fatalf("Calendars: %v", err)
	}
	if len(list) != 1 || list[0].OwnerID != "bob" {
		t.Errorf("Calendars(bob) = %+v, want bob's one", list)
	}
}

func calendarMissing(t *testing.T, s db.Repo) {
	if _, err := s.CalendarByName(t.Context(), "alice", "nope"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("CalendarByName = %v, want ErrNotFound", err)
	}
	_, err := s.PutCalendarObject(t.Context(), db.CalendarObject{
		CalendarID: 9999, Name: "a.ics", UID: "a", Component: "VEVENT", Data: calendarEvent("a"), Changed: calendarAt,
	})
	if !errors.Is(err, db.ErrNotFound) {
		t.Errorf("PutCalendarObject into no calendar = %v, want ErrNotFound", err)
	}
}

func calendarObjectRoundTrip(t *testing.T, s db.Repo) {
	c := newCalendar(t, s, "alice", "work")
	put := putObject(t, s, c, "standup.ics", "standup@example.com")
	if put.SyncToken == 0 {
		t.Error("PutCalendarObject returned no token")
	}

	want := db.CalendarObject{
		CalendarID: c.ID, Name: "standup.ics", UID: "standup@example.com", Component: "VEVENT",
		Data: calendarEvent("standup@example.com"), ETag: `"standup@example.com"`,
		SyncToken: put.SyncToken, Changed: calendarAt.Truncate(time.Millisecond),
	}
	got, err := s.CalendarObject(t.Context(), c.ID, "standup.ics")
	if err != nil {
		t.Fatalf("CalendarObject: %v", err)
	}
	if got != want {
		t.Errorf("CalendarObject = %+v, want %+v", got, want)
	}
	if put != want {
		t.Errorf("PutCalendarObject returned %+v, want %+v", put, want)
	}

	putObject(t, s, c, "review.ics", "review@example.com")
	list, err := s.CalendarObjects(t.Context(), c.ID)
	if err != nil {
		t.Fatalf("CalendarObjects: %v", err)
	}
	if len(list) != 2 || list[0].Name != "review.ics" || list[1] != want {
		t.Errorf("CalendarObjects = %+v, want review.ics then %+v", list, want)
	}
	if got := calendarByName(t, s, c); got.SyncToken != 2 {
		t.Errorf("calendar token = %d after two writes, want 2", got.SyncToken)
	}
}

// calendarObjectLarge is past the 64 KB a MySQL TEXT holds, which is what an
// event carrying an attachment inline reaches.
func calendarObjectLarge(t *testing.T, s db.Repo) {
	c := newCalendar(t, s, "alice", "work")
	data := strings.Replace(calendarEvent("big"), "SUMMARY:Standup",
		"ATTACH;ENCODING=BASE64;VALUE=BINARY:"+strings.Repeat("QUJD", 50_000), 1)
	if _, err := s.PutCalendarObject(t.Context(), db.CalendarObject{
		CalendarID: c.ID, Name: "big.ics", UID: "big", Component: "VEVENT", Data: data, Changed: calendarAt,
	}); err != nil {
		t.Fatalf("PutCalendarObject: %v", err)
	}
	got, err := s.CalendarObject(t.Context(), c.ID, "big.ics")
	if err != nil {
		t.Fatalf("CalendarObject: %v", err)
	}
	if got.Data != data {
		t.Errorf("Data came back as %d bytes, want %d", len(got.Data), len(data))
	}
}

func calendarObjectReplace(t *testing.T, s db.Repo) {
	c := newCalendar(t, s, "alice", "work")
	first := putObject(t, s, c, "standup.ics", "standup")

	second, err := s.PutCalendarObject(t.Context(), db.CalendarObject{
		CalendarID: c.ID, Name: "standup.ics", UID: "standup", Component: "VEVENT",
		Data: calendarEvent("standup") + " ", ETag: `"moved"`, Changed: calendarAt.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("second PutCalendarObject: %v", err)
	}
	if second.SyncToken <= first.SyncToken {
		t.Errorf("token went from %d to %d, want it to grow", first.SyncToken, second.SyncToken)
	}
	got, err := s.CalendarObject(t.Context(), c.ID, "standup.ics")
	if err != nil {
		t.Fatalf("CalendarObject: %v", err)
	}
	if got.ETag != `"moved"` || !got.Changed.Equal(calendarAt.Add(time.Hour).Truncate(time.Millisecond)) {
		t.Errorf("CalendarObject = %+v, want the second write", got)
	}
	list, err := s.CalendarObjects(t.Context(), c.ID)
	if err != nil {
		t.Fatalf("CalendarObjects: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("CalendarObjects holds %d, want 1", len(list))
	}
}

func calendarUIDTaken(t *testing.T, s db.Repo) {
	c := newCalendar(t, s, "alice", "work")
	putObject(t, s, c, "one.ics", "standup")

	_, err := s.PutCalendarObject(t.Context(), db.CalendarObject{
		CalendarID: c.ID, Name: "two.ics", UID: "standup", Component: "VEVENT",
		Data: calendarEvent("standup"), Changed: calendarAt,
	})
	if !errors.Is(err, db.ErrConflict) {
		t.Errorf("PutCalendarObject with a taken UID = %v, want ErrConflict", err)
	}
	if got, err := s.CalendarObject(t.Context(), c.ID, "one.ics"); err != nil || got.UID != "standup" {
		t.Errorf("the object holding the UID = %+v, %v; want it untouched", got, err)
	}
	if _, err := s.CalendarObject(t.Context(), c.ID, "two.ics"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("the refused object = %v, want ErrNotFound", err)
	}
}

func calendarUIDPerCalendar(t *testing.T, s db.Repo) {
	work := newCalendar(t, s, "alice", "work")
	home := newCalendar(t, s, "alice", "home")
	putObject(t, s, work, "standup.ics", "standup")
	putObject(t, s, home, "standup.ics", "standup")
}

func calendarObjectMissing(t *testing.T, s db.Repo) {
	c := newCalendar(t, s, "alice", "work")
	if _, err := s.CalendarObject(t.Context(), c.ID, "nope.ics"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("CalendarObject = %v, want ErrNotFound", err)
	}
	if err := s.DeleteCalendarObject(t.Context(), c.ID, "nope.ics", calendarAt); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("DeleteCalendarObject = %v, want ErrNotFound", err)
	}
	if got := calendarByName(t, s, c); got.SyncToken != 0 {
		t.Errorf("token = %d after deleting nothing, want 0", got.SyncToken)
	}
}

func calendarChanges(t *testing.T, s db.Repo) {
	c := newCalendar(t, s, "alice", "work")
	other := newCalendar(t, s, "alice", "home")
	a := putObject(t, s, c, "a.ics", "a")
	putObject(t, s, c, "b.ics", "b")
	putObject(t, s, other, "elsewhere.ics", "elsewhere")
	if err := s.DeleteCalendarObject(t.Context(), c.ID, "a.ics", calendarAt); err != nil {
		t.Fatalf("DeleteCalendarObject: %v", err)
	}
	putObject(t, s, c, "c.ics", "c")

	if _, err := s.CalendarObject(t.Context(), c.ID, "a.ics"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("deleted object = %v, want ErrNotFound", err)
	}

	got, err := s.CalendarChanges(t.Context(), c.ID, 0)
	if err != nil {
		t.Fatalf("CalendarChanges: %v", err)
	}
	want := []db.CalendarChange{
		{Name: "b.ics", SyncToken: 2},
		{Name: "a.ics", SyncToken: 3, Deleted: true},
		{Name: "c.ics", SyncToken: 4},
	}
	if !slices.Equal(got, want) {
		t.Errorf("CalendarChanges(0) = %+v, want %+v", got, want)
	}

	got, err = s.CalendarChanges(t.Context(), c.ID, a.SyncToken+1)
	if err != nil {
		t.Fatalf("CalendarChanges: %v", err)
	}
	if !slices.Equal(got, want[1:]) {
		t.Errorf("CalendarChanges(%d) = %+v, want %+v", a.SyncToken+1, got, want[1:])
	}

	got, err = s.CalendarChanges(t.Context(), c.ID, 4)
	if err != nil {
		t.Fatalf("CalendarChanges: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("CalendarChanges(current) = %+v, want none", got)
	}
}

func calendarChangesRevive(t *testing.T, s db.Repo) {
	c := newCalendar(t, s, "alice", "work")
	putObject(t, s, c, "a.ics", "a")
	if err := s.DeleteCalendarObject(t.Context(), c.ID, "a.ics", calendarAt); err != nil {
		t.Fatalf("DeleteCalendarObject: %v", err)
	}
	again := putObject(t, s, c, "a.ics", "a")

	got, err := s.CalendarChanges(t.Context(), c.ID, 0)
	if err != nil {
		t.Fatalf("CalendarChanges: %v", err)
	}
	want := []db.CalendarChange{{Name: "a.ics", SyncToken: again.SyncToken}}
	if !slices.Equal(got, want) {
		t.Errorf("CalendarChanges = %+v, want %+v", got, want)
	}
}

func calendarPrune(t *testing.T, s db.Repo) {
	c := newCalendar(t, s, "alice", "work")
	untouched := newCalendar(t, s, "alice", "home")
	for _, name := range []string{"old.ics", "older.ics", "new.ics"} {
		putObject(t, s, c, name, name)
	}
	putObject(t, s, untouched, "kept.ics", "kept")
	deleteAt := func(name string, at time.Time) {
		t.Helper()
		if err := s.DeleteCalendarObject(t.Context(), c.ID, name, at); err != nil {
			t.Fatalf("DeleteCalendarObject(%q): %v", name, err)
		}
	}
	deleteAt("older.ics", calendarAt)
	deleteAt("old.ics", calendarAt.Add(time.Hour))
	deleteAt("new.ics", calendarAt.Add(48*time.Hour))

	n, err := s.PruneCalendarTombstones(t.Context(), calendarAt.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("PruneCalendarTombstones: %v", err)
	}
	if n != 2 {
		t.Errorf("PruneCalendarTombstones forgot %d, want 2", n)
	}

	if got := calendarByName(t, s, c); got.PrunedToken != 5 {
		t.Errorf("PrunedToken = %d, want 5, the later of the two forgotten", got.PrunedToken)
	}
	if got := calendarByName(t, s, untouched); got.PrunedToken != 0 {
		t.Errorf("PrunedToken of a calendar with nothing pruned = %d, want 0", got.PrunedToken)
	}
	got, err := s.CalendarChanges(t.Context(), c.ID, 0)
	if err != nil {
		t.Fatalf("CalendarChanges: %v", err)
	}
	want := []db.CalendarChange{{Name: "new.ics", SyncToken: 6, Deleted: true}}
	if !slices.Equal(got, want) {
		t.Errorf("CalendarChanges = %+v, want %+v", got, want)
	}

	if n, err := s.PruneCalendarTombstones(t.Context(), calendarAt.Add(24*time.Hour)); err != nil || n != 0 {
		t.Errorf("pruning again = %d, %v; want 0", n, err)
	}
	if got := calendarByName(t, s, c); got.PrunedToken != 5 {
		t.Errorf("PrunedToken after pruning nothing = %d, want it left at 5", got.PrunedToken)
	}
}
