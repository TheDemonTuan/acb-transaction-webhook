package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/config"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

const credentialTestOrigin = "https://bank.example.test"

func credentialHTTPFixture(t *testing.T) (*Server, *storage.Store, storage.Connection, string) {
	t.Helper()
	base, store := setupTestServerWithKeyring(t)
	cfg := base.cfg
	cfg.PublicOrigin = credentialTestOrigin
	srv := New(cfg, store)
	connection, err := store.ImportACBCredentials(context.Background(), storage.ACBCredentials{
		Username: "original-secret-user", Password: "original-secret-password", AccountNumber: "123456784321",
	})
	if err != nil {
		t.Fatal(err)
	}
	episode, err := store.EnsureAuthRecoveryEpisode(context.Background(), connection.ID, connection.Generation)
	if err != nil {
		t.Fatal(err)
	}
	action, err := store.CreateTelegramAuthAction(context.Background(), storage.TelegramAuthAction{
		BotID: 123, ChatID: 456, UserID: 789, EpisodeID: episode.ID,
		ExpectedGeneration: connection.Generation, Action: "UPDATE_CREDENTIALS",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeliverTelegramAuthAction(context.Background(), action.ID, 42); err != nil {
		t.Fatal(err)
	}
	action, disposition, err := store.ConsumeTelegramAuthAction(context.Background(), action.ID, 123, 456, 789, 42, time.Now())
	if err != nil || disposition != "CREDENTIAL_GRANT" || action.CredentialGrantToken == "" {
		t.Fatalf("grant disposition=%s err=%v", disposition, err)
	}
	return srv, store, connection, action.CredentialGrantToken
}

func credentialHTTPRequest(srv *Server, path string, body any) *http.Request {
	payload, _ := json.Marshal(body)
	csrf, cookie := getCSRF(srv)
	r := httptest.NewRequest(http.MethodPost, credentialTestOrigin+path, bytes.NewReader(payload))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", credentialTestOrigin)
	r.Header.Set("X-CSRF-Token", csrf)
	r.AddCookie(cookie)
	return r
}

func credentialHTTPPost(srv *Server, path string, body any) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, credentialHTTPRequest(srv, path, body))
	return w
}

func credentialSaveBody(grant string) map[string]any {
	return map[string]any{
		"grant": grant, "expectedRevision": 1, "username": " new-secret-user ",
		"password": " new-secret-password ", "passwordConfirmation": " new-secret-password ",
	}
}

func assertCredentialHTTPCode(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid error response: %v", err)
	}
	if response.Code != status || body.Code != code {
		t.Fatalf("status=%d code=%s want %d %s", response.Code, body.Code, status, code)
	}
}

func assertCredentialResponsePrivate(t *testing.T, response *httptest.ResponseRecorder, grant string) {
	t.Helper()
	for _, secret := range []string{grant, "original-secret-user", "original-secret-password", "new-secret-user", "new-secret-password", "123456784321"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatal("credential response exposed a secret")
		}
	}
	if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("credential response is missing privacy headers")
	}
}

func TestACBCredentialsHTTPGrantAndSave(t *testing.T) {
	srv, store, connection, grant := credentialHTTPFixture(t)
	// A link prefetch is not a grant validation or consumption.
	prefetch := httptest.NewRecorder()
	srv.Handler().ServeHTTP(prefetch, httptest.NewRequest(http.MethodGet, credentialTestOrigin+"/api/v1/connection/credentials/grant", nil))
	if prefetch.Code != http.StatusMethodNotAllowed {
		t.Fatalf("prefetch status=%d", prefetch.Code)
	}
	viewResponse := credentialHTTPPost(srv, "/api/v1/connection/credentials/grant", map[string]string{"grant": grant})
	if viewResponse.Code != http.StatusOK {
		t.Fatalf("grant status=%d", viewResponse.Code)
	}
	var view storage.ACBCredentialGrantView
	if err := json.Unmarshal(viewResponse.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Revision != 1 || !view.CanSave || view.AccountMasked == "" || view.UsernameMasked == "" {
		t.Fatalf("grant metadata invalid: revision=%d canSave=%t", view.Revision, view.CanSave)
	}
	assertCredentialResponsePrivate(t, viewResponse, grant)
	// Validating repeatedly does not consume the one-use save permission.
	if w := credentialHTTPPost(srv, "/api/v1/connection/credentials/grant", map[string]string{"grant": grant}); w.Code != http.StatusOK {
		t.Fatalf("second validation status=%d", w.Code)
	}
	saved := credentialHTTPPost(srv, "/api/v1/connection/credentials", credentialSaveBody(grant))
	if saved.Code != http.StatusOK {
		t.Fatalf("save status=%d", saved.Code)
	}
	var result struct {
		Saved         bool  `json:"saved"`
		Revision      int64 `json:"revision"`
		RequiresLogin bool  `json:"requiresLogin"`
	}
	if err := json.Unmarshal(saved.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Saved || result.Revision != 2 || !result.RequiresLogin {
		t.Fatalf("invalid save result: %+v", result)
	}
	assertCredentialResponsePrivate(t, saved, grant)
	credentials, err := store.ReadACBCredentials(context.Background(), connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	if credentials.Revision != 2 || credentials.Username != "new-secret-user" || credentials.Password != " new-secret-password " || credentials.AccountNumber != "123456784321" {
		t.Fatal("saved credential values do not preserve password spaces and immutable account")
	}
	current, err := store.Connection(context.Background())
	if err != nil || current.ID != connection.ID || current.State != "AUTH_REQUIRED" || current.Generation <= connection.Generation {
		t.Fatal("save must fence old state without starting login or changing connection")
	}
	for _, suffix := range []string{"", "-wal"} {
		raw, err := os.ReadFile(srv.cfg.DatabasePath + suffix)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"original-secret-user", "original-secret-password", "new-secret-user", "new-secret-password", "123456784321", grant} {
			if bytes.Contains(raw, []byte(secret)) {
				t.Fatal("database or WAL exposed plaintext credentials or grant")
			}
		}
	}
	assertCredentialHTTPCode(t, credentialHTTPPost(srv, "/api/v1/connection/credentials", credentialSaveBody(grant)), http.StatusGone, "CREDENTIAL_GRANT_EXPIRED")
}

func TestACBCredentialsHTTPSecurityBoundaries(t *testing.T) {
	for _, test := range []struct {
		name   string
		modify func(*http.Request)
		code   string
	}{
		{"missing CSRF", func(r *http.Request) { r.Header.Del("X-CSRF-Token") }, "CSRF_TOKEN_INVALID"},
		{"missing origin", func(r *http.Request) { r.Header.Del("Origin") }, "ORIGIN_MISMATCH"},
		{"host cannot override public origin", func(r *http.Request) {
			r.Host = "attacker.example.test"
			r.Header.Set("Origin", "https://attacker.example.test")
		}, "ORIGIN_MISMATCH"},
		{"origin path rejected", func(r *http.Request) { r.Header.Set("Origin", credentialTestOrigin+"/path") }, "ORIGIN_MISMATCH"},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv, store, connection, grant := credentialHTTPFixture(t)
			r := credentialHTTPRequest(srv, "/api/v1/connection/credentials/grant", map[string]string{"grant": grant})
			test.modify(r)
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, r)
			assertCredentialHTTPCode(t, w, http.StatusForbidden, test.code)
			assertCredentialResponsePrivate(t, w, grant)
			var owner *string
			if err := store.DB().QueryRow(`SELECT owner_subject FROM acb_credential_grants WHERE connection_id=?`, connection.ID).Scan(&owner); err != nil || owner != nil {
				t.Fatal("rejected request bound grant owner")
			}
		})
	}
	t.Run("Access required", func(t *testing.T) {
		srv, store, _, grant := credentialHTTPFixture(t)
		cfg := srv.cfg
		cfg.Production = true
		private := New(cfg, store)
		w := httptest.NewRecorder()
		private.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, credentialTestOrigin+"/api/v1/connection/credentials/grant", strings.NewReader(`{"grant":"`+grant+`"}`)))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("missing Access status=%d", w.Code)
		}
		assertCredentialResponsePrivate(t, w, grant)
	})
	t.Run("OWNER only", func(t *testing.T) {
		srv, store, _, grant := credentialHTTPFixture(t)
		cfg := srv.cfg
		cfg.DevelopmentSubject = "operator"
		cfg.Roles = config.RoleSubjects{Operators: map[string]struct{}{"operator": {}}}
		operator := New(cfg, store)
		w := credentialHTTPPost(operator, "/api/v1/connection/credentials/grant", map[string]string{"grant": grant})
		if w.Code != http.StatusForbidden {
			t.Fatalf("operator status=%d", w.Code)
		}
	})
	t.Run("owner binding", func(t *testing.T) {
		srv, store, _, grant := credentialHTTPFixture(t)
		if w := credentialHTTPPost(srv, "/api/v1/connection/credentials/grant", map[string]string{"grant": grant}); w.Code != http.StatusOK {
			t.Fatal("initial grant validation failed")
		}
		cfg := srv.cfg
		cfg.DevelopmentSubject = "another-owner"
		other := New(cfg, store)
		assertCredentialHTTPCode(t, credentialHTTPPost(other, "/api/v1/connection/credentials/grant", map[string]string{"grant": grant}), http.StatusGone, "CREDENTIAL_GRANT_EXPIRED")
		assertCredentialHTTPCode(t, credentialHTTPPost(other, "/api/v1/connection/credentials", credentialSaveBody(grant)), http.StatusGone, "CREDENTIAL_GRANT_EXPIRED")
		if w := credentialHTTPPost(srv, "/api/v1/connection/credentials", credentialSaveBody(grant)); w.Code != http.StatusOK {
			t.Fatalf("bound owner save status=%d", w.Code)
		}
	})
}

func TestACBCredentialsHTTPStrictInput(t *testing.T) {
	for _, test := range []struct{ name, body string }{
		{"unknown account", `{"accountNumber":"8765"}`},
		{"trailing JSON", `{} {}`},
		{"oversized body", `{"grant":"` + strings.Repeat("x", credentialBodyLimit) + `"}`},
		{"invalid JSON", `{"password":"secret"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv, _, _, _ := credentialHTTPFixture(t)
			r := credentialHTTPRequest(srv, "/api/v1/connection/credentials", nil)
			r.Body = io.NopCloser(strings.NewReader(test.body))
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, r)
			assertCredentialHTTPCode(t, w, http.StatusBadRequest, "INVALID_CREDENTIAL_INPUT")
		})
	}
	for _, test := range []struct {
		name, field string
		value       any
	}{
		{"empty username", "username", " \t "}, {"long username", "username", strings.Repeat("u", 257)},
		{"username newline", "username", "user\n"}, {"empty password", "password", ""},
		{"long password", "password", strings.Repeat("p", 1025)}, {"password NUL", "password", "p\x00"},
		{"password CR", "password", "p\r"}, {"wrong confirmation", "passwordConfirmation", "different"},
		{"invalid revision", "expectedRevision", 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv, store, connection, grant := credentialHTTPFixture(t)
			body := credentialSaveBody(grant)
			body[test.field] = test.value
			assertCredentialHTTPCode(t, credentialHTTPPost(srv, "/api/v1/connection/credentials", body), http.StatusBadRequest, "INVALID_CREDENTIAL_INPUT")
			credentials, err := store.ReadACBCredentials(context.Background(), connection.ID)
			if err != nil || credentials.Revision != 1 {
				t.Fatal("invalid input changed credentials")
			}
		})
	}
}

func TestACBCredentialsHTTPConcurrentOneUse(t *testing.T) {
	srv, store, connection, grant := credentialHTTPFixture(t)
	if w := credentialHTTPPost(srv, "/api/v1/connection/credentials/grant", map[string]string{"grant": grant}); w.Code != http.StatusOK {
		t.Fatal("grant binding failed")
	}
	requests := []*http.Request{
		credentialHTTPRequest(srv, "/api/v1/connection/credentials", credentialSaveBody(grant)),
		credentialHTTPRequest(srv, "/api/v1/connection/credentials", credentialSaveBody(grant)),
	}
	responses := []*httptest.ResponseRecorder{httptest.NewRecorder(), httptest.NewRecorder()}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range requests {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			srv.Handler().ServeHTTP(responses[i], requests[i])
		}(i)
	}
	close(start)
	wg.Wait()
	if responses[0].Code == http.StatusGone {
		responses[0], responses[1] = responses[1], responses[0]
	}
	if responses[0].Code != http.StatusOK {
		t.Fatalf("winning save status=%d", responses[0].Code)
	}
	assertCredentialHTTPCode(t, responses[1], http.StatusGone, "CREDENTIAL_GRANT_EXPIRED")
	credentials, err := store.ReadACBCredentials(context.Background(), connection.ID)
	if err != nil || credentials.Revision != 2 {
		t.Fatal("concurrent saves changed revision more than once")
	}
}

func TestACBCredentialsHTTPRollbackAndBusy(t *testing.T) {
	srv, store, connection, grant := credentialHTTPFixture(t)
	if w := credentialHTTPPost(srv, "/api/v1/connection/credentials/grant", map[string]string{"grant": grant}); w.Code != http.StatusOK {
		t.Fatal("grant binding failed")
	}
	if _, err := store.DB().Exec(`UPDATE connections SET state='MONITORING' WHERE id=?`, connection.ID); err != nil {
		t.Fatal(err)
	}
	busy := credentialHTTPPost(srv, "/api/v1/connection/credentials/grant", map[string]string{"grant": grant})
	var view storage.ACBCredentialGrantView
	if err := json.Unmarshal(busy.Body.Bytes(), &view); err != nil || busy.Code != http.StatusOK || view.CanSave || view.BlockedReason != "ACB_SESSION_BUSY" {
		t.Fatal("healthy session must provide blocked grant view")
	}
	assertCredentialHTTPCode(t, credentialHTTPPost(srv, "/api/v1/connection/credentials", credentialSaveBody(grant)), http.StatusConflict, "ACB_SESSION_BUSY")
	if _, err := store.DB().Exec(`UPDATE connections SET state='AUTH_REQUIRED' WHERE id=?`, connection.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`CREATE TRIGGER fail_credential_save BEFORE UPDATE OF config_revision ON connections BEGIN SELECT RAISE(ABORT,'new-secret-password'); END`); err != nil {
		t.Fatal(err)
	}
	failed := credentialHTTPPost(srv, "/api/v1/connection/credentials", credentialSaveBody(grant))
	assertCredentialHTTPCode(t, failed, http.StatusServiceUnavailable, "CREDENTIALS_UNAVAILABLE")
	assertCredentialResponsePrivate(t, failed, grant)
	credentials, err := store.ReadACBCredentials(context.Background(), connection.ID)
	if err != nil || credentials.Revision != 1 || credentials.Password != "original-secret-password" {
		t.Fatal("failed transaction lost original credentials")
	}
	var status string
	if err := store.DB().QueryRow(`SELECT status FROM acb_credential_grants WHERE connection_id=?`, connection.ID).Scan(&status); err != nil || status != "PENDING" {
		t.Fatal("failed transaction consumed grant")
	}
	if _, err := store.DB().Exec(`DROP TRIGGER fail_credential_save`); err != nil {
		t.Fatal(err)
	}
	if w := credentialHTTPPost(srv, "/api/v1/connection/credentials", credentialSaveBody(grant)); w.Code != http.StatusOK {
		t.Fatalf("save after recovery status=%d", w.Code)
	}
}

func TestACBCredentialsHTTPGrantExpiryRevisionRateAndFence(t *testing.T) {
	t.Run("expired", func(t *testing.T) {
		srv, store, _, grant := credentialHTTPFixture(t)
		if _, err := store.DB().Exec(`UPDATE acb_credential_grants SET expires_at=?`, time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
		assertCredentialHTTPCode(t, credentialHTTPPost(srv, "/api/v1/connection/credentials/grant", map[string]string{"grant": grant}), http.StatusGone, "CREDENTIAL_GRANT_EXPIRED")
	})
	t.Run("revision fence", func(t *testing.T) {
		srv, store, connection, grant := credentialHTTPFixture(t)
		if _, err := store.DB().Exec(`UPDATE connections SET generation=generation+1 WHERE id=?`, connection.ID); err != nil {
			t.Fatal(err)
		}
		assertCredentialHTTPCode(t, credentialHTTPPost(srv, "/api/v1/connection/credentials/grant", map[string]string{"grant": grant}), http.StatusConflict, "CREDENTIALS_REVISION_CONFLICT")
	})
	t.Run("owner validation limit", func(t *testing.T) {
		srv, store, connection, grant := credentialHTTPFixture(t)
		for i := range 10 {
			if w := credentialHTTPPost(srv, "/api/v1/connection/credentials/grant", map[string]string{"grant": grant}); w.Code != http.StatusOK {
				t.Fatalf("validation %d status=%d", i, w.Code)
			}
		}
		limited := credentialHTTPPost(srv, "/api/v1/connection/credentials/grant", map[string]string{"grant": grant})
		assertCredentialHTTPCode(t, limited, http.StatusTooManyRequests, "CREDENTIAL_GRANT_RATE_LIMITED")
		if limited.Header().Get("Retry-After") != "60" {
			t.Fatal("rate limited request has no retry guidance")
		}
		var status string
		if err := store.DB().QueryRow(`SELECT status FROM acb_credential_grants WHERE connection_id=?`, connection.ID).Scan(&status); err != nil || status != "PENDING" {
			t.Fatal("rate limit consumed grant")
		}
		if w := credentialHTTPPost(srv, "/api/v1/connection/credentials", credentialSaveBody(grant)); w.Code != http.StatusOK {
			t.Fatalf("save should not be blocked by validation limiter: %d", w.Code)
		}
	})
	t.Run("deployment fence", func(t *testing.T) {
		srv, store, _, grant := credentialHTTPFixture(t)
		if _, err := store.AcquireMutationGate(context.Background(), "credential-http-test", time.Minute, "fixture"); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{"/api/v1/connection/credentials/grant", "/api/v1/connection/credentials"} {
			w := credentialHTTPPost(srv, path, credentialSaveBody(grant))
			assertCredentialHTTPCode(t, w, http.StatusServiceUnavailable, "MUTATION_GATE_LOCKED")
		}
	})
}

func TestACBCredentialsHTTPBindingRevisionAndEncryptionFailure(t *testing.T) {
	srv, store, connection, grant := credentialHTTPFixture(t)
	// Possession of the Telegram grant is not sufficient before OWNER binding.
	assertCredentialHTTPCode(t, credentialHTTPPost(srv, "/api/v1/connection/credentials", credentialSaveBody(grant)), http.StatusGone, "CREDENTIAL_GRANT_EXPIRED")
	if w := credentialHTTPPost(srv, "/api/v1/connection/credentials/grant", map[string]string{"grant": grant}); w.Code != http.StatusOK {
		t.Fatal("grant binding failed")
	}
	wrongRevision := credentialSaveBody(grant)
	wrongRevision["expectedRevision"] = 2
	assertCredentialHTTPCode(t, credentialHTTPPost(srv, "/api/v1/connection/credentials", wrongRevision), http.StatusConflict, "CREDENTIALS_REVISION_CONFLICT")
	store.WithKeyring(nil)
	failed := credentialHTTPPost(srv, "/api/v1/connection/credentials", credentialSaveBody(grant))
	store.WithKeyring(srv.keyring)
	assertCredentialHTTPCode(t, failed, http.StatusServiceUnavailable, "CREDENTIALS_UNAVAILABLE")
	assertCredentialResponsePrivate(t, failed, grant)
	credentials, err := store.ReadACBCredentials(context.Background(), connection.ID)
	if err != nil || credentials.Revision != 1 || credentials.Password != "original-secret-password" {
		t.Fatal("encryption unavailable lost original credentials")
	}
	if w := credentialHTTPPost(srv, "/api/v1/connection/credentials", credentialSaveBody(grant)); w.Code != http.StatusOK {
		t.Fatalf("grant must survive encryption failure: %d", w.Code)
	}
}
