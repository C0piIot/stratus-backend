package web_test

import (
	htmlstd "html"
	"net/http"
	"strings"
	"testing"

	"github.com/emersion/go-vcard"

	"github.com/C0piIot/stratus-backend/internal/contacts"
	"github.com/C0piIot/stratus-backend/internal/db"
)

func putContact(t *testing.T, meta db.Store, book, name string, lines ...string) {
	t.Helper()
	text := "BEGIN:VCARD\r\nVERSION:4.0\r\nUID:" + name + "\r\n" + strings.Join(lines, "\r\n") + "\r\nEND:VCARD\r\n"
	card, err := vcard.NewDecoder(strings.NewReader(text)).Decode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := contacts.New(meta).Put(t.Context(), username, book, name+".vcf", card, nil); err != nil {
		t.Fatal(err)
	}
}

func TestTheContactsNeedASession(t *testing.T) {
	t.Parallel()
	h := newHandler(t, nil)
	rec := get(t, h, "/contacts/edu/contacts/")
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/login") {
		t.Errorf("the contacts with no session = %d", rec.Code)
	}
}

// TestAnAddressBookIsAListByName is #423's page: every contact under its
// letter, in order, each way to reach them a link.
func TestAnAddressBookIsAListByName(t *testing.T) {
	t.Parallel()
	h, _, meta := browserOver(t)
	cookie := signIn(t, h)

	putContact(t, meta, contacts.DefaultName, "turing", "FN:Alan Turing", "ORG:Bletchley Park;Hut 8",
		"TITLE:Cryptanalyst", "TEL;TYPE=cell,voice:+44 (0)20 7946-0000", "EMAIL:alan@example.com",
		"ADR;TYPE=home:;;2 Station Rd;Bletchley;;MK3 6EB;UK", "BDAY:19120623")
	putContact(t, meta, contacts.DefaultName, "ada", "FN:ada Lovelace", "NOTE:Notes & numbers")
	putContact(t, meta, contacts.DefaultName, "odd", "FN:3M Helpdesk", `TEL:javascript:alert(1)`)

	rec := get(t, h, "/contacts/", cookie)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/contacts/edu/" {
		t.Fatalf("/contacts/ = %d to %q", rec.Code, rec.Header().Get("Location"))
	}
	rec = get(t, h, "/contacts/edu/", cookie)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/contacts/edu/contacts/" {
		t.Fatalf("/contacts/edu/ = %d to %q", rec.Code, rec.Header().Get("Location"))
	}

	page := get(t, h, "/contacts/edu/contacts/", cookie)
	if page.Code != http.StatusOK {
		t.Fatalf("the address book = %d:\n%s", page.Code, page.Body.String())
	}
	body := htmlstd.UnescapeString(page.Body.String())
	for _, want := range []string{
		"3 contacts", "Cryptanalyst · Bletchley Park · Hut 8",
		`href="tel:+442079460000">+44 (0)20 7946-0000</a> <span class="text-body-secondary">cell</span>`,
		`href="mailto:alan@example.com"`, "2 Station Rd, MK3 6EB, Bletchley, UK", "born 19120623", "Notes & numbers",
		`class="dropdown-item" href="/contacts/">Contacts</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the address book has no %q", want)
		}
	}
	// Under their letters, in order, with case not deciding it.
	hash, a := strings.Index(body, ">#<"), strings.Index(body, ">A<")
	if hash < 0 || a < hash || strings.Index(body, "ada Lovelace") > strings.Index(body, "Alan Turing") {
		t.Errorf("the contacts are not under #, then A, by name")
	}
	if !strings.Contains(body, `<div class="small">javascript:alert(1)</div>`) {
		t.Error("a number that is not one became a link")
	}
}

func TestAnEmptyAddressBookAndTheOthers(t *testing.T) {
	t.Parallel()
	h, _, meta := browserOver(t)
	cookie := signIn(t, h)
	svc := contacts.New(meta)
	if _, err := svc.AddressBooks(t.Context(), username); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(t.Context(), username, db.AddressBook{Name: "work", DisplayName: "Work"}); err != nil {
		t.Fatal(err)
	}

	body := get(t, h, "/contacts/edu/work/", cookie).Body.String()
	for _, want := range []string{
		"No contacts in this address book.", `href="/contacts/edu/contacts/">Contacts</a>`,
		`active" href="/contacts/edu/work/" aria-current="page">Work</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the address book has no %q:\n%s", want, body)
		}
	}
	for target, code := range map[string]int{
		"/contacts/edu/nope/":         http.StatusNotFound,
		"/contacts/someone/contacts/": http.StatusNotFound,
		"/contacts/edu/contacts/x/":   http.StatusNotFound,
	} {
		if rec := get(t, h, target, cookie); rec.Code != code {
			t.Errorf("%s = %d, want %d", target, rec.Code, code)
		}
	}
}
