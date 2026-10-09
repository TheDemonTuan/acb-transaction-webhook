package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	payos "github.com/payOSHQ/payos-lib-golang/v2"
	"github.com/thedemontuan/acb-transaction-webhook/internal/config"
	"github.com/thedemontuan/acb-transaction-webhook/internal/payments"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

const paymentTestOrigin = "https://transactions.example.test"

type paymentHTTPFixture struct {
	store             *storage.Store
	service           *payments.Service
	cfg               config.Config
	adapter           *payments.PayOSAdapter
	createHTTPStatus  atomic.Int64
	confirmHTTPStatus atomic.Int64
	creates           atomic.Int64
	confirms          atomic.Int64
	confirmedURL      string
	mu                sync.Mutex
	handler           http.Handler
}

func newPaymentHTTPFixture(t *testing.T) *paymentHTTPFixture {
	t.Helper()
	f := &paymentHTTPFixture{}
	var err error
	f.store, err = storage.Open(context.Background(), filepath.Join(t.TempDir(), "payments.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.store.Close() })
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data map[string]any
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v2/payment-requests":
			f.creates.Add(1)
			if status := f.createHTTPStatus.Load(); status != 0 {
				w.WriteHeader(int(status))
				_ = json.NewEncoder(w).Encode(map[string]any{"code": "UNVERIFIED_PROVIDER_CODE", "desc": "http-contract-api-secret raw rejection"})
				return
			}
			var req payos.CreatePaymentLinkRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			if !strings.HasPrefix(req.ReturnUrl, paymentTestOrigin+"/pay/") || !validPaymentCapability(strings.TrimPrefix(req.ReturnUrl, paymentTestOrigin+"/pay/")) || req.CancelUrl != req.ReturnUrl {
				t.Errorf("invalid provider callbacks")
			}
			link := fmt.Sprintf("link-%d", req.OrderCode)
			data = map[string]any{"orderCode": req.OrderCode, "amount": req.Amount, "description": req.Description, "currency": "VND", "paymentLinkId": link, "status": "PENDING", "qrCode": "provider-original-qr", "accountNumber": fmt.Sprintf("VA-%d", req.OrderCode), "accountName": "SHOP", "bin": "970452", "checkoutUrl": "https://pay.payos.vn/web/" + link}
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cancel"):
			code := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v2/payment-requests/"), "/cancel")
			var orderCode int64
			_, _ = fmt.Sscan(code, &orderCode)
			order, err := f.store.PaymentOrderByCode(context.Background(), orderCode)
			if err != nil {
				t.Error(err)
				w.WriteHeader(404)
				return
			}
			data = map[string]any{"id": order.PaymentLinkID, "orderCode": order.OrderCode, "amount": order.AmountVnd, "amountPaid": 0, "amountRemaining": order.AmountVnd, "status": "CANCELLED", "createdAt": order.CreatedAt, "transactions": []any{}}
		case r.Method == http.MethodPost && r.URL.Path == "/confirm-webhook":
			f.confirms.Add(1)
			if status := f.confirmHTTPStatus.Load(); status != 0 {
				w.WriteHeader(int(status))
				_ = json.NewEncoder(w).Encode(map[string]any{"code": "UNVERIFIED_PROVIDER_CODE", "desc": "http-contract-api-secret raw rejection"})
				return
			}
			var req map[string]string
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
				return
			}
			f.mu.Lock()
			f.confirmedURL = req["webhookUrl"]
			f.mu.Unlock()
			if got := webhookTestPost(f.handler, webhookTestBody(t, webhookTestSample())); got.Code != 200 {
				t.Errorf("confirmation sample rejected: %d %s", got.Code, got.Body.String())
			}
			data = map[string]any{"webhookUrl": req["webhookUrl"], "accountNumber": "secret-confirm-account"}
		default:
			t.Errorf("unexpected SDK request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": "00", "data": data, "signature": webhookTestSignature(t, data)})
	}))
	t.Cleanup(provider.Close)
	adapter, err := payments.NewPayOS("http-contract-channel-secret", "http-contract-api-secret", webhookTestChecksum, payments.WithBaseURL(provider.URL), payments.WithHTTPClient(provider.Client()))
	if err != nil {
		t.Fatal(err)
	}
	f.adapter = adapter
	f.cfg = config.Config{DevelopmentSubject: "owner", PublicOrigin: paymentTestOrigin, Timezone: time.UTC, PayOSClientID: "http-contract-channel-secret", PayOSAPIKey: "http-contract-api-secret", PayOSChecksumKey: webhookTestChecksum, PaymentPublicOrigin: paymentTestOrigin, PaymentsEnabled: true, PayOSWebhookConfirmed: true, PaymentMaxAmountVND: 500000000, Roles: config.RoleSubjects{Owners: map[string]struct{}{"owner": {}}, Operators: map[string]struct{}{"operator": {}}, Viewers: map[string]struct{}{"viewer": {}}}}
	f.service = payments.NewService(f.cfg, f.store, adapter, nil)
	f.handler = f.roleHandler("owner")
	return f
}

func (f *paymentHTTPFixture) roleHandler(subject string) http.Handler {
	cfg := f.cfg
	cfg.DevelopmentSubject = subject
	return New(cfg, f.store).WithPayments(f.service).Handler()
}

func paymentRequest(handler http.Handler, method, path, body, key string, csrf bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, paymentTestOrigin+path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Origin", paymentTestOrigin)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Idempotency-Key", key)
	if csrf {
		r.AddCookie(&http.Cookie{Name: "tbg_csrf", Value: "payment-test-token"})
		r.Header.Set("X-CSRF-Token", "payment-test-token")
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func paymentKey(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }

func decodePaymentOrder(t *testing.T, w *httptest.ResponseRecorder) storage.PaymentOrder {
	t.Helper()
	var order storage.PaymentOrder
	if err := json.Unmarshal(w.Body.Bytes(), &order); err != nil {
		t.Fatal(err)
	}
	if order.ID == "" || !validPaymentCapability(order.ID) {
		t.Fatalf("missing order capability: %s", w.Body.String())
	}
	for _, secret := range []string{"http-contract-channel-secret", "http-contract-api-secret", webhookTestChecksum, "paymentLinkId", "operationToken", "idempotencyKey", "requestHash"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("internal metadata leaked: %s", secret)
		}
	}
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("missing capability response protection")
	}
	return order
}

func TestPaymentPublicCreateReplayAndCapabilities(t *testing.T) {
	f := newPaymentHTTPFixture(t)
	w := paymentRequest(f.handler, "POST", "/api/public/v1/payments", `{"amountVnd":50000,"origin":"STATIC_URL"}`, paymentKey(1), false)
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	order := decodePaymentOrder(t, w)
	for range 10 {
		w = paymentRequest(f.handler, "POST", "/api/public/v1/payments", `{"amountVnd":50000,"origin":"STATIC_URL"}`, paymentKey(1), false)
		if w.Code != 200 || decodePaymentOrder(t, w).ID != order.ID {
			t.Fatal("retry changed intent")
		}
	}
	if f.creates.Load() != 1 {
		t.Fatal("retry called provider")
	}
	w = paymentRequest(f.handler, "POST", "/api/public/v1/payments", `{"amountVnd":50001,"origin":"STATIC_URL"}`, paymentKey(1), false)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "IDEMPOTENCY_CONFLICT") {
		t.Fatal("conflict accepted")
	}
	w = paymentRequest(f.handler, "GET", "/api/public/v1/payments/"+order.ID, "", "", false)
	if w.Code != 200 || decodePaymentOrder(t, w).ID != order.ID {
		t.Fatal("capability unreadable")
	}
	for _, path := range []string{"/api/public/v1/payments", fmt.Sprintf("/api/public/v1/payments/%d", order.OrderCode), "/api/public/v1/payments/AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"} {
		if got := paymentRequest(f.handler, "GET", path, "", "", false); got.Code != 404 && got.Code != 405 {
			t.Fatalf("public lookup/list exposed: %s %d", path, got.Code)
		}
	}
	for _, path := range []string{"/api/public/v1/payments/" + order.ID + "/cancel", "/api/public/v1/payment-provider/confirm-webhook", "/api/public/v1/payment-reviews"} {
		if got := paymentRequest(f.handler, "POST", path, "{}", "", false); got.Code != 404 && got.Code != 405 {
			t.Fatalf("anonymous mutation exposed: %s", path)
		}
	}
}

func TestPaymentPublicStrictRequestSafety(t *testing.T) {
	f := newPaymentHTTPFixture(t)
	for _, body := range []string{`{"amountVnd":0,"origin":"STATIC_URL"}`, `{"amountVnd":-1,"origin":"STATIC_URL"}`, `{"amountVnd":500000001,"origin":"STATIC_URL"}`, `{"amountVnd":1.5,"origin":"STATIC_URL"}`, `{"amountVnd":1.0,"origin":"STATIC_URL"}`, `{"amountVnd":"50","origin":"STATIC_URL"}`, `{"amountVnd":null,"origin":"STATIC_URL"}`, `{"origin":"STATIC_URL"}`, `{"amountVnd":50,"origin":"BAD"}`, `{"amountVnd":50,"origin":"STATIC_URL","orderCode":123}`, `{"amountVnd":50,"amountVnd":50,"origin":"STATIC_URL"}`, `{"amountVnd":50,"origin":"STATIC_URL"} {}`, `[]`} {
		if got := paymentRequest(f.handler, "POST", "/api/public/v1/payments", body, paymentKey(1), false); got.Code != 400 {
			t.Fatalf("accepted %s: %d %s", body, got.Code, got.Body.String())
		}
	}
	for _, key := range []string{"", "123", "00000000-0000-4000-0000-000000000001"} {
		if got := paymentRequest(f.handler, "POST", "/api/public/v1/payments", `{"amountVnd":50,"origin":"STATIC_URL"}`, key, false); got.Code != 400 || !strings.Contains(got.Body.String(), "INVALID_IDEMPOTENCY_KEY") {
			t.Fatal("invalid key accepted")
		}
	}
	for _, origin := range []string{"", "https://evil.example", paymentTestOrigin + "/", paymentTestOrigin + ".evil.example"} {
		r := httptest.NewRequest("POST", paymentTestOrigin+"/api/public/v1/payments", strings.NewReader(`{"amountVnd":50,"origin":"STATIC_URL"}`))
		r.Header.Set("Origin", origin)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", paymentKey(1))
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		if w.Code != 403 || w.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatal("cross-origin request allowed")
		}
	}
	r := httptest.NewRequest("POST", paymentTestOrigin+"/api/public/v1/payments", strings.NewReader(`{"amountVnd":50,"origin":"STATIC_URL"}`))
	r.Header.Set("Origin", paymentTestOrigin)
	r.Header.Set("Content-Type", "text/plain")
	r.Header.Set("Idempotency-Key", paymentKey(1))
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatal("nonJSON accepted")
	}
	w = paymentRequest(f.handler, "POST", "/api/public/v1/payments", `{"amountVnd":50,"origin":"STATIC_URL","x":"`+strings.Repeat("x", 1024)+`"}`, paymentKey(1), false)
	if w.Code != 413 {
		t.Fatalf("body cap: %d", w.Code)
	}
	if f.creates.Load() != 0 {
		t.Fatal("invalid input reached SDK")
	}
}

func TestPaymentQuotaIgnoresReplayAndSpoofedHeaders(t *testing.T) {
	f := newPaymentHTTPFixture(t)
	var first storage.PaymentOrder
	for i := 1; i <= 7; i++ {
		r := httptest.NewRequest("POST", paymentTestOrigin+"/api/public/v1/payments", strings.NewReader(`{"amountVnd":50,"origin":"STATIC_URL"}`))
		r.RemoteAddr = "127.0.0.1:1234"
		r.Header.Set("Origin", paymentTestOrigin)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", paymentKey(i))
		r.Header.Set("X-Forwarded-For", fmt.Sprintf("203.0.113.%d", i))
		r.Header.Set("CF-Connecting-IP", fmt.Sprintf("203.0.113.%d", i))
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		if i <= 6 {
			if w.Code != 201 {
				t.Fatalf("quota early rejection: %d", w.Code)
			}
			if i == 1 {
				first = decodePaymentOrder(t, w)
			}
		} else if w.Code != 429 {
			t.Fatalf("quota bypass: %d", w.Code)
		}
	}
	w := paymentRequest(f.handler, "POST", "/api/public/v1/payments", `{"amountVnd":50,"origin":"STATIC_URL"}`, paymentKey(1), false)
	if w.Code != 200 || decodePaymentOrder(t, w).ID != first.ID || f.creates.Load() != 6 {
		t.Fatal("replay consumed quota")
	}
	for i := range 61 {
		w = paymentRequest(f.handler, "GET", "/api/public/v1/payments/"+first.ID, "", "", false)
		if i < 60 && w.Code != 200 {
			t.Fatal("GET quota early rejection")
		}
	}
	if w.Code != 429 {
		t.Fatal("GET quota not enforced")
	}
	limiter := newIPRateLimiter()
	now := time.Now()
	limiter.allow("expired", 1, time.Minute, now.Add(-2*time.Minute))
	limiter.allow("new", 1, time.Minute, now)
	if _, exists := limiter.history["expired"]; exists {
		t.Fatal("idle IP was not pruned")
	}
}

func TestPaymentConcurrentRetriesUseOneQuotaAndProviderIntent(t *testing.T) {
	f := newPaymentHTTPFixture(t)
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := paymentRequest(f.handler, "POST", "/api/public/v1/payments", `{"amountVnd":50,"origin":"STATIC_URL"}`, paymentKey(1), false)
			if w.Code != 201 && w.Code != 200 {
				t.Errorf("retry rejected %d", w.Code)
			}
		}()
	}
	wg.Wait()
	if f.creates.Load() != 1 {
		t.Fatal("concurrent retry called SDK again")
	}
	for i := 2; i <= 6; i++ {
		if w := paymentRequest(f.handler, "POST", "/api/public/v1/payments", `{"amountVnd":50,"origin":"STATIC_URL"}`, paymentKey(i), false); w.Code != 201 {
			t.Fatalf("retry consumed quota: %d", w.Code)
		}
	}
}

func TestPaymentAdminRolesCSRFAndCancellation(t *testing.T) {
	f := newPaymentHTTPFixture(t)
	created := paymentRequest(f.handler, "POST", "/api/v1/payments", `{"amountVnd":50}`, paymentKey(1), true)
	if created.Code != 201 {
		t.Fatalf("admin create %d %s", created.Code, created.Body.String())
	}
	order := decodePaymentOrder(t, created)
	if order.Origin != "OPERATOR_DYNAMIC" {
		t.Fatal("admin origin wrong")
	}
	for _, role := range []string{"owner", "operator", "viewer"} {
		h := f.roleHandler(role)
		if w := paymentRequest(h, "GET", "/api/v1/payments", "", "", false); w.Code != 200 {
			t.Fatalf("%s cannot list", role)
		}
		if w := paymentRequest(h, "GET", "/api/v1/payments/"+order.ID, "", "", false); w.Code != 200 {
			t.Fatalf("%s cannot read", role)
		}
		w := paymentRequest(h, "GET", "/api/v1/payment-reviews", "", "", false)
		if (role == "viewer" && w.Code != 403) || (role != "viewer" && w.Code != 200) {
			t.Fatalf("review role gate %s %d", role, w.Code)
		}
		w = paymentRequest(h, "POST", "/api/v1/payment-provider/confirm-webhook", "{}", "", true)
		if (role == "owner" && w.Code != 200) || (role != "owner" && w.Code != 403) {
			t.Fatalf("confirm role gate %s %d %s", role, w.Code, w.Body.String())
		}
		if role == "viewer" {
			for _, path := range []string{"/api/v1/payments", "/api/v1/payments/" + order.ID + "/cancel"} {
				if got := paymentRequest(h, "POST", path, `{"amountVnd":50}`, paymentKey(2), true); got.Code != 403 {
					t.Fatal("viewer mutation allowed")
				}
			}
		}
	}
	if w := paymentRequest(f.handler, "POST", "/api/v1/payments", `{"amountVnd":50}`, paymentKey(3), false); w.Code != 403 {
		t.Fatal("CSRF not required")
	}
	w := paymentRequest(f.roleHandler("operator"), "POST", "/api/v1/payments/"+order.ID+"/cancel", "{}", "", true)
	if w.Code != 200 || decodePaymentOrder(t, w).Status != "CANCELLED" {
		t.Fatalf("cancel %d %s", w.Code, w.Body.String())
	}
	if f.confirms.Load() != 1 {
		t.Fatal("unauthorized confirmation reached provider")
	}
	f.mu.Lock()
	url := f.confirmedURL
	f.mu.Unlock()
	if url != paymentTestOrigin+"/api/integrations/payos/webhook" {
		t.Fatal("confirm used noncanonical URL")
	}
	for _, table := range []string{"transactions", "payment_receipts", "event_journal", "deliveries"} {
		if webhookTestCount(t, f.store, table) != 0 {
			t.Fatalf("confirm sample generated money %s", table)
		}
	}
	if w := paymentRequest(f.handler, "POST", "/api/v1/payment-provider/confirm-webhook", `{"url":"https://evil.example"}`, "", true); w.Code != 400 || f.confirms.Load() != 1 {
		t.Fatal("confirmation URL override allowed")
	}
}

func TestPaymentStatusReviewsAndPaginationAreSanitizedReadonly(t *testing.T) {
	f := newPaymentHTTPFixture(t)
	w := paymentRequest(f.handler, "POST", "/api/public/v1/payments", `{"amountVnd":50,"origin":"STATIC_URL"}`, paymentKey(1), false)
	order := decodePaymentOrder(t, w)
	if err := f.store.RecordPaymentActivity(context.Background(), f.cfg.PayOSClientID, "RECONCILED"); err != nil {
		t.Fatal(err)
	}
	if got := webhookTestPost(f.handler, webhookTestBody(t, webhookTestSample())); got.Code != 200 {
		t.Fatal("sample rejected")
	}
	data := webhookTestData(order)
	data["amount"] = 51
	if got := webhookTestPost(f.handler, webhookTestBody(t, data)); got.Code != 200 {
		t.Fatal("review not committed")
	}
	before := f.creates.Load()
	for _, path := range []string{"/api/v1/status", "/api/public/v1/payment-config", "/api/v1/payment-reviews", "/api/v1/payments?limit=1&status=PENDING"} {
		w = paymentRequest(f.handler, "GET", path, "", "", false)
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("read %s %d", path, w.Code)
		}
		for _, secret := range []string{"http-contract-channel-secret", "http-contract-api-secret", webhookTestChecksum, "payloadJson", "MAIN-ACCOUNT", "secret-confirm-account"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatalf("metadata leaked %s", secret)
			}
		}
		if path == "/api/v1/status" {
			var body struct {
				Payments payments.Status `json:"payments"`
				ACB      json.RawMessage `json:"acb"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.ACB != nil || !body.Payments.Configured || body.Payments.Status != "READY" || body.Payments.LastWebhookAt == nil || body.Payments.LastReconciledAt == nil || body.Payments.PendingOrders != 1 || body.Payments.ReviewCount != 1 {
				t.Fatalf("status wrong: %+v", body)
			}
		}
	}
	if f.creates.Load() != before {
		t.Fatal("status read called provider")
	}
	for _, path := range []string{"/api/v1/payments?limit=101", "/api/v1/payments?limit=0", "/api/v1/payments?status=BAD", "/api/v1/payments?cursor=bad", "/api/v1/payment-reviews?limit=101", "/api/v1/payment-reviews?cursor=bad"} {
		if got := paymentRequest(f.handler, "GET", path, "", "", false); got.Code != 400 {
			t.Fatalf("invalid pagination accepted %s %d", path, got.Code)
		}
	}
}

func TestPaymentUncertainCreateAndSanitizedConfirmFailure(t *testing.T) {
	f := newPaymentHTTPFixture(t)
	f.createHTTPStatus.Store(http.StatusBadRequest)
	w := paymentRequest(f.handler, "POST", "/api/public/v1/payments", `{"amountVnd":50,"origin":"STATIC_URL"}`, paymentKey(1), false)
	if w.Code != http.StatusAccepted {
		t.Fatalf("uncertain create %d %s", w.Code, w.Body.String())
	}
	order := decodePaymentOrder(t, w)
	if order.Status != "CREATING" || order.QRCode != "" {
		t.Fatal("provider rejection manufactured a QR/status")
	}
	w = paymentRequest(f.handler, "POST", "/api/public/v1/payments", `{"amountVnd":50,"origin":"STATIC_URL"}`, paymentKey(1), false)
	if w.Code != http.StatusOK || decodePaymentOrder(t, w).ID != order.ID || f.creates.Load() != 1 {
		t.Fatal("uncertain intent replay recreated provider link")
	}
	f.confirmHTTPStatus.Store(http.StatusBadRequest)
	w = paymentRequest(f.handler, "POST", "/api/v1/payment-provider/confirm-webhook", "{}", "", true)
	if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "secret") || strings.Contains(w.Body.String(), "UNVERIFIED_PROVIDER_CODE") {
		t.Fatalf("SDK error leaked %d %s", w.Code, w.Body.String())
	}
}

func TestPaymentOperationalFlagsDoNotInvalidateIssuedOrders(t *testing.T) {
	f := newPaymentHTTPFixture(t)
	w := paymentRequest(f.handler, "POST", "/api/public/v1/payments", `{"amountVnd":50,"origin":"STATIC_URL"}`, paymentKey(1), false)
	order := decodePaymentOrder(t, w)
	cfg := f.cfg
	cfg.PaymentsEnabled, cfg.PayOSWebhookConfirmed = false, false
	service := payments.NewService(cfg, f.store, f.adapter, nil)
	h := New(cfg, f.store).WithPayments(service).Handler()
	if got := paymentRequest(h, "GET", "/api/public/v1/payments/"+order.ID, "", "", false); got.Code != 200 {
		t.Fatal("issued capability disabled")
	}
	w = paymentRequest(h, "POST", "/api/public/v1/payments", `{"amountVnd":50,"origin":"STATIC_URL"}`, paymentKey(1), false)
	if w.Code != 200 || decodePaymentOrder(t, w).ID != order.ID {
		t.Fatal("issued retry disabled")
	}
	w = paymentRequest(h, "POST", "/api/public/v1/payments", `{"amountVnd":50,"origin":"STATIC_URL"}`, paymentKey(2), false)
	if w.Code != 503 || !strings.Contains(w.Body.String(), "PAYMENTS_DISABLED") {
		t.Fatal("disabled creation allowed")
	}
	order, err := f.store.PaymentOrder(context.Background(), order.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := webhookTestPost(h, webhookTestBody(t, webhookTestData(order))); got.Code != 200 {
		t.Fatal("issued settlement disabled")
	}
	w = paymentRequest(h, "POST", "/api/v1/payments/"+order.ID+"/cancel", "{}", "", true)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "PAYMENT_ALREADY_PAID") {
		t.Fatal("paid cancellation allowed")
	}
	// Confirm while new payments are disabled must work without mutating the gate flag.
	f.handler = h
	w = paymentRequest(h, "POST", "/api/v1/payment-provider/confirm-webhook", "{}", "", true)
	if w.Code != 200 {
		t.Fatalf("disabled confirmation %d %s", w.Code, w.Body.String())
	}
	status, err := service.Status(context.Background())
	if err != nil || status.WebhookConfirmed || status.Status != "DISABLED" {
		t.Fatal("confirm silently changed deployment flags")
	}
	cfg.PaymentsEnabled = true
	service = payments.NewService(cfg, f.store, f.adapter, nil)
	h = New(cfg, f.store).WithPayments(service).Handler()
	w = paymentRequest(h, "POST", "/api/public/v1/payments", `{"amountVnd":50,"origin":"STATIC_URL"}`, paymentKey(3), false)
	if w.Code != 503 || !strings.Contains(w.Body.String(), "WEBHOOK_UNCONFIRMED") {
		t.Fatal("unconfirmed creation allowed")
	}
}

func TestPaymentDeploymentGateRejectsMutationsButKeepsSnapshots(t *testing.T) {
	f := newPaymentHTTPFixture(t)
	w := paymentRequest(f.handler, "POST", "/api/public/v1/payments", `{"amountVnd":50,"origin":"STATIC_URL"}`, paymentKey(1), false)
	order := decodePaymentOrder(t, w)
	gate, err := f.store.AcquireMutationGate(context.Background(), "payment-http-test", time.Minute, "upgrade")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.store.ReleaseMutationGate(context.Background(), "payment-http-test", gate.LeaseToken) }()
	for _, request := range []struct{ path, body, key string }{
		{"/api/public/v1/payments", `{"amountVnd":50,"origin":"STATIC_URL"}`, paymentKey(2)},
		{"/api/v1/payments", `{"amountVnd":50}`, paymentKey(3)},
		{"/api/v1/payments/" + order.ID + "/cancel", "{}", ""},
		{"/api/v1/payment-provider/confirm-webhook", "{}", ""},
	} {
		w = paymentRequest(f.handler, "POST", request.path, request.body, request.key, true)
		if w.Code != 503 || w.Header().Get("Retry-After") != "5" {
			t.Fatalf("mutation escaped gate: %s %d %s", request.path, w.Code, w.Body.String())
		}
	}
	if got := paymentRequest(f.handler, "GET", "/api/public/v1/payments/"+order.ID, "", "", false); got.Code != 200 {
		t.Fatal("gate hid issued snapshot")
	}
	w = paymentRequest(f.handler, "GET", "/api/public/v1/payment-config", "", "", false)
	var readiness payments.Config
	if err := json.Unmarshal(w.Body.Bytes(), &readiness); err != nil {
		t.Fatal(err)
	}
	if readiness.Ready || readiness.Status != "UNAVAILABLE" {
		t.Fatal("gate reported ready")
	}
	if f.creates.Load() != 1 || f.confirms.Load() != 0 {
		t.Fatal("gate issued provider request")
	}
}

func TestPaymentUnconfiguredContractWithoutProvider(t *testing.T) {
	store, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "unconfigured.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.Config{DevelopmentSubject: "owner", Timezone: time.UTC, PaymentPublicOrigin: paymentTestOrigin, PaymentMaxAmountVND: 500000000, PaymentsEnabled: true, Roles: config.RoleSubjects{Owners: map[string]struct{}{"owner": {}}}}
	h := New(cfg, store).Handler()
	w := paymentRequest(h, "GET", "/api/public/v1/payment-config", "", "", false)
	var readiness payments.Config
	if err := json.Unmarshal(w.Body.Bytes(), &readiness); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || readiness.Status != "UNCONFIGURED" || readiness.Ready || readiness.StaticURL != paymentTestOrigin+"/pay" || readiness.Provider != "PAYOS" || readiness.Bank != "KienlongBank" || readiness.MinAmountVND != 1 || readiness.MaxAmountVND != 500000000 {
		t.Fatalf("unconfigured readiness %+v %d", readiness, w.Code)
	}
	for _, path := range []string{"/api/v1/payments?status=CREATING&limit=20", "/api/v1/payments?status=PROCESSING&limit=20", "/api/v1/payment-reviews?limit=20"} {
		w = paymentRequest(h, "GET", path, "", "", false)
		if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("unconfigured read %s: %d %s", path, w.Code, w.Body.String())
		}
		var page storage.Page[storage.PaymentOrder]
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 0 {
			t.Fatalf("unconfigured empty database returned orders: %+v", page.Items)
		}
	}
	w = paymentRequest(h, "GET", "/api/public/v1/payments/"+strings.Repeat("A", 43), "", "", false)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown capability without provider: %d %s", w.Code, w.Body.String())
	}
	w = paymentRequest(h, "POST", "/api/public/v1/payments", `{"amountVnd":50,"origin":"STATIC_URL"}`, paymentKey(1), false)
	if w.Code != 503 || !strings.Contains(w.Body.String(), "PAYMENT_UNAVAILABLE") {
		t.Fatal("unconfigured creation accepted")
	}
}

func TestPaymentSnapshotsRemainReadableWithoutProvider(t *testing.T) {
	f := newPaymentHTTPFixture(t)
	w := paymentRequest(f.handler, "POST", "/api/public/v1/payments", `{"amountVnd":50000,"origin":"STATIC_URL"}`, paymentKey(1), false)
	if w.Code != http.StatusCreated {
		t.Fatalf("create issued order: %d %s", w.Code, w.Body.String())
	}
	order := decodePaymentOrder(t, w)
	cfg := f.cfg
	cfg.PayOSAPIKey, cfg.PayOSChecksumKey = "", ""
	h := New(cfg, f.store).Handler()
	for _, path := range []string{"/api/public/v1/payments/" + order.ID, "/api/v1/payments/" + order.ID} {
		w = paymentRequest(h, "GET", path, "", "", false)
		if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("issued snapshot %s: %d %s", path, w.Code, w.Body.String())
		}
		got := decodePaymentOrder(t, w)
		if got.ID != order.ID || got.Status != order.Status || got.QRCode != order.QRCode || got.OrderCode != order.OrderCode {
			t.Fatalf("snapshot changed without provider: %+v", got)
		}
	}
	w = paymentRequest(h, "GET", "/api/v1/payments?status=PENDING&limit=20", "", "", false)
	var page storage.Page[storage.PaymentOrder]
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || len(page.Items) != 1 || page.Items[0].ID != order.ID {
		t.Fatalf("issued order list without provider: %d %+v", w.Code, page.Items)
	}
	for _, path := range []string{"/api/v1/payments", "/api/v1/payments/" + order.ID + "/cancel", "/api/v1/payment-provider/confirm-webhook"} {
		body := "{}"
		if path == "/api/v1/payments" {
			body = `{"amountVnd":50000}`
		}
		w = paymentRequest(h, "POST", path, body, paymentKey(2), true)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("providerless mutation %s: %d %s", path, w.Code, w.Body.String())
		}
	}
	if f.creates.Load() != 1 || f.confirms.Load() != 0 {
		t.Fatal("snapshot recovery issued a provider request")
	}
}
