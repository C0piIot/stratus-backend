CREATE TABLE files (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    owner_id    TEXT    NOT NULL,
    path        TEXT    NOT NULL,
    parent_path TEXT    NOT NULL,
    blob_key    TEXT    NOT NULL,
    size        INTEGER NOT NULL,
    mtime       INTEGER NOT NULL,
    etag        TEXT    NOT NULL,
    mime_type   TEXT    NOT NULL,
    is_dir      INTEGER NOT NULL DEFAULT 0
);

CREATE UNIQUE INDEX files_owner_path ON files (owner_id, path);

CREATE INDEX files_owner_parent ON files (owner_id, parent_path, path);

CREATE TABLE media (
    file_id             INTEGER PRIMARY KEY REFERENCES files (id) ON DELETE CASCADE,
    kind                TEXT    NOT NULL,
    indexed_at          INTEGER NOT NULL,
    version             INTEGER NOT NULL,
    error               TEXT    NOT NULL DEFAULT '',
    taken_at            INTEGER,
    width               INTEGER NOT NULL DEFAULT 0,
    height              INTEGER NOT NULL DEFAULT 0,
    orientation         INTEGER NOT NULL DEFAULT 0,
    latitude            REAL,
    longitude           REAL,
    camera              TEXT    NOT NULL DEFAULT '',
    duration_ms         INTEGER NOT NULL DEFAULT 0,
    codec               TEXT    NOT NULL DEFAULT '',
    artist              TEXT    NOT NULL DEFAULT '',
    album               TEXT    NOT NULL DEFAULT '',
    title               TEXT    NOT NULL DEFAULT '',
    track_no            INTEGER NOT NULL DEFAULT 0,
    disc_no             INTEGER NOT NULL DEFAULT 0,
    year                INTEGER NOT NULL DEFAULT 0,
    genre               TEXT    NOT NULL DEFAULT '',
    album_artist        TEXT    NOT NULL DEFAULT '',
    search_song         TEXT    NOT NULL DEFAULT '',
    search_album        TEXT    NOT NULL DEFAULT '',
    search_album_artist TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX media_taken_at ON media (taken_at);

CREATE INDEX media_kind_artist_album ON media (kind, album_artist, album);
