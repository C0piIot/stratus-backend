-- Plays, on the row a track's star and rating are already on (#195). A count
-- rather than a log of every play: nothing asks when each one happened, only how
-- many and the latest, and a log would grow for as long as somebody listens.
ALTER TABLE track_annotations ADD COLUMN play_count BIGINT NOT NULL DEFAULT 0;

ALTER TABLE track_annotations ADD COLUMN played_at TIMESTAMPTZ;
