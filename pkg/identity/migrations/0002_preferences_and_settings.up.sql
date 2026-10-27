-- Scope preferences by extension identity (C-27), record the declared fate of
-- each extension's data (C-29), and store the current value of a setting item
-- (C-23). A setting value is desired configuration, not a mirror of cluster
-- state (C-61).

CREATE TABLE preferences_scoped (
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    extension  TEXT NOT NULL,
    "key"      TEXT NOT NULL,
    value      TEXT NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (user_id, extension, "key")
);

INSERT INTO preferences_scoped (user_id, extension, "key", value, updated_at)
    SELECT user_id, '', "key", value, updated_at FROM preferences;

DROP TABLE preferences;

ALTER TABLE preferences_scoped RENAME TO preferences;

CREATE TABLE preference_domains (
    identifier TEXT PRIMARY KEY,
    component  TEXT NOT NULL,
    keep       INTEGER NOT NULL CHECK (keep IN (0, 1)),
    updated_at INTEGER NOT NULL
);

CREATE TABLE setting_values (
    scope      TEXT NOT NULL,
    target     TEXT NOT NULL,
    setting_id TEXT NOT NULL,
    value      TEXT NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (scope, target, setting_id)
);
