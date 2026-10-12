package dav

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/C0piIot/stratus-backend/internal/auth"
	"github.com/C0piIot/stratus-backend/internal/db"
)

// SEARCH, RFC 5323, which is the only standard way to ask a WebDAV server to
// find something (#327).
//
// The library is searchable from the web UI and over OpenSubsonic and was not
// searchable over WebDAV at all, so no WebDAV client could search it -- and
// the client this project has is the one that most needs to, since ten years
// of photographs is not a tree anybody walks. The alternative was an endpoint
// of our own, which is the private API principle 2 forbids; this is the
// standard that already exists for it.
//
// **It answers from db.Finder and adds nothing to that port.** The Files
// bucket already promises what this surface needs -- a whole word of a name,
// several words as a phrase, ordered by path -- and the three engines already
// carry the index for it. The one thing written here is the protocol.
//
// Two deviations are deliberate and both are the RFC's to allow:
//
//   - **DAV:contains matches the name, not the content.** 5.16 says the
//     matching is "at the discretion of the server implementation" and
//     deliberately does not constrain it; our discretion is what db.Finder
//     promises, which is the name. There is no index of what is *inside* a
//     file here, so a word in a PDF finds nothing -- which is a smaller lie
//     than DAV:like would be, since that one means substrings and wildcards
//     and the port refuses both by contract.
//   - **The scope is a collection and nothing else.** The whole tree, or any
//     folder in it (#329); a scope naming anything outside -- a generated
//     collection, a path above -- is refused rather than silently widened.
//     What a narrower one buys is a smaller answer rather than a faster
//     search: db.Finder's index is on words and the scope is a predicate on
//     what it hands back, so a broad term costs the same in a small folder.
//
// There is no cursor, because DASL has none and inventing one would be an
// extension a client would have to know about. The limit is honoured, the
// RFC's 507 says the answer was cut, and the answer to a hundred of the wrong
// thing is a better term rather than a second page -- which is the same thing
// the web UI's own search decided.
const searchGrammar = "DAV:basicsearch"

// maxSearchResults caps what one SEARCH answers however large a DAV:limit is.
//
// 5.17 says a server MAY disregard the client's number, and it has to here:
// the ordering is the path and not relevance, so "more rows" is not a better
// answer to a vague term, it is the same answer for longer. The listing's own
// ceiling is 500 rows and this is lower on purpose -- a search is read.
const maxSearchResults = 200

// searchDepth is the only scope depth this answers. The others are legal and
// are refused: "this collection and everything under it" is what db.Finder
// searches, and depth 0 or 1 would be a second shape of query -- a parent and
// not a prefix -- for a client that has not asked for one.
const searchDepth = "infinity"

// Searchable answers SEARCH, and is mounted twice.
//
// arbiter is the URL it answers at, which is what a scope has to name: the
// origin is one search arbiter and the user's tree is another, and both search
// the same library because the only resources with text in them are the files.
// collection is the prefix every href is built with, which is the tree's
// wherever the question was asked.
//
// Separate from the fileSystem above because it needs none of it -- no blob
// store, no locks, no paths to resolve -- and because the root collection has
// no fileSystem at all.
func Searchable(arbiter, collection string, finder db.Finder) http.Handler {
	return &searcher{
		arbiter:    "/" + strings.Trim(arbiter, "/"),
		collection: strings.TrimSuffix(collection, "/"),
		finder:     finder,
	}
}

type searcher struct {
	arbiter    string
	collection string
	finder     db.Finder
}

func (f *searcher) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	owner, ok := auth.User(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, searchBodyLimit))
	if err != nil {
		writeFault(w, searchFault{status: http.StatusBadRequest, reason: "the query could not be read"})
		return
	}

	query, fault := parseSearch(body, f.arbiter, f.collection)
	if fault != nil {
		writeFault(w, *fault)
		return
	}

	found, err := f.finder.Find(r.Context(), owner, db.FindFilter{
		Text:  query.text,
		Under: query.under,
		// One more than asked for, so the answer knows it was cut without a
		// second query -- the same trick the web UI's own search plays.
		Files: db.Window{Limit: query.limit + 1},
	})
	if err != nil {
		writeFault(w, searchFault{status: http.StatusInternalServerError, reason: "the search could not be run"})
		return
	}

	rows := found.Files
	truncated := len(rows) > query.limit
	if truncated {
		rows = rows[:query.limit]
	}
	f.writeResults(r.Context(), w, query, rows, truncated)
}

// searchBodyLimit is how much query this will read. A basicsearch is a few
// hundred bytes and anything larger is a mistake or a probe.
const searchBodyLimit = 64 << 10

// searchQuery is a DAV:basicsearch reduced to what this server can answer.
type searchQuery struct {
	text string
	// under is the directory the scope named, "" being the whole collection.
	// It is a path in the library, so the collection's prefix is already off
	// the front of it.
	under string
	limit int
	// props is what DAV:select asked for, already in the order it asked. Empty
	// means DAV:allprop, which here is every property this surface knows.
	props []xml.Name
}

// arbiterHref is the collection the question was asked of, as an href. The
// origin is already "/"; anything else needs the slash a collection carries.
func arbiterHref(arbiter string) string {
	if strings.HasSuffix(arbiter, "/") {
		return arbiter
	}
	return arbiter + "/"
}

// parseSearch reads a DAV:searchrequest, or says why it will not.
//
// Every refusal names a condition rather than failing vaguely: a client that
// sent an operator this does not have needs to know which one, and 2.4 is
// where the RFC asks for a body saying so.
func parseSearch(body []byte, arbiter, collection string) (searchQuery, *searchFault) {
	var request searchRequest
	if err := xml.Unmarshal(body, &request); err != nil {
		return searchQuery{}, &searchFault{
			status: http.StatusBadRequest,
			reason: "the query is not valid XML",
		}
	}
	if request.Basic == nil {
		// Whatever grammar that was, it is not one this server has. 3.2 is the
		// discovery that should have prevented it; the DASL header on OPTIONS
		// names the only one there is.
		return searchQuery{}, &searchFault{
			status:       http.StatusUnprocessableEntity,
			precondition: "search-grammar-supported",
			reason:       "this server answers " + searchGrammar + " and no other grammar",
		}
	}
	basic := request.Basic

	switch len(basic.From.Scopes) {
	case 1:
	case 0:
		return searchQuery{}, &searchFault{
			status:       http.StatusUnprocessableEntity,
			precondition: "search-scope-valid",
			reason:       "a search needs a scope to run in",
		}
	default:
		return searchQuery{}, &searchFault{
			status:       http.StatusUnprocessableEntity,
			precondition: "search-multiple-scope-supported",
			reason:       "one scope at a time",
		}
	}
	under, fault := scopeOf(basic.From.Scopes[0], arbiter, collection)
	if fault != nil {
		return searchQuery{}, fault
	}

	text, fault := conditionOf(basic.Where)
	if fault != nil {
		return searchQuery{}, fault
	}
	if fault := checkOrder(basic.OrderBy); fault != nil {
		return searchQuery{}, fault
	}

	query := searchQuery{text: text, under: under, limit: maxSearchResults}
	if basic.Limit != nil && basic.Limit.NResults > 0 && basic.Limit.NResults < maxSearchResults {
		query.limit = basic.Limit.NResults
	}
	if basic.Select.Prop != nil {
		query.props = basic.Select.Prop.Names
	} else if basic.Select.AllProp == nil {
		return searchQuery{}, &searchFault{
			status: http.StatusUnprocessableEntity,
			reason: "a search has to say what to select: DAV:prop or DAV:allprop",
		}
	}
	return query, nil
}

// scopeOf reads the scope as a directory in the library, "" being all of it.
//
// Three things are a scope: the arbiter the question was asked of, the
// collection itself -- which at the origin is /files/ and is the same answer
// -- and any collection under it, which is #329 and is what lets somebody
// search the folder they are standing in. Anything else is 409 with
// DAV:search-scope-valid, the shape B.3's own example uses: a generated
// collection has no rows behind it, and a path above the tree is not ours to
// search. Refused rather than widened, because answering a search of one
// folder with the whole library would be the worst of both.
//
// The href is parsed rather than trimmed because it is a URL: url.Parse
// decodes, so a folder with a space or an accent in its name arrives as its
// name and not as %20.
func scopeOf(s scope, arbiter, collection string) (string, *searchFault) {
	refuse := func(reason string) (string, *searchFault) {
		return "", &searchFault{
			status:       http.StatusConflict,
			precondition: "search-scope-valid",
			reason:       reason,
		}
	}

	href := s.Href
	if u, err := url.Parse(strings.TrimSpace(href)); err == nil {
		href = u.Path
	}
	if depth := strings.TrimSpace(s.Depth); depth != "" && depth != searchDepth {
		return refuse("the scope is searched at depth " + searchDepth)
	}

	here := path.Clean("/" + strings.Trim(arbiter, "/"))
	tree := path.Clean("/" + strings.Trim(collection, "/"))
	asked := path.Clean("/" + strings.Trim(href, "/"))
	if asked == here || asked == tree {
		return "", nil
	}

	// The root collection is its own prefix, so it is taken off as nothing.
	prefix := strings.TrimSuffix(tree, "/")
	if !strings.HasPrefix(asked, prefix+"/") {
		return refuse("this server searches " + arbiterHref(tree) + " and the collections under it")
	}
	under := strings.TrimPrefix(asked, prefix+"/")
	if err := db.ValidateDir(under); err != nil {
		return refuse("that is not a path this server can search")
	}
	return under, nil
}

// conditionOf reduces DAV:where to the one operator this has.
//
// A where with nothing in it is every resource in scope, which is a listing
// and is what PROPFIND is for; it is refused rather than answered with the
// library. An empty DAV:contains matches nothing by db.Finder's own contract,
// and is refused here for the same reason -- a person with an empty box is
// asking for nothing, not for everything.
func conditionOf(where *whereClause) (string, *searchFault) {
	unsupported := func(what string) *searchFault {
		return &searchFault{
			status: http.StatusUnprocessableEntity,
			reason: "this server searches with DAV:contains, not " + what,
		}
	}
	if where == nil {
		return "", unsupported("a query with no condition in it")
	}
	if len(where.Other) > 0 {
		return "", unsupported("DAV:" + where.Other[0].XMLName.Local)
	}
	if where.Contains == nil {
		return "", unsupported("an empty condition")
	}
	text := strings.TrimSpace(*where.Contains)
	if text == "" {
		return "", &searchFault{
			status: http.StatusUnprocessableEntity,
			reason: "DAV:contains needs something to look for",
		}
	}
	return text, nil
}

// checkOrder refuses an ordering this cannot give.
//
// db.Finder orders by path and says so, because relevance differs per engine
// and has no stable key to resume from. An orderby asking for anything else --
// DAV:score above all, which is a ranking nobody here computes -- is refused
// rather than ignored, since a client that sorted the answer itself would at
// least know it had to.
func checkOrder(order *orderBy) *searchFault {
	if order == nil {
		return nil
	}
	for _, o := range order.Orders {
		if o.Descending != nil {
			return &searchFault{
				status: http.StatusUnprocessableEntity,
				reason: "results are ordered by path, ascending",
			}
		}
		if o.Score != nil || o.Prop == nil || len(o.Prop.Names) != 1 ||
			(o.Prop.Names[0] != displayNameProp && o.Prop.Names[0] != hrefProp) {
			return &searchFault{
				status: http.StatusUnprocessableEntity,
				reason: "results are ordered by path, which is DAV:displayname here",
			}
		}
	}
	return nil
}

// The request, as the RFC spells it. Only what is read is declared: an element
// this server does not know is either caught by the `,any` beside it or is one
// it was always allowed to ignore.
type searchRequest struct {
	XMLName xml.Name     `xml:"DAV: searchrequest"`
	Basic   *basicSearch `xml:"DAV: basicsearch"`
}

type basicSearch struct {
	Select  selectClause `xml:"DAV: select"`
	From    fromClause   `xml:"DAV: from"`
	Where   *whereClause `xml:"DAV: where"`
	OrderBy *orderBy     `xml:"DAV: orderby"`
	Limit   *limitClause `xml:"DAV: limit"`
}

type selectClause struct {
	AllProp *struct{}  `xml:"DAV: allprop"`
	Prop    *propNames `xml:"DAV: prop"`
}

type propNames struct {
	Names []xml.Name `xml:",any"`
}

type fromClause struct {
	Scopes []scope `xml:"DAV: scope"`
}

type scope struct {
	Href  string `xml:"DAV: href"`
	Depth string `xml:"DAV: depth"`
}

// whereClause carries the one operator this answers and whatever else was
// sent, so that an unsupported one can be named in the refusal instead of
// being dropped silently.
type whereClause struct {
	Contains *string  `xml:"DAV: contains"`
	Other    []anyXML `xml:",any"`
}

type anyXML struct {
	XMLName xml.Name
}

type orderBy struct {
	Orders []order `xml:"DAV: order"`
}

type order struct {
	Prop       *propNames `xml:"DAV: prop"`
	Score      *struct{}  `xml:"DAV: score"`
	Ascending  *struct{}  `xml:"DAV: ascending"`
	Descending *struct{}  `xml:"DAV: descending"`
}

type limitClause struct {
	NResults int `xml:"DAV: nresults"`
}

var (
	displayNameProp   = xml.Name{Space: "DAV:", Local: "displayname"}
	hrefProp          = xml.Name{Space: "DAV:", Local: "href"}
	resourceTypeProp  = xml.Name{Space: "DAV:", Local: "resourcetype"}
	contentLengthProp = xml.Name{Space: "DAV:", Local: "getcontentlength"}
	contentTypeProp   = xml.Name{Space: "DAV:", Local: "getcontenttype"}
	etagProp          = xml.Name{Space: "DAV:", Local: "getetag"}
	lastModifiedProp  = xml.Name{Space: "DAV:", Local: "getlastmodified"}
)

// known is every property this surface answers, which is the five a listing
// asks for plus the name. Everything else is reported as absent rather than
// invented, which is what a 404 propstat means.
var known = []xml.Name{
	resourceTypeProp, displayNameProp, contentLengthProp,
	contentTypeProp, etagProp, lastModifiedProp,
}

// writeResults sends the multistatus.
//
// **The values come from rowInfo**, which is what PROPFIND answers from
// (propfind.go), so the two surfaces cannot disagree about a file's validator
// or its type -- which is the whole reason that type is shared rather than
// this building its own. What is not shared is the encoder: x/net builds a
// child's href by joining the parent with the entry's name, and a search
// result is not under the collection it was found from, so its walker cannot
// be borrowed.
func (f *searcher) writeResults(
	ctx context.Context,
	w http.ResponseWriter,
	query searchQuery,
	rows []db.File,
	truncated bool,
) {
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)

	enc := xml.NewEncoder(w)
	start := xml.StartElement{Name: xml.Name{Space: "DAV:", Local: "multistatus"}}
	if err := enc.EncodeToken(start); err != nil {
		return
	}
	for _, row := range rows {
		if err := enc.Encode(f.responseFor(ctx, query, row)); err != nil {
			return
		}
	}
	if truncated {
		// 2.3.1: the arbiter's own href with 507 on it is how a server says it
		// stopped early, and the partial results above it are what the RFC
		// asks to be sent anyway.
		if err := enc.Encode(searchResponse{
			Href:   arbiterHref(f.arbiter),
			Status: statusLine(http.StatusInsufficientStorage),
			Description: fmt.Sprintf(
				"More than %d files match. The results are the first by path; a narrower search finds fewer.",
				query.limit),
		}); err != nil {
			return
		}
	}
	if err := enc.EncodeToken(start.End()); err != nil {
		return
	}
	_ = enc.Flush()
}

// responseFor is one found file, described the way a PROPFIND describes it.
func (f *searcher) responseFor(ctx context.Context, query searchQuery, row db.File) searchResponse {
	info := rowInfo{row: row}
	wanted := query.props
	if len(wanted) == 0 {
		wanted = known
	}

	found := propstat{Status: statusLine(http.StatusOK)}
	absent := propstat{Status: statusLine(http.StatusNotFound)}
	for _, name := range wanted {
		value, ok := valueOf(ctx, info, name)
		if !ok {
			absent.Prop.Any = append(absent.Prop.Any, anyProperty{XMLName: name})
			continue
		}
		found.Prop.Any = append(found.Prop.Any, value)
	}

	response := searchResponse{Href: href(f.collection, row)}
	if len(found.Prop.Any) > 0 {
		response.Propstats = append(response.Propstats, found)
	}
	if len(absent.Prop.Any) > 0 {
		response.Propstats = append(response.Propstats, absent)
	}
	return response
}

// valueOf answers one property about one row, or says it has none.
func valueOf(ctx context.Context, info rowInfo, name xml.Name) (anyProperty, bool) {
	prop := anyProperty{XMLName: name}
	switch name {
	case resourceTypeProp:
		if info.IsDir() {
			prop.Inner = []byte(`<collection xmlns="DAV:"/>`)
		}
		return prop, true
	case displayNameProp:
		prop.Value = info.Name()
		return prop, true
	case contentLengthProp:
		if info.IsDir() {
			return prop, false
		}
		prop.Value = strconv.FormatInt(info.Size(), 10)
		return prop, true
	case contentTypeProp:
		if info.IsDir() {
			return prop, false
		}
		kind, err := info.ContentType(ctx)
		if err != nil || kind == "" {
			return prop, false
		}
		prop.Value = kind
		return prop, true
	case etagProp:
		tag, err := info.ETag(ctx)
		if err != nil || tag == "" {
			return prop, false
		}
		// Already quoted by ETag, which is the form the header wants; the
		// property carries the same string every other surface sends.
		prop.Value = tag
		return prop, true
	case lastModifiedProp:
		prop.Value = info.ModTime().UTC().Format(http.TimeFormat)
		return prop, true
	}
	return prop, false
}

// href is where a found file lives, which is the collection's prefix and the
// row's path with each segment escaped. A collection ends in a slash, as
// RFC 4918's own examples do and as PROPFIND here has since #136.
func href(prefix string, row db.File) string {
	escaped := (&url.URL{Path: "/" + row.Path}).EscapedPath()
	if row.IsDir {
		escaped += "/"
	}
	return prefix + escaped
}

func statusLine(code int) string {
	return fmt.Sprintf("HTTP/1.1 %d %s", code, http.StatusText(code))
}

// The response, which is RFC 4918's multistatus and not a shape of this
// server's own.
type searchResponse struct {
	XMLName     xml.Name   `xml:"DAV: response"`
	Href        string     `xml:"DAV: href"`
	Propstats   []propstat `xml:"DAV: propstat"`
	Status      string     `xml:"DAV: status,omitempty"`
	Description string     `xml:"DAV: responsedescription,omitempty"`
}

type propstat struct {
	Prop   propBag `xml:"DAV: prop"`
	Status string  `xml:"DAV: status"`
}

type propBag struct {
	Any []anyProperty
}

// anyProperty is one property written out: a value, or a fragment for the one
// property whose value is an element rather than a string.
type anyProperty struct {
	XMLName xml.Name
	Value   string `xml:",chardata"`
	Inner   []byte `xml:",innerxml"`
}

// searchFault is a refusal with a reason a client can act on.
type searchFault struct {
	status int
	// precondition is the local name of the DAV: element the RFC gives for
	// this refusal, where it gives one.
	precondition string
	reason       string
}

// writeFault answers with the error body RFC 3253 1.6 asks for, which 2.4
// adopts for this method.
func writeFault(w http.ResponseWriter, fault searchFault) {
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(fault.status)

	body := struct {
		XMLName      xml.Name     `xml:"DAV: error"`
		Precondition *anyProperty `xml:""`
		Description  string       `xml:"DAV: responsedescription,omitempty"`
	}{Description: fault.reason}
	if fault.precondition != "" {
		body.Precondition = &anyProperty{XMLName: xml.Name{Space: "DAV:", Local: fault.precondition}}
	}
	_ = xml.NewEncoder(w).Encode(body)
}
