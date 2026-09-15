-- MySQL cannot index a TEXT column without a prefix length, and InnoDB caps an
-- index key at 3072 bytes, which is 768 characters of utf8mb4. A path is up to
-- MaxPathLen bytes, so the unique constraint the other two drivers put straight
-- on (owner_id, path) cannot exist here.
--
-- path_hash carries it instead: sha256 of the path, computed by the adapter. A
-- prefix index would not do, because two paths sharing a prefix would collide
-- as duplicates and one would silently replace the other.
--
-- Every text column is utf8mb4_0900_bin. The default collation is accent- and
-- case-insensitive, which would make Photo.jpg, photo.jpg and phóto.jpg one row
-- under that unique key. Binary is what the other two drivers do and what a
-- filesystem does; it is a correctness requirement, not a preference.

CREATE TABLE files (
    id          BIGINT       NOT NULL AUTO_INCREMENT PRIMARY KEY,
    -- VARCHAR rather than TEXT so the whole value fits in an index: an owner is
    -- a username, and the unique key below has to hold it entire.
    owner_id    VARCHAR(255) NOT NULL,
    path        TEXT         NOT NULL,
    path_hash   BINARY(32)   NOT NULL,
    parent_path TEXT         NOT NULL,
    blob_key    TEXT         NOT NULL,
    size        BIGINT       NOT NULL,
    -- Milliseconds since the epoch, as in the SQLite driver. A DATETIME would
    -- drag in session time zones and a TIMESTAMP would drag in the 2038 cutoff,
    -- for a column the port hands over as a time.Time either way.
    mtime       BIGINT       NOT NULL,
    etag        TEXT         NOT NULL,
    mime_type   TEXT         NOT NULL,
    is_dir      TINYINT(1)   NOT NULL DEFAULT 0,

    UNIQUE KEY files_owner_path (owner_id, path_hash),
    -- A prefix is enough here, unlike above: this index narrows a lookup and
    -- the engine still compares the whole value, so a shared prefix costs a
    -- comparison rather than correctness.
    KEY files_owner_parent (owner_id, parent_path(500))
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_0900_bin;

CREATE TABLE media (
    file_id             BIGINT      NOT NULL PRIMARY KEY,
    kind                VARCHAR(32) NOT NULL,
    indexed_at          BIGINT      NOT NULL,
    version             INT         NOT NULL,
    error               TEXT        NOT NULL,
    taken_at            BIGINT      NULL,
    width               INT         NOT NULL DEFAULT 0,
    height              INT         NOT NULL DEFAULT 0,
    orientation         INT         NOT NULL DEFAULT 0,
    latitude            DOUBLE      NULL,
    longitude           DOUBLE      NULL,
    camera              TEXT        NOT NULL,
    duration_ms         BIGINT      NOT NULL DEFAULT 0,
    codec               TEXT        NOT NULL,
    artist              TEXT        NOT NULL,
    album               TEXT        NOT NULL,
    title               TEXT        NOT NULL,
    track_no            INT         NOT NULL DEFAULT 0,
    disc_no             INT         NOT NULL DEFAULT 0,
    year                INT         NOT NULL DEFAULT 0,
    genre               TEXT        NOT NULL,
    album_artist        TEXT        NOT NULL,
    search_song         TEXT        NOT NULL,
    search_album        TEXT        NOT NULL,
    search_album_artist TEXT        NOT NULL,

    KEY media_taken_at (taken_at),
    KEY media_kind_artist_album (kind, album_artist(191), album(191)),
    CONSTRAINT media_file FOREIGN KEY (file_id) REFERENCES files (id) ON DELETE CASCADE
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_0900_bin;
