-- What a user has said about their library: stars and ratings (#194).
--
-- Two tables because a track is a row and an album or an artist is not. A
-- track's annotation goes with its file, which is what the foreign key is for;
-- the id already survives an overwrite and a rename. An album's is keyed by the
-- tags that make it one, so it outlives its tracks and a retag leaves it behind.
--
-- The owner is on both even while there is only one, because favourites are the
-- first thing sharing will want: somebody else's file can be my favourite.
-- file_id leads the key so the cascade on a delete is a seek.
--
-- A row with neither a star nor a rating is deleted rather than kept.

CREATE TABLE track_annotations (
    file_id    BIGINT       NOT NULL,
    owner_id   VARCHAR(255) NOT NULL,
    starred_at BIGINT       NULL,
    rating     INT          NOT NULL DEFAULT 0,

    PRIMARY KEY (file_id, owner_id),
    CONSTRAINT track_annotations_file FOREIGN KEY (file_id) REFERENCES files (id) ON DELETE CASCADE
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_0900_bin;

-- The tags are TEXT and cannot be a key, for the reason files.path cannot: the
-- key is key_hash, sha256 of the kind and both tags, computed by the adapter.
CREATE TABLE tag_annotations (
    owner_id   VARCHAR(255) NOT NULL,
    key_hash   BINARY(32)   NOT NULL,
    kind       VARCHAR(16)  NOT NULL,
    artist     TEXT         NOT NULL,
    album      TEXT         NOT NULL,
    starred_at BIGINT       NULL,
    rating     INT          NOT NULL DEFAULT 0,

    PRIMARY KEY (owner_id, key_hash)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_0900_bin;
