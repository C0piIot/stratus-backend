package dav_test

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-vcard"
	"github.com/emersion/go-webdav"
	"github.com/emersion/go-webdav/carddav"

	"github.com/C0piIot/stratus-backend/internal/contacts"
	"github.com/C0piIot/stratus-backend/internal/dav"
	"github.com/C0piIot/stratus-backend/internal/files/filestest"
)

func person(uid, name string) vcard.Card {
	card := vcard.Card{}
	card.SetValue(vcard.FieldVersion, "4.0")
	card.SetValue(vcard.FieldUID, uid)
	card.SetValue(vcard.FieldFormattedName, name)
	card.SetValue(vcard.FieldEmail, uid+"@example.com")
	return card
}

func vcardText(t *testing.T, card vcard.Card) string {
	t.Helper()
	var b strings.Builder
	if err := vcard.NewEncoder(&b).Encode(card); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// TestAContactsClientFindsItsAddressBook is #423 driven by the library's own
// client: discovery from the principal, a write, a read, a query, and a sync
// by token -- which is what DAVx5 does, in that order.
func TestAContactsClientFindsItsAddressBook(t *testing.T) {
	t.Parallel()
	_, meta := filestest.Backends(t)
	srv := httptest.NewServer(withUser(dav.CardDAV("/contacts/", contacts.New(meta)), "edu"))
	t.Cleanup(srv.Close)
	hc := srv.Client()
	ctx := t.Context()

	discovering, err := webdav.NewClient(hc, srv.URL+"/contacts/")
	if err != nil {
		t.Fatal(err)
	}
	principal, err := discovering.FindCurrentUserPrincipal(ctx)
	if err != nil {
		t.Fatalf("FindCurrentUserPrincipal: %v", err)
	}
	client, err := carddav.NewClient(hc, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	home, err := client.FindAddressBookHomeSet(ctx, principal)
	if err != nil || home != "/contacts/edu/" {
		t.Fatalf("FindAddressBookHomeSet = %q, %v; want /contacts/edu/", home, err)
	}
	books, err := client.FindAddressBooks(ctx, home)
	if err != nil {
		t.Fatalf("FindAddressBooks: %v", err)
	}
	if len(books) != 1 || books[0].Path != "/contacts/edu/contacts/" || !books[0].SupportsAddressData(vcard.MIMEType, "4.0") {
		t.Fatalf("FindAddressBooks = %+v, want the default one taking vCard 4", books)
	}
	book := books[0].Path

	ada, err := client.PutAddressObject(ctx, book+"ada.vcf", person("ada", "Ada Lovelace"))
	if err != nil {
		t.Fatalf("PutAddressObject: %v", err)
	}
	if _, err = client.PutAddressObject(ctx, book+"alan.vcf", person("alan", "Alan Turing")); err != nil {
		t.Fatalf("PutAddressObject: %v", err)
	}
	got, err := client.GetAddressObject(ctx, book+"ada.vcf")
	if err != nil {
		t.Fatalf("GetAddressObject: %v", err)
	}
	if got.ETag == "" || got.ETag != ada.ETag || got.Card.PreferredValue(vcard.FieldFormattedName) != "Ada Lovelace" {
		t.Errorf("GET = %+v, want what was PUT with its ETag %q", got, ada.ETag)
	}

	found, err := client.QueryAddressBook(ctx, book, &carddav.AddressBookQuery{
		DataRequest: carddav.AddressDataRequest{AllProp: true},
		PropFilters: []carddav.PropFilter{{Name: vcard.FieldFormattedName, TextMatches: []carddav.TextMatch{{Text: "turing"}}}},
	}) // folded, as RFC 6352's default collation has it and the library's matcher does not
	if err != nil || len(found) != 1 || found[0].Path != book+"alan.vcf" {
		t.Errorf("a query for Turing = %+v, %v; want alan.vcf alone", found, err)
	}

	first, err := client.SyncCollection(ctx, book, &carddav.SyncQuery{})
	if err != nil || first.SyncToken == "" || len(first.Updated) != 2 {
		t.Fatalf("a first sync = %+v, %v; want both", first, err)
	}
	if err = client.RemoveAll(ctx, book+"alan.vcf"); err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	if ada, err = client.PutAddressObject(ctx, book+"ada.vcf", person("ada", "Ada King")); err != nil {
		t.Fatalf("PutAddressObject: %v", err)
	}
	next, err := client.SyncCollection(ctx, book, &carddav.SyncQuery{SyncToken: first.SyncToken})
	if err != nil {
		t.Fatalf("SyncCollection: %v", err)
	}
	if len(next.Updated) != 1 || next.Updated[0].Path != book+"ada.vcf" || len(next.Deleted) != 1 ||
		next.Deleted[0] != book+"alan.vcf" || next.SyncToken == first.SyncToken {
		t.Errorf("a sync after a write and a delete = %+v", next)
	}

	put := func(name, ifMatch, body string) int {
		t.Helper()
		r, rerr := http.NewRequestWithContext(ctx, http.MethodPut, srv.URL+book+name, strings.NewReader(body))
		if rerr != nil {
			t.Fatal(rerr)
		}
		r.Header.Set("Content-Type", vcard.MIMEType)
		if ifMatch != "" {
			r.Header.Set("If-Match", ifMatch)
		}
		answer, rerr := hc.Do(r)
		if rerr != nil {
			t.Fatal(rerr)
		}
		_ = answer.Body.Close()
		return answer.StatusCode
	}
	renamed := vcardText(t, person("ada", "Augusta Ada King"))
	if code := put("ada.vcf", `"somebody else's"`, renamed); code != http.StatusPreconditionFailed {
		t.Errorf("PUT over a changed contact = %d, want 412", code)
	}
	if code := put("ada.vcf", `"`+ada.ETag+`"`, renamed); code != http.StatusCreated {
		t.Errorf("PUT over the contact it read = %d, want 201", code)
	}
	if code := put("again.vcf", "", renamed); code != http.StatusConflict {
		t.Errorf("PUT of a UID another contact holds = %d, want 409", code)
	}
	nameless := strings.Replace(vcardText(t, person("bob", "x")), "FN:x\r\n", "", 1)
	if code := put("bob.vcf", "", nameless); code != http.StatusConflict {
		t.Errorf("PUT of a contact with no FN = %d, want 409 with valid-address-data", code)
	}

	// Somebody else's address books are not there, rather than forbidden.
	if rec := do(t, withUser(dav.CardDAV("/contacts/", contacts.New(meta)), "edu"), "PROPFIND", "/contacts/someone/contacts/", "", "Depth", "0"); rec.Code != http.StatusNotFound {
		t.Errorf("PROPFIND of another owner's address book = %d, want 404", rec.Code)
	}
}

// TestAnAddressBookIsMadeChangedAndRemoved is what a client sends to manage the
// collections themselves, and the answers off the path a client takes.
func TestAnAddressBookIsMadeChangedAndRemoved(t *testing.T) {
	t.Parallel()
	_, meta := filestest.Backends(t)
	h := withUser(dav.CardDAV("/contacts/", contacts.New(meta)), "edu")

	mkcol := `<?xml version="1.0" encoding="utf-8"?>
<D:mkcol xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">
  <D:set><D:prop>
    <D:resourcetype><D:collection/><C:addressbook/></D:resourcetype>
    <D:displayname>Work</D:displayname>
  </D:prop></D:set>
</D:mkcol>`
	if rec := do(t, h, "MKCOL", "/contacts/edu/work/", mkcol, "Content-Type", "application/xml"); rec.Code != http.StatusCreated {
		t.Fatalf("MKCOL = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, h, "MKCOL", "/contacts/edu/work/", mkcol, "Content-Type", "application/xml"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("a second MKCOL = %d, want 405", rec.Code)
	}

	patch := `<?xml version="1.0" encoding="utf-8"?>
<D:propertyupdate xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">
  <D:set><D:prop><D:displayname>Office</D:displayname><C:addressbook-description>Colleagues</C:addressbook-description></D:prop></D:set>
</D:propertyupdate>`
	rec := do(t, h, "PROPPATCH", "/contacts/edu/work/", patch, "Content-Type", "application/xml")
	wellFormed(t, rec.Body.String())
	if rec.Code != http.StatusMultiStatus || !strings.Contains(rec.Body.String(), "200 OK") {
		t.Errorf("PROPPATCH = %d:\n%s", rec.Code, rec.Body.String())
	}
	rec = do(t, h, "PROPFIND", "/contacts/edu/work", "", "Depth", "0")
	if !strings.Contains(rec.Body.String(), "Office") || !strings.Contains(rec.Body.String(), "Colleagues") {
		t.Errorf("PROPFIND after the PROPPATCH, without its slash:\n%s", rec.Body.String())
	}
	refused := `<?xml version="1.0" encoding="utf-8"?>
<D:propertyupdate xmlns:D="DAV:"><D:set><D:prop><D:displayname>Kept</D:displayname><D:owner>me</D:owner></D:prop></D:set>
<D:remove><D:prop><D:displayname/></D:prop></D:remove></D:propertyupdate>`
	rec = do(t, h, "PROPPATCH", "/contacts/edu/work/", refused, "Content-Type", "application/xml")
	if !strings.Contains(rec.Body.String(), "403 Forbidden") || !strings.Contains(rec.Body.String(), "424 Failed Dependency") {
		t.Errorf("a PROPPATCH with one it cannot keep:\n%s", rec.Body.String())
	}
	if rec := do(t, h, "PROPPATCH", "/contacts/edu/work/", "<not xml", "Content-Type", "application/xml"); rec.Code != http.StatusBadRequest {
		t.Errorf("a PROPPATCH that is not XML = %d, want 400", rec.Code)
	}
	if rec := do(t, h, "PROPPATCH", "/contacts/edu/nope/", patch, "Content-Type", "application/xml"); rec.Code != http.StatusNotFound {
		t.Errorf("a PROPPATCH of nothing = %d, want 404", rec.Code)
	}
	if rec := do(t, h, "PROPPATCH", "/contacts/someone/work/", patch, "Content-Type", "application/xml"); rec.Code != http.StatusNotFound {
		t.Errorf("a PROPPATCH of somebody else's = %d, want 404", rec.Code)
	}

	if rec := do(t, h, http.MethodDelete, "/contacts/edu/contacts/", ""); rec.Code != http.StatusForbidden {
		t.Errorf("DELETE of the default address book = %d, want 403", rec.Code)
	}
	if rec := do(t, h, http.MethodDelete, "/contacts/edu/work/", ""); rec.Code != http.StatusNoContent {
		t.Errorf("DELETE of an address book = %d, want 204", rec.Code)
	}
	for _, target := range []string{"/contacts/edu/work/", "/contacts/edu/work/a.vcf", "/contacts/edu/a/b/c/d"} {
		if rec := do(t, h, "PROPFIND", target, "", "Depth", "1"); rec.Code != http.StatusNotFound {
			t.Errorf("PROPFIND %s = %d, want 404", target, rec.Code)
		}
	}
	query := `<?xml version="1.0" encoding="utf-8"?>
<C:addressbook-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">
  <D:prop><D:getetag/></D:prop>
</C:addressbook-query>`
	if rec := do(t, h, "REPORT", "/contacts/edu/work/", query, "Depth", "1", "Content-Type", "application/xml"); rec.Code != http.StatusNotFound {
		t.Errorf("REPORT on a deleted address book = %d, want 404", rec.Code)
	}

	anonymous := dav.CardDAV("/contacts/", contacts.New(meta))
	for _, target := range []string{"/contacts/", "/contacts/edu/", "/contacts/edu/contacts/"} {
		if rec := do(t, anonymous, "PROPFIND", target, "", "Depth", "0"); rec.Code != http.StatusUnauthorized {
			t.Errorf("PROPFIND %s with nobody on the request = %d, want 401", target, rec.Code)
		}
	}
}

// TestAnAddressBookSyncsByToken is the calendars' sync, asked of an address
// book: the properties a client reads first, and the tokens it cannot be
// answered from.
func TestAnAddressBookSyncsByToken(t *testing.T) {
	t.Parallel()
	_, meta := filestest.Backends(t)
	svc := contacts.New(meta)
	h := withUser(dav.CardDAV("/contacts/", svc), "edu")
	ctx := t.Context()
	if _, err := svc.Put(ctx, "edu", contacts.DefaultName, "ada.vcf", person("ada", "Ada Lovelace"), nil); err != nil {
		t.Fatal(err)
	}

	props := `<?xml version="1.0" encoding="utf-8"?>
<D:propfind xmlns:D="DAV:" xmlns:CS="http://calendarserver.org/ns/">
  <D:prop><D:displayname/><D:sync-token/><CS:getctag/><D:supported-report-set/></D:prop>
</D:propfind>`
	rec := do(t, h, "PROPFIND", "/contacts/edu/", props, "Depth", "1", "Content-Type", "application/xml")
	wellFormed(t, rec.Body.String())
	for _, want := range []string{"Contacts", "urn:stratus:sync:", "getctag", "addressbook-multiget", "<sync-collection"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("listing the address books lacks %q:\n%s", want, rec.Body.String())
		}
	}

	report := func(token, level string) *httptest.ResponseRecorder {
		t.Helper()
		body := `<?xml version="1.0" encoding="utf-8"?>
<D:sync-collection xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">
  <D:sync-token>` + token + `</D:sync-token><D:sync-level>` + level + `</D:sync-level>
  <D:prop><D:getetag/><C:address-data/><D:getcontenttype/></D:prop>
</D:sync-collection>`
		rec := do(t, h, "REPORT", "/contacts/edu/contacts/", body, "Content-Type", "application/xml")
		wellFormed(t, rec.Body.String())
		return rec
	}
	first := report("", "1")
	for _, want := range []string{"ada.vcf", "BEGIN:VCARD", "text/vcard"} {
		if !strings.Contains(first.Body.String(), want) {
			t.Errorf("the first sync lacks %q:\n%s", want, first.Body.String())
		}
	}
	m := syncTokenRE.FindStringSubmatch(first.Body.String())
	if m == nil {
		t.Fatalf("no token:\n%s", first.Body.String())
	}
	if err := svc.Delete(ctx, "edu", contacts.DefaultName, "ada.vcf"); err != nil {
		t.Fatal(err)
	}
	if _, err := meta.PruneContactTombstones(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ token, level string }{
		{m[1], "1"},
		{"not a token", "1"},
		{m[1], "infinite"},
	} {
		if rec := report(tc.token, tc.level); rec.Code != http.StatusForbidden {
			t.Errorf("sync with %q at level %s = %d:\n%s", tc.token, tc.level, rec.Code, rec.Body.String())
		}
	}
	nope := `<?xml version="1.0" encoding="utf-8"?>
<D:sync-collection xmlns:D="DAV:"><D:sync-token/><D:sync-level>1</D:sync-level><D:prop/></D:sync-collection>`
	if rec := do(t, h, "REPORT", "/contacts/edu/nope/", nope, "Content-Type", "application/xml"); rec.Code != http.StatusNotFound {
		t.Errorf("sync of an address book nobody made = %d, want 404", rec.Code)
	}
	if rec := do(t, h, "PROPFIND", "/contacts/edu/nope/", props, "Depth", "0", "Content-Type", "application/xml"); rec.Code != http.StatusNotFound {
		t.Errorf("PROPFIND for the token of an address book nobody made = %d, want 404", rec.Code)
	}
}

// TestAContactIsServedAsStored is a GET answered from the bytes the ETag is a
// digest of, which the library's own GET -- go-vcard encoding the card again,
// its parameters in map order -- could not promise.
func TestAContactIsServedAsStored(t *testing.T) {
	t.Parallel()
	_, meta := filestest.Backends(t)
	service := contacts.New(meta)
	h := withUser(dav.CardDAV("/contacts/", service), "edu")
	card := person("ada", "Ada Lovelace")
	card.Add(vcard.FieldTelephone, &vcard.Field{Value: "+44 1", Params: vcard.Params{"TYPE": {"cell"}, "PREF": {"1"}, "X-A": {"1"}}})
	stored, err := service.Put(t.Context(), "edu", contacts.DefaultName, "ada.vcf", card, nil)
	if err != nil {
		t.Fatal(err)
	}
	serve := func(method, target string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), method, target, nil))
		return rec
	}

	for range 20 {
		rec := serve(http.MethodGet, "/contacts/edu/contacts/ada.vcf")
		if rec.Code != http.StatusOK || rec.Body.String() != stored.Data || rec.Header().Get("ETag") != `"`+stored.ETag+`"` ||
			rec.Header().Get("Content-Type") != vcard.MIMEType {
			t.Fatalf("GET = %d %v:\n%s\nwant\n%s", rec.Code, rec.Header(), rec.Body, stored.Data)
		}
	}
	if rec := serve(http.MethodHead, "/contacts/edu/contacts/ada.vcf"); rec.Code != http.StatusOK || rec.Body.Len() != 0 ||
		rec.Header().Get("Content-Length") != strconv.Itoa(len(stored.Data)) {
		t.Errorf("HEAD = %d, %d bytes, length %q", rec.Code, rec.Body.Len(), rec.Header().Get("Content-Length"))
	}
	for target, code := range map[string]int{
		"/contacts/edu/contacts/nobody.vcf":  http.StatusNotFound,
		"/contacts/someone/contacts/ada.vcf": http.StatusNotFound,
	} {
		if rec := serve(http.MethodGet, target); rec.Code != code {
			t.Errorf("GET %s = %d, want %d", target, rec.Code, code)
		}
	}
}
