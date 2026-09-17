package storage_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func TestMigration010UpdatesDefaultLegacyCadence(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test_m10.db")

	store, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("storage.Open failed: %v", err)
	}
	defer store.Close()

	settings, err := store.GetMonitorSettings(ctx)
	if err != nil {
		t.Fatalf("GetMonitorSettings failed: %v", err)
	}

	if len(settings.Windows) == 0 {
		t.Fatalf("expected at least 1 window, got 0")
	}

	w0 := settings.Windows[0]
	if w0.Profile.MinSeconds != 20 || w0.Profile.MaxSeconds != 30 {
		t.Errorf("expected migrated window cadence 20-30s, got %d-%ds", w0.Profile.MinSeconds, w0.Profile.MaxSeconds)
	}

	// Schema version must reflect migration 10
	rep, err := store.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion failed: %v", err)
	}
	if rep.Version != 10 || rep.AppliedCount != 10 {
		t.Errorf("expected version 10 / count 10, got %d / %d", rep.Version, rep.AppliedCount)
	}
}

func TestMigration010PreservesCustomizedOperatorProfile(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test_custom.db")

	store, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("storage.Open failed: %v", err)
	}
	defer store.Close()

	// Simulate operator who previously customized window to 7-14s
	customSettings := storage.DefaultMonitorSettings
	customSettings.Windows[0].Profile.MinSeconds = 7
	customSettings.Windows[0].Profile.MaxSeconds = 14
	if _, err := store.SaveMonitorSettings(ctx, customSettings); err != nil {
		t.Fatalf("SaveMonitorSettings failed: %v", err)
	}

	// Re-run migration
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("re-run Migrate failed: %v", err)
	}

	readBack, err := store.GetMonitorSettings(ctx)
	if err != nil {
		t.Fatalf("GetMonitorSettings failed: %v", err)
	}

	if readBack.Windows[0].Profile.MinSeconds != 7 || readBack.Windows[0].Profile.MaxSeconds != 14 {
		t.Errorf("expected custom cadence 7-14s to be preserved, got %d-%ds",
			readBack.Windows[0].Profile.MinSeconds, readBack.Windows[0].Profile.MaxSeconds)
	}
}
