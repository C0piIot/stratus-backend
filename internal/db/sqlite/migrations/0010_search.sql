-- What a search box matches against (#259).
--
-- The column is the file's own name and not its path: searching for a folder's
-- name finds the folder, not the thousand photographs under it. The name is not
-- stored anywhere -- a row has the whole path and its parent -- so it is what is
-- left of the path after the parent and the slash, and nothing at all is cut
-- from a row at the root, where the parent is empty.
--
-- The separators a filename is made of are turned into spaces, so that the
-- words inside IMG_0001.JPG are three words. That is what makes the promise in
-- db.Finder true on every engine rather than on the one whose tokeniser happens
-- to split on a dot: PostgreSQL's reads photo.jpg as a single token, MySQL's
-- splits it, and neither is something to depend on.
--
-- STORED, and the difference was measured rather than assumed: a virtual column
-- is the expression itself, recomputed for every row a scan passes, and seven
-- nested replaces over a hundred thousand files cost 408 ms a search against
-- 172 stored. Worth knowing if this ever stops being accepted -- SQLite's own
-- documentation says ADD COLUMN takes a virtual generated column and not a
-- stored one, while the build we vendor takes both. A driver bump that removed
-- that would fail here, on a fresh database, with the conformance suite as the
-- thing that notices.
ALTER TABLE files ADD COLUMN search_name TEXT NOT NULL
    GENERATED ALWAYS AS (
        replace(replace(replace(replace(replace(replace(replace(
            substr(path, length(parent_path) + (CASE WHEN parent_path = '' THEN 1 ELSE 2 END)),
        '.', ' '), '_', ' '), '-', ' '), '(', ' '), ')', ' '), '[', ' '), ']', ' ')
    ) STORED;

-- No index on it, and that is the measurement too. The search orders by path
-- and takes fifty, so the planner walks files_owner_path -- which exists for the
-- unique constraint -- in order and applies the match as a filter, which is the
-- right plan: a term with hits finds its fifty and stops, 0.5 ms a page after
-- the first. An index on the name would be written by every upload and read by
-- nothing, since nothing here orders by it.
--
-- What it costs is the term nothing matches: 170 ms over a hundred thousand
-- files, the whole of one owner's rows walked to find out. That is this
-- driver's answer for now and the number to beat; FTS5 is in the pure-Go driver
-- and is where this goes next, which needs either triggers -- and db.Migrate
-- splits statements on semicolons, so it cannot carry one -- or the index
-- maintained from the driver on every write. Neither is earned by 170 ms yet.
