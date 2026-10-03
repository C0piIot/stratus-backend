-- The schema, as one migration.
--
-- It was fifteen until #269. Keeping the steps was buying nothing: there is no
-- database anywhere that needs them, and a reader who wants to know what a
-- column is for had to reconstruct it from a chain of ALTERs. What a migration
-- file is really worth is the paragraph beside each decision, and those are all
-- here -- the issue numbers with them, so the argument is still findable.
--
-- The way back is not supported and does not pretend to be: a database written
-- by the fifteen is at version 15, and db.Migrate refuses a schema it does not
-- know rather than running against it. The answer is a new data directory.
--
-- From here the next one is 0002 and nothing is ever edited in place again.

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
    is_dir      INTEGER NOT NULL DEFAULT 0,

    -- What a search box matches against (#259): the file's own name, which no
    -- row stores -- it is what is left of the path after the parent, and
    -- nothing at all is cut from a row at the root, where the parent is empty.
    -- The name and not the path, so that a word in a folder finds the folder
    -- and not the thousand photographs under it.
    --
    -- The separators a filename is made of are turned into spaces, so the
    -- words inside IMG_0001.JPG are three words. That is what makes
    -- db.Finder's promise true on every engine rather than on the one whose
    -- tokeniser happens to split on a dot: PostgreSQL's reads photo.jpg as a
    -- single token, MySQL's splits it, and neither is something to depend on.
    --
    -- STORED, and the difference was measured rather than assumed: a virtual
    -- column is the expression itself, recomputed for every row a scan passes,
    -- and seven nested replaces over a hundred thousand files cost 408 ms a
    -- search against 172 stored.
    search_name TEXT    NOT NULL GENERATED ALWAYS AS (
        replace(replace(replace(replace(replace(replace(replace(
            substr(path, length(parent_path) + (CASE WHEN parent_path = '' THEN 1 ELSE 2 END)),
        '.', ' '), '_', ' '), '-', ' '), '(', ' '), ')', ' '), '[', ' '), ']', ' ')
    ) STORED
);

CREATE UNIQUE INDEX files_owner_path ON files (owner_id, path);

-- The trailing columns are the paged listing's ORDER BY, in its order and in
-- its direction: a page of a directory is a seek into this index rather than a
-- sort of everything under parent_path, and a collection sorts before a file
-- because that is what a file manager shows.
--
-- It is one index for two orderings, and the whole-directory listing is the
-- one that pays: ordered by path alone, it sorts what it read instead of
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
-- DESC, which is a descending page, while the group stays where it was. Had
-- is_dir stayed in the ORDER BY, reversing the scan would have reversed the
-- grouping too and each of these would have needed a descending twin.
--
-- The cursor has to be a row comparison -- (size, path) > (?, ?) -- for either
-- of these to be a seek, which is the same thing the photo timeline found in
-- #211 and is worth measuring rather than assuming. On a folder of a hundred
-- thousand files, page 900 cost 44 ms with the spelled-out OR and 0.78 ms with
-- the row comparison, against 0.43 ms for the ordering by path. Without the
-- index at all it is a sort of the whole folder.
--
-- MySQL has neither. Its parent_path is indexed by a prefix, because a path is
-- TEXT there, and nothing after a prefix column can satisfy an ORDER BY -- so
-- either of these would be written by every write and read by nothing.
CREATE INDEX files_owner_parent_size ON files (owner_id, parent_path, is_dir, size, path);

CREATE INDEX files_owner_parent_mtime ON files (owner_id, parent_path, is_dir, mtime, path);

-- The search index this engine answers names from (#261), measured on a
-- hundred thousand files:
--
--   a term nothing matches        170 ms  ->  0.37 ms
--   a term a sixth of them match    0.5 ms  ->  31 ms
--   a term all of them match        0.6 ms  ->  80 ms
--
-- The last two got worse and the trade is still right. The LIKE this replaced
-- was quick there because it walked files in path order and stopped at fifty;
-- the index cannot, because the order is the path and the matches arrive in
-- rowid order, so every match is sorted before fifty are taken. What that buys
-- is that the worst case is a term matching the whole library -- a search
-- nobody meant -- instead of a typo, and the typo is what people actually do.
--
-- The join is written CROSS JOIN in the driver for exactly this: without it
-- the planner drives from files in path order and probes the index per row,
-- which is 3.9 seconds for the term that matches nothing. The order of the two
-- tables is the whole difference between 0.37 ms and that.
--
-- unicode61 and not trigram, which is a decision about what is promised rather
-- than about speed: whole words are what db.Finder guarantees and what the
-- other two engines do, so all three answer the same question.
--
-- External content: the index holds the terms and files holds the text, so a
-- name is not stored twice. It is still 10 MiB over a hundred thousand files,
-- about a hundred bytes each, in somebody's data directory.
CREATE VIRTUAL TABLE files_fts USING fts5(
    search_name,
    content = 'files',
    content_rowid = 'id',
    tokenize = 'unicode61'
);

-- An external-content index is told what changed; it cannot look at the old
-- row itself, so a delete and an update have to hand it the text that is going
-- away. Getting that wrong leaves terms in the index pointing at rows that no
-- longer say them, which reads as a search finding something that is not
-- there.
--
-- Triggers and not driver code, and that is the whole reason db.Migrate learned
-- to carry a trigger body: MoveFile rewrites a subtree in one statement, and
-- search_name is generated from the path, so every row it touches has to be
-- re-indexed. Here that stays one statement; from Go it would have been two
-- per row. Measured where it is worst -- a subtree of a hundred thousand rows,
-- and therefore a hundred thousand firings -- it went from 4.5 to 5.4 seconds.
CREATE TRIGGER files_fts_insert AFTER INSERT ON files BEGIN
    INSERT INTO files_fts (rowid, search_name) VALUES (new.id, new.search_name);
END;

CREATE TRIGGER files_fts_delete AFTER DELETE ON files BEGIN
    INSERT INTO files_fts (files_fts, rowid, search_name)
        VALUES ('delete', old.id, old.search_name);
END;

CREATE TRIGGER files_fts_update AFTER UPDATE ON files BEGIN
    INSERT INTO files_fts (files_fts, rowid, search_name)
        VALUES ('delete', old.id, old.search_name);
    INSERT INTO files_fts (rowid, search_name) VALUES (new.id, new.search_name);
END;

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
    -- The moment the gallery lists a photograph by (#211): when the camera
    -- says it was taken, and when the file arrived for one that says nothing,
    -- like a screenshot. A column because the two halves live in different
    -- tables, and an ORDER BY over an expression spanning both is one no index
    -- can serve -- every page would sort every image the owner has. PutMedia
    -- writes it.
    sort_at             INTEGER,
    width               INTEGER NOT NULL DEFAULT 0,
    height              INTEGER NOT NULL DEFAULT 0,
    orientation         INTEGER NOT NULL DEFAULT 0,
    latitude            REAL,
    longitude           REAL,
    camera              TEXT    NOT NULL DEFAULT '',
    duration_ms         INTEGER NOT NULL DEFAULT 0,
    codec               TEXT    NOT NULL DEFAULT '',
    -- What a transcode decision is made from (#197, #207): whether a client
    -- can take the file as it is, and what a transcode must not exceed. Zero is
    -- unknown. codec_profile, bit_depth, level and frame_rate describe the
    -- row's own stream -- the picture, for a video -- and bitrate is the
    -- stream's for audio and the whole file's for a video. sample_rate,
    -- channels and audio_codec are the sound, a video's included: its audio
    -- track is what most often needs a transcode while the picture does not.
    bitrate             INTEGER NOT NULL DEFAULT 0,
    sample_rate         INTEGER NOT NULL DEFAULT 0,
    channels            INTEGER NOT NULL DEFAULT 0,
    bit_depth           INTEGER NOT NULL DEFAULT 0,
    codec_profile       TEXT    NOT NULL DEFAULT '',
    level               INTEGER NOT NULL DEFAULT 0,
    frame_rate          INTEGER NOT NULL DEFAULT 0,
    audio_codec         TEXT    NOT NULL DEFAULT '',
    -- A picture's colour as ffprobe names it, which is how HDR is told from SDR
    -- (#50): PQ is smpte2084 and HLG arib-std-b67. dovi_profile is a Dolby Vision
    -- profile, zero for none: profile 5 has no picture an SDR player can be given.
    color_primaries     TEXT    NOT NULL DEFAULT '',
    color_transfer      TEXT    NOT NULL DEFAULT '',
    color_space         TEXT    NOT NULL DEFAULT '',
    dovi_profile        INTEGER NOT NULL DEFAULT 0,
    artist              TEXT    NOT NULL DEFAULT '',
    album               TEXT    NOT NULL DEFAULT '',
    title               TEXT    NOT NULL DEFAULT '',
    track_no            INTEGER NOT NULL DEFAULT 0,
    disc_no             INTEGER NOT NULL DEFAULT 0,
    year                INTEGER NOT NULL DEFAULT 0,
    genre               TEXT    NOT NULL DEFAULT '',
    album_artist        TEXT    NOT NULL DEFAULT '',

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
    search_title        TEXT    NOT NULL DEFAULT '',
    search_artist       TEXT    NOT NULL DEFAULT '',
    search_album        TEXT    NOT NULL DEFAULT '',
    search_album_artist TEXT    NOT NULL DEFAULT '',
    search_photo        TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX media_taken_at ON media (taken_at);

CREATE INDEX media_kind_artist_album ON media (kind, album_artist, album);

-- The gallery's ORDER BY, and a month is a range of it.
CREATE INDEX media_kind_sort ON media (kind, sort_at, file_id);

-- The index OpenSubsonic's search3 answers from, and the three buckets a search
-- asks about tags (#262).
--
-- A trigram tokenizer, which is the opposite of what files_fts chose and on
-- purpose. There the promise is whole words and the substring matching LIKE
-- gave was never part of it. Here it is: a Subsonic client searches as somebody
-- types, so "ute" has to keep finding Autechre, and a client cannot be told the
-- server changed its mind. The promise decides the tokenizer, not the other way
-- round.
--
-- Measured on fifty thousand tracks: a term nothing matches went from 310 ms to
-- 1.5 ms. A term matching a sixth of the library stays large -- 103 ms -- because
-- the artist and album buckets group over whatever matched, which is the shape
-- of the question and not of the index. The CROSS JOIN lesson is the same one
-- files_fts paid for: written as a subquery the planner walks every audio row
-- and probes the index per row, which was 80 ms of the 310.
--
-- Four columns, asked one at a time -- `search_album : "x"` -- which is how one
-- index answers four questions. The other two engines cannot do that: MySQL
-- carries a FULLTEXT key per column and PostgreSQL narrows with a combined
-- vector and decides with the column.
CREATE VIRTUAL TABLE media_fts USING fts5(
    search_title,
    search_artist,
    search_album,
    search_album_artist,
    content = 'media',
    content_rowid = 'file_id',
    tokenize = 'trigram'
);

-- The same three as files_fts, for the same reason. PutMedia is an upsert, so
-- the update is the one that runs most.
CREATE TRIGGER media_fts_insert AFTER INSERT ON media BEGIN
    INSERT INTO media_fts (rowid, search_title, search_artist, search_album, search_album_artist)
        VALUES (new.file_id, new.search_title, new.search_artist, new.search_album, new.search_album_artist);
END;

CREATE TRIGGER media_fts_delete AFTER DELETE ON media BEGIN
    INSERT INTO media_fts (media_fts, rowid, search_title, search_artist, search_album, search_album_artist)
        VALUES ('delete', old.file_id, old.search_title, old.search_artist, old.search_album, old.search_album_artist);
END;

CREATE TRIGGER media_fts_update AFTER UPDATE ON media BEGIN
    INSERT INTO media_fts (media_fts, rowid, search_title, search_artist, search_album, search_album_artist)
        VALUES ('delete', old.file_id, old.search_title, old.search_artist, old.search_album, old.search_album_artist);
    INSERT INTO media_fts (rowid, search_title, search_artist, search_album, search_album_artist)
        VALUES (new.file_id, new.search_title, new.search_artist, new.search_album, new.search_album_artist);
END;

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
