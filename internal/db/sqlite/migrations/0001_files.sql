CREATE TABLE files (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    owner_id    TEXT    NOT NULL,
    path        TEXT    NOT NULL,
    parent_path TEXT    NOT NULL,
    blob_key    TEXT    NOT NULL,
    size        INTEGER NOT NULL,
    mtime       INTEGER NOT NULL,
    etag        TEXT    NOT NULL,
    mime_type   TEXT    NOT NULL
);

CREATE UNIQUE INDEX files_owner_path ON files (owner_id, path);

CREATE INDEX files_owner_parent ON files (owner_id, parent_path, path);
