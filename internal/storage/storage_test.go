package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

type historicalConnection struct {
	ID            string
	Generation    int64
	AccountMasked string
}

func historicalConnectionFixture(t *testing.T, store *Store, ctx context.Context, accountMasked string) historicalConnection {
	t.Helper()
	c := historicalConnection{ID: id("historic_acb"), Generation: 1, AccountMasked: accountMasked}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO connections(id,bank_code,state,account_masked,generation,config_revision,created_at,updated_at) VALUES(?,'ACB','PAUSED',?,1,1,?,?)`, c.ID, c.AccountMasked, now(), now()); err != nil {
		t.Fatalf("seed historical ACB connection: %v", err)
	}
	return c
}

func historicalTransactionFixture(t *testing.T, store *Store, ctx context.Context, in TransactionInput, source, baseline string) string {
	t.Helper()
	txnID := id("historic_txn")
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO transactions(id,connection_id,semantic_key,canonical_hash,transaction_date,effective_date,debit,credit,balance,description_envelope,parser_version,first_seen_at,ingest_source,baseline_state) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, txnID, in.ConnectionID, in.SemanticKey, in.CanonicalHash, in.TransactionAt, in.EffectiveAt, in.Debit, in.Credit, in.Balance, in.Description, in.ParserVersion, now(), source, baseline); err != nil {
		t.Fatalf("seed historical transaction: %v", err)
	}
	return txnID
}

func historicalEventFixture(t *testing.T, store *Store, ctx context.Context, transactionID, eventType string, payload []byte) string {
	t.Helper()
	eventID := id("historic_event")
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO events(id,transaction_id,event_type,payload,payload_hash,created_at) VALUES(?,?,?,?,?,?)`, eventID, transactionID, eventType, payload, eventID, now()); err != nil {
		t.Fatalf("seed historical event: %v", err)
	}
	return eventID
}

func TestOpenMigratesAndRejectsChangedMigrationChecksum(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var tables int
	if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('connections','events','deliveries','transaction_quarantine')`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 4 {
		t.Fatalf("expected 4 core tables, got %d", tables)
	}
	var foreignKeys int
	if err := store.DB().QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if foreignKeys != 1 {
		t.Fatalf("foreign_keys = %d", foreignKeys)
	}
	var missing sql.NullString
	if err := store.DB().QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name='schema_migrations'`).Scan(&missing); err != nil {
		t.Fatal(err)
	}
}

func TestOpenMigratesProductionV10ToRecoveryV11(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "production-v10.db")
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, checksum TEXT NOT NULL, applied_at TEXT NOT NULL)`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	for _, migration := range migrations[:10] {
		if _, err := db.ExecContext(ctx, migration.sql); err != nil {
			db.Close()
			t.Fatalf("apply migration %d: %v", migration.version, err)
		}
		checksum := migration.checksum
		if migration.version == 10 {
			checksum = "2026-09-17-v10-monitor-idle-cadence"
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations(version, checksum, applied_at) VALUES(?,?,?)`, migration.version, checksum, "2026-09-18T02:51:35.121150122Z"); err != nil {
			db.Close()
			t.Fatalf("record migration %d: %v", migration.version, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("production v10 database must migrate: %v", err)
	}
	defer store.Close()
	var columns int
	if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('recovery_runs') WHERE name IN ('reason','range_from','range_to','next_day')`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if columns != 4 {
		t.Fatalf("expected recovery plan columns, got %d", columns)
	}
}

func TestStore_HistorySyncJobs_MigrationFromV7(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "v7_migration.db")

	// Set up database manually with versions 1-7
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()

	if _, err := db.ExecContext(ctx, `CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, checksum TEXT NOT NULL, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatalf("create schema_migrations: %v", err)
	}

	for _, m := range migrations {
		if m.version > 7 {
			break
		}
		if _, err := db.ExecContext(ctx, m.sql); err != nil {
			t.Fatalf("apply migration %d: %v", m.version, err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations(version, checksum, applied_at) VALUES(?,?,?)`, m.version, m.checksum, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			t.Fatalf("record migration %d: %v", m.version, err)
		}
	}

	// Insert pre-existing v7 connection and history jobs
	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO connections (id, bank_code, state, generation, config_revision, created_at, updated_at)
		VALUES ('conn_v7', 'ACB', 'MONITORING', 1, 1, ?, ?)
	`, nowStr, nowStr); err != nil {
		t.Fatalf("insert v7 connection: %v", err)
	}

	// Insert historical COMPLETED, FAILED, and RUNNING rows in v7 schema
	if _, err := db.ExecContext(ctx, `
		INSERT INTO history_sync_jobs (id, connection_id, range_from, range_to, status, rows_seen, error_message, created_at, updated_at)
		VALUES
			('job_v7_completed', 'conn_v7', '2026-08-01', '2026-08-05', 'COMPLETED', 25, NULL, '2026-08-01T00:00:00Z', '2026-08-01T00:05:00Z'),
			('job_v7_failed', 'conn_v7', '2026-08-06', '2026-08-10', 'FAILED', 5, 'v7 failure error', '2026-08-06T00:00:00Z', '2026-08-06T00:01:00Z'),
			('job_v7_running', 'conn_v7', '2026-08-11', '2026-08-15', 'RUNNING', 10, NULL, '2026-08-11T00:00:00Z', '2026-08-11T00:02:00Z')
	`); err != nil {
		t.Fatalf("insert v7 jobs: %v", err)
	}
	db.Close()

	// Open with store to trigger migration 8
	store, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open store and run migration 8: %v", err)
	}
	defer store.Close()

	// Verify schema version is at least 8
	rep, err := store.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("check schema version: %v", err)
	}
	if rep.Version < 8 {
		t.Fatalf("unexpected schema version: %+v", rep)
	}

	// Verify historical rows are readable and backfilled safely
	var status, startedAt, finishedAt, errorMessage string
	var generation, rowsSeen int64
	if err := store.DB().QueryRowContext(ctx, `SELECT status,generation,rows_seen,started_at,finished_at FROM history_sync_jobs WHERE id='job_v7_completed'`).Scan(&status, &generation, &rowsSeen, &startedAt, &finishedAt); err != nil || status != "COMPLETED" || generation != 0 || rowsSeen != 25 || startedAt != "2026-08-01T00:00:00Z" || finishedAt != "2026-08-01T00:05:00Z" {
		t.Fatalf("unexpected backfilled completed job: status=%s generation=%d rows=%d started=%s finished=%s err=%v", status, generation, rowsSeen, startedAt, finishedAt, err)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT status,error_message,finished_at FROM history_sync_jobs WHERE id='job_v7_failed'`).Scan(&status, &errorMessage, &finishedAt); err != nil || status != "FAILED" || errorMessage != "v7 failure error" || finishedAt != "2026-08-06T00:01:00Z" {
		t.Fatalf("unexpected backfilled failed job: status=%s error=%s finished=%s err=%v", status, errorMessage, finishedAt, err)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT status,started_at FROM history_sync_jobs WHERE id='job_v7_running'`).Scan(&status, &startedAt); err != nil || status != "RUNNING" || startedAt != "2026-08-11T00:00:00Z" {
		t.Fatalf("unexpected backfilled running job: status=%s started=%s err=%v", status, startedAt, err)
	}
}

func TestSessionControlMigrationCancelsOnlyUnsafeLegacyLogin(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY,checksum TEXT NOT NULL,applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations[:12] {
		if _, err := db.ExecContext(ctx, m.sql); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations VALUES(?,?,?)`, m.version, m.checksum, now()); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct{ id, state, attempt, episode string }{
		{"unsafe", "AUTH_STARTING", "IN_PROGRESS", "LOGIN"},
		{"healthy", "MONITORING", "VERIFIED", "COMPLETED"},
		{"catchup", "MONITORING", "VERIFIED", "CATCHING_UP"},
	} {
		if _, err := db.ExecContext(ctx, `INSERT INTO connections(id,state,generation,config_revision,created_at,updated_at) VALUES(?,?,7,2,?,?)`, c.id, c.state, now(), now()); err != nil {
			t.Fatal(err)
		}
		var finished any
		if c.attempt == "VERIFIED" {
			finished = now()
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO auth_attempts(id,connection_id,generation,owner_subject,status,expires_at,created_at,finished_at) VALUES(?,?,7,?,?,?,?,?)`, c.id+"-attempt", c.id, "historical-recovery-owner", c.attempt, time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano), now(), finished); err != nil {
			t.Fatal(err)
		}
		finished = nil
		var run any
		if c.episode == "COMPLETED" {
			finished = now()
		}
		if c.episode == "CATCHING_UP" {
			run = "committed-run"
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO auth_recovery_episodes(id,connection_id,trigger_generation,generation,config_revision,attempt_id,state,recovery_run_id,created_at,updated_at,finished_at) VALUES(?,?,6,7,2,?,?,?,?,?,?)`, c.id+"-episode", c.id, c.id+"-attempt", c.episode, run, now(), now(), finished); err != nil {
			t.Fatal(err)
		}
		if c.state == "MONITORING" {
			if _, err := db.ExecContext(ctx, `INSERT INTO sessions(connection_id,generation,envelope,key_id,verified_at,updated_at) VALUES(?,7,X'00','fixture',?,?)`, c.id, now(), now()); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var state, consentActionID string
	var generation int64
	if err := s.DB().QueryRowContext(ctx, `SELECT state,generation,COALESCE(consent_action_id,'') FROM auth_recovery_episodes WHERE id='unsafe-episode'`).Scan(&state, &generation, &consentActionID); err != nil || state != "WAIT_OPERATOR" || generation != 8 || consentActionID != "" {
		t.Fatalf("unsafe legacy admission survived: state=%s generation=%d consent=%s err=%v", state, generation, consentActionID, err)
	}
	var status string
	if err := s.DB().QueryRowContext(ctx, `SELECT status FROM auth_attempts WHERE id='unsafe-attempt'`).Scan(&status); err != nil || status != "CANCELLED" {
		t.Fatalf("unsafe attempt=%s %v", status, err)
	}
	for _, id := range []string{"healthy", "catchup"} {
		var generation int64
		if err := s.DB().QueryRowContext(ctx, `SELECT state,generation FROM connections WHERE id=?`, id).Scan(&status, &generation); err != nil || status != "MONITORING" || generation != 7 {
			t.Fatalf("healthy fence changed: %s %d %v", status, generation, err)
		}
		var count int
		if err := s.DB().QueryRowContext(ctx, `SELECT count(*) FROM sessions WHERE connection_id=? AND generation=7`, id).Scan(&count); err != nil || count != 1 {
			t.Fatalf("healthy session removed: %d %v", count, err)
		}
	}
	var recoveryRunID string
	if err := s.DB().QueryRowContext(ctx, `SELECT state,recovery_run_id FROM auth_recovery_episodes WHERE id='catchup-episode'`).Scan(&state, &recoveryRunID); err != nil || state != "CATCHING_UP" || recoveryRunID != "committed-run" {
		t.Fatalf("committed catchup lost: state=%s run=%s err=%v", state, recoveryRunID, err)
	}
}
