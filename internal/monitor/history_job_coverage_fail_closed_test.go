package monitor_test

import (
	"context"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/monitor"
	"github.com/thedemontuan/acb-transaction-webhook/internal/scheduler"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func TestHistoryJobCoverageLookupFailureIsTerminal(t *testing.T) {
	ctx := context.Background()
	store, connID := setupTestDB(t, "history_coverage_lookup_error.db")
	defer store.Close()

	client := newFake31DayClient()
	runner := monitor.NewHistoryJobRunner(store, client, nil, nil)
	job, _, err := store.CreateOrGetHistorySyncJob(ctx, connID, 1, "2026-09-12", "2026-09-12")
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.ClaimNextHistorySyncJob(ctx, time.Now())
	if err != nil || !ok {
		t.Fatalf("claim job: claimed=%v err=%v", ok, err)
	}
	conn, err := store.Connection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, "DROP TABLE history_coverage"); err != nil {
		t.Fatal(err)
	}

	task := monitor.NewHistoryJobTask(runner, claimed, conn)
	res, err := task.Step(ctx)
	if err == nil || res.Outcome != scheduler.OutcomeFatal {
		t.Fatalf("expected terminal coverage lookup error, result=%+v err=%v", res, err)
	}
	if client.historyCalls.Load() != 0 {
		t.Fatalf("expected no ACB request after coverage lookup failure, got %d", client.historyCalls.Load())
	}
	updated, err := store.GetHistorySyncJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != storage.HistoryJobStatusFailed || updated.ErrorCode != "COVERAGE_LOOKUP_ERROR" {
		t.Fatalf("expected FAILED/COVERAGE_LOOKUP_ERROR, got %s/%s", updated.Status, updated.ErrorCode)
	}
}
