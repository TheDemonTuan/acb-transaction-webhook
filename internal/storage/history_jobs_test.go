package storage

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func setupTestStore(t *testing.T) (*Store, Connection) {
	t.Helper()
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("failed to open test store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	conn, err := store.ConfigureConnection(ctx, "***9999")
	if err != nil {
		t.Fatalf("failed to configure connection: %v", err)
	}
	return store, conn
}

func TestStore_HistorySyncJobs_BasicLifecycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, conn := setupTestStore(t)

	// 1. Initial check - no jobs
	_, found, err := store.GetLatestHistorySyncJob(ctx, conn.ID)
	if err != nil {
		t.Fatalf("unexpected error getting latest job: %v", err)
	}
	if found {
		t.Fatal("expected no history sync job found initially")
	}

	// 2. Create job
	job, created, err := store.CreateOrGetHistorySyncJob(ctx, conn.ID, conn.Generation, "2026-09-01", "2026-09-10")
	if err != nil {
		t.Fatalf("failed to create history sync job: %v", err)
	}
	if !created {
		t.Fatal("expected job to be newly created")
	}
	if job.Status != HistoryJobStatusQueued || job.RangeFrom != "2026-09-01" || job.RangeTo != "2026-09-10" {
		t.Fatalf("unexpected queued job state: %+v", job)
	}

	// 3. Claim job
	now := time.Now().UTC()
	claimedJob, claimed, err := store.ClaimNextHistorySyncJob(ctx, now)
	if err != nil || !claimed {
		t.Fatalf("expected job to be claimed: claimed=%v, err=%v", claimed, err)
	}
	if claimedJob.ID != job.ID || claimedJob.Status != HistoryJobStatusRunning || claimedJob.Attempts != 1 {
		t.Fatalf("unexpected claimed job state: %+v", claimedJob)
	}

	// 4. Heartbeat
	if err := store.HeartbeatHistorySyncJob(ctx, job.ID, "2026-09-03", 3, 42); err != nil {
		t.Fatalf("heartbeat failed: %v", err)
	}
	hbJob, err := store.GetHistorySyncJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job failed: %v", err)
	}
	if hbJob.CurrentDay != "2026-09-03" || hbJob.PagesDone != 3 || hbJob.RowsSeen != 42 || hbJob.HeartbeatAt == "" {
		t.Fatalf("unexpected job after heartbeat: %+v", hbJob)
	}

	// 5. Complete job
	if err := store.CompleteHistorySyncJob(ctx, job.ID, 10, 150); err != nil {
		t.Fatalf("failed to complete job: %v", err)
	}

	completedJob, err := store.GetHistorySyncJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get completed job: %v", err)
	}
	if completedJob.Status != HistoryJobStatusCompleted || completedJob.PagesDone != 10 || completedJob.RowsSeen != 150 || completedJob.FinishedAt == "" {
		t.Fatalf("unexpected completed job state: %+v", completedJob)
	}

	// 6. Backwards compatible helper check
	compatJob, found, err := store.GetLatestHistorySyncJob(ctx, conn.ID)
	if err != nil || !found {
		t.Fatalf("expected to find latest job: %v", err)
	}
	if compatJob.ID != job.ID || compatJob.Status != HistoryJobStatusCompleted {
		t.Fatalf("unexpected latest job: %+v", compatJob)
	}
}

func TestStore_HistorySyncJobs_MigrationFromV7(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "v7_migration.db")

	// Set up database manually with versions 1-7
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()

	if _, err := db.ExecContext(ctx, `CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, checksum TEXT NOT NULL, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatalf("create schema_migrations: %v", err)
	}

	for _, m := range migrations {
		if m.version > 7 {
			break
		}
		if _, err := db.ExecContext(ctx, m.sql); err != nil {
			t.Fatalf("apply migration %d: %v", m.version, err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations(version, checksum, applied_at) VALUES(?,?,?)`, m.version, m.checksum, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			t.Fatalf("record migration %d: %v", m.version, err)
		}
	}

	// Insert pre-existing v7 connection and history jobs
	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO connections (id, bank_code, state, generation, config_revision, created_at, updated_at)
		VALUES ('conn_v7', 'ACB', 'MONITORING', 1, 1, ?, ?)
	`, nowStr, nowStr); err != nil {
		t.Fatalf("insert v7 connection: %v", err)
	}

	// Insert historical COMPLETED, FAILED, and RUNNING rows in v7 schema
	if _, err := db.ExecContext(ctx, `
		INSERT INTO history_sync_jobs (id, connection_id, range_from, range_to, status, rows_seen, error_message, created_at, updated_at)
		VALUES
			('job_v7_completed', 'conn_v7', '2026-08-01', '2026-08-05', 'COMPLETED', 25, NULL, '2026-08-01T00:00:00Z', '2026-08-01T00:05:00Z'),
			('job_v7_failed', 'conn_v7', '2026-08-06', '2026-08-10', 'FAILED', 5, 'v7 failure error', '2026-08-06T00:00:00Z', '2026-08-06T00:01:00Z'),
			('job_v7_running', 'conn_v7', '2026-08-11', '2026-08-15', 'RUNNING', 10, NULL, '2026-08-11T00:00:00Z', '2026-08-11T00:02:00Z')
	`); err != nil {
		t.Fatalf("insert v7 jobs: %v", err)
	}
	db.Close()

	// Open with store to trigger migration 8
	store, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open store and run migration 8: %v", err)
	}
	defer store.Close()

	// Verify schema version is at least 8
	rep, err := store.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("check schema version: %v", err)
	}
	if rep.Version < 8 {
		t.Fatalf("unexpected schema version: %+v", rep)
	}

	// Verify historical rows are readable and backfilled safely
	compJob, err := store.GetHistorySyncJob(ctx, "job_v7_completed")
	if err != nil {
		t.Fatalf("read completed v7 job: %v", err)
	}
	if compJob.Status != HistoryJobStatusCompleted || compJob.Generation != 0 || compJob.RowsSeen != 25 || compJob.StartedAt != "2026-08-01T00:00:00Z" || compJob.FinishedAt != "2026-08-01T00:05:00Z" {
		t.Fatalf("unexpected backfilled completed job: %+v", compJob)
	}

	failJob, err := store.GetHistorySyncJob(ctx, "job_v7_failed")
	if err != nil {
		t.Fatalf("read failed v7 job: %v", err)
	}
	if failJob.Status != HistoryJobStatusFailed || failJob.ErrorMessage != "v7 failure error" || failJob.FinishedAt != "2026-08-06T00:01:00Z" {
		t.Fatalf("unexpected backfilled failed job: %+v", failJob)
	}

	runJob, err := store.GetHistorySyncJob(ctx, "job_v7_running")
	if err != nil {
		t.Fatalf("read running v7 job: %v", err)
	}
	if runJob.Status != HistoryJobStatusRunning || runJob.StartedAt != "2026-08-11T00:00:00Z" {
		t.Fatalf("unexpected backfilled running job: %+v", runJob)
	}
}

func TestStore_HistorySyncJobs_ActiveJobUniqueness(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, conn := setupTestStore(t)

	// Create job 1
	job1, created1, err := store.CreateOrGetHistorySyncJob(ctx, conn.ID, conn.Generation, "2026-09-01", "2026-09-05")
	if err != nil || !created1 {
		t.Fatalf("job 1 creation failed: created=%v, err=%v", created1, err)
	}

	// Create job 2 with same range and generation -> should return existing job
	job2, created2, err := store.CreateOrGetHistorySyncJob(ctx, conn.ID, conn.Generation, "2026-09-01", "2026-09-05")
	if err != nil {
		t.Fatalf("job 2 creation failed: %v", err)
	}
	if created2 {
		t.Fatal("expected job 2 to return existing active job, not created")
	}
	if job2.ID != job1.ID {
		t.Fatalf("expected identical job ID %s, got %s", job1.ID, job2.ID)
	}

	// Complete job 1
	_, claimed, err := store.ClaimNextHistorySyncJob(ctx, time.Now().UTC())
	if err != nil || !claimed {
		t.Fatalf("claim failed: %v", err)
	}
	if err := store.CompleteHistorySyncJob(ctx, job1.ID, 5, 20); err != nil {
		t.Fatalf("complete failed: %v", err)
	}

	// Now that job 1 is COMPLETED, creating for the same range succeeds and creates a new active job
	job3, created3, err := store.CreateOrGetHistorySyncJob(ctx, conn.ID, conn.Generation, "2026-09-01", "2026-09-05")
	if err != nil || !created3 {
		t.Fatalf("job 3 creation failed: created=%v, err=%v", created3, err)
	}
	if job3.ID == job1.ID {
		t.Fatal("expected new job ID for subsequent active job after previous completed")
	}
}

func TestStore_HistorySyncJobs_ConcurrentCreateOrGet(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, conn := setupTestStore(t)

	const concurrency = 20
	var wg sync.WaitGroup
	var createdCount int64
	jobIDs := make([]string, concurrency)
	errorsList := make([]error, concurrency)

	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func(idx int) {
			defer wg.Done()
			job, created, err := store.CreateOrGetHistorySyncJob(ctx, conn.ID, conn.Generation, "2026-09-01", "2026-09-10")
			if err != nil {
				errorsList[idx] = err
				return
			}
			jobIDs[idx] = job.ID
			if created {
				atomic.AddInt64(&createdCount, 1)
			}
		}(i)
	}
	wg.Wait()

	for i, err := range errorsList {
		if err != nil {
			t.Fatalf("goroutine %d failed: %v", i, err)
		}
	}

	if createdCount != 1 {
		t.Fatalf("expected exactly 1 job created, got %d", createdCount)
	}

	firstID := jobIDs[0]
	for i, id := range jobIDs {
		if id != firstID {
			t.Fatalf("goroutine %d got different job ID: expected %s, got %s", i, firstID, id)
		}
	}
}

func TestStore_HistorySyncJobs_AtomicClaim(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, conn := setupTestStore(t)

	_, created, err := store.CreateOrGetHistorySyncJob(ctx, conn.ID, conn.Generation, "2026-09-01", "2026-09-05")
	if err != nil || !created {
		t.Fatalf("create failed: %v", err)
	}

	const concurrency = 10
	var wg sync.WaitGroup
	var claimedCount int64
	now := time.Now().UTC()

	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			defer wg.Done()
			_, claimed, err := store.ClaimNextHistorySyncJob(ctx, now)
			if err == nil && claimed {
				atomic.AddInt64(&claimedCount, 1)
			}
		}()
	}
	wg.Wait()

	if claimedCount != 1 {
		t.Fatalf("expected exactly 1 claim winner, got %d", claimedCount)
	}
}

func TestStore_HistorySyncJobs_TransientRequeue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, conn := setupTestStore(t)

	job, _, err := store.CreateOrGetHistorySyncJob(ctx, conn.ID, conn.Generation, "2026-09-01", "2026-09-05")
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	claimedJob, claimed, err := store.ClaimNextHistorySyncJob(ctx, now)
	if err != nil || !claimed {
		t.Fatalf("claim failed: %v", err)
	}
	if claimedJob.Attempts != 1 {
		t.Fatalf("expected attempt 1, got %d", claimedJob.Attempts)
	}

	// Requeue with future next_attempt_at (+5 seconds)
	nextAttempt := now.Add(5 * time.Second)
	if err := store.RequeueHistorySyncJob(ctx, job.ID, "ACB_BUSY", "system is busy", nextAttempt); err != nil {
		t.Fatalf("requeue failed: %v", err)
	}

	// Attempt to claim immediately at 'now' -> must not claim
	_, claimedAtNow, err := store.ClaimNextHistorySyncJob(ctx, now)
	if err != nil {
		t.Fatalf("claim error: %v", err)
	}
	if claimedAtNow {
		t.Fatal("expected job to not be claimable before next_attempt_at")
	}

	// Attempt to claim at 'nextAttempt + 1s' -> must succeed and increment attempts to 2
	claimedAfter, claimed, err := store.ClaimNextHistorySyncJob(ctx, nextAttempt.Add(1*time.Second))
	if err != nil || !claimed {
		t.Fatalf("expected claim after delay: claimed=%v, err=%v", claimed, err)
	}
	if claimedAfter.Attempts != 2 {
		t.Fatalf("expected attempt 2 after second claim, got %d", claimedAfter.Attempts)
	}
	if claimedAfter.Status != HistoryJobStatusRunning {
		t.Fatalf("expected RUNNING status, got %s", claimedAfter.Status)
	}
}

func TestStore_HistorySyncJobs_TerminalTransitions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, conn := setupTestStore(t)

	// Case 1: Job completed cannot transition
	job1, _, _ := store.CreateOrGetHistorySyncJob(ctx, conn.ID, conn.Generation, "2026-09-01", "2026-09-02")
	_, _, _ = store.ClaimNextHistorySyncJob(ctx, time.Now().UTC())
	if err := store.CompleteHistorySyncJob(ctx, job1.ID, 1, 10); err != nil {
		t.Fatal(err)
	}

	if err := store.HeartbeatHistorySyncJob(ctx, job1.ID, "2026-09-02", 2, 20); !errors.Is(err, ErrInvalidStateTransition) {
		t.Fatalf("expected ErrInvalidStateTransition on heartbeat completed job, got: %v", err)
	}
	if err := store.RequeueHistorySyncJob(ctx, job1.ID, "ERR", "msg", time.Now().UTC()); !errors.Is(err, ErrInvalidStateTransition) {
		t.Fatalf("expected ErrInvalidStateTransition on requeue completed job, got: %v", err)
	}
	if err := store.CancelHistorySyncJob(ctx, job1.ID); !errors.Is(err, ErrJobTerminal) {
		t.Fatalf("expected ErrJobTerminal on cancel completed job, got: %v", err)
	}
	if err := store.FailHistorySyncJob(ctx, job1.ID, "ERR", "msg"); !errors.Is(err, ErrJobTerminal) {
		t.Fatalf("expected ErrJobTerminal on fail completed job, got: %v", err)
	}

	// Case 2: Cancellation idempotency and terminal behavior
	job2, _, _ := store.CreateOrGetHistorySyncJob(ctx, conn.ID, conn.Generation, "2026-09-03", "2026-09-04")
	if err := store.CancelHistorySyncJob(ctx, job2.ID); err != nil {
		t.Fatalf("first cancel failed: %v", err)
	}
	// Idempotent cancel
	if err := store.CancelHistorySyncJob(ctx, job2.ID); err != nil {
		t.Fatalf("idempotent cancel should succeed with nil, got: %v", err)
	}

	// Cannot heartbeat or complete canceled job
	if err := store.HeartbeatHistorySyncJob(ctx, job2.ID, "2026-09-03", 1, 5); !errors.Is(err, ErrJobCanceled) {
		t.Fatalf("expected ErrJobCanceled, got: %v", err)
	}
	if err := store.CompleteHistorySyncJob(ctx, job2.ID, 1, 5); !errors.Is(err, ErrJobCanceled) && !errors.Is(err, ErrInvalidStateTransition) {
		t.Fatalf("expected ErrJobCanceled or ErrInvalidStateTransition, got: %v", err)
	}
}

func TestStore_HistorySyncJobs_StaleHeartbeatRecovery(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, conn := setupTestStore(t)

	job, _, err := store.CreateOrGetHistorySyncJob(ctx, conn.ID, conn.Generation, "2026-09-01", "2026-09-05")
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	_, claimed, err := store.ClaimNextHistorySyncJob(ctx, now)
	if err != nil || !claimed {
		t.Fatalf("claim failed: %v", err)
	}

	// Simulate stale heartbeat (10 minutes ago)
	staleTime := now.Add(-10 * time.Minute).Format(time.RFC3339Nano)
	if _, err := store.DB().ExecContext(ctx, `UPDATE history_sync_jobs SET heartbeat_at = ? WHERE id = ?`, staleTime, job.ID); err != nil {
		t.Fatal(err)
	}

	// Requeue stale jobs older than 5 minutes
	threshold := now.Add(-5 * time.Minute)
	requeued, err := store.RequeueStaleHistorySyncJobs(ctx, threshold)
	if err != nil {
		t.Fatalf("requeue stale jobs failed: %v", err)
	}
	if requeued != 1 {
		t.Fatalf("expected 1 job requeued, got %d", requeued)
	}

	// Verify job is back in QUEUED state
	recovered, err := store.GetHistorySyncJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != HistoryJobStatusQueued || recovered.ErrorCode != "STALE_HEARTBEAT_RECOVERED" {
		t.Fatalf("unexpected recovered job state: %+v", recovered)
	}

	// Worker can claim it again
	reclaimed, claimed2, err := store.ClaimNextHistorySyncJob(ctx, now)
	if err != nil || !claimed2 {
		t.Fatalf("reclaim failed: %v", err)
	}
	if reclaimed.ID != job.ID || reclaimed.Attempts != 2 {
		t.Fatalf("unexpected reclaimed job: %+v", reclaimed)
	}
}

func TestStore_HistorySyncJobs_GenerationFencing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, conn := setupTestStore(t)

	// Create job at generation 0
	job, _, err := store.CreateOrGetHistorySyncJob(ctx, conn.ID, conn.Generation, "2026-09-01", "2026-09-05")
	if err != nil {
		t.Fatal(err)
	}

	// Claim job at generation 0
	_, claimed, err := store.ClaimNextHistorySyncJob(ctx, time.Now().UTC())
	if err != nil || !claimed {
		t.Fatal(err)
	}

	// Bump connection generation to 1
	if _, err := store.DB().ExecContext(ctx, `UPDATE connections SET generation = generation + 1 WHERE id = ?`, conn.ID); err != nil {
		t.Fatal(err)
	}

	// Heartbeat on stale generation must fail closed
	err = store.HeartbeatHistorySyncJob(ctx, job.ID, "2026-09-02", 1, 10)
	if !errors.Is(err, ErrGenerationFenceMismatch) {
		t.Fatalf("expected ErrGenerationFenceMismatch on heartbeat, got: %v", err)
	}

	// Verify the stale job was marked CANCELED
	staleJob, err := store.GetHistorySyncJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if staleJob.Status != HistoryJobStatusCanceled || staleJob.ErrorCode != "STALE_GENERATION" {
		t.Fatalf("expected job to be canceled due to stale generation, got: %+v", staleJob)
	}

	// CreateOrGet with old generation 0 must fail closed
	_, _, err = store.CreateOrGetHistorySyncJob(ctx, conn.ID, 0, "2026-09-06", "2026-09-07")
	if !errors.Is(err, ErrGenerationFenceMismatch) {
		t.Fatalf("expected ErrGenerationFenceMismatch on CreateOrGet with stale gen, got: %v", err)
	}
}

func TestStore_HistorySyncJobs_SanitizeErrorMessages(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, conn := setupTestStore(t)

	job, _, err := store.CreateOrGetHistorySyncJob(ctx, conn.ID, conn.Generation, "2026-09-01", "2026-09-05")
	if err != nil {
		t.Fatal(err)
	}

	// 1. Strip HTML
	rawHTMLErr := "<!DOCTYPE html><html><head><title>502 Bad Gateway</title></head><body><center><h1>502 Bad Gateway</h1></center><hr><center>cloudflare</center></body></html>"
	if err := store.FailHistorySyncJob(ctx, job.ID, "HTTP_502", rawHTMLErr); err != nil {
		t.Fatal(err)
	}

	failedJob, err := store.GetHistorySyncJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failedJob.ErrorMessage != "upstream returned HTML error response" {
		t.Fatalf("expected HTML error to be replaced, got: %q", failedJob.ErrorMessage)
	}

	// 2. Redact sensitive auth tokens, cookies, and passwords
	job2, _, err := store.CreateOrGetHistorySyncJob(ctx, conn.ID, conn.Generation, "2026-09-06", "2026-09-07")
	if err != nil {
		t.Fatal(err)
	}
	sensitiveErr := "request failed: Authorization: Bearer secret-token-abc123xyz; Cookie: session_id=sess_456; password=superSecretPassword!123"
	if err := store.FailHistorySyncJob(ctx, job2.ID, "AUTH_ERR", sensitiveErr); err != nil {
		t.Fatal(err)
	}

	failedJob2, err := store.GetHistorySyncJob(ctx, job2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failedJob2.ErrorMessage == sensitiveErr {
		t.Fatal("expected sensitive error message to be sanitized")
	}
	if len(failedJob2.ErrorMessage) > 1005 {
		t.Fatalf("error message exceeded max length: %d", len(failedJob2.ErrorMessage))
	}
}

func TestStore_HistorySyncJobs_RecordHistoryJobProgress(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, conn := setupTestStore(t)

	job, _, err := store.CreateOrGetHistorySyncJob(ctx, conn.ID, conn.Generation, "2026-09-01", "2026-09-03")
	if err != nil {
		t.Fatal(err)
	}

	_, claimed, err := store.ClaimNextHistorySyncJob(ctx, time.Now().UTC())
	if err != nil || !claimed {
		t.Fatal(err)
	}

	// Record progress for day 1 (completed)
	if err := store.RecordHistoryJobProgress(ctx, job.ID, "2026-09-01", 1, 15, true, 15); err != nil {
		t.Fatalf("record progress failed: %v", err)
	}

	// Verify coverage record was created in history_coverage
	covered, err := store.CheckRangeCoverage(ctx, conn.ID, "2026-09-01", "2026-09-01")
	if err != nil {
		t.Fatalf("check coverage failed: %v", err)
	}
	if !covered {
		t.Fatal("expected day 2026-09-01 to be covered")
	}

	// Verify job progress fields updated
	updatedJob, err := store.GetHistorySyncJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updatedJob.CurrentDay != "2026-09-01" || updatedJob.PagesDone != 1 || updatedJob.RowsSeen != 15 {
		t.Fatalf("unexpected updated job: %+v", updatedJob)
	}
}

func TestStore_HistorySyncJobs_OverlapSafeTransactionWrites(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, conn := setupTestStore(t)

	// Set connection to MONITORING so IngestTransactionsBatchWithSource passes fence
	if _, err := store.DB().ExecContext(ctx, `UPDATE connections SET state = 'MONITORING' WHERE id = ?`, conn.ID); err != nil {
		t.Fatal(err)
	}

	items := []BatchTransactionItem{
		{
			Number:        "TXN001",
			Credit:        500000,
			Debit:         0,
			TransactionAt: "01/09/2026 10:00:00",
			EffectiveAt:   "01/09/2026 10:00:00",
			Description:   "Transfer from John",
		},
		{
			Number:        "TXN002",
			Credit:        0,
			Debit:         200000,
			TransactionAt: "01/09/2026 11:00:00",
			EffectiveAt:   "01/09/2026 11:00:00",
			Description:   "Payment to store",
		},
	}

	// Ingest with FILTER_SYNC source
	res1, err := store.IngestTransactionsBatchWithSource(ctx, conn.ID, conn.Generation, "***9999", items, false, "FILTER_SYNC")
	if err != nil {
		t.Fatalf("first ingestion failed: %v", err)
	}
	if res1.InsertedCount != 2 || res1.SkippedCount != 0 {
		t.Fatalf("expected 2 inserted, got: %+v", res1)
	}
	// FILTER_SYNC must not emit credit events or deliveries
	if len(res1.NewEvents) != 0 {
		t.Fatalf("expected 0 events for FILTER_SYNC, got %d", len(res1.NewEvents))
	}

	// Repeat ingestion for the same day (simulating runner restart from page 1)
	res2, err := store.IngestTransactionsBatchWithSource(ctx, conn.ID, conn.Generation, "***9999", items, false, "FILTER_SYNC")
	if err != nil {
		t.Fatalf("second ingestion failed: %v", err)
	}
	if res2.InsertedCount != 0 || res2.SkippedCount != 2 || res2.ConflictCount != 0 {
		t.Fatalf("expected 0 inserted and 2 skipped without conflict on replay, got: %+v", res2)
	}
}

func TestStore_HistorySyncJobs_RequeueRunningJobsOnShutdown(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, conn := setupTestStore(t)

	// Job 1 on current generation
	job1, _, err := store.CreateOrGetHistorySyncJob(ctx, conn.ID, conn.Generation, "2026-09-01", "2026-09-02")
	if err != nil {
		t.Fatal(err)
	}
	_, claimed1, err := store.ClaimNextHistorySyncJob(ctx, time.Now().UTC())
	if err != nil || !claimed1 {
		t.Fatal("failed to claim job 1")
	}

	// Bump connection generation in DB so job2 will be on new generation while job1 stays on old generation
	_, err = store.DB().ExecContext(ctx, `UPDATE connections SET generation = generation + 1 WHERE id = ?`, conn.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Job 2 created on new generation
	job2, _, err := store.CreateOrGetHistorySyncJob(ctx, conn.ID, conn.Generation+1, "2026-09-03", "2026-09-04")
	if err != nil {
		t.Fatal(err)
	}
	_, claimed2, err := store.ClaimNextHistorySyncJob(ctx, time.Now().UTC())
	if err != nil || !claimed2 {
		t.Fatal("failed to claim job 2")
	}

	// Now job2 is on current generation (gen+1), while job1 is on stale generation (gen)
	requeued, err := store.RequeueRunningHistorySyncJobs(ctx, "graceful shutdown test")
	if err != nil {
		t.Fatalf("RequeueRunningHistorySyncJobs failed: %v", err)
	}
	if requeued != 1 {
		t.Fatalf("expected 1 job requeued, got %d", requeued)
	}

	// Job 1 should be CANCELED due to stale generation
	j1, err := store.GetHistorySyncJob(ctx, job1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if j1.Status != HistoryJobStatusCanceled {
		t.Errorf("expected job 1 status CANCELED, got %s", j1.Status)
	}
	if j1.ErrorCode != "STALE_GENERATION" {
		t.Errorf("expected job 1 error code STALE_GENERATION, got %s", j1.ErrorCode)
	}

	// Job 2 should be QUEUED with WORKER_SHUTDOWN error code
	j2, err := store.GetHistorySyncJob(ctx, job2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if j2.Status != HistoryJobStatusQueued {
		t.Errorf("expected job 2 status QUEUED, got %s", j2.Status)
	}
	if j2.ErrorCode != "WORKER_SHUTDOWN" {
		t.Errorf("expected job 2 error code WORKER_SHUTDOWN, got %s", j2.ErrorCode)
	}
}

func TestStore_HistorySyncJobs_MaxAttemptsExceeded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, conn := setupTestStore(t)

	job, _, err := store.CreateOrGetHistorySyncJob(ctx, conn.ID, conn.Generation, "2026-09-01", "2026-09-02")
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	for i := 1; i <= 4; i++ {
		claimedJob, claimed, err := store.ClaimNextHistorySyncJob(ctx, now.Add(time.Duration(i)*time.Hour))
		if err != nil || !claimed {
			t.Fatalf("attempt %d: claim failed: %v", i, err)
		}
		if claimedJob.Attempts != i {
			t.Fatalf("attempt %d: expected attempts=%d, got %d", i, i, claimedJob.Attempts)
		}
		if err := store.RequeueHistorySyncJob(ctx, job.ID, "TRANSIENT_ERR", "temporary glitch", now.Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatalf("attempt %d: requeue failed: %v", i, err)
		}
	}

	claimedJob5, claimed, err := store.ClaimNextHistorySyncJob(ctx, now.Add(5*time.Hour))
	if err != nil || !claimed {
		t.Fatalf("attempt 5: claim failed: %v", err)
	}
	if claimedJob5.Attempts != 5 {
		t.Fatalf("attempt 5: expected attempts=5, got %d", claimedJob5.Attempts)
	}

	if err := store.RequeueHistorySyncJob(ctx, job.ID, "TRANSIENT_ERR", "glitch 5", now.Add(5*time.Hour)); err != nil {
		t.Fatalf("attempt 5 requeue error: %v", err)
	}

	finalJob, err := store.GetHistorySyncJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finalJob.Status != HistoryJobStatusFailed {
		t.Fatalf("expected job to be FAILED, got: %s", finalJob.Status)
	}
	if finalJob.ErrorCode != "MAX_ATTEMPTS_EXCEEDED" {
		t.Fatalf("expected error code MAX_ATTEMPTS_EXCEEDED, got: %s", finalJob.ErrorCode)
	}
}
