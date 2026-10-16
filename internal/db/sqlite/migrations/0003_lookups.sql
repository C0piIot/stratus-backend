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
-- once. media.Version moves for them, which re-reads the library.
--
-- album_lookups is keyed the way tag_annotations is: an album is its album
-- artist and its name, and a retag makes a different album. retry_at NULL is
-- "never": an answer that was found is kept until something loses it. The
-- cover is a derived object under one of the album's own tracks, named in
-- cover_blob, so that the sweep collects it with the track and nothing here
-- needs a rule of its own.
--
-- lyrics hangs off the file like track_annotations does, and carries the etag
-- the words were fetched against: a replaced file keeps its id and may carry
-- different tags, which is the same reason media carries one.
ALTER TABLE media ADD COLUMN mb_release_group TEXT NOT NULL DEFAULT '';
ALTER TABLE media ADD COLUMN mb_release       TEXT NOT NULL DEFAULT '';

CREATE TABLE album_lookups (
    owner_id      TEXT    NOT NULL,
    artist        TEXT    NOT NULL,
    album         TEXT    NOT NULL,
    looked_up_at  INTEGER NOT NULL,
    retry_at      INTEGER,
    release_group TEXT    NOT NULL DEFAULT '',
    cover_blob    TEXT    NOT NULL DEFAULT '',
    error         TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (owner_id, artist, album)
);

CREATE TABLE lyrics (
    file_id      INTEGER PRIMARY KEY REFERENCES files (id) ON DELETE CASCADE,
    etag         TEXT    NOT NULL DEFAULT '',
    fetched_at   INTEGER NOT NULL,
    retry_at     INTEGER,
    plain        TEXT    NOT NULL DEFAULT '',
    synced       TEXT    NOT NULL DEFAULT '',
    instrumental INTEGER NOT NULL DEFAULT 0,
    error        TEXT    NOT NULL DEFAULT ''
);
