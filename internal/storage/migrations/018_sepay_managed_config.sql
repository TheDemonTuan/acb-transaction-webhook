-- No placeholder row: absence alone permits legacy file fallback.
CREATE TABLE sepay_managed_config (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    revision INTEGER NOT NULL CHECK (revision > 0),
    config_envelope BLOB NOT NULL,
    updated_at TEXT NOT NULL
);
