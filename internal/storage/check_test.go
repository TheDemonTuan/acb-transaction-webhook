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
	if rep.Version != 7 {
		t.Errorf("expected schema version 7, got %d", rep.Version)
	}
	if rep.AppliedCount != 7 {
		t.Errorf("expected 7 migrations applied, got %d", rep.AppliedCount)
	}
	if rep.Checksum != "2026-09-13-v6-notification-providers" {
		t.Errorf("expected checksum '2026-09-13-v6-notification-providers', got %q", rep.Checksum)
	}
}
