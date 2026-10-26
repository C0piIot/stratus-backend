package dav

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/emersion/go-vcard"
	"github.com/emersion/go-webdav"
	"github.com/emersion/go-webdav/carddav"

	"github.com/stratus-private-cloud/stratus-backend/internal/contacts"
	"github.com/stratus-private-cloud/stratus-backend/internal/db"
)

// CardDAV serves address books (#423), the calendars' twin in every respect
// the library lets it be: the same four depths below the prefix --
//
//	/contacts/                       the principal: whoever is asking
//	/contacts/<owner>/               their address books
//	/contacts/<owner>/<book>/        one address book
//	/contacts/<owner>/<book>/<name>  one contact
//
// -- the same slash given to a collection before the library compares it with
// ==, and the same sync-collection answered in front of it. What differs is
// what the library does itself: it routes the extended MKCOL of RFC 5689 and
// the DELETE of an address book to the backend, so neither is intercepted
// here, while a PROPPATCH of one is a 405 there and is answered here. A GET
// of a contact is answered here too, from the bytes stored: the library
// encodes the card again with go-vcard, whose parameters come out in map
// order, so the body would not be the bytes the ETag is a digest of.
func CardDAV(prefix string, service *contacts.Service) http.Handler {
	b := &contactsBackend{prefix: prefix, contacts: service}
	h := &carddav.Handler{Backend: b}
	bare := strings.TrimSuffix(prefix, "/")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Count(strings.Trim(strings.TrimPrefix(r.URL.Path, bare), "/"), "/") > 2 {
			http.NotFound(w, r)
			return
		}
		if p := r.URL.Path; !strings.HasSuffix(p, "/") && strings.HasPrefix(p+"/", prefix) &&
			strings.Count(strings.TrimPrefix(p, bare), "/") < 3 {
			r = r.Clone(r.Context())
			r.URL.Path = p + "/"
			r.URL.RawPath = ""
		}
		switch {
		case r.Method == "REPORT":
			b.report(w, r, h)
		case r.Method == "PROPFIND":
			propfindExtras(w, r, h, addressBookExtras, b.addressBookValues)
		case r.Method == "PROPPATCH" && b.isAddressBook(r.URL.Path):
			serve(w, b.proppatch(w, r))
		case (r.Method == http.MethodGet || r.Method == http.MethodHead) && !strings.HasSuffix(r.URL.Path, "/"):
			serve(w, b.get(w, r))
		default:
			h.ServeHTTP(w, r)
		}
	})
}

var (
	propAddressData        = xml.Name{Space: nsCardDAV, Local: "address-data"}
	propAddressDescription = xml.Name{Space: nsCardDAV, Local: "addressbook-description"}
)

const nsCardDAV = "urn:ietf:params:xml:ns:carddav"

// addressBookExtras is what an address book adds to the library's PROPFIND:
// the properties sync-collection needs.
var addressBookExtras = []xml.Name{propSyncToken, propCTag, propSupportedReport}

// addressBookReports is what an address book answers.
const addressBookReports = `<supported-report xmlns="DAV:"><report><addressbook-query xmlns="urn:ietf:params:xml:ns:carddav"/></report></supported-report>` +
	`<supported-report xmlns="DAV:"><report><addressbook-multiget xmlns="urn:ietf:params:xml:ns:carddav"/></report></supported-report>` +
	`<supported-report xmlns="DAV:"><report><sync-collection/></report></supported-report>`

type contactsBackend struct {
	prefix   string
	contacts *contacts.Service
}

// report answers sync-collection here and hands every other REPORT to the
// library.
func (b *contactsBackend) report(w http.ResponseWriter, r *http.Request, next http.Handler) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "cannot read the request", http.StatusBadRequest)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	var req syncCollection
	if xml.Unmarshal(body, &req) != nil {
		next.ServeHTTP(w, r)
		return
	}
	if err := b.serveSync(w, r, req); err != nil {
		serveCalendarError(w, err)
	}
}

// serveSync answers a sync-collection REPORT on an address book.
func (b *contactsBackend) serveSync(w http.ResponseWriter, r *http.Request, req syncCollection) error {
	ctx := r.Context()
	owner, book, _, err := b.split(ctx, r.URL.Path, 2)
	if err != nil {
		return err
	}
	if req.SyncLevel != "1" {
		return precondition(http.StatusForbidden, nsDAV, "sync-level")
	}
	ab, err := b.contacts.AddressBook(ctx, owner, book)
	if err != nil {
		return mapContactsErr(ctx, err)
	}
	since, ok := parseSyncToken(req.SyncToken, ab.ID)
	if !ok {
		return precondition(http.StatusForbidden, nsDAV, "valid-sync-token")
	}
	changes, ab, err := b.contacts.Changes(ctx, owner, book, since)
	if errors.Is(err, contacts.ErrStaleToken) {
		return precondition(http.StatusForbidden, nsDAV, "valid-sync-token")
	}
	if err != nil {
		return mapContactsErr(ctx, err)
	}
	return writeSyncFeed(w, syncFeed{
		base: b.home(owner) + book + "/", changes: changes, token: syncToken(ab.ID, ab.SyncToken),
		item: func(n string) (syncItem, error) {
			c, err := b.contacts.Contact(ctx, owner, book, n)
			if err != nil {
				return syncItem{}, err
			}
			return syncItem{etag: c.ETag, data: c.Data, changed: c.Changed, dataName: propAddressData, mime: vcard.MIMEType}, nil
		},
	}, req.Prop, func(err error) error { return mapContactsErr(ctx, err) })
}

// addressBookValues are an address book's extra properties, and none for a
// path that is not one.
func (b *contactsBackend) addressBookValues(r *http.Request, p string) (map[xml.Name]string, error) {
	owner, book, _, err := b.split(r.Context(), p, 2)
	if err != nil {
		return nil, nil //nolint:nilerr // the home set or the principal
	}
	ab, err := b.contacts.AddressBook(r.Context(), owner, book)
	if err != nil {
		return nil, mapContactsErr(r.Context(), err)
	}
	token := string(escapeText(syncToken(ab.ID, ab.SyncToken)))
	return map[xml.Name]string{propSyncToken: token, propCTag: token, propSupportedReport: addressBookReports}, nil
}

func (b *contactsBackend) isAddressBook(p string) bool {
	rest, ok := strings.CutPrefix(p, b.prefix)
	return ok && strings.Count(strings.Trim(rest, "/"), "/") == 1
}

// proppatch renames and describes an address book, all or nothing, as a
// calendar's does.
func (b *contactsBackend) proppatch(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	owner, book, _, err := b.split(ctx, r.URL.Path, 2)
	if err != nil {
		return err
	}
	var body propertyUpdate
	if err = xml.NewDecoder(r.Body).Decode(&body); err != nil {
		return precondition(http.StatusBadRequest, nsDAV, "propertyupdate")
	}

	var patch contacts.Patch
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
			case propAddressDescription:
				patch.Description = &value
			default:
				refused = append(refused, p.XMLName)
			}
		}
	}

	if len(refused) == 0 {
		if _, err = b.contacts.Update(ctx, owner, book, patch); err != nil {
			return mapContactsErr(ctx, err)
		}
	}
	return writeMultistatus(w, syncMultistatus{Responses: []syncResponse{
		patchResponse(hrefOf(r.URL.Path), named, refused),
	}})
}

// CurrentUserPrincipal implements carddav.Backend.
func (b *contactsBackend) CurrentUserPrincipal(ctx context.Context) (string, error) {
	if _, err := calendarOwner(ctx); err != nil {
		return "", err
	}
	return b.prefix, nil
}

// AddressBookHomeSetPath implements carddav.Backend.
func (b *contactsBackend) AddressBookHomeSetPath(ctx context.Context) (string, error) {
	owner, err := calendarOwner(ctx)
	if err != nil {
		return "", err
	}
	return b.home(owner), nil
}

// ListAddressBooks implements carddav.Backend.
func (b *contactsBackend) ListAddressBooks(ctx context.Context) ([]carddav.AddressBook, error) {
	owner, err := calendarOwner(ctx)
	if err != nil {
		return nil, err
	}
	list, err := b.contacts.AddressBooks(ctx, owner)
	if err != nil {
		return nil, mapContactsErr(ctx, err)
	}
	out := make([]carddav.AddressBook, 0, len(list))
	for _, ab := range list {
		out = append(out, b.addressBook(ab))
	}
	return out, nil
}

// GetAddressBook implements carddav.Backend.
func (b *contactsBackend) GetAddressBook(ctx context.Context, p string) (*carddav.AddressBook, error) {
	owner, book, _, err := b.split(ctx, p, 2)
	if err != nil {
		return nil, err
	}
	ab, err := b.contacts.AddressBook(ctx, owner, book)
	if err != nil {
		return nil, mapContactsErr(ctx, err)
	}
	out := b.addressBook(ab)
	return &out, nil
}

// CreateAddressBook implements carddav.Backend, for the extended MKCOL of RFC
// 5689.
func (b *contactsBackend) CreateAddressBook(ctx context.Context, ab *carddav.AddressBook) error {
	owner, book, _, err := b.split(ctx, ab.Path, 2)
	if err != nil {
		return err
	}
	_, err = b.contacts.Create(ctx, owner, db.AddressBook{Name: book, DisplayName: ab.Name, Description: ab.Description})
	if errors.Is(err, db.ErrConflict) {
		return webdav.NewHTTPError(http.StatusMethodNotAllowed, err)
	}
	return mapContactsErr(ctx, err)
}

// DeleteAddressBook implements carddav.Backend.
func (b *contactsBackend) DeleteAddressBook(ctx context.Context, p string) error {
	owner, book, _, err := b.split(ctx, p, 2)
	if err != nil {
		return err
	}
	return mapContactsErr(ctx, b.contacts.DeleteAddressBook(ctx, owner, book))
}

// GetAddressObject implements carddav.Backend. The request for some of the
// properties is not honoured, as a calendar object's is not: a client that
// asked for less can ignore the rest.
func (b *contactsBackend) GetAddressObject(ctx context.Context, p string, _ *carddav.AddressDataRequest) (*carddav.AddressObject, error) {
	owner, book, name, err := b.split(ctx, p, 3)
	if err != nil {
		return nil, err
	}
	c, err := b.contacts.Contact(ctx, owner, book, name)
	if err != nil {
		return nil, mapContactsErr(ctx, err)
	}
	out, err := b.object(ctx, owner, book, c)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (b *contactsBackend) get(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	owner, book, name, err := b.split(ctx, r.URL.Path, 3)
	if err != nil {
		return err
	}
	c, err := b.contacts.Contact(ctx, owner, book, name)
	if err != nil {
		return mapContactsErr(ctx, err)
	}
	w.Header().Set("Content-Type", vcard.MIMEType)
	w.Header().Set("Content-Length", strconv.Itoa(len(c.Data)))
	w.Header().Set("ETag", `"`+c.ETag+`"`)
	w.Header().Set("Last-Modified", c.Changed.UTC().Format(http.TimeFormat))
	if r.Method == http.MethodGet {
		_, _ = io.WriteString(w, c.Data)
	}
	return nil
}

// ListAddressObjects implements carddav.Backend.
func (b *contactsBackend) ListAddressObjects(ctx context.Context, p string, _ *carddav.AddressDataRequest) ([]carddav.AddressObject, error) {
	owner, book, _, err := b.split(ctx, p, 2)
	if err != nil {
		return nil, err
	}
	list, err := b.contacts.Contacts(ctx, owner, book)
	if err != nil {
		return nil, mapContactsErr(ctx, err)
	}
	out := make([]carddav.AddressObject, 0, len(list))
	for _, c := range list {
		ao, err := b.object(ctx, owner, book, c)
		if err != nil {
			return nil, err
		}
		out = append(out, ao)
	}
	return out, nil
}

// QueryAddressObjects implements carddav.Backend: the library's own matcher
// over the whole address book, which is a few hundred vCards at most.
//
// It is asked about folded copies, because it compares as the bytes are and
// RFC 6352 8.3 makes i;unicode-casemap the default: a search for "turing"
// finds Turing. The originals are what is answered.
func (b *contactsBackend) QueryAddressObjects(ctx context.Context, p string, query *carddav.AddressBookQuery) ([]carddav.AddressObject, error) {
	all, err := b.ListAddressObjects(ctx, p, nil)
	if err != nil {
		return nil, err
	}
	folded := make([]carddav.AddressObject, len(all))
	byPath := make(map[string]carddav.AddressObject, len(all))
	for i, ao := range all {
		folded[i] = ao
		folded[i].Card = foldCard(ao.Card)
		byPath[ao.Path] = ao
	}
	matched, err := carddav.Filter(foldQuery(query), folded)
	if err != nil {
		return nil, webdav.NewHTTPError(http.StatusBadRequest, err)
	}
	out := make([]carddav.AddressObject, 0, len(matched))
	for _, ao := range matched {
		out = append(out, byPath[ao.Path])
	}
	return out, nil
}

func foldCard(card vcard.Card) vcard.Card {
	out := make(vcard.Card, len(card))
	for k, fields := range card {
		copied := make([]*vcard.Field, len(fields))
		for i, f := range fields {
			c := *f
			c.Value = strings.ToLower(f.Value)
			c.Params = make(vcard.Params, len(f.Params))
			for name, values := range f.Params {
				lowered := make([]string, len(values))
				for j, v := range values {
					lowered[j] = strings.ToLower(v)
				}
				c.Params[name] = lowered
			}
			copied[i] = &c
		}
		out[k] = copied
	}
	return out
}

func foldQuery(query *carddav.AddressBookQuery) *carddav.AddressBookQuery {
	if query == nil {
		return nil
	}
	q := *query
	q.PropFilters = make([]carddav.PropFilter, len(query.PropFilters))
	for i, pf := range query.PropFilters {
		pf.TextMatches = foldMatches(pf.TextMatches)
		params := make([]carddav.ParamFilter, len(pf.Params))
		for j, pp := range pf.Params {
			if pp.TextMatch != nil {
				m := foldMatches([]carddav.TextMatch{*pp.TextMatch})[0]
				pp.TextMatch = &m
			}
			params[j] = pp
		}
		pf.Params = params
		q.PropFilters[i] = pf
	}
	return &q
}

func foldMatches(matches []carddav.TextMatch) []carddav.TextMatch {
	out := make([]carddav.TextMatch, len(matches))
	for i, m := range matches {
		m.Text = strings.ToLower(m.Text)
		out[i] = m
	}
	return out
}

// PutAddressObject implements carddav.Backend. The library only forwards the
// conditional headers, so they are compared here, inside the write.
func (b *contactsBackend) PutAddressObject(ctx context.Context, p string, card vcard.Card, opts *carddav.PutAddressObjectOptions) (*carddav.AddressObject, error) {
	owner, book, name, err := b.split(ctx, p, 3)
	if err != nil {
		return nil, err
	}
	var refused error
	check := func(etag string, exists bool) error {
		if opts != nil {
			refused = checkConditions(opts.IfMatch, opts.IfNoneMatch, etag, exists)
		}
		return refused
	}
	c, err := b.contacts.Put(ctx, owner, book, name, card, check)
	if refused != nil {
		return nil, refused
	}
	if err != nil {
		return nil, mapContactsErr(ctx, err)
	}
	out, err := b.object(ctx, owner, book, c)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteAddressObject implements carddav.Backend.
func (b *contactsBackend) DeleteAddressObject(ctx context.Context, p string) error {
	owner, book, name, err := b.split(ctx, p, 3)
	if err != nil {
		return err
	}
	return mapContactsErr(ctx, b.contacts.Delete(ctx, owner, book, name))
}

func (b *contactsBackend) home(owner string) string { return b.prefix + owner + "/" }

// addressBook is a row as the library wants it. Both vCard versions are
// offered, because a client writes the one it is told it may and the library's
// own default is 3.0 alone.
func (b *contactsBackend) addressBook(ab db.AddressBook) carddav.AddressBook {
	return carddav.AddressBook{
		Path:        b.home(ab.OwnerID) + ab.Name + "/",
		Name:        ab.DisplayName,
		Description: ab.Description,
		SupportedAddressData: []carddav.AddressDataType{
			{ContentType: vcard.MIMEType, Version: "3.0"},
			{ContentType: vcard.MIMEType, Version: "4.0"},
		},
	}
}

func (b *contactsBackend) object(ctx context.Context, owner, book string, c db.Contact) (carddav.AddressObject, error) {
	card, err := contacts.Decode(c)
	if err != nil {
		return carddav.AddressObject{}, mapContactsErr(ctx, err)
	}
	return carddav.AddressObject{
		Path:          b.home(owner) + book + "/" + c.Name,
		ModTime:       c.Changed,
		ContentLength: int64(len(c.Data)),
		ETag:          c.ETag,
		Card:          card,
	}, nil
}

func (b *contactsBackend) split(ctx context.Context, p string, segments int) (owner, book, name string, err error) {
	return splitOwned(ctx, b.prefix, p, segments)
}

// mapContactsErr is mapErr with CardDAV's own preconditions in front of it.
func mapContactsErr(ctx context.Context, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, contacts.ErrInvalid):
		return carddav.NewPreconditionError(carddav.PreconditionValidAddressData)
	case errors.Is(err, contacts.ErrDefault):
		return webdav.NewHTTPError(http.StatusForbidden, err)
	case errors.Is(err, db.ErrConflict):
		return carddav.NewPreconditionError(carddav.PreconditionNoUIDConflict)
	}
	return mapErr(ctx, err)
}
