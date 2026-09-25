package monitor

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func newTestCatchUpTask(m *Monitor, connectionID string, generation int64) *CatchUpTask {
	task := newCatchUpTask(m, connectionID, generation, "test", "")
	task.recoveryReady = true
	return task
}

type catchUpInterleaveMockClient struct {
	historyCalls atomic.Int32
	historyLogMu sync.Mutex
	historyLog   []string
}

func (m *catchUpInterleaveMockClient) Bootstrap(ctx context.Context) (acb.Response, error) {
	body := `<form action="/history" method="POST">
		<input type="hidden" name="dse_operationName" value="op1" />
		<input type="hidden" name="dse_processorState" value="ps1" />
		<input type="hidden" name="AccountNbr" value="123456" />
	</form>`
	return acb.Response{StatusCode: 200, Body: body, Kind: acb.AccountDetailPage}, nil
}

func (m *catchUpInterleaveMockClient) History(ctx context.Context, endpoint string, fields map[string]string) (acb.Response, error) {
	call := int(m.historyCalls.Add(1))
	isPage2 := fields["dse_nextEventName"] == "nextPage"
	page := "page1"
	if isPage2 {
		page = "page2"
	}
	var kind string
	if fields["_explicitRange"] == "true" || (fields["_raw"] == "true" && (call == 2 || call == 5)) {
		kind = "catchup"
	} else {
		kind = "realtime"
	}
	label := fmt.Sprintf("%s_%s_%s", kind, fields["FromDate"], page)
	m.historyLogMu.Lock()
	m.historyLog = append(m.historyLog, label)
	m.historyLogMu.Unlock()

	var navRow string
	if kind == "catchup" && !isPage2 {
		navRow = `<tr><td colspan="6"><a href="/history?page=2" onclick="submitEvent('nextPage')">Trang sau</a></td></tr>`
	} else {
		navRow = `<tr><td colspan="6"><span class="disabled">Trang sau</span></td></tr>`
	}

	body := fmt.Sprintf(`
	<form action="/history" method="POST">
		<input type="hidden" name="dse_operationName" value="op1" />
		<input type="hidden" name="dse_processorState" value="ps_next" />
		<input type="hidden" name="AccountNbr" value="123456" />
	</form>
	<table>
		<tr><th>Số GD</th><th>Ngày giao dịch</th><th>Ghi nợ</th><th>Ghi có</th><th>Số dư</th><th>Nội dung giao dịch</th></tr>
		<tr><td>TXN_%d</td><td>%s</td><td>0</td><td>100,000</td><td>1,000,000</td><td>Transfer %d</td></tr>
		%s
	</table>
		`, call, fields["FromDate"], call, navRow)

	return acb.Response{StatusCode: 200, Body: body, Kind: acb.HistoryPage}, nil
}

func TestCatchUpTaskFiltersAdjacentDaysAcrossPages(t *testing.T) {
	ctx := context.Background()
	store, conn, _ := setupRecoveryTestEnv(t, "mixed_recovery.db")
	defer store.Close()
	client := newRecoveryMockBankClient()
	client.pages["25/09/2026"] = []string{
		buildHistoryHTML([]acb.Transaction{{Number: "A", TransactionAt: "24/09/2026 20:00:00", Credit: 100}}, true),
		buildHistoryHTML([]acb.Transaction{{Number: "B", TransactionAt: "25/09/2026 08:00:00", Credit: 100}, {Number: "C", TransactionAt: "25/09/2026 09:00:00", Credit: 100}}, false),
	}
	mon := New(store, client, 5*time.Second, 15*time.Second)
	task := newTestCatchUpTask(mon, conn.ID, conn.Generation)
	task.fromDate, task.toDate = "2026-09-25", "2026-09-25"
	task.currentDay, _ = time.Parse("2006-01-02", task.fromDate)
	task.initialized = true
	res, err := task.Step(ctx)
	if err != nil || !res.Done {
		t.Fatalf("mixed-day recovery: done=%v err=%v", res.Done, err)
	}
	if len(client.historyCalls) != 2 {
		t.Fatalf("page without matching rows must advance pagination: %d requests", len(client.historyCalls))
	}
	txns, err := store.ListTransactions(ctx, 10)
	if err != nil || len(txns) != 2 {
		t.Fatalf("expected two matched transactions: count=%d err=%v", len(txns), err)
	}
	for _, txn := range txns {
		if !strings.HasPrefix(txn.TransactionAt, "2026-09-25") {
			t.Fatalf("wrong transaction day ingested: %s", txn.TransactionAt)
		}
	}
	var rows int
	if err := store.DB().QueryRowContext(ctx, "SELECT rows_seen FROM history_coverage WHERE connection_id=? AND day=?", conn.ID, "2026-09-25").Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("coverage must count matched rows: rows=%d err=%v", rows, err)
	}
	runs, err := store.ListPollRuns(ctx, 1)
	if err != nil || len(runs) != 1 || runs[0].Pages != 2 || runs[0].RowsSeen != 3 {
		t.Fatalf("poll must count original pages/rows: runs=%+v err=%v", runs, err)
	}
}

func TestCatchUpTaskMixedDayBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, lastPage string
		wantError      bool
	}{
		{name: "only adjacent days", lastPage: buildHistoryHTML([]acb.Transaction{{Number: "A2", TransactionAt: "24/09/2026", Credit: 100}}, false)},
		{name: "malformed final page", lastPage: buildHistoryHTML([]acb.Transaction{{Number: "BAD", TransactionAt: "invalid", Credit: 100}}, false), wantError: true},
		{name: "explicitly empty day", lastPage: `<form action="/history"><input name="dse_operationName" value="op1"><input name="dse_processorState" value="done"></form><table><tr><th>Số GD</th><th>Ngày giao dịch</th><th>Ghi nợ</th><th>Ghi có</th><th>Số dư</th><th>Nội dung giao dịch</th></tr><tr><td colspan="6">Không có giao dịch</td></tr></table>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store, conn, _ := setupRecoveryTestEnv(t, "boundary.db")
			defer store.Close()
			client := newRecoveryMockBankClient()
			if tc.name == "explicitly empty day" {
				client.pages["25/09/2026"] = []string{tc.lastPage}
			} else {
				client.pages["25/09/2026"] = []string{buildHistoryHTML([]acb.Transaction{{Number: "A", TransactionAt: "24/09/2026", Credit: 100}}, true), tc.lastPage}
			}
			task := newTestCatchUpTask(New(store, client, 5*time.Second, 15*time.Second), conn.ID, conn.Generation)
			task.fromDate, task.toDate = "2026-09-25", "2026-09-25"
			task.currentDay, _ = time.Parse("2006-01-02", task.fromDate)
			task.initialized = true
			_, err := task.Step(ctx)
			if tc.wantError != (err != nil) {
				t.Fatalf("unexpected recovery error: %v", err)
			}
			var coverage, transactions int
			err = store.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM history_coverage WHERE connection_id=? AND day=? AND status='COMPLETE' AND rows_seen=0", conn.ID, "2026-09-25").Scan(&coverage)
			if err != nil {
				t.Fatal(err)
			}
			err = store.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM transactions").Scan(&transactions)
			if err != nil {
				t.Fatal(err)
			}
			if transactions != 0 || (coverage == 1) == tc.wantError {
				t.Fatalf("invalid day durability: coverage=%d transactions=%d", coverage, transactions)
			}
			if tc.name == "only adjacent days" && len(client.historyCalls) != 2 {
				t.Fatalf("expected both pages, got %d", len(client.historyCalls))
			}
		})
	}
}

func TestCatchUpTaskRejectsAmbiguousEmptyDayWithoutCoverage(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "ambiguous_day.db"))
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
	day := time.Now().In(acb.DefaultLocation).AddDate(0, 0, -1).Format("2006-01-02")
	client := &mockBankClient{
		getResp:     acb.Response{StatusCode: 200, Kind: acb.HistoryPage, Body: `<form action="/history"><input name="dse_operationName" value="ibkacctDetailProc"><input name="dse_processorState" value="current"><input name="dse_sessionId" value="s1"></form>`},
		historyResp: acb.Response{StatusCode: 200, Kind: acb.HistoryPage, Body: `<form action="/history"><input name="dse_operationName" value="ibkacctDetailProc"><input name="dse_processorState" value="next"><input name="dse_sessionId" value="s1"></form><table><tr><th>Số GD</th><th>Ngày giao dịch</th><th>Ghi nợ</th><th>Ghi có</th></tr><tr><td colspan="4"><span class="disabled">Trang sau</span></td></tr></table>`},
	}
	mon := New(store, client, 5*time.Second, 15*time.Second)
	task := newTestCatchUpTask(mon, conn.ID, conn.Generation)
	task.fromDate, task.toDate = day, day
	_, err = task.Step(ctx)
	if err == nil || !strings.Contains(err.Error(), "empty transactions") {
		t.Fatalf("ambiguous day must fail closed: %v", err)
	}
	covered, err := store.CheckRangeCoverage(ctx, conn.ID, day, day)
	if err != nil || covered {
		t.Fatalf("ambiguous day marked covered: covered=%t err=%v", covered, err)
	}
}

func TestCatchUpTask_DayAtomicAndPreemptedAtDayBoundary(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "cu_yield.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	conn, _ := store.ConfigureConnection(ctx, "***1234")
	_, _ = store.DB().ExecContext(ctx, "UPDATE connections SET state='MONITORING'")

	client := &catchUpInterleaveMockClient{}
	mon := New(store, client, 5*time.Second, 15*time.Second)

	sched := mon.Scheduler()
	sched.Start(ctx)
	defer sched.Stop()

	// Step 1: Enqueue CatchUpTask (priority 50)
	cuTask := newTestCatchUpTask(mon, conn.ID, conn.Generation)
	if err := sched.Enqueue(cuTask); err != nil {
		t.Fatal(err)
	}

	// Wait until Day 1 catch-up has started
	deadline := time.Now().Add(2 * time.Second)
	for client.historyCalls.Load() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for catch-up day 1 to begin")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Enqueue RealtimeTask (priority 80) while Day 1 is executing or right as it yields
	rtExecuted := make(chan struct{}, 1)
	mon.WithEventNotifier(func(events []storage.EventNotification) {
		select {
		case rtExecuted <- struct{}{}:
		default:
		}
	})

	rtTask := NewRealtimeTask(mon, PriorityRealtimePoll, conn.ID, conn.Generation)
	if err := sched.Enqueue(rtTask); err != nil {
		t.Fatal(err)
	}

	// Realtime poll must execute before catch-up finishes completely
	select {
	case <-rtExecuted:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for realtime poll preemption")
	}

	// Wait for queue to drain
	deadline = time.Now().Add(5 * time.Second)
	for sched.TotalQueueDepth() > 0 || sched.IsBusy() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for scheduler queue to drain")
		}
		time.Sleep(10 * time.Millisecond)
	}

	client.historyLogMu.Lock()
	defer client.historyLogMu.Unlock()

	// Verification of atomic day processing and preemption:
	// Day 1 must complete both page1 and page2 atomically before realtime poll runs.
	// Realtime poll must run between Day 1 and Day 2.
	// Day 2 must then complete page1 and page2.
	if len(client.historyLog) != 5 {
		t.Fatalf("expected exactly 5 history calls (day1 p1, day1 p2, rt p1, day2 p1, day2 p2), got %d: %v",
			len(client.historyLog), client.historyLog)
	}
	if !strings.HasPrefix(client.historyLog[0], "catchup_") || !strings.HasSuffix(client.historyLog[0], "_page1") {
		t.Fatalf("call 0 should be day 1 page 1 catchup, got %s", client.historyLog[0])
	}
	if !strings.HasPrefix(client.historyLog[1], "catchup_") || !strings.HasSuffix(client.historyLog[1], "_page2") {
		t.Fatalf("call 1 should be day 1 page 2 catchup, got %s", client.historyLog[1])
	}
	if !strings.HasPrefix(client.historyLog[2], "realtime_") {
		t.Fatalf("call 2 should be realtime preemption, got %s", client.historyLog[2])
	}
	if !strings.HasPrefix(client.historyLog[3], "catchup_") || !strings.HasSuffix(client.historyLog[3], "_page1") {
		t.Fatalf("call 3 should be day 2 page 1 catchup, got %s", client.historyLog[3])
	}
	if !strings.HasPrefix(client.historyLog[4], "catchup_") || !strings.HasSuffix(client.historyLog[4], "_page2") {
		t.Fatalf("call 4 should be day 2 page 2 catchup, got %s", client.historyLog[4])
	}
}

func TestCatchUpTask_CheckpointAdvancesOnlyAfterFullDay(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "cu_checkpoint.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	conn, _ := store.ConfigureConnection(ctx, "***1234")
	_, _ = store.DB().ExecContext(ctx, "UPDATE connections SET state='MONITORING'")

	// Set initial checkpoint and coverage to 3 days ago, so there are 2 days to catch up: twoDaysAgo and oneDayAgo
	threeDaysAgo := time.Now().In(acb.DefaultLocation).AddDate(0, 0, -3).Format("2006-01-02")
	twoDaysAgo := time.Now().In(acb.DefaultLocation).AddDate(0, 0, -2).Format("2006-01-02")
	oneDayAgo := time.Now().In(acb.DefaultLocation).AddDate(0, 0, -1).Format("2006-01-02")
	_ = store.SaveCheckpoint(ctx, storage.Checkpoint{
		ConnectionID: conn.ID,
		ScanID:       "init",
		CoverageFrom: threeDaysAgo,
		CoverageTo:   threeDaysAgo,
	})
	_ = store.RecordCoveragePerDay(ctx, conn.ID, map[string]int{threeDaysAgo: 1})

	client := &catchUpInterleaveMockClient{}
	mon := New(store, client, 5*time.Second, 15*time.Second)

	task := newTestCatchUpTask(mon, conn.ID, conn.Generation)

	// Step 1: Processes full day of twoDaysAgo (2 pages) and completes twoDaysAgo.
	res1, err := task.Step(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res1.Done {
		t.Fatal("expected quantum yield at day boundary (Done=false), got Done=true")
	}

	// Checkpoint MUST have advanced to twoDaysAgo, but NOT to oneDayAgo yet!
	cp1, _ := store.GetCheckpoint(ctx, conn.ID)
	if cp1.CoverageTo != twoDaysAgo {
		t.Fatalf("checkpoint should have advanced to twoDaysAgo %s, got %s", twoDaysAgo, cp1.CoverageTo)
	}

	// Step 2: Processes full day of oneDayAgo (2 pages) and completes oneDayAgo.
	res2, err := task.Step(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Checkpoint MUST NOW advance to oneDayAgo!
	cp2, _ := store.GetCheckpoint(ctx, conn.ID)
	if cp2.CoverageTo != oneDayAgo {
		t.Fatalf("checkpoint should have advanced to completed day %s, got %s", oneDayAgo, cp2.CoverageTo)
	}
	_ = res2
}

func TestCatchUpTask_EmitsDeliveriesWithCatchUpSource(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "cu_source.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	conn, _ := store.ConfigureConnection(ctx, "***1234")
	_, _ = store.DB().ExecContext(ctx, "UPDATE connections SET state='MONITORING'")

	ep, err := store.CreateEndpointWithSecret(ctx, "Test Endpoint", "https://example.com/webhook")
	if err != nil {
		t.Fatal(err)
	}
	_ = store.SetEndpointStatus(ctx, ep.ID, "ACTIVE")

	client := &catchUpInterleaveMockClient{}
	mon := New(store, client, 5*time.Second, 15*time.Second)

	task := newTestCatchUpTask(mon, conn.ID, conn.Generation)

	res, err := task.Step(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = res

	// Check that deliveries were created in storage for the newly ingested credit
	deliveries, err := store.ListDeliveries(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(deliveries) == 0 {
		t.Fatal("expected delivery to be created for catch-up credit transaction")
	}
}

func TestCatchUpTask_ClampsCheckpointToInclusiveSevenDays(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "cu_clamp.db"))
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
	old := time.Now().In(acb.DefaultLocation).AddDate(0, 0, -30).Format("2006-01-02")
	if err := store.SaveCheckpoint(ctx, storage.Checkpoint{ConnectionID: conn.ID, CoverageFrom: old, CoverageTo: old}); err != nil {
		t.Fatal(err)
	}

	task := newTestCatchUpTask(New(store, &catchUpInterleaveMockClient{}, time.Second, time.Second), conn.ID, conn.Generation)
	if _, err := task.Step(ctx); err != nil {
		t.Fatal(err)
	}
	today := time.Now().In(acb.DefaultLocation)
	want := today.AddDate(0, 0, -6).Format("2006-01-02")
	if task.fromDate != want || task.toDate != today.Format("2006-01-02") {
		t.Fatalf("expected inclusive seven-day range %s..%s, got %s..%s", want, today.Format("2006-01-02"), task.fromDate, task.toDate)
	}
}

func TestCatchUpTask_InvalidCheckpointFailsClosed(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "cu_invalid_checkpoint.db"))
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
	if err := store.SaveCheckpoint(ctx, storage.Checkpoint{ConnectionID: conn.ID, CoverageFrom: "not-a-date", CoverageTo: "not-a-date"}); err != nil {
		t.Fatal(err)
	}

	task := newTestCatchUpTask(New(store, &catchUpInterleaveMockClient{}, time.Second, time.Second), conn.ID, conn.Generation)
	res, err := task.Step(ctx)
	if err == nil || res.Outcome != OutcomeFatal {
		t.Fatalf("expected fatal invalid checkpoint error, got result=%+v err=%v", res, err)
	}
}

func TestCatchUpTask_DBErrorIsReturned(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "cu_db_error.db"))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := store.ConfigureConnection(ctx, "***1234")
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, "UPDATE connections SET state='MONITORING'"); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	task := newTestCatchUpTask(New(store, &catchUpInterleaveMockClient{}, time.Second, time.Second), conn.ID, conn.Generation)
	res, err := task.Step(ctx)
	if err == nil || res.Outcome != OutcomeFatal {
		t.Fatalf("expected fatal database error, got result=%+v err=%v", res, err)
	}
}

func TestCatchUpTask_RestartReconstructsFromCheckpoint(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "cu_restart.db")
	store, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	conn, _ := store.ConfigureConnection(ctx, "***1234")
	_, _ = store.DB().ExecContext(ctx, "UPDATE connections SET state='MONITORING'")

	targetDay := time.Now().In(acb.DefaultLocation).AddDate(0, 0, -1).Format("2006-01-02")
	_ = store.SaveCheckpoint(ctx, storage.Checkpoint{
		ConnectionID: conn.ID,
		ScanID:       "prior_scan",
		CoverageFrom: targetDay,
		CoverageTo:   targetDay,
	})
	_ = store.RecordCoveragePerDay(ctx, conn.ID, map[string]int{targetDay: 3})

	// Simulate fresh worker restart with new monitor instance (no preserved form tokens)
	client := &catchUpInterleaveMockClient{}
	newMon := New(store, client, 5*time.Second, 15*time.Second)

	newTask := newTestCatchUpTask(newMon, conn.ID, conn.Generation)
	res, err := newTask.Step(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = res

	// Verify that the task picked up the checkpoint without errors and safely bootstrapped
	if !newTask.initialized {
		t.Fatal("expected task to be initialized")
	}
	if newTask.fromDate != targetDay {
		t.Fatalf("expected reconstructed fromDate %s, got %s", targetDay, newTask.fromDate)
	}
}

type catchUpPaginationFieldsClient struct {
	mu    sync.Mutex
	calls []map[string]string
}

func (c *catchUpPaginationFieldsClient) Bootstrap(context.Context) (acb.Response, error) {
	return acb.Response{StatusCode: 200, Kind: acb.AccountDetailPage, Body: `<form action="/history" method="POST">
		<input type="hidden" name="dse_operationName" value="op1" />
		<input type="hidden" name="dse_processorState" value="bootstrap" />
		<input type="hidden" name="dse_sessionId" value="session-1" />
		<input type="hidden" name="activeDatetimeByMonth" value="Y" />
		<input type="hidden" name="MonthCurr" value="9" />
		<input type="hidden" name="YearCurr" value="2026" />
	</form>`}, nil
}

func (c *catchUpPaginationFieldsClient) History(_ context.Context, _ string, fields map[string]string) (acb.Response, error) {
	c.mu.Lock()
	captured := make(map[string]string, len(fields))
	for key, value := range fields {
		captured[key] = value
	}
	c.calls = append(c.calls, captured)
	call := len(c.calls)
	c.mu.Unlock()

	nav := `<tr><td colspan="6"><span class="disabled">Trang sau</span></td></tr>`
	if call == 1 {
		nav = `<tr><td colspan="6"><a href="/history?page=2" onclick="submitEvent('nextPage')">Trang sau</a></td></tr>`
	}
	body := fmt.Sprintf(`<form action="/history" method="POST">
		<input type="hidden" name="dse_operationName" value="op1" />
		<input type="hidden" name="dse_processorState" value="page-%d" />
		<input type="hidden" name="dse_sessionId" value="session-%d" />
		<input type="hidden" name="activeDatetimeByMonth" value="Y" />
		<input type="hidden" name="MonthCurr" value="9" />
		<input type="hidden" name="YearCurr" value="2026" />
	</form>
	<table>
		<tr><th>Số GD</th><th>Ngày giao dịch</th><th>Ghi nợ</th><th>Ghi có</th><th>Số dư</th><th>Nội dung giao dịch</th></tr>
		<tr><td>PIN_TX_%d</td><td>21/09/2026</td><td>0</td><td>100,000</td><td>1,000,000</td><td>Transfer %d</td></tr>
		%s
	</table>`, call, call, call, call, nav)
	return acb.Response{StatusCode: 200, Kind: acb.HistoryPage, Body: body}, nil
}

func TestCatchUpTaskPinsContinuationWithoutMutatingPaginationFields(t *testing.T) {
	ctx := context.Background()
	store, conn := newRecoveryAdmissionStore(t)
	defer store.Close()

	client := &catchUpPaginationFieldsClient{}
	mon := New(store, client, 5*time.Second, 5*time.Second)
	mon.now = fixedRealtimeTime
	task := newTestCatchUpTask(mon, conn.ID, conn.Generation)
	// Step 1 processes day 1 atomically across all its pages (page 1 and continuation page 2)
	if _, err := task.Step(ctx); err != nil {
		t.Fatal(err)
	}

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.calls) != 2 {
		t.Fatalf("expected two history pages, got %d", len(client.calls))
	}
	first, second := client.calls[0], client.calls[1]
	if first["_explicitRange"] != "true" {
		t.Fatalf("first page lost explicit-range preparation: %#v", first)
	}
	if second["dse_processorState"] != "page-1" || second["dse_sessionId"] != "session-1" || second["dse_nextEventName"] != "nextPage" {
		t.Fatalf("continuation fields were not preserved: %#v", second)
	}
	if second["FromDate"] != "21/09/2026" || second["ToDate"] != "21/09/2026" || second["_raw"] != "true" {
		t.Fatalf("continuation date pinning is incorrect: %#v", second)
	}
	for _, key := range []string{"_explicitRange", "activeDatetimeByMonth", "MonthCurr", "YearCurr"} {
		if _, ok := second[key]; ok {
			t.Fatalf("continuation retained forbidden field %q: %#v", key, second)
		}
	}
	if first["FromDate"] != "21/09/2026" || first["ToDate"] != "21/09/2026" || first["_explicitRange"] != "true" {
		t.Fatalf("first-page fields were mutated after continuation: %#v", first)
	}
}
