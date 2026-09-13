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
