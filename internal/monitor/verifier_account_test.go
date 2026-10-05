package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
	"github.com/thedemontuan/acb-transaction-webhook/internal/authbrowser"
	"github.com/thedemontuan/acb-transaction-webhook/internal/authsession"
	"github.com/thedemontuan/acb-transaction-webhook/internal/security"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

type verifierAccountTransport func(*http.Request) (*http.Response, error)

func (f verifierAccountTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSessionVerifierFiniteResultsAndExactAccountGate(t *testing.T) {
	for _, tc := range []struct {
		name, account, body, code, phase string
		cookieOnly                       bool
		upstreamErr                      error
	}{
		{name: "exact", account: "222222222", phase: "COMPLETE"},
		{name: "other", account: "111111111", code: "VERIFICATION_ACCOUNT_MISMATCH", phase: "ACCOUNT"},
		{name: "masked", account: "***2222", code: "VERIFICATION_ACCOUNT_MISMATCH", phase: "ACCOUNT"},
		{name: "missing", code: "VERIFICATION_ACCOUNT_MISSING", phase: "ACCOUNT"},
		{name: "empty", body: `<form action="/acbib/Request"><input name="dse_operationName" value="ibkacctDetailProc"><input name="dse_processorState" value="acctDetailPage"><input name="AccountNbr" value=""></form>`, code: "VERIFICATION_ACCOUNT_MISSING", phase: "ACCOUNT"},
		{name: "history_omitted_account", body: `<form action="/acbib/Request"><input name="dse_operationName" value="ibkacctDetailProc"><input name="dse_processorState" value="acctDetailPage"><input name="dse_sessionId" value="fresh-session"></form><table><tr><th>Ngày giao dịch</th><th>Số GD</th><th>Ghi nợ</th><th>Ghi có</th></tr><tr><td colspan="4">Không có giao dịch</td></tr></table>`, phase: "COMPLETE"},
		{name: "history_empty_account", body: `<form action="/acbib/Request"><input name="dse_operationName" value="ibkacctDetailProc"><input name="dse_processorState" value="acctDetailPage"><input name="dse_sessionId" value="fresh-session"><input name="AccountNbr" value=""></form><table><tr><th>Ngày giao dịch</th><th>Số GD</th><th>Ghi nợ</th><th>Ghi có</th></tr><tr><td>03/10/2026</td><td>TX1</td><td>0</td><td>100</td></tr></table>`, phase: "COMPLETE"},
		{name: "history_unbound_get", body: `<form action="/acbib/Request"><input name="dse_operationName" value="ibkacctDetailProc"><input name="dse_processorState" value="acctDetailPage"><input name="dse_sessionId" value="fresh-session"></form><table><tr><th>Ngày giao dịch</th><th>Số GD</th><th>Ghi nợ</th><th>Ghi có</th></tr><tr><td colspan="4">Không có giao dịch</td></tr></table>`, code: "VERIFICATION_ACCOUNT_MISSING", phase: "ACCOUNT"},
		{name: "history_invalid_rows", body: `<form action="/acbib/Request"><input name="dse_operationName" value="ibkacctDetailProc"><input name="dse_processorState" value="acctDetailPage"><input name="dse_sessionId" value="fresh-session"></form><table><tr><th>Ngày giao dịch</th><th>Số GD</th><th>Ghi nợ</th><th>Ghi có</th></tr></table>`, code: "VERIFICATION_FORM_INVALID", phase: "ACCOUNT"},
		{name: "invalid_form", body: `<div>ibkacctDetailProc AccountNbr synthetic-private-body</div>`, code: "VERIFICATION_UNAVAILABLE", phase: "BOOTSTRAP"},
		{name: "login", body: `<input name="username"><input name="password">`, code: "VERIFICATION_AUTH_REQUIRED", phase: "BOOTSTRAP"},
		{name: "otp", body: `<input name="otp">`, code: "VERIFICATION_AUTH_REQUIRED", phase: "BOOTSTRAP"},
		{name: "captcha", body: `<input name="captcha">`, code: "VERIFICATION_AUTH_REQUIRED", phase: "BOOTSTRAP"},
		{name: "maintenance", body: `maintenance synthetic-private-body`, code: "VERIFICATION_MAINTENANCE", phase: "BOOTSTRAP"},
		{name: "unsupported", body: `synthetic-private-body`, code: "VERIFICATION_PAGE_UNSUPPORTED", phase: "BOOTSTRAP"},
		{name: "network", upstreamErr: errors.New("synthetic-secret-error https://private.example/credential"), code: "VERIFICATION_UNAVAILABLE", phase: "BOOTSTRAP"},
		{name: "cookie_only", account: "111111111", cookieOnly: true, phase: "COMPLETE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureVerificationLogs(t)
			ctx := context.Background()
			store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "verify.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			conn, err := store.ConfigureConnection(ctx, "***2222")
			if err != nil {
				t.Fatal(err)
			}
			attempt, err := store.StartAuthAttempt(ctx, "fixture-owner", 15*time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			keyring, err := security.NewKeyring(bytes.Repeat([]byte{7}, 32))
			if err != nil {
				t.Fatal(err)
			}
			candidate := authbrowser.Handoff{Version: 1, URL: "https://online.acb.com.vn/acbib/Request", Action: "https://online.acb.com.vn/acbib/Request", Fields: map[string]string{"dse_sessionId": "synthetic-session", "dse_processorState": "acctDetailPage", "dse_operationName": "ibkacctDetailProc", "AccountNbr": "222222222"}, Cookies: []authbrowser.Cookie{{Name: "session", Value: "synthetic-cookie", Domain: acb.OfficialHost, Path: "/", Secure: true}}}
			if tc.cookieOnly {
				candidate.Action, candidate.Fields = "", nil
			}
			handoff, err := authbrowser.EncodeHandoff(candidate, []byte("synthetic-nonce"))
			if err != nil {
				t.Fatal(err)
			}
			envelope, err := keyring.Encrypt([]byte(handoff), security.SessionAAD(conn.ID, attempt.Generation))
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(envelope)
			if err != nil {
				t.Fatal(err)
			}
			accountField := ""
			if tc.account != "" {
				accountField = `<input type="hidden" name="AccountNbr" value="` + tc.account + `">`
			}
			body := `<form action="/acbib/Request" method="post"><input name="dse_operationName" value="ibkacctDetailProc"><input name="dse_processorState" value="acctDetailPage"><input name="dse_sessionId" value="fresh-session">` + accountField + `</form>`
			if tc.body != "" {
				body = tc.body
			}
			client, err := acb.NewClient("https://online.acb.com.vn", verifierAccountTransport(func(r *http.Request) (*http.Response, error) {
				if tc.upstreamErr != nil {
					return nil, tc.upstreamErr
				}
				if tc.name == "history_unbound_get" {
					r = r.Clone(r.Context())
					r.Method = http.MethodGet
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			verifier := NewSessionVerifier(NewSessionLoader(store, keyring, client), client)
			verified, verifyErr := verifier.VerifySession(ctx, conn.ID, attempt.Generation, encoded)
			err = verifyErr
			if err != nil && len(verified) != 0 {
				t.Fatal("rejected candidate returned a committable session")
			}
			if got := authsession.VerificationCode(err); got != tc.code {
				t.Fatalf("verification code=%q, want %q", got, tc.code)
			}
			if tc.code == "" && err != nil {
				t.Fatalf("accepted candidate failed: %v", err)
			}
			if err != nil && err.Error() != tc.code {
				t.Fatalf("error must contain only finite code: %v", err)
			}
			logCode := tc.code
			if logCode == "" {
				logCode = "VERIFIED"
			}
			logs.assertResult(t, attempt.Generation, tc.phase, logCode, tc.upstreamErr == nil, tc.code == "" && !tc.cookieOnly || tc.code == "VERIFICATION_ACCOUNT_MISSING" || tc.code == "VERIFICATION_ACCOUNT_MISMATCH" || tc.name == "history_invalid_rows", tc.account != "" && tc.phase != "BOOTSTRAP" && !tc.cookieOnly, tc.name == "exact")
			current, err := store.AuthAttemptStatusForOwner(ctx, attempt.ID, "fixture-owner")
			if err != nil {
				t.Fatal(err)
			}
			if current.Status == "VERIFIED" {
				t.Fatal("verification alone committed authentication")
			}
		})
	}
}
