-- Settings the web UI stores (#397): a value for a variable the environment
-- left unset, written as it would be typed there. The environment still wins,
-- so a row is what an install with nothing in its environment runs on, and is
-- ignored -- not deleted -- the day somebody sets the variable after all.
--
-- No owner: these are the server's, and Stratus has one user for good.
CREATE TABLE settings (
    name       TEXT    PRIMARY KEY,
    value      TEXT    NOT NULL,
    updated_at INTEGER NOT NULL
);
