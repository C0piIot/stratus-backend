-- What a photograph is found by, which is not its name (#262).
--
-- A camera calls everything IMG_0042.JPG, so the name search that 0010 added
-- finds nothing anybody meant. What does say something is in this table: the
-- camera, and the year the camera says the picture was taken.
--
-- Not the month, which has a name only in a language -- English here, and a
-- promise to keep in every other one afterwards. Not where it was taken: a
-- coordinate is two numbers, and turning one into "Lisbon" is a service this
-- project does not have and principle 5 does not want.
--
-- The text is folded in Go, like the three columns beside it and for the same
-- reason (#85): what is compared must not depend on the engine's own lower().
-- Which is also why nothing is backfilled here. media.Version went to 7
-- instead, so the queue refills the column the way the extractor would have --
-- a pass over the library, and no second opinion about what lower() means.
ALTER TABLE media ADD COLUMN search_photo TEXT NOT NULL;

ALTER TABLE media ADD FULLTEXT KEY media_search_photo (search_photo);
