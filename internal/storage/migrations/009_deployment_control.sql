-- Migration 009: Durable deployment control and mutation gate table
CREATE TABLE IF NOT EXISTS deployment_control (
    id TEXT PRIMARY KEY,
    gate_state TEXT NOT NULL DEFAULT 'OPEN',
    owner TEXT,
    lease_token TEXT,
    lease_expires_at TEXT,
    reason TEXT,
    fence_generation INTEGER NOT NULL DEFAULT 1,
    metadata_json TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_deployment_control_state
ON deployment_control(gate_state, lease_expires_at);

-- Initialize singleton control row if absent
INSERT OR IGNORE INTO deployment_control (
    id, gate_state, owner, lease_token, lease_expires_at, reason, fence_generation, metadata_json, created_at, updated_at
) VALUES (
    'singleton', 'OPEN', NULL, NULL, NULL, 'bootstrap', 1, '{}', datetime('now'), datetime('now')
);
