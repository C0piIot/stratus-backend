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
    file_id             INTEGER PRIMARY KEY REFERENCES files (id) ON DELETE CASCADE,
    kind                TEXT    NOT NULL,
    indexed_at          INTEGER NOT NULL,
    version             INTEGER NOT NULL,
    -- The file's validator when this was extracted. A replaced file keeps its
    -- row and its id, so this is what tells the queue that the metadata
    -- describes bytes that are no longer there.
    etag                TEXT    NOT NULL DEFAULT '',
    error               TEXT    NOT NULL DEFAULT '',
    -- When the queue should offer this file again, and NULL when never. A
    -- failure to reach the bytes is not a verdict on them (#157): the row
    -- records what happened so that /status can say it, and a time to find out
    -- again. A file nothing can parse gets no such time.
    retry_at            INTEGER,
    taken_at            INTEGER,
    width               INTEGER NOT NULL DEFAULT 0,
    height              INTEGER NOT NULL DEFAULT 0,
    orientation         INTEGER NOT NULL DEFAULT 0,
    latitude            REAL,
    longitude           REAL,
    camera              TEXT    NOT NULL DEFAULT '',
    duration_ms         INTEGER NOT NULL DEFAULT 0,
    codec               TEXT    NOT NULL DEFAULT '',
    -- The audio stream a transcode decision is made from (#197): whether a
    -- client can take the file as it is, and what a transcode must not exceed.
    -- Zero is unknown, and bit_depth is zero for a lossy codec, which has none.
    -- Filled for audio only; a video's are #207.
    bitrate             INTEGER NOT NULL DEFAULT 0,
    sample_rate         INTEGER NOT NULL DEFAULT 0,
    channels            INTEGER NOT NULL DEFAULT 0,
    bit_depth           INTEGER NOT NULL DEFAULT 0,
    codec_profile       TEXT    NOT NULL DEFAULT '',
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

CREATE TABLE uploads (
    id         TEXT    NOT NULL PRIMARY KEY,
    owner_id   TEXT    NOT NULL,
    path       TEXT    NOT NULL,
    size       INTEGER NOT NULL,
    received   INTEGER NOT NULL,
    blob_key   TEXT    NOT NULL,
    store_id   TEXT    NOT NULL,
    digest     BLOB    NOT NULL,
    mime_type  TEXT    NOT NULL,
    expires_at INTEGER NOT NULL
);

CREATE INDEX uploads_expires_at ON uploads (expires_at);

-- What a user has said about their library: stars and ratings (#194).
--
-- Two tables because a track is a row and an album or an artist is not. A
-- track's annotation goes with its file, which is what the foreign key is for;
-- the id already survives an overwrite and a rename. An album's is keyed by the
-- tags that make it one, so it outlives its tracks and a retag leaves it behind.
--
-- The owner is on both even while there is only one, because favourites are the
-- first thing sharing will want: somebody else's file can be my favourite.
-- file_id leads the key so the cascade on a delete is a seek.
--
-- A row with neither a star nor a rating is deleted rather than kept.

CREATE TABLE track_annotations (
    file_id    INTEGER NOT NULL REFERENCES files (id) ON DELETE CASCADE,
    owner_id   TEXT    NOT NULL,
    starred_at INTEGER,
    rating     INTEGER NOT NULL DEFAULT 0,
    -- Plays (#195): a count rather than a log of every play. Nothing asks when
    -- each one happened, only how many and the latest, and a log would grow
    -- for as long as somebody listens.
    play_count INTEGER NOT NULL DEFAULT 0,
    played_at  INTEGER,
    PRIMARY KEY (file_id, owner_id)
);

-- kind is 'album' or 'artist'; an artist's album is ''.
CREATE TABLE tag_annotations (
    owner_id   TEXT    NOT NULL,
    kind       TEXT    NOT NULL,
    artist     TEXT    NOT NULL,
    album      TEXT    NOT NULL,
    starred_at INTEGER,
    rating     INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (owner_id, kind, artist, album)
);

-- Playlists (#196). A row rather than something derived from tags, because
-- somebody made it.
--
-- An entry's position only orders: a deleted file's entries go with it by
-- cascade and leave a gap, which nothing minds, because an index a client sends
-- is into the list as it is read and never into these numbers.
--
-- The owner is on the playlist and not on each entry: an entry is part of the
-- playlist, and sharing one will be a question about the playlist.

CREATE TABLE playlists (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    owner_id   TEXT    NOT NULL,
    name       TEXT    NOT NULL,
    comment    TEXT    NOT NULL DEFAULT '',
    public     INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    changed_at INTEGER NOT NULL
);

CREATE INDEX playlists_owner ON playlists (owner_id, name);

CREATE TABLE playlist_entries (
    playlist_id INTEGER NOT NULL REFERENCES playlists (id) ON DELETE CASCADE,
    position    INTEGER NOT NULL,
    file_id     INTEGER NOT NULL REFERENCES files (id) ON DELETE CASCADE,
    PRIMARY KEY (playlist_id, position)
);

-- What the cascade from files seeks on.
CREATE INDEX playlist_entries_file ON playlist_entries (file_id);
