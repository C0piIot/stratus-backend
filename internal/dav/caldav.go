package dav

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav"
	"github.com/emersion/go-webdav/caldav"

	"github.com/C0piIot/stratus-backend/internal/auth"
	"github.com/C0piIot/stratus-backend/internal/calendar"
	"github.com/C0piIot/stratus-backend/internal/db"
)

// CalDAV serves calendars (#75), a mount of its own beside the generated ones.
//
// **The layout is the library's, counted in segments.** caldav.Handler tells a
// principal from a home set from a calendar from an object by how deep the path
// is below its prefix -- one, two, three, four -- so the prefix here is empty
// and the four are:
//
//	/calendars/                          the principal: whoever is asking
//	/calendars/<owner>/                  their calendars
//	/calendars/<owner>/<calendar>/       one calendar
//	/calendars/<owner>/<calendar>/<name> one object
//
// The principal being one address for everybody is what makes the depths come
// out, and it is honest: it is the current user's, which is what a client asks
// it about. The owner is in the path below it, so sharing has somewhere to put
// a second one.
//
// **The library compares a principal and a home set with ==**, so both are
// written here once, with the trailing slash, and nowhere else. One slash out
// of place and a PROPFIND answers an empty multistatus with no error at all.
//
// So a collection is given its slash before the library sees it: clients drop
// it -- go-webdav's own does, through path.Join -- and a principal asked for
// as /calendars is otherwise a multistatus with nothing in it. An object is
// the fourth segment and is left alone.
//
// It is not wrapped in http.StripPrefix like Handler is: the library builds
// every href from the request's own path and the paths the backend returns,
// so it has to see the whole of it.
func CalDAV(prefix string, service *calendar.Service) http.Handler {
	b := &calendarBackend{prefix: prefix, cal: service}
	h := &caldav.Handler{Backend: b}
	bare := strings.TrimSuffix(prefix, "/")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Deeper than an object is nothing at all, which the library would
		// answer with an empty multistatus rather than say.
		if strings.Count(strings.Trim(strings.TrimPrefix(r.URL.Path, bare), "/"), "/") > 2 {
			http.NotFound(w, r)
			return
		}
		if p := r.URL.Path; !strings.HasSuffix(p, "/") && strings.HasPrefix(p+"/", prefix) &&
			strings.Count(strings.TrimPrefix(p, bare), "/") < 3 {
			r = r.Clone(r.Context())
			r.URL.Path = p + "/"
			r.URL.RawPath = ""
		}
		switch r.Method {
		case "REPORT":
			b.report(w, r, h)
		case "PROPFIND":
			b.propfindExtras(w, r, h)
		case "MKCALENDAR":
			serve(w, b.mkcalendar(w, r))
		case "PROPPATCH":
			if b.isCalendar(r.URL.Path) {
				serve(w, b.proppatch(w, r))
			} else {
				h.ServeHTTP(w, r)
			}
		case http.MethodDelete:
			if b.isCalendar(r.URL.Path) {
				serve(w, b.deleteCalendar(w, r))
			} else {
				h.ServeHTTP(w, r)
			}
		default:
			h.ServeHTTP(w, r)
		}
	})
}

// report answers sync-collection here and hands every other REPORT to the
// library, which refuses the ones it does not know.
func (b *calendarBackend) report(w http.ResponseWriter, r *http.Request, next http.Handler) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "cannot read the request", http.StatusBadRequest)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	var req syncCollection
	if xml.Unmarshal(body, &req) != nil {
		next.ServeHTTP(w, r)
		return
	}
	if err := b.serveSync(w, r, req); err != nil {
		serveCalendarError(w, err)
	}
}

type calendarBackend struct {
	prefix string
	cal    *calendar.Service
}

// CurrentUserPrincipal implements caldav.Backend.
func (b *calendarBackend) CurrentUserPrincipal(ctx context.Context) (string, error) {
	if _, err := calendarOwner(ctx); err != nil {
		return "", err
	}
	return b.prefix, nil
}

// CalendarHomeSetPath implements caldav.Backend.
func (b *calendarBackend) CalendarHomeSetPath(ctx context.Context) (string, error) {
	owner, err := calendarOwner(ctx)
	if err != nil {
		return "", err
	}
	return b.home(owner), nil
}

// CreateCalendar implements caldav.Backend, for the extended MKCOL of RFC 5689.
func (b *calendarBackend) CreateCalendar(ctx context.Context, c *caldav.Calendar) error {
	owner, name, _, err := b.split(ctx, c.Path, 2)
	if err != nil {
		return err
	}
	_, err = b.cal.Create(ctx, owner, db.Calendar{Name: name, DisplayName: c.Name, Description: c.Description})
	return mapCalendarErr(ctx, err)
}

// ListCalendars implements caldav.Backend.
func (b *calendarBackend) ListCalendars(ctx context.Context) ([]caldav.Calendar, error) {
	owner, err := calendarOwner(ctx)
	if err != nil {
		return nil, err
	}
	list, err := b.cal.Calendars(ctx, owner)
	if err != nil {
		return nil, mapCalendarErr(ctx, err)
	}
	out := make([]caldav.Calendar, 0, len(list))
	for _, c := range list {
		out = append(out, b.calendar(c))
	}
	return out, nil
}

// GetCalendar implements caldav.Backend.
func (b *calendarBackend) GetCalendar(ctx context.Context, p string) (*caldav.Calendar, error) {
	owner, name, _, err := b.split(ctx, p, 2)
	if err != nil {
		return nil, err
	}
	c, err := b.cal.Calendar(ctx, owner, name)
	if err != nil {
		return nil, mapCalendarErr(ctx, err)
	}
	out := b.calendar(c)
	return &out, nil
}

// GetCalendarObject implements caldav.Backend. The request for parts of the
// object is not honoured: the whole of it is a few kilobytes, and a client
// that asked for less can ignore the rest.
func (b *calendarBackend) GetCalendarObject(ctx context.Context, p string, _ *caldav.CalendarCompRequest) (*caldav.CalendarObject, error) {
	owner, cal, name, err := b.split(ctx, p, 3)
	if err != nil {
		return nil, err
	}
	o, err := b.cal.Object(ctx, owner, cal, name)
	if err != nil {
		return nil, mapCalendarErr(ctx, err)
	}
	out, err := b.object(ctx, owner, cal, o)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ListCalendarObjects implements caldav.Backend.
func (b *calendarBackend) ListCalendarObjects(ctx context.Context, p string, _ *caldav.CalendarCompRequest) ([]caldav.CalendarObject, error) {
	owner, cal, _, err := b.split(ctx, p, 2)
	if err != nil {
		return nil, err
	}
	list, err := b.cal.Objects(ctx, owner, cal)
	if err != nil {
		return nil, mapCalendarErr(ctx, err)
	}
	out := make([]caldav.CalendarObject, 0, len(list))
	for _, o := range list {
		co, err := b.object(ctx, owner, cal, o)
		if err != nil {
			return nil, err
		}
		out = append(out, co)
	}
	return out, nil
}

// QueryCalendarObjects implements caldav.Backend. The handler filters nothing
// itself, so the filter is called here -- and it is where a recurring event is
// matched against a window by its occurrences rather than its first date,
// which is why nothing is narrowed in SQL first: an event every Tuesday since
// January starts in January and still falls in March.
//
// A window over a component is taken out of the query before the library sees
// it and asked of calendar.Overlaps afterwards: the library answers no for a
// task or a journal entry (#79), and reads a repetition without its RDATEs
// (#78).
func (b *calendarBackend) QueryCalendarObjects(ctx context.Context, p string, query *caldav.CalendarQuery) ([]caldav.CalendarObject, error) {
	all, err := b.ListCalendarObjects(ctx, p, nil)
	if err != nil {
		return nil, err
	}
	rest, windows := ownWindows(query)
	out, err := caldav.Filter(rest, all)
	if err != nil {
		return nil, webdav.NewHTTPError(http.StatusBadRequest, err)
	}
	if len(windows) == 0 {
		return out, nil
	}
	kept := out[:0]
	for _, o := range out {
		in, err := inWindows(o.Data, windows)
		if err != nil {
			return nil, webdav.NewHTTPError(http.StatusBadRequest, err)
		}
		if in {
			kept = append(kept, o)
		}
	}
	return kept, nil
}

// componentWindow is a time range a query asks of one kind of component.
type componentWindow struct {
	name       string
	start, end time.Time
}

// ownWindows is the query with its windows taken out of it, and those
// windows. Only where clients put them: on a component filter directly under
// VCALENDAR.
func ownWindows(query *caldav.CalendarQuery) (*caldav.CalendarQuery, []componentWindow) {
	if query == nil {
		return nil, nil
	}
	rest := *query
	rest.CompFilter.Comps = slices.Clone(query.CompFilter.Comps)
	var windows []componentWindow
	for i, f := range rest.CompFilter.Comps {
		if !f.Start.IsZero() {
			windows = append(windows, componentWindow{name: f.Name, start: f.Start, end: f.End})
			rest.CompFilter.Comps[i].Start, rest.CompFilter.Comps[i].End = time.Time{}, time.Time{}
		}
	}
	return &rest, windows
}

// inWindows is whether some instance of each window's kind falls in it.
func inWindows(cal *ical.Calendar, windows []componentWindow) (bool, error) {
	for _, w := range windows {
		in, err := calendar.Overlaps(cal, w.name, w.start, w.end)
		if err != nil || !in {
			return false, err
		}
	}
	return true, nil
}

// PutCalendarObject implements caldav.Backend. The library only forwards the
// conditional headers, so they are compared here, inside the write.
func (b *calendarBackend) PutCalendarObject(ctx context.Context, p string, cal *ical.Calendar, opts *caldav.PutCalendarObjectOptions) (*caldav.CalendarObject, error) {
	owner, calName, name, err := b.split(ctx, p, 3)
	if err != nil {
		return nil, err
	}
	// Kept to be handed back as it is: it already carries its status, and
	// the library's error type is not one this package can name to tell.
	var refused error
	check := func(etag string, exists bool) error {
		if opts != nil {
			refused = checkConditions(opts.IfMatch, opts.IfNoneMatch, etag, exists)
		}
		return refused
	}
	o, err := b.cal.Put(ctx, owner, calName, name, cal, check)
	if refused != nil {
		return nil, refused
	}
	if err != nil {
		return nil, mapCalendarErr(ctx, err)
	}
	out, err := b.object(ctx, owner, calName, o)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteCalendarObject implements caldav.Backend.
func (b *calendarBackend) DeleteCalendarObject(ctx context.Context, p string) error {
	owner, cal, name, err := b.split(ctx, p, 3)
	if err != nil {
		return err
	}
	return mapCalendarErr(ctx, b.cal.Delete(ctx, owner, cal, name))
}

func (b *calendarBackend) home(owner string) string { return b.prefix + owner + "/" }

func (b *calendarBackend) calendar(c db.Calendar) caldav.Calendar {
	return caldav.Calendar{
		Path:                  b.home(c.OwnerID) + c.Name + "/",
		Name:                  c.DisplayName,
		Description:           c.Description,
		SupportedComponentSet: strings.Split(c.Components, ","),
	}
}

// object is a row as the library wants it: parsed, since its filter panics on
// an object with no calendar in it.
func (b *calendarBackend) object(ctx context.Context, owner, cal string, o db.CalendarObject) (caldav.CalendarObject, error) {
	data, err := calendar.Decode(o)
	if err != nil {
		return caldav.CalendarObject{}, mapCalendarErr(ctx, err)
	}
	return caldav.CalendarObject{
		Path:          b.home(owner) + cal + "/" + o.Name,
		ModTime:       o.Changed,
		ContentLength: int64(len(o.Data)),
		ETag:          o.ETag,
		Data:          data,
	}, nil
}

// split reads the owner, the calendar and the object out of a path, wanting
// exactly segments of them. Somebody else's owner segment is not found rather
// than forbidden, which is what their calendar is from here.
func (b *calendarBackend) split(ctx context.Context, p string, segments int) (owner, cal, name string, err error) {
	user, err := calendarOwner(ctx)
	if err != nil {
		return "", "", "", err
	}
	rest, ok := strings.CutPrefix(p, b.prefix)
	parts := strings.Split(strings.TrimSuffix(rest, "/"), "/")
	if !ok || len(parts) != segments || slicesContainEmpty(parts) {
		return "", "", "", webdav.NewHTTPError(http.StatusNotFound, errors.New("not a calendar path"))
	}
	if parts[0] != user {
		return "", "", "", webdav.NewHTTPError(http.StatusNotFound, errors.New("not your calendars"))
	}
	if segments == 3 {
		return parts[0], parts[1], parts[2], nil
	}
	return parts[0], parts[1], "", nil
}

func slicesContainEmpty(parts []string) bool {
	for _, p := range parts {
		if p == "" {
			return true
		}
	}
	return false
}

func calendarOwner(ctx context.Context) (string, error) {
	username, ok := auth.User(ctx)
	if !ok {
		return "", webdav.NewHTTPError(http.StatusUnauthorized, errors.New("no authenticated user on the request"))
	}
	return username, nil
}

// mapCalendarErr is mapErr with CalDAV's own preconditions in front of it,
// which are what a client reads to know why a PUT was refused.
func mapCalendarErr(ctx context.Context, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, calendar.ErrUnsupported):
		return caldav.NewPreconditionError(caldav.PreconditionSupportedCalendarComponent)
	case errors.Is(err, calendar.ErrInvalid):
		return caldav.NewPreconditionError(caldav.PreconditionValidCalendarObjectResource)
	case errors.Is(err, db.ErrConflict):
		return caldav.NewPreconditionError(caldav.PreconditionNoUIDConflict)
	}
	return mapErr(ctx, err)
}
