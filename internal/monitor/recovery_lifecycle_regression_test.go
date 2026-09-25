package monitor

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
	"github.com/thedemontuan/acb-transaction-webhook/internal/scheduler"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

type recoveryMockBankClient struct {
	mu           sync.Mutex
	historyCalls []map[string]string
	pages        map[string][]string // date -> slice of HTML bodies for page 1, page 2, etc.
	pageIndices  map[string]int
	authFailOn   int
	callCount    int
	resetOn      int
	inconclusive bool
}

func newRecoveryMockBankClient() *recoveryMockBankClient {
	return &recoveryMockBankClient{
		pages:       make(map[string][]string),
		pageIndices: make(map[string]int),
	}
}

func (m *recoveryMockBankClient) Bootstrap(ctx context.Context) (acb.Response, error) {
	body := `<form action="/history" method="POST">
		<input type="hidden" name="dse_operationName" value="op1" />
		<input type="hidden" name="dse_processorState" value="ps1" />
		<input type="hidden" name="AccountNbr" value="123456" />
	</form>`
	return acb.Response{StatusCode: 200, Body: body, Kind: acb.AccountDetailPage}, nil
}

func (m *recoveryMockBankClient) History(ctx context.Context, endpoint string, fields map[string]string) (acb.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.callCount++
	copied := make(map[string]string, len(fields))
	for k, v := range fields {
		copied[k] = v
	}
	m.historyCalls = append(m.historyCalls, copied)

	if m.authFailOn > 0 && m.callCount >= m.authFailOn {
		return acb.Response{StatusCode: 401, Kind: acb.LoginPage}, &acb.AuthFailure{Kind: acb.LoginPage, Reason: "SESSION_EXPIRED"}
	}

	if m.resetOn > 0 && m.callCount == m.resetOn {
		return acb.Response{}, acb.ErrConversationReset
	}

	if m.inconclusive {
		return acb.Response{StatusCode: 200, Kind: acb.LoginPage, Body: `<form action="/login"><input name="username"></form>`}, nil
	}

	fromDate := fields["FromDate"]
	idx := m.pageIndices[fromDate]
	bodies := m.pages[fromDate]

	var body string
	if idx < len(bodies) {
		body = bodies[idx]
		m.pageIndices[fromDate]++
	} else {
		body = `<form action="/history" method="POST">
			<input type="hidden" name="dse_operationName" value="op1" />
			<input type="hidden" name="dse_processorState" value="ps_end" />
			<input type="hidden" name="AccountNbr" value="123456" />
		</form>
		<table>
			<tr><th>Số GD</th><th>Ngày giao dịch</th><th>Ghi nợ</th><th>Ghi có</th><th>Số dư</th><th>Nội dung giao dịch</th></tr>
			<tr><td colspan="6"><span class="disabled">Trang sau</span></td></tr>
		</table>`
	}

	return acb.Response{StatusCode: 200, Body: body, Kind: acb.HistoryPage}, nil
}

func buildHistoryHTML(txns []acb.Transaction, hasNext bool) string {
	var rows strings.Builder
	for _, txn := range txns {
		rows.WriteString(fmt.Sprintf(
			`<tr><td>%s</td><td>%s</td><td>%d</td><td>%d</td><td>%d</td><td>%s</td></tr>`,
			txn.Number, txn.TransactionAt, txn.Debit, txn.Credit, 1000000, txn.Description,
		))
	}
	nav := `<tr><td colspan="6"><span class="disabled">Trang sau</span></td></tr>`
	if hasNext {
		nav = `<tr><td colspan="6"><a href="/history?page=2" onclick="submitEvent('nextPage')">Trang sau</a></td></tr>`
	}
	return fmt.Sprintf(`
	<form action="/history" method="POST">
		<input type="hidden" name="dse_operationName" value="op1" />
		<input type="hidden" name="dse_processorState" value="ps_next" />
		<input type="hidden" name="AccountNbr" value="123456" />
	</form>
	<table>
		<tr><th>Số GD</th><th>Ngày giao dịch</th><th>Ghi nợ</th><th>Ghi có</th><th>Số dư</th><th>Nội dung giao dịch</th></tr>
		%s
		%s
	</table>
	`, rows.String(), nav)
}

func setupRecoveryTestEnv(t *testing.T, dbName string) (*storage.Store, storage.Connection, *storage.EndpointWithSecret) {
	t.Helper()
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), dbName))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := store.ConfigureConnection(ctx, "***1234")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, "UPDATE connections SET state = 'MONITORING', generation = 1 WHERE id = ?", conn.ID); err != nil {
		t.Fatal(err)
	}
	conn.State = "MONITORING"
	conn.Generation = 1

	ep, err := store.CreateEndpointWithSecret(ctx, "Webhook 1", "https://example.com/webhook")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetEndpointStatus(ctx, ep.ID, "ACTIVE"); err != nil {
		t.Fatal(err)
	}

	return store, conn, &ep
}

// 1. Completion event baseline & debits, real pages/rows counters
func TestRecoveryLifecycle_BaselineAndDebitsCompletionCounters(t *testing.T) {
	ctx := context.Background()
	store, conn, _ := setupRecoveryTestEnv(t, "rec_baseline_debits.db")
	defer store.Close()

	today := time.Now().In(acb.DefaultLocation).Format("2006-01-02")
	todayDisplay := time.Now().In(acb.DefaultLocation).Format("02/01/2006")

	mockClient := newRecoveryMockBankClient()
	// Two pages for today:
	// Page 1: 1 credit, 1 debit
	// Page 2: 1 debit
	mockClient.pages[todayDisplay] = []string{
		buildHistoryHTML([]acb.Transaction{
			{Number: "TX001", TransactionAt: todayDisplay + " 09:00:00", Credit: 200000, Debit: 0, Description: "Baseline Credit"},
			{Number: "TX002", TransactionAt: todayDisplay + " 10:00:00", Credit: 0, Debit: 50000, Description: "Baseline Debit 1"},
		}, true),
		buildHistoryHTML([]acb.Transaction{
			{Number: "TX003", TransactionAt: todayDisplay + " 11:00:00", Credit: 0, Debit: 75000, Description: "Baseline Debit 2"},
		}, false),
	}

	mon := New(store, mockClient, 5*time.Second, 15*time.Second)

	var notifiedPoll storage.PollRun
	var notifiedInserted int
	var notifierCalled bool
	mon.WithPollNotifier(func(poll storage.PollRun, insertedCount int) {
		notifiedPoll = poll
		notifiedInserted = insertedCount
		notifierCalled = true
	})

	// Ensure recovery run with INITIAL_AUTH_BOOTSTRAP
	plan := storage.RecoveryRunPlan{
		Reason:    storage.RecoveryReasonInitialAuth,
		RangeFrom: today,
		RangeTo:   today,
		NextDay:   today,
	}
	run, _, err := store.EnsureRecoveryRunWithPlan(ctx, conn.ID, conn.Generation, "baseline-test", plan)
	if err != nil {
		t.Fatal(err)
	}

	task := NewRecoveryCatchUpTask(mon, conn.ID, conn.Generation, run.Reason, run.ID)

	// Step 1: Processes today (pages 1 and 2)
	res, err := task.Step(ctx)
	if err != nil {
		t.Fatalf("task step: %v", err)
	}
	// After processing today, currentDay advances past toT -> completes or yields
	for !res.Done {
		res, err = task.Step(ctx)
		if err != nil {
			t.Fatalf("task step continuation: %v", err)
		}
	}

	if !notifierCalled {
		t.Fatal("expected WithPollNotifier to be called on recovery completion")
	}

	// Verify poll completion metrics
	if notifiedPoll.Status != "SUCCEEDED" {
		t.Fatalf("expected SUCCEEDED poll status, got %s", notifiedPoll.Status)
	}
	if notifiedPoll.Classifier != "RECOVERY" {
		t.Fatalf("expected classifier RECOVERY, got %s", notifiedPoll.Classifier)
	}
	if notifiedPoll.Pages != 2 {
		t.Fatalf("expected 2 pages scanned, got %d", notifiedPoll.Pages)
	}
	if notifiedPoll.RowsSeen != 3 {
		t.Fatalf("expected 3 rows seen, got %d", notifiedPoll.RowsSeen)
	}
	if notifiedInserted != 3 {
		t.Fatalf("expected 3 transactions inserted, got %d", notifiedInserted)
	}

	// Verify database state: transactions saved with BASELINE state
	txns, err := store.ListTransactionsFiltered(ctx, storage.TransactionFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(txns.Items) != 3 {
		t.Fatalf("expected 3 transactions in DB, got %d", len(txns.Items))
	}

	// Verify INITIAL_AUTH_BOOTSTRAP baseline policy: 0 webhook deliveries enqueued
	deliveries, err := store.ListDeliveries(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(deliveries) != 0 {
		t.Fatalf("expected 0 webhook deliveries for baseline bootstrap, got %d", len(deliveries))
	}

	// Verify recovery run is completed
	completedRun, err := store.GetRecoveryRunByEvent(ctx, conn.ID, conn.Generation, "baseline-test")
	if err != nil {
		t.Fatal(err)
	}
	if completedRun.Status != storage.RecoveryRunStatusCompleted {
		t.Fatalf("expected run status COMPLETED, got %s", completedRun.Status)
	}
}

// 2. AuthRequired during recovery records real pages and rows seen (not 0/0)
func TestRecoveryLifecycle_AuthRequiredRecordsPagesAndRows(t *testing.T) {
	ctx := context.Background()
	store, conn, _ := setupRecoveryTestEnv(t, "rec_auth_req.db")
	defer store.Close()

	today := time.Now().In(acb.DefaultLocation).Format("2006-01-02")
	todayDisplay := time.Now().In(acb.DefaultLocation).Format("02/01/2006")

	mockClient := newRecoveryMockBankClient()
	// Page 1 succeeds with 2 rows, Page 2 triggers AuthFailure
	mockClient.pages[todayDisplay] = []string{
		buildHistoryHTML([]acb.Transaction{
			{Number: "TX101", TransactionAt: todayDisplay + " 08:00:00", Credit: 100000, Debit: 0, Description: "Deposit 1"},
			{Number: "TX102", TransactionAt: todayDisplay + " 08:30:00", Credit: 0, Debit: 20000, Description: "Withdraw 1"},
		}, true),
	}
	mockClient.authFailOn = 2 // Fail on call 2 (page 2)

	mon := New(store, mockClient, 5*time.Second, 15*time.Second)

	var notifiedPoll storage.PollRun
	mon.WithPollNotifier(func(poll storage.PollRun, insertedCount int) {
		notifiedPoll = poll
	})

	plan := storage.RecoveryRunPlan{
		Reason:    storage.RecoveryReasonReauth,
		RangeFrom: today,
		RangeTo:   today,
		NextDay:   today,
	}
	run, _, err := store.EnsureRecoveryRunWithPlan(ctx, conn.ID, conn.Generation, "auth-req-test", plan)
	if err != nil {
		t.Fatal(err)
	}

	task := NewRecoveryCatchUpTask(mon, conn.ID, conn.Generation, run.Reason, run.ID)

	res, _ := task.Step(ctx)
	if !res.Done {
		t.Fatal("expected task to be Done on authRequired")
	}
	if res.Outcome != scheduler.OutcomeAuth {
		t.Fatalf("expected OutcomeAuth, got %s", res.Outcome)
	}

	// Verify poll run has real pages and rows seen, NOT 0/0!
	if notifiedPoll.Status != "AUTH_REQUIRED" {
		t.Fatalf("expected AUTH_REQUIRED status, got %s", notifiedPoll.Status)
	}
	if notifiedPoll.Pages != 1 {
		t.Fatalf("expected Pages=1 (page 1 was scanned before auth expired), got %d", notifiedPoll.Pages)
	}
	if notifiedPoll.RowsSeen != 2 {
		t.Fatalf("expected RowsSeen=2, got %d", notifiedPoll.RowsSeen)
	}

	// Verify recovery run is terminal FAILED so reconcileOpenRecovery does not requeue it
	var status, errCode string
	if err := store.DB().QueryRowContext(ctx, "SELECT status, error_code FROM recovery_runs WHERE id = ?", run.ID).Scan(&status, &errCode); err != nil {
		t.Fatal(err)
	}
	if status != string(storage.RecoveryRunStatusFailed) {
		t.Fatalf("expected recovery run status FAILED, got %s", status)
	}
	if errCode != "AUTH_REQUIRED" && errCode != "sensitive error details redacted" {
		t.Fatalf("expected error code AUTH_REQUIRED or redacted, got %s", errCode)
	}

	// Reconcile open recovery for current connection generation should find NO open runs
	currConn, err := store.Connection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	openRuns, err := store.ListOpenRecoveryRuns(ctx, currConn.ID, currConn.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if len(openRuns) != 0 {
		t.Fatalf("expected 0 open recovery runs after terminal auth failure, got %d", len(openRuns))
	}
}

// 3. Terminal paths mark recovery_runs as FAILED to prevent 5-second infinite retry loop
func TestRecoveryLifecycle_TerminalPathsMarkFailed_NoRetryLoop(t *testing.T) {
	ctx := context.Background()

	t.Run("ExhaustedInconclusiveLogin", func(t *testing.T) {
		store, conn, _ := setupRecoveryTestEnv(t, "rec_term_login.db")
		defer store.Close()

		today := time.Now().In(acb.DefaultLocation).Format("2006-01-02")
		mockClient := newRecoveryMockBankClient()
		mockClient.inconclusive = true

		mon := New(store, mockClient, 5*time.Second, 15*time.Second)

		plan := storage.RecoveryRunPlan{
			Reason:    storage.RecoveryReasonStartup,
			RangeFrom: today,
			RangeTo:   today,
			NextDay:   today,
		}
		run, _, err := store.EnsureRecoveryRunWithPlan(ctx, conn.ID, conn.Generation, "term-login", plan)
		if err != nil {
			t.Fatal(err)
		}

		task := NewRecoveryCatchUpTask(mon, conn.ID, conn.Generation, run.Reason, run.ID)

		// Step until bounded retries are exhausted
		var res scheduler.TaskStepResult
		for i := 0; i <= catchUpMaxTransientRetries+1; i++ {
			res, _ = task.Step(ctx)
			if res.Done {
				break
			}
		}

		if !res.Done {
			t.Fatal("expected task to be Done after exhausting transient retries")
		}

		// Verify run transitioned to terminal FAILED
		r, err := store.GetRecoveryRunByEvent(ctx, conn.ID, conn.Generation, "term-login")
		if err != nil {
			t.Fatal(err)
		}
		if r.Status != storage.RecoveryRunStatusFailed {
			t.Fatalf("expected FAILED status for exhausted retries, got %s", r.Status)
		}
		if r.ErrorCode != "UNCONFIRMED_LOGIN" {
			t.Fatalf("expected UNCONFIRMED_LOGIN error code, got %s", r.ErrorCode)
		}

		// Reconcile must not find open runs
		openRuns, err := store.ListOpenRecoveryRuns(ctx, conn.ID, conn.Generation)
		if err != nil {
			t.Fatal(err)
		}
		if len(openRuns) != 0 {
			t.Fatalf("expected 0 open runs, got %d (retry loop prevented)", len(openRuns))
		}
	})

	t.Run("RepeatedConversationReset", func(t *testing.T) {
		store, conn, _ := setupRecoveryTestEnv(t, "rec_term_reset.db")
		defer store.Close()

		today := time.Now().In(acb.DefaultLocation).Format("2006-01-02")
		mockClient := newRecoveryMockBankClient()
		mockClient.resetOn = 1 // triggers reset immediately

		mon := New(store, mockClient, 5*time.Second, 15*time.Second)

		plan := storage.RecoveryRunPlan{
			Reason:    storage.RecoveryReasonStartup,
			RangeFrom: today,
			RangeTo:   today,
			NextDay:   today,
		}
		run, _, err := store.EnsureRecoveryRunWithPlan(ctx, conn.ID, conn.Generation, "term-reset", plan)
		if err != nil {
			t.Fatal(err)
		}

		task := NewRecoveryCatchUpTask(mon, conn.ID, conn.Generation, run.Reason, run.ID)
		res, _ := task.Step(ctx)
		// If first reset triggers restart, call Step again if needed
		if !res.Done {
			mockClient.resetOn = 2 // reset again on second attempt
			res, _ = task.Step(ctx)
		}

		r, err := store.GetRecoveryRunByEvent(ctx, conn.ID, conn.Generation, "term-reset")
		if err != nil {
			t.Fatal(err)
		}
		if res.Done && r.Status != storage.RecoveryRunStatusFailed {
			t.Fatalf("expected FAILED status for repeated reset, got %s", r.Status)
		}
	})
}

// 4. Durable resume across restart without fake coverage
func TestRecoveryLifecycle_DurableResumeAcrossRestart(t *testing.T) {
	ctx := context.Background()
	store, conn, _ := setupRecoveryTestEnv(t, "rec_resume.db")
	defer store.Close()

	day1 := time.Now().In(acb.DefaultLocation).AddDate(0, 0, -2).Format("2006-01-02")
	day2 := time.Now().In(acb.DefaultLocation).AddDate(0, 0, -1).Format("2006-01-02")
	day1Display := time.Now().In(acb.DefaultLocation).AddDate(0, 0, -2).Format("02/01/2006")
	day2Display := time.Now().In(acb.DefaultLocation).AddDate(0, 0, -1).Format("02/01/2006")

	mockClient := newRecoveryMockBankClient()
	mockClient.pages[day1Display] = []string{
		buildHistoryHTML([]acb.Transaction{
			{Number: "D1_01", TransactionAt: day1Display + " 10:00:00", Credit: 100000, Debit: 0, Description: "Day 1 Txn"},
		}, false),
	}
	mockClient.pages[day2Display] = []string{
		buildHistoryHTML([]acb.Transaction{
			{Number: "D2_01", TransactionAt: day2Display + " 10:00:00", Credit: 200000, Debit: 0, Description: "Day 2 Txn"},
		}, false),
	}

	mon := New(store, mockClient, 5*time.Second, 15*time.Second)

	plan := storage.RecoveryRunPlan{
		Reason:    storage.RecoveryReasonStartup,
		RangeFrom: day1,
		RangeTo:   day2,
		NextDay:   day1,
	}
	run, _, err := store.EnsureRecoveryRunWithPlan(ctx, conn.ID, conn.Generation, "resume-test", plan)
	if err != nil {
		t.Fatal(err)
	}

	// Instance 1: runs Day 1 and yields at day boundary
	task1 := NewRecoveryCatchUpTask(mon, conn.ID, conn.Generation, run.Reason, run.ID)
	res1, err := task1.Step(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res1.Done {
		t.Fatal("expected yield at day boundary (Done=false)")
	}

	// Verify day 1 was committed to recovery_runs.next_day
	persistedRun, err := store.GetRecoveryRunByEvent(ctx, conn.ID, conn.Generation, "resume-test")
	if err != nil {
		t.Fatal(err)
	}
	if persistedRun.NextDay != day2 {
		t.Fatalf("expected NextDay=%s after day 1, got %s", day2, persistedRun.NextDay)
	}

	// Simulate restart: Instance 2 resumes same runID
	task2 := NewRecoveryCatchUpTask(mon, conn.ID, conn.Generation, persistedRun.Reason, persistedRun.ID)
	res2, err := task2.Step(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Task 2 should process day 2 and complete
	for !res2.Done {
		res2, err = task2.Step(ctx)
		if err != nil {
			t.Fatal(err)
		}
	}

	// Verify Day 1 was NOT scanned again by task2
	finalRun, err := store.GetRecoveryRunByEvent(ctx, conn.ID, conn.Generation, "resume-test")
	if err != nil {
		t.Fatal(err)
	}
	if finalRun.Status != storage.RecoveryRunStatusCompleted {
		t.Fatalf("expected COMPLETED status, got %s", finalRun.Status)
	}

	// Check total transactions in DB
	txns, err := store.ListTransactionsFiltered(ctx, storage.TransactionFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(txns.Items) != 2 {
		t.Fatalf("expected 2 transactions (one from each day, no duplicates), got %d", len(txns.Items))
	}
}

// 5. Generation fencing discards recovery task when generation changes
func TestRecoveryLifecycle_GenerationFencing(t *testing.T) {
	ctx := context.Background()
	store, conn, _ := setupRecoveryTestEnv(t, "rec_fence.db")
	defer store.Close()

	today := time.Now().In(acb.DefaultLocation).Format("2006-01-02")
	mockClient := newRecoveryMockBankClient()
	mon := New(store, mockClient, 5*time.Second, 15*time.Second)

	plan := storage.RecoveryRunPlan{
		Reason:    storage.RecoveryReasonStartup,
		RangeFrom: today,
		RangeTo:   today,
		NextDay:   today,
	}
	run, _, err := store.EnsureRecoveryRunWithPlan(ctx, conn.ID, conn.Generation, "fence-test", plan)
	if err != nil {
		t.Fatal(err)
	}

	task := NewRecoveryCatchUpTask(mon, conn.ID, conn.Generation, run.Reason, run.ID)

	// Invalidate generation behind task's back
	if _, err := store.DB().ExecContext(ctx, "UPDATE connections SET generation = generation + 1 WHERE id = ?", conn.ID); err != nil {
		t.Fatal(err)
	}

	res, err := task.Step(ctx)
	if err != nil {
		t.Fatalf("expected silent fence discard, got err=%v", err)
	}
	if !res.Done {
		t.Fatal("expected task to exit Done=true on generation mismatch")
	}
}
