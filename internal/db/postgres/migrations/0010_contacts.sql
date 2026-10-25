-- Contacts over CardDAV (#423). See the SQLite migration for the why.
CREATE TABLE address_books (
    id           BIGSERIAL   PRIMARY KEY,
    owner_id     TEXT        NOT NULL,
    name         TEXT        NOT NULL,
    display_name TEXT        NOT NULL DEFAULT '',
    description  TEXT        NOT NULL DEFAULT '',
    sync_token   BIGINT      NOT NULL DEFAULT 0,
    pruned_token BIGINT      NOT NULL DEFAULT 0,
    created_at   TIMESTAMPTZ NOT NULL,
    UNIQUE (owner_id, name)
);

CREATE TABLE contacts (
    address_book_id BIGINT      NOT NULL REFERENCES address_books (id) ON DELETE CASCADE,
    name            TEXT        NOT NULL,
    uid             TEXT        NOT NULL,
    data            TEXT        NOT NULL,
    etag            TEXT        NOT NULL,
    sync_token      BIGINT      NOT NULL,
    changed_at      TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (address_book_id, name),
    UNIQUE (address_book_id, uid)
);

CREATE INDEX contacts_token ON contacts (address_book_id, sync_token);

CREATE TABLE contact_tombstones (
    address_book_id BIGINT      NOT NULL REFERENCES address_books (id) ON DELETE CASCADE,
    name            TEXT        NOT NULL,
    sync_token      BIGINT      NOT NULL,
    deleted_at      TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (address_book_id, name)
);
