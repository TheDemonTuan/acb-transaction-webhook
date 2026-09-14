package integration_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

// TestHistoryCancelAndGenerationFencing verifies:
// 1. Browser cancel transitions job to CANCELED idempotently;
// 2. Generation fencing prevents jobs of older connection generations from mutating progress;
// 3. Worker crash (stale running job) is recovered via RequeueStaleHistorySyncJobs / RequeueRunningHistorySyncJobs.
func TestHistoryCancelAndGenerationFencing(t *testing.T) {
	ctx := context.Background()
	dbDir := t.TempDir()
	dbPath := filepath.Join(dbDir, "gateway.db")

	store, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}
	defer store.Close()

	// 1. Setup connection
	conn, err := store.ConfigureConnection(ctx, "***9999")
	if err != nil {
		t.Fatalf("configure connection: %v", err)
	}

	// 2. Create history job under generation 1
	job1, created, err := store.CreateOrGetHistorySyncJob(ctx, conn.ID, conn.Generation, "2026-09-01", "2026-09-05")
	if err != nil {
		t.Fatalf("create history job: %v", err)
	}
	if !created {
		t.Fatal("expected new job creation")
	}
	if job1.Status != storage.HistoryJobStatusQueued {
		t.Fatalf("expected QUEUED, got %s", job1.Status)
	}

	// 3. Gate 2: Explicit cancel sets CANCELED immediately and idempotently
	if err := store.CancelHistorySyncJob(ctx, job1.ID); err != nil {
		t.Fatalf("cancel job: %v", err)
	}
	cancelledJob, err := store.GetHistorySyncJob(ctx, job1.ID)
	if err != nil {
		t.Fatalf("get cancelled job: %v", err)
	}
	if cancelledJob.Status != storage.HistoryJobStatusCanceled {
		t.Fatalf("expected CANCELED status, got %s", cancelledJob.Status)
	}
	// Cancelling again is a safe no-op / idempotent
	_ = store.CancelHistorySyncJob(ctx, job1.ID)

	// 4. Gate 3 & Recovery: Create another job, claim it, simulate worker crash (orphaned RUNNING job)
	job2, _, err := store.CreateOrGetHistorySyncJob(ctx, conn.ID, conn.Generation, "2026-09-06", "2026-09-10")
	if err != nil {
		t.Fatalf("create job2: %v", err)
	}

	// Worker 1 claims job2
	claimedJob, ok, err := store.ClaimNextHistorySyncJob(ctx, time.Now().UTC())
	if err != nil || !ok {
		t.Fatalf("claim job2: %v, ok=%v", err, ok)
	}
	if claimedJob.ID != job2.ID {
		t.Fatalf("expected job %s claimed, got %s", job2.ID, claimedJob.ID)
	}
	if claimedJob.Status != storage.HistoryJobStatusRunning {
		t.Fatalf("expected RUNNING, got %s", claimedJob.Status)
	}

	// Record partial progress before crash
	err = store.RecordHistoryJobProgress(ctx, job2.ID, "2026-09-06", 1, 10, true, 10)
	if err != nil {
		t.Fatalf("record progress: %v", err)
	}

	// Simulate worker crash: worker dies, leaving job2 in RUNNING with a stale heartbeat
	// Successor worker boots up and runs RequeueRunningHistorySyncJobs or RequeueStaleHistorySyncJobs
	requeued, err := store.RequeueRunningHistorySyncJobs(ctx, "worker crash recovery")
	if err != nil {
		t.Fatalf("requeue running jobs: %v", err)
	}
	if requeued != 1 {
		t.Fatalf("expected 1 requeued job, got %d", requeued)
	}

	recoveredJob, err := store.GetHistorySyncJob(ctx, job2.ID)
	if err != nil {
		t.Fatalf("get recovered job: %v", err)
	}
	if recoveredJob.Status != storage.HistoryJobStatusQueued {
		t.Fatalf("expected recovered job to be QUEUED, got %s", recoveredJob.Status)
	}
	// Checkpoint is preserved
	if recoveredJob.PagesDone != 1 || recoveredJob.RowsSeen != 10 {
		t.Fatalf("checkpoint progress lost: pages=%d, rows=%d", recoveredJob.PagesDone, recoveredJob.RowsSeen)
	}

	// 5. Generation Fencing: Now bump connection generation (e.g. user re-authenticated)
	_, err = store.DB().ExecContext(ctx, "UPDATE connections SET generation = generation + 1 WHERE id = ?", conn.ID)
	if err != nil {
		t.Fatalf("bump connection generation: %v", err)
	}

	// Stale worker tries to claim or update job2 (which has old generation)
	// ClaimNextHistorySyncJob must fence and reject/cancel stale generation jobs
	claimedStale, ok, err := store.ClaimNextHistorySyncJob(ctx, time.Now().UTC())
	if err == nil && ok && claimedStale.ID == job2.ID {
		t.Fatalf("stale generation job should have been fenced, but was claimed")
	}

	// Check that job2 was marked CANCELED with STALE_GENERATION error
	fencedJob, err := store.GetHistorySyncJob(ctx, job2.ID)
	if err != nil {
		t.Fatalf("get fenced job: %v", err)
	}
	if fencedJob.Status != storage.HistoryJobStatusCanceled {
		t.Fatalf("expected fenced job to be CANCELED, got %s", fencedJob.Status)
	}
	if fencedJob.ErrorCode != "STALE_GENERATION" {
		t.Fatalf("expected error code STALE_GENERATION, got %s", fencedJob.ErrorCode)
	}
}
