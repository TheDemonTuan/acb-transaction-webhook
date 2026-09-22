package monitor

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func TestReconcileRecoveryStopsOnCheckpointLookupError(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "reconcile_checkpoint_error.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	conn, err := store.ConfigureConnection(ctx, "***1234")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, "UPDATE connections SET state='MONITORING'"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, "DROP TABLE checkpoints"); err != nil {
		t.Fatal(err)
	}

	mon := New(store, nil, 5*time.Second, 15*time.Second)
	mon.now = fixedRealtimeTime
	mon.reconcileRecovery(ctx)

	runs, err := store.ListOpenRecoveryRuns(ctx, conn.ID, conn.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("expected no recovery intent after checkpoint lookup failure, got %d", len(runs))
	}
}

func TestReconcileRecoveryStopsOnCoverageLookupError(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "reconcile_coverage_error.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	conn, err := store.ConfigureConnection(ctx, "***1234")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, "UPDATE connections SET state='MONITORING'"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, "DROP TABLE history_coverage"); err != nil {
		t.Fatal(err)
	}

	mon := New(store, nil, 5*time.Second, 15*time.Second)
	mon.now = fixedRealtimeTime
	mon.reconcileRecovery(ctx)

	runs, err := store.ListOpenRecoveryRuns(ctx, conn.ID, conn.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("expected no recovery intent after coverage lookup failure, got %d", len(runs))
	}
}
