CREATE TABLE media (
    file_id     INTEGER PRIMARY KEY REFERENCES files (id) ON DELETE CASCADE,
    kind        TEXT    NOT NULL,
    indexed_at  INTEGER NOT NULL,
    version     INTEGER NOT NULL,
    error       TEXT    NOT NULL DEFAULT '',
    taken_at    INTEGER,
    width       INTEGER NOT NULL DEFAULT 0,
    height      INTEGER NOT NULL DEFAULT 0,
    orientation INTEGER NOT NULL DEFAULT 0,
    latitude    REAL,
    longitude   REAL,
    camera      TEXT    NOT NULL DEFAULT '',
    duration_ms INTEGER NOT NULL DEFAULT 0,
    codec       TEXT    NOT NULL DEFAULT '',
    artist      TEXT    NOT NULL DEFAULT '',
    album       TEXT    NOT NULL DEFAULT '',
    title       TEXT    NOT NULL DEFAULT '',
    track_no    INTEGER NOT NULL DEFAULT 0,
    disc_no     INTEGER NOT NULL DEFAULT 0,
    year        INTEGER NOT NULL DEFAULT 0,
    genre       TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX media_taken_at ON media (taken_at);
