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
