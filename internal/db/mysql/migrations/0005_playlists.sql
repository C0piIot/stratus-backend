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
    id         BIGINT       NOT NULL AUTO_INCREMENT PRIMARY KEY,
    owner_id   VARCHAR(255) NOT NULL,
    name       TEXT         NOT NULL,
    comment    TEXT         NOT NULL,
    public     TINYINT(1)   NOT NULL DEFAULT 0,
    created_at BIGINT       NOT NULL,
    changed_at BIGINT       NOT NULL,

    KEY playlists_owner (owner_id)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_0900_bin;

-- The foreign key to files is what indexes file_id here: InnoDB makes one for
-- every foreign key that has none.
CREATE TABLE playlist_entries (
    playlist_id BIGINT NOT NULL,
    position    INT    NOT NULL,
    file_id     BIGINT NOT NULL,

    PRIMARY KEY (playlist_id, position),
    CONSTRAINT playlist_entries_playlist FOREIGN KEY (playlist_id) REFERENCES playlists (id) ON DELETE CASCADE,
    CONSTRAINT playlist_entries_file FOREIGN KEY (file_id) REFERENCES files (id) ON DELETE CASCADE
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_0900_bin;
