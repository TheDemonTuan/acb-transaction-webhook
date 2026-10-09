package httpapi

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strconv"
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

const webhookTestChecksum = "http-webhook-fixture-checksum"
const webhookTestPath = "/api/integrations/payos/webhook"

func webhookTestSignature(t *testing.T, data map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	var normalized map[string]any
	if err := json.Unmarshal(encoded, &normalized); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(normalized))
	for key := range normalized {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		var value string
		switch item := normalized[key].(type) {
		case nil:
		case string:
			value = item
		case float64:
			value = strconv.FormatFloat(item, 'f', -1, 64)
		default:
			encoded, err := json.Marshal(item)
			if err != nil {
				t.Fatal(err)
			}
			value = string(encoded)
		}
		parts = append(parts, key+"="+value)
	}
	mac := hmac.New(sha256.New, []byte(webhookTestChecksum))
	_, _ = mac.Write([]byte(strings.Join(parts, "&")))
	return hex.EncodeToString(mac.Sum(nil))
}

func webhookTestBody(t *testing.T, data map[string]any) []byte {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{"code": "00", "success": true, "data": data, "signature": webhookTestSignature(t, data)})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func webhookTestPost(handler http.Handler, encoded []byte) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "https://transactions.example.test"+webhookTestPath, bytes.NewReader(encoded))
	r.Header.Set("Content-Type", "application/json")
	// No Access identity, CSRF token or cookies: only the signature authenticates.
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func webhookTestSample() map[string]any {
	return map[string]any{"orderCode": 123, "amount": 3000, "description": "VQRIO123", "accountNumber": "12345678", "reference": "TF230204212323", "transactionDateTime": "2023-02-04 18:25:00", "currency": "VND", "paymentLinkId": "124c33293c43417ab7879e14c8d9eb18", "code": "00"}
}

func webhookTestData(order storage.PaymentOrder) map[string]any {
	return map[string]any{"orderCode": order.OrderCode, "amount": order.AmountVnd, "description": order.Description, "accountNumber": "MAIN-ACCOUNT", "virtualAccountNumber": order.AccountNumber, "reference": fmt.Sprintf("REF-%d", order.OrderCode), "transactionDateTime": "2026-10-08 10:05:00", "currency": "VND", "paymentLinkId": order.PaymentLinkID, "code": "00", "unknownSignedField": map[string]any{"items": []any{1, 2, 3}}}
}

func webhookTestSetup(t *testing.T) (*storage.Store, *payments.Service, http.Handler, *atomic.Int64) {
	t.Helper()
	store, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "webhook.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v2/payment-requests" {
			t.Errorf("unexpected provider request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
			return
		}
		var request payos.CreatePaymentLinkRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		link := fmt.Sprintf("link-%d", request.OrderCode)
		data := map[string]any{"orderCode": request.OrderCode, "amount": request.Amount, "description": request.Description, "currency": "VND", "paymentLinkId": link, "status": "PENDING", "qrCode": "provider-original-qr", "accountNumber": fmt.Sprintf("VA-%d", request.OrderCode), "accountName": "SHOP", "bin": "970452", "checkoutUrl": "https://pay.payos.vn/web/" + link}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": "00", "data": data, "signature": webhookTestSignature(t, data)})
	}))
	t.Cleanup(fixture.Close)
	adapter, err := payments.NewPayOS("webhook-fixture-channel", "fixture-api", webhookTestChecksum, payments.WithBaseURL(fixture.URL), payments.WithHTTPClient(fixture.Client()))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Production: true, Timezone: time.UTC, PayOSClientID: "webhook-fixture-channel", PayOSAPIKey: "fixture-api", PayOSChecksumKey: webhookTestChecksum, PaymentsEnabled: true, PayOSWebhookConfirmed: true, PaymentMaxAmountVND: 500000000, PaymentPublicOrigin: "https://transactions.example.test"}
	commits := &atomic.Int64{}
	service := payments.NewService(cfg, store, adapter, func(event storage.EventNotification) {
		// The callback observes committed storage, never partially written money.
		var receipts int
		if err := store.DB().QueryRow("SELECT count(*) FROM payment_receipts WHERE transaction_id=?", event.TransactionID).Scan(&receipts); err != nil || receipts != 1 {
			t.Errorf("publish before commit: receipts=%d err=%v", receipts, err)
		}
		commits.Add(1)
	})
	return store, service, New(cfg, store).WithPayments(service).Handler(), commits
}

func webhookTestCount(t *testing.T, store *storage.Store, table string) int {
	t.Helper()
	var count int
	if err := store.DB().QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestPayOSWebhookStrictBodyAndSignature(t *testing.T) {
	store, _, handler, _ := webhookTestSetup(t)
	valid := webhookTestBody(t, webhookTestSample())
	cases := []struct {
		name   string
		body   []byte
		status int
	}{
		{"malformed", []byte("{"), 400}, {"array", []byte("[]"), 400}, {"null", []byte("null"), 400},
		{"trailing", append(append([]byte{}, valid...), []byte(" {}")...), 400},
		{"duplicateEnvelope", []byte(`{"code":"00","code":"00","success":true,"data":{},"signature":"x"}`), 400},
		{"duplicateData", []byte(`{"code":"00","success":true,"data":{"orderCode":123,"orderCode":123,"amount":3000},"signature":"x"}`), 400},
		{"duplicateUnknownNested", []byte(`{"data":{"extension":{"x":1,"x":2}}}`), 400},
		{"oversize", []byte(strings.Repeat(" ", maxPayOSWebhookBytes+1)), 413},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			if w := webhookTestPost(handler, item.body); w.Code != item.status {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
	for _, signature := range []any{nil, 123, map[string]any{}, strings.Repeat("0", 64), "not-hex"} {
		body, _ := json.Marshal(map[string]any{"code": "00", "success": true, "data": webhookTestSample(), "signature": signature})
		if w := webhookTestPost(handler, body); w.Code != 401 {
			t.Fatalf("signature %T: %d %s", signature, w.Code, w.Body.String())
		}
	}
	for _, amount := range []any{"3000", 1.5, json.Number("9007199254740991.1"), json.Number("9007199254740992")} {
		data := webhookTestSample()
		data["amount"] = amount
		if w := webhookTestPost(handler, webhookTestBody(t, data)); w.Code != 400 {
			t.Fatalf("unsafe amount %v: %d %s", amount, w.Code, w.Body.String())
		}
	}
	for _, key := range []string{"accountNumber", "reference", "transactionDateTime", "currency", "paymentLinkId", "code", "virtualAccountNumber"} {
		data := webhookTestSample()
		data[key] = 123
		if w := webhookTestPost(handler, webhookTestBody(t, data)); w.Code != 400 {
			t.Fatalf("wrong %s type: %d", key, w.Code)
		}
	}
	for _, change := range []func(map[string]any){
		func(body map[string]any) { body["success"] = "true" },
		func(body map[string]any) { body["code"] = 0 },
		func(body map[string]any) { delete(body, "success") },
	} {
		data := webhookTestSample()
		body := map[string]any{"code": "00", "success": true, "data": data, "signature": webhookTestSignature(t, data)}
		change(body)
		encoded, _ := json.Marshal(body)
		if w := webhookTestPost(handler, encoded); w.Code != 400 {
			t.Fatalf("wrong envelope type: %d", w.Code)
		}
	}
	for _, path := range []string{webhookTestPath + "/", "/api/integrations/payos/other"} {
		r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(valid))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code == 200 {
			t.Fatalf("non-exact route accepted %s", path)
		}
	}
	r := httptest.NewRequest(http.MethodGet, webhookTestPath, nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code == 200 {
		t.Fatal("GET accepted")
	}
	if webhookTestCount(t, store, "payos_webhook_inbox") != 0 || webhookTestCount(t, store, "transactions") != 0 {
		t.Fatal("invalid bodies retained")
	}
}

func TestPayOSWebhookExactSampleAndNonSuccess(t *testing.T) {
	store, _, handler, commits := webhookTestSetup(t)
	if w := webhookTestPost(handler, webhookTestBody(t, webhookTestSample())); w.Code != 200 || w.Body.String() != "{\"ok\":true}\n" {
		t.Fatalf("sample %d %s", w.Code, w.Body.String())
	}
	for _, table := range []string{"payment_orders", "payment_receipts", "transactions", "events", "event_journal", "deliveries", "payos_webhook_inbox"} {
		if webhookTestCount(t, store, table) != 0 {
			t.Fatalf("sample changed %s", table)
		}
	}
	changed := webhookTestSample()
	changed["description"] = "changed sample"
	if w := webhookTestPost(handler, webhookTestBody(t, changed)); w.Code != 200 {
		t.Fatalf("changed sample %d", w.Code)
	}
	var reason string
	if err := store.DB().QueryRow("SELECT reason FROM payos_webhook_inbox").Scan(&reason); err != nil || reason != "UNKNOWN_ORDER" {
		t.Fatalf("changed sample reason=%s err=%v", reason, err)
	}
	data := webhookTestSample()
	data["reference"] = "NON-SUCCESS"
	body, _ := json.Marshal(map[string]any{"code": "99", "success": false, "data": data, "signature": webhookTestSignature(t, data)})
	for range 3 {
		if w := webhookTestPost(handler, body); w.Code != 200 {
			t.Fatalf("non-success %d", w.Code)
		}
	}
	if err := store.DB().QueryRow("SELECT reason FROM payos_webhook_inbox WHERE reference='NON-SUCCESS'").Scan(&reason); err != nil || reason != "NON_SUCCESS" {
		t.Fatalf("reason=%s err=%v", reason, err)
	}
	if webhookTestCount(t, store, "payos_webhook_inbox") != 2 || webhookTestCount(t, store, "transactions") != 0 || commits.Load() != 0 {
		t.Fatal("review/sample generated money or duplicate inbox")
	}
}

func TestPayOSWebhookSameAmountOutOfOrderReplayAndReviews(t *testing.T) {
	store, service, handler, commits := webhookTestSetup(t)
	endpoint, err := store.CreateEndpointWithSecret(context.Background(), "receiver", "https://receiver.example.test/events")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec("UPDATE webhook_endpoints SET status='ACTIVE' WHERE id=?", endpoint.ID); err != nil {
		t.Fatal(err)
	}
	orders := make([]storage.PaymentOrder, 3)
	for i, amount := range []int64{50000, 50000, 120000} {
		orders[i], _, err = service.CreateOrder(context.Background(), amount, "OPERATOR_DYNAMIC", fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1))
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, index := range []int{1, 0, 2} {
		if w := webhookTestPost(handler, webhookTestBody(t, webhookTestData(orders[index]))); w.Code != 200 {
			t.Fatalf("settle %d %s", w.Code, w.Body.String())
		}
	}
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if w := webhookTestPost(handler, webhookTestBody(t, webhookTestData(orders[1]))); w.Code != 200 {
				t.Errorf("replay %d %s", w.Code, w.Body.String())
			}
		}()
	}
	wg.Wait()
	for _, table := range []string{"transactions", "payment_receipts", "events", "event_journal", "deliveries"} {
		if webhookTestCount(t, store, table) != 3 {
			t.Fatalf("duplicate %s", table)
		}
	}
	var total int64
	if err := store.DB().QueryRow("SELECT sum(credit) FROM transactions").Scan(&total); err != nil || total != 220000 || commits.Load() != 3 {
		t.Fatalf("total=%d commits=%d err=%v", total, commits.Load(), err)
	}
	for _, order := range orders {
		current, err := store.PaymentOrder(context.Background(), order.ID)
		if err != nil || current.Status != "PAID" || current.TransactionID == "" {
			t.Fatalf("wrong paid order %+v %v", current, err)
		}
	}
	for _, item := range []struct {
		change func(map[string]any)
		reason string
	}{
		{func(data map[string]any) { data["amount"] = 50001 }, "REFERENCE_CONFLICT"},
		{func(data map[string]any) { data["paymentLinkId"] = "another-link" }, "REFERENCE_CONFLICT"},
		{func(data map[string]any) { data["reference"] = "EXTRA-REFERENCE" }, "EXTRA_PAYMENT_REVIEW"},
		{func(data map[string]any) { data["virtualAccountNumber"] = "wrong-VA" }, "PAYMENT_MISMATCH"},
		{func(data map[string]any) { data["currency"] = "USD" }, "PAYMENT_MISMATCH"},
		{func(data map[string]any) { data["transactionDateTime"] = "invalid" }, "INVALID_TRANSACTION_DATE"},
		{func(data map[string]any) { data["reference"] = "" }, "PAYMENT_MISMATCH"},
	} {
		data := webhookTestData(orders[0])
		item.change(data)
		if w := webhookTestPost(handler, webhookTestBody(t, data)); w.Code != 200 {
			t.Fatalf("review %s: %d", item.reason, w.Code)
		}
		var count int
		if err := store.DB().QueryRow("SELECT count(*) FROM payos_webhook_inbox WHERE reason=?", item.reason).Scan(&count); err != nil || count == 0 {
			t.Fatalf("missing %s err=%v", item.reason, err)
		}
	}
	if webhookTestCount(t, store, "payment_receipts") != 3 || commits.Load() != 3 {
		t.Fatal("review changed money")
	}
}

func TestPayOSWebhookGateAndAtomicFailureRetry(t *testing.T) {
	store, service, handler, commits := webhookTestSetup(t)
	order, _, err := service.CreateOrder(context.Background(), 50000, "STATIC_URL", "00000000-0000-4000-8000-000000000001")
	if err != nil {
		t.Fatal(err)
	}
	body := webhookTestBody(t, webhookTestData(order))
	gate, err := store.AcquireMutationGate(context.Background(), "webhook-test", time.Minute, "upgrade")
	if err != nil {
		t.Fatal(err)
	}
	for _, encoded := range [][]byte{body, webhookTestBody(t, webhookTestSample())} {
		if w := webhookTestPost(handler, encoded); w.Code != 503 || w.Header().Get("Retry-After") != "5" {
			t.Fatalf("gate %d %s", w.Code, w.Body.String())
		}
	}
	if err := store.ReleaseMutationGate(context.Background(), "webhook-test", gate.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec("CREATE TRIGGER webhook_fail_journal BEFORE INSERT ON event_journal BEGIN SELECT RAISE(ABORT,'fixture failure'); END"); err != nil {
		t.Fatal(err)
	}
	if w := webhookTestPost(handler, body); w.Code != 503 {
		t.Fatalf("fault %d %s", w.Code, w.Body.String())
	}
	for _, table := range []string{"transactions", "payment_receipts", "events", "event_journal", "deliveries"} {
		if webhookTestCount(t, store, table) != 0 {
			t.Fatalf("partial financial commit in %s", table)
		}
	}
	current, err := store.PaymentOrder(context.Background(), order.ID)
	if err != nil || current.Status != "PENDING" || commits.Load() != 0 {
		t.Fatalf("partial order %+v err=%v", current, err)
	}
	if _, err := store.DB().Exec("DROP TRIGGER webhook_fail_journal"); err != nil {
		t.Fatal(err)
	}
	if w := webhookTestPost(handler, body); w.Code != 200 {
		t.Fatalf("retry %d %s", w.Code, w.Body.String())
	}
	if webhookTestCount(t, store, "payment_receipts") != 1 || commits.Load() != 1 {
		t.Fatal("retry did not commit once")
	}
}
