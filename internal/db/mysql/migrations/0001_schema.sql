-- The schema, as one migration.
--
-- It was seven until #269. Keeping the steps was buying nothing: there is no
-- database anywhere that needs them, and a reader who wants to know what a
-- column is for had to reconstruct it from a chain of ALTERs. What a migration
-- file is really worth is the paragraph beside each decision, and those are all
-- here -- the issue numbers with them, so the argument is still findable.
--
-- The way back is not supported and does not pretend to be: a database written
-- by the seven is at version 15, and db.Migrate refuses a schema it does not
-- know rather than running against it. The answer is a new database.
--
-- From here the next one is 0002 and nothing is ever edited in place again.

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

    -- What a search box matches against (#259): the file's own name, which no
    -- row stores -- it is what is left of the path after the parent, and
    -- nothing at all is cut from a row at the root, where the parent is empty.
    -- The name and not the path, so that a word in a folder finds the folder
    -- and not the thousand photographs under it. Seven nested REPLACEs because
    -- this dialect has no TRANSLATE, which is ugly and is written once.
    --
    -- **The one column in this schema that is not binary-collated.** A
    -- FULLTEXT match uses the column's collation, and utf8mb4_0900_bin would
    -- make searching case-sensitive, which db.Finder forbids. The paragraph at
    -- the top is about a unique key over a path; this column is in no unique
    -- key and is never read back.
    search_name TEXT         COLLATE utf8mb4_0900_ai_ci
    GENERATED ALWAYS AS (
        REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(
            SUBSTRING(path, CHAR_LENGTH(parent_path) + IF(parent_path = '', 1, 2)),
        '.', ' '), '_', ' '), '-', ' '), '(', ' '), ')', ' '), '[', ' '), ']', ' ')
    ) STORED,

    UNIQUE KEY files_owner_path (owner_id, path_hash),
    -- A prefix is enough here, unlike above: this index narrows a lookup and
    -- the engine still compares the whole value, so a shared prefix costs a
    -- comparison rather than correctness.
    --
    -- What it cannot do is order. The other two drivers carry is_dir and path
    -- at the end of this index so that a page of a listing is a seek; nothing
    -- after a prefix column is usable for an ORDER BY, so adding them here
    -- would be dead weight and the driver says outright that it sorts instead.
    KEY files_owner_parent (owner_id, parent_path(500)),
    FULLTEXT KEY files_search_name (search_name)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_0900_bin;

CREATE TABLE media (
    file_id             BIGINT      NOT NULL PRIMARY KEY,
    kind                VARCHAR(32) NOT NULL,
    indexed_at          BIGINT      NOT NULL,
    version             INT         NOT NULL,
    -- The file's validator when this was extracted. A replaced file keeps its
    -- row and its id, so this is what tells the queue that the metadata
    -- describes bytes that are no longer there.
    etag                TEXT        NOT NULL,
    error               TEXT        NOT NULL,
    -- When the queue should offer this file again, and NULL when never. A
    -- failure to reach the bytes is not a verdict on them (#157): the row
    -- records what happened so that /status can say it, and a time to find out
    -- again. A file nothing can parse gets no such time.
    retry_at            BIGINT       NULL,
    taken_at            BIGINT      NULL,
    -- The moment the gallery lists a photograph by (#211): when the camera
    -- says it was taken, and when the file arrived for one that says nothing,
    -- like a screenshot. A column because the two halves live in different
    -- tables, and an ORDER BY over an expression spanning both is one no index
    -- can serve -- every page would sort every image the owner has. PutMedia
    -- writes it.
    sort_at             BIGINT      NULL,
    width               INT         NOT NULL DEFAULT 0,
    height              INT         NOT NULL DEFAULT 0,
    orientation         INT         NOT NULL DEFAULT 0,
    latitude            DOUBLE      NULL,
    longitude           DOUBLE      NULL,
    camera              TEXT        NOT NULL,
    duration_ms         BIGINT      NOT NULL DEFAULT 0,
    codec               TEXT        NOT NULL,
    -- What a transcode decision is made from (#197, #207): whether a client
    -- can take the file as it is, and what a transcode must not exceed. Zero is
    -- unknown. codec_profile, bit_depth, level and frame_rate describe the
    -- row's own stream -- the picture, for a video -- and bitrate is the
    -- stream's for audio and the whole file's for a video. sample_rate,
    -- channels and audio_codec are the sound, a video's included: its audio
    -- track is what most often needs a transcode while the picture does not.
    bitrate             INT         NOT NULL DEFAULT 0,
    sample_rate         INT         NOT NULL DEFAULT 0,
    channels            INT         NOT NULL DEFAULT 0,
    bit_depth           INT         NOT NULL DEFAULT 0,
    codec_profile       TEXT        NOT NULL,
    level               INT         NOT NULL DEFAULT 0,
    frame_rate          INT         NOT NULL DEFAULT 0,
    audio_codec         TEXT        NOT NULL,
    -- A picture's colour as ffprobe names it, which is how HDR is told from SDR
    -- (#50): PQ is smpte2084 and HLG arib-std-b67. dovi_profile is a Dolby Vision
    -- profile, zero for none: profile 5 has no picture an SDR player can be given.
    color_primaries     TEXT        NOT NULL,
    color_transfer      TEXT        NOT NULL,
    color_space         TEXT        NOT NULL,
    dovi_profile        INT         NOT NULL DEFAULT 0,
    artist              TEXT        NOT NULL,
    album               TEXT        NOT NULL,
    title               TEXT        NOT NULL,
    track_no            INT         NOT NULL DEFAULT 0,
    disc_no             INT         NOT NULL DEFAULT 0,
    year                INT         NOT NULL DEFAULT 0,
    genre               TEXT        NOT NULL,
    album_artist        TEXT        NOT NULL,

    -- The five columns a search matches, folded in Go and not by the engine
    -- (#85): what is compared must not depend on anyone's lower(). They keep
    -- this table's binary collation for that reason, unlike files.search_name
    -- above -- a folded column is already case-insensitive by construction.
    --
    -- One tag each. The title and the artist credited on the track are two
    -- columns because a search answers an artist as a row of its own, and a
    -- bucket promising the title cannot read a column that also holds a name
    -- (#262). search_photo is what a photograph is found by, which is not its
    -- name: the camera, and the year it says the picture was taken.
    search_title        TEXT        NOT NULL,
    search_artist       TEXT        NOT NULL,
    search_album        TEXT        NOT NULL,
    search_album_artist TEXT        NOT NULL,
    search_photo        TEXT        NOT NULL,

    KEY media_taken_at (taken_at),
    KEY media_kind_artist_album (kind, album_artist(191), album(191)),
    -- The gallery's ORDER BY, and a month is a range of it.
    KEY media_kind_sort (kind, sort_at, file_id),

    -- **One FULLTEXT key per column, which is this engine's alone** (#262). A
    -- MATCH has to name exactly the columns some key was built on and there is
    -- no column-restricted form of it, so three buckets asking about one
    -- column each need three indexes. SQLite restricts a match to a column of
    -- its FTS5 table and PostgreSQL narrows with a combined vector and decides
    -- with the column; neither of them carries these.
    --
    -- No key on search_artist: nothing matches it through an index, because
    -- Music.Search scans with LIKE here -- the ngram parser is not exact
    -- substring and depends on a server variable, which is #262's measurement
    -- and not an oversight.
    FULLTEXT KEY media_search_title (search_title),
    FULLTEXT KEY media_search_album (search_album),
    FULLTEXT KEY media_search_album_artist (search_album_artist),
    FULLTEXT KEY media_search_photo (search_photo),

    CONSTRAINT media_file FOREIGN KEY (file_id) REFERENCES files (id) ON DELETE CASCADE
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_0900_bin;

-- An upload id is ours rather than a path, so it fits in an index whole and
-- needs none of the hashing the files table does.
CREATE TABLE uploads (
    id         VARCHAR(64)  NOT NULL PRIMARY KEY,
    owner_id   VARCHAR(255) NOT NULL,
    path       TEXT         NOT NULL,
    size       BIGINT       NOT NULL,
    received   BIGINT       NOT NULL,
    blob_key   TEXT         NOT NULL,
    store_id   TEXT         NOT NULL,
    digest     VARBINARY(256) NOT NULL,
    mime_type  TEXT         NOT NULL,
    expires_at BIGINT       NOT NULL,

    KEY uploads_expires_at (expires_at)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_0900_bin;

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
    file_id    BIGINT       NOT NULL,
    owner_id   VARCHAR(255) NOT NULL,
    starred_at BIGINT       NULL,
    rating     INT          NOT NULL DEFAULT 0,
    -- Plays (#195): a count rather than a log of every play. Nothing asks when
    -- each one happened, only how many and the latest, and a log would grow
    -- for as long as somebody listens.
    play_count BIGINT       NOT NULL DEFAULT 0,
    played_at  BIGINT       NULL,

    PRIMARY KEY (file_id, owner_id),
    CONSTRAINT track_annotations_file FOREIGN KEY (file_id) REFERENCES files (id) ON DELETE CASCADE
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_0900_bin;

-- The tags are TEXT and cannot be a key, for the reason files.path cannot: the
-- key is key_hash, sha256 of the kind and both tags, computed by the adapter.
CREATE TABLE tag_annotations (
    owner_id   VARCHAR(255) NOT NULL,
    key_hash   BINARY(32)   NOT NULL,
    kind       VARCHAR(16)  NOT NULL,
    artist     TEXT         NOT NULL,
    album      TEXT         NOT NULL,
    starred_at BIGINT       NULL,
    rating     INT          NOT NULL DEFAULT 0,

    PRIMARY KEY (owner_id, key_hash)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_0900_bin;

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
