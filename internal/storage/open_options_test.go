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
	_, _ = store.ConfigureConnection(ctx, "***1234")
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
	conn, err := roStore.Connection(ctx)
	if err != nil {
		t.Fatalf("read connection from ro store: %v", err)
	}
	if conn.AccountMasked != "***1234" {
		t.Errorf("expected ***1234, got %s", conn.AccountMasked)
	}

	// Writes should fail
	_, err = roStore.ConfigureConnection(ctx, "***9999")
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
