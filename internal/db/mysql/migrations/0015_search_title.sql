-- The folded song becomes two columns, and one FULLTEXT key per column (#262).
--
-- A search now answers an artist as a row of its own, so the bucket that
-- answers tracks has to be about the title and nothing else. It could not be:
-- search_song held the title and the artist credited on the track together,
-- which is why searching for somebody's name came back as everything they ever
-- recorded. Two columns, one tag each.
--
-- **And three keys where there was one, which is this engine's alone.** A
-- MATCH has to name exactly the columns some FULLTEXT key was built on and
-- there is no column-restricted form of it, so three buckets asking about one
-- column each need three indexes. Nothing is lost with media_search_tags: its
-- only reader was the track bucket, and that is the reader that stopped asking
-- about three columns at once. SQLite restricts a match to a column of its
-- FTS5 table and PostgreSQL uses the combined vector to narrow and the column
-- to decide, so neither of them has this migration's second half.
--
-- No key on search_artist: nothing matches it through an index. Music.Search
-- scans with LIKE here, which is #262's measurement and not an oversight.
--
-- The columns keep the table's binary collation, because internal/db folds
-- them before they are written (#85) -- unlike files.search_name, which is
-- generated from a path nothing folds and therefore needs an accent- and
-- case-insensitive one.
ALTER TABLE media DROP KEY media_search_tags;

ALTER TABLE media RENAME COLUMN search_song TO search_title;
ALTER TABLE media ADD COLUMN search_artist TEXT NOT NULL;

ALTER TABLE media ADD FULLTEXT KEY media_search_title (search_title);
ALTER TABLE media ADD FULLTEXT KEY media_search_album (search_album);
ALTER TABLE media ADD FULLTEXT KEY media_search_album_artist (search_album_artist);
