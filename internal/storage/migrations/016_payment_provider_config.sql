-- Credentials are a master-key envelope; neither API keys nor channel IDs are plaintext.
CREATE TABLE payment_provider_config (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    revision INTEGER NOT NULL DEFAULT 0,
    credentials_envelope TEXT NOT NULL DEFAULT '',
    enabled INTEGER NOT NULL DEFAULT 0 CHECK (enabled IN (0,1)),
    webhook_confirmed INTEGER NOT NULL DEFAULT 0 CHECK (webhook_confirmed IN (0,1)),
    updated_at TEXT NOT NULL
);
INSERT INTO payment_provider_config(id, updated_at) VALUES (1, strftime('%Y-%m-%dT%H:%M:%fZ','now'));
-- Retain old same-channel signatures for delayed callbacks after key rotation.
CREATE TABLE payment_provider_credential_versions (
    revision INTEGER PRIMARY KEY,
    credentials_envelope TEXT NOT NULL
);
CREATE TABLE payment_provider_runtime (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    quiesced INTEGER NOT NULL DEFAULT 0 CHECK (quiesced IN (0,1)),
    next_request_at TEXT NOT NULL DEFAULT ''
);
INSERT INTO payment_provider_runtime(id) VALUES (1);
CREATE TABLE payment_provider_operations (
    token TEXT PRIMARY KEY,
    revision INTEGER NOT NULL,
    lease_until TEXT NOT NULL
);
CREATE INDEX idx_payment_provider_operations_lease ON payment_provider_operations(lease_until);
