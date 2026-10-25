package db

import (
	"context"
	"time"
)

// Contacts over CardDAV (#423), the calendars' twin: an address book is a row
// and so is every contact in it, the vCard text in the database for the reasons
// an iCalendar is, and the same counter and tombstones for sync-collection.

// AddressBook is one address book collection.
type AddressBook struct {
	ID      int64
	OwnerID string
	// Name is the address book's segment in the URL; DisplayName is what a
	// client shows.
	Name        string
	DisplayName string
	Description string
	// SyncToken and PrunedToken are a calendar's, for an address book.
	SyncToken   int64
	PrunedToken int64
	Created     time.Time
}

// Normalize is AddressBook the way every driver stores it: its time in UTC and
// to the millisecond.
func (b AddressBook) Normalize() AddressBook {
	b.Created = b.Created.UTC().Truncate(time.Millisecond)
	return b
}

// Contact is one vCard in an address book.
type Contact struct {
	AddressBookID int64
	// Name is the contact's segment in the URL, chosen by the client.
	Name string
	UID  string
	// Data is the vCard as stored, which is what an ETag summarises.
	Data      string
	ETag      string
	SyncToken int64
	Changed   time.Time
}

// Normalize is Contact the way every driver stores it: its time in UTC and to
// the millisecond.
func (c Contact) Normalize() Contact {
	c.Changed = c.Changed.UTC().Truncate(time.Millisecond)
	return c
}

// ContactChange is a CalendarChange, since the two are answered by the same
// sync-collection.
type ContactChange = CalendarChange

// Contacts is the repository for address books, shaped like Calendars and for
// the same reasons: writes to a contact run inside Tx, and the contact calls
// take an id that is only had by reading the address book by its owner.
type Contacts interface {
	// CreateAddressBook stores b and returns it with its id, or ErrConflict
	// when the owner already has one by that name.
	CreateAddressBook(ctx context.Context, b AddressBook) (AddressBook, error)

	// AddressBooks lists an owner's address books by name.
	AddressBooks(ctx context.Context, owner string) ([]AddressBook, error)

	// AddressBookByName returns one of an owner's address books, or
	// ErrNotFound.
	AddressBookByName(ctx context.Context, owner, name string) (AddressBook, error)

	// UpdateAddressBook writes b's display name and description, found by its
	// owner and name, or answers ErrNotFound.
	UpdateAddressBook(ctx context.Context, b AddressBook) error

	// DeleteAddressBook removes one of an owner's address books with every
	// contact and deletion in it, or answers ErrNotFound.
	DeleteAddressBook(ctx context.Context, owner, name string) error

	// Contacts lists everything in an address book by name.
	Contacts(ctx context.Context, addressBookID int64) ([]Contact, error)

	// Contact returns one contact by name, or ErrNotFound.
	Contact(ctx context.Context, addressBookID int64, name string) (Contact, error)

	// PutContact writes c under its name, replacing what was there, and
	// returns it stamped with the address book's next token. Another contact
	// with the same UID is ErrConflict; an address book that is not there is
	// ErrNotFound.
	PutContact(ctx context.Context, c Contact) (Contact, error)

	// DeleteContact removes one contact and leaves a tombstone, or answers
	// ErrNotFound.
	DeleteContact(ctx context.Context, addressBookID int64, name string, at time.Time) error

	// ContactChanges returns every name written or deleted after the token
	// since, in token order.
	ContactChanges(ctx context.Context, addressBookID, since int64) ([]ContactChange, error)

	// PruneContactTombstones forgets the deletions made before the time given,
	// moves each address book's PrunedToken past them, and reports how many it
	// forgot.
	PruneContactTombstones(ctx context.Context, before time.Time) (int64, error)
}
