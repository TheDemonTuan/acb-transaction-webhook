package monitor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

// Exercise the production ACB transport's form preparation and parser, not a
// BankClient echo. Only the upstream HTTP exchange is replaced.
type automaticBankFixture struct {
	mu          sync.Mutex
	requests    int
	days        []string
	txns        map[string][]acb.Transaction
	refuseRange bool
}

func (f *automaticBankFixture) client(t *testing.T) *acb.Client {
	t.Helper()
	client, err := acb.NewClient("https://online.acb.com.vn", roundTripFunc(func(req *http.Request) (*http.Response, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests++
		fields := url.Values{}
		if req.Method == http.MethodPost {
			data, err := io.ReadAll(req.Body)
			if err != nil {
				return nil, err
			}
			fields, err = url.ParseQuery(string(data))
			if err != nil {
				return nil, err
			}
		}
		from, to := fields.Get("FromDate"), fields.Get("ToDate")
		var rows strings.Builder
		if req.Method == http.MethodPost {
			f.days = append(f.days, from)
			for _, txn := range f.txns[from] {
				fmt.Fprintf(&rows, `<tr><td>%s</td><td>%s</td><td>%d</td><td>%d</td><td>1000000</td><td>%s</td></tr>`, txn.Number, txn.TransactionAt, txn.Debit, txn.Credit, txn.Description)
			}
			if rows.Len() == 0 {
				rows.WriteString(`<tr><td colspan="6">Không có giao dịch</td></tr>`)
			}
		}
		if f.refuseRange {
			from, to = "01/01/2026", "01/01/2026"
		}
		body := fmt.Sprintf(`<form action="/history"><input name="dse_operationName" value="ibkacctDetailProc"><input name="dse_processorState" value="opaque"><input name="dse_sessionId" value="fixture"><input name="AccountNbr" value="123456"><input name="FromDate" value="%s"><input name="ToDate" value="%s"></form>`, from, to)
		if req.Method == http.MethodPost {
			body += `<table><tr><th>Số GD</th><th>Ngày giao dịch</th><th>Ghi nợ</th><th>Ghi có</th><th>Số dư</th><th>Nội dung giao dịch</th></tr>` + rows.String() + `</table>`
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	return client
}
func (f *automaticBankFixture) count() int { f.mu.Lock(); defer f.mu.Unlock(); return f.requests }

func automaticRecoveryEnv(t *testing.T, gap int) (*storage.Store, storage.Connection, storage.AuthRecoveryEpisode, storage.RecoveryRun) {
	t.Helper()
	ctx := context.Background()
	store, conn, _ := setupTestStoreAndEndpoint(t)
	t.Cleanup(func() { store.Close() })
	now := time.Now().In(acb.DefaultLocation)
	from := now.AddDate(0, 0, -gap+1).Format("2006-01-02")
	// Existing operated connection with an outage checkpoint. Durable coverage
	// provides prior-operation evidence and avoids initial-onboarding baseline.
	if err := store.RecordCoveragePerDay(ctx, conn.ID, map[string]int{from: 0}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveCheckpoint(ctx, storage.Checkpoint{ConnectionID: conn.ID, ScanID: "prior", CoverageFrom: from, CoverageTo: from}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE connections SET state='AUTH_REQUIRED' WHERE id=?`, conn.ID); err != nil {
		t.Fatal(err)
	}
	episode, err := store.EnsureAuthRecoveryEpisode(ctx, conn.ID, conn.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO acb_credentials(connection_id,revision,envelope,key_id,updated_at) VALUES(?,1,X'00','fixture',?)`, conn.ID, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	action, err := store.CreateTelegramAuthAction(ctx, storage.TelegramAuthAction{BotID: 123, ChatID: 456, UserID: 789, EpisodeID: episode.ID, ExpectedGeneration: episode.Generation, Action: "LOGIN"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeliverTelegramAuthAction(ctx, action.ID, 44); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ConsumeTelegramAuthAction(ctx, action.ID, 123, 456, 789, 44, time.Now()); err != nil {
		t.Fatal(err)
	}
	attempt, err := store.StartRecoveryAuthAttempt(ctx, episode.ID, conn.Generation, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	conn, err = store.CompleteAuthSession(ctx, attempt.ID, []byte("test-envelope"))
	if err != nil {
		t.Fatal(err)
	}
	episode, err = store.AuthRecoveryEpisode(ctx, episode.ID)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.GetRecoveryRunByEvent(ctx, conn.ID, conn.Generation, attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	return store, conn, episode, run
}

func assertAutomaticTasksBlocked(t *testing.T, mon *Monitor, conn storage.Connection, f *automaticBankFixture) {
	t.Helper()
	ctx := context.Background()
	before := f.count()
	mon.StartPaymentBoost(100000)
	for _, priority := range []UpstreamPriority{PriorityRealtimePoll, PriorityManualSync} {
		result, err := NewRealtimeTask(mon, priority, conn.ID, conn.Generation).Step(ctx)
		if err != nil || !result.Done {
			t.Fatalf("realtime gate: %+v %v", result, err)
		}
	}
	result, err := NewKeepaliveTask(mon, conn.ID, conn.Generation).Step(ctx)
	if err != nil || !result.Done {
		t.Fatalf("keepalive gate: %+v %v", result, err)
	}
	if err := mon.RequestSync(ctx); err != ErrSyncUnavailable {
		t.Fatalf("manual admission allowed: %v", err)
	}
	day := time.Now().In(acb.DefaultLocation).AddDate(0, 0, -20).Format("2006-01-02")
	job, _, err := mon.store.CreateOrGetHistorySyncJob(ctx, conn.ID, conn.Generation, day, day)
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := mon.store.ClaimNextHistorySyncJob(ctx, time.Now().Add(time.Minute))
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	runner := NewHistoryJobRunner(mon.store, mon.client, nil, nil)
	task := NewHistoryJobTask(runner, claimed, conn)
	start := time.Now()
	result, err = task.Step(ctx)
	if err != nil || !result.Done || result.RequeueAt.Before(start.Add(5*time.Second)) {
		t.Fatalf("history gate: %+v %v", result, err)
	}
	after, err := mon.store.GetHistorySyncJob(ctx, job.ID)
	if err != nil || after.Status != storage.HistoryJobStatusQueued {
		t.Fatalf("blocked history job lost: %+v %v", after, err)
	}
	if f.count() != before {
		t.Fatalf("blocked task contacted bank: before=%d after=%d", before, f.count())
	}
}

func TestAutomaticRecoveryCatchupGate(t *testing.T) {
	ctx := context.Background()
	store, conn, episode, run := automaticRecoveryEnv(t, 7)
	fixture := &automaticBankFixture{txns: make(map[string][]acb.Transaction)}
	mon := New(store, fixture.client(t), 5*time.Second, 15*time.Second)
	// There is deliberately no controller or Telegram process in this test.
	assertAutomaticTasksBlocked(t, mon, conn, fixture)
	if _, err := store.ClaimRecoveryRun(ctx, run.ID, conn.ID, conn.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateRecoveryRunProgress(ctx, run.ID, conn.ID, conn.Generation, storage.RecoveryRunStatusFailed, "{}", "TRANSPORT", "unavailable"); err != nil {
		t.Fatal(err)
	}
	assertAutomaticTasksBlocked(t, mon, conn, fixture)
	if err := store.RetryAuthRecoveryCatchup(ctx, episode.ID, conn.Generation); err != nil {
		t.Fatal(err)
	}
	if err := drainTask(ctx, NewRecoveryCatchUpTask(mon, conn.ID, conn.Generation, run.Reason, run.ID)); err != nil {
		t.Fatal(err)
	}
	blocked, err := store.HasBlockingAuthRecovery(ctx, conn.ID, conn.Generation)
	if err != nil || blocked {
		t.Fatalf("worker did not release gate: %v %v", blocked, err)
	}
	before := fixture.count()
	if _, err := NewRealtimeTask(mon, PriorityRealtimePoll, conn.ID, conn.Generation).Step(ctx); err != nil {
		t.Fatal(err)
	}
	if fixture.count() <= before {
		t.Fatal("boost did not resume after durable completion")
	}
	before = fixture.count()
	if _, err := NewKeepaliveTask(mon, conn.ID, conn.Generation).Step(ctx); err != nil {
		t.Fatal(err)
	}
	if fixture.count() <= before {
		t.Fatal("keepalive did not resume")
	}
	if err := mon.RequestSync(ctx); err != nil {
		t.Fatalf("manual sync did not resume: %v", err)
	}
	claimed, ok, err := store.ClaimNextHistorySyncJob(ctx, time.Now().Add(time.Minute))
	if err != nil || !ok {
		t.Fatalf("history reclaim: %v %v", ok, err)
	}
	before = fixture.count()
	history := NewHistoryJobTask(NewHistoryJobRunner(store, mon.client, nil, nil), claimed, conn)
	for range 4 {
		result, err := history.Step(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if result.Done {
			break
		}
	}
	if fixture.count() <= before {
		t.Fatal("history did not resume bank requests")
	}
	after, err := store.GetHistorySyncJob(ctx, claimed.ID)
	if err != nil || after.Status != storage.HistoryJobStatusCompleted {
		t.Fatalf("history job did not resume: %+v %v", after, err)
	}
}

func TestAutomaticRecoveryLongGap(t *testing.T) {
	ctx := context.Background()
	store, conn, _, run := automaticRecoveryEnv(t, 10)
	now := time.Now().In(acb.DefaultLocation)
	oldest := now.AddDate(0, 0, -9).Format("02/01/2006")
	duplicate := storage.BatchTransactionItem{Number: "EXISTING", TransactionAt: oldest + " 08:00:00", Credit: 100000, Description: "old credit"}
	if _, err := store.IngestTransactionsBatchWithSource(ctx, conn.ID, conn.Generation, conn.AccountMasked, []storage.BatchTransactionItem{duplicate}, true, "BOOTSTRAP"); err != nil {
		t.Fatal(err)
	}
	fixture := &automaticBankFixture{txns: make(map[string][]acb.Transaction)}
	fixture.txns[oldest] = []acb.Transaction{{Number: "EXISTING", TransactionAt: duplicate.TransactionAt, Credit: duplicate.Credit, Description: duplicate.Description}, {Number: "MISSING_OLD", TransactionAt: oldest + " 09:00:00", Credit: 200000}}
	for _, offset := range []int{-2, 0} {
		day := now.AddDate(0, 0, offset).Format("02/01/2006")
		fixture.txns[day] = []acb.Transaction{{Number: fmt.Sprintf("MISSING_%d", offset), TransactionAt: day + " 09:00:00", Credit: 300000}}
	}
	mon := New(store, fixture.client(t), 5*time.Second, 15*time.Second)
	mon.now = func() time.Time { return now }
	task := NewRecoveryCatchUpTask(mon, conn.ID, conn.Generation, run.Reason, run.ID)
	previousNext, previousCoverage := run.NextDay, ""
	steps := 0
	for {
		result, err := task.Step(ctx)
		if err != nil {
			t.Fatal(err)
		}
		steps++
		current, err := store.GetRecoveryRun(ctx, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		cp, err := store.GetCheckpoint(ctx, conn.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.NextDay < previousNext || cp.CoverageTo < previousCoverage {
			t.Fatalf("cursor/checkpoint regressed: %+v %+v", current, cp)
		}
		previousNext, previousCoverage = current.NextDay, cp.CoverageTo
		if result.Done {
			break
		}
		if steps > 11 {
			t.Fatal("unbounded day loop")
		}
	}
	if steps != 10 {
		t.Fatalf("long outage clamped or did not yield daily: steps=%d", steps)
	}
	for i := range 10 {
		day := now.AddDate(0, 0, -9+i).Format("02/01/2006")
		seen := false
		for _, requested := range fixture.days {
			if requested == day {
				seen = true
				break
			}
		}
		if !seen {
			t.Fatalf("missing requested outage day %s", day)
		}
	}
	for table, want := range map[string]int{"transactions": 4, "events": 3, "deliveries": 3} {
		var count int
		if err := store.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil || count != want {
			t.Fatalf("%s: %d want %d err=%v", table, count, want, err)
		}
	}
	blocked, err := store.HasBlockingAuthRecovery(ctx, conn.ID, conn.Generation)
	if err != nil || blocked {
		t.Fatalf("long gap unresolved: %v %v", blocked, err)
	}
}

func TestAutomaticRecoveryRejectsUnconfirmedHistoryRange(t *testing.T) {
	ctx := context.Background()
	store, conn, _, run := automaticRecoveryEnv(t, 10)
	fixture := &automaticBankFixture{txns: make(map[string][]acb.Transaction), refuseRange: true}
	mon := New(store, fixture.client(t), 5*time.Second, 15*time.Second)
	_, err := NewRecoveryCatchUpTask(mon, conn.ID, conn.Generation, run.Reason, run.ID).Step(ctx)
	if err == nil {
		t.Fatal("bank's different date range was accepted")
	}
	current, err := store.GetRecoveryRun(ctx, run.ID)
	if err != nil || current.Status != storage.RecoveryRunStatusFailed || current.ErrorCode != "HISTORY_RANGE_UNAVAILABLE" || current.NextDay != run.NextDay {
		t.Fatalf("unavailable range advanced coverage: %+v %v", current, err)
	}
	blocked, err := store.HasBlockingAuthRecovery(ctx, conn.ID, conn.Generation)
	if err != nil || !blocked {
		t.Fatalf("unavailable range released gate: %v %v", blocked, err)
	}
}

func TestAutomaticRecoveryGateLookupFailureFailsClosed(t *testing.T) {
	ctx := context.Background()
	store, conn, _, _ := automaticRecoveryEnv(t, 7)
	fixture := &automaticBankFixture{txns: make(map[string][]acb.Transaction)}
	mon := New(store, fixture.client(t), 5*time.Second, 15*time.Second)
	// A schema-unavailable read must not turn payment boost into a gate bypass.
	if _, err := store.DB().ExecContext(ctx, `ALTER TABLE auth_recovery_episodes RENAME TO unavailable_episodes`); err != nil {
		t.Fatal(err)
	}
	mon.StartPaymentBoost(100000)
	for _, priority := range []UpstreamPriority{PriorityRealtimePoll, PriorityManualSync} {
		result, err := NewRealtimeTask(mon, priority, conn.ID, conn.Generation).Step(ctx)
		if err != nil || result.Done || result.RequeueAt.IsZero() {
			t.Fatalf("gate error did not defer realtime: %+v %v", result, err)
		}
	}
	result, err := NewKeepaliveTask(mon, conn.ID, conn.Generation).Step(ctx)
	if err != nil || result.Done || result.RequeueAt.IsZero() {
		t.Fatalf("gate error did not defer keepalive: %+v %v", result, err)
	}
	if err := mon.RequestSync(ctx); err != ErrSyncUnavailable {
		t.Fatalf("gate error admitted sync: %v", err)
	}
	day := time.Now().In(acb.DefaultLocation).Format("2006-01-02")
	job, _, err := store.CreateOrGetHistorySyncJob(ctx, conn.ID, conn.Generation, day, day)
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.ClaimNextHistorySyncJob(ctx, time.Now())
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	result, err = NewHistoryJobTask(NewHistoryJobRunner(store, mon.client, nil, nil), claimed, conn).Step(ctx)
	if err != nil || !result.Done || result.RequeueAt.IsZero() {
		t.Fatalf("gate error did not requeue history: %+v %v", result, err)
	}
	after, err := store.GetHistorySyncJob(ctx, job.ID)
	if err != nil || after.Status != storage.HistoryJobStatusQueued {
		t.Fatalf("gate error lost history job: %+v %v", after, err)
	}
	if fixture.count() != 0 {
		t.Fatalf("gate error made %d bank requests", fixture.count())
	}
}
