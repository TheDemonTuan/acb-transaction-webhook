package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func TestWorkerService_VerifySession_GenerationGuard(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "worker_svc_test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Configure initial connection at generation 5
	conn, err := store.ConfigureConnection(ctx, "***1234")
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.DB().ExecContext(ctx, `UPDATE connections SET generation = 5 WHERE id = ?`, conn.ID)
	if err != nil {
		t.Fatal(err)
	}

	ws := &workerService{
		store: store,
	}

	// 1. Invalid generation <= 0
	if err := ws.VerifySession(ctx, conn.ID, 0, []byte("pw")); err == nil {
		t.Fatal("expected error for generation <= 0, got nil")
	}
	if err := ws.VerifySession(ctx, conn.ID, -1, []byte("pw")); err == nil {
		t.Fatal("expected error for generation < 0, got nil")
	}

	// 2. Stale generation (< 5) rejected by generation guard
	if err := ws.VerifySession(ctx, conn.ID, 4, []byte("pw")); err == nil {
		t.Fatal("expected error for stale generation 4 < current 5, got nil")
	}

	// 3. Mismatched future generation (> 5) rejected by strict equality fence
	if err := ws.VerifySession(ctx, conn.ID, 6, []byte("pw")); err == nil {
		t.Fatal("expected error for mismatched generation 6 != current 5, got nil")
	}

	// 4. Exact matching generation 5 proceeds past guard (fails on unconfigured verifier in this test)
	err = ws.VerifySession(ctx, conn.ID, 5, []byte("pw"))
	if err == nil || err.Error() != "session verifier not configured" {
		t.Fatalf("expected 'session verifier not configured' error when generation guard passes, got: %v", err)
	}
}

func TestWorkerService_NotifyAndWake_Uninitialized(t *testing.T) {
	ctx := context.Background()
	ws := &workerService{}

	if err := ws.NotifySettingsChanged(ctx); err == nil {
		t.Fatal("expected error when bank monitor is nil, got nil")
	}
	if err := ws.WakeDispatcher(ctx); err == nil {
		t.Fatal("expected error when dispatcher is nil, got nil")
	}
	if err := ws.RequestSync(ctx); err == nil {
		t.Fatal("expected error when bank monitor is nil, got nil")
	}
	if _, err := ws.EnsureHistory(ctx, "2026-09-01", "2026-09-02"); err == nil {
		t.Fatal("expected error when bank monitor is nil, got nil")
	}
}
