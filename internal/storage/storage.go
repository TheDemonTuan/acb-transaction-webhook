package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/security"
	migrationfiles "github.com/thedemontuan/acb-transaction-webhook/internal/storage/migrations"
	_ "modernc.org/sqlite"
)

type Store struct {
	db      *sql.DB
	keyring *security.Keyring
	writeMu sync.Mutex
}

func (s *Store) WithKeyring(k *security.Keyring) *Store {
	s.keyring = k
	return s
}

func Open(ctx context.Context, path string) (*Store, error) {
	return openInternal(ctx, path, OpenOptions{RunMigrations: true})
}

func openInternal(ctx context.Context, path string, opts OpenOptions) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(0)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db}
	if opts.RunMigrations {
		if err := s.Migrate(ctx); err != nil {
			db.Close()
			return nil, err
		}
	}
	return s, nil
}
func (s *Store) Close() error { return s.db.Close() }
func (s *Store) DB() *sql.DB  { return s.db }
func (s *Store) Migrate(ctx context.Context) error {
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, checksum TEXT NOT NULL, applied_at TEXT NOT NULL)`); err != nil {
			return err
		}
		for _, m := range migrations {
			var checksum string
			err := tx.QueryRowContext(ctx, `SELECT checksum FROM schema_migrations WHERE version = ?`, m.version).Scan(&checksum)
			if errors.Is(err, sql.ErrNoRows) {
				if _, err := tx.ExecContext(ctx, m.sql); err != nil {
					return fmt.Errorf("migration %d: %w", m.version, err)
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, checksum, applied_at) VALUES(?,?,?)`, m.version, m.checksum, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
					return err
				}
			} else if err != nil {
				return err
			} else if checksum != m.checksum {
				return fmt.Errorf("migration %d checksum mismatch", m.version)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	_, _ = s.BackfillCanonicalDates(ctx)
	return nil
}
func (s *Store) withTx(ctx context.Context, fn func(*sql.Tx) error) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return err
	}
	if err = fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
func (s *Store) Health(ctx context.Context) error { return s.db.PingContext(ctx) }

type migration struct {
	version       int
	checksum, sql string
}

var migrations = []migration{{1, "2026-09-10-v2-foundation", `
CREATE TABLE IF NOT EXISTS connections (id TEXT PRIMARY KEY, bank_code TEXT NOT NULL DEFAULT 'ACB', account_identity_hmac TEXT UNIQUE, account_envelope BLOB, account_masked TEXT, state TEXT NOT NULL DEFAULT 'UNCONFIGURED', generation INTEGER NOT NULL DEFAULT 0, config_revision INTEGER NOT NULL DEFAULT 0, started_at TEXT, created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS sessions (connection_id TEXT PRIMARY KEY REFERENCES connections(id), generation INTEGER NOT NULL, envelope BLOB NOT NULL, key_id TEXT NOT NULL, verified_at TEXT, expires_at TEXT, updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS auth_attempts (id TEXT PRIMARY KEY, connection_id TEXT NOT NULL REFERENCES connections(id), generation INTEGER NOT NULL, owner_subject TEXT, status TEXT NOT NULL, expires_at TEXT NOT NULL, created_at TEXT NOT NULL, finished_at TEXT);
CREATE UNIQUE INDEX IF NOT EXISTS one_active_auth_attempt ON auth_attempts(connection_id) WHERE status IN ('STARTING','IN_PROGRESS','EXPORTING','VERIFYING');
CREATE TABLE IF NOT EXISTS poll_runs (id TEXT PRIMARY KEY, connection_id TEXT NOT NULL REFERENCES connections(id), generation INTEGER NOT NULL, status TEXT NOT NULL, classifier TEXT, http_status INTEGER, pages INTEGER NOT NULL DEFAULT 0, rows_seen INTEGER NOT NULL DEFAULT 0, sanitized_error TEXT, started_at TEXT NOT NULL, finished_at TEXT);
CREATE TABLE IF NOT EXISTS checkpoints (connection_id TEXT PRIMARY KEY REFERENCES connections(id), scan_id TEXT, coverage_from TEXT, coverage_to TEXT, updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS coverage_gaps (id TEXT PRIMARY KEY, connection_id TEXT NOT NULL REFERENCES connections(id), coverage_from TEXT NOT NULL, coverage_to TEXT NOT NULL, reason TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'OPEN', created_at TEXT NOT NULL, resolved_at TEXT);
CREATE TABLE IF NOT EXISTS transactions (id TEXT PRIMARY KEY, connection_id TEXT NOT NULL REFERENCES connections(id), semantic_key TEXT NOT NULL, canonical_hash TEXT NOT NULL, transaction_date TEXT NOT NULL, effective_date TEXT NOT NULL, debit INTEGER NOT NULL DEFAULT 0, credit INTEGER NOT NULL DEFAULT 0, balance INTEGER, description_envelope BLOB, parser_version TEXT NOT NULL, baseline_state TEXT NOT NULL DEFAULT 'NONE', first_seen_at TEXT NOT NULL, UNIQUE(connection_id,semantic_key));
CREATE TABLE IF NOT EXISTS transaction_quarantine (id TEXT PRIMARY KEY, connection_id TEXT NOT NULL REFERENCES connections(id), semantic_key TEXT NOT NULL, existing_hash TEXT, candidate_envelope BLOB NOT NULL, reason TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'OPEN', created_at TEXT NOT NULL, resolved_at TEXT);
CREATE TABLE IF NOT EXISTS dedupe_keys (semantic_key TEXT PRIMARY KEY, canonical_hash TEXT NOT NULL, event_id TEXT, created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS events (id TEXT PRIMARY KEY, transaction_id TEXT REFERENCES transactions(id), event_type TEXT NOT NULL, payload BLOB NOT NULL, payload_hash TEXT NOT NULL, created_at TEXT NOT NULL, UNIQUE(transaction_id,event_type));
CREATE TABLE IF NOT EXISTS webhook_endpoints (id TEXT PRIMARY KEY, name TEXT NOT NULL, status TEXT NOT NULL, current_revision INTEGER NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS endpoint_versions (endpoint_id TEXT NOT NULL REFERENCES webhook_endpoints(id), revision INTEGER NOT NULL, url TEXT NOT NULL, filters_json TEXT NOT NULL, created_at TEXT NOT NULL, PRIMARY KEY(endpoint_id,revision));
CREATE TABLE IF NOT EXISTS endpoint_secrets (endpoint_id TEXT NOT NULL REFERENCES webhook_endpoints(id), key_id TEXT NOT NULL, envelope BLOB NOT NULL, status TEXT NOT NULL, created_at TEXT NOT NULL, retired_at TEXT, PRIMARY KEY(endpoint_id,key_id));
CREATE TABLE IF NOT EXISTS deliveries (id TEXT PRIMARY KEY, event_id TEXT NOT NULL REFERENCES events(id), endpoint_id TEXT NOT NULL, endpoint_revision INTEGER NOT NULL, key_id TEXT NOT NULL, status TEXT NOT NULL, attempts INTEGER NOT NULL DEFAULT 0, next_attempt_at TEXT NOT NULL, claim_token TEXT, lease_until TEXT, created_at TEXT NOT NULL, updated_at TEXT NOT NULL, UNIQUE(event_id,endpoint_id));
CREATE TABLE IF NOT EXISTS delivery_attempts (id TEXT PRIMARY KEY, delivery_id TEXT NOT NULL REFERENCES deliveries(id), attempt_number INTEGER NOT NULL, status_code INTEGER, latency_ms INTEGER, outcome TEXT NOT NULL, sanitized_error TEXT, created_at TEXT NOT NULL, UNIQUE(delivery_id,attempt_number));
CREATE TABLE IF NOT EXISTS system_events (id TEXT PRIMARY KEY, incident_key TEXT NOT NULL, event_type TEXT NOT NULL, state TEXT NOT NULL, details_json TEXT NOT NULL, created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS audit_logs (id TEXT PRIMARY KEY, actor_subject TEXT, actor_role TEXT, action TEXT NOT NULL, target TEXT NOT NULL, request_id TEXT NOT NULL, details_json TEXT NOT NULL, created_at TEXT NOT NULL);
`}, {2, "2026-09-10-v2-hot-indexes", `
CREATE INDEX IF NOT EXISTS idx_transactions_connection_date ON transactions(connection_id, transaction_date DESC);
CREATE INDEX IF NOT EXISTS idx_deliveries_due ON deliveries(status, next_attempt_at);
CREATE INDEX IF NOT EXISTS idx_poll_runs_connection_started ON poll_runs(connection_id, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_created ON audit_logs(created_at DESC);
`}, {3, "2026-09-11-pagination-indexes", `
CREATE INDEX IF NOT EXISTS idx_transactions_page ON transactions(first_seen_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_deliveries_page ON deliveries(created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_poll_runs_page ON poll_runs(started_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_audit_logs_page ON audit_logs(created_at DESC, id DESC);
`}, {4, "2026-09-12-event-journal-and-leasing", `
CREATE TABLE IF NOT EXISTS event_journal (
    seq INTEGER PRIMARY KEY AUTOINCREMENT,
    epoch TEXT NOT NULL DEFAULT 'ep1',
    event_type TEXT NOT NULL,
    aggregate_id TEXT NOT NULL,
    payload_json TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_event_journal_seq ON event_journal(seq);
CREATE INDEX IF NOT EXISTS idx_event_journal_epoch_seq ON event_journal(epoch, seq);
CREATE INDEX IF NOT EXISTS idx_event_journal_created ON event_journal(created_at);
CREATE INDEX IF NOT EXISTS idx_deliveries_claim_order ON deliveries(status, next_attempt_at, id);
CREATE INDEX IF NOT EXISTS idx_deliveries_claim_lease ON deliveries(endpoint_id, status, next_attempt_at, lease_until);
ALTER TABLE endpoint_secrets ADD COLUMN encoding_version TEXT NOT NULL DEFAULT 'legacy-hex';
`}, {5, "2026-09-12-v3-filter-schedule-qr", `
ALTER TABLE transactions ADD COLUMN transaction_at_iso TEXT;
ALTER TABLE transactions ADD COLUMN transaction_day TEXT;
ALTER TABLE transactions ADD COLUMN date_precision TEXT NOT NULL DEFAULT 'datetime';
ALTER TABLE transactions ADD COLUMN ingest_source TEXT NOT NULL DEFAULT 'REALTIME';
CREATE INDEX IF NOT EXISTS idx_transactions_day_id ON transactions(connection_id, transaction_day, id DESC);
CREATE INDEX IF NOT EXISTS idx_transactions_first_seen ON transactions(connection_id, first_seen_at DESC, id DESC);

CREATE TABLE IF NOT EXISTS monitor_settings (
    id TEXT PRIMARY KEY,
    revision INTEGER NOT NULL DEFAULT 1,
    enabled INTEGER NOT NULL DEFAULT 1,
    timezone TEXT NOT NULL DEFAULT 'Asia/Ho_Chi_Minh',
    default_profile_json TEXT NOT NULL,
    windows_json TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS history_coverage (
    id TEXT PRIMARY KEY,
    connection_id TEXT NOT NULL REFERENCES connections(id),
    day TEXT NOT NULL,
    status TEXT NOT NULL,
    last_sync_at TEXT NOT NULL,
    rows_seen INTEGER NOT NULL DEFAULT 0,
    error_message TEXT,
    UNIQUE(connection_id, day)
);

CREATE TABLE IF NOT EXISTS history_sync_jobs (
    id TEXT PRIMARY KEY,
    connection_id TEXT NOT NULL REFERENCES connections(id),
    range_from TEXT NOT NULL,
    range_to TEXT NOT NULL,
    status TEXT NOT NULL,
    rows_seen INTEGER NOT NULL DEFAULT 0,
    error_message TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS payment_qr_settings (
    id TEXT PRIMARY KEY,
    connection_id TEXT NOT NULL REFERENCES connections(id),
    account_number TEXT NOT NULL,
    account_name TEXT NOT NULL,
    bin TEXT NOT NULL DEFAULT '970416',
    bank_name TEXT NOT NULL DEFAULT 'ACB',
    image_path TEXT,
    image_hash TEXT,
    image_content_type TEXT,
    provider TEXT NOT NULL DEFAULT 'UPLOAD',
    revision INTEGER NOT NULL DEFAULT 1,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(connection_id)
);
`}, {6, "2026-09-12-v5-voice-settings", `
CREATE TABLE IF NOT EXISTS voice_settings (
    id TEXT PRIMARY KEY,
    revision INTEGER NOT NULL DEFAULT 1,
    provider_mode TEXT NOT NULL DEFAULT 'ONLINE_AUTO',
    edge_voice TEXT NOT NULL DEFAULT 'vi-VN-HoaiMyNeural',
    online_fallback INTEGER NOT NULL DEFAULT 1,
    updated_at TEXT NOT NULL
);
`}, {7, "2026-09-13-v6-notification-providers", `
ALTER TABLE webhook_endpoints ADD COLUMN provider TEXT NOT NULL DEFAULT 'WEBHOOK';
ALTER TABLE endpoint_versions ADD COLUMN provider_config_json TEXT NOT NULL DEFAULT '{}';
ALTER TABLE endpoint_secrets ADD COLUMN secret_kind TEXT NOT NULL DEFAULT 'WEBHOOK_HMAC';
ALTER TABLE delivery_attempts ADD COLUMN provider TEXT NOT NULL DEFAULT 'WEBHOOK';
ALTER TABLE delivery_attempts ADD COLUMN provider_error_code TEXT;
ALTER TABLE deliveries ADD COLUMN retry_cycle_start_attempt INTEGER NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS idx_webhook_endpoints_provider_status ON webhook_endpoints(provider, status);
`}, {8, "2026-09-14-v8-history-job-queue", migrationfiles.HistoryJobQueueSQL},
	{9, "2026-09-14-v9-deployment-control", migrationfiles.DeploymentControlSQL}}
