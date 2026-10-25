-- Contacts over CardDAV (#423), the calendars' twin: a vCard is a row for the
-- same reasons an iCalendar is, and the counters and tombstones are
-- sync-collection's in the same shape. Tables of their own rather than the
-- calendars' with a kind on them: nothing reads both, and the calendars were
-- already serving when these arrived.
--
-- One contact per UID in an address book is CardDAV's no-uid-conflict
-- precondition (RFC 6352 6.3.2.1).
CREATE TABLE address_books (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    owner_id     TEXT    NOT NULL,
    name         TEXT    NOT NULL,
    display_name TEXT    NOT NULL DEFAULT '',
    description  TEXT    NOT NULL DEFAULT '',
    sync_token   INTEGER NOT NULL DEFAULT 0,
    pruned_token INTEGER NOT NULL DEFAULT 0,
    created_at   INTEGER NOT NULL,
    UNIQUE (owner_id, name)
);

CREATE TABLE contacts (
    address_book_id INTEGER NOT NULL REFERENCES address_books (id) ON DELETE CASCADE,
    name            TEXT    NOT NULL,
    uid             TEXT    NOT NULL,
    data            TEXT    NOT NULL,
    etag            TEXT    NOT NULL,
    sync_token      INTEGER NOT NULL,
    changed_at      INTEGER NOT NULL,
    PRIMARY KEY (address_book_id, name),
    UNIQUE (address_book_id, uid)
);

CREATE INDEX contacts_token ON contacts (address_book_id, sync_token);

CREATE TABLE contact_tombstones (
    address_book_id INTEGER NOT NULL REFERENCES address_books (id) ON DELETE CASCADE,
    name            TEXT    NOT NULL,
    sync_token      INTEGER NOT NULL,
    deleted_at      INTEGER NOT NULL,
    PRIMARY KEY (address_book_id, name)
);
