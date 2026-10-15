-- What is known about a file's pictures, so that something other than a
-- request can make them (#341).
--
-- The indexer's trick cannot be copied here. What is pending for it is a LEFT
-- JOIN over the file rows -- nothing enqueued, nothing dequeued -- and that
-- works because "indexed" is a row. "Has a thumbnail" is a blob, and no query
-- joins against the blob store: a Stat per file per pass is a HEAD per file on
-- S3, and a set in memory drifts and dies with the process. So the fact is a
-- column, and this is the first migration since the schema became one (#269).
--
-- thumb_blob_key and not the etag, although the row above it compares etags:
-- what the state describes is derived/<blob key>/..., so the exact question is
-- whether the file still points at the bytes the pictures were made from. It
-- is also never empty, which an etag is on a row adopted from a Nextcloud
-- instance (#24) -- there is no hash of the content on that side to carry over.
--
-- thumb_at is #338's week, moved out of the sweep and into the query: a
-- refusal is offered again seven days after it was written, because the one
-- thing a verdict cannot know is whether the build that reached it is still
-- this build. The note in the blob store stays as words for whoever reads a
-- data directory, and as the verdict for cover art, which is a second picture
-- per blob and answers a different surface.
--
-- PutMedia does not write any of these. Its upsert names its columns one at a
-- time, so they survive a re-index: raising media.Version must not regenerate
-- a library of pictures that were never wrong.
--
-- No index. The predicate is a disjunction over four columns, which nothing
-- can seek; what the pass walks is media_kind_sort, by the kind and in the
-- order the gallery already pages in, and the state is read per row. That is
-- the cost PendingMedia already pays for the same reason.
--
-- Three things are this driver's own. The key is a VARCHAR and not a TEXT,
-- because MySQL takes no default on a TEXT column and a column added to a
-- table with rows in it needs one; 1024 is storage.MaxKeyLen, so nothing a key
-- may be is cut. The time is a BIGINT of milliseconds like every other time
-- here. And the four are one ALTER rather than four: DDL is not transactional
-- on this engine, so four statements is four chances to stop half way and
-- leave the schema between two versions (#242).
ALTER TABLE media
    ADD COLUMN thumb_state      INT           NOT NULL DEFAULT 0,
    ADD COLUMN thumb_blob_key   VARCHAR(1024) NOT NULL DEFAULT '',
    ADD COLUMN thumb_generation INT           NOT NULL DEFAULT 0,
    ADD COLUMN thumb_at         BIGINT        NULL;
