package web

import (
	"context"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/emersion/go-vcard"

	"github.com/C0piIot/stratus-backend/internal/contacts"
	"github.com/C0piIot/stratus-backend/internal/db"
)

// The contacts (#423): one address book, read-only, at the address CardDAV
// serves it at, as a calendar's month is (#111) -- split by method in the
// composition root, a contact's own address staying CardDAV's for a GET, and
// no script. A list by name rather than a month, under a letter each, and the
// same list on every width.
//
// **The photograph is not drawn.** It is inside the vCard, so drawing it would
// mean a data: URI the page's img-src does not allow, or an address of its own
// to serve it from; neither is worth it for a page that is read to find a
// number.
const contactsPrefix = "/contacts/"

// Contacts is the pages' view of the address books: internal/contacts'
// service, so the default address book a client finds is the one a page finds
// and a contact written here is one a client syncs like its own.
type Contacts interface {
	AddressBooks(ctx context.Context, owner string) ([]db.AddressBook, error)
	AddressBook(ctx context.Context, owner, name string) (db.AddressBook, error)
	Contacts(ctx context.Context, owner, book string) ([]db.Contact, error)
	Contact(ctx context.Context, owner, book, name string) (db.Contact, error)
	Put(ctx context.Context, owner, book, name string, card vcard.Card, check contacts.Precondition) (db.Contact, error)
	Delete(ctx context.Context, owner, book, name string) error
}

// contactsView is one address book.
type contactsView struct {
	Name    string
	New     string
	Count   int
	Letters []contactLetter
	Others  []calendarLink
}

type contactLetter struct {
	Letter   string
	Contacts []contactRow
}

type contactRow struct {
	Edit             string
	Name, Org, Title string
	Emails           []string
	Phones           []contactPhone
	Addresses        []string
	Birthday, Note   string
}

type contactPhone struct {
	Number, Kind string
	Href         template.URL
}

// contactPages is every address under /contacts/ that a browser asks for.
func (h *handler) contactPages(w http.ResponseWriter, r *http.Request, user string) {
	parts := strings.Split(strings.Trim(r.PathValue("path"), "/"), "/")
	switch {
	case parts[0] == "":
		redirectLocal(w, r, contactsPrefix+url.PathEscape(user)+"/")
	case parts[0] != user || len(parts) > 2:
		h.fail(w, r, user, db.ErrNotFound)
	case len(parts) == 1:
		redirectLocal(w, r, addressBookHref(user, contacts.DefaultName))
	default:
		h.addressBook(w, r, user, parts[1])
	}
}

func (h *handler) addressBook(w http.ResponseWriter, r *http.Request, user, name string) {
	ctx := r.Context()
	b, err := h.contacts.AddressBook(ctx, user, name)
	if err != nil {
		h.fail(w, r, user, err)
		return
	}
	list, err := h.contacts.Contacts(ctx, user, name)
	if err != nil {
		h.fail(w, r, user, err)
		return
	}

	rows := make([]contactRow, 0, len(list))
	for _, c := range list {
		card, err := contacts.Decode(c)
		if err != nil {
			h.fail(w, r, user, err)
			return
		}
		row := contactRowOf(card)
		row.Edit = contactHref(contactEditPrefix, name, c.Name)
		rows = append(rows, row)
	}
	slices.SortStableFunc(rows, func(a, b contactRow) int {
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})

	cv := &contactsView{Name: bookName(b), New: contactHref(contactNewPrefix, name, ""), Count: len(rows)}
	for _, row := range rows {
		letter := initial(row.Name)
		if n := len(cv.Letters); n == 0 || cv.Letters[n-1].Letter != letter {
			cv.Letters = append(cv.Letters, contactLetter{Letter: letter})
		}
		last := &cv.Letters[len(cv.Letters)-1]
		last.Contacts = append(last.Contacts, row)
	}
	if all, err := h.contacts.AddressBooks(ctx, user); err == nil && len(all) > 1 {
		for _, other := range all {
			cv.Others = append(cv.Others, calendarLink{
				Name: bookName(other), Href: addressBookHref(user, other.Name), Current: other.Name == name,
			})
		}
	}
	h.render(w, http.StatusOK, pageContacts, view{Title: cv.Name, User: user, Contacts: cv})
}

// contactRowOf is what a list says about one contact: enough to reach them.
func contactRowOf(card vcard.Card) contactRow {
	row := contactRow{
		Name:     card.PreferredValue(vcard.FieldFormattedName),
		Org:      joinParts(card.PreferredValue(vcard.FieldOrganization)),
		Title:    card.PreferredValue(vcard.FieldTitle),
		Emails:   card.Values(vcard.FieldEmail),
		Birthday: card.PreferredValue(vcard.FieldBirthday),
		Note:     card.PreferredValue(vcard.FieldNote),
	}
	for _, f := range card[vcard.FieldTelephone] {
		number := strings.TrimPrefix(f.Value, "tel:")
		row.Phones = append(row.Phones, contactPhone{
			Number: number, Kind: phoneKind(f), Href: telHref(number),
		})
	}
	for _, a := range card.Addresses() {
		parts := []string{a.StreetAddress, a.ExtendedAddress, a.PostalCode, a.Locality, a.Region, a.Country}
		if line := strings.Join(slices.DeleteFunc(parts, func(s string) bool { return s == "" }), ", "); line != "" {
			row.Addresses = append(row.Addresses, line)
		}
	}
	return row
}

// phoneKind is the first type a number carries that a person would say --
// cell, home, work -- and nothing for the ones that describe the line.
func phoneKind(f *vcard.Field) string {
	for _, t := range f.Params.Types() {
		switch t = strings.ToLower(t); t {
		case vcard.TypeCell, vcard.TypeHome, vcard.TypeWork, vcard.TypeFax:
			return t
		}
	}
	return ""
}

// telHref is a tel: link, which html/template would otherwise refuse as a
// scheme it does not know. Only a value written the way numbers are -- digits
// and the punctuation around them -- becomes one, and of that only the digits
// and a leading plus reach the attribute. The "(0)" of a number written for
// dialling at home and abroad is dropped, since abroad is what the plus says.
func telHref(number string) template.URL {
	var digits strings.Builder
	for i, r := range strings.ReplaceAll(number, "(0)", "") {
		switch {
		case unicode.IsDigit(r), r == '+' && i == 0:
			digits.WriteRune(r)
		case strings.ContainsRune(" -.()/", r):
		default:
			return ""
		}
	}
	if digits.Len() == 0 {
		return ""
	}
	return template.URL("tel:" + digits.String()) //nolint:gosec // digits and a leading plus, nothing else
}

// joinParts is a structured value -- an ORG is the company then its
// departments -- as one line.
func joinParts(v string) string {
	parts := strings.Split(v, ";")
	return strings.Join(slices.DeleteFunc(parts, func(s string) bool { return s == "" }), " · ")
}

// initial is the heading a name is listed under: its first letter, or # for
// one that does not start with a letter.
func initial(name string) string {
	r, _ := utf8.DecodeRuneInString(name)
	if unicode.IsLetter(r) {
		return string(unicode.ToUpper(r))
	}
	return "#"
}

func bookName(b db.AddressBook) string {
	if b.DisplayName != "" {
		return b.DisplayName
	}
	return b.Name
}

func addressBookHref(user, name string) string {
	return contactsPrefix + url.PathEscape(user) + "/" + url.PathEscape(name) + "/"
}
