-- Calendars over CalDAV (#75). The iCalendar text is a row and not a blob: it
-- is a few kilobytes, and a time-range query has to read every object of a
-- calendar whole, because a recurring event's first date says nothing about
-- whether it falls in March.
--
-- The counters are sync-collection's (RFC 6578), and they are here from the
-- first migration because a column is free now and a migration later is not.
-- sync_token on a calendar moves on every write to it and stamps the object
-- or the tombstone the write left; "what changed since n" is everything above
-- n in both tables. A tombstone is pruned after a while, and pruned_token is
-- how far: a client asking about anything older is told to list again.
--
-- One object per UID in a calendar is CalDAV's no-uid-conflict precondition
-- (RFC 4791 5.3.2.1), and it is a unique key so that it holds under two
-- writers as well as one.
--
-- Two things only this engine needs. A TEXT cannot be a key, so the names are
-- VARCHAR(255) -- a client names an object after its UID, and internal/calendar
-- refuses a longer name before it gets here -- while the UID itself has no
-- such bound and is unique through uid_hash, sha256 of it, computed by the
-- adapter. And a TEXT holds 64 KB, which an event with an attachment inline
-- can pass, so the iCalendar is a MEDIUMTEXT.
CREATE TABLE calendars (
    id           BIGINT       NOT NULL AUTO_INCREMENT PRIMARY KEY,
    owner_id     VARCHAR(255) NOT NULL,
    name         VARCHAR(255) NOT NULL,
    display_name TEXT         NOT NULL,
    description  TEXT         NOT NULL,
    sync_token   BIGINT       NOT NULL DEFAULT 0,
    pruned_token BIGINT       NOT NULL DEFAULT 0,
    created_at   BIGINT       NOT NULL,

    UNIQUE KEY calendars_owner_name (owner_id, name)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_0900_bin;

CREATE TABLE calendar_objects (
    calendar_id BIGINT       NOT NULL,
    name        VARCHAR(255) NOT NULL,
    uid         TEXT         NOT NULL,
    uid_hash    BINARY(32)   NOT NULL,
    component   VARCHAR(32)  NOT NULL,
    data        MEDIUMTEXT   NOT NULL,
    etag        TEXT         NOT NULL,
    sync_token  BIGINT       NOT NULL,
    changed_at  BIGINT       NOT NULL,

    PRIMARY KEY (calendar_id, name),
    UNIQUE KEY calendar_objects_uid (calendar_id, uid_hash),
    KEY calendar_objects_token (calendar_id, sync_token),
    CONSTRAINT calendar_objects_calendar FOREIGN KEY (calendar_id) REFERENCES calendars (id) ON DELETE CASCADE
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_0900_bin;

CREATE TABLE calendar_tombstones (
    calendar_id BIGINT       NOT NULL,
    name        VARCHAR(255) NOT NULL,
    sync_token  BIGINT       NOT NULL,
    deleted_at  BIGINT       NOT NULL,

    PRIMARY KEY (calendar_id, name),
    CONSTRAINT calendar_tombstones_calendar FOREIGN KEY (calendar_id) REFERENCES calendars (id) ON DELETE CASCADE
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_0900_bin;
