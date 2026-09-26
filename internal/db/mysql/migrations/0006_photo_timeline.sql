-- The moment a photo is listed by (#211): when the camera says it was taken,
-- and when the file arrived for one that says nothing, like a screenshot.
--
-- A column because the two halves live in different tables, and an ORDER BY
-- over an expression spanning both is one no index can serve: every page of the
-- gallery would sort every image the owner has, the linear shape #160 removed
-- from folder listings. Written by PutMedia, and filled here for the rows that
-- already exist, so no re-index is needed.
ALTER TABLE media ADD COLUMN sort_at BIGINT NULL;

UPDATE media m JOIN files f ON f.id = m.file_id SET m.sort_at = COALESCE(m.taken_at, f.mtime);

-- The gallery's ORDER BY, and a month is a range of it.
CREATE INDEX media_kind_sort ON media (kind, sort_at, file_id);
