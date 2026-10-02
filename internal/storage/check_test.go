package storage

import (
	"context"
	"path/filepath"
	"testing"
)

func TestCheckIntegrity(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test_check.db")
	store, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	rep, err := store.CheckIntegrity(ctx)
	if err != nil {
		t.Fatalf("check integrity failed: %v", err)
	}
	if !rep.IntegrityOK {
		t.Errorf("expected integrity ok, got %v: %s", rep.IntegrityOK, rep.IntegrityMessage)
	}
	if rep.MigrationsApplied == 0 {
		t.Errorf("expected migrations applied > 0, got %d", rep.MigrationsApplied)
	}
}

func TestSchemaVersion(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// 1. Unmigrated database
	unmigratedPath := filepath.Join(dir, "unmigrated.db")
	unmigratedStore, err := OpenRuntime(ctx, unmigratedPath)
	if err != nil {
		t.Fatalf("open unmigrated store: %v", err)
	}
	defer unmigratedStore.Close()

	repUnmigrated, err := unmigratedStore.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("schema version unmigrated failed: %v", err)
	}
	if repUnmigrated.Version != 0 || repUnmigrated.AppliedCount != 0 {
		t.Errorf("expected version 0 and appliedCount 0, got %+v", repUnmigrated)
	}

	// 2. Migrated database
	migratedPath := filepath.Join(dir, "migrated.db")
	store, err := Open(ctx, migratedPath)
	if err != nil {
		t.Fatalf("open migrated store: %v", err)
	}
	defer store.Close()

	rep, err := store.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("schema version failed: %v", err)
	}
	var version, count int
	var checksum string
	if err := store.DB().QueryRowContext(ctx, `SELECT version,checksum,(SELECT count(*) FROM schema_migrations) FROM schema_migrations ORDER BY version DESC LIMIT 1`).Scan(&version, &checksum, &count); err != nil {
		t.Fatal(err)
	}
	if rep.Version != version || rep.AppliedCount != count || rep.Checksum != checksum {
		t.Errorf("schema report does not match durable migration ledger: %+v", rep)
	}
}
