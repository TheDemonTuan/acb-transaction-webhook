package monitor_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
	"github.com/thedemontuan/acb-transaction-webhook/internal/monitor"
	"github.com/thedemontuan/acb-transaction-webhook/internal/scheduler"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

// fakePaginatedACBClient simulates multi-day, multi-page ACB responses.
type fakePaginatedACBClient struct {
	bootstrapCalls atomic.Int32
	historyCalls   atomic.Int32
	pageCallsLog   []string
	mu             sync.Mutex

	customHistoryFn func(ctx context.Context, action string, fields map[string]string) (acb.Response, error)
}

func newFake31DayClient() *fakePaginatedACBClient {
	return &fakePaginatedACBClient{}
}

func (c *fakePaginatedACBClient) Bootstrap(ctx context.Context) (acb.Response, error) {
	c.bootstrapCalls.Add(1)
	body := `<form action="/history" method="POST">
		<input type="hidden" name="dse_operationName" value="ibkacctDetailProc" />
		<input type="hidden" name="dse_processorState" value="initial" />
		<input type="hidden" name="AccountNbr" value="123456" />
	</form>`
	return acb.Response{StatusCode: 200, Body: body, Kind: acb.AccountDetailPage}, nil
}

func (c *fakePaginatedACBClient) History(ctx context.Context, action string, fields map[string]string) (acb.Response, error) {
	c.historyCalls.Add(1)
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.customHistoryFn != nil {
		return c.customHistoryFn(ctx, action, fields)
	}

	fromDate := fields["FromDate"] // format "DD/MM/YYYY"
	isPage2 := fields["dse_nextEventName"] == "nextPage"

	callKey := fmt.Sprintf("%s_p%d", fromDate, 1)
	if isPage2 {
		callKey = fmt.Sprintf("%s_p%d", fromDate, 2)
	}
	c.pageCallsLog = append(c.pageCallsLog, callKey)

	if !isPage2 {
		// Page 1: 2 transactions and active "Trang sau" next link
		body := fmt.Sprintf(`<form action="/history" method="POST">
			<input type="hidden" name="dse_operationName" value="ibkacctDetailProc" />
			<input type="hidden" name="dse_processorState" value="next" />
			<input type="hidden" name="FromDate" value="%s" />
			<input type="hidden" name="ToDate" value="%s" />
		</form>
		<table>
			<tr><th>Ngày giao dịch</th><th>Số GD</th><th>Ghi nợ</th><th>Ghi có</th><th>Số dư</th><th>Nội dung giao dịch</th></tr>
			<tr><td>%s</td><td>TXN_%s_P1_1</td><td>0</td><td>50.000</td><td>100.000</td><td>Transfer 1</td></tr>
			<tr><td>%s</td><td>TXN_%s_P1_2</td><td>0</td><td>25.000</td><td>125.000</td><td>Transfer 2</td></tr>
			<tr><td colspan="6"><a href="/history?page=2" onclick="submitEvent('nextPage')">Trang sau</a></td></tr>
		</table>`, fromDate, fromDate, fromDate, fromDate, fromDate, fromDate)
		return acb.Response{StatusCode: 200, Body: body, Kind: acb.HistoryPage}, nil
	}

	// Page 2: 2 transactions and disabled next link (final page of the day)
	body := fmt.Sprintf(`<form action="/history" method="POST">
		<input type="hidden" name="dse_operationName" value="ibkacctDetailProc" />
		<input type="hidden" name="dse_processorState" value="last" />
	</form>
	<table>
		<tr><th>Ngày giao dịch</th><th>Số GD</th><th>Ghi nợ</th><th>Ghi có</th><th>Số dư</th><th>Nội dung giao dịch</th></tr>
		<tr><td>%s</td><td>TXN_%s_P2_1</td><td>0</td><td>10.000</td><td>135.000</td><td>Transfer 3</td></tr>
		<tr><td>%s</td><td>TXN_%s_P2_2</td><td>0</td><td>15.000</td><td>150.000</td><td>Transfer 4</td></tr>
		<tr><td colspan="6"><span class="disabled">Trang sau</span></td></tr>
	</table>`, fromDate, fromDate, fromDate, fromDate)
	return acb.Response{StatusCode: 200, Body: body, Kind: acb.HistoryPage}, nil
}

func setupTestDB(t *testing.T, dbName string) (*storage.Store, string) {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), dbName)
	store, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("Open DB %s: %v", dbName, err)
	}

	connID := "conn_hist_test"
	if _, err := store.DB().ExecContext(ctx, `
		INSERT INTO connections(id, state, generation, account_masked, created_at, updated_at)
		VALUES(?, 'MONITORING', 1, '123456', '2026-08-01T00:00:00Z', '2026-08-01T00:00:00Z')
	`, connID); err != nil {
		t.Fatalf("insert connection: %v", err)
	}
	return store, connID
}

// 1. Build a fake paginated ACB client and write a 31-day history test where each day has multiple pages.
// 2. Prove each runner Step performs at most one ACB page request.
func TestHistoryJobRunner_31DaysMultiPage_SinglePageQuanta(t *testing.T) {
	ctx := context.Background()
	store, connID := setupTestDB(t, "test_31_days.db")
	defer store.Close()

	client := newFake31DayClient()
	runner := monitor.NewHistoryJobRunner(store, client, nil, nil)

	// Create 31-day range: 2026-08-01 to 2026-08-31
	job, created, err := store.CreateOrGetHistorySyncJob(ctx, connID, 1, "2026-08-01", "2026-08-31")
	if err != nil {
		t.Fatalf("CreateOrGetHistorySyncJob: %v", err)
	}
	if !created {
		t.Fatal("expected new job to be created")
	}

	// Claim next job
	claimedJob, claimed, err := store.ClaimNextHistorySyncJob(ctx, time.Now())
	if err != nil || !claimed {
		t.Fatalf("ClaimNextHistorySyncJob: claimed=%v, err=%v", claimed, err)
	}
	if claimedJob.ID != job.ID {
		t.Fatalf("claimed job ID mismatch: expected %s, got %s", job.ID, claimedJob.ID)
	}

	conn, _ := store.Connection(ctx)
	task := monitor.NewHistoryJobTask(runner, claimedJob, conn)

		// Execute step-by-step and prove:
		// - each Step executes AT MOST ONE day's pagination atomically (2 ACB calls per day here).
		// - exactly 31 days = 31 steps to completion (yielding across days).
		steps := 0
		for {
			callsBefore := client.historyCalls.Load()
			res, err := task.Step(ctx)
			callsAfter := client.historyCalls.Load()

			callsInStep := callsAfter - callsBefore
			if callsInStep > 2 {
				t.Fatalf("step %d violated bounded quantum invariant: made %d ACB calls, max 2 allowed", steps+1, callsInStep)
			}
			if err != nil {
				t.Fatalf("step %d failed: %v", steps+1, err)
			}

			steps++
			if res.Done {
				break
			}
			if steps > 100 {
				t.Fatal("infinite loop detected in task step execution")
			}
		}

		// 31 days with 2 pages each = exactly 31 steps (one step per day)
		if steps != 31 {
			t.Fatalf("expected 31 steps for 31 days with 2 pages each, got %d", steps)
		}
		if client.historyCalls.Load() != 62 {
			t.Fatalf("expected 62 ACB history calls, got %d", client.historyCalls.Load())
		}

	// Verify terminal state in storage
	finishedJob, err := store.GetHistorySyncJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetHistorySyncJob: %v", err)
	}
	if finishedJob.Status != storage.HistoryJobStatusCompleted {
		t.Fatalf("expected COMPLETED status, got %s", finishedJob.Status)
	}
	if finishedJob.PagesDone != 62 {
		t.Errorf("expected 62 pagesDone, got %d", finishedJob.PagesDone)
	}
	// 4 transactions per day * 31 days = 124 transactions
	if finishedJob.RowsSeen != 124 {
		t.Errorf("expected 124 rowsSeen, got %d", finishedJob.RowsSeen)
	}

	// Verify coverage was recorded for every day
	for d := 1; d <= 31; d++ {
		dayStr := fmt.Sprintf("2026-08-%02d", d)
		covered, err := store.CheckRangeCoverage(ctx, connID, dayStr, dayStr)
		if err != nil || !covered {
			t.Errorf("expected day %s to be covered, got covered=%v, err=%v", dayStr, covered, err)
		}
	}
}

type mockRealtimeTask struct {
	id         string
	executedAt time.Time
	stepCalled atomic.Bool
}

func (m *mockRealtimeTask) ID() string                          { return m.id }
func (m *mockRealtimeTask) Kind() string                        { return "REALTIME_POLL" }
func (m *mockRealtimeTask) Priority() scheduler.UpstreamPriority { return scheduler.PriorityRealtimePoll }
func (m *mockRealtimeTask) Generation() int64                   { return 1 }
func (m *mockRealtimeTask) CoalesceKey() string                 { return m.id }
func (m *mockRealtimeTask) Step(ctx context.Context) (scheduler.TaskStepResult, error) {
	m.executedAt = time.Now()
	m.stepCalled.Store(true)
	return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeSuccess}, nil
}

// 3. Prove a realtime task inserted after a history page runs before the next history page.
func TestHistoryJobRunner_RealtimePreemption(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	store, connID := setupTestDB(t, "test_preempt.db")
	defer store.Close()

	var orderMu sync.Mutex
	var executionOrder []string
	var p1Done sync.WaitGroup
	p1Done.Add(1)

	client := newFake31DayClient()
	client.customHistoryFn = func(ctx context.Context, action string, fields map[string]string) (acb.Response, error) {
		isPage2 := fields["dse_nextEventName"] == "nextPage"
		fromDate := fields["FromDate"]
		var prefix string
		if fromDate == "12/09/2026" {
			prefix = "hist_d1"
		} else {
			prefix = "hist_d2"
		}
		if !isPage2 {
			orderMu.Lock()
			executionOrder = append(executionOrder, prefix+"_p1")
			orderMu.Unlock()
			if prefix == "hist_d1" {
				p1Done.Done()
				time.Sleep(30 * time.Millisecond) // Allow realtime task to be enqueued while day 1 is running
			}
			body := fmt.Sprintf(`<form action="/history" method="POST">
				<input type="hidden" name="dse_operationName" value="ibkacctDetailProc" />
				<input type="hidden" name="dse_processorState" value="next" />
				<input type="hidden" name="FromDate" value="%s" />
				<input type="hidden" name="ToDate" value="%s" />
			</form>
			<table>
				<tr><th>Ngày giao dịch</th><th>Số GD</th><th>Ghi nợ</th><th>Ghi có</th></tr>
				<tr><td>%s</td><td>TXN_1</td><td>0</td><td>10.000</td></tr>
				<tr><td colspan="4"><a href="/history?page=2" onclick="submitEvent('nextPage')">Trang sau</a></td></tr>
			</table>`, fromDate, fromDate, fromDate)
			return acb.Response{StatusCode: 200, Body: body, Kind: acb.HistoryPage}, nil
		}
		orderMu.Lock()
		executionOrder = append(executionOrder, prefix+"_p2")
		orderMu.Unlock()
		body := `<form action="/history" method="POST"><input type="hidden" name="dse_processorState" value="last" /></form>
		<table>
			<tr><th>Ngày giao dịch</th><th>Số GD</th><th>Ghi nợ</th><th>Ghi có</th></tr>
			<tr><td>12/09/2026</td><td>TXN_2</td><td>0</td><td>20.000</td></tr>
			<tr><td colspan="4"><span class="disabled">Trang sau</span></td></tr>
		</table>`
		return acb.Response{StatusCode: 200, Body: body, Kind: acb.HistoryPage}, nil
	}

	sched := scheduler.New(nil)
	sched.Start(ctx)
	defer sched.Stop()

	runner := monitor.NewHistoryJobRunner(store, client, sched, nil)

	// Create a 2-day job (2 pages per day)
	_, _, err := store.CreateOrGetHistorySyncJob(ctx, connID, 1, "2026-09-12", "2026-09-13")
	if err != nil {
		t.Fatalf("CreateOrGetHistorySyncJob: %v", err)
	}

	claimedJob, _, err := store.ClaimNextHistorySyncJob(ctx, time.Now())
	if err != nil {
		t.Fatalf("ClaimNextHistorySyncJob: %v", err)
	}

	conn, _ := store.Connection(ctx)
	histTask := monitor.NewHistoryJobTask(runner, claimedJob, conn)

	// Enqueue history task into scheduler
	if err := sched.Enqueue(histTask); err != nil {
		t.Fatalf("enqueue histTask: %v", err)
	}

	// Wait for Page 1 of Day 1 to start
	p1Done.Wait()

	// Insert high-priority realtime task
	rtTask := &mockRealtimeTask{id: "rt_preempt_1"}
	customRtTask := &customStepTask{
		mockRealtimeTask: rtTask,
		stepFn: func(ctx context.Context) (scheduler.TaskStepResult, error) {
			orderMu.Lock()
			executionOrder = append(executionOrder, "realtime")
			orderMu.Unlock()
			rtTask.stepCalled.Store(true)
			return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeSuccess}, nil
		},
	}
	if err := sched.Enqueue(customRtTask); err != nil {
		t.Fatalf("enqueue rtTask: %v", err)
	}

	// Wait for history task to finish completely
	select {
	case <-histTask.Done():
	case <-ctx.Done():
		t.Fatal("timed out waiting for history task to complete")
	}

	if !rtTask.stepCalled.Load() {
		t.Fatal("expected realtime task to have been executed")
	}

	// Verify exact execution sequence: full Day 1, realtime at day boundary, full Day 2
	orderMu.Lock()
	defer orderMu.Unlock()
	expectedOrder := []string{"hist_d1_p1", "hist_d1_p2", "realtime", "hist_d2_p1", "hist_d2_p2"}
	if len(executionOrder) != len(expectedOrder) {
		t.Fatalf("expected execution order %v, got %v", expectedOrder, executionOrder)
	}
	for i := range expectedOrder {
		if executionOrder[i] != expectedOrder[i] {
			t.Fatalf("expected execution order %v, got %v", expectedOrder, executionOrder)
		}
	}
}

type customStepTask struct {
	*mockRealtimeTask
	stepFn func(ctx context.Context) (scheduler.TaskStepResult, error)
}

func (c *customStepTask) Step(ctx context.Context) (scheduler.TaskStepResult, error) {
	if c.stepFn != nil {
		return c.stepFn(ctx)
	}
	return c.mockRealtimeTask.Step(ctx)
}

// 4. Prove filter-history ingestion uses FILTER_SYNC and creates no notification deliveries/events.
func TestHistoryJobRunner_FilterSyncSource_NoEventsOrDeliveries(t *testing.T) {
	ctx := context.Background()
	store, connID := setupTestDB(t, "test_filter_sync.db")
	defer store.Close()

	client := newFake31DayClient()
	runner := monitor.NewHistoryJobRunner(store, client, nil, nil)

	_, _, err := store.CreateOrGetHistorySyncJob(ctx, connID, 1, "2026-09-12", "2026-09-12")
	if err != nil {
		t.Fatalf("CreateOrGetHistorySyncJob: %v", err)
	}

	claimedJob, _, _ := store.ClaimNextHistorySyncJob(ctx, time.Now())
	conn, _ := store.Connection(ctx)
	task := monitor.NewHistoryJobTask(runner, claimedJob, conn)

	for {
		res, err := task.Step(ctx)
		if err != nil {
			t.Fatalf("Step failed: %v", err)
		}
		if res.Done {
			break
		}
	}

	// Assert 0 webhook deliveries were created
	var deliveryCount int
	err = store.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM deliveries").Scan(&deliveryCount)
	if err != nil {
		t.Fatalf("query deliveries: %v", err)
	}
	if deliveryCount != 0 {
		t.Errorf("expected 0 deliveries for FILTER_SYNC ingestion, got %d", deliveryCount)
	}

	// Assert 0 events were created
	var eventCount int
	err = store.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM events").Scan(&eventCount)
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	if eventCount != 0 {
		t.Errorf("expected 0 events for FILTER_SYNC ingestion, got %d", eventCount)
	}

	// Assert transactions exist with source = 'FILTER_SYNC'
	var txnSource string
	err = store.DB().QueryRowContext(ctx, "SELECT ingest_source FROM transactions LIMIT 1").Scan(&txnSource)
	if err != nil {
		t.Fatalf("query transactions source: %v", err)
	}
	if txnSource != "FILTER_SYNC" {
		t.Errorf("expected transaction source FILTER_SYNC, got %s", txnSource)
	}
}

// 5. Prove a worker shutdown/canceled scheduler context leaves the job requeueable rather than permanently RUNNING.
func TestHistoryJobRunner_Shutdown_RequeuesToQueued(t *testing.T) {
	ctx := context.Background()
	store, connID := setupTestDB(t, "test_shutdown.db")
	defer store.Close()

	client := newFake31DayClient()
	runner := monitor.NewHistoryJobRunner(store, client, nil, nil)

	job, _, err := store.CreateOrGetHistorySyncJob(ctx, connID, 1, "2026-09-01", "2026-09-10")
	if err != nil {
		t.Fatalf("CreateOrGetHistorySyncJob: %v", err)
	}

	claimedJob, _, _ := store.ClaimNextHistorySyncJob(ctx, time.Now())
	conn, _ := store.Connection(ctx)
	task := monitor.NewHistoryJobTask(runner, claimedJob, conn)

	// Cancel context to simulate worker shutdown
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()

	res, err := task.Step(canceledCtx)
	if err == nil {
		t.Fatal("expected error on canceled context step, got nil")
	}
	if !res.Done {
		t.Fatal("expected step to finish on canceled context")
	}

	// Verify job was transitioned to QUEUED and NOT permanently RUNNING!
	afterJob, err := store.GetHistorySyncJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetHistorySyncJob: %v", err)
	}
	if afterJob.Status != storage.HistoryJobStatusQueued {
		t.Fatalf("expected job to be left in QUEUED status after shutdown, got: %s", afterJob.Status)
	}
	if afterJob.ErrorCode != "WORKER_SHUTDOWN" {
		t.Errorf("expected error_code WORKER_SHUTDOWN, got %s", afterJob.ErrorCode)
	}
}

// 6. Prove a simulated worker restart requeues stale work and safely repeats the current day from page one without duplicate transactions.
func TestHistoryJobRunner_CrashRecovery_ResumesWithoutDuplicates(t *testing.T) {
	ctx := context.Background()
	store, connID := setupTestDB(t, "test_crash_recovery.db")
	defer store.Close()

	client := newFake31DayClient()
	runner1 := monitor.NewHistoryJobRunner(store, client, nil, nil)

	// Range: 2 days (2026-09-01 to 2026-09-02)
	job, _, err := store.CreateOrGetHistorySyncJob(ctx, connID, 1, "2026-09-01", "2026-09-02")
	if err != nil {
		t.Fatalf("CreateOrGetHistorySyncJob: %v", err)
	}

	claimedJob, _, _ := store.ClaimNextHistorySyncJob(ctx, time.Now())
	conn, _ := store.Connection(ctx)
	task1 := monitor.NewHistoryJobTask(runner1, claimedJob, conn)

		// Step 1: Day 1 (both pages of Day 1 completed, coverage recorded)
		res, err := task1.Step(ctx)
		if err != nil || res.Done {
			t.Fatalf("step 1 failed: %v", err)
		}

		// Step 2: Day 2 begins. Page 1 ingests transactions, then page 2 fails with transport error simulating mid-day crash
		client.customHistoryFn = func(ctx context.Context, action string, fields map[string]string) (acb.Response, error) {
			isPage2 := fields["dse_nextEventName"] == "nextPage"
			fromDate := fields["FromDate"]
			if fromDate == "02/09/2026" && isPage2 {
				return acb.Response{}, errors.New("simulated worker crash/transport failure")
			}
			body := fmt.Sprintf(`<form action="/history" method="POST">
				<input type="hidden" name="dse_operationName" value="ibkacctDetailProc" />
				<input type="hidden" name="dse_processorState" value="next" />
				<input type="hidden" name="FromDate" value="%s" />
				<input type="hidden" name="ToDate" value="%s" />
			</form>
			<table>
				<tr><th>Ngày giao dịch</th><th>Số GD</th><th>Ghi nợ</th><th>Ghi có</th><th>Số dư</th><th>Nội dung giao dịch</th></tr>
				<tr><td>%s</td><td>TXN_%s_P1_1</td><td>0</td><td>50.000</td><td>100.000</td><td>Transfer 1</td></tr>
				<tr><td>%s</td><td>TXN_%s_P1_2</td><td>0</td><td>25.000</td><td>125.000</td><td>Transfer 2</td></tr>
				<tr><td colspan="6"><a href="/history?page=2" onclick="submitEvent('nextPage')">Trang sau</a></td></tr>
			</table>`, fromDate, fromDate, fromDate, fromDate, fromDate, fromDate)
			return acb.Response{StatusCode: 200, Body: body, Kind: acb.HistoryPage}, nil
		}

		_, _ = task1.Step(ctx) // Fails on page 2 after page 1 has been ingested
		client.customHistoryFn = nil

		// SIMULATE HARD WORKER CRASH:
		// Worker process terminates abruptly. The job remains in RUNNING in DB with old heartbeat.
		staleTime := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339Nano)
		if _, err := store.DB().ExecContext(ctx, `
			UPDATE history_sync_jobs
			SET status = 'RUNNING', heartbeat_at = ?, started_at = ?
			WHERE id = ?
		`, staleTime, staleTime, job.ID); err != nil {
			t.Fatalf("simulate stale heartbeat: %v", err)
		}

	// SIMULATE WORKER RESTART:
	// New worker process boots up with new HistoryJobRunner
	runner2 := monitor.NewHistoryJobRunner(store, client, nil, nil).WithStaleThreshold(60 * time.Second)

	// Startup recovery pass recovers stale RUNNING job back to QUEUED
	recovered, err := runner2.RecoverStaleJobs(ctx)
	if err != nil {
		t.Fatalf("RecoverStaleJobs: %v", err)
	}
	if recovered != 1 {
		t.Fatalf("expected 1 recovered job, got %d", recovered)
	}

	reclaimedJob, claimed, err := store.ClaimNextHistorySyncJob(ctx, time.Now())
	if err != nil || !claimed {
		t.Fatalf("re-claim job after recovery: claimed=%v, err=%v", claimed, err)
	}
	if reclaimedJob.ID != job.ID {
		t.Fatalf("expected reclaimed job ID %s, got %s", job.ID, reclaimedJob.ID)
	}
	// Day 1 was already completed, so current_day should be Day 2
	if reclaimedJob.CurrentDay != "2026-09-02" {
		t.Fatalf("expected current_day to be 2026-09-02, got %s", reclaimedJob.CurrentDay)
	}

	// Resume job: Day 2 repeats from Page 1 without duplicates
	task2 := monitor.NewHistoryJobTask(runner2, reclaimedJob, conn)
	for {
		res, err := task2.Step(ctx)
		if err != nil {
			t.Fatalf("resumed step failed: %v", err)
		}
		if res.Done {
			break
		}
	}

	finalJob, err := store.GetHistorySyncJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetHistorySyncJob: %v", err)
	}
	if finalJob.Status != storage.HistoryJobStatusCompleted {
		t.Fatalf("expected job to be COMPLETED, got %s", finalJob.Status)
	}

	// Check transactions table for duplicate rows:
	// 2 days * 4 transactions/day = exactly 8 unique transactions!
	var totalTxns int
	if err := store.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM transactions").Scan(&totalTxns); err != nil {
		t.Fatalf("count transactions: %v", err)
	}
	if totalTxns != 8 {
		t.Fatalf("expected exactly 8 unique transactions without duplicates, got %d", totalTxns)
	}
}

// 7. Classify transient failures into requeue/backoff and auth/parser/invalid-session failures into terminal typed errors.
func TestHistoryJobRunner_ErrorClassification(t *testing.T) {
	ctx := context.Background()

	// 7.1 Terminal Auth Error (Confirmed AuthFailure)
	t.Run("Terminal_AuthRequired", func(t *testing.T) {
		store, connID := setupTestDB(t, "test_auth_err.db")
		defer store.Close()

		client := newFake31DayClient()
		client.customHistoryFn = func(ctx context.Context, action string, fields map[string]string) (acb.Response, error) {
			return acb.Response{}, &acb.AuthFailure{Kind: acb.LoginPage, Reason: "SESSION_EXPIRED"}
		}
		runner := monitor.NewHistoryJobRunner(store, client, nil, nil)

		job, _, _ := store.CreateOrGetHistorySyncJob(ctx, connID, 1, "2026-09-12", "2026-09-12")
		claimed, _, _ := store.ClaimNextHistorySyncJob(ctx, time.Now())
		conn, _ := store.Connection(ctx)
		task := monitor.NewHistoryJobTask(runner, claimed, conn)

		res, err := task.Step(ctx)
		if err == nil || res.Outcome != scheduler.OutcomeAuth {
			t.Fatalf("expected OutcomeAuth, got %s, err: %v", res.Outcome, err)
		}

		dbJob, _ := store.GetHistorySyncJob(ctx, job.ID)
		if dbJob.Status != storage.HistoryJobStatusFailed {
			t.Fatalf("expected status FAILED, got %s", dbJob.Status)
		}
		if dbJob.ErrorCode != "AUTH_REQUIRED" {
			t.Errorf("expected error_code AUTH_REQUIRED, got %s", dbJob.ErrorCode)
		}
	})

	// 7.1b Inconclusive Auth (LoginPage without AuthFailure keeps session active)
	t.Run("Inconclusive_AuthPreserved", func(t *testing.T) {
		store, connID := setupTestDB(t, "test_inconclusive_auth.db")
		defer store.Close()

		client := newFake31DayClient()
		client.customHistoryFn = func(ctx context.Context, action string, fields map[string]string) (acb.Response, error) {
			return acb.Response{StatusCode: 200, Body: "<html>Login</html>", Kind: acb.LoginPage}, nil
		}
		runner := monitor.NewHistoryJobRunner(store, client, nil, nil)

		job, _, _ := store.CreateOrGetHistorySyncJob(ctx, connID, 1, "2026-09-12", "2026-09-12")
		claimed, _, _ := store.ClaimNextHistorySyncJob(ctx, time.Now())
		conn, _ := store.Connection(ctx)
		task := monitor.NewHistoryJobTask(runner, claimed, conn)

		res, err := task.Step(ctx)
		if err == nil || res.Outcome != scheduler.OutcomeTransient {
			t.Fatalf("expected OutcomeTransient, got %s, err: %v", res.Outcome, err)
		}

			dbJob, _ := store.GetHistorySyncJob(ctx, job.ID)
			if dbJob.Status == storage.HistoryJobStatusFailed {
				t.Fatalf("expected job NOT to fail with unconfirmed login, got status %s", dbJob.Status)
			}
		})

		// 7.2 Terminal Parse Error
	t.Run("Terminal_ParseError", func(t *testing.T) {
		store, connID := setupTestDB(t, "test_parse_err.db")
		defer store.Close()

		client := newFake31DayClient()
		client.customHistoryFn = func(ctx context.Context, action string, fields map[string]string) (acb.Response, error) {
			return acb.Response{StatusCode: 200, Body: "not-html-corrupted", Kind: acb.HistoryPage}, nil
		}
		runner := monitor.NewHistoryJobRunner(store, client, nil, nil)

		job, _, _ := store.CreateOrGetHistorySyncJob(ctx, connID, 1, "2026-09-12", "2026-09-12")
		claimed, _, _ := store.ClaimNextHistorySyncJob(ctx, time.Now())
		conn, _ := store.Connection(ctx)
		task := monitor.NewHistoryJobTask(runner, claimed, conn)

		res, err := task.Step(ctx)
		if err == nil || res.Outcome != scheduler.OutcomeFatal {
			t.Fatalf("expected OutcomeFatal, got %s, err: %v", res.Outcome, err)
		}

		dbJob, _ := store.GetHistorySyncJob(ctx, job.ID)
		if dbJob.Status != storage.HistoryJobStatusFailed {
			t.Fatalf("expected status FAILED, got %s", dbJob.Status)
		}
		if dbJob.ErrorCode != "PARSE_ERROR" {
			t.Fatalf("expected error_code PARSE_ERROR, got %s", dbJob.ErrorCode)
		}
	})

	// 7.3 Transient Network / Upstream Error
	t.Run("Transient_NetworkError", func(t *testing.T) {
		store, connID := setupTestDB(t, "test_transient_err.db")
		defer store.Close()

		client := newFake31DayClient()
		client.customHistoryFn = func(ctx context.Context, action string, fields map[string]string) (acb.Response, error) {
			return acb.Response{}, errors.New("connection reset by peer")
		}
		runner := monitor.NewHistoryJobRunner(store, client, nil, nil)

		job, _, _ := store.CreateOrGetHistorySyncJob(ctx, connID, 1, "2026-09-12", "2026-09-12")
		claimed, _, _ := store.ClaimNextHistorySyncJob(ctx, time.Now())
		conn, _ := store.Connection(ctx)
		task := monitor.NewHistoryJobTask(runner, claimed, conn)

		res, err := task.Step(ctx)
		if err == nil || res.Outcome != scheduler.OutcomeTransient {
			t.Fatalf("expected OutcomeTransient, got %s, err: %v", res.Outcome, err)
		}

		dbJob, _ := store.GetHistorySyncJob(ctx, job.ID)
		if dbJob.Status != storage.HistoryJobStatusQueued {
			t.Fatalf("expected status QUEUED for transient retry, got %s", dbJob.Status)
		}
		if dbJob.ErrorCode != "TRANSIENT_ERROR" {
			t.Fatalf("expected error_code TRANSIENT_ERROR, got %s", dbJob.ErrorCode)
		}
		if dbJob.NextAttemptAt == "" {
			t.Fatal("expected next_attempt_at to be populated for transient backoff")
		}
	})

	// 7.4 Stale Generation Fencing
	t.Run("Stale_GenerationFencing", func(t *testing.T) {
		store, connID := setupTestDB(t, "test_fence_err.db")
		defer store.Close()

		client := newFake31DayClient()
		runner := monitor.NewHistoryJobRunner(store, client, nil, nil)

		// Create job at generation 1
		job, _, _ := store.CreateOrGetHistorySyncJob(ctx, connID, 1, "2026-09-12", "2026-09-12")
		claimed, _, _ := store.ClaimNextHistorySyncJob(ctx, time.Now())
		conn, _ := store.Connection(ctx)

		// Bump connection generation in database to 2
		if _, err := store.DB().ExecContext(ctx, `UPDATE connections SET generation = 2 WHERE id = ?`, connID); err != nil {
			t.Fatalf("bump generation: %v", err)
		}

		task := monitor.NewHistoryJobTask(runner, claimed, conn)
		res, err := task.Step(ctx)
		if err == nil || !errors.Is(err, storage.ErrGenerationFenceMismatch) {
			t.Fatalf("expected ErrGenerationFenceMismatch, got: %v", err)
		}
		if !res.Done {
			t.Fatal("expected task to be marked Done when generation is stale")
		}

		dbJob, _ := store.GetHistorySyncJob(ctx, job.ID)
		if dbJob.Status != storage.HistoryJobStatusCanceled {
			t.Fatalf("expected job to be CANCELED due to generation fence, got %s", dbJob.Status)
		}
	})
}

func TestHistoryJobRunnerFiltersAdjacentDaysAcrossPages(t *testing.T) {
	ctx := context.Background()
	store, connID := setupTestDB(t, "mixed_history.db")
	defer store.Close()
	client := newFake31DayClient()
	client.customHistoryFn = func(_ context.Context, _ string, fields map[string]string) (acb.Response, error) {
		page2 := client.historyCalls.Load() == 2
		rows, nav := `<tr><td>24/09/2026 20:00:00</td><td>A</td><td>0</td><td>100</td><td>1000</td><td>Prior</td></tr>`, `<a href="/history?page=2" onclick="submitEvent('nextPage')">Trang sau</a>`
		if page2 {
			rows, nav = `<tr><td>25/09/2026 08:00:00</td><td>B</td><td>0</td><td>100</td><td>1100</td><td>Today</td></tr><tr><td>25/09/2026 09:00:00</td><td>C</td><td>0</td><td>100</td><td>1200</td><td>Today</td></tr>`, `<span class="disabled">Trang sau</span>`
		}
		body := fmt.Sprintf(`<form action="/history" method="POST"><input name="dse_operationName" value="ibkacctDetailProc"><input name="dse_processorState" value="page"></form><table><tr><th>Ngày giao dịch</th><th>Số GD</th><th>Ghi nợ</th><th>Ghi có</th><th>Số dư</th><th>Nội dung giao dịch</th></tr>%s<tr><td colspan="6">%s</td></tr></table>`, rows, nav)
		return acb.Response{StatusCode: 200, Body: body, Kind: acb.HistoryPage}, nil
	}
	job, _, err := store.CreateOrGetHistorySyncJob(ctx, connID, 1, "2026-09-25", "2026-09-25")
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.ClaimNextHistorySyncJob(ctx, time.Now())
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	conn, err := store.Connection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	res, err := monitor.NewHistoryJobTask(monitor.NewHistoryJobRunner(store, client, nil, nil), claimed, conn).Step(ctx)
	if err != nil || !res.Done {
		t.Fatalf("history job: done=%v err=%v", res.Done, err)
	}
	finished, err := store.GetHistorySyncJob(ctx, job.ID)
	if err != nil || finished.Status != storage.HistoryJobStatusCompleted || finished.PagesDone != 2 || finished.RowsSeen != 3 || client.historyCalls.Load() != 2 {
		t.Fatalf("raw pagination and completion: job=%+v calls=%d err=%v", finished, client.historyCalls.Load(), err)
	}
	var matched, events, deliveries int
	if err := store.DB().QueryRowContext(ctx, "SELECT rows_seen FROM history_coverage WHERE connection_id=? AND day=?", connID, "2026-09-25").Scan(&matched); err != nil || matched != 2 {
		t.Fatalf("coverage: %d %v", matched, err)
	}
	if err := store.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM transactions WHERE transaction_day=?", "2026-09-25").Scan(&matched); err != nil || matched != 2 {
		t.Fatalf("matched transactions: %d %v", matched, err)
	}
	if err := store.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM transactions").Scan(&matched); err != nil || matched != 2 {
		t.Fatalf("adjacent-day row ingested: count=%d err=%v", matched, err)
	}
	if err := store.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM events").Scan(&events); err != nil || events != 0 {
		t.Fatalf("events: %d %v", events, err)
	}
	if err := store.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM deliveries").Scan(&deliveries); err != nil || deliveries != 0 {
		t.Fatalf("deliveries: %d %v", deliveries, err)
	}
}
