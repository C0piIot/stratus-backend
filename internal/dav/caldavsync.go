package dav

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-ical"

	"github.com/stratus-private-cloud/stratus-backend/internal/calendar"
	"github.com/stratus-private-cloud/stratus-backend/internal/db"
)

// sync-collection (RFC 6578) in front of the library, which answers neither
// the REPORT nor the properties a client looks for before it would send one
// (#75). Three things are here:
//
//   - The REPORT itself, answered from the counter and the tombstones the
//     schema keeps: what changed after a token, deletions as a 404 each.
//   - DAV:sync-token, CS:getctag and DAV:supported-report-set on a calendar,
//     spliced into the library's own PROPFIND answer rather than written
//     instead of it -- everything else about a calendar stays the library's.
//   - A token that cannot be answered, which is the valid-sync-token
//     precondition and a client's cue to list the calendar again.
//
// The XML is written here with types of its own, because the library's are in
// an internal package Go will not let this one import.

const (
	nsDAV    = "DAV:"
	nsCalDAV = "urn:ietf:params:xml:ns:caldav"
	// nsCS is Apple's calendarserver namespace, where getctag lives. Older
	// than sync-collection and still what several clients ask first.
	nsCS = "http://calendarserver.org/ns/"
	// nsApple is where calendar-color lives, which no RFC has and every
	// client reads.
	nsApple = "http://apple.com/ns/ical/"
)

var (
	propSyncToken       = xml.Name{Space: nsDAV, Local: "sync-token"}
	propCTag            = xml.Name{Space: nsCS, Local: "getctag"}
	propSupportedReport = xml.Name{Space: nsDAV, Local: "supported-report-set"}
	propColor           = xml.Name{Space: nsApple, Local: "calendar-color"}
	propCalendarData    = xml.Name{Space: nsCalDAV, Local: "calendar-data"}
)

// syncTokenPrefix makes a token a URI, which RFC 6578 asks for. The
// collection's id is in it so that one calendar's token is not read as
// another's. An address book's id can be a calendar's too, which costs
// nothing: a token is only ever read back by the collection it came from.
const syncTokenPrefix = "urn:stratus:sync:" //nolint:gosec // a sync token is a counter, not a credential

func syncToken(collectionID, token int64) string {
	return fmt.Sprintf("%s%d:%d", syncTokenPrefix, collectionID, token)
}

// parseSyncToken reads a token back, or answers false for one this server did
// not write for that collection. An empty token is a first sync.
func parseSyncToken(token string, collectionID int64) (int64, bool) {
	if token == "" {
		return 0, true
	}
	rest, ok := strings.CutPrefix(token, syncTokenPrefix)
	if !ok {
		return 0, false
	}
	id, n, ok := strings.Cut(rest, ":")
	if !ok || id != strconv.FormatInt(collectionID, 10) {
		return 0, false
	}
	v, err := strconv.ParseInt(n, 10, 64)
	return v, err == nil && v >= 0
}

type syncCollection struct {
	XMLName   xml.Name   `xml:"DAV: sync-collection"`
	SyncToken string     `xml:"DAV: sync-token"`
	SyncLevel string     `xml:"DAV: sync-level"`
	Prop      *namesOnly `xml:"DAV: prop"`
}

type propfindBody struct {
	XMLName xml.Name   `xml:"DAV: propfind"`
	Prop    *namesOnly `xml:"DAV: prop"`
}

type namesOnly struct {
	Names []struct {
		XMLName xml.Name
	} `xml:",any"`
}

func (n *namesOnly) has(name xml.Name) bool {
	if n == nil {
		return false
	}
	for _, e := range n.Names {
		if e.XMLName == name {
			return true
		}
	}
	return false
}

// The response side. Every element carries its namespace and inner XML is
// written as it is, which is how a property the library produced survives
// being parsed and written again: the library declares its namespaces on each
// element rather than with prefixes on the root.
type syncMultistatus struct {
	XMLName   xml.Name       `xml:"DAV: multistatus"`
	Responses []syncResponse `xml:"DAV: response"`
	SyncToken string         `xml:"DAV: sync-token,omitempty"`
	Rest      []rawXML       `xml:",any"`
}

type syncResponse struct {
	Hrefs     []string       `xml:"DAV: href"`
	Propstats []syncPropstat `xml:"DAV: propstat"`
	Status    string         `xml:"DAV: status,omitempty"`
	Rest      []rawXML       `xml:",any"`
}

type syncPropstat struct {
	Prop   syncProp `xml:"DAV: prop"`
	Status string   `xml:"DAV: status"`
	Rest   []rawXML `xml:",any"`
}

type syncProp struct {
	Props []rawXML `xml:",any"`
}

// rawXML keeps no attributes: the only ones a property in a multistatus has
// are its namespace declarations, which the encoder writes again from the
// name, and keeping them would write each one twice.
type rawXML struct {
	XMLName xml.Name
	Inner   []byte `xml:",innerxml"`
}

func escapeText(s string) []byte {
	var buf bytes.Buffer
	_ = xml.EscapeText(&buf, []byte(s))
	return buf.Bytes()
}

func element(name xml.Name, inner string) rawXML {
	return rawXML{XMLName: name, Inner: []byte(inner)}
}

func writeMultistatus(w http.ResponseWriter, ms syncMultistatus) error {
	body, err := xml.Marshal(ms)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	_, err = w.Write(append([]byte(xml.Header), body...))
	return err
}

func hrefOf(p string) string { return (&url.URL{Path: p}).EscapedPath() }

// serveSync answers a sync-collection REPORT on a calendar.
func (b *calendarBackend) serveSync(w http.ResponseWriter, r *http.Request, req syncCollection) error {
	ctx := r.Context()
	owner, name, _, err := b.split(ctx, r.URL.Path, 2)
	if err != nil {
		return err
	}
	// Infinite depth is a request for members of members, and a calendar has
	// none to give.
	if req.SyncLevel != "1" {
		return precondition(http.StatusForbidden, nsDAV, "sync-level")
	}
	c, err := b.cal.Calendar(ctx, owner, name)
	if err != nil {
		return mapCalendarErr(ctx, err)
	}
	since, ok := parseSyncToken(req.SyncToken, c.ID)
	if !ok {
		return precondition(http.StatusForbidden, nsDAV, "valid-sync-token")
	}
	changes, c, err := b.cal.Changes(ctx, owner, name, since)
	if errors.Is(err, calendar.ErrStaleToken) {
		return precondition(http.StatusForbidden, nsDAV, "valid-sync-token")
	}
	if err != nil {
		return mapCalendarErr(ctx, err)
	}
	return writeSyncFeed(w, syncFeed{
		base: b.home(owner) + name + "/", changes: changes, token: syncToken(c.ID, c.SyncToken),
		item: func(n string) (syncItem, error) {
			o, err := b.cal.Object(ctx, owner, name, n)
			if err != nil {
				return syncItem{}, err
			}
			return syncItem{etag: o.ETag, data: o.Data, changed: o.Changed, dataName: propCalendarData, mime: ical.MIMEType}, nil
		},
	}, req.Prop, func(err error) error { return mapCalendarErr(ctx, err) })
}

// syncFeed is one collection's answer to sync-collection, whatever the
// collection holds: where its members are, what changed in it, the token to
// come back with, and how to read one member.
type syncFeed struct {
	base    string
	changes []db.CalendarChange
	token   string
	item    func(name string) (syncItem, error)
}

// syncItem is a member as a sync answer describes it.
type syncItem struct {
	etag, data string
	changed    time.Time
	// dataName and mime are the member's text as a property and as a type:
	// calendar-data and iCalendar, or address-data and vCard.
	dataName xml.Name
	mime     string
}

// writeSyncFeed writes what changed, a deletion as a 404 each.
func writeSyncFeed(w http.ResponseWriter, f syncFeed, asked *namesOnly, mapErr func(error) error) error {
	ms := syncMultistatus{SyncToken: f.token}
	for _, ch := range f.changes {
		href := hrefOf(f.base + ch.Name)
		if ch.Deleted {
			ms.Responses = append(ms.Responses, syncResponse{Hrefs: []string{href}, Status: statusLine(http.StatusNotFound)})
			continue
		}
		it, err := f.item(ch.Name)
		if errors.Is(err, db.ErrNotFound) {
			// Deleted between the two reads; the next sync says so.
			continue
		}
		if err != nil {
			return mapErr(err)
		}
		ms.Responses = append(ms.Responses, itemResponse(href, it, asked))
	}
	return writeMultistatus(w, ms)
}

// itemResponse is one changed member with the properties asked for: the ones
// a client syncing actually asks for are here, and anything else is a 404 in
// its own propstat, as RFC 4918 has it.
func itemResponse(href string, it syncItem, asked *namesOnly) syncResponse {
	found, missing := syncProp{}, syncProp{}
	if asked == nil {
		asked = &namesOnly{}
	}
	for _, n := range asked.Names {
		switch n.XMLName {
		case xml.Name{Space: nsDAV, Local: "getetag"}:
			found.Props = append(found.Props, element(n.XMLName, string(escapeText(`"`+it.etag+`"`))))
		case xml.Name{Space: nsDAV, Local: "getcontenttype"}:
			found.Props = append(found.Props, element(n.XMLName, it.mime))
		case xml.Name{Space: nsDAV, Local: "getcontentlength"}:
			found.Props = append(found.Props, element(n.XMLName, strconv.Itoa(len(it.data))))
		case xml.Name{Space: nsDAV, Local: "getlastmodified"}:
			found.Props = append(found.Props, element(n.XMLName, it.changed.UTC().Format(http.TimeFormat)))
		case it.dataName:
			found.Props = append(found.Props, element(n.XMLName, string(escapeText(it.data))))
		default:
			missing.Props = append(missing.Props, rawXML{XMLName: n.XMLName})
		}
	}
	out := syncResponse{Hrefs: []string{href}}
	if len(found.Props) > 0 || len(missing.Props) == 0 {
		out.Propstats = append(out.Propstats, syncPropstat{Prop: found, Status: statusLine(http.StatusOK)})
	}
	if len(missing.Props) > 0 {
		out.Propstats = append(out.Propstats, syncPropstat{Prop: missing, Status: statusLine(http.StatusNotFound)})
	}
	return out
}

// supportedReports is what a calendar answers, which is how a client learns
// that sync-collection is worth sending.
const supportedReports = `<supported-report xmlns="DAV:"><report><calendar-query xmlns="urn:ietf:params:xml:ns:caldav"/></report></supported-report>` +
	`<supported-report xmlns="DAV:"><report><calendar-multiget xmlns="urn:ietf:params:xml:ns:caldav"/></report></supported-report>` +
	`<supported-report xmlns="DAV:"><report><sync-collection/></report></supported-report>`

// calendarExtras is what a calendar adds to the library's PROPFIND: the
// three properties sync-collection needs, and the color.
var calendarExtras = []xml.Name{propSyncToken, propCTag, propSupportedReport, propColor}

// propfindExtras passes a PROPFIND to the library and, when it asked for any
// of the properties wanted -- ones the library does not know -- adds them to
// each response valuesFor has values for.
func propfindExtras(w http.ResponseWriter, r *http.Request, next http.Handler, wanted []xml.Name,
	valuesFor func(r *http.Request, p string) (map[xml.Name]string, error),
) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "cannot read the request", http.StatusBadRequest)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	var asked propfindBody
	if xml.Unmarshal(body, &asked) != nil || !slices.ContainsFunc(wanted, asked.Prop.has) {
		next.ServeHTTP(w, r)
		return
	}

	rec := httptest.NewRecorder()
	next.ServeHTTP(rec, r)
	var ms syncMultistatus
	if rec.Code != http.StatusMultiStatus || xml.Unmarshal(rec.Body.Bytes(), &ms) != nil {
		copyRecorded(w, rec)
		return
	}
	for i := range ms.Responses {
		resp := &ms.Responses[i]
		if len(resp.Hrefs) != 1 {
			continue
		}
		p, err := url.PathUnescape(resp.Hrefs[0])
		if err != nil || !strings.HasSuffix(p, "/") {
			continue
		}
		values, err := valuesFor(r, p)
		if err != nil {
			serveCalendarError(w, err)
			return
		}
		spliceProps(resp, values)
	}
	if err := writeMultistatus(w, ms); err != nil {
		serveCalendarError(w, err)
	}
}

// calendarValues are a calendar's extra properties, and none for a path that
// is not a calendar: the home set or the principal.
func (b *calendarBackend) calendarValues(r *http.Request, p string) (map[xml.Name]string, error) {
	owner, name, _, err := b.split(r.Context(), p, 2)
	if err != nil {
		return nil, nil //nolint:nilerr // not a calendar, and not ours to judge
	}
	c, err := b.cal.Calendar(r.Context(), owner, name)
	if err != nil {
		return nil, mapCalendarErr(r.Context(), err)
	}
	token := string(escapeText(syncToken(c.ID, c.SyncToken)))
	values := map[xml.Name]string{
		propSyncToken:       token,
		propCTag:            token,
		propSupportedReport: supportedReports,
	}
	// A color nobody chose stays the 404 it was: a client then picks one.
	if c.Color != "" {
		values[propColor] = c.Color
	}
	return values, nil
}

// spliceProps moves the properties values has out of the 404 the library
// answered them with and into the 200.
func spliceProps(resp *syncResponse, values map[xml.Name]string) {
	if len(values) == 0 {
		return
	}
	var ours []rawXML
	for i := range resp.Propstats {
		ps := &resp.Propstats[i]
		if !strings.Contains(ps.Status, " 404 ") {
			continue
		}
		kept := ps.Prop.Props[:0]
		for _, e := range ps.Prop.Props {
			if v, ok := values[e.XMLName]; ok {
				ours = append(ours, element(e.XMLName, v))
				continue
			}
			kept = append(kept, e)
		}
		ps.Prop.Props = kept
	}
	if len(ours) == 0 {
		return
	}

	propstats := resp.Propstats[:0]
	added := false
	for _, ps := range resp.Propstats {
		if strings.Contains(ps.Status, " 200 ") && !added {
			ps.Prop.Props = append(ps.Prop.Props, ours...)
			added = true
		}
		if len(ps.Prop.Props) > 0 {
			propstats = append(propstats, ps)
		}
	}
	if !added {
		propstats = append(propstats, syncPropstat{Prop: syncProp{Props: ours}, Status: statusLine(http.StatusOK)})
	}
	resp.Propstats = propstats
}

func copyRecorded(w http.ResponseWriter, rec *httptest.ResponseRecorder) {
	for k, v := range rec.Header() {
		w.Header()[k] = v
	}
	w.WriteHeader(rec.Code)
	_, _ = w.Write(rec.Body.Bytes())
}

// preconditionError is a refusal carrying the element that says why, written
// by this package rather than by the library.
type preconditionError struct {
	code        int
	space, name string
}

func (e *preconditionError) Error() string { return "dav: " + e.name }

func precondition(code int, space, name string) error {
	return &preconditionError{code: code, space: space, name: name}
}

// serveCalendarError writes what the requests this package answers in front
// of the library are refused with.
func serveCalendarError(w http.ResponseWriter, err error) {
	var pe *preconditionError
	if errors.As(err, &pe) {
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.WriteHeader(pe.code)
		_, _ = fmt.Fprintf(w, `%s<error xmlns="DAV:"><%s xmlns="%s"/></error>`, xml.Header, pe.name, pe.space)
		return
	}
	http.Error(w, err.Error(), statusOf(err))
}

// statusOf reads the code off one of the library's errors, whose type is in a
// package this one cannot import and whose message is "<code> <text>: ...".
// Anything else is a 500.
func statusOf(err error) int {
	code, _, _ := strings.Cut(err.Error(), " ")
	if n, convErr := strconv.Atoi(code); convErr == nil && n >= 400 && n < 600 {
		return n
	}
	return http.StatusInternalServerError
}
