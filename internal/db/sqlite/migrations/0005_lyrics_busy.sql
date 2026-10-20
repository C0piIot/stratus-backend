-- LRCLIB's 503 was written down as "not held" and parked for a month, when it
-- means the track is still being fetched and an hour later is usually the
-- answer. Those rows are due now; the pass defers whatever is still busy.
UPDATE lyrics SET retry_at = fetched_at WHERE error LIKE '%too busy to fetch it%';
