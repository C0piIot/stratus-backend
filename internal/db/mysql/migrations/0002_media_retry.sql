-- When the queue should offer this file again, and NULL when never.
--
-- A failure to reach the bytes is not a verdict on them (#157): the row records
-- what happened so that /status can say it, and a time to find out again. A
-- file nothing can parse gets no such time.
ALTER TABLE media ADD COLUMN retry_at BIGINT NULL;
