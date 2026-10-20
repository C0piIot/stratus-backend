package dav

import (
	"context"
	"encoding/xml"
	"net/http"
	"time"

	"github.com/emersion/go-webdav"
	"github.com/emersion/go-webdav/caldav"

	"github.com/C0piIot/stratus-backend/internal/calendar"
)

// CALDAV:expand (RFC 4791 9.6.5, #78): a client asking a calendar-query or a
// calendar-multiget for the instances of what repeats, rather than the rule.
// The library reads the calendar-data of neither -- a calendar-query's is a
// TODO in its source -- so the window is read here, carried on the request's
// context, and applied to what the backend hands back once the filter has
// chosen it. Applied after and not before, because the filter's window and
// the expansion's need not be the same one.

type expandKey struct{}

type expandAsk struct {
	Prop struct {
		Data *struct {
			Expand *struct {
				Start string `xml:"start,attr"`
				End   string `xml:"end,attr"`
			} `xml:"urn:ietf:params:xml:ns:caldav expand"`
		} `xml:"urn:ietf:params:xml:ns:caldav calendar-data"`
	} `xml:"DAV: prop"`
}

// icalUTC is how a time-range or an expand writes a moment.
const icalUTC = "20060102T150405Z"

// withExpand is ctx carrying the window a report asks to expand into, if it
// asks. Both ends are required, and a time that is not one is a 400.
func withExpand(ctx context.Context, body []byte) (context.Context, error) {
	var ask expandAsk
	if err := xml.Unmarshal(body, &ask); err != nil {
		return ctx, nil //nolint:nilerr // a body that does not parse is the library's to refuse
	}
	if ask.Prop.Data == nil || ask.Prop.Data.Expand == nil {
		return ctx, nil
	}
	e := ask.Prop.Data.Expand
	start, err := time.Parse(icalUTC, e.Start)
	if err != nil {
		return ctx, precondition(http.StatusBadRequest, nsCalDAV, "valid-filter")
	}
	end, err := time.Parse(icalUTC, e.End)
	if err != nil || !end.After(start) {
		return ctx, precondition(http.StatusBadRequest, nsCalDAV, "valid-filter")
	}
	return context.WithValue(ctx, expandKey{}, componentWindow{start: start, end: end}), nil
}

// expandAll replaces each object's calendar with its instances in the window
// the request carries, if it carries one.
func expandAll(ctx context.Context, objects []caldav.CalendarObject) error {
	w, ok := ctx.Value(expandKey{}).(componentWindow)
	if !ok {
		return nil
	}
	for i := range objects {
		expanded, err := calendar.Expand(objects[i].Data, w.start, w.end)
		if err != nil {
			return webdav.NewHTTPError(http.StatusBadRequest, err)
		}
		objects[i].Data = expanded
	}
	return nil
}
