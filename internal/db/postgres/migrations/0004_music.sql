ALTER TABLE media ADD COLUMN album_artist TEXT NOT NULL DEFAULT '';

CREATE INDEX media_kind_artist_album ON media (kind, album_artist, album);
