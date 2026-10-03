-- What a search box matches against (#259).
--
-- The column is the file's own name and not its path: searching for a folder's
-- name finds the folder, not the thousand photographs under it. The name is not
-- stored anywhere -- a row has the whole path and its parent -- so it is what is
-- left of the path after the parent and the slash, and nothing at all is cut
-- from a row at the root, where the parent is empty.
--
-- The separators a filename is made of are turned into spaces, so that the
-- words inside IMG_0001.JPG are three words. Seven nested REPLACEs because this
-- dialect has no TRANSLATE, which is ugly and is written once.
--
-- The collation is this table's one exception. Everything else in it is
-- utf8mb4_0900_bin, because a unique path must not fold Photo.jpg and photo.jpg
-- into one row (#16) -- but a FULLTEXT match uses the column's collation, and a
-- binary one would make searching case-sensitive. db.Finder promises case does
-- not matter, so this column alone is accent- and case-insensitive.
ALTER TABLE files ADD COLUMN search_name TEXT
    COLLATE utf8mb4_0900_ai_ci
    GENERATED ALWAYS AS (
        REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(
            SUBSTRING(path, CHAR_LENGTH(parent_path) + IF(parent_path = '', 1, 2)),
        '.', ' '), '_', ' '), '-', ' '), '(', ' '), ')', ' '), '[', ' '), ']', ' ')
    ) STORED;

ALTER TABLE files ADD FULLTEXT KEY files_search_name (search_name);

-- The same for a track's tags, over the folded columns that already exist. They
-- are lower-cased by internal/db before they are written, so a binary collation
-- matches them case-insensitively anyway -- the property #85 bought, and the
-- reason these three keep the table's collation while the one above does not.
ALTER TABLE media ADD FULLTEXT KEY media_search_tags (search_song, search_album, search_album_artist);
