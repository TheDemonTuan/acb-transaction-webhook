package storage_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func TestOpenRuntime_DoesNotAutoMigrate(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test_runtime.db")

	// First, migrate explicitly
	store, err := storage.OpenWithOptions(ctx, dbPath, storage.OpenOptions{RunMigrations: true})
	if err != nil {
		t.Fatalf("migrate failed: %v", err)
	}
	_ = store.Close()

	// Second, open with OpenRuntime (RunMigrations: false)
	runtimeStore, err := storage.OpenRuntime(ctx, dbPath)
	if err != nil {
		t.Fatalf("open runtime failed: %v", err)
	}
	defer runtimeStore.Close()

	if err := runtimeStore.Health(ctx); err != nil {
		t.Fatalf("health check failed: %v", err)
	}
}

func TestOpenReadOnly_RejectsMigrationsAndAllowsQueries(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test_ro.db")

	// Initialize and migrate
	store, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("init store: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO connections(id,bank_code,state,account_masked,generation,created_at,updated_at) VALUES('historical-acb','ACB','PAUSED','***1234',1,'2026-09-12T00:00:00Z','2026-09-12T00:00:00Z')`); err != nil {
		t.Fatalf("seed historical connection: %v", err)
	}
	_ = store.Close()

	// Open read-only
	roStore, err := storage.OpenWithOptions(ctx, dbPath, storage.OpenOptions{
		RunMigrations: false,
		ReadOnly:      true,
	})
	if err != nil {
		t.Fatalf("open read-only store: %v", err)
	}
	defer roStore.Close()

	// Queries should succeed
	var masked string
	if err := roStore.DB().QueryRowContext(ctx, `SELECT account_masked FROM connections WHERE id='historical-acb'`).Scan(&masked); err != nil {
		t.Fatalf("read connection from ro store: %v", err)
	}
	if masked != "***1234" {
		t.Errorf("expected ***1234, got %s", masked)
	}

	// Writes should fail
	_, err = roStore.CreateEndpoint(ctx, "Read-only write", "https://example.com/hook")
	if err == nil {
		t.Fatal("expected write to fail on read-only store, got nil")
	}

	// Attempting to run migrations with ReadOnly: true must fail
	_, err = storage.OpenWithOptions(ctx, dbPath, storage.OpenOptions{
		RunMigrations: true,
		ReadOnly:      true,
	})
	if err == nil {
		t.Fatal("expected migration on read-only store to fail, got nil")
	}
}
