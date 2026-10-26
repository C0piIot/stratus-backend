package web

import (
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-vcard"

	"github.com/stratus-private-cloud/stratus-backend/internal/contacts"
	"github.com/stratus-private-cloud/stratus-backend/internal/db"
)

// Writing a contact (#442): three forms over contacts.Service, so what is
// written here a CardDAV client syncs like its own. They are under /contact/
// rather than at the contact's address because that address is CardDAV's for
// every method, as /rename/ and /delete/ are beside /files/.
//
// **An edit is made to the vCard as stored, not rebuilt from the form.** The
// form shows a handful of fields; the photograph a phone put there, a client's
// X- properties and the parameters on a number nobody touched all stay, and a
// field that comes back as it was shown is not written at all. A card rebuilt
// from the form would be every web edit quietly deleting what a phone knew.
const (
	contactNewPrefix    = "/contact/new/"
	contactEditPrefix   = "/contact/edit/"
	contactDeletePrefix = "/contact/delete/"
)

// errChanged is a contact or an event written or deleted elsewhere since its
// form was drawn, which is the form's If-Match failing.
var errChanged = errors.New("web: what the form edits changed since it was drawn")

var (
	phoneKinds = []string{vcard.TypeCell, vcard.TypeHome, vcard.TypeWork, vcard.TypeFax}
	placeKinds = []string{vcard.TypeHome, vcard.TypeWork}
)

// contactForm is one contact as the form shows it, or as it was posted.
type contactForm struct {
	Heading, Action, Back, Delete string
	ETag                          string
	Given, Family, Org, JobTitle  string
	Birthday, Note                string
	// BirthdayText is a birthday the date field cannot hold -- a vCard 4.0
	// one with no year -- shown as the text it is instead.
	BirthdayText bool
	Phones       []contactValue
	Emails       []contactValue
	Addresses    []contactAddress
	PhoneKinds   []string
	PlaceKinds   []string
	NameError    string
	BirthdayErr  string
	// Changed is the contact having been written somewhere else since this
	// form was drawn.
	Changed bool
}

// contactValue is a number or an address. Index is where it is among the
// stored card's fields of its kind, and -1 for one the form added.
type contactValue struct {
	Index       int
	Value, Kind string
	Error       string
}

type contactAddress struct {
	Index                                         int
	Kind                                          string
	Street, Locality, Region, PostalCode, Country string
}

func contactHref(prefix, book, name string) string {
	h := prefix + url.PathEscape(book)
	if name != "" {
		h += "/" + url.PathEscape(name)
	}
	return h
}

func (h *handler) newContactForm(w http.ResponseWriter, r *http.Request, user string) {
	book := r.PathValue("book")
	if _, err := h.contacts.AddressBook(r.Context(), user, book); err != nil {
		h.fail(w, r, user, err)
		return
	}
	h.renderContact(w, http.StatusOK, user, book, "", withBlankRows(&contactForm{}))
}

func (h *handler) editContactForm(w http.ResponseWriter, r *http.Request, user string) {
	book, name := r.PathValue("book"), r.PathValue("name")
	card, etag, err := h.storedContact(r, user, book, name)
	if err != nil {
		h.fail(w, r, user, err)
		return
	}
	f := contactFormOf(card)
	f.ETag = etag
	h.renderContact(w, http.StatusOK, user, book, name, withBlankRows(f))
}

func (h *handler) createContact(w http.ResponseWriter, r *http.Request, user string) {
	h.saveContact(w, r, user, r.PathValue("book"), "")
}

func (h *handler) updateContact(w http.ResponseWriter, r *http.Request, user string) {
	h.saveContact(w, r, user, r.PathValue("book"), r.PathValue("name"))
}

// saveContact writes what was posted onto the card as it is stored, or onto a
// new one when name is empty. A button named "add" asks for one more row of a
// kind instead, which is how a form with no script grows.
func (h *handler) saveContact(w http.ResponseWriter, r *http.Request, user, book, name string) {
	ctx := r.Context()
	posted := postedContact(r)
	if add := r.PostFormValue("add"); add != "" {
		posted.addRow(add)
		h.renderContact(w, http.StatusOK, user, book, name, posted)
		return
	}

	var card vcard.Card
	if name == "" {
		if _, err := h.contacts.AddressBook(ctx, user, book); err != nil {
			h.fail(w, r, user, err)
			return
		}
		card = vcard.Card{}
		// 3.0 because it is what every client reads; iOS is the one that
		// reads 4.0 worst.
		card.SetValue(vcard.FieldVersion, "3.0")
		card.SetName(&vcard.Name{})
		uid := newUID()
		card.SetValue(vcard.FieldUID, uid)
		name = uid + ".vcf"
		posted.ETag = ""
	} else {
		stored, _, err := h.storedContact(r, user, book, name)
		switch {
		case errors.Is(err, db.ErrNotFound):
			posted.Changed = true
			h.renderContact(w, http.StatusConflict, user, book, name, posted)
			return
		case err != nil:
			h.fail(w, r, user, err)
			return
		}
		card = stored
	}

	shown := contactFormOf(card)
	if !posted.check(shown) {
		h.renderContact(w, http.StatusBadRequest, user, book, r.PathValue("name"), posted)
		return
	}
	posted.applyTo(card, shown)

	_, err := h.contacts.Put(ctx, user, book, name, card, func(etag string, exists bool) error {
		if exists != (posted.ETag != "") || etag != posted.ETag {
			return errChanged
		}
		return nil
	})
	switch {
	case errors.Is(err, errChanged):
		posted.Changed = true
		h.renderContact(w, http.StatusConflict, user, book, r.PathValue("name"), posted)
	case err != nil:
		h.fail(w, r, user, err)
	default:
		redirectLocal(w, r, addressBookHref(user, book))
	}
}

func (h *handler) deleteContactForm(w http.ResponseWriter, r *http.Request, user string) {
	book, name := r.PathValue("book"), r.PathValue("name")
	card, _, err := h.storedContact(r, user, book, name)
	if err != nil {
		h.fail(w, r, user, err)
		return
	}
	h.render(w, http.StatusOK, pageUncontact, view{
		Title: "Delete contact", User: user, Name: card.PreferredValue(vcard.FieldFormattedName),
		Action: contactHref(contactDeletePrefix, book, name), Back: contactHref(contactEditPrefix, book, name),
	})
}

func (h *handler) deleteContact(w http.ResponseWriter, r *http.Request, user string) {
	book := r.PathValue("book")
	if err := h.contacts.Delete(r.Context(), user, book, r.PathValue("name")); err != nil {
		h.fail(w, r, user, err)
		return
	}
	redirectLocal(w, r, addressBookHref(user, book))
}

func (h *handler) storedContact(r *http.Request, user, book, name string) (vcard.Card, string, error) {
	c, err := h.contacts.Contact(r.Context(), user, book, name)
	if err != nil {
		return nil, "", err
	}
	card, err := contacts.Decode(c)
	return card, c.ETag, err
}

func (h *handler) renderContact(w http.ResponseWriter, status int, user, book, name string, f *contactForm) {
	f.PhoneKinds, f.PlaceKinds = phoneKinds, placeKinds
	f.Back = addressBookHref(user, book)
	if name == "" {
		f.Heading, f.Action = "New contact", contactHref(contactNewPrefix, book, "")
	} else {
		f.Heading, f.Action = "Edit contact", contactHref(contactEditPrefix, book, name)
		f.Delete = contactHref(contactDeletePrefix, book, name)
	}
	h.render(w, status, pageContact, view{Title: f.Heading, User: user, Contact: f})
}

// contactFormOf is what the form shows of a card, trimmed as a post is so
// that a field nobody touched compares equal. A card with no N shows its FN
// as the given name, for the same reason.
func contactFormOf(card vcard.Card) *contactForm {
	text := func(k string) string { return strings.TrimSpace(card.PreferredValue(k)) }
	f := &contactForm{
		Org:      strings.TrimSpace(strings.Split(card.PreferredValue(vcard.FieldOrganization), ";")[0]),
		JobTitle: text(vcard.FieldTitle),
		Note:     text(vcard.FieldNote),
	}
	if n := card.Name(); n != nil {
		f.Given, f.Family = strings.TrimSpace(n.GivenName), strings.TrimSpace(n.FamilyName)
	} else {
		f.Given = text(vcard.FieldFormattedName)
	}
	if b := card.Value(vcard.FieldBirthday); b != "" {
		f.Birthday, f.BirthdayText = b, true
		if d, ok := parseBirthday(b); ok {
			f.Birthday, f.BirthdayText = d.Format(time.DateOnly), false
		}
	}
	for i, t := range card[vcard.FieldTelephone] {
		f.Phones = append(f.Phones, contactValue{Index: i, Value: strings.TrimSpace(strings.TrimPrefix(t.Value, "tel:")), Kind: kindOf(t, phoneKinds)})
	}
	for i, e := range card[vcard.FieldEmail] {
		f.Emails = append(f.Emails, contactValue{Index: i, Value: strings.TrimSpace(e.Value), Kind: kindOf(e, placeKinds)})
	}
	for i, a := range card.Addresses() {
		f.Addresses = append(f.Addresses, contactAddress{
			Index: i, Kind: kindOf(a.Field, placeKinds), Street: strings.TrimSpace(a.StreetAddress),
			Locality: strings.TrimSpace(a.Locality), Region: strings.TrimSpace(a.Region),
			PostalCode: strings.TrimSpace(a.PostalCode), Country: strings.TrimSpace(a.Country),
		})
	}
	return f
}

func withBlankRows(f *contactForm) *contactForm {
	f.addRow("tel")
	f.addRow("email")
	f.addRow("adr")
	return f
}

func (f *contactForm) addRow(kind string) {
	switch kind {
	case "tel":
		f.Phones = append(f.Phones, contactValue{Index: -1})
	case "email":
		f.Emails = append(f.Emails, contactValue{Index: -1})
	case "adr":
		f.Addresses = append(f.Addresses, contactAddress{Index: -1})
	}
}

// postedContact is the form as it came back, rows with nothing in them
// dropped.
func postedContact(r *http.Request) *contactForm {
	text := func(k string) string { return strings.TrimSpace(r.PostFormValue(k)) }
	f := &contactForm{
		ETag: r.PostFormValue("etag"), Given: text("given"), Family: text("family"), Org: text("org"),
		JobTitle: text("title"), Birthday: text("bday"), BirthdayText: r.PostFormValue("bday-text") != "",
		Note: strings.TrimSpace(strings.ReplaceAll(r.PostFormValue("note"), "\r\n", "\n")),
	}
	column := func(k string, i int) string {
		if v := r.PostForm[k]; i < len(v) {
			return strings.TrimSpace(v[i])
		}
		return ""
	}
	index := func(k string, i int) int {
		n, err := strconv.Atoi(column(k, i))
		if err != nil {
			return -1
		}
		return n
	}
	for i := range r.PostForm["tel"] {
		if v := column("tel", i); v != "" {
			f.Phones = append(f.Phones, contactValue{Index: index("tel-index", i), Value: v, Kind: column("tel-kind", i)})
		}
	}
	for i := range r.PostForm["email"] {
		if v := column("email", i); v != "" {
			f.Emails = append(f.Emails, contactValue{Index: index("email-index", i), Value: v, Kind: column("email-kind", i)})
		}
	}
	for i := range r.PostForm["street"] {
		a := contactAddress{
			Index: index("adr-index", i), Kind: column("adr-kind", i), Street: column("street", i),
			Locality: column("locality", i), Region: column("region", i), PostalCode: column("postcode", i),
			Country: column("country", i),
		}
		if a.Street+a.Locality+a.Region+a.PostalCode+a.Country != "" {
			f.Addresses = append(f.Addresses, a)
		}
	}
	return f
}

// check marks what cannot be stored, and says whether anything was. A
// birthday is read only when it changed, since one the date field could not
// hold comes back as the text it was.
func (f *contactForm) check(shown *contactForm) bool {
	ok := true
	if f.Given == "" && f.Family == "" && f.Org == "" {
		f.NameError, ok = "A contact needs a name or a company.", false
	}
	for i := range f.Emails {
		if v := f.Emails[i].Value; !strings.Contains(v, "@") || strings.ContainsAny(v, " \t") {
			f.Emails[i].Error, ok = v+" is not an email address.", false
		}
	}
	if f.Birthday != "" && f.Birthday != shown.Birthday {
		if _, err := time.Parse(time.DateOnly, f.Birthday); err != nil {
			f.BirthdayErr, ok = "A birthday is a date.", false
		}
	}
	return ok
}

// applyTo writes onto card what differs between the posted form and shown,
// which is the same card drawn as a form. What is equal is left exactly as
// the card had it.
func (f *contactForm) applyTo(card vcard.Card, shown *contactForm) {
	named := f.Given != shown.Given || f.Family != shown.Family
	if named {
		n := card.Name()
		if n == nil {
			n = &vcard.Name{}
		}
		n.GivenName, n.FamilyName = component(f.Given), component(f.Family)
		card.SetName(n)
	}
	// The display name follows the name unless somebody gave it one of its
	// own, which a client can and this form does not offer.
	fn := card.PreferredValue(vcard.FieldFormattedName)
	if named || fn == "" || (f.Org != shown.Org && fn == composedName(shown)) {
		setText(card, vcard.FieldFormattedName, composedName(f))
	}
	if f.Org != shown.Org {
		if org := card.Preferred(vcard.FieldOrganization); org != nil && f.Org != "" {
			parts := strings.Split(org.Value, ";")
			parts[0] = component(f.Org)
			org.Value = strings.Join(parts, ";")
		} else {
			setText(card, vcard.FieldOrganization, component(f.Org))
		}
	}
	if f.JobTitle != shown.JobTitle {
		setText(card, vcard.FieldTitle, f.JobTitle)
	}
	if f.Note != shown.Note {
		setText(card, vcard.FieldNote, f.Note)
	}
	if f.Birthday != shown.Birthday {
		b := f.Birthday
		if d, err := time.Parse(time.DateOnly, b); err == nil && card.Value(vcard.FieldVersion) == "4.0" {
			b = d.Format("20060102")
		}
		setText(card, vcard.FieldBirthday, b)
	}

	applyValues(card, vcard.FieldTelephone, f.Phones, shown.Phones, phoneKinds, func(fd *vcard.Field, v string) {
		// Free text and not a tel: URI, which vCard 4.0 allows for the
		// number's sake and which a number typed with spaces would not be.
		fd.Value = v
		delete(fd.Params, vcard.ParamValue)
	})
	applyValues(card, vcard.FieldEmail, f.Emails, shown.Emails, placeKinds, func(fd *vcard.Field, v string) { fd.Value = v })

	stored := card.Addresses()
	out := make([]*vcard.Field, 0, len(f.Addresses))
	used := map[int]bool{}
	for _, a := range f.Addresses {
		var fd *vcard.Field
		var was contactAddress
		if a.Index >= 0 && a.Index < len(stored) && !used[a.Index] {
			used[a.Index] = true
			fd, was = stored[a.Index].Field, shown.Addresses[a.Index]
		} else {
			fd, was = &vcard.Field{Params: vcard.Params{}}, contactAddress{Index: -1}
		}
		if a.Street != was.Street || a.Locality != was.Locality || a.Region != was.Region ||
			a.PostalCode != was.PostalCode || a.Country != was.Country {
			box, extended := "", ""
			if was.Index >= 0 {
				box, extended = stored[a.Index].PostOfficeBox, stored[a.Index].ExtendedAddress
			}
			fd.Value = strings.Join([]string{
				box, extended, component(a.Street), component(a.Locality), component(a.Region),
				component(a.PostalCode), component(a.Country),
			}, ";")
		}
		if a.Kind != was.Kind {
			setKind(fd, a.Kind, placeKinds)
		}
		out = append(out, fd)
	}
	setFields(card, vcard.FieldAddress, out)
}

// applyValues is the numbers or the email addresses: every posted row is a
// field, the stored one it came from when it has an index, and a stored field
// with no row left was cleared.
func applyValues(card vcard.Card, key string, rows, shown []contactValue, kinds []string, set func(*vcard.Field, string)) {
	stored := card[key]
	out := make([]*vcard.Field, 0, len(rows))
	used := map[int]bool{}
	for _, row := range rows {
		var fd *vcard.Field
		was := contactValue{Index: -1}
		if row.Index >= 0 && row.Index < len(stored) && !used[row.Index] {
			used[row.Index] = true
			fd, was = stored[row.Index], shown[row.Index]
		} else {
			fd = &vcard.Field{Params: vcard.Params{}}
		}
		if row.Value != was.Value {
			set(fd, row.Value)
		}
		if row.Kind != was.Kind {
			setKind(fd, row.Kind, kinds)
		}
		out = append(out, fd)
	}
	setFields(card, key, out)
}

func setFields(card vcard.Card, key string, fields []*vcard.Field) {
	if len(fields) == 0 {
		delete(card, key)
		return
	}
	card[key] = fields
}

// setText sets the preferred field of a kind, keeping its parameters, or
// removes it when v is empty.
func setText(card vcard.Card, key, v string) {
	fd := card.Preferred(key)
	switch {
	case v == "" && fd != nil:
		setFields(card, key, slices.DeleteFunc(card[key], func(x *vcard.Field) bool { return x == fd }))
	case v == "":
	case fd != nil:
		fd.Value = v
	default:
		card.AddValue(key, v)
	}
}

// kindOf is the first of kinds a field's TYPE names, or nothing.
func kindOf(fd *vcard.Field, kinds []string) string {
	for _, t := range fd.Params.Types() {
		if t = strings.ToLower(t); slices.Contains(kinds, t) {
			return t
		}
	}
	return ""
}

// setKind replaces whichever of kinds a field's TYPE names with kind, and
// leaves the rest of it -- voice, pref -- as it was.
func setKind(fd *vcard.Field, kind string, kinds []string) {
	if fd.Params == nil {
		fd.Params = vcard.Params{}
	}
	types := slices.DeleteFunc(slices.Clone(fd.Params[vcard.ParamType]), func(t string) bool {
		return slices.Contains(kinds, strings.ToLower(t))
	})
	if slices.Contains(kinds, kind) {
		types = append(types, kind)
	}
	if len(types) == 0 {
		delete(fd.Params, vcard.ParamType)
		return
	}
	fd.Params[vcard.ParamType] = types
}

// component is a value that goes inside a structured one, where a semicolon
// is the separator and go-vcard writes it unescaped.
func component(v string) string {
	return strings.ReplaceAll(v, ";", ",")
}

func composedName(f *contactForm) string {
	if n := strings.TrimSpace(f.Given + " " + f.Family); n != "" {
		return n
	}
	return f.Org
}

// parseBirthday reads the two ways a full date is written, vCard 4.0's and
// 3.0's.
func parseBirthday(v string) (time.Time, bool) {
	for _, layout := range []string{"20060102", time.DateOnly} {
		if d, err := time.Parse(layout, v); err == nil {
			return d, true
		}
	}
	return time.Time{}, false
}

// newUID is a random UUID, which is what RFC 6350 asks a UID to be.
func newUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
