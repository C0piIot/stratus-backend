package dav

import (
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/C0piIot/stratus-backend/internal/calendar"
	"github.com/C0piIot/stratus-backend/internal/db"
)

// Making, changing and removing a calendar (#399), the three requests about a
// calendar itself that the library either does not route -- MKCALENDAR, which
// is what DAVx5 sends -- or routes somewhere wrong: a PROPPATCH is a 501
// there, and a DELETE of a calendar reaches the backend as the deletion of an
// object whose path has one segment too few.

// calendarProps is what a client may say about a calendar, in an MKCALENDAR
// or a PROPPATCH alike.
type calendarProps struct {
	DisplayName *string     `xml:"DAV: displayname"`
	Description *string     `xml:"urn:ietf:params:xml:ns:caldav calendar-description"`
	Color       *string     `xml:"http://apple.com/ns/ical/ calendar-color"`
	Components  *components `xml:"urn:ietf:params:xml:ns:caldav supported-calendar-component-set"`
}

type components struct {
	Comps []struct {
		Name string `xml:"name,attr"`
	} `xml:"urn:ietf:params:xml:ns:caldav comp"`
}

type mkcalendarBody struct {
	XMLName xml.Name `xml:"urn:ietf:params:xml:ns:caldav mkcalendar"`
	Set     []struct {
		Prop calendarProps `xml:"DAV: prop"`
	} `xml:"DAV: set"`
}

func (b *calendarBackend) isCalendar(p string) bool {
	rest, ok := strings.CutPrefix(p, b.prefix)
	return ok && strings.Count(strings.Trim(rest, "/"), "/") == 1
}

// serve writes the refusal a handler here returned, if it returned one; on
// success it has written its own answer.
func serve(w http.ResponseWriter, err error) {
	if err != nil {
		serveCalendarError(w, err)
	}
}

// mkcalendar makes a calendar (RFC 4791 5.3.1).
//
// A property it does not keep -- a time zone, an order -- is taken and
// dropped rather than refused. The RFC would have the whole request fail for
// one, and DAVx5 sends a time zone with every calendar it makes: refusing it
// would be refusing the client this exists for.
func (b *calendarBackend) mkcalendar(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	if !b.isCalendar(r.URL.Path) {
		return precondition(http.StatusForbidden, nsCalDAV, "calendar-collection-location-ok")
	}
	owner, name, _, err := b.split(ctx, r.URL.Path, 2)
	if err != nil {
		return err
	}
	var body mkcalendarBody
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return precondition(http.StatusBadRequest, nsDAV, "unreadable-body")
	}
	if len(strings.TrimSpace(string(raw))) > 0 && xml.Unmarshal(raw, &body) != nil {
		return precondition(http.StatusBadRequest, nsCalDAV, "mkcalendar")
	}

	c := db.Calendar{Name: name}
	for _, set := range body.Set {
		p := set.Prop
		if p.DisplayName != nil {
			c.DisplayName = *p.DisplayName
		}
		if p.Description != nil {
			c.Description = *p.Description
		}
		if p.Color != nil {
			c.Color = strings.TrimSpace(*p.Color)
		}
		if p.Components != nil && !offersEvents(p.Components) {
			return precondition(http.StatusForbidden, nsCalDAV, "supported-calendar-component")
		}
	}

	_, err = b.cal.Create(ctx, owner, c)
	switch {
	case errors.Is(err, db.ErrConflict):
		return precondition(http.StatusMethodNotAllowed, nsDAV, "resource-must-be-null")
	case errors.Is(err, calendar.ErrColor):
		return precondition(http.StatusForbidden, nsApple, "calendar-color")
	case err != nil:
		return mapCalendarErr(ctx, err)
	}
	w.WriteHeader(http.StatusCreated)
	return nil
}

// offersEvents is whether a calendar asked for can hold what this server
// stores. One that also wants tasks is made, and refuses them one at a time
// as they arrive; one that wants only tasks would be a calendar nothing can
// ever be put in.
func offersEvents(c *components) bool {
	for _, comp := range c.Comps {
		if comp.Name == calendar.Component {
			return true
		}
	}
	return false
}

// propertyUpdate is a PROPPATCH's body: sets and removes, in order.
type propertyUpdate struct {
	XMLName xml.Name    `xml:"DAV: propertyupdate"`
	Changes []patchItem `xml:",any"`
}

type patchItem struct {
	XMLName xml.Name
	Prop    struct {
		Props []struct {
			XMLName xml.Name
			Value   string `xml:",chardata"`
		} `xml:",any"`
	} `xml:"DAV: prop"`
}

var (
	propDisplayName = xml.Name{Space: nsDAV, Local: "displayname"}
	propDescription = xml.Name{Space: nsCalDAV, Local: "calendar-description"}
)

// proppatch changes a calendar's name, description and color (RFC 4918 9.2).
// It is all or nothing, as the RFC has it: a property that cannot be changed
// is a 403 and every other one in the request a 424, and nothing is written.
func (b *calendarBackend) proppatch(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	owner, name, _, err := b.split(ctx, r.URL.Path, 2)
	if err != nil {
		return err
	}
	var body propertyUpdate
	if err = xml.NewDecoder(r.Body).Decode(&body); err != nil {
		return precondition(http.StatusBadRequest, nsDAV, "propertyupdate")
	}

	var patch calendar.Patch
	var named, refused []xml.Name
	for _, change := range body.Changes {
		remove := change.XMLName == xml.Name{Space: nsDAV, Local: "remove"}
		if !remove && change.XMLName != (xml.Name{Space: nsDAV, Local: "set"}) {
			continue
		}
		for _, p := range change.Prop.Props {
			value := p.Value
			if remove {
				value = ""
			}
			named = append(named, p.XMLName)
			switch p.XMLName {
			case propDisplayName:
				patch.DisplayName = &value
			case propDescription:
				patch.Description = &value
			case propColor:
				value = strings.TrimSpace(value)
				if !calendar.ValidColor(value) {
					refused = append(refused, p.XMLName)
				}
				patch.Color = &value
			default:
				refused = append(refused, p.XMLName)
			}
		}
	}

	if len(refused) == 0 {
		if _, err = b.cal.Update(ctx, owner, name, patch); err != nil {
			return mapCalendarErr(ctx, err)
		}
	}
	return writeMultistatus(w, syncMultistatus{Responses: []syncResponse{
		patchResponse(hrefOf(r.URL.Path), named, refused),
	}})
}

// patchResponse is one propstat per outcome: every property 200 when nothing
// was refused, and otherwise the refused ones 403 and the rest 424.
func patchResponse(href string, named, refused []xml.Name) syncResponse {
	isRefused := func(n xml.Name) bool {
		for _, r := range refused {
			if r == n {
				return true
			}
		}
		return false
	}
	byStatus := map[int]*syncProp{}
	var order []int
	for _, n := range named {
		code := http.StatusOK
		switch {
		case isRefused(n):
			code = http.StatusForbidden
		case len(refused) > 0:
			code = http.StatusFailedDependency
		}
		if byStatus[code] == nil {
			byStatus[code] = &syncProp{}
			order = append(order, code)
		}
		byStatus[code].Props = append(byStatus[code].Props, rawXML{XMLName: n})
	}
	out := syncResponse{Hrefs: []string{href}}
	for _, code := range order {
		out.Propstats = append(out.Propstats, syncPropstat{Prop: *byStatus[code], Status: statusLine(code)})
	}
	return out
}

// deleteCalendar removes a calendar and everything in it.
func (b *calendarBackend) deleteCalendar(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	owner, name, _, err := b.split(ctx, r.URL.Path, 2)
	if err != nil {
		return err
	}
	switch err = b.cal.DeleteCalendar(ctx, owner, name); {
	case errors.Is(err, calendar.ErrDefault):
		return precondition(http.StatusForbidden, nsDAV, "cannot-modify-protected-property")
	case err != nil:
		return mapCalendarErr(ctx, err)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
