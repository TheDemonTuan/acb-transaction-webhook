package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

type postAuthMockBankClient struct {
	mu           sync.Mutex
	historyCalls []map[string]string
	txnsByDate   map[string][]acb.Transaction
}

func newPostAuthMockBankClient() *postAuthMockBankClient {
	return &postAuthMockBankClient{
		txnsByDate: make(map[string][]acb.Transaction),
	}
}

func (m *postAuthMockBankClient) Bootstrap(ctx context.Context) (acb.Response, error) {
	body := `<form action="/history" method="POST">
		<input type="hidden" name="dse_operationName" value="op1" />
		<input type="hidden" name="dse_processorState" value="ps1" />
		<input type="hidden" name="AccountNbr" value="123456" />
	</form>`
	return acb.Response{StatusCode: 200, Body: body, Kind: acb.AccountDetailPage}, nil
}

func (m *postAuthMockBankClient) History(ctx context.Context, endpoint string, fields map[string]string) (acb.Response, error) {
	m.mu.Lock()
	copied := make(map[string]string, len(fields))
	for k, v := range fields {
		copied[k] = v
	}
	m.historyCalls = append(m.historyCalls, copied)
	fromDate := fields["FromDate"]
	txns := m.txnsByDate[fromDate]
	m.mu.Unlock()

	var rows strings.Builder
	if len(txns) == 0 {
		rows.WriteString(`<tr><td colspan="6">Không có giao dịch</td></tr>`)
	} else {
		for _, txn := range txns {
			rows.WriteString(fmt.Sprintf(
				`<tr><td>%s</td><td>%s</td><td>%d</td><td>%d</td><td>%d</td><td>%s</td></tr>`,
				txn.Number, txn.TransactionAt, txn.Debit, txn.Credit, 1000000, txn.Description,
			))
		}
	}

	body := fmt.Sprintf(`
	<form action="/history" method="POST">
		<input type="hidden" name="dse_operationName" value="op1" />
		<input type="hidden" name="dse_processorState" value="ps_next" />
		<input type="hidden" name="AccountNbr" value="123456" />
	</form>
	<table>
		<tr><th>Số GD</th><th>Ngày giao dịch</th><th>Ghi nợ</th><th>Ghi có</th><th>Số dư</th><th>Nội dung giao dịch</th></tr>
		%s
	</table>
	`, rows.String())

	return acb.Response{StatusCode: 200, Body: body, Kind: acb.HistoryPage}, nil
}

func setupTestStoreAndEndpoint(t *testing.T) (*storage.Store, storage.Connection, storage.EndpointWithSecret) {
	t.Helper()
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "post_auth.db"))
	if err != nil {
		t.Fatal(err)
	}

	conn, err := store.ConfigureConnection(ctx, "***1234")
	if err != nil {
		store.Close()
		t.Fatal(err)
	}

	ep, err := store.CreateEndpointWithSecret(ctx, "Test Hook", "https://example.com/webhook")
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.SetEndpointStatus(ctx, ep.ID, "ACTIVE"); err != nil {
		store.Close()
		t.Fatal(err)
	}

	return store, conn, ep
}

func drainTask(ctx context.Context, task *CatchUpTask) error {
	for {
		res, err := task.Step(ctx)
		if err != nil {
			return err
		}
		if res.Done {
			return res.Error
		}
	}
}

// Test 1: First-ever auth creates INITIAL_AUTH_BOOTSTRAP, scans 7 days, stores transactions as BASELINE,
// emits 0 events, creates 0 deliveries (no spam Bark).
func TestPostAuth_FirstAuth_BaselineBootstrap7Days_NoSpamBark(t *testing.T) {
	ctx := context.Background()
	store, conn, _ := setupTestStoreAndEndpoint(t)
	defer store.Close()

	attempt, err := store.StartAuthAttempt(ctx, "owner", 1)
	if err != nil {
		t.Fatal(err)
	}
	completedConn, err := store.CompleteAuthSession(ctx, attempt.ID, []byte("opaque-session"))
	if err != nil {
		t.Fatal(err)
	}
	if completedConn.State != "MONITORING" {
		t.Fatalf("expected MONITORING state, got %s", completedConn.State)
	}

	// Verify reason in recovery_runs is INITIAL_AUTH_BOOTSTRAP
	run, err := store.GetRecoveryRunByEvent(ctx, conn.ID, attempt.Generation, attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Reason != storage.RecoveryReasonInitialAuth {
		t.Fatalf("expected reason %s, got %s", storage.RecoveryReasonInitialAuth, run.Reason)
	}

	mockClient := newPostAuthMockBankClient()
	nowLocal := time.Now().In(acb.DefaultLocation)
	todayStr := nowLocal.Format("02/01/2006")
	threeDaysAgoStr := nowLocal.AddDate(0, 0, -3).Format("02/01/2006")

	mockClient.txnsByDate[threeDaysAgoStr] = []acb.Transaction{
		{Number: "OLD_CREDIT_1", TransactionAt: threeDaysAgoStr + " 10:00:00", Credit: 200000, Debit: 0, Description: "Old Payment"},
	}
	mockClient.txnsByDate[todayStr] = []acb.Transaction{
		{Number: "TODAY_CREDIT_1", TransactionAt: todayStr + " 09:00:00", Credit: 500000, Debit: 0, Description: "Today Payment"},
	}

	mon := New(store, mockClient, 5*time.Second, 15*time.Second)
	mon.now = func() time.Time { return nowLocal }

	sched := mon.Scheduler()
	sched.Start(ctx)
	defer sched.Stop()

	if err := mon.ScheduleRecovery(ctx, completedConn.ID, completedConn.Generation, attempt.ID); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		r, err := store.GetRecoveryRunByEvent(ctx, conn.ID, completedConn.Generation, attempt.ID)
		if err == nil && r.Status == storage.RecoveryRunStatusCompleted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for recovery to complete: %+v, err=%v", r, err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Assertions:
	// 1. Transactions stored with baseline_state = 'BASELINE'
	var baselineCount int
	if err := store.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM transactions WHERE baseline_state = 'BASELINE'").Scan(&baselineCount); err != nil {
		t.Fatal(err)
	}
	if baselineCount != 2 {
		t.Fatalf("expected 2 baseline transactions, got %d", baselineCount)
	}

	// 2. No events emitted (0 Bark / webhook spam)
	var eventCount int
	if err := store.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM events").Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 0 {
		t.Fatalf("expected 0 events on initial bootstrap, got %d", eventCount)
	}

	// 3. No deliveries created
	var deliveryCount int
	if err := store.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM deliveries").Scan(&deliveryCount); err != nil {
		t.Fatal(err)
	}
	if deliveryCount != 0 {
		t.Fatalf("expected 0 deliveries on initial bootstrap, got %d", deliveryCount)
	}

	// 4. Recovery run COMPLETED
	finishedRun, err := store.GetRecoveryRunByEvent(ctx, conn.ID, attempt.Generation, attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finishedRun.Status != storage.RecoveryRunStatusCompleted {
		t.Fatalf("expected recovery run COMPLETED, got %s", finishedRun.Status)
	}
}

// Test 2: Re-auth after session death scans 7 days, deduplicates existing transactions,
// inserts missed transactions, and emits CATCH_UP events only for missed CREDIT transactions.
func TestPostAuth_Reauth_CatchUp7Days_BarkOnlyForNewMissingCredits(t *testing.T) {
	ctx := context.Background()
	store, conn, _ := setupTestStoreAndEndpoint(t)
	defer store.Close()

	nowLocal := time.Now().In(acb.DefaultLocation)
	todayStr := nowLocal.Format("02/01/2006")
	todayIso := nowLocal.Format("2006-01-02")
	fourDaysAgoStr := nowLocal.AddDate(0, 0, -4).Format("02/01/2006")
	twoDaysAgoStr := nowLocal.AddDate(0, 0, -2).Format("02/01/2006")

	// Pre-transition connection to MONITORING at generation 1
	if _, err := store.DB().ExecContext(ctx, "UPDATE connections SET state = 'MONITORING', generation = 1 WHERE id = ?", conn.ID); err != nil {
		t.Fatal(err)
	}
	conn.State = "MONITORING"
	conn.Generation = 1

	// Pre-populate existing transaction from 4 days ago
	oldTxn := storage.BatchTransactionItem{
		Number:        "EXISTING_1",
		Credit:        100000,
		Debit:         0,
		TransactionAt: fourDaysAgoStr + " 08:00:00",
		EffectiveAt:   fourDaysAgoStr,
		Description:   "Old Existing Deposit",
	}
	_, err := store.IngestTransactionsBatchWithSource(ctx, conn.ID, 1, "***1234", []storage.BatchTransactionItem{oldTxn}, true, "BOOTSTRAP")
	if err != nil {
		t.Fatalf("pre-populate transaction: %v", err)
	}

	// Record checkpoint as today (simulating user had normal polls today before session died)
	if err := store.SaveCheckpoint(ctx, storage.Checkpoint{
		ConnectionID: conn.ID,
		ScanID:       "old_scan",
		CoverageFrom: todayIso,
		CoverageTo:   todayIso,
	}); err != nil {
		t.Fatal(err)
	}

	// Connection transitions to AUTH_STARTING, generation bumps to 2
	attempt, err := store.StartAuthAttempt(ctx, "owner", 2)
	if err != nil {
		t.Fatal(err)
	}
	completedConn, err := store.CompleteAuthSession(ctx, attempt.ID, []byte("reauth-session"))
	if err != nil {
		t.Fatal(err)
	}

	// Verify reason is SESSION_REAUTH_CATCHUP
	run, err := store.GetRecoveryRunByEvent(ctx, conn.ID, attempt.Generation, attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Reason != storage.RecoveryReasonReauth {
		t.Fatalf("expected reason %s, got %s", storage.RecoveryReasonReauth, run.Reason)
	}

	// Mock bank returns:
	// - 4 days ago: EXISTING_1 (already in DB -> should skip/dedupe)
	// - 2 days ago: MISSED_CREDIT (credit 250k -> should emit CATCH_UP event & delivery)
	// - 2 days ago: MISSED_DEBIT (debit 50k -> should insert, but NO credit event / delivery)
	// - today: TODAY_CREDIT (credit 300k -> should emit CATCH_UP event & delivery)
	mockClient := newPostAuthMockBankClient()
	mockClient.txnsByDate[fourDaysAgoStr] = []acb.Transaction{
		{Number: "EXISTING_1", TransactionAt: fourDaysAgoStr + " 08:00:00", Credit: 100000, Debit: 0, Description: "Old Existing Deposit"},
	}
	mockClient.txnsByDate[twoDaysAgoStr] = []acb.Transaction{
		{Number: "MISSED_CREDIT", TransactionAt: twoDaysAgoStr + " 14:00:00", Credit: 250000, Debit: 0, Description: "Missed Transfer In"},
		{Number: "MISSED_DEBIT", TransactionAt: twoDaysAgoStr + " 15:00:00", Credit: 0, Debit: 50000, Description: "Missed Fee Out"},
	}
	mockClient.txnsByDate[todayStr] = []acb.Transaction{
		{Number: "TODAY_CREDIT", TransactionAt: todayStr + " 10:00:00", Credit: 300000, Debit: 0, Description: "Today Transfer In"},
	}

	mon := New(store, mockClient, 5*time.Second, 15*time.Second)
	mon.now = func() time.Time { return nowLocal }

	sched := mon.Scheduler()
	sched.Start(ctx)
	defer sched.Stop()

	if err := mon.ScheduleRecovery(ctx, completedConn.ID, completedConn.Generation, attempt.ID); err != nil {
		t.Fatal(err)
	}

	// Verify plan was NOT collapsed to today!
	plannedRun, err := store.GetRecoveryRunByEvent(ctx, conn.ID, attempt.Generation, attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	expectedFrom := nowLocal.AddDate(0, 0, -6).Format("2006-01-02")
	if plannedRun.RangeFrom != expectedFrom {
		t.Fatalf("checkpoint must not collapse recovery window: expected RangeFrom %s, got %s", expectedFrom, plannedRun.RangeFrom)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		r, err := store.GetRecoveryRunByEvent(ctx, conn.ID, completedConn.Generation, attempt.ID)
		if err == nil && r.Status == storage.RecoveryRunStatusCompleted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for recovery to complete: %+v, err=%v", r, err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Assertions:
	// 1. Transactions in DB:
	// - EXISTING_1 preserved
	// - MISSED_CREDIT, MISSED_DEBIT, TODAY_CREDIT inserted with baseline_state='NONE'
	var noneCount int
	if err := store.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM transactions WHERE baseline_state = 'NONE'").Scan(&noneCount); err != nil {
		t.Fatal(err)
	}
	if noneCount != 3 {
		t.Fatalf("expected 3 new transactions with baseline_state NONE, got %d", noneCount)
	}

	// 2. Exactly 2 credit events (MISSED_CREDIT and TODAY_CREDIT)
	var eventCount int
	if err := store.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM events").Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 2 {
		t.Fatalf("expected exactly 2 events for new credits, got %d", eventCount)
	}

	// 3. Exactly 2 deliveries created, both with source CATCH_UP
	rows, err := store.DB().QueryContext(ctx, `
		SELECT e.payload FROM deliveries d
		JOIN events e ON d.event_id = e.id
	`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var payloadCount int
	for rows.Next() {
		payloadCount++
		var payloadBytes []byte
		if err := rows.Scan(&payloadBytes); err != nil {
			t.Fatal(err)
		}
		var data map[string]any
		if err := json.Unmarshal(payloadBytes, &data); err != nil {
			t.Fatal(err)
		}
		if data["source"] != "CATCH_UP" {
			t.Fatalf("expected source CATCH_UP in delivery payload, got %v", data["source"])
		}
	}
	if payloadCount != 2 {
		t.Fatalf("expected 2 deliveries, got %d", payloadCount)
	}

	// 4. Recovery run is COMPLETED
	finishedRun, err := store.GetRecoveryRunByEvent(ctx, conn.ID, attempt.Generation, attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finishedRun.Status != storage.RecoveryRunStatusCompleted {
		t.Fatalf("expected recovery run COMPLETED, got %s", finishedRun.Status)
	}
}

// Test 3: Normal realtime poll is suppressed while recovery is open;
// payment boost (priority 80) is permitted during SESSION_REAUTH_CATCHUP;
// but payment boost is deferred during INITIAL_AUTH_BOOTSTRAP to protect baseline.
func TestPostAuth_RealtimePollGating_SuppressedDuringRecovery_BoostAllowed(t *testing.T) {
	ctx := context.Background()
	store, conn, _ := setupTestStoreAndEndpoint(t)
	defer store.Close()

	// Set connection to MONITORING at generation 1
	if _, err := store.DB().ExecContext(ctx, "UPDATE connections SET state = 'MONITORING', generation = 1 WHERE id = ?", conn.ID); err != nil {
		t.Fatal(err)
	}
	conn.State = "MONITORING"
	conn.Generation = 1

	mockClient := newPostAuthMockBankClient()
	mon := New(store, mockClient, 5*time.Second, 15*time.Second)

	// Case 1: SESSION_REAUTH_CATCHUP is open
	today := time.Now().In(acb.DefaultLocation).Format("2006-01-02")
	plan := storage.RecoveryRunPlan{
		Reason:    storage.RecoveryReasonReauth,
		RangeFrom: today,
		RangeTo:   today,
		NextDay:   today,
	}
	run, _, err := store.EnsureRecoveryRunWithPlan(ctx, conn.ID, conn.Generation, "reauth-open", plan)
	if err != nil {
		t.Fatal(err)
	}

	// Regular idle realtime poll should be suppressed
	idleTask := NewRealtimeTask(mon, PriorityRealtimePoll, conn.ID, conn.Generation)
	res, err := idleTask.Step(ctx)
	if err != nil {
		t.Fatalf("idle task step: %v", err)
	}
	if !res.Done {
		t.Fatal("expected idle poll to finish (suppressed)")
	}
	if len(mockClient.historyCalls) != 0 {
		t.Fatalf("expected 0 bank requests from suppressed idle poll, got %d", len(mockClient.historyCalls))
	}

	// Payment boost is active: should be permitted between catch-up days
	mon.StartPaymentBoost(150000)
	boostedTask := NewRealtimeTask(mon, PriorityRealtimePoll, conn.ID, conn.Generation)
	resBoost, err := boostedTask.Step(ctx)
	if err != nil {
		t.Fatalf("boosted task step: %v", err)
	}
	if !resBoost.Done {
		t.Fatal("expected boosted poll to complete")
	}
	if len(mockClient.historyCalls) == 0 {
		t.Fatal("expected bank history call from boosted poll")
	}
	mon.StopPaymentBoost("")

	// Mark run completed
	_, _ = store.DB().ExecContext(ctx, "UPDATE recovery_runs SET status='COMPLETED' WHERE id=?", run.ID)

	// Case 2: INITIAL_AUTH_BOOTSTRAP is open
	planInit := storage.RecoveryRunPlan{
		Reason:    storage.RecoveryReasonInitialAuth,
		RangeFrom: today,
		RangeTo:   today,
		NextDay:   today,
	}
	runInit, _, err := store.EnsureRecoveryRunWithPlan(ctx, conn.ID, conn.Generation, "init-open", planInit)
	if err != nil {
		t.Fatal(err)
	}

	mockClient.mu.Lock()
	mockClient.historyCalls = nil
	mockClient.mu.Unlock()

	// Both idle poll and boost must be suppressed during INITIAL_AUTH_BOOTSTRAP
	mon.StartPaymentBoost(200000)
	boostedInitTask := NewRealtimeTask(mon, PriorityRealtimePoll, conn.ID, conn.Generation)
	resInitBoost, err := boostedInitTask.Step(ctx)
	if err != nil {
		t.Fatalf("boosted init task step: %v", err)
	}
	if !resInitBoost.Done {
		t.Fatal("expected boosted init poll to finish immediately (suppressed)")
	}
	if len(mockClient.historyCalls) != 0 {
		t.Fatalf("expected 0 bank calls during INITIAL_AUTH_BOOTSTRAP, got %d", len(mockClient.historyCalls))
	}
	mon.StopPaymentBoost("")

	_, _ = store.DB().ExecContext(ctx, "UPDATE recovery_runs SET status='COMPLETED' WHERE id=?", runInit.ID)
}

// Test 4: Re-auth does NOT skip days having coverage from an older session.
func TestPostAuth_Reauth_DoesNotSkipCoveredDaysFromOldSession(t *testing.T) {
	ctx := context.Background()
	store, conn, _ := setupTestStoreAndEndpoint(t)
	defer store.Close()

	nowLocal := time.Now().In(acb.DefaultLocation)
	yesterday := nowLocal.AddDate(0, 0, -1).Format("2006-01-02")
	twoDaysAgo := nowLocal.AddDate(0, 0, -2).Format("2006-01-02")

	// Set connection to MONITORING at generation 1
	if _, err := store.DB().ExecContext(ctx, "UPDATE connections SET state = 'MONITORING', generation = 1 WHERE id = ?", conn.ID); err != nil {
		t.Fatal(err)
	}
	conn.State = "MONITORING"
	conn.Generation = 1

	// Pre-record coverage as COMPLETE for yesterday and 2 days ago from old session
	if err := store.RecordCoveragePerDay(ctx, conn.ID, map[string]int{
		twoDaysAgo: 5,
		yesterday:  3,
	}); err != nil {
		t.Fatal(err)
	}

	mockClient := newPostAuthMockBankClient()
	mon := New(store, mockClient, 5*time.Second, 15*time.Second)
	mon.now = func() time.Time { return nowLocal }

	today := nowLocal.Format("2006-01-02")
	plan := storage.RecoveryRunPlan{
		Reason:    storage.RecoveryReasonReauth,
		RangeFrom: twoDaysAgo,
		RangeTo:   today,
		NextDay:   twoDaysAgo,
	}
	run, _, err := store.EnsureRecoveryRunWithPlan(ctx, conn.ID, conn.Generation, "reauth-coverage-test", plan)
	if err != nil {
		t.Fatal(err)
	}

	task := NewRecoveryCatchUpTask(mon, conn.ID, conn.Generation, storage.RecoveryReasonReauth, run.ID)

	// Step 1: Must process twoDaysAgo (NOT skip to today)
	res1, err := task.Step(ctx)
	if err != nil {
		t.Fatalf("step 1: %v", err)
	}
	if res1.Done {
		t.Fatal("expected step 1 to yield between days, not be done")
	}

	// Verify bank received history request for twoDaysAgo
	mockClient.mu.Lock()
	if len(mockClient.historyCalls) == 0 {
		t.Fatal("expected history call for twoDaysAgo, got none (incorrectly skipped)")
	}
	calledDate := mockClient.historyCalls[0]["FromDate"]
	expectedDate := nowLocal.AddDate(0, 0, -2).Format("02/01/2006")
	mockClient.mu.Unlock()

	if calledDate != expectedDate {
		t.Fatalf("expected call for %s, got %s", expectedDate, calledDate)
	}
}
