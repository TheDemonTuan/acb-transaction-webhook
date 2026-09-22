package storage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func recoveryTestRun(t *testing.T, ctx context.Context, store *Store, eventKey string) (Connection, RecoveryRun) {
	t.Helper()
	conn, err := store.ConfigureConnection(ctx, "***1234")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, "UPDATE connections SET generation=1 WHERE id=?", conn.ID); err != nil {
		t.Fatal(err)
	}
	conn.Generation = 1
	run, created, err := store.EnsureRecoveryRunWithPlan(ctx, conn.ID, conn.Generation, eventKey, RecoveryRunPlan{
		Reason:    "WORKER_STARTUP",
		RangeFrom: "2026-09-21",
		RangeTo:   "2026-09-22",
		NextDay:   "2026-09-21",
	})
	if err != nil || !created {
		t.Fatalf("ensure recovery run: %+v created=%v err=%v", run, created, err)
	}
	run, err = store.ClaimRecoveryRun(ctx, run.ID, conn.ID, conn.Generation)
	if err != nil {
		t.Fatal(err)
	}
	return conn, run
}

func TestRecoveryRunSurvivesStoreReopen(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "recovery-reopen.db")
	store, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	conn, run := recoveryTestRun(t, ctx, store, "startup-reopen")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	persisted, err := reopened.GetRecoveryRunByEvent(ctx, conn.ID, conn.Generation, "startup-reopen")
	if err != nil {
		t.Fatal(err)
	}
	if persisted.ID != run.ID || persisted.Status != RecoveryRunStatusRunning || persisted.NextDay != "2026-09-21" {
		t.Fatalf("reopened recovery run = %+v", persisted)
	}
	committed, err := reopened.CommitRecoveryDay(ctx, RecoveryDayCommit{
		RunID:        run.ID,
		ConnectionID: conn.ID,
		Generation:   conn.Generation,
		Day:          "2026-09-21",
		NextDay:      "2026-09-22",
		RowsSeen:     3,
		ScanID:       "scan-reopen",
		CoverageFrom: "2026-09-21",
	})
	if err != nil {
		t.Fatal(err)
	}
	if committed.NextDay != "2026-09-22" {
		t.Fatalf("next day after reopen commit = %q", committed.NextDay)
	}
	coverage, err := reopened.CheckRangeCoverage(ctx, conn.ID, "2026-09-21", "2026-09-21")
	if err != nil {
		t.Fatal(err)
	}
	if !coverage {
		t.Fatal("committed recovery day was not durable after reopen")
	}
}

func TestCommitRecoveryDayRollsBackAllProgressOnFailure(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "recovery-atomic.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	conn, run := recoveryTestRun(t, ctx, store, "atomic-failure")
	if err := store.SaveCheckpoint(ctx, Checkpoint{
		ConnectionID: conn.ID,
		ScanID:       "before",
		CoverageFrom: "2026-09-20",
		CoverageTo:   "2026-09-20",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, "DROP TABLE history_coverage"); err != nil {
		t.Fatal(err)
	}
	_, err = store.CommitRecoveryDay(ctx, RecoveryDayCommit{
		RunID:        run.ID,
		ConnectionID: conn.ID,
		Generation:   conn.Generation,
		Day:          "2026-09-21",
		NextDay:      "2026-09-22",
		RowsSeen:     3,
		ScanID:       "scan-failed",
		CoverageFrom: "2026-09-21",
	})
	if err == nil {
		t.Fatal("expected atomic recovery commit to fail")
	}
	checkpoint, err := store.GetCheckpoint(ctx, conn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.CoverageTo != "2026-09-20" || checkpoint.ScanID != "before" {
		t.Fatalf("checkpoint advanced after failed commit: %+v", checkpoint)
	}
	persisted, err := store.GetRecoveryRunByEvent(ctx, conn.ID, conn.Generation, "atomic-failure")
	if err != nil {
		t.Fatal(err)
	}
	if persisted.NextDay != "2026-09-21" {
		t.Fatalf("recovery cursor advanced after failed commit: %q", persisted.NextDay)
	}
}

func TestCommitRecoveryDayRejectsStaleGenerationWithoutWrites(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "recovery-generation.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	conn, run := recoveryTestRun(t, ctx, store, "generation-failure")
	if _, err := store.DB().ExecContext(ctx, "UPDATE connections SET generation=generation+1 WHERE id=?", conn.ID); err != nil {
		t.Fatal(err)
	}
	_, err = store.CommitRecoveryDay(ctx, RecoveryDayCommit{
		RunID:        run.ID,
		ConnectionID: conn.ID,
		Generation:   conn.Generation,
		Day:          "2026-09-21",
		NextDay:      "2026-09-22",
		RowsSeen:     3,
		ScanID:       "scan-stale",
		CoverageFrom: "2026-09-21",
	})
	if !errors.Is(err, ErrGenerationFenceMismatch) {
		t.Fatalf("expected generation fence mismatch, got %v", err)
	}
	var coverageRows int
	if err := store.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM history_coverage WHERE connection_id=? AND day=?", conn.ID, "2026-09-21").Scan(&coverageRows); err != nil {
		t.Fatal(err)
	}
	if coverageRows != 0 {
		t.Fatalf("stale generation wrote coverage rows: %d", coverageRows)
	}
}
