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
	"strconv"
	"strings"

	"github.com/emersion/go-ical"

	"github.com/C0piIot/stratus-backend/internal/calendar"
	"github.com/C0piIot/stratus-backend/internal/db"
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
)

// syncTokenPrefix makes a token a URI, which RFC 6578 asks for. The calendar's
// id is in it so that one calendar's token is not read as another's.
const syncTokenPrefix = "urn:stratus:sync:" //nolint:gosec // a sync token is a counter, not a credential

func syncToken(c db.Calendar) string {
	return fmt.Sprintf("%s%d:%d", syncTokenPrefix, c.ID, c.SyncToken)
}

// parseSyncToken reads a token back, or answers false for one this server did
// not write for that calendar. An empty token is a first sync.
func parseSyncToken(token string, calendarID int64) (int64, bool) {
	if token == "" {
		return 0, true
	}
	rest, ok := strings.CutPrefix(token, syncTokenPrefix)
	if !ok {
		return 0, false
	}
	id, n, ok := strings.Cut(rest, ":")
	if !ok || id != strconv.FormatInt(calendarID, 10) {
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

	ms := syncMultistatus{SyncToken: syncToken(c)}
	base := b.home(owner) + name + "/"
	for _, ch := range changes {
		href := hrefOf(base + ch.Name)
		if ch.Deleted {
			ms.Responses = append(ms.Responses, syncResponse{Hrefs: []string{href}, Status: statusLine(http.StatusNotFound)})
			continue
		}
		o, err := b.cal.Object(ctx, owner, name, ch.Name)
		if errors.Is(err, db.ErrNotFound) {
			// Deleted between the two reads; the next sync says so.
			continue
		}
		if err != nil {
			return mapCalendarErr(ctx, err)
		}
		ms.Responses = append(ms.Responses, objectResponse(href, o, req.Prop))
	}
	return writeMultistatus(w, ms)
}

// objectResponse is one changed object with the properties asked for: the ones
// a client syncing actually asks for are here, and anything else is a 404 in
// its own propstat, as RFC 4918 has it.
func objectResponse(href string, o db.CalendarObject, asked *namesOnly) syncResponse {
	found, missing := syncProp{}, syncProp{}
	if asked == nil {
		asked = &namesOnly{}
	}
	for _, n := range asked.Names {
		switch n.XMLName {
		case xml.Name{Space: nsDAV, Local: "getetag"}:
			found.Props = append(found.Props, element(n.XMLName, string(escapeText(`"`+o.ETag+`"`))))
		case xml.Name{Space: nsDAV, Local: "getcontenttype"}:
			found.Props = append(found.Props, element(n.XMLName, ical.MIMEType))
		case xml.Name{Space: nsDAV, Local: "getcontentlength"}:
			found.Props = append(found.Props, element(n.XMLName, strconv.Itoa(len(o.Data))))
		case xml.Name{Space: nsDAV, Local: "getlastmodified"}:
			found.Props = append(found.Props, element(n.XMLName, o.Changed.UTC().Format(http.TimeFormat)))
		case xml.Name{Space: nsCalDAV, Local: "calendar-data"}:
			found.Props = append(found.Props, element(n.XMLName, string(escapeText(o.Data))))
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

// propfindExtras passes a PROPFIND to the library and, when it asked for any
// of the properties the library does not know -- the three sync-collection
// needs, and the color -- adds them to each calendar in its answer.
func (b *calendarBackend) propfindExtras(w http.ResponseWriter, r *http.Request, next http.Handler) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "cannot read the request", http.StatusBadRequest)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	var asked propfindBody
	if xml.Unmarshal(body, &asked) != nil {
		next.ServeHTTP(w, r)
		return
	}
	if !asked.Prop.has(propSyncToken) && !asked.Prop.has(propCTag) && !asked.Prop.has(propSupportedReport) &&
		!asked.Prop.has(propColor) {
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
		if err := b.addExtraProps(r, &ms.Responses[i]); err != nil {
			serveCalendarError(w, err)
			return
		}
	}
	if err := writeMultistatus(w, ms); err != nil {
		serveCalendarError(w, err)
	}
}

// addExtraProps moves those properties out of the 404 the library answered
// them with and into the 200, for a response that is a calendar.
func (b *calendarBackend) addExtraProps(r *http.Request, resp *syncResponse) error {
	if len(resp.Hrefs) != 1 {
		return nil
	}
	p, err := url.PathUnescape(resp.Hrefs[0])
	if err != nil || !strings.HasSuffix(p, "/") {
		return nil //nolint:nilerr // not a calendar, and not ours to judge
	}
	owner, name, _, err := b.split(r.Context(), p, 2)
	if err != nil {
		return nil //nolint:nilerr // the home set or the principal
	}
	c, err := b.cal.Calendar(r.Context(), owner, name)
	if err != nil {
		return mapCalendarErr(r.Context(), err)
	}
	values := map[xml.Name]string{
		propSyncToken:       string(escapeText(syncToken(c))),
		propCTag:            string(escapeText(syncToken(c))),
		propSupportedReport: supportedReports,
	}
	// A color nobody chose stays the 404 it was: a client then picks one.
	if c.Color != "" {
		values[propColor] = c.Color
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
		return nil
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
	return nil
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
