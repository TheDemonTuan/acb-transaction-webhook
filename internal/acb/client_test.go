package acb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/authbrowser"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestClientClearSessionRemovesAuthentication(t *testing.T) {
	var method, path, cookie string
	client, err := NewClient("https://online.acb.com.vn", roundTripFunc(func(r *http.Request) (*http.Response, error) {
		method, path, cookie = r.Method, r.URL.Path, r.Header.Get("Cookie")
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: http.NoBody, Request: r}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.RestoreSession(authbrowser.Handoff{Version: 1, Action: "https://online.acb.com.vn/acbib/Request", Fields: map[string]string{"dse_sessionId": "synthetic", "dse_processorState": "synthetic"}, Cookies: []authbrowser.Cookie{{Name: "session", Value: "synthetic", Domain: OfficialHost, Path: "/", Secure: true}}}); err != nil {
		t.Fatal(err)
	}
	if err := client.ClearSession(); err != nil {
		t.Fatal(err)
	}
	if err := client.ClearSession(); err != nil {
		t.Fatal(err)
	}
	if _, err := client.SnapshotSession(); !errors.Is(err, ErrAuthenticatedFormStateUnavailable) {
		t.Fatalf("cleared snapshot=%v", err)
	}
	if _, err := client.Get(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if cookie != "" || method != http.MethodGet || path != "/acbib/" {
		t.Fatalf("clear retained auth/form state: method=%s path=%s cookie=%s", method, path, cookie)
	}
}

func TestClientRejectsNonOfficialBase(t *testing.T) {
	if _, err := NewClient("https://example.test", nil); err == nil {
		t.Fatal("accepted untrusted host")
	}
}

func TestClientClassifiesLoginPage(t *testing.T) {
	client, err := NewClient("https://online.acb.com.vn", roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`<input name="username"><input type="password" name="password">`)), Request: r}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Get(context.Background(), "/acbib/Request")
	if err != nil {
		t.Fatal(err)
	}
	if response.Kind != LoginPage {
		t.Fatalf("got %s", response.Kind)
	}
}

func TestBootstrapOverridesCapturedMonthFilter(t *testing.T) {
	var posted url.Values
	client, err := NewClient("https://online.acb.com.vn", roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			t.Fatal(readErr)
		}
		posted, readErr = url.ParseQuery(string(body))
		if readErr != nil {
			t.Fatal(readErr)
		}
		responseBody := `<form action="/acbib/Request"><input name="dse_operationName" value="ibkacctDetailProc"><input name="dse_processorState" value="next"><input name="dse_sessionId" value="next"><input name="AccountNbr" value="12345678"><input name="dse_nextEventName" value="byDate"></form><table><tr><th>Ngày giao dịch</th><th>Số GD</th><th>Ghi nợ</th><th>Ghi có</th></tr><tr><td colspan="4">Không có giao dịch</td></tr></table>`
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(responseBody)), Request: r}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	location := time.FixedZone("Asia/Ho_Chi_Minh", 7*60*60)
	client.now = func() time.Time { return time.Date(2026, 9, 12, 12, 0, 0, 0, location) }
	client.location = location
	if err := client.RestoreSession(authbrowser.Handoff{
		Version: 1,
		Action:  "https://online.acb.com.vn/acbib/Request",
		Fields: map[string]string{
			"dse_operationName":     "ibkacctDetailProc",
			"dse_processorState":    "acctDetailPage",
			"dse_sessionId":         "secret",
			"dse_nextEventName":     "byMonth",
			"activeDatetimeYN":      "Y",
			"activeDatetimeByMonth": "Y",
			"MonthCurr":             "8",
			"YearCurr":              "2026",
			"FromDate":              "13/08/2026",
			"ToDate":                "12/09/2026",
			"CheckRef":              "true",
			"CheckDoiUng":           "true",
		},
		Cookies: []authbrowser.Cookie{{Name: "JSESSIONID", Value: "session", Domain: OfficialHost}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if posted.Get("dse_nextEventName") != "byDate" || posted.Get("activeDatetimeYN") != "N" {
		t.Fatalf("wrong search mode: %v", posted)
	}
	if posted.Get("FromDate") != "12/09/2026" || posted.Get("ToDate") != "12/09/2026" {
		t.Fatalf("wrong upstream range: %v", posted)
	}
	if posted.Has("MonthCurr") || posted.Has("YearCurr") || posted.Has("activeDatetimeByMonth") {
		t.Fatalf("month filters were posted: %v", posted)
	}
}

func TestSessionCookieSurvivesRestoreAndIsSent(t *testing.T) {
	var gotCookie string
	client, err := NewClient("https://online.acb.com.vn", roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotCookie = r.Header.Get("Cookie")
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`ibkacctDetailProc dse_processorState AccountNbr`)), Request: r}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.RestoreCookies([]authbrowser.Cookie{{Name: "JSESSIONID", Value: "session-value", Domain: "online.acb.com.vn", Path: "/", Secure: true, HTTPOnly: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Get(context.Background(), "/acbib/Request"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotCookie, "JSESSIONID=session-value") {
		t.Fatalf("session cookie missing from request: %q", gotCookie)
	}
}

func TestRestoredSessionUsesAuthenticatedBrowserURL(t *testing.T) {
	var gotURL string
	client, err := NewClient("https://online.acb.com.vn", roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotURL = r.URL.String()
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`ibkacctDetailProc dse_processorState AccountNbr`)), Request: r}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.RestoreSession(authbrowser.Handoff{Version: 1, URL: "https://online.acb.com.vn/acbib/AccountSummary?dse_sessionId=opaque", Cookies: []authbrowser.Cookie{{Name: "JSESSIONID", Value: "session-value", Domain: "online.acb.com.vn", Path: "/", Secure: true}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Get(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if gotURL != "https://online.acb.com.vn/acbib/AccountSummary?dse_sessionId=opaque" {
		t.Fatalf("bootstrap URL = %q", gotURL)
	}
}

func TestRestoreSessionRejectsUntrustedBootstrapURL(t *testing.T) {
	client, err := NewClient("https://online.acb.com.vn", nil)
	if err != nil {
		t.Fatal(err)
	}
	err = client.RestoreSession(authbrowser.Handoff{Version: 1, URL: "https://example.test/steal", Cookies: []authbrowser.Cookie{{Name: "JSESSIONID", Value: "session-value", Domain: "online.acb.com.vn", Path: "/", Secure: true}}})
	if err == nil {
		t.Fatal("accepted untrusted bootstrap URL")
	}
}

func TestHistoryForDatePinsHistoricalDateAndOmitsInternalFields(t *testing.T) {
	var posted url.Values
	client, err := NewClient("https://online.acb.com.vn", roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost {
			t.Fatalf("method %s", r.Method)
		}
		body, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			t.Fatal(readErr)
		}
		posted, readErr = url.ParseQuery(string(body))
		if readErr != nil {
			t.Fatal(readErr)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`ibkacctDetailProc dse_processorState AccountNbr Số GD Ghi nợ Ghi có FromDate ToDate`)), Request: r}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.HistoryForDate(context.Background(), "/acbib/Request", map[string]string{
		"dse_operationName":     "ibkacctDetailProc",
		"dse_processorState":    "fresh",
		"dse_sessionId":         "session",
		"AccountNbr":            "12345678",
		"_raw":                  "false",
		"_explicitRange":        "true",
		"activeDatetimeByMonth": "Y",
		"MonthCurr":             "8",
		"YearCurr":              "2026",
		"FromDate":              "01/09/2026",
		"ToDate":                "30/09/2026",
	}, "21/09/2026")
	if err != nil {
		t.Fatal(err)
	}
	if posted.Get("FromDate") != "21/09/2026" || posted.Get("ToDate") != "21/09/2026" {
		t.Fatalf("wrong historical range: %v", posted)
	}
	for _, key := range []string{"_raw", "_explicitRange", "MonthCurr", "YearCurr", "activeDatetimeByMonth"} {
		if posted.Has(key) {
			t.Fatalf("internal field %q was posted: %v", key, posted)
		}
	}
}

func TestHistorySendsCurrentFormState(t *testing.T) {
	client, err := NewClient("https://online.acb.com.vn", roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost {
			t.Fatalf("method %s", r.Method)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "dse_processorState=fresh") {
			t.Fatalf("missing state %q", body)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`ibkacctDetailProc dse_processorState AccountNbr Số GD Ghi nợ Ghi có FromDate ToDate`)), Request: r}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.History(context.Background(), "/acbib/Request", map[string]string{"dse_operationName": "ibkacctDetailProc", "dse_processorState": "fresh"})
	if err != nil || response.Kind != HistoryPage {
		t.Fatalf("response=%+v err=%v", response, err)
	}
}

func TestClientRejectsEndpointOutsideOfficialHost(t *testing.T) {
	client, err := NewClient("https://online.acb.com.vn", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Get(context.Background(), "https://example.test/"); err == nil {
		t.Fatal("accepted untrusted endpoint")
	}
}

func TestEndpointResolvesACBIBPrefix(t *testing.T) {
	client, err := NewClient("https://online.acb.com.vn", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, ep := range []string{"Request", "/Request", "/acbib/Request"} {
		u, err := client.endpoint(ep)
		if err != nil {
			t.Fatalf("endpoint(%q) failed: %v", ep, err)
		}
		if u.Path != "/acbib/Request" {
			t.Fatalf("endpoint(%q) got path %q, want /acbib/Request", ep, u.Path)
		}
	}
}

func TestRestoreCookiesAllowsACBAndOnlineDomains(t *testing.T) {
	client, err := NewClient("https://online.acb.com.vn", nil)
	if err != nil {
		t.Fatal(err)
	}

	validCases := [][]authbrowser.Cookie{
		{{Name: "JSESSIONID", Value: "val1", Domain: "online.acb.com.vn"}},
		{{Name: "TS01", Value: "val2", Domain: ".online.acb.com.vn"}},
		{{Name: "SSO", Value: "val3", Domain: ".acb.com.vn"}},
		{{Name: "ROOT", Value: "val4", Domain: "acb.com.vn"}},
		{{Name: "HOST_ONLY", Value: "val5", Domain: ""}},
	}
	for i, tc := range validCases {
		if err := client.RestoreCookies(tc); err != nil {
			t.Fatalf("case %d: unexpected error for valid cookies: %v", i, err)
		}
	}

	invalidCases := [][]authbrowser.Cookie{
		{{Name: "bad", Value: "val", Domain: "evil.com"}},
		{{Name: "bad", Value: "val", Domain: "online.acb.com.vn.evil.com"}},
		{{Name: "bad", Value: "val", Domain: "acb.com.vn.evil.com"}},
		{{Name: "bad", Value: "val", Domain: "com.vn"}},
		{{Name: "bad", Value: "val", Domain: ".com.vn"}},
		{{Name: "bad", Value: "val", Domain: "sub.online.acb.com.vn"}},
		{{Name: "", Value: "val", Domain: "online.acb.com.vn"}},
		{{Name: "name", Value: "", Domain: "online.acb.com.vn"}},
	}
	for i, tc := range invalidCases {
		if err := client.RestoreCookies(tc); err == nil {
			t.Fatalf("case %d: expected error for invalid cookies, got nil", i)
		}
	}
}

func TestCheckRedirectStripsPort443(t *testing.T) {
	var requestedHost string
	client, err := NewClient("https://online.acb.com.vn", roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/start" {
			header := make(http.Header)
			header.Set("Location", "https://online.acb.com.vn:443/target")
			return &http.Response{StatusCode: http.StatusFound, Header: header, Request: r}, nil
		}
		requestedHost = r.URL.Host
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok")), Request: r}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Get(context.Background(), "/start")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if requestedHost != "online.acb.com.vn" {
		t.Fatalf("expected host to be stripped of port 443, got %q", requestedHost)
	}
}

func TestBootstrapResynchronizesWhenStaleFormRejected(t *testing.T) {
	var requests []*http.Request
	client, err := NewClient("https://online.acb.com.vn", roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests = append(requests, r)
		if r.Method == http.MethodPost {
			// Simulates WebSphere DSE rejecting stale conversational token
			loginBody := `<input name="username"><input type="password" name="password">`
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(loginBody)), Request: r}, nil
		}
		// Subsequent GET /acbib/Request succeeds with active session returning fresh form
		freshAccountBody := `<form action="/acbib/Request"><input name="dse_operationName" value="ibkacctDetailProc"><input name="dse_processorState" value="fresh_token_123"><input name="dse_sessionId" value="fresh_sess_456"><input name="AccountNbr" value="12345678"><input name="dse_nextEventName" value="byDate"></form>`
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(freshAccountBody)), Request: r}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}

	// Restore session with an old token
	if err := client.RestoreSession(authbrowser.Handoff{
		Version: 1,
		URL:     "https://online.acb.com.vn/acbib/Request",
		Action:  "https://online.acb.com.vn/acbib/Request",
		Fields: map[string]string{
			"dse_operationName":  "ibkacctDetailProc",
			"dse_processorState": "stale_token_old",
			"dse_sessionId":      "sess_old",
		},
		Cookies: []authbrowser.Cookie{{Name: "JSESSIONID", Value: "valid_session", Domain: "online.acb.com.vn", Path: "/", Secure: true}},
	}); err != nil {
		t.Fatal(err)
	}

	resp, err := client.Bootstrap(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Kind != AccountDetailPage {
		t.Fatalf("expected resynchronization to AccountDetailPage, got %v", resp.Kind)
	}
	if resp.RequestedAccount != "" {
		t.Fatal("GET reprobe incorrectly proved an outbound account")
	}
	if len(requests) != 2 {
		t.Fatalf("expected 2 requests (POST followed by GET fallback), got %d", len(requests))
	}
	if requests[0].Method != http.MethodPost || requests[1].Method != http.MethodGet {
		t.Fatalf("expected POST then GET, got %s then %s", requests[0].Method, requests[1].Method)
	}

	snapshot, err := client.SnapshotSession()
	if err != nil {
		t.Fatalf("SnapshotSession: %v", err)
	}
	if snapshot.Fields["dse_processorState"] != "fresh_token_123" {
		t.Fatalf("expected fresh token in snapshot, got %q", snapshot.Fields["dse_processorState"])
	}
}

func TestBootstrapReturnsLoginPageWhenSessionTrulyExpired(t *testing.T) {
	var reqCount int
	client, err := NewClient("https://online.acb.com.vn", roundTripFunc(func(r *http.Request) (*http.Response, error) {
		reqCount++
		loginBody := `<input name="username"><input type="password" name="password">`
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(loginBody)), Request: r}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.RestoreSession(authbrowser.Handoff{
		Version: 1,
		URL:     "https://online.acb.com.vn/acbib/Request",
		Action:  "https://online.acb.com.vn/acbib/Request",
		Fields: map[string]string{
			"dse_operationName":  "ibkacctDetailProc",
			"dse_processorState": "token",
			"dse_sessionId":      "sess",
		},
		Cookies: []authbrowser.Cookie{{Name: "JSESSIONID", Value: "expired", Domain: "online.acb.com.vn", Path: "/", Secure: true}},
	}); err != nil {
		t.Fatal(err)
	}
	resp, err := client.Bootstrap(context.Background())
	var authFail *AuthFailure
	if !errors.As(err, &authFail) {
		t.Fatalf("expected AuthFailure error for truly expired session, got: %v", err)
	}
	if authFail.Kind != LoginPage {
		t.Fatalf("expected LoginPage for truly expired session, got %v", authFail.Kind)
	}
	if resp.Kind != LoginPage {
		t.Fatalf("expected LoginPage response, got %v", resp.Kind)
	}
	if reqCount != 2 {
		t.Fatalf("expected 2 requests (POST then GET confirmation), got %d", reqCount)
	}
}

func TestHistoryLoginPageResyncsBeforeAuthRequired(t *testing.T) {
	var requests []*http.Request
	client, err := NewClient("https://online.acb.com.vn", roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests = append(requests, r)
		if len(requests) == 1 {
			// Request 1: POST history with stale token -> returns LoginPage
			loginBody := `<input name="username"><input type="password" name="password">`
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(loginBody)), Request: r}, nil
		} else if len(requests) == 2 {
			// Request 2: GET /acbib/Request probe -> returns fresh AccountDetailPage
			freshAccountBody := `<form action="/acbib/Request"><input name="dse_operationName" value="ibkacctDetailProc"><input name="dse_processorState" value="fresh_token_456"><input name="dse_sessionId" value="fresh_sess_789"><input name="AccountNbr" value="12345678"><input name="dse_nextEventName" value="byDate"></form>`
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(freshAccountBody)), Request: r}, nil
		}
		// Request 3: Replayed POST history with fresh form -> succeeds with history table
		historyBody := `
		<form action="/acbib/Request"><input name="dse_operationName" value="ibkacctDetailProc"><input name="dse_processorState" value="fresh_token_456"><input name="dse_sessionId" value="fresh_sess_789"><input name="AccountNbr" value="12345678"></form>
		<table>
		  <tr><th>Ngày hiệu lực</th><th>Ngày giao dịch</th><th>Số GD</th><th>Ghi nợ</th><th>Ghi có</th><th>Số dư</th><th>Nội dung giao dịch</th></tr>
		  <tr><td>10/09/2026</td><td>10/09/2026</td><td>7788</td><td>-</td><td>150.000</td><td>2.000.000</td><td>Test Nap Tien</td></tr>
		</table>`
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(historyBody)), Request: r}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}

	fields := map[string]string{
		"dse_operationName":  "ibkacctDetailProc",
		"dse_processorState": "stale_token_old",
		"dse_sessionId":      "sess_old",
		"AccountNbr":         "12345678",
		"FromDate":           "10/09/2026",
		"ToDate":             "10/09/2026",
		"_explicitRange":     "true",
	}

	resp, err := client.History(context.Background(), "/acbib/Request", fields)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Kind != HistoryPage {
		t.Fatalf("expected HistoryPage after resync and replay, got: %s", resp.Kind)
	}
	if resp.RequestedAccount != fields["AccountNbr"] {
		t.Fatal("history replay did not retain its actual outbound account")
	}
	if len(requests) != 3 {
		t.Fatalf("expected exactly 3 requests (POST, GET probe, replay POST), got %d", len(requests))
	}
	if requests[0].Method != http.MethodPost || requests[1].Method != http.MethodGet || requests[2].Method != http.MethodPost {
		t.Fatalf("expected POST -> GET -> POST sequence, got %s -> %s -> %s", requests[0].Method, requests[1].Method, requests[2].Method)
	}
}

func TestHistoryConfirmedLoginTransitionsAuthRequired(t *testing.T) {
	var requests []*http.Request
	client, err := NewClient("https://online.acb.com.vn", roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests = append(requests, r)
		loginBody := `<input name="username"><input type="password" name="password">`
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(loginBody)), Request: r}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}

	fields := map[string]string{
		"dse_operationName":  "ibkacctDetailProc",
		"dse_processorState": "token",
		"dse_sessionId":      "sess",
		"AccountNbr":         "12345678",
	}

	resp, err := client.History(context.Background(), "/acbib/Request", fields)
	var authFail *AuthFailure
	if !errors.As(err, &authFail) {
		t.Fatalf("expected AuthFailure error, got: %v", err)
	}
	if authFail.Kind != LoginPage {
		t.Fatalf("expected LoginPage AuthFailure, got: %v", authFail.Kind)
	}
	if resp.Kind != LoginPage {
		t.Fatalf("expected LoginPage response, got: %v", resp.Kind)
	}
	if len(requests) != 2 {
		t.Fatalf("expected exactly 2 requests (POST then GET probe confirmation), got %d", len(requests))
	}
}

func TestHistoryContinuationStaleResyncsToConversationReset(t *testing.T) {
	var requests []*http.Request
	client, err := NewClient("https://online.acb.com.vn", roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests = append(requests, r)
		if len(requests) == 1 {
			loginBody := `<input name="username"><input type="password" name="password">`
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(loginBody)), Request: r}, nil
		}
		freshAccountBody := `<form action="/acbib/Request"><input name="dse_operationName" value="ibkacctDetailProc"><input name="dse_processorState" value="fresh_token_123"><input name="dse_sessionId" value="fresh_sess_456"><input name="AccountNbr" value="12345678"><input name="dse_nextEventName" value="byDate"></form>`
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(freshAccountBody)), Request: r}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}

	continuationFields := map[string]string{
		"_raw":               "true",
		"dse_operationName":  "ibkacctDetailProc",
		"dse_processorState": "stale_continuation_token",
		"dse_sessionId":      "sess",
		"AccountNbr":         "12345678",
	}

	resp, err := client.History(context.Background(), "/acbib/Request", continuationFields)
	if !errors.Is(err, ErrConversationReset) {
		t.Fatalf("expected ErrConversationReset, got: %v", err)
	}
	if resp.Kind != AccountDetailPage && resp.Kind != HistoryPage {
		t.Fatalf("expected probe response to be AccountDetailPage, got: %v", resp.Kind)
	}
	if len(requests) != 2 {
		t.Fatalf("continuation should not replay cursor; expected exactly 2 requests, got %d", len(requests))
	}
}

func TestHistoryLoginPageProbeTransportErrorDoesNotConfirmAuth(t *testing.T) {
	var requests []*http.Request
	client, err := NewClient("https://online.acb.com.vn", roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests = append(requests, r)
		if len(requests) == 1 {
			loginBody := `<input name="username"><input type="password" name="password">`
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(loginBody)), Request: r}, nil
		}
		return nil, errors.New("probe connection reset by peer")
	}))
	if err != nil {
		t.Fatal(err)
	}

	fields := map[string]string{
		"dse_operationName":  "ibkacctDetailProc",
		"dse_processorState": "token",
		"dse_sessionId":      "sess",
	}

	_, err = client.History(context.Background(), "/acbib/Request", fields)
	var authFail *AuthFailure
	if errors.As(err, &authFail) {
		t.Fatalf("probe network failure must not be classified as AuthFailure: %v", err)
	}
	if err == nil {
		t.Fatal("expected transport error, got nil")
	}
}

func TestHistoryReplayAuthChallengeReturnsInconclusiveAuth(t *testing.T) {
	var requests []*http.Request
	client, err := NewClient("https://online.acb.com.vn", roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests = append(requests, r)
		if len(requests) == 1 {
			// Request 1: POST history returns LoginPage
			loginBody := `<input name="username"><input type="password" name="password">`
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(loginBody)), Request: r}, nil
		} else if len(requests) == 2 {
			// Request 2: Probe GET /acbib/Request returns clean AccountDetailPage
			freshAccountBody := `<form action="/acbib/Request"><input name="dse_operationName" value="ibkacctDetailProc"><input name="dse_processorState" value="fresh_token"><input name="dse_sessionId" value="fresh_sess"><input name="AccountNbr" value="12345678"></form>`
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(freshAccountBody)), Request: r}, nil
		}
		// Request 3: Replayed POST also returns LoginPage (auth challenged again)
		loginBody := `<input name="username"><input type="password" name="password">`
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(loginBody)), Request: r}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}

	fields := map[string]string{
		"dse_operationName":  "ibkacctDetailProc",
		"dse_processorState": "stale_token",
		"dse_sessionId":      "sess",
		"AccountNbr":         "12345678",
		"_explicitRange":     "true",
		"FromDate":           "10/09/2026",
		"ToDate":             "10/09/2026",
	}

	_, err = client.History(context.Background(), "/acbib/Request", fields)
	var authFail *AuthFailure
	if !errors.Is(err, ErrInconclusiveAuth) || errors.As(err, &authFail) {
		t.Fatalf("replay challenge lacks a fresh confirming probe: %v", err)
	}
	if len(requests) != 3 {
		t.Fatalf("expected 3 requests, got %d", len(requests))
	}
}

func TestClientCloseIdleConnections(t *testing.T) {
	client, err := NewClient("https://online.acb.com.vn", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Verify CloseIdleConnections executes cleanly without panic
	client.CloseIdleConnections()
}

// Keep the synthetic official origin intact while all traffic goes to local TLS.
func newAccountContinuityClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport.TLSClientConfig.ServerName = serverURL.Hostname()
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != OfficialHost+":443" {
			return nil, fmt.Errorf("unexpected fixture destination %q", address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	t.Cleanup(transport.CloseIdleConnections)
	client, err := NewClient("https://"+OfficialHost, transport)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func accountContinuityHandoff(account string) authbrowser.Handoff {
	return authbrowser.Handoff{
		Version: 1,
		Action:  "https://" + OfficialHost + "/acbib/Request",
		Fields: map[string]string{
			"dse_operationName":  "ibkacctDetailProc",
			"dse_processorState": "old-state",
			"dse_sessionId":      "old-session",
			"AccountNbr":         account,
		},
		Cookies: []authbrowser.Cookie{{Name: "JSESSIONID", Value: "synthetic-session", Domain: OfficialHost, Path: "/", Secure: true}},
	}
}

func accountContinuityForm(accountField, token string) string {
	return `<form action="/acbib/Request"><input name="dse_operationName" value="ibkacctDetailProc"><input name="dse_processorState" value="` + token + `-state"><input name="dse_sessionId" value="` + token + `-session">` + accountField + `</form>`
}

const accountContinuityHistoryTable = `<table><tr><th>Ngày giao dịch</th><th>Số GD</th><th>Ghi nợ</th><th>Ghi có</th></tr><tr><td colspan="4">Không có giao dịch</td></tr></table>`

func TestClientHistoryAccountOmissionKeepsSelectionAcrossSnapshot(t *testing.T) {
	for _, field := range []struct {
		name   string
		markup string
	}{
		{name: "omitted"},
		{name: "empty", markup: `<input name="AccountNbr" value="">`},
	} {
		for _, operation := range []string{"bootstrap", "bootstrap_date", "history", "history_date"} {
			t.Run(field.name+"/"+operation, func(t *testing.T) {
				posted := make(chan url.Values, 4)
				var calls atomic.Int32
				client := newAccountContinuityClient(t, func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodPost || r.Host != OfficialHost {
						t.Errorf("unexpected fixture request: %s %s", r.Method, r.Host)
					}
					if err := r.ParseForm(); err != nil {
						t.Error(err)
					}
					posted <- r.PostForm
					call := calls.Add(1)
					io.WriteString(w, accountContinuityForm(field.markup, fmt.Sprintf("fresh-%d", call))+accountContinuityHistoryTable)
				})
				handoff := accountContinuityHandoff("12345678")
				if err := client.RestoreSession(handoff); err != nil {
					t.Fatal(err)
				}
				selected := handoff.Fields["AccountNbr"]
				var response Response
				var err error
				switch operation {
				case "bootstrap":
					response, err = client.Bootstrap(context.Background())
				case "bootstrap_date":
					response, err = client.BootstrapForDate(context.Background(), "03/10/2026")
				case "history", "history_date":
					fields := cloneFields(handoff.Fields)
					selected = "87654321" // Caller-selected account must remain authoritative for the request.
					fields["AccountNbr"] = selected
					if operation == "history" {
						response, err = client.History(context.Background(), handoff.Action, fields)
					} else {
						response, err = client.HistoryForDate(context.Background(), handoff.Action, fields, "03/10/2026")
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				if response.StatusCode != http.StatusOK || response.Kind != HistoryPage || response.RequestedAccount != selected {
					t.Fatalf("unexpected direct response metadata: status=%d kind=%s requested=%q", response.StatusCode, response.Kind, response.RequestedAccount)
				}
				if _, err := ParseHistoryPage(response.Body); err != nil {
					t.Fatal(err)
				}
				form, err := ExtractHistoryForm(response.Body)
				if err != nil {
					t.Fatal(err)
				}
				if form.Fields["AccountNbr"] != "" {
					t.Fatal("bank response was overwritten with the selected account")
				}
				if first := <-posted; first.Get("AccountNbr") != selected {
					t.Fatal("first request did not post the selected account")
				}
				snapshot, err := client.SnapshotSession()
				if err != nil {
					t.Fatal(err)
				}
				if snapshot.Fields["AccountNbr"] != selected || client.SessionAccountNumber() != selected || snapshot.Fields["dse_processorState"] != "fresh-1-state" || snapshot.Fields["dse_sessionId"] != "fresh-1-session" {
					t.Fatal("snapshot lost the selection or kept stale conversational tokens")
				}
				form.Fields["_raw"] = "true"
				form.Fields["dse_nextEventName"] = "nextPage"
				nextResponse, err := client.HistoryForDate(context.Background(), form.Action, form.Fields, "03/10/2026")
				if err != nil {
					t.Fatal(err)
				}
				next := <-posted
				if next.Get("AccountNbr") != selected || next.Get("dse_processorState") != "fresh-1-state" || next.Get("dse_sessionId") != "fresh-1-session" || next.Get("dse_nextEventName") != "nextPage" || nextResponse.RequestedAccount != selected {
					t.Fatal("continuation lost the selected account, fresh tokens, or navigation event")
				}
				snapshot, err = client.SnapshotSession()
				if err != nil {
					t.Fatal(err)
				}
				restarted, err := NewClient("https://"+OfficialHost, client.http.Transport)
				if err != nil {
					t.Fatal(err)
				}
				if err := restarted.RestoreSession(snapshot); err != nil {
					t.Fatal(err)
				}
				response, err = restarted.BootstrapForDate(context.Background(), "04/10/2026")
				if err != nil {
					t.Fatal(err)
				}
				last := <-posted
				if response.RequestedAccount != selected || last.Get("AccountNbr") != selected || last.Get("dse_processorState") != "fresh-2-state" || last.Get("dse_sessionId") != "fresh-2-session" || last.Get("FromDate") != "04/10/2026" || last.Get("ToDate") != "04/10/2026" {
					t.Fatal("restored snapshot lost the exact account, fresh tokens, or query day")
				}
			})
		}
	}
}

func TestClientNeverReplacesReturnedAccountOrFallsBackOnAccountDetail(t *testing.T) {
	for _, tc := range []struct {
		name    string
		field   string
		account string
		kind    PageKind
	}{
		{name: "history_other", field: `<input name="AccountNbr" value="87654321">`, account: "87654321", kind: HistoryPage},
		{name: "history_masked", field: `<input name="AccountNbr" value="****5678">`, account: "****5678", kind: HistoryPage},
		{name: "detail_omitted", kind: AccountDetailPage},
		{name: "detail_empty", field: `<input name="AccountNbr" value="">`, kind: AccountDetailPage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := accountContinuityForm(tc.field, "fresh")
			if tc.kind == HistoryPage {
				body += accountContinuityHistoryTable
			}
			client := newAccountContinuityClient(t, func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, body)
			})
			if err := client.RestoreSession(accountContinuityHandoff("12345678")); err != nil {
				t.Fatal(err)
			}
			response, err := client.Bootstrap(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if response.Kind != tc.kind || response.RequestedAccount != "12345678" || response.Body != body {
				t.Fatal("response identity or actual request metadata was changed")
			}
			snapshot, err := client.SnapshotSession()
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.Fields["AccountNbr"] != tc.account || client.SessionAccountNumber() != tc.account || snapshot.Fields["dse_processorState"] != "fresh-state" {
				t.Fatal("returned account was replaced or account-detail omission used a fallback")
			}
		})
	}
}

func TestClientCookieOnlyHistoryDoesNotInventAccount(t *testing.T) {
	requests := make(chan string, 3)
	client := newAccountContinuityClient(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.PostForm.Get("AccountNbr") != "" {
			t.Error("cookie-only history invented an outbound account")
		}
		requests <- r.Method
		io.WriteString(w, accountContinuityForm("", "fresh")+accountContinuityHistoryTable)
	})
	if err := client.RestoreCookies(accountContinuityHandoff("").Cookies); err != nil {
		t.Fatal(err)
	}
	response, err := client.Bootstrap(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if response.Kind != HistoryPage || response.RequestedAccount != "" || client.SessionAccountNumber() != "" {
		t.Fatal("cookie-only session inferred an account")
	}
	if first, second := <-requests, <-requests; first != http.MethodGet || second != http.MethodPost {
		t.Fatalf("cookie-only bootstrap sequence=%s,%s", first, second)
	}
	snapshot, err := client.SnapshotSession()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Fields["AccountNbr"] != "" {
		t.Fatal("cookie-only snapshot invented an account")
	}
}

func TestClientGetAndRedirectResponsesDoNotProveRequestedAccount(t *testing.T) {
	for _, scenario := range []string{"get", "redirect_get", "redirect_post", "redirect_query", "redirect_back"} {
		t.Run(scenario, func(t *testing.T) {
			var calls atomic.Int32
			client := newAccountContinuityClient(t, func(w http.ResponseWriter, r *http.Request) {
				call := calls.Add(1)
				if call == 1 && scenario != "get" {
					status, location := http.StatusTemporaryRedirect, "/acbib/other"
					if scenario == "redirect_get" {
						status = http.StatusFound
					} else if scenario == "redirect_query" {
						location = "/acbib/Request?fresh=true"
					}
					http.Redirect(w, r, location, status)
					return
				}
				if call == 2 && scenario == "redirect_back" {
					http.Redirect(w, r, "/acbib/Request", http.StatusTemporaryRedirect)
					return
				}
				io.WriteString(w, accountContinuityForm("", "fresh")+accountContinuityHistoryTable)
			})
			if err := client.RestoreSession(accountContinuityHandoff("12345678")); err != nil {
				t.Fatal(err)
			}
			var response Response
			var err error
			if scenario == "get" {
				response, err = client.Get(context.Background(), "/acbib/Request")
			} else {
				response, err = client.Bootstrap(context.Background())
			}
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusOK || response.Kind != HistoryPage || response.RequestedAccount != "" {
				t.Fatal("GET or redirect was treated as direct account-bound POST proof")
			}
			expectedCalls := 2
			if scenario == "get" {
				expectedCalls = 1
			} else if scenario == "redirect_back" {
				expectedCalls = 3
			}
			if got := calls.Load(); got != int32(expectedCalls) {
				t.Fatalf("request count=%d want=%d", got, expectedCalls)
			}
		})
	}
}

func TestClientHistoryReprobePreservesSelectionWithoutAccountProof(t *testing.T) {
	requests := make(chan string, 4)
	var calls atomic.Int32
	client := newAccountContinuityClient(t, func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		requests <- r.Method
		if call == 1 {
			io.WriteString(w, `<input name="username"><input type="password" name="password">`)
			return
		}
		io.WriteString(w, accountContinuityForm("", "resynced")+accountContinuityHistoryTable)
	})
	if err := client.RestoreSession(accountContinuityHandoff("12345678")); err != nil {
		t.Fatal(err)
	}
	response, err := client.Bootstrap(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if response.Kind != HistoryPage || response.RequestedAccount != "" {
		t.Fatal("reprobe response incorrectly proved an account-bound POST")
	}
	if first, second := <-requests, <-requests; first != http.MethodPost || second != http.MethodGet {
		t.Fatalf("reprobe sequence=%s,%s", first, second)
	}
	snapshot, err := client.SnapshotSession()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Fields["AccountNbr"] != "12345678" || snapshot.Fields["dse_processorState"] != "resynced-state" || snapshot.Fields["dse_sessionId"] != "resynced-session" {
		t.Fatal("history reprobe lost the existing candidate or fresh tokens")
	}
	response, err = client.Bootstrap(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if response.RequestedAccount != "12345678" || <-requests != http.MethodPost {
		t.Fatal("next direct POST failed to reuse the exact candidate")
	}
}
