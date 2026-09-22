-- Migration 011: Durable recovery intent, progress, plan, and resume day.
CREATE TABLE IF NOT EXISTS recovery_runs (
    id TEXT PRIMARY KEY,
    connection_id TEXT NOT NULL REFERENCES connections(id),
    generation INTEGER NOT NULL,
    event_key TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'PENDING',
    progress_json TEXT NOT NULL DEFAULT '{}',
    error_code TEXT,
    error_message TEXT,
    claimed_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    finished_at TEXT,
    reason TEXT NOT NULL DEFAULT '',
    range_from TEXT NOT NULL DEFAULT '',
    range_to TEXT NOT NULL DEFAULT '',
    next_day TEXT NOT NULL DEFAULT '',
    UNIQUE(connection_id, generation, event_key)
);

CREATE INDEX IF NOT EXISTS idx_recovery_runs_current_status
ON recovery_runs(connection_id, generation, status, created_at, id);
