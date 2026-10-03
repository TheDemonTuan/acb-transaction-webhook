package authrecovery

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
	"github.com/thedemontuan/acb-transaction-webhook/internal/authbrowser"
	"github.com/thedemontuan/acb-transaction-webhook/internal/authsession"
	"github.com/thedemontuan/acb-transaction-webhook/internal/monitor"
	"github.com/thedemontuan/acb-transaction-webhook/internal/security"
	"github.com/thedemontuan/acb-transaction-webhook/internal/workerrpc"
)

type verificationRPCHandler struct {
	workerrpc.WorkerHandler
	verifier *monitor.SessionVerifier
}

func (h verificationRPCHandler) VerifySession(ctx context.Context, id string, generation int64, envelope []byte) error {
	return h.verifier.VerifySession(ctx, id, generation, envelope)
}

type verificationHandoff struct {
	*authbrowser.Client
	value string
}

func (b verificationHandoff) Handoff(context.Context, string) (string, error) { return b.value, nil }

type verificationBankTransport func(*http.Request) (*http.Response, error)

func (f verificationBankTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestVerificationAccountResultAcrossRPCFinalizerCoordinator(t *testing.T) {
	for _, tc := range []struct{ name, account, state, reason string }{
		{"exact", "222222222", "CATCHING_UP", ""},
		{"other", "111111111", "MANUAL_REQUIRED", "VERIFICATION_ACCOUNT_MISMATCH"},
		{"missing", "", "MANUAL_REQUIRED", "VERIFICATION_ACCOUNT_MISSING"},
		{"history_omitted", "", "CATCHING_UP", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCoordinatorFixture(t)
			keyring, err := security.NewKeyring(bytes.Repeat([]byte{7}, 32))
			if err != nil {
				t.Fatal(err)
			}
			f.store.WithKeyring(keyring)
			prior, err := f.store.Connection(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			stamp := time.Now().UTC().Format(time.RFC3339Nano)
			if _, err := f.store.DB().ExecContext(f.ctx, `INSERT INTO sessions(connection_id,generation,envelope,key_id,verified_at,updated_at) VALUES(?,?,?,'fixture',?,?)`, prior.ID, prior.Generation, []byte("previous-encrypted-session"), stamp, stamp); err != nil {
				t.Fatal(err)
			}
			candidate := authbrowser.Handoff{Version: 1, URL: "https://online.acb.com.vn/acbib/Request", Action: "https://online.acb.com.vn/acbib/Request", Fields: map[string]string{"dse_sessionId": "synthetic-session", "dse_processorState": "acctDetailPage", "dse_operationName": "ibkacctDetailProc", "AccountNbr": "222222222"}, Cookies: []authbrowser.Cookie{{Name: "session", Value: "synthetic-cookie", Domain: acb.OfficialHost, Path: "/", Secure: true}}}
			handoff, err := authbrowser.EncodeHandoff(candidate, []byte("synthetic-nonce"))
			if err != nil {
				t.Fatal(err)
			}
			accountField := ""
			if tc.account != "" {
				accountField = `<input name="AccountNbr" value="` + tc.account + `">`
			}
			bank, err := acb.NewClient("https://online.acb.com.vn", verificationBankTransport(func(r *http.Request) (*http.Response, error) {
				body := `<form action="/acbib/Request"><input name="dse_operationName" value="ibkacctDetailProc"><input name="dse_processorState" value="acctDetailPage"><input name="dse_sessionId" value="fresh-session">` + accountField + `</form>`
				if tc.name == "history_omitted" {
					body += `<table><tr><th>Ngày giao dịch</th><th>Số GD</th><th>Ghi nợ</th><th>Ghi có</th></tr><tr><td colspan="4">Không có giao dịch</td></tr></table>`
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			verifier := monitor.NewSessionVerifier(monitor.NewSessionLoader(f.store, keyring, bank), bank)
			rpc, err := workerrpc.NewServer(verificationRPCHandler{verifier: verifier}, "synthetic-rpc-token")
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(rpc.Handler())
			defer server.Close()
			f.controller.Finalizer = authsession.NewFinalizer(authsession.Options{Store: f.store, Browser: verificationHandoff{Client: f.browser, value: handoff}, Keyring: keyring, Verifier: workerrpc.NewClient(server.URL, "synthetic-rpc-token")})
			f.reconcile()
			ch := f.pending()
			if err := f.broker.HandleReply(f.ctx, 22, ch.PromptMessageID, 901, "001234"); err != nil {
				t.Fatal(err)
			}
			e := f.episode()
			if e.State != tc.state || e.ReasonCode != tc.reason || e.OTPSubmissions != 1 {
				t.Fatalf("account result lost: state=%s reason=%s submissions=%d", e.State, e.ReasonCode, e.OTPSubmissions)
			}
			var runs, sessions int
			if err := f.store.DB().QueryRowContext(f.ctx, `SELECT (SELECT count(*) FROM recovery_runs WHERE event_key=?),(SELECT count(*) FROM sessions WHERE connection_id=? AND generation=?)`, e.AttemptID, e.ConnectionID, e.Generation).Scan(&runs, &sessions); err != nil {
				t.Fatal(err)
			}
			expected := 0
			if tc.state == "CATCHING_UP" {
				expected = 1
			}
			if runs != expected || sessions != expected {
				t.Fatalf("incorrect commit/recovery gate: sessions=%d runs=%d", sessions, runs)
			}
			if tc.state != "CATCHING_UP" {
				old, err := f.store.Session(f.ctx, prior.ID, prior.Generation)
				if err != nil || string(old.Envelope) != "previous-encrypted-session" {
					t.Fatalf("rejection replaced original encrypted session: %v", err)
				}
			}
			f.reconcile()
			if f.starts != 1 || f.logins != 1 || f.otps != 1 {
				t.Fatal("verification replayed authentication")
			}
		})
	}
}
