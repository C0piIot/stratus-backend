package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/stratus-private-cloud/stratus-backend/internal/db"
	"github.com/stratus-private-cloud/stratus-backend/internal/db/sqlutil"
)

const (
	addressBookColumns = sqlutil.AddressBookColumns
	contactColumns     = sqlutil.ContactColumns
)

// CreateAddressBook implements db.Contacts.
func (r *repo) CreateAddressBook(ctx context.Context, b db.AddressBook) (db.AddressBook, error) {
	b = b.Normalize()
	const query = `INSERT INTO address_books (owner_id, name, display_name, description, created_at)
		VALUES (?, ?, ?, ?, ?) RETURNING id`

	err := r.q.QueryRowContext(ctx, query, b.OwnerID, b.Name, b.DisplayName, b.Description,
		times.Arg(b.Created)).Scan(&b.ID)
	if err != nil {
		return db.AddressBook{}, fmt.Errorf("create address book %q: %w", b.Name, mapErr(err))
	}
	b.SyncToken, b.PrunedToken = 0, 0
	return b, nil
}

// AddressBooks implements db.Contacts.
func (r *repo) AddressBooks(ctx context.Context, owner string) ([]db.AddressBook, error) {
	const query = `SELECT ` + addressBookColumns + ` FROM address_books WHERE owner_id = ? ORDER BY name`

	out, err := sqlutil.Collect(ctx, r.q, scanAddressBook, query, owner)
	if err != nil {
		return nil, fmt.Errorf("list address books: %w", mapErr(err))
	}
	return out, nil
}

// AddressBookByName implements db.Contacts.
func (r *repo) AddressBookByName(ctx context.Context, owner, name string) (db.AddressBook, error) {
	const query = `SELECT ` + addressBookColumns + ` FROM address_books WHERE owner_id = ? AND name = ?`

	b, err := sqlutil.ScanAddressBook(r.q.QueryRowContext(ctx, query, owner, name), times)
	if err != nil {
		return db.AddressBook{}, fmt.Errorf("get address book %q: %w", name, mapErr(err))
	}
	return b, nil
}

// UpdateAddressBook implements db.Contacts.
func (r *repo) UpdateAddressBook(ctx context.Context, b db.AddressBook) error {
	const query = `UPDATE address_books SET display_name = ?, description = ? WHERE owner_id = ? AND name = ?`

	result, err := r.q.ExecContext(ctx, query, b.DisplayName, b.Description, b.OwnerID, b.Name)
	if err != nil {
		return fmt.Errorf("update address book %q: %w", b.Name, mapErr(err))
	}
	return calendarAffected(result, "update address book", b.Name)
}

// DeleteAddressBook implements db.Contacts. The contacts and the tombstones go
// with it by cascade.
func (r *repo) DeleteAddressBook(ctx context.Context, owner, name string) error {
	result, err := r.q.ExecContext(ctx, `DELETE FROM address_books WHERE owner_id = ? AND name = ?`, owner, name)
	if err != nil {
		return fmt.Errorf("delete address book %q: %w", name, mapErr(err))
	}
	return calendarAffected(result, "delete address book", name)
}

// Contacts implements db.Contacts.
func (r *repo) Contacts(ctx context.Context, addressBookID int64) ([]db.Contact, error) {
	const query = `SELECT ` + contactColumns + ` FROM contacts WHERE address_book_id = ? ORDER BY name`

	out, err := sqlutil.Collect(ctx, r.q, scanContact, query, addressBookID)
	if err != nil {
		return nil, fmt.Errorf("list address book %d: %w", addressBookID, mapErr(err))
	}
	return out, nil
}

// Contact implements db.Contacts.
func (r *repo) Contact(ctx context.Context, addressBookID int64, name string) (db.Contact, error) {
	const query = `SELECT ` + contactColumns + ` FROM contacts WHERE address_book_id = ? AND name = ?`

	c, err := sqlutil.ScanContact(r.q.QueryRowContext(ctx, query, addressBookID, name), times)
	if err != nil {
		return db.Contact{}, fmt.Errorf("get contact %q: %w", name, mapErr(err))
	}
	return c, nil
}

// PutContact implements db.Contacts.
func (r *repo) PutContact(ctx context.Context, c db.Contact) (db.Contact, error) {
	c = c.Normalize()
	token, err := r.nextAddressBookToken(ctx, c.AddressBookID)
	if err != nil {
		return db.Contact{}, fmt.Errorf("put contact %q: %w", c.Name, err)
	}
	c.SyncToken = token

	const upsert = `INSERT INTO contacts (` + contactColumns + `)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (address_book_id, name) DO UPDATE SET uid = excluded.uid, data = excluded.data,
			etag = excluded.etag, sync_token = excluded.sync_token, changed_at = excluded.changed_at`
	if _, err := r.q.ExecContext(ctx, upsert, c.AddressBookID, c.Name, c.UID, c.Data, c.ETag,
		c.SyncToken, times.Arg(c.Changed)); err != nil {
		return db.Contact{}, fmt.Errorf("put contact %q: %w", c.Name, mapErr(err))
	}

	const revive = `DELETE FROM contact_tombstones WHERE address_book_id = ? AND name = ?`
	if _, err := r.q.ExecContext(ctx, revive, c.AddressBookID, c.Name); err != nil {
		return db.Contact{}, fmt.Errorf("put contact %q: %w", c.Name, mapErr(err))
	}
	return c, nil
}

// DeleteContact implements db.Contacts.
func (r *repo) DeleteContact(ctx context.Context, addressBookID int64, name string, at time.Time) error {
	result, err := r.q.ExecContext(ctx, `DELETE FROM contacts WHERE address_book_id = ? AND name = ?`, addressBookID, name)
	if err != nil {
		return fmt.Errorf("delete contact %q: %w", name, mapErr(err))
	}
	if err = calendarAffected(result, "delete contact", name); err != nil {
		return err
	}

	token, err := r.nextAddressBookToken(ctx, addressBookID)
	if err != nil {
		return fmt.Errorf("delete contact %q: %w", name, err)
	}
	const tombstone = `INSERT INTO contact_tombstones (address_book_id, name, sync_token, deleted_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (address_book_id, name) DO UPDATE SET sync_token = excluded.sync_token, deleted_at = excluded.deleted_at`
	if _, err := r.q.ExecContext(ctx, tombstone, addressBookID, name, token, times.Arg(at)); err != nil {
		return fmt.Errorf("delete contact %q: %w", name, mapErr(err))
	}
	return nil
}

// ContactChanges implements db.Contacts.
func (r *repo) ContactChanges(ctx context.Context, addressBookID, since int64) ([]db.ContactChange, error) {
	const query = `SELECT name, sync_token, 0 FROM contacts WHERE address_book_id = ? AND sync_token > ?
		UNION ALL
		SELECT name, sync_token, 1 FROM contact_tombstones WHERE address_book_id = ? AND sync_token > ?
		ORDER BY 2`

	out, err := sqlutil.Collect(ctx, r.q, scanCalendarChange, query, addressBookID, since, addressBookID, since)
	if err != nil {
		return nil, fmt.Errorf("changes to address book %d: %w", addressBookID, mapErr(err))
	}
	return out, nil
}

// PruneContactTombstones implements db.Contacts.
func (r *repo) PruneContactTombstones(ctx context.Context, before time.Time) (int64, error) {
	const mark = `UPDATE address_books SET pruned_token = (
			SELECT MAX(t.sync_token) FROM contact_tombstones t
			WHERE t.address_book_id = address_books.id AND t.deleted_at < ?)
		WHERE id IN (SELECT address_book_id FROM contact_tombstones WHERE deleted_at < ?)`
	if _, err := r.q.ExecContext(ctx, mark, times.Arg(before), times.Arg(before)); err != nil {
		return 0, fmt.Errorf("prune contact tombstones: %w", mapErr(err))
	}

	n, err := sqlutil.Affected(ctx, r.q, `DELETE FROM contact_tombstones WHERE deleted_at < ?`, times.Arg(before))
	if err != nil {
		return 0, fmt.Errorf("prune contact tombstones: %w", mapErr(err))
	}
	return n, nil
}

// nextAddressBookToken moves the address book's counter and returns where it
// landed, or ErrNotFound for one that is not there.
func (r *repo) nextAddressBookToken(ctx context.Context, addressBookID int64) (int64, error) {
	const query = `UPDATE address_books SET sync_token = sync_token + 1 WHERE id = ? RETURNING sync_token`

	var token int64
	if err := r.q.QueryRowContext(ctx, query, addressBookID).Scan(&token); err != nil {
		return 0, mapErr(err)
	}
	return token, nil
}

func scanAddressBook(rows *sql.Rows) (db.AddressBook, error) {
	return sqlutil.ScanAddressBook(rows, times)
}

func scanContact(rows *sql.Rows) (db.Contact, error) { return sqlutil.ScanContact(rows, times) }
