-- LRCLIB is asked again by the first name of a credit of several when the
-- whole credit found nothing, as MusicBrainz already was. The tracks that
-- would gain are the ones parked for a month behind "nothing there" under such
-- a credit, so those rows are due now; the pass writes a month again for
-- whatever the first name does not find either. A credit joined by a semicolon
-- is left to its month: the migration runner splits statements on one.
UPDATE lyrics SET retry_at = fetched_at
WHERE error LIKE '%nothing there%'
  AND file_id IN (
    SELECT file_id FROM media
    WHERE artist LIKE '%, %' OR artist LIKE '% feat%'
       OR artist LIKE '% ft. %' OR artist LIKE '% & %'
  );
