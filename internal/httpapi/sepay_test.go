package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/config"
	"github.com/thedemontuan/acb-transaction-webhook/internal/sepay"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func TestPublicSePayStoreConfig(t *testing.T) {
	fixture := sepay.Config{
		Mode: sepay.ModeActive, StoreKey: "private-store-key", StoreName: "Public Store",
		BankCode: "TESTBANK", BankName: "Private Template Bank Name",
		AccountNumber: "VA123456", AccountName: "PUBLIC RECEIVER",
		NotificationAccountNumber: "2210112002",
		QRPayload:                 "0002010102116304ABCD", BotID: 900001,
		ChatID: -100900003, SenderBotID: 900002, TopicID: 42,
		WebhookSecret: base64.RawURLEncoding.EncodeToString([]byte("01234567890123456789012345678901")),
		ActivationAt:  time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC),
	}
	for _, tc := range []struct{ mode, status string }{
		{sepay.ModeDisabled, "DISABLED"}, {sepay.ModeObserve, "OBSERVING"}, {sepay.ModeActive, "ACTIVE"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			cfg := fixture
			cfg.Mode = tc.mode
			// No store or payOS service: receiver configuration must not depend on
			// database connectivity or payment-provider readiness.
			server := New(config.Config{Production: true}, nil).WithSePay(sepay.NewService(cfg, nil, nil))
			w := httptest.NewRecorder()
			server.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/public/v1/sepay-store", nil))
			if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("public receiver unavailable or cached: %d", w.Code)
			}
			var got map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			want := map[string]any{
				"provider": "SEPAY", "status": tc.status, "storeName": "", "bank": "",
				"accountNumber": "", "accountName": "", "qrPayload": "", "lastMessageAt": nil,
			}
			if tc.mode == sepay.ModeActive {
				want["storeName"] = fixture.StoreName
				want["bank"] = fixture.BankCode
				want["accountNumber"] = fixture.AccountNumber
				want["accountName"] = fixture.AccountName
				want["qrPayload"] = fixture.QRPayload
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("DTO fields do not match public contract: got=%v want=%v", got, want)
			}
			for _, private := range []string{fixture.StoreKey, fixture.BankName, fixture.WebhookSecret, fixture.NotificationAccountNumber, "notificationAccountNumber", "900001", "900002", "-100900003", "activationAt", "topicId"} {
				if strings.Contains(w.Body.String(), private) {
					t.Fatal("public response leaked private configuration")
				}
			}
		})
	}
}

func TestPublicSePayStoreDefaultsDisabled(t *testing.T) {
	server := New(config.Config{}, nil)
	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/public/v1/sepay-store", nil))
	var got SePayStoreConfigResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || got.Provider != "SEPAY" || got.Status != "DISABLED" || got.QRPayload != "" || got.LastMessageAt != nil {
		t.Fatal("unconfigured server did not expose disabled Store")
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		w := httptest.NewRecorder()
		server.Handler().ServeHTTP(w, httptest.NewRequest(method, "/api/public/v1/sepay-store", nil))
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("public config accepted mutation %s: %d", method, w.Code)
		}
	}
}

func TestSePayConfigurationDoesNotChangePayOS(t *testing.T) {
	f := newPaymentHTTPFixture(t)
	f.handler = New(f.cfg, f.store).WithPayments(f.service).WithSePay(sepay.NewService(sepay.Config{Mode: sepay.ModeObserve}, f.store, nil)).Handler()
	w := paymentRequest(f.handler, http.MethodGet, "/api/public/v1/payment-config", "", "", false)
	if w.Code != http.StatusOK {
		t.Fatalf("payOS config unavailable: %d", w.Code)
	}
	var response map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["provider"] != "PAYOS" || response["ready"] != true {
		t.Fatalf("Store config changed payOS readiness: %v", response)
	}
	w = paymentRequest(f.handler, http.MethodPost, "/api/public/v1/payments", `{"amountVnd":50000,"origin":"STATIC_URL"}`, paymentKey(1), false)
	if w.Code != http.StatusCreated {
		t.Fatalf("payOS create changed with observing Store: %d", w.Code)
	}
	order := decodePaymentOrder(t, w)
	if order.AmountVnd != 50000 || order.Status != "PENDING" {
		t.Fatal("payOS payment contract changed")
	}
}

func TestSePayReviewsAndStatusPermissionsPrivacyAndPagination(t *testing.T) {
	f := newPaymentHTTPFixture(t)
	sepayWebhookKeyring(t, f.store)
	cfg := sepayWebhookConfig()
	service := sepay.NewService(cfg, f.store, nil)
	roleHandler := func(subject string) http.Handler {
		appConfig := f.cfg
		appConfig.DevelopmentSubject = subject
		return New(appConfig, f.store).WithPayments(f.service).WithSePay(service).Handler()
	}
	owner := roleHandler("owner")
	accepted := sepayWebhookEnvelope(t, 1, 10, "accepted-reference")
	if w := sepayWebhookPost(owner, cfg.WebhookSecret, sepayWebhookBody(t, accepted)); w.Code != 200 {
		t.Fatalf("fixture accepted webhook: %d %s", w.Code, w.Body.String())
	}
	for i := range 3 {
		invalid := sepayWebhookEnvelope(t, int64(i+2), int64(i+20), "invalid-reference")
		invalid["message"].(map[string]any)["text"] = "private malformed payer account"
		if w := sepayWebhookPost(owner, cfg.WebhookSecret, sepayWebhookBody(t, invalid)); w.Code != 200 {
			t.Fatalf("fixture review webhook: %d %s", w.Code, w.Body.String())
		}
	}
	last, err := f.store.LastSePayMessageAt(context.Background(), cfg.StoreKey)
	if err != nil || last == nil {
		t.Fatalf("missing valid last timestamp: %v", err)
	}
	for _, role := range []string{"owner", "operator"} {
		handler := roleHandler(role)
		w := paymentRequest(handler, "GET", "/api/v1/sepay-reviews?limit=2", "", "", false)
		var page storage.Page[storage.SePayReview]
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || len(page.Items) != 2 || page.NextCursor == "" {
			t.Fatalf("%s review pagination: %d %s", role, w.Code, w.Body.String())
		}
		var projection struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &projection); err != nil {
			t.Fatal(err)
		}
		for _, item := range projection.Items {
			if len(item) != 4 || item["storeKey"] != cfg.StoreKey || item["reason"] != "INVALID_TEMPLATE" || item["receivedAt"] == nil {
				t.Fatalf("unsafe projection: %v", item)
			}
			if _, ok := item["messageId"].(string); !ok {
				t.Fatal("message ID not a lossless string")
			}
		}
		next := paymentRequest(handler, "GET", "/api/v1/sepay-reviews?limit=2&cursor="+page.NextCursor, "", "", false)
		var tail storage.Page[storage.SePayReview]
		if err := json.Unmarshal(next.Body.Bytes(), &tail); err != nil || next.Code != 200 || len(tail.Items) != 1 || tail.NextCursor != "" {
			t.Fatalf("tail failed: %d %s err=%v", next.Code, next.Body.String(), err)
		}
		if tail.Items[0].MessageID == page.Items[0].MessageID || tail.Items[0].MessageID == page.Items[1].MessageID {
			t.Fatal("cursor repeated a review")
		}
		for _, path := range []string{"/api/v1/sepay-reviews?cursor=bad", "/api/v1/sepay-reviews?limit=101", "/api/v1/sepay-reviews?limit=0"} {
			if got := paymentRequest(handler, "GET", path, "", "", false); got.Code != 400 {
				t.Fatalf("invalid pagination accepted: %s %d", path, got.Code)
			}
		}
	}
	for _, role := range []string{"owner", "operator", "viewer"} {
		w := paymentRequest(roleHandler(role), "GET", "/api/v1/status", "", "", false)
		var status struct {
			UserRole string              `json:"userRole"`
			SePay    SePayStatusResponse `json:"sepay"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil || w.Code != 200 || status.UserRole != strings.ToUpper(role) || status.SePay.Mode != "active" || status.SePay.ReviewCount != 3 || status.SePay.LastMessageAt == nil || !status.SePay.LastMessageAt.Equal(*last) {
			t.Fatalf("status mismatch: %d %s err=%v", w.Code, w.Body.String(), err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &fields); err != nil {
			t.Fatal(err)
		}
		var sepayFields map[string]any
		if err := json.Unmarshal(fields["sepay"], &sepayFields); err != nil || len(sepayFields) != 3 {
			t.Fatalf("unsafe status projection: %v err=%v", sepayFields, err)
		}
		for _, private := range []string{cfg.WebhookSecret, cfg.QRPayload, cfg.AccountNumber, cfg.StoreKey, "private malformed payer", "private payer memo", "accepted-reference"} {
			if strings.Contains(w.Body.String(), private) {
				t.Fatalf("status leaked private information for %s", role)
			}
		}
	}
	if w := paymentRequest(roleHandler("viewer"), "GET", "/api/v1/sepay-reviews", "", "", false); w.Code != 403 {
		t.Fatalf("viewer read reviews: %d", w.Code)
	}
	public := New(config.Config{Production: true}, f.store).WithSePay(service).Handler()
	for _, path := range []string{"/api/v1/sepay-reviews", "/api/v1/status"} {
		if w := paymentRequest(public, "GET", path, "", "", false); w.Code != 401 && w.Code != 403 {
			t.Fatalf("unauthenticated access to %s: %d", path, w.Code)
		}
	}
	if w := paymentRequest(public, "GET", "/api/public/v1/sepay-reviews", "", "", false); w.Code != 404 {
		t.Fatalf("public reviews route exposed: %d", w.Code)
	}
	for _, method := range []string{"POST", "PUT", "DELETE"} {
		if w := paymentRequest(owner, method, "/api/v1/sepay-reviews", `{}`, "", true); w.Code != 405 {
			t.Fatalf("reviews accepted mutation %s: %d", method, w.Code)
		}
	}
}

func TestSePayStatusModesAndHistoricalReviewCount(t *testing.T) {
	f := newPaymentHTTPFixture(t)
	if _, err := f.store.DB().Exec(`INSERT INTO sepay_telegram_inbox(bot_id,update_id,chat_id,message_id,payload_hash,payload_envelope,store_key,reason,received_at) VALUES(900001,1,-100900003,10,'private-hash',?,'historical-store','REFERENCE_CONFLICT','2026-10-10T12:00:00Z')`, []byte("encrypted historical evidence")); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{sepay.ModeDisabled, sepay.ModeObserve, sepay.ModeActive} {
		cfg := sepayWebhookConfig()
		cfg.Mode = mode
		handler := New(f.cfg, f.store).WithPayments(f.service).WithSePay(sepay.NewService(cfg, f.store, nil)).Handler()
		w := paymentRequest(handler, "GET", "/api/v1/status", "", "", false)
		var response struct {
			SePay SePayStatusResponse `json:"sepay"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || w.Code != 200 || response.SePay.Mode != mode || response.SePay.LastMessageAt != nil || response.SePay.ReviewCount != 1 {
			t.Fatalf("mode %s status: %d %s err=%v", mode, w.Code, w.Body.String(), err)
		}
	}
	w := paymentRequest(f.handler, "GET", "/api/v1/status", "", "", false)
	var response struct {
		SePay SePayStatusResponse `json:"sepay"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || w.Code != 200 || response.SePay.Mode != "disabled" {
		t.Fatalf("missing service status: %d %s err=%v", w.Code, w.Body.String(), err)
	}
}

func TestSePayReviewsStorageFailureIsNotInvalidCursor(t *testing.T) {
	f := newPaymentHTTPFixture(t)
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	// A syntactically valid cursor must not mask an outage as a caller error.
	cursor := base64.RawURLEncoding.EncodeToString([]byte("2026-10-10T12:00:00Z\x00900001:1"))
	for _, path := range []string{"/api/v1/sepay-reviews", "/api/v1/sepay-reviews?cursor=" + cursor, "/api/v1/status"} {
		w := paymentRequest(f.handler, "GET", path, "", "", false)
		if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "storage_error") {
			t.Fatalf("database failure misclassified: %s %d %s", path, w.Code, w.Body.String())
		}
	}
}
