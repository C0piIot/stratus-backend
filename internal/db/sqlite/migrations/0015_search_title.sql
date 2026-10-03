-- The folded song becomes two columns, and the index follows it (#262).
--
-- A search now answers an artist as a row of its own, so the bucket that
-- answers tracks has to be about the title and nothing else. It could not be:
-- search_song held the title and the artist credited on the track together,
-- which is why searching for somebody's name came back as everything they ever
-- recorded. Two columns, one tag each.
--
-- Music.Search reads both and is unchanged by this -- OpenSubsonic's search3
-- matched the two as one string and now matches them as two, which is the same
-- answer for every term that is not a phrase straddling the join between a
-- title and a name.
--
-- Nothing backfills the columns, for the third time and the same reason (#85):
-- the folding is Go's so the engines cannot disagree about it, and a SQL
-- backfill would be each engine's own lower(). media.Version went to 8
-- instead, so the indexer rewrites the rows as it passes over them and
-- /status is where that is watched.
DROP TRIGGER media_fts_insert;
DROP TRIGGER media_fts_delete;
DROP TRIGGER media_fts_update;

-- Before the rename, not after: SQLite rewrites references to a renamed column
-- inside triggers, and an external-content index whose columns no longer name
-- the table's is a schema that fails to open rather than an index that is
-- wrong.
DROP TABLE media_fts;

ALTER TABLE media RENAME COLUMN search_song TO search_title;
ALTER TABLE media ADD COLUMN search_artist TEXT NOT NULL DEFAULT '';

CREATE VIRTUAL TABLE media_fts USING fts5(
    search_title,
    search_artist,
    search_album,
    search_album_artist,
    content = 'media',
    content_rowid = 'file_id',
    tokenize = 'trigram'
);

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

INSERT INTO media_fts (media_fts) VALUES ('rebuild');
