-- WebDAV locks, which used to live in the process and therefore lasted until
-- the next restart and were invisible to a second instance (#243).
--
-- root is the URL form the If header carries -- "/" or "/notes.txt" -- and not
-- a storage path, because the adapter is the only thing that compares them and
-- that is the form it holds. Finding the lock that covers a resource is an IN
-- over the ancestors, computed in Go; taking one over a whole collection is a
-- range over root. Neither is a LIKE, and both ride the unique index below.
--
-- held_by and held_until are the request that has the lock taken right now, and
-- the lease on that hold. A lock with no lease would have to be freed by the
-- process that took it, which is exactly the process that may have died mid-PUT.
CREATE TABLE locks (
    token      TEXT        NOT NULL PRIMARY KEY,
    owner_id   TEXT        NOT NULL,
    root       TEXT        NOT NULL,
    zero_depth BOOLEAN     NOT NULL DEFAULT FALSE,
    owner_xml  TEXT        NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    held_by    TEXT        NOT NULL DEFAULT '',
    held_until TIMESTAMPTZ NOT NULL DEFAULT 'epoch',

    -- One lock per resource: this server takes exclusive write locks only, so
    -- two rows with the same root is the state it refuses. The index is what
    -- makes that refusal safe between two instances rather than a check one of
    -- them can lose.
    UNIQUE (owner_id, root)
);

-- What the sweep reads. Every other query filters on the expiry too, so this is
-- the one that would otherwise scan.
CREATE INDEX locks_expires_at ON locks (expires_at);
