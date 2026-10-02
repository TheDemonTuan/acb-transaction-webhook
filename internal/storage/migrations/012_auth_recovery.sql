CREATE TABLE auth_recovery_episodes (
 id TEXT PRIMARY KEY,
 connection_id TEXT NOT NULL REFERENCES connections(id),
 trigger_generation INTEGER NOT NULL,
 generation INTEGER NOT NULL,
 config_revision INTEGER NOT NULL,
 attempt_id TEXT,
 state TEXT NOT NULL CHECK(state IN ('DETECTED','STARTING','LOGIN','WAITING_CAPTCHA','WAITING_OTP','VERIFYING','CATCHING_UP','RETRY_WAIT','WAIT_OPERATOR','MAINTENANCE_WAIT','MANUAL_REQUIRED','COMPLETED','SUPERSEDED','CANCELLED')),
 attempt_count INTEGER NOT NULL DEFAULT 0,
 budget_start_count INTEGER NOT NULL DEFAULT 0,
 captcha_submissions INTEGER NOT NULL DEFAULT 0,
 ai_used INTEGER NOT NULL DEFAULT 0,
 otp_submissions INTEGER NOT NULL DEFAULT 0,
 next_attempt_at TEXT,
 last_login_at TEXT,
 required_from TEXT,
 required_to TEXT,
 reason_code TEXT NOT NULL DEFAULT '',
 recovery_run_id TEXT,
 status_message_id INTEGER,
 created_at TEXT NOT NULL,
 updated_at TEXT NOT NULL,
 finished_at TEXT,
 UNIQUE(connection_id,trigger_generation),
 CHECK ((state IN ('COMPLETED','SUPERSEDED','CANCELLED')) = (finished_at IS NOT NULL))
);
CREATE UNIQUE INDEX one_open_auth_recovery ON auth_recovery_episodes(connection_id) WHERE finished_at IS NULL;
CREATE TABLE auth_challenges (
 id TEXT PRIMARY KEY,
 episode_id TEXT NOT NULL REFERENCES auth_recovery_episodes(id),
 connection_id TEXT NOT NULL,
 generation INTEGER NOT NULL,
 attempt_id TEXT NOT NULL,
 browser_revision TEXT NOT NULL,
 kind TEXT NOT NULL CHECK(kind IN ('CAPTCHA_TEXT','OTP')),
 status TEXT NOT NULL CHECK(status IN ('DELIVERING','PENDING','CONSUMING','CONSUMED','EXPIRED','CANCELLED','INVALIDATED')),
 chat_id INTEGER NOT NULL,
 prompt_message_id INTEGER,
 expires_at TEXT NOT NULL,
 created_at TEXT NOT NULL,
 consumed_at TEXT
);
CREATE UNIQUE INDEX one_open_auth_challenge ON auth_challenges(attempt_id) WHERE status IN ('DELIVERING','PENDING','CONSUMING');
CREATE INDEX auth_challenge_prompt ON auth_challenges(chat_id,prompt_message_id);
CREATE TABLE telegram_auth_state (
 bot_id INTEGER PRIMARY KEY,
 next_update_id INTEGER NOT NULL DEFAULT 0,
 paused INTEGER NOT NULL DEFAULT 0 CHECK(paused IN (0,1)),
 updated_at TEXT NOT NULL
);
CREATE TABLE telegram_auth_actions (
 id TEXT PRIMARY KEY,
 bot_id INTEGER NOT NULL,
 chat_id INTEGER NOT NULL,
 user_id INTEGER NOT NULL,
 message_id INTEGER,
 episode_id TEXT,
 expected_generation INTEGER NOT NULL,
 action TEXT NOT NULL,
 status TEXT NOT NULL,
 expires_at TEXT NOT NULL,
 created_at TEXT NOT NULL
);
CREATE TABLE auth_recovery_notices (
 id TEXT PRIMARY KEY,
 episode_id TEXT NOT NULL,
 event_key TEXT NOT NULL UNIQUE,
 kind TEXT NOT NULL,
 status TEXT NOT NULL,
 message_id INTEGER,
 next_attempt_at TEXT,
 created_at TEXT NOT NULL,
 sent_at TEXT
);
