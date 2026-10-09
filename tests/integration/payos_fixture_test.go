package integration_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	payos "github.com/payOSHQ/payos-lib-golang/v2"
	"github.com/thedemontuan/acb-transaction-webhook/internal/config"
	"github.com/thedemontuan/acb-transaction-webhook/internal/httpapi"
	"github.com/thedemontuan/acb-transaction-webhook/internal/payments"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

const integrationChecksum = "integration-payos-checksum"
const integrationWebhookPath = "/api/integrations/payos/webhook"

// This HTTP fixture exercises the production SDK's response and webhook
// signature verification. It is not a provider implementation in a binary.
type paymentFixture struct {
	service      *payments.Service
	handler      http.Handler
	creates      atomic.Int64
	beforeCreate func(context.Context, payos.CreatePaymentLinkRequest)
}

func newPaymentFixture(t *testing.T, store *storage.Store, onCommit func(storage.EventNotification)) *paymentFixture {
	t.Helper()
	f := &paymentFixture{}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v2/payment-requests" {
			t.Errorf("unexpected provider request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var request payos.CreatePaymentLinkRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.creates.Add(1)
		if f.beforeCreate != nil {
			f.beforeCreate(r.Context(), request)
		}
		link := fmt.Sprintf("link-%d", request.OrderCode)
		data := map[string]any{"orderCode": request.OrderCode, "amount": request.Amount, "description": request.Description,
			"currency": "VND", "paymentLinkId": link, "status": "PENDING", "qrCode": "original-provider-qr-" + link,
			"accountNumber": fmt.Sprintf("VA-%d", request.OrderCode), "accountName": "SHOP", "bin": "970452",
			"checkoutUrl": "https://pay.payos.vn/web/" + link}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": "00", "data": data, "signature": paymentSignature(t, data)})
	}))
	t.Cleanup(provider.Close)
	adapter, err := payments.NewPayOS("integration-channel", "integration-api", integrationChecksum,
		payments.WithBaseURL(provider.URL), payments.WithHTTPClient(provider.Client()))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{DevelopmentSubject: "pipeline@example.com", Timezone: time.UTC,
		PayOSClientID: "integration-channel", PayOSAPIKey: "integration-api", PayOSChecksumKey: integrationChecksum,
		PaymentsEnabled: true, PayOSWebhookConfirmed: true, PaymentMaxAmountVND: 500000000,
		PaymentPublicOrigin: "https://transactions.example.test"}
	f.service = payments.NewService(cfg, store, adapter, onCommit)
	f.handler = httpapi.New(cfg, store).WithPayments(f.service).Handler()
	return f
}

func paymentSignature(t *testing.T, data map[string]any) string {
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
	mac := hmac.New(sha256.New, []byte(integrationChecksum))
	_, _ = mac.Write([]byte(strings.Join(parts, "&")))
	return hex.EncodeToString(mac.Sum(nil))
}

func createPayment(t *testing.T, f *paymentFixture, amount int64, index int) storage.PaymentOrder {
	t.Helper()
	order, created, err := f.service.CreateOrder(context.Background(), amount, "OPERATOR_DYNAMIC", fmt.Sprintf("00000000-0000-4000-8000-%012d", index))
	if err != nil || !created || order.Status != "PENDING" {
		t.Fatalf("create order: order=%+v created=%v err=%v", order, created, err)
	}
	return order
}

func paymentWebhookBody(t *testing.T, order storage.PaymentOrder) []byte {
	t.Helper()
	data := map[string]any{"orderCode": order.OrderCode, "amount": order.AmountVnd, "description": order.Description,
		"accountNumber": "MAIN-ACCOUNT", "virtualAccountNumber": order.AccountNumber,
		"reference": fmt.Sprintf("REF-%d", order.OrderCode), "transactionDateTime": "2026-10-09 10:05:00",
		"currency": "VND", "paymentLinkId": order.PaymentLinkID, "code": "00"}
	body, err := json.Marshal(map[string]any{"code": "00", "success": true, "data": data, "signature": paymentSignature(t, data)})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func postPaymentWebhook(t *testing.T, handler http.Handler, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "https://transactions.example.test"+integrationWebhookPath, bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func settlePayment(t *testing.T, f *paymentFixture, order storage.PaymentOrder) {
	t.Helper()
	w := postPaymentWebhook(t, f.handler, paymentWebhookBody(t, order))
	if w.Code != http.StatusOK {
		t.Fatalf("settlement webhook: status=%d body=%s", w.Code, w.Body.String())
	}
}

// Historical ACB rows are retained data, not a runtime connection mutation.
// Stable IDs, amounts, hashes and pending outbox rows make restoration observable.
func seedLegacyHistory(t *testing.T, store *storage.Store) {
	t.Helper()
	_, err := store.DB().Exec(`
INSERT INTO connections(id,bank_code,account_masked,state,generation,created_at,updated_at)
VALUES('legacy-acb','ACB','***8888','PAUSED',7,'2026-09-01T00:00:00Z','2026-09-01T00:00:00Z');
INSERT INTO transactions(id,connection_id,semantic_key,canonical_hash,transaction_date,effective_date,debit,credit,balance,parser_version,baseline_state,first_seen_at,transaction_at_iso,transaction_day,ingest_source)
VALUES('legacy-credit','legacy-acb','ACB:LEGACY-CREDIT','legacy-credit-hash','01/09/2026','01/09/2026',0,100000,500000,'acb-v1','NONE','2026-09-01T00:00:00Z','2026-09-01T00:00:00Z','2026-09-01','REALTIME'),
('legacy-debit','legacy-acb','ACB:LEGACY-DEBIT','legacy-debit-hash','01/09/2026','01/09/2026',25000,0,475000,'acb-v1','NONE','2026-09-01T00:00:00Z','2026-09-01T00:00:00Z','2026-09-01','REALTIME');
INSERT INTO events(id,transaction_id,event_type,payload,payload_hash,created_at)
VALUES('legacy-event','legacy-credit','bank.transaction.credit','{"transactionId":"legacy-credit","credit":"100000","bank":"ACB"}','legacy-event-hash','2026-09-01T00:00:00Z');
INSERT INTO event_journal(epoch,event_type,aggregate_id,payload_json,created_at)
VALUES('ep1','bank.transaction.credit','legacy-credit','{"transactionId":"legacy-credit","credit":"100000","bank":"ACB"}','2026-09-01T00:00:00Z');
INSERT INTO webhook_endpoints(id,name,status,current_revision,created_at,updated_at)
VALUES('legacy-endpoint','Retained webhook','ACTIVE',1,'2026-09-01T00:00:00Z','2026-09-01T00:00:00Z');
INSERT INTO endpoint_versions(endpoint_id,revision,url,filters_json,created_at)
VALUES('legacy-endpoint',1,'https://example.test/legacy','{}','2026-09-01T00:00:00Z');
INSERT INTO deliveries(id,event_id,endpoint_id,endpoint_revision,key_id,status,attempts,next_attempt_at,created_at,updated_at)
VALUES('legacy-delivery','legacy-event','legacy-endpoint',1,'retained-key','PENDING',2,'2099-01-01T00:00:00Z','2026-09-01T00:00:00Z','2026-09-01T00:00:00Z');`)
	if err != nil {
		t.Fatal(err)
	}
}

func assertLegacyHistory(t *testing.T, db *sql.DB) {
	t.Helper()
	var count int
	var credit, debit int64
	err := db.QueryRow(`SELECT count(*),sum(credit),sum(debit) FROM transactions WHERE connection_id='legacy-acb' AND ((id='legacy-credit' AND canonical_hash='legacy-credit-hash') OR (id='legacy-debit' AND canonical_hash='legacy-debit-hash'))`).Scan(&count, &credit, &debit)
	if err != nil || count != 2 || credit != 100000 || debit != 25000 {
		t.Fatalf("retained history changed: count=%d credit=%d debit=%d err=%v", count, credit, debit, err)
	}
	var status string
	var attempts int
	if err := db.QueryRow(`SELECT status,attempts FROM deliveries WHERE id='legacy-delivery' AND event_id='legacy-event' AND endpoint_id='legacy-endpoint'`).Scan(&status, &attempts); err != nil || status != "PENDING" || attempts != 2 {
		t.Fatalf("retained outbox changed: status=%s attempts=%d err=%v", status, attempts, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM event_journal WHERE aggregate_id='legacy-credit' AND event_type='bank.transaction.credit'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("retained journal changed: count=%d err=%v", count, err)
	}
}
