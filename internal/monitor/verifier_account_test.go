package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
	"github.com/thedemontuan/acb-transaction-webhook/internal/authbrowser"
	"github.com/thedemontuan/acb-transaction-webhook/internal/security"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

type verifierAccountTransport func(*http.Request) (*http.Response, error)

func (f verifierAccountTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSessionVerifierRejectsDifferentReturnedAccount(t *testing.T) {
	for _, tc := range []struct {
		name, account string
		accepted      bool
	}{
		{"exact", "222222222", true},
		{"other", "111111111", false},
		{"masked", "***2222", false},
		{"missing", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			handoff, err := authbrowser.EncodeHandoff(authbrowser.Handoff{Version: 1, URL: "https://online.acb.com.vn/acbib/Request", Action: "https://online.acb.com.vn/acbib/Request", Fields: map[string]string{"dse_sessionId": "synthetic-session", "dse_processorState": "acctDetailPage", "dse_operationName": "ibkacctDetailProc", "AccountNbr": "222222222"}, Cookies: []authbrowser.Cookie{{Name: "session", Value: "synthetic-cookie", Domain: acb.OfficialHost, Path: "/", Secure: true}}}, []byte("synthetic-nonce"))
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
			client, err := acb.NewClient("https://online.acb.com.vn", verifierAccountTransport(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			verifier := NewSessionVerifier(NewSessionLoader(store, keyring, client), client)
			err = verifier.VerifySession(ctx, conn.ID, attempt.Generation, encoded)
			if tc.accepted && err != nil {
				t.Fatal(err)
			}
			if !tc.accepted && err == nil {
				t.Fatal("authenticated page for another/missing account was verified")
			}
			if err != nil && (strings.Contains(err.Error(), tc.account) && tc.account != "" || strings.Contains(err.Error(), "222222222") || strings.Contains(err.Error(), "synthetic-session")) {
				t.Fatal("verification error exposed private account or session")
			}
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
