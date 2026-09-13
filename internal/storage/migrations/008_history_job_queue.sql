-- Migration 008: Durable history job queue and fenced progress
ALTER TABLE history_sync_jobs ADD COLUMN generation INTEGER NOT NULL DEFAULT 0;
ALTER TABLE history_sync_jobs ADD COLUMN pages_done INTEGER NOT NULL DEFAULT 0;
ALTER TABLE history_sync_jobs ADD COLUMN current_day TEXT;
ALTER TABLE history_sync_jobs ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE history_sync_jobs ADD COLUMN next_attempt_at TEXT;
ALTER TABLE history_sync_jobs ADD COLUMN error_code TEXT;
ALTER TABLE history_sync_jobs ADD COLUMN started_at TEXT;
ALTER TABLE history_sync_jobs ADD COLUMN heartbeat_at TEXT;
ALTER TABLE history_sync_jobs ADD COLUMN finished_at TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS uq_active_history_sync_jobs
ON history_sync_jobs(connection_id, generation, range_from, range_to)
WHERE status IN ('QUEUED', 'RUNNING');

CREATE INDEX IF NOT EXISTS idx_history_jobs_claim
ON history_sync_jobs(status, next_attempt_at, created_at);

CREATE INDEX IF NOT EXISTS idx_history_jobs_heartbeat
ON history_sync_jobs(status, heartbeat_at);

CREATE INDEX IF NOT EXISTS idx_history_jobs_connection
ON history_sync_jobs(connection_id, created_at DESC);

-- Backfill existing rows with safe defaults
UPDATE history_sync_jobs
SET started_at = created_at
WHERE started_at IS NULL;

UPDATE history_sync_jobs
SET finished_at = updated_at
WHERE finished_at IS NULL AND status IN ('COMPLETED', 'FAILED', 'CANCELED');
