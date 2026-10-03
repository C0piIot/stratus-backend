-- What a search box matches against (#259).
--
-- The column is the file's own name and not its path: searching for a folder's
-- name finds the folder, not the thousand photographs under it. The name is not
-- stored anywhere -- a row has the whole path and its parent -- so it is what is
-- left of the path after the parent and the slash, and nothing at all is cut
-- from a row at the root, where the parent is empty.
--
-- The separators a filename is made of are turned into spaces before the
-- tokeniser sees them, and that is not optional here: this parser reads
-- photo.jpg as one token called a file, so searching photo would find nothing
-- at all. With them flattened it is photo and jpg, which is what db.Finder
-- promises.
--
-- 'simple' and not a language configuration: a filename is not prose, and
-- English stemming would turn notes into note and make a search for the name
-- somebody typed miss it.
ALTER TABLE files ADD COLUMN search_name tsvector
    GENERATED ALWAYS AS (
        to_tsvector('simple', translate(
            substring(path FROM char_length(parent_path) + (CASE WHEN parent_path = '' THEN 1 ELSE 2 END)),
            '._-()[]', '       '))
    ) STORED;

CREATE INDEX files_search_name ON files USING GIN (search_name);

-- The same for a track's tags, over the folded columns that already exist. They
-- are lower-cased by internal/db before they are written, so matching them is
-- case-insensitive by construction rather than by collation -- which is the
-- property #85 bought and this does not give up.
ALTER TABLE media ADD COLUMN search_tags tsvector
    GENERATED ALWAYS AS (
        to_tsvector('simple', translate(
            search_song || ' ' || search_album || ' ' || search_album_artist,
            '._-()[]', '       '))
    ) STORED;

CREATE INDEX media_search_tags ON media USING GIN (search_tags);
