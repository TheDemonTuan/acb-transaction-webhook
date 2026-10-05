package monitor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
	"github.com/thedemontuan/acb-transaction-webhook/internal/authbrowser"
	"github.com/thedemontuan/acb-transaction-webhook/internal/scheduler"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

const unavailableDay = "2026-09-25"
const unavailableDisplayDay = "25/09/2026"
const unavailableAccount = "12345678"

// A synthetic bank conversation with strict token/event/range transitions. Real
// Client methods perform the wrong-detail GET/replay/reset; no sentinel is mocked.
type unavailableHistoryBank struct {
	t          *testing.T
	mode       string
	stage      string
	token      int
	probes     int
	scan       int
	requests   []unavailableHistoryRequest
	beforePost func(string)
}

type unavailableHistoryRequest struct {
	method string
	event  string
	from   string
	to     string
}

func (b *unavailableHistoryBank) form(operation string) string {
	return fmt.Sprintf(`<form action="/acbib/Request"><input name="dse_operationName" value="%s"><input name="dse_processorState" value="state-%d"><input name="dse_sessionId" value="session-%d"><input name="AccountNbr" value="%s"></form>`, operation, b.token, b.token, unavailableAccount)
}

func (b *unavailableHistoryBank) page(last bool) string {
	number, credit, debit, description := "TX_DAY_1", "100.000", "0", "Synthetic credit"
	nav := `<a href="/acbib/Request" onclick="submitEvent('nextPage')">Trang sau</a>`
	if last {
		number, credit, debit, description = "TX_DAY_2", "0", "50.000", "Synthetic debit"
		nav = `<span class="disabled">Trang sau</span>`
	}
	return b.form("ibkacctDetailProc") + fmt.Sprintf(`<table><tr><th>Ngày hiệu lực</th><th>Ngày giao dịch</th><th>Số GD</th><th>Ghi nợ</th><th>Ghi có</th><th>Nội dung giao dịch</th></tr><tr><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr><tr><td colspan="6">%s</td></tr></table><div>Tổng số dòng: 2</div>`, unavailableDisplayDay, unavailableDisplayDay, number, debit, credit, description, nav)
}

func (b *unavailableHistoryBank) RoundTrip(r *http.Request) (*http.Response, error) {
	b.t.Helper()
	if r.URL.Hostname() != acb.OfficialHost || r.URL.Path != "/acbib/Request" {
		b.t.Fatal("fixture escaped the official synthetic destination")
	}
	if !strings.Contains(r.Header.Get("Cookie"), "JSESSIONID=synthetic") {
		b.t.Fatal("history recovery discarded its authenticated cookie")
	}
	fields := url.Values{}
	if r.Body != nil {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			b.t.Fatal(err)
		}
		fields, err = url.ParseQuery(string(body))
		if err != nil {
			b.t.Fatal(err)
		}
	}
	b.requests = append(b.requests, unavailableHistoryRequest{method: r.Method, event: fields.Get("dse_nextEventName"), from: fields.Get("FromDate"), to: fields.Get("ToDate")})
	respond := func(body string) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	}
	if r.Method == http.MethodGet {
		b.probes++
		if b.probes > 1 || b.stage != "probe" {
			b.t.Fatal("consumer exceeded the bounded recovery probe or probed out of sequence")
		}
		b.token++
		if b.mode == "continuation_unavailable" {
			b.stage = "terminal"
			return respond(b.form("ibkacctSumProc"))
		}
		if b.mode == "initial_unavailable" {
			b.stage = "replay_fail"
		} else {
			b.stage = "bootstrap"
		}
		return respond(b.form("ibkacctDetailProc"))
	}
	if r.Method != http.MethodPost || fields.Get("dse_processorState") != fmt.Sprintf("state-%d", b.token) || fields.Get("dse_sessionId") != fmt.Sprintf("session-%d", b.token) || fields.Get("dse_operationName") != "ibkacctDetailProc" || fields.Get("AccountNbr") != unavailableAccount {
		b.t.Fatal("consumer POST did not use the current exact-account conversation")
	}
	if fields.Has("_raw") || fields.Has("_explicitRange") {
		b.t.Fatal("consumer leaked bookkeeping to the bank")
	}
	if b.beforePost != nil {
		b.beforePost(b.stage)
	}
	b.token++
	switch b.stage {
	case "bootstrap":
		if fields.Get("dse_nextEventName") != "byDate" {
			b.t.Fatal("bootstrap did not restart the history conversation")
		}
		b.stage = "page1"
		return respond(b.form("ibkacctDetailProc"))
	case "page1", "page2", "replay_fail":
		if fields.Get("FromDate") != unavailableDisplayDay || fields.Get("ToDate") != "27/09/2026" || fields.Get("activeDatetimeYN") != "N" {
			b.t.Fatal("recovery changed the original historical effective-date range")
		}
		if b.stage == "page1" {
			if fields.Get("dse_nextEventName") != "byDate" {
				b.t.Fatal("day restart replayed a continuation instead of page one")
			}
			b.scan++
			if b.mode == "initial_unavailable" {
				b.stage = "probe"
				return respond(b.form("ibkacctDetailProc"))
			}
			b.stage = "page2"
			return respond(b.page(false))
		}
		if b.stage == "replay_fail" {
			if fields.Get("dse_nextEventName") != "byDate" {
				b.t.Fatal("bounded recovery did not replay page one")
			}
			b.stage = "terminal"
			return respond(b.form("ibkacctDetailProc"))
		}
		if fields.Get("dse_nextEventName") != "nextPage" {
			b.t.Fatal("continuation lost its server-generated event")
		}
		if b.scan == 1 {
			b.stage = "probe"
			return respond(b.form("ibkacctDetailProc"))
		}
		b.stage = "complete"
		return respond(b.page(true))
	default:
		b.t.Fatalf("unexpected bank stage: %s", b.stage)
		return nil, errors.New("unreachable bank stage")
	}
}

func newUnavailableHistoryClient(t *testing.T, mode string) (*acb.Client, *unavailableHistoryBank) {
	t.Helper()
	bank := &unavailableHistoryBank{t: t, mode: mode, stage: "bootstrap", token: 1}
	client, err := acb.NewClient("https://"+acb.OfficialHost, bank)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.RestoreSession(authbrowser.Handoff{
		Version: 1, Action: "https://" + acb.OfficialHost + "/acbib/Request",
		Fields:  map[string]string{"dse_operationName": "ibkacctDetailProc", "dse_processorState": "state-1", "dse_sessionId": "session-1", "AccountNbr": unavailableAccount},
		Cookies: []authbrowser.Cookie{{Name: "JSESSIONID", Value: "synthetic", Domain: acb.OfficialHost, Path: "/", Secure: true}},
	}); err != nil {
		t.Fatal(err)
	}
	return client, bank
}

type unavailableDurableState struct {
	checkpoint storage.Checkpoint
	coverage   string
	rows       int
	lastSync   string
	txns       int
	events     int
}

func unavailableState(t *testing.T, store *storage.Store, conn storage.Connection) unavailableDurableState {
	t.Helper()
	ctx := context.Background()
	cp, err := store.GetCheckpoint(ctx, conn.ID)
	if err != nil || cp == nil {
		t.Fatalf("read seeded checkpoint: %v", err)
	}
	state := unavailableDurableState{checkpoint: *cp}
	if err := store.DB().QueryRowContext(ctx, "SELECT status,rows_seen,last_sync_at FROM history_coverage WHERE connection_id=? AND day=?", conn.ID, "2026-09-24").Scan(&state.coverage, &state.rows, &state.lastSync); err != nil {
		t.Fatal(err)
	}
	for _, query := range []struct {
		sql string
		out *int
	}{{"SELECT COUNT(*) FROM transactions", &state.txns}, {"SELECT COUNT(*) FROM events", &state.events}} {
		if err := store.DB().QueryRowContext(ctx, query.sql).Scan(query.out); err != nil {
			t.Fatal(err)
		}
	}
	return state
}

func seedUnavailableDurability(t *testing.T, store *storage.Store, conn storage.Connection, sameDay bool) unavailableDurableState {
	t.Helper()
	ctx := context.Background()
	if err := store.RecordCoveragePerDay(ctx, conn.ID, map[string]int{"2026-09-24": 1}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveCheckpoint(ctx, storage.Checkpoint{ConnectionID: conn.ID, ScanID: "preserved", CoverageFrom: "2026-09-24", CoverageTo: "2026-09-24"}); err != nil {
		t.Fatal(err)
	}
	item := storage.BatchTransactionItem{Number: "PRIOR", TransactionAt: "24/09/2026", EffectiveAt: "24/09/2026", Credit: 100000, Description: "Synthetic prior credit"}
	if sameDay {
		item.Number, item.TransactionAt, item.EffectiveAt, item.Description = "TX_DAY_1", unavailableDisplayDay, unavailableDisplayDay, "Synthetic credit"
	}
	if _, err := store.IngestTransactionsBatch(ctx, conn.ID, conn.Generation, conn.AccountMasked, []storage.BatchTransactionItem{item}, false); err != nil {
		t.Fatal(err)
	}
	return unavailableState(t, store, conn)
}

func assertUnavailableDurability(t *testing.T, store *storage.Store, conn storage.Connection, before unavailableDurableState) {
	t.Helper()
	if after := unavailableState(t, store, conn); !reflect.DeepEqual(before, after) {
		t.Fatal("failed/incomplete day changed checkpoint, existing coverage, transactions, or events")
	}
	var coverage int
	if err := store.DB().QueryRowContext(context.Background(), "SELECT COUNT(*) FROM history_coverage WHERE connection_id=? AND day=?", conn.ID, unavailableDay).Scan(&coverage); err != nil || coverage != 0 {
		t.Fatalf("incomplete day acquired coverage: count=%d err=%v", coverage, err)
	}
	current, err := store.Connection(context.Background())
	if err != nil || current.State != "MONITORING" || current.Generation != conn.Generation {
		t.Fatalf("protocol recovery changed connection auth/generation: %v", err)
	}
}

func assertUnavailableSequence(t *testing.T, bank *unavailableHistoryBank, continuation bool, success bool) {
	t.Helper()
	want := []string{"POST:byDate", "POST:byDate"}
	if continuation {
		want = append(want, "POST:nextPage", "GET:")
		if success {
			want = append(want, "POST:byDate", "POST:byDate", "POST:nextPage")
		}
	} else {
		want = append(want, "GET:", "POST:byDate")
	}
	got := make([]string, len(bank.requests))
	for i, request := range bank.requests {
		got[i] = request.method + ":" + request.event
	}
	if !reflect.DeepEqual(got, want) || bank.probes != 1 {
		t.Fatalf("bounded recovery sequence=%v want=%v probes=%d", got, want, bank.probes)
	}
}

func TestCatchUpHistoryUnavailableIsTerminalWithoutDurableDayChanges(t *testing.T) {
	for _, mode := range []string{"initial_unavailable", "continuation_unavailable"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			store, conn, _ := setupRecoveryTestEnv(t, "unavailable_catchup.db")
			defer store.Close()
			before := seedUnavailableDurability(t, store, conn, false)
			client, bank := newUnavailableHistoryClient(t, mode)
			mon := New(store, client, 5*time.Second, 15*time.Second)
			mon.now = func() time.Time { return time.Date(2026, 10, 5, 12, 0, 0, 0, acb.DefaultLocation) }
			run, _, err := store.EnsureRecoveryRunWithPlan(ctx, conn.ID, conn.Generation, mode, storage.RecoveryRunPlan{Reason: "WORKER_STARTUP", RangeFrom: unavailableDay, RangeTo: unavailableDay, NextDay: unavailableDay})
			if err != nil {
				t.Fatal(err)
			}
			task := NewRecoveryCatchUpTask(mon, conn.ID, conn.Generation, run.Reason, run.ID)
			result, err := task.Step(ctx)
			if !errors.Is(err, acb.ErrHistoryUnavailable) || !result.Done || !result.RequeueAt.IsZero() || result.Outcome != scheduler.OutcomeFatal || mon.IsBackoffActive() {
				t.Fatalf("protocol failure was retried/backed off instead of terminal: result=%+v err=%v", result, err)
			}
			failed, err := store.GetRecoveryRun(ctx, run.ID)
			if err != nil || failed.Status != storage.RecoveryRunStatusFailed || failed.ErrorCode != "HISTORY_UNAVAILABLE" || failed.ErrorMessage != "HISTORY_UNAVAILABLE" || failed.NextDay != unavailableDay {
				t.Fatalf("wrong terminal recovery state: %+v err=%v", failed, err)
			}
			polls, err := store.ListPollRuns(ctx, 1)
			if err != nil || len(polls) != 1 || polls[0].Status != "FAILED" || polls[0].Error != "HISTORY_UNAVAILABLE" || polls[0].Classifier != string(acb.AccountDetailPage) || polls[0].HTTPStatus != http.StatusOK {
				t.Fatalf("wrong terminal poll metadata: %+v err=%v", polls, err)
			}
			assertUnavailableDurability(t, store, conn, before)
			assertUnavailableSequence(t, bank, mode == "continuation_unavailable", false)
			open, err := store.ListOpenRecoveryRuns(ctx, conn.ID, conn.Generation)
			if err != nil || len(open) != 0 {
				t.Fatal("failed recovery remained automatically runnable")
			}
		})
	}
}

func TestHistoryJobUnavailableFailsWithoutRequeueOrFailedPageIngestion(t *testing.T) {
	for _, mode := range []string{"initial_unavailable", "continuation_unavailable"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			store, conn, _ := setupRecoveryTestEnv(t, "unavailable_job.db")
			defer store.Close()
			before := seedUnavailableDurability(t, store, conn, false)
			client, bank := newUnavailableHistoryClient(t, mode)
			mon := New(store, client, 5*time.Second, 15*time.Second)
			runner := NewHistoryJobRunner(store, client, nil, nil).WithMonitor(mon)
			job, _, err := store.CreateOrGetHistorySyncJob(ctx, conn.ID, conn.Generation, unavailableDay, unavailableDay)
			if err != nil {
				t.Fatal(err)
			}
			claimed, ok, err := store.ClaimNextHistorySyncJob(ctx, time.Now())
			if err != nil || !ok || claimed.ID != job.ID {
				t.Fatalf("claim job: %v", err)
			}
			result, err := NewHistoryJobTask(runner, claimed, conn).Step(ctx)
			if !errors.Is(err, acb.ErrHistoryUnavailable) || !result.Done || !result.RequeueAt.IsZero() || result.Outcome != scheduler.OutcomeFatal || mon.IsBackoffActive() {
				t.Fatalf("protocol job failure was retried/backed off: result=%+v err=%v", result, err)
			}
			failed, err := store.GetHistorySyncJob(ctx, job.ID)
			if err != nil || failed.Status != storage.HistoryJobStatusFailed || failed.ErrorCode != "HISTORY_UNAVAILABLE" || failed.ErrorMessage != "HISTORY_UNAVAILABLE" || failed.NextAttemptAt != "" {
				t.Fatalf("job did not fail terminally: %+v err=%v", failed, err)
			}
			if mode == "continuation_unavailable" {
				// Jobs retain parsed valid prior pages; only failed-page data and
				// incomplete-day coverage/checkpoint must be rejected.
				before.txns++
				var validRows int
				if err := store.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM transactions WHERE semantic_key='ACB:TX_DAY_1' AND credit=100000 AND debit=0 AND ingest_source='FILTER_SYNC'").Scan(&validRows); err != nil || validRows != 1 {
					t.Fatalf("valid prior-page history was lost: count=%d err=%v", validRows, err)
				}
			}
			assertUnavailableDurability(t, store, conn, before)
			assertUnavailableSequence(t, bank, mode == "continuation_unavailable", false)
			if _, ok, err := store.ClaimNextHistorySyncJob(ctx, time.Now().Add(time.Hour)); err != nil || ok {
				t.Fatal("protocol-failed job was automatically requeued")
			}
		})
	}
}

func assertUnavailableCompletedDay(t *testing.T, store *storage.Store, conn storage.Connection, before unavailableDurableState, catchup bool) {
	t.Helper()
	ctx := context.Background()
	after := unavailableState(t, store, conn)
	if after.txns != before.txns+1 || after.events != before.events || after.coverage != before.coverage || after.rows != before.rows || after.lastSync != before.lastSync {
		t.Fatal("day restart duplicated transactions/events or changed existing historical coverage")
	}
	if !catchup && after.checkpoint != before.checkpoint {
		t.Fatal("FILTER_SYNC history job changed the realtime checkpoint")
	}
	if catchup && (after.checkpoint.CoverageFrom != unavailableDay || after.checkpoint.CoverageTo != unavailableDay) {
		t.Fatal("catch-up did not advance checkpoint to the fully completed original day")
	}
	var status string
	var rows int
	if err := store.DB().QueryRowContext(ctx, "SELECT status,rows_seen FROM history_coverage WHERE connection_id=? AND day=?", conn.ID, unavailableDay).Scan(&status, &rows); err != nil || status != "COMPLETE" || rows != 2 {
		t.Fatalf("full restarted day did not commit exact coverage: status=%s rows=%d err=%v", status, rows, err)
	}
	for _, expected := range []struct {
		number string
		credit int
		debit  int
	}{{"TX_DAY_1", 100000, 0}, {"TX_DAY_2", 0, 50000}} {
		var count int
		if err := store.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM transactions WHERE connection_id=? AND semantic_key=? AND credit=? AND debit=? AND transaction_day=?", conn.ID, "ACB:"+expected.number, expected.credit, expected.debit, unavailableDay).Scan(&count); err != nil || count != 1 {
			t.Fatalf("completed day lost/duplicated its exact synthetic identity/amount/date: count=%d err=%v", count, err)
		}
	}
	var conflicts int
	if err := store.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM transaction_quarantine").Scan(&conflicts); err != nil || conflicts != 0 {
		t.Fatalf("restart created conflicting dedupe payloads: count=%d err=%v", conflicts, err)
	}
	current, err := store.Connection(ctx)
	if err != nil || current.State != "MONITORING" || current.Generation != conn.Generation {
		t.Fatalf("conversation reset changed confirmed authentication state: %v", err)
	}
}

func TestHistoryDayContinuationResetRestartsOriginalDayAndDeduplicates(t *testing.T) {
	for _, consumer := range []string{"catchup", "history_job"} {
		t.Run(consumer, func(t *testing.T) {
			ctx := context.Background()
			store, conn, _ := setupRecoveryTestEnv(t, "reset_dedupe.db")
			defer store.Close()
			before := seedUnavailableDurability(t, store, conn, true)
			client, bank := newUnavailableHistoryClient(t, "continuation_reset")
			mon := New(store, client, 5*time.Second, 15*time.Second)
			mon.now = func() time.Time { return time.Date(2026, 10, 5, 12, 0, 0, 0, acb.DefaultLocation) }
			// Observe at each POST, including final-page fetch: coverage and
			// checkpoint remain unchanged until that final page has returned.
			bank.beforePost = func(stage string) {
				assertUnavailableDurability(t, store, conn, before)
			}
			var step func(context.Context) (scheduler.TaskStepResult, error)
			var runID, jobID string
			if consumer == "catchup" {
				run, _, err := store.EnsureRecoveryRunWithPlan(ctx, conn.ID, conn.Generation, "reset-dedupe", storage.RecoveryRunPlan{Reason: "WORKER_STARTUP", RangeFrom: unavailableDay, RangeTo: unavailableDay, NextDay: unavailableDay})
				if err != nil {
					t.Fatal(err)
				}
				runID = run.ID
				step = NewRecoveryCatchUpTask(mon, conn.ID, conn.Generation, run.Reason, run.ID).Step
			} else {
				runner := NewHistoryJobRunner(store, client, nil, nil).WithMonitor(mon)
				job, _, err := store.CreateOrGetHistorySyncJob(ctx, conn.ID, conn.Generation, unavailableDay, unavailableDay)
				if err != nil {
					t.Fatal(err)
				}
				claimed, ok, err := store.ClaimNextHistorySyncJob(ctx, time.Now())
				if err != nil || !ok || claimed.ID != job.ID {
					t.Fatalf("claim restart job: %v", err)
				}
				jobID = job.ID
				step = NewHistoryJobTask(runner, claimed, conn).Step
			}
			done := false
			for range 3 {
				result, err := step(ctx)
				if err != nil || !result.RequeueAt.IsZero() || result.Outcome != scheduler.OutcomeSuccess {
					t.Fatalf("restarted day did not complete without retries/auth transition: result=%+v err=%v", result, err)
				}
				if result.Done {
					done = true
					break
				}
			}
			if !done || bank.stage != "complete" || mon.IsBackoffActive() {
				t.Fatal("restarted day failed to reach bounded complete state")
			}
			assertUnavailableSequence(t, bank, true, true)
			assertUnavailableCompletedDay(t, store, conn, before, consumer == "catchup")
			if runID != "" {
				run, err := store.GetRecoveryRun(ctx, runID)
				if err != nil || run.Status != storage.RecoveryRunStatusCompleted || run.NextDay != "2026-09-26" {
					t.Fatalf("reset recovery did not commit original day cursor: %+v err=%v", run, err)
				}
			} else {
				job, err := store.GetHistorySyncJob(ctx, jobID)
				if err != nil || job.Status != storage.HistoryJobStatusCompleted {
					t.Fatalf("reset history job did not complete: %+v err=%v", job, err)
				}
			}
		})
	}
}
