package acb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/authbrowser"
)

const recoveryAccount = "12345678"
const recoveryOtherAccount = "87654321"
const recoveryDate = "05/10/2026"

// This is a synthetic conversational bank, not captured ACB markup. Only a POST
// carrying the current server-issued tokens, exact selection, pinned date range,
// and history event can produce history. A timed-out POST consumes those tokens
// before returning its error; stale POSTs return authenticated detail without a
// history-capable state. Counting requests alone cannot make recovery succeed.
type historyRecoveryBank struct {
	t             *testing.T
	token         int
	from          string
	to            string
	requests      []recoveryRequest
	timeoutNext   bool
	probes        int
	replayPending bool
	paginate      bool
	incomplete    bool
	probe         func(*http.Request) (*http.Response, error)
	replay        func(*http.Request) (*http.Response, error)
	initial       func(*http.Request) (*http.Response, error)
	redirect      func(*http.Request) (*http.Response, error)
	onProbe       func()
}

type recoveryRequest struct {
	method string
	path   string
	fields url.Values
}

func newHistoryRecoveryBank(t *testing.T) *historyRecoveryBank {
	t.Helper()
	return &historyRecoveryBank{t: t, token: 1, from: recoveryDate, to: recoveryDate}
}

func recoveryResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

func (b *historyRecoveryBank) form(account string) string {
	selection := ""
	if account != "" {
		selection = `<input name="AccountNbr" value="` + account + `">`
	}
	return fmt.Sprintf(`<form action="/acbib/Request"><input name="dse_operationName" value="ibkacctDetailProc"><input name="dse_processorState" value="state-%d"><input name="dse_sessionId" value="session-%d"><input name="dse_nextEventName" value="byMonth"><input name="activeDatetimeYN" value="Y"><input name="MonthCurr" value="10"><input name="YearCurr" value="2026">%s</form>`, b.token, b.token, selection)
}

func (b *historyRecoveryBank) staleDetail() string {
	if b.incomplete {
		return `<form action="/acbib/Request"><input name="dse_operationName" value="ibkacctDetailProc"><input name="dse_processorState" value=""><input name="dse_sessionId" value="detail-session"><input name="AccountNbr" value="` + recoveryOtherAccount + `"></form><div>Account detail</div>`
	}
	return `<form action="/acbib/Request"><input name="dse_operationName" value="ibkacctDetailProc"><input name="dse_processorState" value="detail-only-state"><input name="dse_sessionId" value="detail-only-session"><input name="AccountNbr" value="` + recoveryOtherAccount + `"></form><div>Account detail</div>`
}

func (b *historyRecoveryBank) history(account string, next bool) string {
	body := b.form(account) + `<table><tr><th>Ngày hiệu lực</th><th>Ngày giao dịch</th><th>Số GD</th><th>Ghi nợ</th><th>Ghi có</th><th>Nội dung giao dịch</th></tr>`
	if next {
		body += `<tr><td>05/10/2026</td><td>05/10/2026</td><td>TX101</td><td>0</td><td>100.000</td><td>Synthetic credit</td></tr><tr><td colspan="6"><a href="/acbib/Request" onclick="submitEvent('nextPage')">Trang sau</a></td></tr>`
	} else {
		if !b.paginate {
			body += `<tr><td>05/10/2026</td><td>05/10/2026</td><td>TX101</td><td>0</td><td>100.000</td><td>Synthetic credit</td></tr>`
		}
		body += `<tr><td>06/10/2026</td><td>05/10/2026</td><td>TX102</td><td>50.000</td><td>0</td><td>Synthetic debit</td></tr><tr><td colspan="6"><span class="disabled">Trang sau</span></td></tr>`
	}
	return body + `</table><div>Tổng số dòng: 2</div>`
}

func (b *historyRecoveryBank) RoundTrip(r *http.Request) (*http.Response, error) {
	b.t.Helper()
	if r.URL.Scheme != "https" || r.URL.Hostname() != OfficialHost {
		b.t.Fatal("fixture request left the official synthetic origin")
	}
	if !strings.Contains(r.Header.Get("Cookie"), "JSESSIONID=synthetic-session") {
		b.t.Fatal("conversational recovery discarded the authenticated cookie")
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
	b.requests = append(b.requests, recoveryRequest{method: r.Method, path: r.URL.Path, fields: fields})
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	if r.URL.Path == "/acbib/redirected" && b.redirect != nil {
		return b.redirect(r)
	}
	if r.Method == http.MethodGet {
		if r.URL.Path != "/acbib/Request" {
			b.t.Fatalf("unexpected probe path: %s", r.URL.Path)
		}
		b.probes++
		if b.probes > 1 {
			b.t.Fatal("history recovery exceeded one fresh-form probe")
		}
		b.token++
		b.replayPending = true
		if b.onProbe != nil {
			b.onProbe()
		}
		if b.probe != nil {
			return b.probe(r)
		}
		return recoveryResponse(r, http.StatusOK, b.form(recoveryOtherAccount)), nil
	}
	if r.Method != http.MethodPost {
		b.t.Fatalf("unexpected method: %s", r.Method)
	}
	if b.initial != nil && !b.replayPending {
		return b.initial(r)
	}
	if fields.Get("dse_processorState") != fmt.Sprintf("state-%d", b.token) || fields.Get("dse_sessionId") != fmt.Sprintf("session-%d", b.token) {
		return recoveryResponse(r, http.StatusOK, b.staleDetail()), nil
	}
	if fields.Get("dse_operationName") != "ibkacctDetailProc" || fields.Get("AccountNbr") != recoveryAccount || fields.Get("FromDate") != b.from || fields.Get("ToDate") != b.to || fields.Get("activeDatetimeYN") != "N" {
		b.t.Fatal("history POST lost operation, exact target, original range, or date mode")
	}
	if fields.Has("_raw") || fields.Has("_explicitRange") || fields.Has("MonthCurr") || fields.Has("YearCurr") || fields.Has("activeDatetimeByMonth") {
		b.t.Fatal("history POST leaked internal or month-only controls")
	}
	event := fields.Get("dse_nextEventName")
	if event != "byDate" && !(b.paginate && event == "nextPage") {
		b.t.Fatalf("history POST used wrong navigation event: %s", event)
	}
	b.token++
	if b.timeoutNext {
		b.timeoutNext = false
		return nil, context.DeadlineExceeded
	}
	if b.replayPending {
		b.replayPending = false
		if event != "byDate" {
			b.t.Fatal("recovery replayed a continuation cursor")
		}
		if b.replay != nil {
			return b.replay(r)
		}
	}
	return recoveryResponse(r, http.StatusOK, b.history(recoveryAccount, b.paginate && event == "byDate")), nil
}

func (b *historyRecoveryBank) handoff() authbrowser.Handoff {
	handoff := accountContinuityHandoff(recoveryAccount)
	handoff.Fields["dse_processorState"] = fmt.Sprintf("state-%d", b.token)
	handoff.Fields["dse_sessionId"] = fmt.Sprintf("session-%d", b.token)
	return handoff
}

func (b *historyRecoveryBank) client(handoff authbrowser.Handoff) *Client {
	b.t.Helper()
	client, err := NewClient("https://"+OfficialHost, b)
	if err != nil {
		b.t.Fatal(err)
	}
	client.location = time.FixedZone("Asia/Ho_Chi_Minh", 7*60*60)
	client.now = func() time.Time { return time.Date(2026, 10, 5, 12, 0, 0, 0, client.location) }
	if err := client.RestoreSession(handoff); err != nil {
		b.t.Fatal(err)
	}
	return client
}

func assertRecoveryRequests(t *testing.T, requests []recoveryRequest, methods ...string) {
	t.Helper()
	if len(requests) != len(methods) {
		t.Fatalf("request count=%d want=%d", len(requests), len(methods))
	}
	for i, method := range methods {
		if requests[i].method != method {
			t.Fatalf("request %d method=%s want=%s", i, requests[i].method, method)
		}
	}
}

func assertRecoveryTransactions(t *testing.T, response Response) HistoryPageResult {
	t.Helper()
	if response.StatusCode != http.StatusOK || response.Kind != HistoryPage || response.RequestedAccount != recoveryAccount {
		t.Fatalf("unproved history result: status=%d kind=%s", response.StatusCode, response.Kind)
	}
	page, err := ParseHistoryPage(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	want := []Transaction{
		{Number: "TX101", EffectiveDate: recoveryDate, TransactionAt: recoveryDate, Debit: 0, Credit: 100000, Description: "Synthetic credit"},
		{Number: "TX102", EffectiveDate: "06/10/2026", TransactionAt: recoveryDate, Debit: 50000, Credit: 0, Description: "Synthetic debit"},
	}
	if len(page.Transactions) != len(want) || page.HasNext || page.TotalRows != 2 {
		t.Fatal("history result lost transactions or final-page contract")
	}
	for i := range want {
		if page.Transactions[i] != want[i] {
			t.Fatalf("transaction %d differs from the synthetic date/amount/identity contract", i)
		}
	}
	filtered, err := FilterHistoryTransactionDay(page.Transactions, recoveryDate)
	if err != nil || len(filtered) != 2 {
		t.Fatalf("mixed effective dates must retain both original transaction-day rows: %v", err)
	}
	return page
}

func assertRecoveryTarget(t *testing.T, client *Client) {
	t.Helper()
	if client.SessionAccountNumber() != recoveryAccount {
		t.Fatal("recovery replaced the live outbound target with a response account")
	}
}

func TestHistoryDetailRecoveryAfterConsumedTimeoutAndStaleSnapshot(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart_%t", restart), func(t *testing.T) {
			bank := newHistoryRecoveryBank(t)
			client := bank.client(bank.handoff())
			first, err := client.BootstrapForDate(context.Background(), recoveryDate)
			if err != nil {
				t.Fatal(err)
			}
			assertRecoveryTransactions(t, first)
			snapshot, err := client.SnapshotSession()
			if err != nil {
				t.Fatal(err)
			}
			bank.timeoutNext = true
			_, err = client.HistoryForDate(context.Background(), snapshot.Action, snapshot.Fields, recoveryDate)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("consumed POST did not return deadline failure: %v", err)
			}
			assertRecoveryRequests(t, bank.requests, http.MethodPost, http.MethodPost)
			if restart {
				client = bank.client(snapshot)
			}
			response, err := client.HistoryForDate(context.Background(), snapshot.Action, snapshot.Fields, recoveryDate)
			if err != nil {
				t.Fatal(err)
			}
			assertRecoveryTransactions(t, response)
			assertRecoveryRequests(t, bank.requests, http.MethodPost, http.MethodPost, http.MethodPost, http.MethodGet, http.MethodPost)
			assertRecoveryTarget(t, client)
			fresh, err := client.SnapshotSession()
			if err != nil {
				t.Fatal(err)
			}
			if fresh.Fields["dse_processorState"] != fmt.Sprintf("state-%d", bank.token) || fresh.Fields["dse_sessionId"] != fmt.Sprintf("session-%d", bank.token) || fresh.Fields["dse_processorState"] == snapshot.Fields["dse_processorState"] {
				t.Fatal("successful snapshot did not capture the newly established conversation")
			}
			if snapshot.Fields["dse_processorState"] != "state-2" || snapshot.Fields["AccountNbr"] != recoveryAccount {
				t.Fatal("recovery mutated the persisted pre-timeout snapshot")
			}
			next, err := client.HistoryForDate(context.Background(), fresh.Action, fresh.Fields, recoveryDate)
			if err != nil {
				t.Fatal(err)
			}
			assertRecoveryTransactions(t, next)
			if bank.probes != 1 {
				t.Fatal("fresh next poll unnecessarily resynchronized")
			}
		})
	}
}

func TestHistoryDetailContinuationResetsWithoutReplayingCursor(t *testing.T) {
	bank := newHistoryRecoveryBank(t)
	bank.paginate = true
	client := bank.client(bank.handoff())
	response, err := client.BootstrapForDate(context.Background(), recoveryDate)
	if err != nil {
		t.Fatal(err)
	}
	page, err := ParseHistoryPage(response.Body)
	if err != nil || !page.HasNext || len(page.Transactions) != 1 || page.Transactions[0].Number != "TX101" {
		t.Fatalf("invalid synthetic first page: %v", err)
	}
	fields := cloneFields(page.NextFields)
	fields["_raw"] = "true"
	bank.token++ // The server expires this cursor while the client still has it.
	response, err = client.HistoryForDate(context.Background(), page.NextAction, fields, recoveryDate)
	if !errors.Is(err, ErrConversationReset) || response.Kind != AccountDetailPage {
		t.Fatalf("stale continuation must return the fresh form and reset: %v kind=%s", err, response.Kind)
	}
	assertRecoveryRequests(t, bank.requests, http.MethodPost, http.MethodPost, http.MethodGet)
	if bank.requests[1].fields.Get("dse_nextEventName") != "nextPage" {
		t.Fatal("the original continuation was rewritten to page one")
	}
	assertRecoveryTarget(t, client)
	fresh, err := client.SnapshotSession()
	if err != nil {
		t.Fatal(err)
	}
	bank.paginate = false
	response, err = client.HistoryForDate(context.Background(), fresh.Action, fresh.Fields, recoveryDate)
	if err != nil {
		t.Fatal(err)
	}
	assertRecoveryTransactions(t, response)
	if bank.requests[3].fields.Get("dse_nextEventName") != "byDate" || bank.probes != 1 {
		t.Fatal("the next day scan failed to restart at page one from the fresh conversation")
	}
}

func TestHistoryDetailRecoveryBoundedReplayFailure(t *testing.T) {
	for _, kind := range []PageKind{AccountDetailPage, UnknownPage} {
		t.Run(string(kind), func(t *testing.T) {
			bank := newHistoryRecoveryBank(t)
			handoff := bank.handoff()
			client := bank.client(handoff)
			bank.token++
			lastBody := ""
			bank.replay = func(r *http.Request) (*http.Response, error) {
				lastBody = bank.staleDetail()
				if kind == UnknownPage {
					lastBody = `<div>Unrecognized result</div>`
				}
				return recoveryResponse(r, http.StatusOK, lastBody), nil
			}
			response, err := client.HistoryForDate(context.Background(), handoff.Action, handoff.Fields, recoveryDate)
			if err == nil {
				t.Fatal("authenticated wrong page was accepted as history")
			}
			if !errors.Is(err, ErrHistoryUnavailable) || response.Kind != kind || response.StatusCode != http.StatusOK || response.Body != lastBody {
				t.Fatalf("repeated wrong page did not retain the final response and protocol sentinel: %v kind=%s", err, response.Kind)
			}
			assertRecoveryRequests(t, bank.requests, http.MethodPost, http.MethodGet, http.MethodPost)
			if _, parseErr := ParseHistoryPage(response.Body); parseErr == nil {
				t.Fatal("wrong-page fixture unexpectedly represents parseable empty history")
			}
			assertRecoveryTarget(t, client)
		})
	}
}

func TestHistoryDetailRecoveryRejectsIncompatibleFreshForm(t *testing.T) {
	for _, invalid := range []string{"missing_state", "missing_session", "summary_only", "foreign_action", "wrong_operation"} {
		for _, continuation := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/continuation_%t", invalid, continuation), func(t *testing.T) {
				bank := newHistoryRecoveryBank(t)
				handoff := bank.handoff()
				client := bank.client(handoff)
				bank.token++
				bank.probe = func(r *http.Request) (*http.Response, error) {
					body := bank.form(recoveryOtherAccount)
					switch invalid {
					case "missing_state":
						body = strings.ReplaceAll(body, fmt.Sprintf(`value="state-%d"`, bank.token), `value=""`)
					case "missing_session":
						body = strings.ReplaceAll(body, fmt.Sprintf(`value="session-%d"`, bank.token), `value=""`)
					case "summary_only":
						body = strings.ReplaceAll(body, "ibkacctDetailProc", "ibkacctSumProc")
					case "foreign_action":
						body = strings.ReplaceAll(body, `action="/acbib/Request"`, `action="https://example.test/acbib/Request"`)
					case "wrong_operation":
						body = strings.ReplaceAll(body, "ibkacctDetailProc", "unrelatedProc") + `<div>ibkacctDetailProc AccountNbr</div>`
					}
					return recoveryResponse(r, http.StatusOK, body), nil
				}
				fields := cloneFields(handoff.Fields)
				if continuation {
					fields["_raw"] = "true"
					fields["dse_nextEventName"] = "nextPage"
				}
				response, err := client.HistoryForDate(context.Background(), handoff.Action, fields, recoveryDate)
				var authFailure *AuthFailure
				if !errors.Is(err, ErrHistoryUnavailable) || errors.As(err, &authFailure) || errors.Is(err, ErrConversationReset) || response.Kind != AccountDetailPage {
					t.Fatalf("incompatible fresh form must fail closed, not confirm auth loss/reset: %v kind=%s", err, response.Kind)
				}
				assertRecoveryRequests(t, bank.requests, http.MethodPost, http.MethodGet)
				assertRecoveryTarget(t, client)
			})
		}
	}
}

func TestHistoryDetailRecoveryProbeFailuresDoNotConfirmAuthenticationLoss(t *testing.T) {
	for _, failure := range []string{"network", "rate_limit", "server_error", "maintenance", "unknown"} {
		t.Run(failure, func(t *testing.T) {
			bank := newHistoryRecoveryBank(t)
			handoff := bank.handoff()
			client := bank.client(handoff)
			bank.token++
			transportErr := errors.New("synthetic probe network failure")
			status := http.StatusOK
			body := `<div>Unrecognized probe</div>`
			switch failure {
			case "rate_limit":
				status = http.StatusTooManyRequests
				body = bank.form(recoveryOtherAccount)
			case "server_error":
				status = http.StatusServiceUnavailable
				body = bank.form(recoveryOtherAccount)
			case "maintenance":
				body = `<div>Hệ thống đang bảo trì</div>`
			}
			bank.probe = func(r *http.Request) (*http.Response, error) {
				if failure == "network" {
					return nil, transportErr
				}
				return recoveryResponse(r, status, body), nil
			}
			response, err := client.HistoryForDate(context.Background(), handoff.Action, handoff.Fields, recoveryDate)
			var authFailure *AuthFailure
			if err == nil || errors.As(err, &authFailure) || errors.Is(err, ErrHistoryUnavailable) {
				t.Fatalf("probe transient must remain inconclusive/transport, not auth/protocol failure: %v", err)
			}
			if failure == "network" {
				if !errors.Is(err, transportErr) {
					t.Fatalf("probe transport cause was lost: %v", err)
				}
			} else if !errors.Is(err, ErrInconclusiveAuth) || response.StatusCode != status || response.Body != body {
				t.Fatalf("inconclusive probe lost its final response/status: %v status=%d", err, response.StatusCode)
			}
			assertRecoveryRequests(t, bank.requests, http.MethodPost, http.MethodGet)
			assertRecoveryTarget(t, client)
		})
	}
}

func TestHistoryDetailRecoveryOnlyProbeConfirmsAuthenticationLoss(t *testing.T) {
	for _, challenge := range []struct {
		name   string
		status int
		body   string
		kind   PageKind
	}{
		{name: "login", status: http.StatusOK, body: `<input name="username"><input type="password" name="password">`, kind: LoginPage},
		{name: "otp", status: http.StatusOK, body: `Nhập mã OTP SafeKey`, kind: OTPChallenge},
		{name: "captcha", status: http.StatusOK, body: `captcha Mã xác nhận`, kind: CaptchaPage},
		{name: "unauthorized", status: http.StatusUnauthorized, body: `<div>Unauthorized</div>`, kind: LoginPage},
		{name: "forbidden", status: http.StatusForbidden, body: `<div>Forbidden</div>`, kind: LoginPage},
	} {
		for _, phase := range []string{"probe", "replay"} {
			t.Run(challenge.name+"/"+phase, func(t *testing.T) {
				bank := newHistoryRecoveryBank(t)
				handoff := bank.handoff()
				client := bank.client(handoff)
				bank.token++
				respond := func(r *http.Request) (*http.Response, error) {
					return recoveryResponse(r, challenge.status, challenge.body), nil
				}
				if phase == "probe" {
					bank.probe = respond
				} else {
					bank.replay = respond
				}
				response, err := client.HistoryForDate(context.Background(), handoff.Action, handoff.Fields, recoveryDate)
				var authFailure *AuthFailure
				if phase == "probe" {
					if !errors.As(err, &authFailure) || authFailure.Kind != challenge.kind {
						t.Fatalf("fresh probe did not confirm auth loss: %v", err)
					}
					assertRecoveryRequests(t, bank.requests, http.MethodPost, http.MethodGet)
				} else {
					if !errors.Is(err, ErrInconclusiveAuth) || errors.As(err, &authFailure) {
						t.Fatalf("replay cannot confirm auth loss without another fresh probe: %v", err)
					}
					assertRecoveryRequests(t, bank.requests, http.MethodPost, http.MethodGet, http.MethodPost)
				}
				if response.StatusCode != challenge.status || response.Kind != challenge.kind || response.Body != challenge.body {
					t.Fatal("auth challenge response was not preserved")
				}
				assertRecoveryTarget(t, client)
			})
		}
	}
}

func TestHistoryDetailRecoveryRequiresExactAccountProof(t *testing.T) {
	for _, proof := range []string{"exact_echo", "direct_no_echo", "different_echo", "masked_echo", "redirect_get", "redirect_post"} {
		t.Run(proof, func(t *testing.T) {
			bank := newHistoryRecoveryBank(t)
			handoff := bank.handoff()
			client := bank.client(handoff)
			bank.token++
			account := recoveryAccount
			switch proof {
			case "direct_no_echo", "redirect_get", "redirect_post":
				account = ""
			case "different_echo":
				account = recoveryOtherAccount
			case "masked_echo":
				account = "****5678"
			}
			lastBody := ""
			bank.replay = func(r *http.Request) (*http.Response, error) {
				lastBody = bank.history(account, false)
				if strings.HasPrefix(proof, "redirect_") {
					status := http.StatusFound
					if proof == "redirect_post" {
						status = http.StatusTemporaryRedirect
					}
					response := recoveryResponse(r, status, "")
					response.Header.Set("Location", "/acbib/redirected")
					return response, nil
				}
				return recoveryResponse(r, http.StatusOK, lastBody), nil
			}
			bank.redirect = func(r *http.Request) (*http.Response, error) {
				return recoveryResponse(r, http.StatusOK, lastBody), nil
			}
			response, err := client.HistoryForDate(context.Background(), handoff.Action, handoff.Fields, recoveryDate)
			accepted := proof == "exact_echo" || proof == "direct_no_echo"
			if accepted {
				if err != nil {
					t.Fatal(err)
				}
				assertRecoveryTransactions(t, response)
			} else if !errors.Is(err, ErrHistoryUnavailable) {
				t.Fatalf("unproved replay must be unavailable, not ingestible: %v", err)
			}
			if response.Kind != HistoryPage || response.Body != lastBody {
				t.Fatal("proof validation altered the bank-returned body/identity")
			}
			form, formErr := ExtractHistoryForm(response.Body)
			if formErr != nil || form.Fields["AccountNbr"] != account {
				t.Fatal("outbound target was falsely inserted into response evidence")
			}
			if strings.HasPrefix(proof, "redirect_") {
				method := http.MethodGet
				if proof == "redirect_post" {
					method = http.MethodPost
				}
				assertRecoveryRequests(t, bank.requests, http.MethodPost, http.MethodGet, http.MethodPost, method)
				if response.RequestedAccount != "" {
					t.Fatal("redirect/GET response was treated as direct POST account proof")
				}
			} else {
				assertRecoveryRequests(t, bank.requests, http.MethodPost, http.MethodGet, http.MethodPost)
				if response.RequestedAccount != recoveryAccount {
					t.Fatal("direct response lost actual outbound selection metadata")
				}
			}
			assertRecoveryTarget(t, client)
			fresh, snapshotErr := client.SnapshotSession()
			if snapshotErr != nil {
				t.Fatal(snapshotErr)
			}
			bank.replay = nil
			response, err = client.HistoryForDate(context.Background(), fresh.Action, fresh.Fields, recoveryDate)
			if err != nil {
				t.Fatal(err)
			}
			assertRecoveryTransactions(t, response)
			if bank.probes != 1 || bank.requests[len(bank.requests)-1].fields.Get("AccountNbr") != recoveryAccount {
				t.Fatal("the next call lost the exact account or retried recovery unnecessarily")
			}
		})
	}
}

func TestHistoryDetailRecoveryPinsOriginalDateRangeAcrossRollover(t *testing.T) {
	for _, mode := range []string{"implicit_today", "caller_day", "explicit_range"} {
		t.Run(mode, func(t *testing.T) {
			bank := newHistoryRecoveryBank(t)
			handoff := bank.handoff()
			client := bank.client(handoff)
			fields := cloneFields(handoff.Fields)
			delete(fields, "AccountNbr") // Selection is inherited before the first do.
			if mode == "explicit_range" {
				bank.from = "29/09/2026"
				fields["_explicitRange"] = "true"
				fields["FromDate"] = bank.from
				fields["ToDate"] = bank.to
			}
			client.now = func() time.Time { return time.Date(2026, 10, 5, 23, 59, 59, 0, client.location) }
			bank.token++
			bank.onProbe = func() {
				client.now = func() time.Time { return time.Date(2026, 10, 6, 0, 0, 1, 0, client.location) }
			}
			bank.probe = func(r *http.Request) (*http.Response, error) {
				// Browser-only bookkeeping is not an upstream pagination request.
				body := strings.Replace(bank.form(recoveryOtherAccount), "</form>", `<input name="_raw" value="true"><input name="_explicitRange" value="true"><input name="FromDate" value="06/10/2026"><input name="ToDate" value="06/10/2026"></form>`, 1)
				return recoveryResponse(r, http.StatusOK, body), nil
			}
			var response Response
			var err error
			if mode == "caller_day" {
				response, err = client.HistoryForDate(context.Background(), handoff.Action, fields, recoveryDate)
			} else {
				response, err = client.History(context.Background(), handoff.Action, fields)
			}
			if err != nil {
				t.Fatal(err)
			}
			assertRecoveryTransactions(t, response)
			assertRecoveryRequests(t, bank.requests, http.MethodPost, http.MethodGet, http.MethodPost)
			original, replay := bank.requests[0].fields, bank.requests[2].fields
			if original.Get("FromDate") != replay.Get("FromDate") || original.Get("ToDate") != replay.Get("ToDate") || replay.Get("AccountNbr") != recoveryAccount || replay.Get("dse_processorState") != "state-3" || replay.Get("dse_sessionId") != "session-3" {
				t.Fatal("replay did not combine original selection/range with only fresh server state")
			}
			if fields["AccountNbr"] != "" || fields["dse_processorState"] != "state-1" {
				t.Fatal("recovery mutated caller request fields")
			}
			assertRecoveryTarget(t, client)
		})
	}
}

func TestHistoryContinuationRetainsServerEventAcrossRollover(t *testing.T) {
	bank := newHistoryRecoveryBank(t)
	bank.paginate = true
	client := bank.client(bank.handoff())
	response, err := client.BootstrapForDate(context.Background(), recoveryDate)
	if err != nil {
		t.Fatal(err)
	}
	page, err := ParseHistoryPage(response.Body)
	if err != nil || !page.HasNext {
		t.Fatalf("invalid first page: %v", err)
	}
	fields := cloneFields(page.NextFields)
	fields["_raw"] = "true"
	client.now = func() time.Time { return time.Date(2026, 10, 6, 0, 0, 1, 0, client.location) }
	response, err = client.HistoryForDate(context.Background(), page.NextAction, fields, recoveryDate)
	if err != nil {
		t.Fatal(err)
	}
	last, parseErr := ParseHistoryPage(response.Body)
	want := Transaction{Number: "TX102", EffectiveDate: "06/10/2026", TransactionAt: recoveryDate, Debit: 50000, Credit: 0, Description: "Synthetic debit"}
	if parseErr != nil || last.HasNext || last.TotalRows != 2 || len(last.Transactions) != 1 || last.Transactions[0] != want || response.RequestedAccount != recoveryAccount {
		t.Fatalf("continuation did not complete the exact synthetic transaction set: %v", parseErr)
	}
	assertRecoveryRequests(t, bank.requests, http.MethodPost, http.MethodPost)
	if bank.requests[1].fields.Get("dse_nextEventName") != "nextPage" || bank.requests[1].fields.Get("FromDate") != recoveryDate || bank.requests[1].fields.Get("ToDate") != recoveryDate || bank.probes != 0 {
		t.Fatal("rollover changed the pinned continuation range or server navigation")
	}
}

func TestHistoryDetailRecoveryReplayTransientResponseKeepsConsumerPolicy(t *testing.T) {
	for _, failure := range []string{"network", "rate_limit", "server_error", "maintenance"} {
		t.Run(failure, func(t *testing.T) {
			bank := newHistoryRecoveryBank(t)
			handoff := bank.handoff()
			client := bank.client(handoff)
			bank.token++
			transportErr := errors.New("synthetic replay network failure")
			status := http.StatusOK
			body := `<div>Hệ thống đang bảo trì</div>`
			kind := MaintenancePage
			switch failure {
			case "rate_limit":
				status, body, kind = http.StatusTooManyRequests, bank.staleDetail(), AccountDetailPage
			case "server_error":
				status, body, kind = http.StatusServiceUnavailable, bank.staleDetail(), AccountDetailPage
			}
			bank.replay = func(r *http.Request) (*http.Response, error) {
				if failure == "network" {
					return nil, transportErr
				}
				return recoveryResponse(r, status, body), nil
			}
			response, err := client.HistoryForDate(context.Background(), handoff.Action, handoff.Fields, recoveryDate)
			if failure == "network" {
				if !errors.Is(err, transportErr) {
					t.Fatalf("replay transport cause was lost: %v", err)
				}
			} else if err != nil || response.StatusCode != status || response.Kind != kind || response.Body != body {
				t.Fatalf("replay transient was remapped instead of preserving consumer policy: %v status=%d kind=%s", err, response.StatusCode, response.Kind)
			}
			var authFailure *AuthFailure
			if errors.As(err, &authFailure) || errors.Is(err, ErrHistoryUnavailable) {
				t.Fatalf("transient replay incorrectly became an auth/protocol failure: %v", err)
			}
			assertRecoveryRequests(t, bank.requests, http.MethodPost, http.MethodGet, http.MethodPost)
			assertRecoveryTarget(t, client)
		})
	}
}

func TestHistoryDetailRecoveryLeavesHistorySchemaValidationToParser(t *testing.T) {
	bank := newHistoryRecoveryBank(t)
	handoff := bank.handoff()
	client := bank.client(handoff)
	bank.token++
	bank.replay = func(r *http.Request) (*http.Response, error) {
		return recoveryResponse(r, http.StatusOK, bank.form(recoveryAccount)+`<table><tr><th>Số GD</th><th>Ghi nợ</th><th>Ghi có</th></tr><tr><td>TX101</td><td>0</td><td>100.000</td></tr></table>`), nil
	}
	response, err := client.HistoryForDate(context.Background(), handoff.Action, handoff.Fields, recoveryDate)
	if err != nil || response.Kind != HistoryPage || response.RequestedAccount != recoveryAccount {
		t.Fatalf("client reclassified a proved HistoryPage instead of returning it to its parser: %v kind=%s", err, response.Kind)
	}
	if _, parseErr := ParseHistoryPage(response.Body); parseErr == nil {
		t.Fatal("missing-date schema must fail at the existing parser, not become empty history")
	}
	assertRecoveryRequests(t, bank.requests, http.MethodPost, http.MethodGet, http.MethodPost)
	assertRecoveryTarget(t, client)
}

func TestHistoryDoesNotResynchronizeNonDetailFailures(t *testing.T) {
	for _, failure := range []struct {
		name   string
		status int
		body   string
		kind   PageKind
	}{
		{name: "unknown", status: http.StatusOK, body: `<div>Unrecognized upstream page</div>`, kind: UnknownPage},
		{name: "rate_limit", status: http.StatusTooManyRequests, kind: AccountDetailPage},
		{name: "server_error", status: http.StatusServiceUnavailable, kind: AccountDetailPage},
		{name: "maintenance", status: http.StatusOK, body: `<div>Hệ thống đang bảo trì</div>`, kind: MaintenancePage},
		{name: "unrecognized_schema", status: http.StatusOK, body: `<div>ibkacctDetailProc AccountNbr dse_processorState Số GD Ghi nợ Ghi có</div>`, kind: HistoryPage},
	} {
		t.Run(failure.name, func(t *testing.T) {
			bank := newHistoryRecoveryBank(t)
			handoff := bank.handoff()
			client := bank.client(handoff)
			body := failure.body
			if body == "" {
				body = bank.staleDetail()
			}
			bank.initial = func(r *http.Request) (*http.Response, error) {
				return recoveryResponse(r, failure.status, body), nil
			}
			response, err := client.HistoryForDate(context.Background(), handoff.Action, handoff.Fields, recoveryDate)
			if err != nil || response.StatusCode != failure.status || response.Kind != failure.kind || response.Body != body {
				t.Fatalf("non-recovery response changed policy: %v status=%d kind=%s", err, response.StatusCode, response.Kind)
			}
			assertRecoveryRequests(t, bank.requests, http.MethodPost)
			if bank.probes != 0 {
				t.Fatal("arbitrary status/kind/schema error caused an extra bank request")
			}
		})
	}
}

func TestHistoryDetailRecoveryDoesNotProbeCanceledContext(t *testing.T) {
	for _, cancelAt := range []string{"before_post", "detail_response"} {
		t.Run(cancelAt, func(t *testing.T) {
			bank := newHistoryRecoveryBank(t)
			handoff := bank.handoff()
			client := bank.client(handoff)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if cancelAt == "before_post" {
				cancel()
			} else {
				bank.initial = func(r *http.Request) (*http.Response, error) {
					cancel()
					return recoveryResponse(r, http.StatusOK, bank.staleDetail()), nil
				}
			}
			_, err := client.HistoryForDate(ctx, handoff.Action, handoff.Fields, recoveryDate)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled request did not retain its cancellation cause: %v", err)
			}
			if cancelAt == "before_post" {
				// net/http may reject before RoundTrip or let the transport reject it.
				if len(bank.requests) > 1 {
					t.Fatal("pre-canceled request triggered recovery traffic")
				}
			} else {
				assertRecoveryRequests(t, bank.requests, http.MethodPost)
			}
			if bank.probes != 0 {
				t.Fatal("canceled wrong-page response triggered a bank probe")
			}
		})
	}
}

func TestHistoryDetailRecoveryDoesNotUseFreshGetHistoryAsAccountProof(t *testing.T) {
	bank := newHistoryRecoveryBank(t)
	handoff := bank.handoff()
	client := bank.client(handoff)
	bank.token++
	bank.probe = func(r *http.Request) (*http.Response, error) {
		return recoveryResponse(r, http.StatusOK, bank.history("", false)), nil
	}
	bank.replay = func(r *http.Request) (*http.Response, error) {
		return recoveryResponse(r, http.StatusOK, bank.history("", false)), nil
	}
	response, err := client.HistoryForDate(context.Background(), handoff.Action, handoff.Fields, recoveryDate)
	if err != nil {
		t.Fatal(err)
	}
	assertRecoveryTransactions(t, response)
	assertRecoveryRequests(t, bank.requests, http.MethodPost, http.MethodGet, http.MethodPost)
	form, err := ExtractHistoryForm(response.Body)
	if err != nil || form.Fields["AccountNbr"] != "" {
		t.Fatal("GET recovery evidence was fabricated into an echoed selection")
	}
	assertRecoveryTarget(t, client)
}

func TestBootstrapIncompleteRecoveryAfterConsumedTimeoutAndStaleSnapshot(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart_%t", restart), func(t *testing.T) {
			bank := newHistoryRecoveryBank(t)
			bank.incomplete = true
			client := bank.client(bank.handoff())
			first, err := client.BootstrapForDate(context.Background(), recoveryDate)
			if err != nil {
				t.Fatal(err)
			}
			assertRecoveryTransactions(t, first)
			snapshot, err := client.SnapshotSession()
			if err != nil {
				t.Fatal(err)
			}
			bank.timeoutNext = true
			_, err = client.BootstrapForDate(context.Background(), recoveryDate)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("consumed POST lost timeout: %v", err)
			}
			if restart {
				client = bank.client(snapshot)
			}
			response, err := client.BootstrapForDate(context.Background(), recoveryDate)
			if err != nil {
				t.Fatal(err)
			}
			assertRecoveryTransactions(t, response)
			assertRecoveryRequests(t, bank.requests, http.MethodPost, http.MethodPost, http.MethodPost, http.MethodGet, http.MethodPost)
			assertRecoveryTarget(t, client)
			fresh, err := client.SnapshotSession()
			if err != nil {
				t.Fatal(err)
			}
			if fresh.Fields["dse_processorState"] != fmt.Sprintf("state-%d", bank.token) || fresh.Fields["dse_sessionId"] != fmt.Sprintf("session-%d", bank.token) {
				t.Fatal("snapshot did not capture fresh server state")
			}
			if snapshot.Fields["dse_processorState"] != "state-2" || snapshot.Fields["AccountNbr"] != recoveryAccount {
				t.Fatal("bootstrap mutated the persisted stale fields")
			}
			next, err := client.BootstrapForDate(context.Background(), recoveryDate)
			if err != nil {
				t.Fatal(err)
			}
			assertRecoveryTransactions(t, next)
			if bank.probes != 1 {
				t.Fatal("fresh poll unexpectedly probed")
			}
		})
	}
}

func TestBootstrapIncompleteRecoveryPinsClockAndRange(t *testing.T) {
	for _, mode := range []string{"implicit_today", "caller_day", "explicit_range", "initial_probe"} {
		t.Run(mode, func(t *testing.T) {
			bank := newHistoryRecoveryBank(t)
			bank.incomplete = true
			handoff := bank.handoff()
			if mode == "explicit_range" {
				bank.from = "29/09/2026"
				handoff.Fields["_explicitRange"] = "true"
				handoff.Fields["FromDate"], handoff.Fields["ToDate"] = bank.from, bank.to
			}
			if mode == "initial_probe" {
				handoff.Action, handoff.Fields = "", nil
			}
			client := bank.client(handoff)
			clockCalls := 0
			current := time.Date(2026, 10, 5, 23, 59, 59, 0, client.location)
			client.now = func() time.Time { clockCalls++; return current }
			bank.onProbe = func() { current = current.Add(2 * time.Second) }
			bank.probe = func(r *http.Request) (*http.Response, error) {
				return recoveryResponse(r, http.StatusOK, bank.form(recoveryAccount)), nil
			}
			if mode != "initial_probe" {
				bank.token++
			}
			var response Response
			var err error
			if mode == "caller_day" {
				response, err = client.BootstrapForDate(context.Background(), recoveryDate)
			} else {
				response, err = client.Bootstrap(context.Background())
			}
			if err != nil {
				t.Fatal(err)
			}
			assertRecoveryTransactions(t, response)
			if clockCalls != 1 {
				t.Fatalf("bootstrap clock snapshots=%d want=1", clockCalls)
			}
			if mode == "initial_probe" {
				assertRecoveryRequests(t, bank.requests, http.MethodGet, http.MethodPost)
			} else {
				assertRecoveryRequests(t, bank.requests, http.MethodPost, http.MethodGet, http.MethodPost)
			}
			for _, request := range bank.requests {
				if request.method == http.MethodPost && (request.fields.Get("FromDate") != bank.from || request.fields.Get("ToDate") != bank.to || request.fields.Get("AccountNbr") != recoveryAccount) {
					t.Fatal("bootstrap lost original target/range across midnight")
				}
			}
			if mode != "initial_probe" && (handoff.Fields["dse_processorState"] != "state-1" || handoff.Fields["AccountNbr"] != recoveryAccount) {
				t.Fatal("bootstrap mutated caller fields")
			}
			assertRecoveryTarget(t, client)
		})
	}
}

func TestBootstrapIncompleteRecoveryRejectsFreshForm(t *testing.T) {
	for _, invalid := range []string{"missing_state", "missing_session", "summary_only", "foreign_action", "wrong_operation"} {
		t.Run(invalid, func(t *testing.T) {
			bank := newHistoryRecoveryBank(t)
			bank.incomplete = true
			client := bank.client(bank.handoff())
			bank.token++
			lastBody := ""
			bank.probe = func(r *http.Request) (*http.Response, error) {
				lastBody = bank.form(recoveryOtherAccount)
				switch invalid {
				case "missing_state":
					lastBody = strings.ReplaceAll(lastBody, fmt.Sprintf(`value="state-%d"`, bank.token), `value=""`)
				case "missing_session":
					lastBody = strings.ReplaceAll(lastBody, fmt.Sprintf(`value="session-%d"`, bank.token), `value=""`)
				case "summary_only":
					lastBody = strings.ReplaceAll(lastBody, "ibkacctDetailProc", "ibkacctSumProc")
				case "foreign_action":
					lastBody = strings.ReplaceAll(lastBody, `action="/acbib/Request"`, `action="https://example.test/acbib/Request"`)
				case "wrong_operation":
					lastBody = strings.ReplaceAll(lastBody, "ibkacctDetailProc", "unrelatedProc") + `<div>ibkacctDetailProc AccountNbr</div>`
				}
				return recoveryResponse(r, http.StatusOK, lastBody), nil
			}
			response, err := client.BootstrapForDate(context.Background(), recoveryDate)
			var authFailure *AuthFailure
			if !errors.Is(err, ErrHistoryUnavailable) || errors.As(err, &authFailure) || response.Body != lastBody || response.Kind != AccountDetailPage {
				t.Fatalf("invalid fresh form did not fail closed with final response: %v kind=%s", err, response.Kind)
			}
			assertRecoveryRequests(t, bank.requests, http.MethodPost, http.MethodGet)
			assertRecoveryTarget(t, client)
		})
	}
}

func TestBootstrapIncompleteRecoveryReplayProofAndFailures(t *testing.T) {
	for _, result := range []string{"detail", "unknown", "exact_echo", "direct_no_echo", "different_echo", "masked_echo", "redirect_get", "redirect_post", "network", "rate_limit", "server_error", "maintenance", "schema_error"} {
		t.Run(result, func(t *testing.T) {
			bank := newHistoryRecoveryBank(t)
			bank.incomplete = true
			client := bank.client(bank.handoff())
			bank.token++
			transportErr := errors.New("synthetic bootstrap replay transport failure")
			lastBody := ""
			status := http.StatusOK
			bank.replay = func(r *http.Request) (*http.Response, error) {
				account := recoveryAccount
				switch result {
				case "direct_no_echo", "redirect_get", "redirect_post":
					account = ""
				case "different_echo":
					account = recoveryOtherAccount
				case "masked_echo":
					account = "****5678"
				}
				lastBody = bank.history(account, false)
				switch result {
				case "detail":
					lastBody = bank.staleDetail()
				case "unknown":
					lastBody = `<div>Unrecognized result</div>`
				case "network":
					return nil, transportErr
				case "rate_limit":
					status, lastBody = http.StatusTooManyRequests, bank.staleDetail()
				case "server_error":
					status, lastBody = http.StatusServiceUnavailable, bank.staleDetail()
				case "maintenance":
					lastBody = `<div>Hệ thống đang bảo trì</div>`
				case "schema_error":
					lastBody = bank.form(recoveryAccount) + `<table><tr><th>Số GD</th><th>Ghi nợ</th><th>Ghi có</th></tr></table>`
				}
				if strings.HasPrefix(result, "redirect_") {
					redirectStatus := http.StatusFound
					if result == "redirect_post" {
						redirectStatus = http.StatusTemporaryRedirect
					}
					response := recoveryResponse(r, redirectStatus, "")
					response.Header.Set("Location", "/acbib/redirected")
					return response, nil
				}
				return recoveryResponse(r, status, lastBody), nil
			}
			bank.redirect = func(r *http.Request) (*http.Response, error) {
				return recoveryResponse(r, http.StatusOK, lastBody), nil
			}
			response, err := client.BootstrapForDate(context.Background(), recoveryDate)
			switch result {
			case "exact_echo", "direct_no_echo":
				if err != nil {
					t.Fatal(err)
				}
				assertRecoveryTransactions(t, response)
			case "network":
				if !errors.Is(err, transportErr) {
					t.Fatalf("lost transport cause: %v", err)
				}
			case "rate_limit", "server_error", "maintenance":
				if err != nil || response.StatusCode != status {
					t.Fatalf("transient replay changed policy: %v status=%d", err, response.StatusCode)
				}
			case "schema_error":
				if err != nil || response.Kind != HistoryPage {
					t.Fatalf("client must leave proved schema validation to parser: %v", err)
				}
				if _, parseErr := ParseHistoryPage(response.Body); parseErr == nil {
					t.Fatal("invalid schema parsed as empty history")
				}
			default:
				if !errors.Is(err, ErrHistoryUnavailable) {
					t.Fatalf("unproved replay accepted: %v kind=%s", err, response.Kind)
				}
			}
			if result != "network" && response.Body != lastBody {
				t.Fatal("final bank body was changed")
			}
			if strings.HasPrefix(result, "redirect_") {
				method := http.MethodGet
				if result == "redirect_post" {
					method = http.MethodPost
				}
				assertRecoveryRequests(t, bank.requests, http.MethodPost, http.MethodGet, http.MethodPost, method)
				if response.RequestedAccount != "" {
					t.Fatal("redirect was treated as exact POST proof")
				}
			} else {
				assertRecoveryRequests(t, bank.requests, http.MethodPost, http.MethodGet, http.MethodPost)
			}
			assertRecoveryTarget(t, client)
		})
	}
}

func TestBootstrapIncompleteRecoveryProbeBoundaries(t *testing.T) {
	for _, result := range []string{"network", "rate_limit", "server_error", "maintenance", "unknown", "login", "otp", "captcha", "unauthorized", "forbidden"} {
		for _, phase := range []string{"probe", "replay"} {
			t.Run(result+"/"+phase, func(t *testing.T) {
				bank := newHistoryRecoveryBank(t)
				bank.incomplete = true
				client := bank.client(bank.handoff())
				bank.token++
				transportErr := errors.New("synthetic bootstrap transport failure")
				status, body := http.StatusOK, `<div>Unrecognized result</div>`
				challenge := false
				switch result {
				case "rate_limit":
					status, body = http.StatusTooManyRequests, bank.form(recoveryOtherAccount)
				case "server_error":
					status, body = http.StatusServiceUnavailable, bank.form(recoveryOtherAccount)
				case "maintenance":
					body = `<div>Hệ thống đang bảo trì</div>`
				case "login":
					body, challenge = `<input name="username"><input type="password" name="password">`, true
				case "otp":
					body, challenge = `Nhập mã OTP SafeKey`, true
				case "captcha":
					body, challenge = `captcha Mã xác nhận`, true
				case "unauthorized":
					status, challenge = http.StatusUnauthorized, true
				case "forbidden":
					status, challenge = http.StatusForbidden, true
				}
				respond := func(r *http.Request) (*http.Response, error) {
					if result == "network" {
						return nil, transportErr
					}
					return recoveryResponse(r, status, body), nil
				}
				if phase == "probe" {
					bank.probe = respond
				} else {
					bank.replay = respond
				}
				response, err := client.BootstrapForDate(context.Background(), recoveryDate)
				var authFailure *AuthFailure
				switch {
				case result == "network":
					if !errors.Is(err, transportErr) {
						t.Fatalf("transport cause lost: %v", err)
					}
				case challenge && phase == "probe":
					if !errors.As(err, &authFailure) {
						t.Fatalf("fresh probe must confirm auth loss: %v", err)
					}
				case challenge || phase == "probe":
					if !errors.Is(err, ErrInconclusiveAuth) || errors.As(err, &authFailure) {
						t.Fatalf("challenge/transient incorrectly confirmed auth loss: %v", err)
					}
				case result == "unknown":
					if !errors.Is(err, ErrHistoryUnavailable) {
						t.Fatalf("unknown replay accepted: %v", err)
					}
				default:
					if err != nil {
						t.Fatalf("transient replay changed consumer policy: %v", err)
					}
				}
				if result != "network" && (response.StatusCode != status || response.Body != body) {
					t.Fatal("boundary lost final response")
				}
				if phase == "probe" {
					assertRecoveryRequests(t, bank.requests, http.MethodPost, http.MethodGet)
				} else {
					assertRecoveryRequests(t, bank.requests, http.MethodPost, http.MethodGet, http.MethodPost)
				}
				assertRecoveryTarget(t, client)
			})
		}
	}
}

func TestBootstrapOneProbeBudgetAndCleanChallenge(t *testing.T) {
	for _, mode := range []string{"cookies_incomplete", "cookies_challenge", "stale_challenge", "cookies_canceled"} {
		t.Run(mode, func(t *testing.T) {
			bank := newHistoryRecoveryBank(t)
			bank.incomplete = true
			handoff := bank.handoff()
			if strings.HasPrefix(mode, "cookies_") {
				handoff.Action, handoff.Fields = "", nil
			}
			client := bank.client(handoff)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			bank.probe = func(r *http.Request) (*http.Response, error) {
				if mode == "cookies_canceled" {
					cancel()
				}
				return recoveryResponse(r, http.StatusOK, bank.form(recoveryAccount)), nil
			}
			if mode == "stale_challenge" {
				bank.initial = func(r *http.Request) (*http.Response, error) {
					return recoveryResponse(r, http.StatusOK, `Nhập mã OTP SafeKey`), nil
				}
			} else {
				bank.replay = func(r *http.Request) (*http.Response, error) {
					body := bank.staleDetail()
					if mode == "cookies_challenge" {
						body = `Nhập mã OTP SafeKey`
					}
					return recoveryResponse(r, http.StatusOK, body), nil
				}
			}
			response, err := client.BootstrapForDate(ctx, recoveryDate)
			var authFailure *AuthFailure
			switch mode {
			case "cookies_incomplete":
				if !errors.Is(err, ErrHistoryUnavailable) || response.Kind != AccountDetailPage {
					t.Fatalf("used probe budget must fail unavailable: %v", err)
				}
			case "cookies_challenge":
				if !errors.Is(err, ErrInconclusiveAuth) || errors.As(err, &authFailure) {
					t.Fatalf("post-probe challenge cannot confirm expiration: %v", err)
				}
			case "cookies_canceled":
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation lost: %v", err)
				}
			case "stale_challenge":
				if err != nil || response.Kind != AccountDetailPage {
					t.Fatalf("clean challenge probe semantics changed: %v kind=%s", err, response.Kind)
				}
			}
			if mode == "stale_challenge" {
				assertRecoveryRequests(t, bank.requests, http.MethodPost, http.MethodGet)
			} else if mode == "cookies_canceled" {
				assertRecoveryRequests(t, bank.requests, http.MethodGet)
			} else {
				assertRecoveryRequests(t, bank.requests, http.MethodGet, http.MethodPost)
			}
			if bank.probes != 1 {
				t.Fatal("bootstrap exceeded single probe budget")
			}
		})
	}
}

func TestBootstrapIncompleteRecoveryCancellationAndEmptyTarget(t *testing.T) {
	for _, mode := range []string{"before_post", "detail_response", "fresh_probe", "empty_target"} {
		t.Run(mode, func(t *testing.T) {
			bank := newHistoryRecoveryBank(t)
			bank.incomplete = true
			handoff := bank.handoff()
			if mode == "empty_target" {
				delete(handoff.Fields, "AccountNbr")
			}
			client := bank.client(handoff)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "before_post" {
				cancel()
			}
			bank.initial = func(r *http.Request) (*http.Response, error) {
				if mode == "detail_response" {
					cancel()
				}
				return recoveryResponse(r, http.StatusOK, bank.staleDetail()), nil
			}
			if mode == "fresh_probe" {
				bank.onProbe = cancel
			}
			_, err := client.BootstrapForDate(ctx, recoveryDate)
			if mode == "empty_target" {
				if !errors.Is(err, ErrHistoryUnavailable) {
					t.Fatalf("missing original target accepted fresh DOM account: %v", err)
				}
				if len(bank.requests) > 2 {
					t.Fatal("empty target replayed")
				}
				if client.SessionAccountNumber() != "" {
					t.Fatal("empty original target replaced by fresh DOM selection")
				}
			} else if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", err)
			}
			if mode == "detail_response" {
				assertRecoveryRequests(t, bank.requests, http.MethodPost)
			}
			if mode == "fresh_probe" {
				assertRecoveryRequests(t, bank.requests, http.MethodPost, http.MethodGet)
			}
			if mode == "before_post" && len(bank.requests) > 1 {
				t.Fatal("pre-canceled bootstrap triggered recovery")
			}
		})
	}
}

func TestBootstrapPreservesNonRecoveryResponses(t *testing.T) {
	for _, result := range []string{"valid_detail", "unknown", "rate_limit", "server_error", "maintenance", "history_schema_error"} {
		t.Run(result, func(t *testing.T) {
			bank := newHistoryRecoveryBank(t)
			bank.incomplete = true
			client := bank.client(bank.handoff())
			status, body, kind := http.StatusOK, bank.form(recoveryAccount), AccountDetailPage
			switch result {
			case "unknown":
				body, kind = `<div>Unrecognized result</div>`, UnknownPage
			case "rate_limit":
				status, body = http.StatusTooManyRequests, bank.staleDetail()
			case "server_error":
				status, body = http.StatusServiceUnavailable, bank.staleDetail()
			case "maintenance":
				body, kind = `<div>Hệ thống đang bảo trì</div>`, MaintenancePage
			case "history_schema_error":
				body, kind = `<div>ibkacctDetailProc AccountNbr dse_processorState Số GD Ghi nợ Ghi có</div>`, HistoryPage
			}
			bank.initial = func(r *http.Request) (*http.Response, error) { return recoveryResponse(r, status, body), nil }
			response, err := client.BootstrapForDate(context.Background(), recoveryDate)
			if err != nil || response.StatusCode != status || response.Kind != kind || response.Body != body {
				t.Fatalf("non-recovery policy changed: %v kind=%s", err, response.Kind)
			}
			assertRecoveryRequests(t, bank.requests, http.MethodPost)
		})
	}
}
