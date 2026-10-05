package monitor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
	"github.com/thedemontuan/acb-transaction-webhook/internal/authbrowser"
	"github.com/thedemontuan/acb-transaction-webhook/internal/authsession"
	"github.com/thedemontuan/acb-transaction-webhook/internal/scheduler"
	"github.com/thedemontuan/acb-transaction-webhook/internal/security"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

const protocolRecoveryDate = "05/10/2026"

// This is a synthetic token-consumption model, not captured bank markup.
// The wrong-page form is never history-capable: only a fresh GET can repair it.
type protocolRecoveryBank struct {
	mu               sync.Mutex
	state            int
	timeoutNext      bool
	repeatDetail     bool
	summaryProbe     bool
	incompleteDetail bool
	requestDate      string
	pages            int
	page             int
	byDateCalls      int
	invalidateAfter  int
	events           []string
	failure          string
	blockPhase       string
	entered          chan struct{}
	release          chan struct{}
}

func (b *protocolRecoveryBank) token() string { return fmt.Sprintf("token-%d", b.state) }

func protocolRecoveryForm(token, operation string) string {
	return fmt.Sprintf(`<form action="/acbib/Request" method="POST"><input name="dse_operationName" value="%s"><input name="dse_processorState" value="%s"><input name="dse_sessionId" value="synthetic"><input name="AccountNbr" value="12341234"><input name="FromDate" value="04/10/2026"><input name="ToDate" value="04/10/2026"></form>`, operation, token)
}

func protocolRecoveryIncompleteForm() string {
	return `<form action="/acbib/Request"><input name="dse_operationName" value="ibkacctDetailProc"><input name="dse_sessionId" value="synthetic"><input name="AccountNbr" value="12341234"></form>`
}

func (b *protocolRecoveryBank) markup(token string) string {
	if b.pages == 0 {
		return protocolRecoveryForm(token, "ibkacctDetailProc") + `<table><tr><th>Ngày hiệu lực</th><th>Ngày giao dịch</th><th>Số GD</th><th>Ghi nợ</th><th>Ghi có</th><th>Số dư</th><th>Nội dung giao dịch</th></tr><tr><td>05/10/2026</td><td>05/10/2026</td><td>TX101</td><td>0</td><td>100.000</td><td>1.000.000</td><td>Synthetic incoming</td></tr><tr><td>05/10/2026</td><td>05/10/2026</td><td>TX102</td><td>50.000</td><td>0</td><td>950.000</td><td>Synthetic outgoing</td></tr><tr><td colspan="7"><span class="disabled">Trang sau</span></td></tr></table><div>Tổng số dòng: 2</div>`
	}
	navigation := `<span class="disabled">Trang sau</span>`
	if b.page < b.pages {
		navigation = `<a href="/acbib/Request" onclick="submitEvent('nextPage')">Trang sau</a>`
	}
	return protocolRecoveryForm(token, "ibkacctDetailProc") + fmt.Sprintf(`<table><tr><th>Ngày hiệu lực</th><th>Ngày giao dịch</th><th>Số GD</th><th>Ghi nợ</th><th>Ghi có</th></tr><tr><td>05/10/2026</td><td>05/10/2026</td><td>TX%d</td><td>0</td><td>100</td></tr><tr><td colspan="5">%s</td></tr></table><div>Tổng số dòng: %d</div>`, b.page, navigation, b.pages)
}

func (b *protocolRecoveryBank) RoundTrip(request *http.Request) (*http.Response, error) {
	b.mu.Lock()
	phase := "post"
	var body string
	if request.URL.Host != acb.OfficialHost || request.URL.Path != "/acbib/Request" {
		b.failure = "unexpected destination"
	}
	if request.Method == http.MethodGet {
		phase = "get"
		b.state++
		operation := "ibkacctDetailProc"
		if b.summaryProbe {
			operation = "ibkacctSumProc"
		}
		body = protocolRecoveryForm(b.token(), operation)
	} else {
		if request.Method != http.MethodPost || request.ParseForm() != nil {
			b.failure = "unexpected method or malformed form"
		}
		fields := request.PostForm
		requestDate := b.requestDate
		if requestDate == "" {
			requestDate = protocolRecoveryDate
		}
		if fields.Get("AccountNbr") != "12341234" || fields.Get("FromDate") != requestDate || fields.Get("ToDate") != requestDate || fields.Get("activeDatetimeYN") != "N" || fields.Get("dse_sessionId") != "synthetic" || fields.Get("dse_operationName") != "ibkacctDetailProc" || fields.Has("_raw") || fields.Has("_explicitRange") {
			b.failure = "outbound pinned form contract changed"
		}
		event := fields.Get("dse_nextEventName")
		if event != "byDate" && event != "nextPage" {
			b.failure = "navigation event changed"
		}
		if fields.Get("dse_processorState") != b.token() {
			phase = "stale"
			body = protocolRecoveryForm("detail-only", "ibkacctDetailProc")
			if b.incompleteDetail {
				body = protocolRecoveryIncompleteForm()
			}
		} else {
			b.state++
			if b.timeoutNext {
				b.timeoutNext = false
				b.events = append(b.events, "timeout")
				b.mu.Unlock()
				// Model a response-header timeout after the server consumed state.
				return nil, context.DeadlineExceeded
			}
			if b.repeatDetail {
				phase = "detail"
				body = protocolRecoveryForm("detail-only", "ibkacctDetailProc")
				if b.incompleteDetail {
					body = protocolRecoveryIncompleteForm()
				}
			} else {
				if event == "byDate" {
					b.page = 1
					b.byDateCalls++
				} else {
					b.page++
				}
				body = b.markup(b.token())
				if b.invalidateAfter > 0 && b.byDateCalls >= 2 && b.page == b.invalidateAfter {
					b.state++
				}
			}
		}
	}
	b.events = append(b.events, phase)
	block := b.blockPhase == phase
	if block {
		b.blockPhase = ""
	}
	b.mu.Unlock()
	if block {
		close(b.entered)
		select {
		case <-b.release:
		case <-request.Context().Done():
			return nil, request.Context().Err()
		}
	}
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
}

func protocolRecoveryFixture(t *testing.T, bank *protocolRecoveryBank) (*storage.Store, storage.Connection, *security.Keyring, *acb.Client, *SessionLoader, *Monitor) {
	t.Helper()
	store, conn, keyring, _ := sessionFenceFixture(t)
	client, err := acb.NewClient("https://"+acb.OfficialHost, bank)
	if err != nil {
		t.Fatal(err)
	}
	handoff := authbrowser.Handoff{Version: 1, URL: "https://online.acb.com.vn/acbib/Request", Action: "https://online.acb.com.vn/acbib/Request", Fields: map[string]string{"dse_operationName": "ibkacctDetailProc", "dse_processorState": "token-0", "dse_sessionId": "synthetic", "AccountNbr": "12341234"}, Cookies: []authbrowser.Cookie{{Name: "session", Value: "synthetic", Domain: acb.OfficialHost, Path: "/", Secure: true}}}
	if err := client.RestoreSession(handoff); err != nil {
		t.Fatal(err)
	}
	loader := NewSessionLoader(store, keyring, client)
	if err := loader.Persist(context.Background(), conn.ID, conn.Generation); err != nil {
		t.Fatal(err)
	}
	if err := loader.Restore(context.Background(), conn.ID, conn.Generation); err != nil {
		t.Fatal(err)
	}
	mon := New(store, client, time.Second, time.Second).WithSessionLoader(loader)
	mon.now = func() time.Time { return time.Date(2026, 10, 5, 12, 0, 0, 0, acb.DefaultLocation) }
	return store, conn, keyring, client, loader, mon
}

func protocolRecoveryStep(t *testing.T, mon *Monitor, conn storage.Connection, success bool) *RealtimeTask {
	t.Helper()
	task := NewRealtimeTask(mon, PriorityRealtimePoll, conn.ID, conn.Generation)
	result, err := task.Step(context.Background())
	if success && (err != nil || !result.Done || result.Outcome != scheduler.OutcomeSuccess) {
		t.Fatalf("poll result=%+v err=%v", result, err)
	}
	return task
}

func protocolRecoveryCount(t *testing.T, store *storage.Store, table string) int {
	t.Helper()
	var count int
	if err := store.DB().QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func protocolRecoveryEvents(t *testing.T, bank *protocolRecoveryBank, expected string) {
	t.Helper()
	bank.mu.Lock()
	defer bank.mu.Unlock()
	if bank.failure != "" || strings.Join(bank.events, ",") != expected {
		t.Fatalf("contract=%s phases=%v expected=%s", bank.failure, bank.events, expected)
	}
}

func TestRealtimeTask_DetailRecoveryFromConsumedTokenAndPersistedSnapshot(t *testing.T) {
	for _, tc := range []struct{ restart, incomplete bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
		t.Run(fmt.Sprintf("restart=%t/incomplete-bootstrap=%t", tc.restart, tc.incomplete), func(t *testing.T) {
			ctx := context.Background()
			bank := &protocolRecoveryBank{incompleteDetail: tc.incomplete}
			store, conn, keyring, _, _, mon := protocolRecoveryFixture(t, bank)
			protocolRecoveryStep(t, mon, conn, true)
			before, err := store.Session(ctx, conn.ID, conn.Generation)
			if err != nil {
				t.Fatal(err)
			}
			bank.timeoutNext = true
			protocolRecoveryStep(t, mon, conn, false)
			if !mon.IsBackoffActive() {
				t.Fatal("timeout did not retain existing network backoff policy")
			}
			afterTimeout, err := store.Session(ctx, conn.ID, conn.Generation)
			if err != nil || !bytes.Equal(before.Envelope, afterTimeout.Envelope) {
				t.Fatalf("timeout persisted unproved form: %v", err)
			}
			mon.ClearBackoff() // Advance past the existing retry boundary, without retrying the fault request.
			if tc.restart {
				client, err := acb.NewClient("https://"+acb.OfficialHost, bank)
				if err != nil {
					t.Fatal(err)
				}
				mon = New(store, client, time.Second, time.Second).WithSessionLoader(NewSessionLoader(store, keyring, client))
				mon.now = func() time.Time { return time.Date(2026, 10, 5, 12, 0, 0, 0, acb.DefaultLocation) }
			}
			protocolRecoveryStep(t, mon, conn, true)
			phases := "post,post,timeout,stale,stale,get,post"
			if tc.incomplete {
				phases = "post,post,timeout,stale,get,post,post"
			}
			protocolRecoveryEvents(t, bank, phases)
			freshClient, err := acb.NewClient("https://"+acb.OfficialHost, bank)
			if err != nil {
				t.Fatal(err)
			}
			freshLoader := NewSessionLoader(store, keyring, freshClient)
			if err := freshLoader.Restore(ctx, conn.ID, conn.Generation); err != nil {
				t.Fatal(err)
			}
			fresh, err := freshClient.SnapshotSession()
			if err != nil || fresh.Fields["dse_processorState"] != bank.token() || fresh.Fields["AccountNbr"] != "12341234" {
				t.Fatalf("successful persisted snapshot not fresh: %v", err)
			}
			protocolRecoveryStep(t, mon, conn, true)
			protocolRecoveryEvents(t, bank, phases+",post,post")
			polls, err := store.ListPollRuns(ctx, 10)
			if err != nil || len(polls) != 4 {
				t.Fatalf("polls=%+v err=%v", polls, err)
			}
			for i, poll := range polls {
				if i == 2 {
					if poll.Status != "FAILED" || poll.Error != "ACB_REQUEST_TIMEOUT" {
						t.Fatalf("timeout poll=%+v", poll)
					}
					continue
				}
				if poll.Status != "SUCCEEDED" || poll.Classifier != string(acb.HistoryPage) || poll.HTTPStatus != 200 || poll.RowsSeen != 2 || poll.Generation != conn.Generation {
					t.Fatalf("recovered poll=%+v", poll)
				}
			}
			final, err := store.Connection(ctx)
			if err != nil || final.State != "MONITORING" || final.Generation != conn.Generation {
				t.Fatalf("connection=%+v err=%v", final, err)
			}
			txns, err := store.ListTransactions(ctx, 10)
			if err != nil || len(txns) != 2 {
				t.Fatalf("transactions=%+v err=%v", txns, err)
			}
			identities := map[string]bool{}
			for _, txn := range txns {
				identities[txn.SemanticKey] = true
				if txn.TransactionDay != "2026-10-05" || txn.EffectiveAt != protocolRecoveryDate || txn.Balance == nil {
					t.Fatalf("transaction dates/balance=%+v", txn)
				}
				if txn.SemanticKey == "ACB:TX101" {
					if txn.Credit != 100000 || txn.Debit != 0 || *txn.Balance != 1000000 || txn.Description != "Synthetic incoming" {
						t.Fatalf("incoming=%+v", txn)
					}
				} else if txn.SemanticKey == "ACB:TX102" {
					if txn.Debit != 50000 || txn.Credit != 0 || *txn.Balance != 950000 || txn.Description != "Synthetic outgoing" {
						t.Fatalf("outgoing=%+v", txn)
					}
				} else {
					t.Fatalf("unexpected identity/data=%+v", txn)
				}
			}
			if len(identities) != 2 || !identities["ACB:TX101"] || !identities["ACB:TX102"] || protocolRecoveryCount(t, store, "events") != 1 || protocolRecoveryCount(t, store, "transaction_quarantine") != 0 {
				t.Fatal("recovery/repeated poll duplicated or altered transaction events")
			}
			var eventIdentity, eventType string
			if err := store.DB().QueryRow("SELECT t.semantic_key,e.event_type FROM events e JOIN transactions t ON t.id=e.transaction_id").Scan(&eventIdentity, &eventType); err != nil {
				t.Fatal(err)
			}
			if eventIdentity != "ACB:TX101" || eventType != "bank.transaction.credit" {
				t.Fatalf("unexpected credit event identity=%s type=%s", eventIdentity, eventType)
			}
		})
	}
}

func TestRealtimeTask_HistoryUnavailablePreservesPartialAndSnapshot(t *testing.T) {
	for _, boundary := range []string{"page-one", "in-quantum", "yielded-continuation"} {
		t.Run(boundary, func(t *testing.T) {
			bank := &protocolRecoveryBank{repeatDetail: true}
			expectedRows := 0
			if boundary != "page-one" {
				bank.repeatDetail = false
				bank.summaryProbe = true
				bank.pages = 2
				bank.invalidateAfter = 1
				expectedRows = 1
				if boundary == "yielded-continuation" {
					bank.pages = 6
					bank.invalidateAfter = 5
					expectedRows = 5
				}
			}
			store, conn, _, _, _, mon := protocolRecoveryFixture(t, bank)
			if _, err := store.DB().Exec(`INSERT INTO checkpoints(connection_id,scan_id,coverage_from,coverage_to,updated_at) VALUES(?,'prior-scan','2026-10-01','2026-10-04','2026-10-04T00:00:00Z')`, conn.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := store.DB().Exec(`INSERT INTO history_coverage(id,connection_id,day,status,last_sync_at,rows_seen) VALUES('prior-coverage',?,'2026-10-04','COMPLETE','2026-10-04T00:00:00Z',7)`, conn.ID); err != nil {
				t.Fatal(err)
			}
			before, err := store.Session(context.Background(), conn.ID, conn.Generation)
			if err != nil {
				t.Fatal(err)
			}
			task := protocolRecoveryStep(t, mon, conn, false)
			if boundary == "yielded-continuation" {
				if !task.continuation || task.finished {
					t.Fatal("fixture did not reach resumed continuation boundary")
				}
				if _, err := task.Step(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			polls, err := store.ListPollRuns(context.Background(), 10)
			if err != nil || len(polls) != 1 || polls[0].Status != "PARTIAL" || polls[0].Error != "HISTORY_UNAVAILABLE" || polls[0].Classifier != string(acb.AccountDetailPage) || polls[0].HTTPStatus != 200 || polls[0].RowsSeen != expectedRows {
				t.Fatalf("protocol failure polls=%+v err=%v", polls, err)
			}
			if mon.IsBackoffActive() {
				t.Fatal("protocol failure recorded network backoff")
			}
			if protocolRecoveryCount(t, store, "transactions") != expectedRows || protocolRecoveryCount(t, store, "events") != expectedRows || protocolRecoveryCount(t, store, "history_coverage") != 1 || protocolRecoveryCount(t, store, "checkpoints") != 1 {
				t.Fatal("failed protocol result ingested data or committed incomplete coverage")
			}
			checkpoint, err := store.GetCheckpoint(context.Background(), conn.ID)
			if err != nil || checkpoint == nil || checkpoint.ScanID != "prior-scan" || checkpoint.CoverageFrom != "2026-10-01" || checkpoint.CoverageTo != "2026-10-04" || checkpoint.UpdatedAt != "2026-10-04T00:00:00Z" {
				t.Fatalf("partial poll changed prior checkpoint: %+v %v", checkpoint, err)
			}
			var coverageDay, coverageStatus, coverageUpdated string
			var coverageRows int
			if err := store.DB().QueryRow("SELECT day,status,last_sync_at,rows_seen FROM history_coverage WHERE connection_id=?", conn.ID).Scan(&coverageDay, &coverageStatus, &coverageUpdated, &coverageRows); err != nil {
				t.Fatal(err)
			}
			if coverageDay != "2026-10-04" || coverageStatus != "COMPLETE" || coverageUpdated != "2026-10-04T00:00:00Z" || coverageRows != 7 {
				t.Fatal("partial poll changed prior coverage")
			}
			after, err := store.Session(context.Background(), conn.ID, conn.Generation)
			if err != nil || !bytes.Equal(before.Envelope, after.Envelope) {
				t.Fatalf("partial form persisted: %v", err)
			}
			final, err := store.Connection(context.Background())
			if err != nil || final.Generation != conn.Generation || final.State != "MONITORING" {
				t.Fatalf("protocol failure changed auth: %+v %v", final, err)
			}
			expected := "detail,stale,get,detail"
			if boundary == "in-quantum" {
				expected = "post,post,stale,get"
			}
			if boundary == "yielded-continuation" {
				expected = "post,post,post,post,post,post,stale,get"
			}
			protocolRecoveryEvents(t, bank, expected)
		})
	}
}

func TestSessionFence_DiscardsDetailRecoveryDuringProbeAndReplay(t *testing.T) {
	for _, tc := range []struct {
		phase     string
		bootstrap bool
	}{{"get", false}, {"post", false}, {"get", true}, {"post", true}} {
		phase := tc.phase
		for _, logout := range []bool{false, true} {
			t.Run(fmt.Sprintf("phase=%s/logout=%t/bootstrap=%t", phase, logout, tc.bootstrap), func(t *testing.T) {
				bank := &protocolRecoveryBank{incompleteDetail: tc.bootstrap, entered: make(chan struct{}), release: make(chan struct{})}
				store, conn, _, client, loader, mon := protocolRecoveryFixture(t, bank)
				bank.state++ // The restored snapshot is now stale, forcing real Client recovery.
				bank.blockPhase = phase
				finished := make(chan struct {
					response acb.Response
					err      error
				}, 1)
				go func() {
					response, err := mon.sessionRequest(context.Background(), conn.ID, conn.Generation, func() (acb.Response, error) {
						if tc.bootstrap {
							return client.BootstrapForDate(context.Background(), protocolRecoveryDate)
						}
						return client.HistoryForDate(context.Background(), "/acbib/Request", map[string]string{"dse_operationName": "ibkacctDetailProc", "dse_processorState": "token-0", "dse_sessionId": "synthetic", "AccountNbr": "12341234"}, protocolRecoveryDate)
					})
					finished <- struct {
						response acb.Response
						err      error
					}{response, err}
				}()
				select {
				case <-bank.entered:
				case <-time.After(5 * time.Second):
					close(bank.release)
					t.Fatal("recovery did not enter blocked phase")
				}
				generation := conn.Generation + 1
				if logout {
					generation = installLogoutFence(t, store, conn)
				} else if _, err := store.DB().Exec("UPDATE connections SET generation=generation+1 WHERE id=?", conn.ID); err != nil {
					close(bank.release)
					t.Fatal(err)
				}
				close(bank.release)
				select {
				case result := <-finished:
					if !errors.Is(result.err, storage.ErrGenerationFenceMismatch) || result.response.Body != "" || result.response.StatusCode != 0 {
						t.Fatalf("late recovery result accepted: %v", result.err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("recovery request did not finish")
				}
				if protocolRecoveryCount(t, store, "transactions") != 0 || protocolRecoveryCount(t, store, "events") != 0 || protocolRecoveryCount(t, store, "history_coverage") != 0 || protocolRecoveryCount(t, store, "checkpoints") != 0 {
					t.Fatal("fenced result ingested late data")
				}
				if err := loader.Persist(context.Background(), conn.ID, conn.Generation); !errors.Is(err, storage.ErrGenerationFenceMismatch) {
					t.Fatalf("fenced recovery snapshot persisted: %v", err)
				}
				if logout {
					if err := loader.InvalidateSession(context.Background(), conn.ID, generation); err != nil {
						t.Fatal(err)
					}
					if _, err := client.SnapshotSession(); !errors.Is(err, acb.ErrAuthenticatedFormStateUnavailable) {
						t.Fatalf("logout resurrected snapshot: %v", err)
					}
				}
				if _, err := mon.sessionRequest(context.Background(), conn.ID, conn.Generation, func() (acb.Response, error) { return client.Get(context.Background(), "/acbib/Request") }); !errors.Is(err, storage.ErrGenerationFenceMismatch) {
					t.Fatalf("stale next operation accepted: %v", err)
				}
				protocolRecoveryEvents(t, bank, "stale,get,post")
			})
		}
	}
}

func TestBootstrapUnavailableRealClientDoesNotPersistOrVerify(t *testing.T) {
	for _, consumer := range []string{"realtime", "keepalive", "verifier"} {
		t.Run(consumer, func(t *testing.T) {
			ctx := context.Background()
			bank := &protocolRecoveryBank{repeatDetail: true, incompleteDetail: true}
			if consumer != "realtime" {
				bank.requestDate = time.Now().In(acb.DefaultLocation).Format("02/01/2006")
			}
			store, conn, _, client, loader, mon := protocolRecoveryFixture(t, bank)
			before, err := store.Session(ctx, conn.ID, conn.Generation)
			if err != nil {
				t.Fatal(err)
			}
			var beforeVerified, beforeUpdated string
			if err := store.DB().QueryRowContext(ctx, "SELECT verified_at,updated_at FROM sessions WHERE connection_id=? AND generation=?", conn.ID, conn.Generation).Scan(&beforeVerified, &beforeUpdated); err != nil {
				t.Fatal(err)
			}
			bank.state++ // The restored bootstrap token was consumed upstream.
			if consumer == "verifier" {
				verified, err := NewSessionVerifier(loader, client).VerifySession(ctx, conn.ID, conn.Generation, before.Envelope)
				if authsession.VerificationCode(err) != "VERIFICATION_UNAVAILABLE" || len(verified) != 0 {
					t.Fatalf("unproved bootstrap verified: code=%s envelope=%t", authsession.VerificationCode(err), len(verified) != 0)
				}
			} else {
				if consumer == "keepalive" {
					result, err := NewKeepaliveTask(mon, conn.ID, conn.Generation).Step(ctx)
					if !errors.Is(err, acb.ErrHistoryUnavailable) || !result.Done || !result.RequeueAt.IsZero() || result.Outcome != scheduler.OutcomeFatal {
						t.Fatalf("keepalive result=%+v err=%v", result, err)
					}
				} else {
					protocolRecoveryStep(t, mon, conn, false)
				}
				polls, err := store.ListPollRuns(ctx, 1)
				if err != nil || len(polls) != 1 || polls[0].Status != "PARTIAL" || polls[0].Error != "HISTORY_UNAVAILABLE" || polls[0].Classifier != string(acb.AccountDetailPage) || polls[0].HTTPStatus != http.StatusOK || polls[0].RowsSeen != 0 {
					t.Fatalf("bootstrap failure poll=%+v err=%v", polls, err)
				}
				if consumer == "keepalive" && polls[0].Pages != 0 {
					t.Fatalf("keepalive counted history pages: %+v", polls[0])
				}
			}
			after, err := store.Session(ctx, conn.ID, conn.Generation)
			if err != nil || !bytes.Equal(before.Envelope, after.Envelope) {
				t.Fatalf("unavailable bootstrap refreshed durable session: %v", err)
			}
			var afterVerified, afterUpdated string
			if err := store.DB().QueryRowContext(ctx, "SELECT verified_at,updated_at FROM sessions WHERE connection_id=? AND generation=?", conn.ID, conn.Generation).Scan(&afterVerified, &afterUpdated); err != nil || beforeVerified != afterVerified || beforeUpdated != afterUpdated {
				t.Fatalf("unavailable bootstrap changed durable session verification timestamps: %v", err)
			}
			current, err := store.Connection(ctx)
			if err != nil || current.State != "MONITORING" || current.Generation != conn.Generation || mon.IsBackoffActive() {
				t.Fatalf("protocol failure changed auth/backoff: connection=%+v err=%v", current, err)
			}
			for _, table := range []string{"transactions", "events", "transaction_quarantine", "history_coverage", "checkpoints"} {
				if protocolRecoveryCount(t, store, table) != 0 {
					t.Fatalf("unavailable bootstrap changed %s", table)
				}
			}
			protocolRecoveryEvents(t, bank, "stale,get,detail")
		})
	}
}
