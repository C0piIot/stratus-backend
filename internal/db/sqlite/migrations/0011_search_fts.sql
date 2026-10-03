-- The search index SQLite answers from (#261).
--
-- 0010 left it as a LIKE over search_name, with the number it cost written
-- down: a term that matches nothing has nowhere to stop, so it walked one
-- owner's whole library -- 170 ms over a hundred thousand files, against
-- 0.5 ms for a term with hits. This is that number paid off, and measured on
-- the same hundred thousand:
--
--   a term nothing matches     170 ms  ->  0.37 ms
--   a term a sixth of them match  0.5 ms  ->  31 ms
--   a term all of them match      0.6 ms  ->  80 ms
--
-- The last two got worse and the trade is still right. LIKE was quick there
-- because it walked files in path order and stopped at fifty; the index cannot,
-- because the order is the path and the matches arrive in rowid order, so every
-- match is sorted before fifty are taken. What that buys is that the worst case
-- is now a term matching the whole library -- a search nobody meant -- instead
-- of a typo, and the typo is what people actually do.
--
-- The join is written CROSS JOIN in the driver for exactly this: without it the
-- planner drives from files in path order and probes the index per row, which
-- is 3.9 seconds for the term that matches nothing. The order of the two tables
-- is the whole difference between 0.37 ms and that.
--
-- unicode61 and not trigram, which is a decision about what is promised rather
-- than about speed: whole words are what db.Finder guarantees and what the
-- other two engines do, so all three now answer the same question. SQLite used
-- to find inside a word, because LIKE does; that was never promised and is
-- gone.
--
-- External content: the index holds the terms and files holds the text, so a
-- name is not stored twice. It is still 10 MiB over a hundred thousand files,
-- about a hundred bytes each, in somebody's data directory.
--
-- What it costs to write is the three triggers below, which are the whole
-- reason db.Migrate learned to carry a trigger body. Measured where it is
-- worst: moving a subtree of a hundred thousand rows, which is one statement
-- and therefore a hundred thousand trigger firings, went from 4.5 to 5.4
-- seconds. A single upload is dominated by its commit and does not show it.
CREATE VIRTUAL TABLE files_fts USING fts5(
    search_name,
    content = 'files',
    content_rowid = 'id',
    tokenize = 'unicode61'
);

-- An external-content index is told what changed; it cannot look at the old row
-- itself, so a delete and an update have to hand it the text that is going
-- away. Getting that wrong leaves terms in the index pointing at rows that no
-- longer say them, which reads as a search finding something that is not there.
CREATE TRIGGER files_fts_insert AFTER INSERT ON files BEGIN
    INSERT INTO files_fts (rowid, search_name) VALUES (new.id, new.search_name);
END;

CREATE TRIGGER files_fts_delete AFTER DELETE ON files BEGIN
    INSERT INTO files_fts (files_fts, rowid, search_name)
        VALUES ('delete', old.id, old.search_name);
END;

-- The update is why this is a trigger and not driver code: MoveFile rewrites a
-- whole subtree in one statement, and search_name is generated from the path,
-- so every row it touches has to be re-indexed. Here that is still one
-- statement; from Go it would have been two per row.
CREATE TRIGGER files_fts_update AFTER UPDATE ON files BEGIN
    INSERT INTO files_fts (files_fts, rowid, search_name)
        VALUES ('delete', old.id, old.search_name);
    INSERT INTO files_fts (rowid, search_name) VALUES (new.id, new.search_name);
END;

-- And what is already stored, for a database that has been running since before
-- this. One statement, and the only one here that reads every row.
INSERT INTO files_fts (files_fts) VALUES ('rebuild');
