package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/sepay"
)

func TestSePayAdminOwnerCSRFAndRedactedDraft(t *testing.T) {
	f := managedHTTPFixture(t)
	draft := `{"revision":0,"config":{"mode":"disabled"},"botToken":"900001:TEST_token_abcdefghijklmnopqrstuvwxyz"}`
	for _, role := range []string{"operator", "viewer"} {
		for _, entry := range []struct{ method, path string }{{"GET", "/api/v1/sepay-store/config"}, {"PUT", "/api/v1/sepay-store/config"}, {"GET", "/api/v1/sepay-store/telegram/status"}, {"POST", "/api/v1/sepay-store/telegram/register"}} {
			w := paymentRequest(f.roleHandler(role), entry.method, entry.path, draft, "", true)
			if w.Code != http.StatusForbidden {
				t.Fatalf("%s accessed %s: %d", role, entry.path, w.Code)
			}
		}
	}
	for _, path := range []string{"/api/v1/sepay-store/config", "/api/v1/sepay-store/telegram/register"} {
		method := "PUT"
		if strings.HasSuffix(path, "register") {
			method = "POST"
		}
		if w := paymentRequest(f.handler, method, path, draft, "", false); w.Code != http.StatusForbidden {
			t.Fatal("admin mutation bypassed CSRF")
		}
	}
	w := paymentRequest(f.handler, "PUT", "/api/v1/sepay-store/config", draft, "", true)
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("draft save: %d %s", w.Code, w.Body.String())
	}
	var result sepay.AdminSnapshot
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Revision != 1 || !result.HasBotToken || !result.HasWebhookSecret || result.Config.Mode != "disabled" {
		t.Fatal("draft response incorrect")
	}
	for _, request := range []struct{ method, path string }{{"GET", "/api/v1/sepay-store/config"}, {"GET", "/api/public/v1/sepay-store"}, {"GET", "/api/v1/status"}} {
		response := paymentRequest(f.handler, request.method, request.path, "", "", false)
		if response.Code != http.StatusOK {
			t.Fatalf("read failed: %d", response.Code)
		}
		if strings.Contains(response.Body.String(), "TEST_token") || strings.Contains(response.Body.String(), `"webhookSecret"`) || strings.Contains(response.Body.String(), `"botToken"`) {
			t.Fatal("public/admin/status exposed a secret")
		}
	}
	if stale := paymentRequest(f.handler, "PUT", "/api/v1/sepay-store/config", draft, "", true); stale.Code != http.StatusConflict || !strings.Contains(stale.Body.String(), "SEPAY_REVISION_CONFLICT") {
		t.Fatal("stale admin save did not demand refresh")
	}
}

func TestSePayAdminStrictBodyAndMissingRealCredentials(t *testing.T) {
	f := managedHTTPFixture(t)
	for _, body := range []string{
		`{"revision":0,"revision":0,"config":{"mode":"disabled"},"botToken":""}`,
		`{"revision":0,"config":{"mode":"disabled","webhookSecret":"browser-secret"},"botToken":""}`,
		`{"revision":0,"config":{"mode":"disabled","botId":900001},"botToken":""}`,
		`{"revision":null,"config":{"mode":"disabled"},"botToken":""}`,
		`{"revision":0,"config":{"mode":"disabled"},"botToken":null}`,
		`{"revision":0,"config":{"mode":"disabled"},"botToken":""} {}`,
		`{"revision":0,"config":{"mode":"active"},"botToken":""}`,
	} {
		if w := paymentRequest(f.handler, "PUT", "/api/v1/sepay-store/config", body, "", true); w.Code != http.StatusBadRequest {
			t.Fatalf("invalid body accepted: %d", w.Code)
		}
	}
	for _, entry := range []struct{ method, path string }{{"GET", "/api/v1/sepay-store/telegram/status"}, {"POST", "/api/v1/sepay-store/telegram/register"}} {
		w := paymentRequest(f.handler, entry.method, entry.path, "{}", "", true)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "TELEGRAM_TOKEN_REQUIRED") {
			t.Fatalf("missing token pretended registration works: %d %s", w.Code, w.Body.String())
		}
	}
}

func TestSePayManagedPublicUpdatesPauseAndDatabaseFailure(t *testing.T) {
	f := managedHTTPFixture(t)
	first := New(f.cfg, f.store).WithPayments(f.service)
	second := New(f.cfg, f.store).WithPayments(f.service)
	fields := sepay.AdminFields{Mode: "active", StoreKey: "managed-store", StoreName: "Managed Store", BankCode: "VCB", BankName: "Fixture Bank", AccountNumber: "VA012345", AccountName: "TEST RECEIVER", QRPayload: "0002010102116304ABCD", BotID: "9007199254740993", ChatID: "-100900003", SenderBotID: "900002", TopicID: "0", ReceiverVerified: true, SourceSeparated: true}
	body, _ := json.Marshal(map[string]any{"revision": 0, "config": fields, "botToken": ""})
	w := paymentRequest(first.Handler(), "PUT", "/api/v1/sepay-store/config", string(body), "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("active save: %d %s", w.Code, w.Body.String())
	}
	for _, handler := range []http.Handler{first.Handler(), second.Handler()} {
		w := paymentRequest(handler, "GET", "/api/public/v1/sepay-store", "", "", false)
		var public SePayStoreConfigResponse
		if err := json.Unmarshal(w.Body.Bytes(), &public); err != nil {
			t.Fatal(err)
		}
		if w.Code != http.StatusOK || public.Status != "ACTIVE" || public.QRPayload != fields.QRPayload {
			t.Fatal("another runtime reader did not observe config immediately")
		}
	}
	w = paymentRequest(second.Handler(), "PUT", "/api/v1/sepay-store/config", `{"revision":1,"config":{"mode":"disabled"},"botToken":""}`, "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("pause: %d %s", w.Code, w.Body.String())
	}
	var saved sepay.AdminSnapshot
	if json.Unmarshal(w.Body.Bytes(), &saved) != nil || saved.Config.AccountNumber != fields.AccountNumber || saved.Config.BotID != fields.BotID {
		t.Fatal("mode-only pause discarded draft/IDs")
	}
	w = paymentRequest(first.Handler(), "GET", "/api/public/v1/sepay-store", "", "", false)
	if !strings.Contains(w.Body.String(), `"status":"DISABLED"`) || strings.Contains(w.Body.String(), fields.AccountNumber) {
		t.Fatal("pause exposed receiver or runtime ignored new revision")
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/public/v1/sepay-store", "/api/v1/sepay-store/config", "/api/v1/status"} {
		w := paymentRequest(first.Handler(), "GET", path, "", "", false)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("DB failure fell back to stale config: %s %d", path, w.Code)
		}
	}
}

func TestSePayConfigSaveRespectsDeploymentGate(t *testing.T) {
	f := managedHTTPFixture(t)
	if _, err := f.store.AcquireMutationGate(context.Background(), "sepay-admin-test", time.Minute, "test"); err != nil {
		t.Fatal(err)
	}
	w := paymentRequest(f.handler, "PUT", "/api/v1/sepay-store/config", `{"revision":0,"config":{"mode":"disabled"},"botToken":""}`, "", true)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatal("admin save bypassed deployment gate")
	}
}
