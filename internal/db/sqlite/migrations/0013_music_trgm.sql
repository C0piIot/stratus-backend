-- The index OpenSubsonic's search3 answers from (#262).
--
-- Music.Search is LIKE '%term%' over three folded columns, which is a scan of
-- the library on every keystroke a client's search box sends: measured at 310
-- to 360 ms over fifty thousand tracks, whatever the term.
--
-- A trigram tokenizer and not unicode61, which is the opposite of what 0011
-- chose for the file names, and on purpose. There the promise was whole words
-- and the substring matching LIKE gave was never part of it. Here it is: a
-- Subsonic client searches as somebody types, so "ute" has to keep finding
-- Autechre, and a client cannot be told the server changed its mind. The
-- promise is what decides the tokenizer, not the other way round.
--
-- Measured on the same fifty thousand: a term nothing matches goes from 310 ms
-- to 1.5 ms, and one matching a sixth of the library from 357 to 103. The
-- second number stays large because the artist and album buckets group over
-- whatever matched, which is the shape of the question and not of the index.
--
-- The other two engines keep scanning, and that is this release's answer rather
-- than an oversight. PostgreSQL can do this with pg_trgm, and the measurement
-- said not yet: 90 ms to 52, for an extension that a role may not be allowed to
-- create -- a server that will not start, bought with a 1.7x. MySQL has only
-- the ngram parser, which is not exact substring and depends on a server
-- variable. The numbers are on #262.
CREATE VIRTUAL TABLE media_fts USING fts5(
    search_song,
    search_album,
    search_album_artist,
    content = 'media',
    content_rowid = 'file_id',
    tokenize = 'trigram'
);

-- The same three triggers as 0011, for the same reason: an external-content
-- index is told what changed, and a delete has to hand it the text that is
-- going away. PutMedia is an upsert, so the update is the one that runs most.
CREATE TRIGGER media_fts_insert AFTER INSERT ON media BEGIN
    INSERT INTO media_fts (rowid, search_song, search_album, search_album_artist)
        VALUES (new.file_id, new.search_song, new.search_album, new.search_album_artist);
END;

CREATE TRIGGER media_fts_delete AFTER DELETE ON media BEGIN
    INSERT INTO media_fts (media_fts, rowid, search_song, search_album, search_album_artist)
        VALUES ('delete', old.file_id, old.search_song, old.search_album, old.search_album_artist);
END;

CREATE TRIGGER media_fts_update AFTER UPDATE ON media BEGIN
    INSERT INTO media_fts (media_fts, rowid, search_song, search_album, search_album_artist)
        VALUES ('delete', old.file_id, old.search_song, old.search_album, old.search_album_artist);
    INSERT INTO media_fts (rowid, search_song, search_album, search_album_artist)
        VALUES (new.file_id, new.search_song, new.search_album, new.search_album_artist);
END;

INSERT INTO media_fts (media_fts) VALUES ('rebuild');
