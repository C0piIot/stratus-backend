package web_test

import (
	htmlstd "html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/emersion/go-vcard"

	"github.com/stratus-private-cloud/stratus-backend/internal/contacts"
	"github.com/stratus-private-cloud/stratus-backend/internal/db"
)

func storedCard(t *testing.T, meta db.Store, name string) (vcard.Card, db.Contact) {
	t.Helper()
	c, err := contacts.New(meta).Contact(t.Context(), username, contacts.DefaultName, name)
	if err != nil {
		t.Fatal(err)
	}
	card, err := contacts.Decode(c)
	if err != nil {
		t.Fatal(err)
	}
	return card, c
}

// TestANewContactIsWrittenAsAClientWould is #442's first form: what is typed
// becomes a vCard 3.0 under a UUID, in the address book a client syncs.
func TestANewContactIsWrittenAsAClientWould(t *testing.T) {
	t.Parallel()
	h, _, meta := browserOver(t)
	cookie := signIn(t, h)

	book := get(t, h, "/contacts/edu/contacts/", cookie).Body.String()
	if !strings.Contains(book, `href="/contact/new/contacts">New contact</a>`) {
		t.Errorf("the address book offers no new contact:\n%s", book)
	}
	if rec := get(t, h, "/contact/new/contacts", cookie); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "New contact") {
		t.Fatalf("the new contact form = %d", rec.Code)
	}

	rec := post(t, h, "/contact/new/contacts", url.Values{
		"given": {"Grace"}, "family": {"Hopper"}, "org": {"US Navy"}, "title": {"Rear Admiral"},
		"tel-index": {"-1", "-1"}, "tel-kind": {"cell", ""}, "tel": {"+1 555 0100", ""},
		"email-index": {"-1"}, "email-kind": {"work"}, "email": {"grace@example.com"},
		"adr-index": {"-1"}, "adr-kind": {"home"}, "street": {"1 Main; St"}, "postcode": {"10001"},
		"locality": {"New York"}, "region": {""}, "country": {"USA"},
		"bday": {"1906-12-09"}, "note": {"COBOL\r\nand bugs"},
	}, cookie)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/contacts/edu/contacts/" {
		t.Fatalf("saving a new contact = %d to %q:\n%s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}

	list, err := contacts.New(meta).Contacts(t.Context(), username, contacts.DefaultName)
	if err != nil || len(list) != 1 {
		t.Fatalf("the address book holds %d contacts, %v", len(list), err)
	}
	card, c := storedCard(t, meta, list[0].Name)
	uid := card.Value(vcard.FieldUID)
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(uid) || c.Name != uid+".vcf" {
		t.Errorf("the contact is %q with UID %q", c.Name, uid)
	}
	for key, want := range map[string]string{
		vcard.FieldVersion: "3.0", vcard.FieldFormattedName: "Grace Hopper", vcard.FieldName: "Hopper;Grace;;;",
		vcard.FieldOrganization: "US Navy", vcard.FieldTitle: "Rear Admiral", vcard.FieldTelephone: "+1 555 0100",
		vcard.FieldEmail: "grace@example.com", vcard.FieldAddress: ";;1 Main, St;New York;;10001;USA",
		vcard.FieldBirthday: "1906-12-09", vcard.FieldNote: "COBOL\nand bugs",
	} {
		if got := card.Value(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if len(card[vcard.FieldTelephone]) != 1 || !card.Get(vcard.FieldTelephone).Params.HasType("cell") ||
		!card.Get(vcard.FieldEmail).Params.HasType("work") || !card.Get(vcard.FieldAddress).Params.HasType("home") {
		t.Errorf("the kinds were not kept: %v", card)
	}
}

// TestAnEditKeepsWhatTheFormDoesNotShow is the reason an edit is made to the
// stored card: a photograph, a client's own properties and the parameters on
// a number survive a web edit, and a field nobody touched is not rewritten.
func TestAnEditKeepsWhatTheFormDoesNotShow(t *testing.T) {
	t.Parallel()
	h, _, meta := browserOver(t)
	cookie := signIn(t, h)
	putContact(t, meta, contacts.DefaultName, "turing",
		"FN:Alan Turing", "N:Turing;Alan;Mathison;;", "ORG:Bletchley Park;Hut 8", "TITLE:Cryptanalyst",
		"TEL;TYPE=cell,voice;PREF=1:+44 20 7946 0000", "TEL;TYPE=home:tel:+44-1908-640404",
		"item1.EMAIL:alan@example.com", "item1.X-ABLABEL:Bletchley", "BDAY:--0623",
		"PHOTO:data:image/jpeg;base64,AAAA", "X-SOCIAL:@turing")
	before, c := storedCard(t, meta, "turing.vcf")

	form := get(t, h, "/contact/edit/contacts/turing.vcf", cookie)
	body := htmlstd.UnescapeString(form.Body.String())
	for _, want := range []string{
		`value="Alan"`, `value="Turing"`, `value="Bletchley Park"`, `value="+44-1908-640404"`,
		`name="bday" type="text" value="--0623"`, `value="` + c.ETag + `"`,
		`href="/contact/delete/contacts/turing.vcf"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the form has no %q", want)
		}
	}

	untouched := url.Values{
		"etag": {c.ETag}, "given": {"Alan"}, "family": {"Turing"}, "org": {"Bletchley Park"}, "title": {"Cryptanalyst"},
		"tel-index": {"0", "1", "-1"}, "tel-kind": {"cell", "home", ""}, "tel": {"+44 20 7946 0000", "+44-1908-640404", ""},
		"email-index": {"0", "-1"}, "email-kind": {"", ""}, "email": {"alan@example.com", ""},
		"bday": {"--0623"}, "bday-text": {"1"}, "note": {""},
	}
	if rec := post(t, h, "/contact/edit/contacts/turing.vcf", untouched, cookie); rec.Code != http.StatusSeeOther {
		t.Fatalf("saving untouched = %d:\n%s", rec.Code, rec.Body)
	}
	_, c2 := storedCard(t, meta, "turing.vcf")
	if c2.Data != c.Data {
		t.Errorf("saving untouched changed the card:\n%s\nto\n%s", c.Data, c2.Data)
	}

	edited := url.Values{}
	for k, v := range untouched {
		edited[k] = v
	}
	edited["etag"] = []string{c2.ETag}
	edited["title"] = []string{"Mathematician"}
	edited["org"] = []string{"King's College"}
	edited["tel-kind"] = []string{"work", "home", "cell"}
	edited["tel"] = []string{"+44 20 7946 0000", "", "+44 7700 900000"}
	if rec := post(t, h, "/contact/edit/contacts/turing.vcf", edited, cookie); rec.Code != http.StatusSeeOther {
		t.Fatalf("saving an edit = %d:\n%s", rec.Code, rec.Body)
	}
	after, _ := storedCard(t, meta, "turing.vcf")

	for _, key := range []string{vcard.FieldPhoto, "X-SOCIAL", vcard.FieldBirthday, "X-ABLABEL", vcard.FieldFormattedName, vcard.FieldName} {
		if after.Value(key) != before.Value(key) {
			t.Errorf("%s went from %q to %q", key, before.Value(key), after.Value(key))
		}
	}
	if e := after.Get(vcard.FieldEmail); e == nil || e.Group != "item1" {
		t.Errorf("the email lost its group: %+v", e)
	}
	if got := after.Value(vcard.FieldOrganization); got != "King's College;Hut 8" {
		t.Errorf("ORG = %q, want the company changed and the department kept", got)
	}
	if got := after.Value(vcard.FieldTitle); got != "Mathematician" {
		t.Errorf("TITLE = %q", got)
	}
	tels := after[vcard.FieldTelephone]
	if len(tels) != 2 {
		t.Fatalf("the numbers are %d, want the cleared one gone and the new one there", len(tels))
	}
	if p := tels[0].Params; !p.HasType("work") || !p.HasType("voice") || p.HasType("cell") || p.Get(vcard.ParamPreferred) != "1" {
		t.Errorf("the first number's parameters are %v, want work and voice and its PREF", p)
	}
	if tels[1].Value != "+44 7700 900000" || !tels[1].Params.HasType("cell") {
		t.Errorf("the new number is %+v", tels[1])
	}
}

func TestANameMakesTheDisplayName(t *testing.T) {
	t.Parallel()
	h, _, meta := browserOver(t)
	cookie := signIn(t, h)
	putContact(t, meta, contacts.DefaultName, "ada", "FN:Ada Lovelace", "N:Lovelace;Ada;;;")
	putContact(t, meta, contacts.DefaultName, "boss", "FN:The Boss", "N:Springsteen;Bruce;;;")
	putContact(t, meta, contacts.DefaultName, "acme", "FN:Acme")

	save := func(name string, form url.Values) {
		t.Helper()
		_, c := storedCard(t, meta, name)
		form.Set("etag", c.ETag)
		if rec := post(t, h, "/contact/edit/contacts/"+name, form, cookie); rec.Code != http.StatusSeeOther {
			t.Fatalf("saving %s = %d:\n%s", name, rec.Code, rec.Body)
		}
	}
	save("ada.vcf", url.Values{"given": {"Augusta Ada"}, "family": {"King"}})
	save("boss.vcf", url.Values{"given": {"Bruce"}, "family": {"Springsteen"}, "org": {"E Street"}})
	save("acme.vcf", url.Values{"given": {"Acme"}, "org": {"Acme Inc"}})

	for name, want := range map[string]string{"ada.vcf": "Augusta Ada King", "boss.vcf": "The Boss", "acme.vcf": "Acme"} {
		if card, _ := storedCard(t, meta, name); card.Value(vcard.FieldFormattedName) != want {
			t.Errorf("%s is called %q, want %q", name, card.Value(vcard.FieldFormattedName), want)
		}
	}
	if card, _ := storedCard(t, meta, "acme.vcf"); card.Get(vcard.FieldName) != nil {
		t.Errorf("a contact with no N was given one it did not ask for: %v", card.Value(vcard.FieldName))
	}
}

// TestAStaleFormSavesNothing is the form's If-Match: a contact changed or
// deleted on a phone since the form was drawn is not overwritten, and what was
// typed is still there.
func TestAStaleFormSavesNothing(t *testing.T) {
	t.Parallel()
	h, _, meta := browserOver(t)
	cookie := signIn(t, h)
	putContact(t, meta, contacts.DefaultName, "ada", "FN:Ada Lovelace", "N:Lovelace;Ada;;;")
	_, was := storedCard(t, meta, "ada.vcf")
	putContact(t, meta, contacts.DefaultName, "ada", "FN:Ada King", "N:King;Ada;;;")
	_, now := storedCard(t, meta, "ada.vcf")

	rec := post(t, h, "/contact/edit/contacts/ada.vcf", url.Values{
		"etag": {was.ETag}, "given": {"Ada"}, "family": {"Byron"},
	}, cookie)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "changed or deleted somewhere else") ||
		!strings.Contains(rec.Body.String(), `value="Byron"`) {
		t.Errorf("a stale form = %d:\n%s", rec.Code, rec.Body)
	}
	if _, after := storedCard(t, meta, "ada.vcf"); after.Data != now.Data {
		t.Error("a stale form overwrote the contact")
	}

	if err := contacts.New(meta).Delete(t.Context(), username, contacts.DefaultName, "ada.vcf"); err != nil {
		t.Fatal(err)
	}
	if rec := post(t, h, "/contact/edit/contacts/ada.vcf", url.Values{"etag": {now.ETag}, "given": {"Ada"}}, cookie); rec.Code != http.StatusConflict {
		t.Errorf("a form for a deleted contact = %d", rec.Code)
	}
	if list, _ := contacts.New(meta).Contacts(t.Context(), username, contacts.DefaultName); len(list) != 0 {
		t.Error("a form for a deleted contact brought it back")
	}
}

func TestAFormWithAMistakeSavesNothing(t *testing.T) {
	t.Parallel()
	h, _, meta := browserOver(t)
	cookie := signIn(t, h)
	for name, tc := range map[string]struct {
		form url.Values
		want string
	}{
		"no name":  {url.Values{"note": {"who?"}}, "A contact needs a name or a company."},
		"an email": {url.Values{"given": {"Ada"}, "email-index": {"-1"}, "email": {"ada at example"}}, "ada at example is not an email address."},
		"a date":   {url.Values{"given": {"Ada"}, "bday": {"10 Dec"}}, "A birthday is a date."},
	} {
		rec := post(t, h, "/contact/new/contacts", tc.form, cookie)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("%s = %d, want 400 saying %q:\n%s", name, rec.Code, tc.want, rec.Body)
		}
	}
	if list, _ := contacts.New(meta).Contacts(t.Context(), username, contacts.DefaultName); len(list) != 0 {
		t.Errorf("a form with a mistake stored %d contacts", len(list))
	}
}

// TestAnotherRowIsAPostThatSavesNothing is how a form with no script grows.
func TestAnotherRowIsAPostThatSavesNothing(t *testing.T) {
	t.Parallel()
	h, _, meta := browserOver(t)
	cookie := signIn(t, h)
	for _, kind := range []string{"tel", "email", "adr"} {
		rec := post(t, h, "/contact/new/contacts", url.Values{
			"add": {kind}, "given": {"Ada"}, "tel-index": {"-1"}, "tel": {"+44 1"},
		}, cookie)
		body := htmlstd.UnescapeString(rec.Body.String())
		if rec.Code != http.StatusOK || !strings.Contains(body, `value="Ada"`) || !strings.Contains(body, `value="+44 1"`) {
			t.Errorf("another %s row = %d, without what was typed", kind, rec.Code)
		}
		name := map[string]string{"tel": `name="tel-index"`, "email": `name="email-index"`, "adr": `name="adr-index"`}[kind]
		if n := strings.Count(body, name); n != map[string]int{"tel": 2, "email": 1, "adr": 1}[kind] {
			t.Errorf("another %s row drew %d of them", kind, n)
		}
	}
	if list, _ := contacts.New(meta).Contacts(t.Context(), username, contacts.DefaultName); len(list) != 0 {
		t.Error("asking for another row saved the contact")
	}
}

func TestDeletingAContactAsksFirst(t *testing.T) {
	t.Parallel()
	h, _, meta := browserOver(t)
	cookie := signIn(t, h)
	putContact(t, meta, contacts.DefaultName, "ada", "FN:Ada Lovelace")

	if body := get(t, h, "/contacts/edu/contacts/", cookie).Body.String(); !strings.Contains(body, `href="/contact/edit/contacts/ada.vcf">Edit</a>`) {
		t.Errorf("the address book offers no edit:\n%s", body)
	}
	page := get(t, h, "/contact/delete/contacts/ada.vcf", cookie)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "<strong>Ada Lovelace</strong> will be deleted for good") {
		t.Errorf("the delete page = %d:\n%s", page.Code, page.Body)
	}
	rec := post(t, h, "/contact/delete/contacts/ada.vcf", nil, cookie)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/contacts/edu/contacts/" {
		t.Errorf("deleting = %d to %q", rec.Code, rec.Header().Get("Location"))
	}
	if list, _ := contacts.New(meta).Contacts(t.Context(), username, contacts.DefaultName); len(list) != 0 {
		t.Error("the contact is still there")
	}
	for _, target := range []string{"/contact/delete/contacts/ada.vcf", "/contact/edit/contacts/ada.vcf", "/contact/new/nope"} {
		if rec := get(t, h, target, cookie); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", target, rec.Code)
		}
	}
	for _, target := range []string{"/contact/delete/contacts/ada.vcf", "/contact/new/nope"} {
		if rec := post(t, h, target, url.Values{"given": {"Ada"}}, cookie); rec.Code != http.StatusNotFound {
			t.Errorf("POST %s = %d, want 404", target, rec.Code)
		}
	}
	if rec := post(t, h, "/contact/new/contacts", url.Values{"given": {"Ada"}}); rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/login") {
		t.Errorf("a new contact with no session = %d", rec.Code)
	}
}

// TestAnEditWritesWhatChanged is the other half: a field cleared is gone, a
// kind taken off leaves no TYPE behind, an address keeps what the form does
// not show of it, and a 4.0 birthday is written the way 4.0 writes one.
func TestAnEditWritesWhatChanged(t *testing.T) {
	t.Parallel()
	h, _, meta := browserOver(t)
	cookie := signIn(t, h)
	putContact(t, meta, contacts.DefaultName, "turing", "FN:Alan Turing", "N:Turing;Alan;;;",
		"TITLE:Cryptanalyst", "NOTE:Hut 8", "BDAY:19120623", "EMAIL;TYPE=work:alan@example.com",
		"ADR;TYPE=home:PO 1;Flat 2;2 Station Rd;Bletchley;;MK3 6EB;UK")
	_, c := storedCard(t, meta, "turing.vcf")
	if body := htmlstd.UnescapeString(get(t, h, "/contact/edit/contacts/turing.vcf", cookie).Body.String()); !strings.Contains(body, `type="date" value="1912-06-23"`) {
		t.Errorf("a 4.0 birthday is not drawn as a date:\n%s", body)
	}

	rec := post(t, h, "/contact/edit/contacts/turing.vcf", url.Values{
		"etag": {c.ETag}, "given": {"Alan"}, "family": {"Turing"}, "bday": {"1912-06-24"},
		"email-index": {"0"}, "email-kind": {""}, "email": {"alan@example.com"},
		"adr-index": {"0"}, "adr-kind": {"work"}, "street": {"Sherborne"}, "locality": {"Bletchley"},
		"postcode": {"MK3 6EB"}, "country": {"UK"},
	}, cookie)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("saving = %d:\n%s", rec.Code, rec.Body)
	}
	card, _ := storedCard(t, meta, "turing.vcf")
	for key, want := range map[string]string{
		vcard.FieldTitle: "", vcard.FieldNote: "", vcard.FieldBirthday: "19120624",
		vcard.FieldAddress: "PO 1;Flat 2;Sherborne;Bletchley;;MK3 6EB;UK",
	} {
		if got := card.Value(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if p := card.Get(vcard.FieldEmail).Params; len(p.Types()) != 0 {
		t.Errorf("the email kept a kind it was told to drop: %v", p)
	}
	if p := card.Get(vcard.FieldAddress).Params; !p.HasType("work") || p.HasType("home") {
		t.Errorf("the address's kind is %v, want work", p)
	}

	// A company and no name is a contact called by the company.
	if rec := post(t, h, "/contact/new/contacts", url.Values{"org": {"Acme"}}, cookie); rec.Code != http.StatusSeeOther {
		t.Fatalf("a company alone = %d", rec.Code)
	}
	list, _ := contacts.New(meta).Contacts(t.Context(), username, contacts.DefaultName)
	for _, c := range list {
		if c.Name != "turing.vcf" && !strings.Contains(c.Data, "FN:Acme") {
			t.Errorf("a company alone is called:\n%s", c.Data)
		}
	}
}
