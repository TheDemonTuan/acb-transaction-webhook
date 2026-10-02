ALTER TABLE auth_recovery_episodes ADD COLUMN consent_action_id TEXT;
ALTER TABLE auth_recovery_episodes ADD COLUMN consent_expires_at TEXT;
ALTER TABLE auth_recovery_episodes ADD COLUMN consent_consumed_at TEXT;
ALTER TABLE auth_recovery_episodes ADD COLUMN credential_revision INTEGER NOT NULL DEFAULT 0;
ALTER TABLE telegram_auth_actions ADD COLUMN expected_config_revision INTEGER NOT NULL DEFAULT 0;
ALTER TABLE telegram_auth_actions ADD COLUMN attempt_id TEXT;
ALTER TABLE telegram_auth_actions ADD COLUMN browser_revision TEXT;
ALTER TABLE auth_challenges ADD COLUMN prompt_deleted_at TEXT;
CREATE INDEX auth_challenge_prompt_cleanup ON auth_challenges(created_at,id)
 WHERE prompt_message_id IS NOT NULL AND prompt_deleted_at IS NULL
 AND status NOT IN ('DELIVERING','PENDING','CONSUMING');

CREATE TABLE acb_credentials (
 connection_id TEXT PRIMARY KEY REFERENCES connections(id),
 revision INTEGER NOT NULL,
 envelope BLOB NOT NULL,
 key_id TEXT NOT NULL,
 updated_at TEXT NOT NULL
);
CREATE TABLE acb_credential_grants (
 token_hash TEXT PRIMARY KEY,
 connection_id TEXT NOT NULL,
 bot_id INTEGER NOT NULL,
 chat_id INTEGER NOT NULL,
 user_id INTEGER NOT NULL,
 expected_generation INTEGER NOT NULL,
 expected_config_revision INTEGER NOT NULL,
 credential_revision INTEGER NOT NULL,
 owner_subject TEXT,
 status TEXT NOT NULL CHECK(status IN ('PENDING','CONSUMED','REVOKED')),
 expires_at TEXT NOT NULL,
 created_at TEXT NOT NULL,
 consumed_at TEXT
);
CREATE UNIQUE INDEX one_pending_acb_credential_grant ON acb_credential_grants(connection_id) WHERE status='PENDING';
CREATE TABLE auth_recovery_ai_claims (
 attempt_id TEXT NOT NULL REFERENCES auth_attempts(id),
 browser_revision TEXT NOT NULL,
 created_at TEXT NOT NULL,
 PRIMARY KEY(attempt_id,browser_revision)
);
CREATE TABLE acb_logout_jobs (
 id TEXT PRIMARY KEY,
 connection_id TEXT NOT NULL,
 old_generation INTEGER NOT NULL,
 fenced_generation INTEGER NOT NULL,
 attempt_id TEXT,
 session_generation INTEGER,
 session_envelope BLOB,
 session_key_id TEXT,
 state TEXT NOT NULL CHECK(state IN ('PENDING','CLEARING','REVOKING','COMPLETED','LOCAL_ONLY')),
 reason_code TEXT NOT NULL DEFAULT '',
 status_message_id INTEGER,
 notice_status TEXT NOT NULL DEFAULT 'PENDING',
 created_at TEXT NOT NULL,
 updated_at TEXT NOT NULL,
 finished_at TEXT,
 local_cleared_at TEXT,
 bank_status TEXT NOT NULL DEFAULT 'PENDING' CHECK(bank_status IN ('PENDING','IN_FLIGHT','CONFIRMED','ALREADY_EXPIRED','UNCONFIRMED')),
 bank_reason_code TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX one_open_acb_logout_job ON acb_logout_jobs(connection_id) WHERE finished_at IS NULL;

-- Legacy callbacks and unfinished login attempts cannot confer button consent.
UPDATE telegram_auth_actions SET status='INVALIDATED' WHERE status IN ('DELIVERING','PENDING');
UPDATE auth_challenges SET status='INVALIDATED' WHERE status IN ('DELIVERING','PENDING','CONSUMING');
UPDATE connections SET state='AUTH_REQUIRED', generation=generation+1,
 updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now')
 WHERE state='AUTH_STARTING' AND EXISTS (
 SELECT 1 FROM auth_attempts a WHERE a.connection_id=connections.id
 AND a.generation=connections.generation AND a.status IN ('STARTING','IN_PROGRESS','EXPORTING','VERIFYING')
 );
UPDATE auth_attempts SET status='CANCELLED',finished_at=strftime('%Y-%m-%dT%H:%M:%fZ','now')
 WHERE status IN ('STARTING','IN_PROGRESS','EXPORTING','VERIFYING');
UPDATE auth_recovery_episodes SET generation=(SELECT generation FROM connections WHERE id=connection_id),
 state='WAIT_OPERATOR',reason_code='CONSENT_REQUIRED',next_attempt_at=NULL,
 updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now')
 WHERE finished_at IS NULL AND recovery_run_id IS NULL
 AND NOT EXISTS(SELECT 1 FROM auth_attempts a WHERE a.id=auth_recovery_episodes.attempt_id AND a.status='VERIFIED');
