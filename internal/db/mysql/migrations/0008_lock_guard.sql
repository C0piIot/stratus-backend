-- A row per owner, taken by every lock creation before it looks at anything
-- else (#244).
--
-- The covering check rides inside the INSERT, which closes the window between
-- reading and writing but not the one between two statements: at READ COMMITTED
-- neither instance sees the other's uncommitted row, so a LOCK on /a and a LOCK
-- on /a/b arriving together on two instances both commit, and afterwards
-- neither holder can write -- each finds the other's lock covering its own.
--
-- The unique index on the root catches two locks with the same root, which is
-- the common case. It cannot see an ancestor or a descendant, and no index can:
-- what has to be excluded is a relationship between two rows. So the two
-- creations are made to queue on something an index does see, which is this.
--
-- One column, and it is the key. There is nothing to store: the row is the
-- rendezvous and not a record.
CREATE TABLE lock_guard (
    owner_id VARCHAR(255) NOT NULL PRIMARY KEY
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_0900_bin;
