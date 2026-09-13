package storage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestStore_HistorySyncJobs(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "history_jobs_test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	conn, err := store.ConfigureConnection(ctx, "***9999")
	if err != nil {
		t.Fatal(err)
	}

	// 1. Initial check - no jobs
	_, found, err := store.GetLatestHistorySyncJob(ctx, conn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("expected no history sync job found initially")
	}

	// 2. Create job
	jobID, err := store.CreateHistorySyncJob(ctx, conn.ID, "2026-09-01", "2026-09-10")
	if err != nil {
		t.Fatalf("failed to create history sync job: %v", err)
	}
	if jobID == "" {
		t.Fatal("expected non-empty job ID")
	}

	job, found, err := store.GetLatestHistorySyncJob(ctx, conn.ID)
	if err != nil || !found {
		t.Fatalf("expected to find running job: %v", err)
	}
	if job.Status != "RUNNING" || job.RangeFrom != "2026-09-01" || job.RangeTo != "2026-09-10" {
		t.Fatalf("unexpected running job state: %+v", job)
	}

	// 3. Complete job successfully
	if err := store.CompleteHistorySyncJob(ctx, jobID, 42, nil); err != nil {
		t.Fatalf("failed to complete job: %v", err)
	}

	job, found, err = store.GetLatestHistorySyncJob(ctx, conn.ID)
	if err != nil || !found {
		t.Fatalf("expected to find completed job: %v", err)
	}
	if job.Status != "COMPLETED" || job.RowsSeen != 42 || job.ErrorMessage != "" {
		t.Fatalf("unexpected completed job state: %+v", job)
	}

	// 4. Create and fail a job
	failedJobID, err := store.CreateHistorySyncJob(ctx, conn.ID, "2026-09-11", "2026-09-12")
	if err != nil {
		t.Fatal(err)
	}
	testErr := errors.New("upstream network error")
	if err := store.CompleteHistorySyncJob(ctx, failedJobID, 0, testErr); err != nil {
		t.Fatal(err)
	}

	job, found, err = store.GetLatestHistorySyncJob(ctx, conn.ID)
	if err != nil || !found {
		t.Fatal(err)
	}
	if job.Status != "FAILED" || job.ErrorMessage != "upstream network error" {
		t.Fatalf("unexpected failed job state: %+v", job)
	}
}
