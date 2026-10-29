-- Reverse the scoping of preferences and the setting value table. Rows the
-- scoped schema added outside an extension domain cannot be represented by the
-- un-scoped primary key, so only the kernel domain and the legacy rows survive.

CREATE TABLE preferences_unscoped (
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    "key"      TEXT NOT NULL,
    value      TEXT NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (user_id, "key")
);

INSERT INTO preferences_unscoped (user_id, "key", value, updated_at)
    SELECT user_id, "key", value, updated_at FROM preferences WHERE extension = '';

DROP TABLE preferences;

ALTER TABLE preferences_unscoped RENAME TO preferences;

DROP TABLE setting_values;

DROP TABLE preference_domains;
