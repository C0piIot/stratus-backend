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
CREATE TABLE calendars (
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

CREATE TABLE calendar_objects (
    calendar_id INTEGER NOT NULL REFERENCES calendars (id) ON DELETE CASCADE,
    name        TEXT    NOT NULL,
    uid         TEXT    NOT NULL,
    component   TEXT    NOT NULL,
    data        TEXT    NOT NULL,
    etag        TEXT    NOT NULL,
    sync_token  INTEGER NOT NULL,
    changed_at  INTEGER NOT NULL,
    PRIMARY KEY (calendar_id, name),
    UNIQUE (calendar_id, uid)
);

CREATE INDEX calendar_objects_token ON calendar_objects (calendar_id, sync_token);

CREATE TABLE calendar_tombstones (
    calendar_id INTEGER NOT NULL REFERENCES calendars (id) ON DELETE CASCADE,
    name        TEXT    NOT NULL,
    sync_token  INTEGER NOT NULL,
    deleted_at  INTEGER NOT NULL,
    PRIMARY KEY (calendar_id, name)
);
