-- The Nextcloud fixture, and what nextcloud.db beside it was built from.
--
-- It is committed as a database rather than built by the tests because the two
-- that read it cannot build one: internal/app may not import a SQLite driver,
-- and scripts/smoke.sh runs against the shipped container. This file is here so
-- the next person can change it:
--
--     rm scripts/testdata/nextcloud.db
--     sqlite3 scripts/testdata/nextcloud.db < scripts/testdata/nextcloud.sql
--
-- Cut down to the columns internal/nextcloud reads, and no further: a survey
-- tested against a hand-rolled fake of the filecache would be testing the fake,
-- and the shape of that table is the whole thing being relied on.

CREATE TABLE oc_storages (numeric_id INTEGER PRIMARY KEY, id TEXT NOT NULL);
CREATE TABLE oc_mimetypes (id INTEGER PRIMARY KEY, mimetype TEXT NOT NULL);
CREATE TABLE oc_filecache (
	fileid INTEGER PRIMARY KEY, storage INTEGER NOT NULL, path TEXT NOT NULL,
	size INTEGER NOT NULL, mtime INTEGER NOT NULL DEFAULT 0,
	mimetype INTEGER, encrypted INTEGER NOT NULL DEFAULT 0);
CREATE TABLE oc_appconfig (appid TEXT NOT NULL, configkey TEXT NOT NULL, configvalue TEXT);
CREATE TABLE oc_preferences (
	userid TEXT NOT NULL, appid TEXT NOT NULL, configkey TEXT NOT NULL, configvalue TEXT);

INSERT INTO oc_mimetypes VALUES (1, 'httpd/unix-directory'), (2, 'image/jpeg');
INSERT INTO oc_storages VALUES (1, 'object::user:edu');

-- held.jpg is in the bucket and gone.jpg is not, which is the drift the survey
-- exists to find. The album is here so that an import has a folder to rebuild.
INSERT INTO oc_filecache (fileid, storage, path, size, mtime, mimetype) VALUES
	(1, 1, '',                       0, 0,          1),
	(2, 1, 'files',                  0, 1700000000, 1),
	(3, 1, 'files/held.jpg',         7, 1700000001, 2),
	(4, 1, 'files/gone.jpg',         9, 1700000002, 2),
	(5, 1, 'files_versions/held.jpg', 3, 1700000003, 2),
	(6, 1, 'files/album',            0, 1700000004, 1),
	(7, 1, 'files/album/inner.jpg',  5, 1700000005, 2);
