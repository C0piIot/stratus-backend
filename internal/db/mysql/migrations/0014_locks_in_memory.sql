-- The lock tables go away, because the locks did (#192).
--
-- 0007 put WebDAV locks in a table and 0008 added the row two creations queue
-- on, and both existed for one reason: with two instances on one database,
-- a lock taken on one of them had to be honoured by the other. This server
-- runs as one process by design, so that reason is gone and the locks are back
-- in memory, where a held one cannot outlive the process holding it and there
-- is no lease to keep alive.
--
-- Forward only. 0007 and 0008 stay where they are -- they ran, and a migration
-- that ran is history -- and this is the statement that undoes them.
DROP TABLE IF EXISTS locks;

DROP TABLE IF EXISTS lock_guard;
