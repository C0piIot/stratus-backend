-- What outside sources said about the music library (#217): an album's
-- identity on MusicBrainz and the front cover fetched for it, and a track's
-- words from LRCLIB. Rows rather than tags, because the user's files are never
-- rewritten; and rows rather than derived objects alone, because the pass has
-- to ask "which albums have not been looked at" in one query, which a blob
-- store cannot answer (the argument thumb_state already made in 0002).
--
-- The two media columns are what a tagger such as Picard writes into a file.
-- An id in the tags is the album's own answer, so the pass makes no search for
-- it -- a search can be wrong, and a tagged library has already been right
-- once. media.Version moves for them, which re-reads the library. Both in one
-- ALTER, for the reason 0002 gives: DDL is not transactional here.
--
-- album_lookups is keyed the way tag_annotations is: the tags are TEXT and
-- cannot be a key, so the key is key_hash, sha256 of both tags, computed by
-- the adapter. retry_at NULL is "never": an answer that was found is kept
-- until something loses it. The cover is a derived object under one of the
-- album's own tracks, named in cover_blob, so that the sweep collects it with
-- the track and nothing here needs a rule of its own.
--
-- lyrics hangs off the file like track_annotations does, and carries the etag
-- the words were fetched against: a replaced file keeps its id and may carry
-- different tags, which is the same reason media carries one.
ALTER TABLE media
    ADD COLUMN mb_release_group VARCHAR(36) NOT NULL DEFAULT '',
    ADD COLUMN mb_release       VARCHAR(36) NOT NULL DEFAULT '';

CREATE TABLE album_lookups (
    owner_id      VARCHAR(255)  NOT NULL,
    key_hash      BINARY(32)    NOT NULL,
    artist        TEXT          NOT NULL,
    album         TEXT          NOT NULL,
    looked_up_at  BIGINT        NOT NULL,
    retry_at      BIGINT        NULL,
    release_group VARCHAR(36)   NOT NULL DEFAULT '',
    cover_blob    VARCHAR(1024) NOT NULL DEFAULT '',
    error         TEXT          NOT NULL,

    PRIMARY KEY (owner_id, key_hash)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_0900_bin;

CREATE TABLE lyrics (
    file_id      BIGINT     NOT NULL PRIMARY KEY,
    etag         TEXT       NOT NULL,
    fetched_at   BIGINT     NOT NULL,
    retry_at     BIGINT     NULL,
    plain        TEXT       NOT NULL,
    synced       TEXT       NOT NULL,
    instrumental TINYINT(1) NOT NULL DEFAULT 0,
    error        TEXT       NOT NULL,

    CONSTRAINT lyrics_file FOREIGN KEY (file_id) REFERENCES files (id) ON DELETE CASCADE
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_0900_bin;
