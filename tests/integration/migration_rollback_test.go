package integration_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
	_ "modernc.org/sqlite"
)

// TestSQLiteMigrationRollbackOnFailure proves the transactional migration invariant:
// If a migration fails mid-way, SQLite rolls back the entire transaction, leaving
// schema version unchanged, no orphaned tables or columns, and data intact.
func TestSQLiteMigrationRollbackOnFailure(t *testing.T) {
	ctx := context.Background()
	dbDir := t.TempDir()
	dbPath := filepath.Join(dbDir, "gateway.db")

	// 1. Initialize store with canonical schema migrations
	store, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}

	// Insert baseline data
	conn, err := store.ConfigureConnection(ctx, "***7777")
	if err != nil {
		t.Fatalf("configure connection: %v", err)
	}
	_ = conn

	// Get initial schema version
	repBefore, err := store.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("schema version before: %v", err)
	}
	versionBefore := repBefore.Version
	countBefore := repBefore.AppliedCount

	store.Close()

	// 2. Re-open direct DB connection to simulate a failing migration step
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath)+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	defer db.Close()

	// Execute a simulated transactional migration with a deliberate error in statement 2
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}

	// Statement 1: valid table creation
	_, err = tx.ExecContext(ctx, `CREATE TABLE test_rollback_table (id TEXT PRIMARY KEY, value TEXT)`)
	if err != nil {
		t.Fatalf("statement 1 failed: %v", err)
	}

	// Statement 2: DELIBERATE syntax error / invalid constraint
	_, err = tx.ExecContext(ctx, `CREATE TABLE invalid syntax (this is not valid sql)`)
	if err == nil {
		tx.Rollback()
		t.Fatal("expected syntax error in statement 2")
	}

	// On error, the migration runner rolls back
	if err := tx.Rollback(); err != nil {
		t.Fatalf("tx rollback failed: %v", err)
	}

	// 3. Verify rollback: test_rollback_table MUST NOT exist
	var tableCount int
	err = db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='test_rollback_table'`).Scan(&tableCount)
	if err != nil {
		t.Fatalf("check table existence: %v", err)
	}
	if tableCount != 0 {
		t.Fatalf("table test_rollback_table exists despite transaction rollback: atomicity violated!")
	}

	// 4. Verify schema_migrations version was NOT bumped
	var versionAfter int
	var countAfter int
	err = db.QueryRowContext(ctx, `SELECT count(*), max(version) FROM schema_migrations`).Scan(&countAfter, &versionAfter)
	if err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	if countAfter != countBefore || versionAfter != versionBefore {
		t.Fatalf("schema_migrations changed on failed migration: before=(ver:%d, cnt:%d), after=(ver:%d, cnt:%d)",
			versionBefore, countBefore, versionAfter, countAfter)
	}

	// 5. Verify database integrity check
	var integrity string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("database integrity check failed: %v, result: %s", err, integrity)
	}

	// 6. Verify existing data preserved
	reopenedStore, err := storage.OpenRuntime(ctx, dbPath)
	if err != nil {
		t.Fatalf("reopen storage after rollback: %v", err)
	}
	defer reopenedStore.Close()

	connAfter, err := reopenedStore.Connection(ctx)
	if err != nil {
		t.Fatalf("fetch connection: %v", err)
	}
	if connAfter.AccountMasked != "***7777" {
		t.Fatalf("connection corrupted after rollback: got %s", connAfter.AccountMasked)
	}
}
