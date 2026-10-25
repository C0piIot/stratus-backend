// Package contacts owns what a contact is (#423): the vCard an address book
// accepts, the one form it is stored in, and the write that checks a client's
// precondition and stores in one transaction. It is internal/calendar's twin,
// and exists for the same reason: a conditional PUT is a read and a write, and
// an inbound adapter has no transaction to put them in.
package contacts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/emersion/go-vcard"

	"github.com/C0piIot/stratus-backend/internal/db"
)

var (
	// ErrInvalid is a vCard an address book cannot hold: no UID, no FN, or
	// text that does not encode.
	ErrInvalid = errors.New("contacts: not a contact")
	// ErrStaleToken is a sync token this server cannot answer from, as a
	// calendar's: the client lists the address book again.
	ErrStaleToken = errors.New("contacts: sync token no longer answerable")
	// ErrDefault is a refusal to delete the default address book, which the
	// next look would make again under the client's feet.
	ErrDefault = errors.New("contacts: the default address book stays")
)

// TombstoneRetention is how long a deletion is remembered for a client that
// syncs by token, the calendars' month.
const TombstoneRetention = 30 * 24 * time.Hour

// DefaultName is the address book every owner has, made on the first look so
// that a client that never makes one still has somewhere to write.
const DefaultName = "contacts"

// maxName bounds a name to what every driver can hold as a key.
const maxName = 255

// Database is what this package needs from the metadata seam.
type Database interface {
	db.Contacts
	Tx(ctx context.Context, fn func(db.Repo) error) error
}

// Precondition is a client's If-Match and If-None-Match, asked about the
// contact as it is inside the write's transaction.
type Precondition func(etag string, exists bool) error

// Service is the contacts layer.
type Service struct {
	meta Database
	now  func() time.Time
}

// New wires it to the database.
func New(meta Database) *Service { return &Service{meta: meta, now: time.Now} }

// AddressBooks lists an owner's address books, making the default one first
// if there are none.
func (s *Service) AddressBooks(ctx context.Context, owner string) ([]db.AddressBook, error) {
	list, err := s.meta.AddressBooks(ctx, owner)
	if err != nil || len(list) > 0 {
		return list, err
	}
	if _, err := s.ensureDefault(ctx, owner); err != nil {
		return nil, err
	}
	return s.meta.AddressBooks(ctx, owner)
}

// AddressBook returns one of an owner's address books, or db.ErrNotFound. The
// default one is never not found.
func (s *Service) AddressBook(ctx context.Context, owner, name string) (db.AddressBook, error) {
	b, err := s.meta.AddressBookByName(ctx, owner, name)
	if errors.Is(err, db.ErrNotFound) && name == DefaultName {
		return s.ensureDefault(ctx, owner)
	}
	return b, err
}

// Create makes an address book, or answers db.ErrConflict when the name is
// taken. Only Name, DisplayName and Description are read from b.
func (s *Service) Create(ctx context.Context, owner string, b db.AddressBook) (db.AddressBook, error) {
	if err := validName(b.Name); err != nil {
		return db.AddressBook{}, err
	}
	return s.meta.CreateAddressBook(ctx, db.AddressBook{
		OwnerID: owner, Name: b.Name, DisplayName: b.DisplayName, Description: b.Description, Created: s.now(),
	})
}

// Patch is what a client may change about an address book, each field nil for
// leave it as it is.
type Patch struct {
	DisplayName, Description *string
}

// Update applies p to one of an owner's address books.
func (s *Service) Update(ctx context.Context, owner, name string, p Patch) (db.AddressBook, error) {
	if err := s.ensure(ctx, owner, name); err != nil {
		return db.AddressBook{}, err
	}
	var out db.AddressBook
	err := s.meta.Tx(ctx, func(r db.Repo) error {
		b, err := r.AddressBookByName(ctx, owner, name)
		if err != nil {
			return err
		}
		if p.DisplayName != nil {
			b.DisplayName = *p.DisplayName
		}
		if p.Description != nil {
			b.Description = *p.Description
		}
		out = b
		return r.UpdateAddressBook(ctx, b)
	})
	return out, err
}

// DeleteAddressBook removes one of an owner's address books and everything in
// it. The default one is refused: see ErrDefault.
func (s *Service) DeleteAddressBook(ctx context.Context, owner, name string) error {
	if name == DefaultName {
		return ErrDefault
	}
	return s.meta.DeleteAddressBook(ctx, owner, name)
}

// Contacts lists what is in one of an owner's address books.
func (s *Service) Contacts(ctx context.Context, owner, book string) ([]db.Contact, error) {
	b, err := s.AddressBook(ctx, owner, book)
	if err != nil {
		return nil, err
	}
	return s.meta.Contacts(ctx, b.ID)
}

// Contact returns one contact, or db.ErrNotFound.
func (s *Service) Contact(ctx context.Context, owner, book, name string) (db.Contact, error) {
	b, err := s.AddressBook(ctx, owner, book)
	if err != nil {
		return db.Contact{}, err
	}
	return s.meta.Contact(ctx, b.ID, name)
}

// Put stores card under name in one of an owner's address books, if check
// agrees with what is there. What is stored is card encoded again, so that the
// ETag summarises what a GET returns, which is the library encoding what it
// parsed.
func (s *Service) Put(ctx context.Context, owner, book, name string, card vcard.Card, check Precondition) (db.Contact, error) {
	if err := validName(name); err != nil {
		return db.Contact{}, err
	}
	uid, err := validate(card)
	if err != nil {
		return db.Contact{}, err
	}
	data, err := Encode(card)
	if err != nil {
		return db.Contact{}, err
	}
	sum := sha256.Sum256([]byte(data))
	if err = s.ensure(ctx, owner, book); err != nil {
		return db.Contact{}, err
	}

	var out db.Contact
	err = s.meta.Tx(ctx, func(r db.Repo) error {
		b, txErr := r.AddressBookByName(ctx, owner, book)
		if txErr != nil {
			return txErr
		}
		existing, txErr := r.Contact(ctx, b.ID, name)
		exists := txErr == nil
		if txErr != nil && !errors.Is(txErr, db.ErrNotFound) {
			return txErr
		}
		if check != nil {
			if txErr = check(existing.ETag, exists); txErr != nil {
				return txErr
			}
		}
		out, txErr = r.PutContact(ctx, db.Contact{
			AddressBookID: b.ID, Name: name, UID: uid, Data: data, ETag: hex.EncodeToString(sum[:]), Changed: s.now(),
		})
		return txErr
	})
	if err != nil {
		return db.Contact{}, err
	}
	return out, nil
}

// Delete removes one contact, or answers db.ErrNotFound.
func (s *Service) Delete(ctx context.Context, owner, book, name string) error {
	if err := s.ensure(ctx, owner, book); err != nil {
		return err
	}
	return s.meta.Tx(ctx, func(r db.Repo) error {
		b, err := r.AddressBookByName(ctx, owner, book)
		if err != nil {
			return err
		}
		return r.DeleteContact(ctx, b.ID, name, s.now())
	})
}

// Changes returns what changed in one of an owner's address books after the
// token since, and the token to come back with, as calendar.Changes does.
func (s *Service) Changes(ctx context.Context, owner, book string, since int64) ([]db.ContactChange, db.AddressBook, error) {
	b, err := s.AddressBook(ctx, owner, book)
	if err != nil {
		return nil, db.AddressBook{}, err
	}
	if since < 0 || since > b.SyncToken || (since > 0 && since < b.PrunedToken) {
		return nil, b, ErrStaleToken
	}
	changes, err := s.meta.ContactChanges(ctx, b.ID, since)
	if err != nil {
		return nil, b, err
	}
	out := changes[:0]
	for _, ch := range changes {
		b.SyncToken = max(b.SyncToken, ch.SyncToken)
		if since == 0 && ch.Deleted {
			continue
		}
		out = append(out, ch)
	}
	return out, b, nil
}

// Prune forgets the deletions older than TombstoneRetention, and says how
// many.
func (s *Service) Prune(ctx context.Context) (int64, error) {
	var n int64
	err := s.meta.Tx(ctx, func(r db.Repo) error {
		var err error
		n, err = r.PruneContactTombstones(ctx, s.now().Add(-TombstoneRetention))
		return err
	})
	return n, err
}

// Decode parses what Put stored.
func Decode(c db.Contact) (vcard.Card, error) {
	card, err := vcard.NewDecoder(strings.NewReader(c.Data)).Decode()
	if err != nil {
		return nil, fmt.Errorf("contacts: decode %q: %w", c.Name, err)
	}
	return card, nil
}

// Encode is the one form a contact is stored and served in: go-vcard's
// encoder but for one thing, a field's parameters in order of name. The
// library ranges over the map, so a number with a TYPE and a PREF came out
// either way round from one call to the next -- and the ETag is a digest of
// these bytes, so the same card stored twice changed it.
func Encode(card vcard.Card) (string, error) {
	version := card.Get(vcard.FieldVersion)
	if version == nil {
		return "", fmt.Errorf("%w: it has no VERSION", ErrInvalid)
	}
	var b strings.Builder
	b.WriteString("BEGIN:VCARD\r\n")
	writeField(&b, vcard.FieldVersion, version)
	for _, k := range slices.Sorted(maps.Keys(card)) {
		if strings.EqualFold(k, vcard.FieldVersion) {
			continue
		}
		for _, f := range card[k] {
			writeField(&b, k, f)
		}
	}
	b.WriteString("END:VCARD\r\n")
	return b.String(), nil
}

// escapeValue is go-vcard's own escaping, which its decoder undoes.
var escapeValue = strings.NewReplacer(`\`, `\\`, "\n", `\n`, ",", `\,`)

func writeField(b *strings.Builder, key string, f *vcard.Field) {
	if f.Group != "" {
		b.WriteString(f.Group + ".")
	}
	b.WriteString(key)
	for _, name := range slices.Sorted(maps.Keys(f.Params)) {
		for _, v := range f.Params[name] {
			b.WriteString(";" + name + "=" + escapeValue.Replace(v))
		}
	}
	b.WriteString(":" + escapeValue.Replace(f.Value) + "\r\n")
}

// ensure makes the default address book if that is the one named, before a
// write opens its transaction, for the reason calendar's ensure gives.
func (s *Service) ensure(ctx context.Context, owner, name string) error {
	if name != DefaultName {
		return nil
	}
	_, err := s.AddressBook(ctx, owner, name)
	return err
}

// ensureDefault makes the default address book, and takes one somebody else
// made a moment earlier as made.
func (s *Service) ensureDefault(ctx context.Context, owner string) (db.AddressBook, error) {
	b, err := s.meta.CreateAddressBook(ctx, db.AddressBook{
		OwnerID: owner, Name: DefaultName, DisplayName: "Contacts", Created: s.now(),
	})
	if errors.Is(err, db.ErrConflict) {
		return s.meta.AddressBookByName(ctx, owner, DefaultName)
	}
	return b, err
}

// validate applies RFC 6352 5.1 and RFC 6350 6.1.3 and 6.7.6: a contact has a
// UID, which is what makes it one resource, and a formatted name. It returns
// the UID.
func validate(card vcard.Card) (string, error) {
	if card == nil {
		return "", ErrInvalid
	}
	uid := card.Value(vcard.FieldUID)
	if uid == "" {
		return "", fmt.Errorf("%w: it has no UID", ErrInvalid)
	}
	if card.Value(vcard.FieldFormattedName) == "" {
		return "", fmt.Errorf("%w: it has no FN", ErrInvalid)
	}
	return uid, nil
}

func validName(name string) error {
	if name == "" || len(name) > maxName || strings.ContainsAny(name, "/\x00") || name == "." || name == ".." {
		return fmt.Errorf("contacts: name %q: %w", name, db.ErrInvalidPath)
	}
	return nil
}
