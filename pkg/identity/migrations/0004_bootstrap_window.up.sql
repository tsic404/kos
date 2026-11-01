-- C-68: the external-control-plane form delivers the first Owner's one-time
-- password through a claim window instead of the install output. The window is
-- recorded once per control-plane database, so a restart cannot move the
-- deadline and a reinstall that keeps the database cannot reopen it. The single
-- row is the whole state: opened_at is when the installation finished, deadline
-- is opened_at plus the fixed window, and claimed_at marks the one delivery.

CREATE TABLE bootstrap_window (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    opened_at  INTEGER NOT NULL,
    deadline   INTEGER NOT NULL,
    claimed_at INTEGER
);
