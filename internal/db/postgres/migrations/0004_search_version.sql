-- Which search wrote an album's row (#217). A row that found nothing is a
-- verdict on the search as much as on the album: when the search gets better
-- -- a second question by the first credited name, say -- what it found
-- nothing for is asked again, once, without a script. media.version is the
-- same mechanism for the extractor and thumb_generation for the pictures. A
-- row that found an id keeps it whatever the version says: the id was right.
ALTER TABLE album_lookups ADD COLUMN search_version INTEGER NOT NULL DEFAULT 0;
