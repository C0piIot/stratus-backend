-- The schema, as one migration.
--
-- It was eight until #269. Keeping the steps was buying nothing: there is no
-- database anywhere that needs them, and a reader who wants to know what a
-- column is for had to reconstruct it from a chain of ALTERs. What a migration
-- file is really worth is the paragraph beside each decision, and those are all
-- here -- the issue numbers with them, so the argument is still findable.
--
-- The way back is not supported and does not pretend to be: a database written
-- by the eight is at version 15, and db.Migrate refuses a schema it does not
-- know rather than running against it. The answer is a new database.
--
-- From here the next one is 0002 and nothing is ever edited in place again.

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
    is_dir      BOOLEAN     NOT NULL DEFAULT FALSE,

    -- What a search box matches against (#259): the file's own name, which no
    -- row stores -- it is what is left of the path after the parent, and
    -- nothing at all is cut from a row at the root, where the parent is empty.
    -- The name and not the path, so that a word in a folder finds the folder
    -- and not the thousand photographs under it.
    --
    -- The separators a filename is made of are turned into spaces before the
    -- tokeniser sees them, and that is not optional here: this parser reads
    -- photo.jpg as one token called a file, so searching photo would find
    -- nothing at all. With them flattened it is photo and jpg, which is what
    -- db.Finder promises.
    --
    -- 'simple' and not a language configuration: a filename is not prose, and
    -- English stemming would turn notes into note and make a search for the
    -- name somebody typed miss it.
    search_name tsvector    GENERATED ALWAYS AS (
        to_tsvector('simple', translate(
            substring(path FROM char_length(parent_path) + (CASE WHEN parent_path = '' THEN 1 ELSE 2 END)),
            '._-()[]', '       '))
    ) STORED
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

-- The two orderings a listing can be asked for that path alone cannot serve
-- (#251): by size and by when a file last changed.
--
-- is_dir is in the key and not in the ORDER BY. A page asks for one group at a
-- time -- directories, then files -- so is_dir is an equality, and with it
-- pinned the remaining columns are the sort exactly. That is what makes one
-- index serve both directions: scanned backwards it yields size DESC, path
-- DESC, which is a descending page, while the group stays where it was.
--
-- The cursor has to be a row comparison -- (size, path) > ($1, $2) -- for
-- either of these to be a seek. MySQL has neither index, because its
-- parent_path is indexed by a prefix and nothing after a prefix column can
-- satisfy an ORDER BY.
CREATE INDEX files_owner_parent_size ON files (owner_id, parent_path, is_dir, size, path);

CREATE INDEX files_owner_parent_mtime ON files (owner_id, parent_path, is_dir, mtime, path);

CREATE INDEX files_search_name ON files USING GIN (search_name);

CREATE TABLE media (
    file_id             BIGINT           PRIMARY KEY REFERENCES files (id) ON DELETE CASCADE,
    kind                TEXT             NOT NULL,
    indexed_at          TIMESTAMPTZ      NOT NULL,
    version             INTEGER          NOT NULL,
    -- The file's validator when this was extracted. A replaced file keeps its
    -- row and its id, so this is what tells the queue that the metadata
    -- describes bytes that are no longer there.
    etag                TEXT             NOT NULL DEFAULT '',
    error               TEXT             NOT NULL DEFAULT '',
    -- When the queue should offer this file again, and NULL when never. A
    -- failure to reach the bytes is not a verdict on them (#157): the row
    -- records what happened so that /status can say it, and a time to find out
    -- again. A file nothing can parse gets no such time.
    retry_at            TIMESTAMPTZ,
    taken_at            TIMESTAMPTZ,
    -- The moment the gallery lists a photograph by (#211): when the camera
    -- says it was taken, and when the file arrived for one that says nothing,
    -- like a screenshot. A column because the two halves live in different
    -- tables, and an ORDER BY over an expression spanning both is one no index
    -- can serve -- every page would sort every image the owner has. PutMedia
    -- writes it.
    sort_at             TIMESTAMPTZ,
    width               INTEGER          NOT NULL DEFAULT 0,
    height              INTEGER          NOT NULL DEFAULT 0,
    orientation         INTEGER          NOT NULL DEFAULT 0,
    latitude            DOUBLE PRECISION,
    longitude           DOUBLE PRECISION,
    camera              TEXT             NOT NULL DEFAULT '',
    duration_ms         BIGINT           NOT NULL DEFAULT 0,
    codec               TEXT             NOT NULL DEFAULT '',
    -- What a transcode decision is made from (#197, #207): whether a client
    -- can take the file as it is, and what a transcode must not exceed. Zero is
    -- unknown. codec_profile, bit_depth, level and frame_rate describe the
    -- row's own stream -- the picture, for a video -- and bitrate is the
    -- stream's for audio and the whole file's for a video. sample_rate,
    -- channels and audio_codec are the sound, a video's included: its audio
    -- track is what most often needs a transcode while the picture does not.
    bitrate             INTEGER          NOT NULL DEFAULT 0,
    sample_rate         INTEGER          NOT NULL DEFAULT 0,
    channels            INTEGER          NOT NULL DEFAULT 0,
    bit_depth           INTEGER          NOT NULL DEFAULT 0,
    codec_profile       TEXT             NOT NULL DEFAULT '',
    level               INTEGER          NOT NULL DEFAULT 0,
    frame_rate          INTEGER          NOT NULL DEFAULT 0,
    audio_codec         TEXT             NOT NULL DEFAULT '',
    -- A picture's colour as ffprobe names it, which is how HDR is told from SDR
    -- (#50): PQ is smpte2084 and HLG arib-std-b67. dovi_profile is a Dolby Vision
    -- profile, zero for none: profile 5 has no picture an SDR player can be given.
    color_primaries     TEXT             NOT NULL DEFAULT '',
    color_transfer      TEXT             NOT NULL DEFAULT '',
    color_space         TEXT             NOT NULL DEFAULT '',
    dovi_profile        INTEGER          NOT NULL DEFAULT 0,
    artist              TEXT             NOT NULL DEFAULT '',
    album               TEXT             NOT NULL DEFAULT '',
    title               TEXT             NOT NULL DEFAULT '',
    track_no            INTEGER          NOT NULL DEFAULT 0,
    disc_no             INTEGER          NOT NULL DEFAULT 0,
    year                INTEGER          NOT NULL DEFAULT 0,
    genre               TEXT             NOT NULL DEFAULT '',
    album_artist        TEXT             NOT NULL DEFAULT '',

    -- The five columns a search matches, folded in Go and not by the engine
    -- (#85): what is compared must not depend on anyone's lower(). One tag
    -- each -- the title and the artist credited on the track are two columns
    -- because a search answers an artist as a row of its own, and a bucket
    -- promising the title cannot read a column that also holds a name (#262).
    --
    -- search_photo is what a photograph is found by, which is not its name: a
    -- camera calls everything IMG_0042.JPG. The camera, and the year it says
    -- the picture was taken -- not the month, which has a name only in a
    -- language, and not the place, because a coordinate is two numbers and
    -- turning one into "Lisbon" is a service this project does not have.
    search_title        TEXT             NOT NULL DEFAULT '',
    search_artist       TEXT             NOT NULL DEFAULT '',
    search_album        TEXT             NOT NULL DEFAULT '',
    search_album_artist TEXT             NOT NULL DEFAULT '',
    search_photo        TEXT             NOT NULL DEFAULT '',

    -- And the two vectors the GIN indexes below are over. The tags one holds
    -- the title, the album and the album artist together, and that is on
    -- purpose: the three buckets that ask about tags each match one column, so
    -- this one **narrows** -- the index gets the query off the library -- and
    -- the column itself decides, with its own to_tsvector in the WHERE. Exact,
    -- because a row whose album matches is in the combined vector too. Three
    -- vectors with a GIN each was the alternative, and it is three indexes
    -- written by every PutMedia to save an expression over what the index had
    -- already narrowed to. The credited artist is not in it: nothing matches
    -- that through an index here, since Music.Search scans with LIKE.
    search_tags         tsvector         GENERATED ALWAYS AS (
        to_tsvector('simple', translate(
            search_title || ' ' || search_album || ' ' || search_album_artist,
            '._-()[]', '       '))
    ) STORED,
    search_photo_text   tsvector         GENERATED ALWAYS AS (
        to_tsvector('simple', translate(search_photo, '._-()[]', '       '))
    ) STORED
);

CREATE INDEX media_taken_at ON media (taken_at);

CREATE INDEX media_kind_artist_album ON media (kind, album_artist, album);

-- The gallery's ORDER BY, and a month is a range of it.
CREATE INDEX media_kind_sort ON media (kind, sort_at, file_id);

CREATE INDEX media_search_tags ON media USING GIN (search_tags);

CREATE INDEX media_search_photo ON media USING GIN (search_photo_text);

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
    file_id    BIGINT      NOT NULL REFERENCES files (id) ON DELETE CASCADE,
    owner_id   TEXT        NOT NULL,
    starred_at TIMESTAMPTZ,
    rating     INTEGER     NOT NULL DEFAULT 0,
    -- Plays (#195): a count rather than a log of every play. Nothing asks when
    -- each one happened, only how many and the latest, and a log would grow
    -- for as long as somebody listens.
    play_count BIGINT      NOT NULL DEFAULT 0,
    played_at  TIMESTAMPTZ,
    PRIMARY KEY (file_id, owner_id)
);

-- kind is 'album' or 'artist'; an artist's album is ''.
CREATE TABLE tag_annotations (
    owner_id   TEXT        NOT NULL,
    kind       TEXT        NOT NULL,
    artist     TEXT        NOT NULL,
    album      TEXT        NOT NULL,
    starred_at TIMESTAMPTZ,
    rating     INTEGER     NOT NULL DEFAULT 0,
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
    id         BIGSERIAL   PRIMARY KEY,
    owner_id   TEXT        NOT NULL,
    name       TEXT        NOT NULL,
    comment    TEXT        NOT NULL DEFAULT '',
    public     BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL,
    changed_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX playlists_owner ON playlists (owner_id, name);

CREATE TABLE playlist_entries (
    playlist_id BIGINT  NOT NULL REFERENCES playlists (id) ON DELETE CASCADE,
    position    INTEGER NOT NULL,
    file_id     BIGINT  NOT NULL REFERENCES files (id) ON DELETE CASCADE,
    PRIMARY KEY (playlist_id, position)
);

-- What the cascade from files seeks on.
CREATE INDEX playlist_entries_file ON playlist_entries (file_id);
