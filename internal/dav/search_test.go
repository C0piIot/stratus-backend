package dav_test

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// searchFor is the query a client sends, with the parts a test varies left
// open. Written out rather than built, because what is under test is a wire
// format and a struct would prove this file agrees with itself.
func searchFor(term string) string {
	return `<?xml version="1.0" encoding="utf-8"?>
<D:searchrequest xmlns:D="DAV:">
  <D:basicsearch>
    <D:select><D:prop><D:resourcetype/><D:getcontentlength/><D:getcontenttype/><D:getetag/><D:getlastmodified/></D:prop></D:select>
    <D:from><D:scope><D:href>/files/</D:href><D:depth>infinity</D:depth></D:scope></D:from>
    <D:where><D:contains>` + term + `</D:contains></D:where>
  </D:basicsearch>
</D:searchrequest>`
}

func TestSearchFindsAFileByAWordOfItsName(t *testing.T) {
	t.Parallel()
	h := server(t)

	do(t, h, "MKCOL", "/files/holiday", "")
	do(t, h, http.MethodPut, "/files/holiday/sunset%20over%20lisbon.jpg", "x")
	do(t, h, http.MethodPut, "/files/holiday/notes.txt", "y")

	rec := do(t, h, "SEARCH", "/files/", searchFor("lisbon"))
	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("SEARCH = %d, want 207\n%s", rec.Code, rec.Body)
	}

	want := "/files/holiday/sunset over lisbon.jpg"
	if got := hrefs(t, rec); len(got) != 1 || got[0] != want {
		t.Fatalf("SEARCH found %v, want just %q", got, want)
	}
	// And it travels escaped, because what a client does with an href is turn
	// it back into a URL.
	if !strings.Contains(rec.Body.String(), "sunset%20over%20lisbon.jpg") {
		t.Errorf("the href is not escaped:\n%s", rec.Body)
	}
}

// A word in a folder finds the folder, which is db.Finder's promise restated
// at this surface: the alternative -- the thousand photographs under it -- is
// not an answer to the word somebody typed.
func TestSearchFindsAFolderAndNotWhatIsUnderIt(t *testing.T) {
	t.Parallel()
	h := server(t)

	do(t, h, "MKCOL", "/files/holiday", "")
	do(t, h, http.MethodPut, "/files/holiday/IMG_0001.JPG", "x")

	rec := do(t, h, "SEARCH", "/files/", searchFor("holiday"))
	// A collection's href ends in a slash, as it does in a PROPFIND here.
	if got := hrefs(t, rec); len(got) != 1 || got[0] != "/files/holiday/" {
		t.Fatalf("SEARCH found %v, want just the folder", got)
	}
}

// The properties come from the same rowInfo PROPFIND answers from, so the two
// surfaces cannot report different bytes for the same file. The ETag is the
// one worth pinning: a validator that differed by which request asked is what
// #136 was about.
func TestSearchAndPropfindAgreeAboutAFile(t *testing.T) {
	t.Parallel()
	h := server(t)
	do(t, h, http.MethodPut, "/files/report.txt", "hello")

	found := propsOf(t, do(t, h, "SEARCH", "/files/", searchFor("report")).Body.String())
	listed := propsOf(t, do(t, h, "PROPFIND", "/files/report.txt", "", "Depth", "0").Body.String())

	const href = "/files/report.txt"
	for _, name := range []string{"getetag", "getcontentlength", "getcontenttype"} {
		got, want := found[href][name], listed[href][name]
		if got == "" || got != want {
			t.Errorf("SEARCH says %s is %q and PROPFIND says %q", name, got, want)
		}
	}
}

// A property this server has nothing to say about is reported absent rather
// than invented, which is what a second propstat at 404 means.
func TestSearchReportsAPropertyItDoesNotHave(t *testing.T) {
	t.Parallel()
	h := server(t)
	do(t, h, http.MethodPut, "/files/report.txt", "hello")

	query := strings.Replace(searchFor("report"),
		"<D:getlastmodified/>", "<D:getlastmodified/><D:creationdate/>", 1)
	ms := parseMultistatus(t, do(t, h, "SEARCH", "/files/", query).Body.String())

	if len(ms.Responses) != 1 || len(ms.Responses[0].Propstat) != 2 {
		t.Fatalf("want one response with a found half and an absent half, got %+v", ms)
	}
	absent := ms.Responses[0].Propstat[1]
	if !strings.Contains(absent.Status, "404") || !strings.Contains(string(absent.Prop.Inner), "creationdate") {
		t.Errorf("the absent half is %+v, want creationdate at 404", absent)
	}
}

// 2.3.1: a cut answer is a 207 carrying the partial results and the arbiter's
// own href at 507. Silence would leave a client believing it had seen
// everything, which is the one thing a search must not do.
func TestSearchSaysWhenTheAnswerWasCut(t *testing.T) {
	t.Parallel()
	h := server(t)
	for i := range 4 {
		do(t, h, http.MethodPut, "/files/beach-"+strconv.Itoa(i)+".jpg", "x")
	}

	query := strings.Replace(searchFor("beach"),
		"</D:basicsearch>", "<D:limit><D:nresults>2</D:nresults></D:limit></D:basicsearch>", 1)
	rec := do(t, h, "SEARCH", "/files/", query)
	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("a cut answer = %d, want 207", rec.Code)
	}

	ms := parseMultistatus(t, rec.Body.String())
	if len(ms.Responses) != 3 {
		t.Fatalf("want two results and the arbiter, got %v", hrefs(t, rec))
	}
	arbiter := ms.Responses[2]
	if arbiter.Href != "/files/" || !strings.Contains(arbiter.Status, "507") {
		t.Errorf("the last response is %q at %q, want /files/ at 507", arbiter.Href, arbiter.Status)
	}
}

// Every refusal names what it refused. A client that sent an operator this
// server does not have cannot act on a bare 400.
func TestSearchRefusesWhatItCannotAnswer(t *testing.T) {
	t.Parallel()
	h := server(t)

	for _, c := range []struct {
		name         string
		query        string
		want         int
		precondition string
	}{{
		name:         "a grammar that is not basicsearch",
		query:        `<D:searchrequest xmlns:D="DAV:"><D:mysearch/></D:searchrequest>`,
		want:         http.StatusUnprocessableEntity,
		precondition: "search-grammar-supported",
	}, {
		name: "an operator we do not have",
		query: strings.Replace(searchFor("x"),
			"<D:contains>x</D:contains>", `<D:like><D:prop><D:displayname/></D:prop><D:literal>%x%</D:literal></D:like>`, 1),
		want: http.StatusUnprocessableEntity,
	}, {
		name:         "a scope somewhere else",
		query:        strings.Replace(searchFor("x"), "<D:href>/files/</D:href>", "<D:href>/photos/</D:href>", 1),
		want:         http.StatusConflict,
		precondition: "search-scope-valid",
	}, {
		name:         "a scope narrower than the collection",
		query:        strings.Replace(searchFor("x"), "<D:href>/files/</D:href>", "<D:href>/files/holiday</D:href>", 1),
		want:         http.StatusConflict,
		precondition: "search-scope-valid",
	}, {
		name:         "two scopes",
		query:        strings.Replace(searchFor("x"), "</D:from>", "<D:scope><D:href>/files/</D:href><D:depth>infinity</D:depth></D:scope></D:from>", 1),
		want:         http.StatusUnprocessableEntity,
		precondition: "search-multiple-scope-supported",
	}, {
		name:  "an ordering we cannot give",
		query: strings.Replace(searchFor("x"), "</D:basicsearch>", "<D:orderby><D:order><D:score/></D:order></D:orderby></D:basicsearch>", 1),
		want:  http.StatusUnprocessableEntity,
	}, {
		name:  "a condition with nothing to look for",
		query: strings.Replace(searchFor("x"), "<D:contains>x</D:contains>", "<D:contains></D:contains>", 1),
		want:  http.StatusUnprocessableEntity,
	}, {
		name:  "no condition at all, which is a listing",
		query: strings.Replace(searchFor("x"), "<D:where><D:contains>x</D:contains></D:where>", "", 1),
		want:  http.StatusUnprocessableEntity,
	}, {
		name:  "a body that is not XML",
		query: "not xml at all",
		want:  http.StatusBadRequest,
	}} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			rec := do(t, h, "SEARCH", "/files/", c.query)
			if rec.Code != c.want {
				t.Fatalf("%s = %d, want %d\n%s", c.name, rec.Code, c.want, rec.Body)
			}
			if c.precondition != "" && !strings.Contains(rec.Body.String(), c.precondition) {
				t.Errorf("the refusal does not name %s:\n%s", c.precondition, rec.Body)
			}
		})
	}
}

// 3.2: the header is the only discovery there is. Without it a client would
// have to send a query to find out whether queries work.
func TestOptionsAdvertisesSearch(t *testing.T) {
	t.Parallel()
	rec := do(t, server(t), http.MethodOptions, "/files/", "")

	if got := rec.Header().Get("DASL"); got != "<DAV:basicsearch>" {
		t.Errorf("DASL = %q, want <DAV:basicsearch>", got)
	}
	if got := rec.Header().Get("Allow"); !strings.Contains(got, "SEARCH") {
		t.Errorf("Allow = %q, want SEARCH in it", got)
	}
}
