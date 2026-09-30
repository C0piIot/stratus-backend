-- WebDAV locks, which used to live in the process and therefore lasted until
-- the next restart and were invisible to a second instance (#243).
--
-- root is the URL form the If header carries -- "/" or "/notes.txt" -- and not
-- a storage path, because the adapter is the only thing that compares them and
-- that is the form it holds. Finding the lock that covers a resource is an IN
-- over the ancestors, computed in Go; taking one over a whole collection is a
-- range over root. Neither is a LIKE.
--
-- root_hash carries the uniqueness for the reason files has one: a lock root is
-- a path, MySQL cannot index a TEXT column whole, and a prefix index would make
-- two roots sharing 500 characters the same lock. The prefix index beside it is
-- for narrowing, which is all it has to do -- the engine still compares the
-- whole value, and this table holds one row per lock a client is holding rather
-- than one per file.
--
-- held_by and held_until are the request that has the lock taken right now, and
-- the lease on that hold. A lock with no lease would have to be freed by the
-- process that took it, which is exactly the process that may have died mid-PUT.
CREATE TABLE locks (
    token      VARCHAR(64)  NOT NULL PRIMARY KEY,
    owner_id   VARCHAR(255) NOT NULL,
    root       TEXT         NOT NULL,
    root_hash  BINARY(32)   NOT NULL,
    zero_depth TINYINT(1)   NOT NULL DEFAULT 0,
    owner_xml  TEXT         NOT NULL,
    expires_at BIGINT       NOT NULL,
    held_by    VARCHAR(64)  NOT NULL DEFAULT '',
    held_until BIGINT       NOT NULL DEFAULT 0,

    UNIQUE KEY locks_owner_root (owner_id, root_hash),
    KEY locks_owner_prefix (owner_id, root(500)),
    KEY locks_expires_at (expires_at)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_0900_bin;
