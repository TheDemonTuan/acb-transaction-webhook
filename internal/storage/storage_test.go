package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

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
	var version int
	if err := store.DB().QueryRowContext(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 11 {
		t.Fatalf("expected recovery migration version 11, got %d", version)
	}
	var columns int
	if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('recovery_runs') WHERE name IN ('reason','range_from','range_to','next_day')`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if columns != 4 {
		t.Fatalf("expected recovery plan columns, got %d", columns)
	}
}
