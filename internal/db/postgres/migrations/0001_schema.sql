CREATE TABLE files (
    id          BIGSERIAL   PRIMARY KEY,
    owner_id    TEXT        NOT NULL,
    path        TEXT        NOT NULL,
    parent_path TEXT        NOT NULL,
    blob_key    TEXT        NOT NULL,
    size        BIGINT      NOT NULL,
    mtime       TIMESTAMPTZ NOT NULL,
    etag        TEXT        NOT NULL,
    mime_type   TEXT        NOT NULL,
    is_dir      BOOLEAN     NOT NULL DEFAULT FALSE
);

CREATE UNIQUE INDEX files_owner_path ON files (owner_id, path);

-- The trailing columns are the paged listing's ORDER BY, in its order and in
-- its direction: a page of a directory is a seek into this index rather than a
-- sort of everything under parent_path, and a collection sorts before a file
-- because that is what a file manager shows.
--
-- It is one index for two orderings, and the whole-directory listing is the one
-- that pays: ordered by path alone, it now sorts what it read instead of
-- reading it in order. That is cheap next to what it was already doing --
-- materialising every child into a slice -- and a second index would be paid
-- on every write instead.
CREATE INDEX files_owner_parent ON files (owner_id, parent_path, is_dir DESC, path);

CREATE TABLE media (
    file_id             BIGINT           PRIMARY KEY REFERENCES files (id) ON DELETE CASCADE,
    kind                TEXT             NOT NULL,
    indexed_at          TIMESTAMPTZ      NOT NULL,
    version             INTEGER          NOT NULL,
    error               TEXT             NOT NULL DEFAULT '',
    taken_at            TIMESTAMPTZ,
    width               INTEGER          NOT NULL DEFAULT 0,
    height              INTEGER          NOT NULL DEFAULT 0,
    orientation         INTEGER          NOT NULL DEFAULT 0,
    latitude            DOUBLE PRECISION,
    longitude           DOUBLE PRECISION,
    camera              TEXT             NOT NULL DEFAULT '',
    duration_ms         BIGINT           NOT NULL DEFAULT 0,
    codec               TEXT             NOT NULL DEFAULT '',
    artist              TEXT             NOT NULL DEFAULT '',
    album               TEXT             NOT NULL DEFAULT '',
    title               TEXT             NOT NULL DEFAULT '',
    track_no            INTEGER          NOT NULL DEFAULT 0,
    disc_no             INTEGER          NOT NULL DEFAULT 0,
    year                INTEGER          NOT NULL DEFAULT 0,
    genre               TEXT             NOT NULL DEFAULT '',
    album_artist        TEXT             NOT NULL DEFAULT '',
    search_song         TEXT             NOT NULL DEFAULT '',
    search_album        TEXT             NOT NULL DEFAULT '',
    search_album_artist TEXT             NOT NULL DEFAULT ''
);

CREATE INDEX media_taken_at ON media (taken_at);

CREATE INDEX media_kind_artist_album ON media (kind, album_artist, album);

CREATE TABLE uploads (
    id         TEXT        NOT NULL PRIMARY KEY,
    owner_id   TEXT        NOT NULL,
    path       TEXT        NOT NULL,
    size       BIGINT      NOT NULL,
    received   BIGINT      NOT NULL,
    blob_key   TEXT        NOT NULL,
    store_id   TEXT        NOT NULL,
    digest     BYTEA       NOT NULL,
    mime_type  TEXT        NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX uploads_expires_at ON uploads (expires_at);
