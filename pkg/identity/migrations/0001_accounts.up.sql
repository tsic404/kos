-- Accounts, sessions, login counters, preferences and audit (C-61, C-63).
-- Timestamps are Unix milliseconds so comparisons never depend on a text
-- format; a session's idle and absolute bounds are both stored as deadlines.

CREATE TABLE users (
    id                   TEXT PRIMARY KEY,
    username             TEXT NOT NULL UNIQUE,
    role                 TEXT NOT NULL CHECK (role IN ('Owner', 'Member', 'Guest')),
    password_hash        TEXT NOT NULL,
    is_disabled          INTEGER NOT NULL DEFAULT 0 CHECK (is_disabled IN (0, 1)),
    must_change_password INTEGER NOT NULL DEFAULT 0 CHECK (must_change_password IN (0, 1)),
    created_at           INTEGER NOT NULL,
    updated_at           INTEGER NOT NULL
);

CREATE INDEX users_role ON users(role);

CREATE TABLE sessions (
    token_hash          TEXT PRIMARY KEY,
    user_id             TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at          INTEGER NOT NULL,
    last_seen_at        INTEGER NOT NULL,
    idle_expires_at     INTEGER NOT NULL,
    absolute_expires_at INTEGER NOT NULL
);

CREATE INDEX sessions_user ON sessions(user_id);

-- The counter is keyed by the name typed at login, not by an account: a name
-- that does not exist must lock exactly like one that does (C-63).
CREATE TABLE login_attempts (
    login_name    TEXT PRIMARY KEY,
    failure_count INTEGER NOT NULL,
    locked_until  INTEGER,
    updated_at    INTEGER NOT NULL
);

CREATE TABLE preferences (
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    "key"      TEXT NOT NULL,
    value      TEXT NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (user_id, "key")
);

CREATE TABLE audit (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    actor       TEXT NOT NULL,
    object      TEXT NOT NULL,
    verb        TEXT NOT NULL,
    result      TEXT NOT NULL,
    source      TEXT NOT NULL,
    occurred_at INTEGER NOT NULL
);

CREATE INDEX audit_occurred_at ON audit(occurred_at);
