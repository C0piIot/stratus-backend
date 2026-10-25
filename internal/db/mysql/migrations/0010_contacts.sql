-- Contacts over CardDAV (#423). See the SQLite migration for the why. The
-- vCard is a MEDIUMTEXT because a client sends the photograph inside it, and
-- TEXT stops at 64 KB; the UID is unique through its hash, as a calendar
-- object's is, since a TEXT cannot be a key.
CREATE TABLE address_books (
    id           BIGINT       NOT NULL AUTO_INCREMENT PRIMARY KEY,
    owner_id     VARCHAR(255) NOT NULL,
    name         VARCHAR(255) NOT NULL,
    display_name TEXT         NOT NULL,
    description  TEXT         NOT NULL,
    sync_token   BIGINT       NOT NULL DEFAULT 0,
    pruned_token BIGINT       NOT NULL DEFAULT 0,
    created_at   BIGINT       NOT NULL,

    UNIQUE KEY address_books_owner_name (owner_id, name)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_0900_bin;

CREATE TABLE contacts (
    address_book_id BIGINT       NOT NULL,
    name            VARCHAR(255) NOT NULL,
    uid             TEXT         NOT NULL,
    uid_hash        BINARY(32)   NOT NULL,
    data            MEDIUMTEXT   NOT NULL,
    etag            TEXT         NOT NULL,
    sync_token      BIGINT       NOT NULL,
    changed_at      BIGINT       NOT NULL,

    PRIMARY KEY (address_book_id, name),
    UNIQUE KEY contacts_uid (address_book_id, uid_hash),
    KEY contacts_token (address_book_id, sync_token),
    CONSTRAINT contacts_address_book FOREIGN KEY (address_book_id) REFERENCES address_books (id) ON DELETE CASCADE
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_0900_bin;

CREATE TABLE contact_tombstones (
    address_book_id BIGINT       NOT NULL,
    name            VARCHAR(255) NOT NULL,
    sync_token      BIGINT       NOT NULL,
    deleted_at      BIGINT       NOT NULL,

    PRIMARY KEY (address_book_id, name),
    CONSTRAINT contact_tombstones_address_book FOREIGN KEY (address_book_id) REFERENCES address_books (id) ON DELETE CASCADE
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_0900_bin;
