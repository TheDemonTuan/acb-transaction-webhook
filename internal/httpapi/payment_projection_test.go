package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/config"
	"github.com/thedemontuan/acb-transaction-webhook/internal/eventhub"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func projectionStore(t *testing.T) *storage.Store {
	t.Helper()
	s, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "projection.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func projectionSettle(t *testing.T, s *storage.Store, source string) (storage.PaymentOrder, storage.EventNotification) {
	t.Helper()
	ctx := context.Background()
	o, _, err := s.ReservePaymentOrder(ctx, storage.PaymentOrderIntent{
		ChannelID: "projection-private-client-id", IdempotencyKey: "projection-" + source,
		RequestHash: "hash-" + source, AmountVnd: 50000, Origin: "OPERATOR_DYNAMIC",
	})
	if err != nil {
		t.Fatal(err)
	}
	link := "projection-private-link-" + source
	account := "projection-private-va-" + source
	o, err = s.CompletePaymentOrderOperation(ctx, o.ID, o.OperationToken, storage.PaymentOrderUpdate{
		Status: "PENDING", PaymentLinkID: link, AccountNumber: account,
		QRCode: "projection-private-qr", CheckoutURL: "https://pay.payos.vn/" + link,
		NextReconcileAt: time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	in := storage.SettlementInput{
		ChannelID: o.ChannelID, OrderCode: o.OrderCode, PaymentLinkID: link, Reference: "projection-reference-" + source,
		AmountVnd: o.AmountVnd, TransactionAt: time.Date(2026, 10, 8, 18, 25, 0, 0, time.FixedZone("HCM", 7*3600)),
		AccountNumber: "projection-private-main-account", VirtualAccountNumber: account,
		Description: "untrusted " + o.ID + " " + account, Source: source,
	}
	if source == "CATCH_UP" {
		in.VerifiedGet = &storage.SettlementEvidence{
			OrderCode: in.OrderCode, PaymentLinkID: in.PaymentLinkID, Reference: in.Reference,
			AmountVnd: in.AmountVnd, TransactionAt: in.TransactionAt,
			AccountNumber: in.AccountNumber, VirtualAccountNumber: in.VirtualAccountNumber,
		}
	}
	settled, err := s.SettlePayment(ctx, in)
	if err != nil || settled.Event == nil || settled.Order.Status != "PAID" {
		t.Fatalf("settlement=%+v err=%v", settled, err)
	}
	return settled.Order, *settled.Event
}

func projectionJSON(t *testing.T, h http.Handler, path string) map[string]any {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("GET %s: status=%d cache=%q body=%s", path, w.Code, w.Header().Get("Cache-Control"), w.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func assertProjectionSafe(t *testing.T, data map[string]any, o storage.PaymentOrder) {
	t.Helper()
	for _, key := range []string{"balance", "idCapability", "paymentLinkId", "checkoutUrl", "channelId", "accountNumber", "virtualAccountNumber", "counterAccount", "qrCode", "clientId", "apiKey", "checksumKey"} {
		if _, exists := data[key]; exists {
			t.Fatalf("public projection contains %s: %v", key, data)
		}
	}
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{o.ID, o.ChannelID, o.PaymentLinkID, o.CheckoutURL, o.AccountNumber, "projection-private-main-account", "projection-private-api-key", "projection-private-checksum-key", "987654321"} {
		if value != "" && strings.Contains(string(raw), value) {
			t.Fatalf("public projection leaked %q: %s", value, raw)
		}
	}
}

func TestPublicPaymentTransactionsAndLegacyHistoryProjection(t *testing.T) {
	s := projectionStore(t)
	ctx := context.Background()
	if _, err := s.DB().ExecContext(ctx, `INSERT INTO connections(id,bank_code,state,generation,created_at,updated_at) VALUES('legacy-projection','ACB','MONITORING',1,'2026-09-12T00:00:00Z','2026-09-12T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	balance := int64(987654321)
	legacy, err := seedHistoricalTransactions(ctx, s, "legacy-projection", "123***789", []historicalTransactionFixture{
		{Number: "legacy-credit", Credit: 25000, Balance: &balance, TransactionAt: "12/09/2026 10:00:00", EffectiveAt: "12/09/2026", Description: "Legacy receipt"},
		{Number: "legacy-debit", Debit: 20000, Balance: &balance, TransactionAt: "12/09/2026 11:00:00", EffectiveAt: "12/09/2026", Description: "Legacy withdrawal"},
	}, "REALTIME")
	if err != nil || len(legacy.NewEvents) != 1 {
		t.Fatalf("legacy=%+v %v", legacy, err)
	}
	h := New(config.Config{DevelopmentSubject: "dev@example.com"}, s).Handler()
	orders := map[string]storage.PaymentOrder{}
	for _, source := range []string{"REALTIME", "CATCH_UP"} {
		o, _ := projectionSettle(t, s, source)
		orders[o.TransactionID] = o
		for _, prefix := range []string{"/api/public/v1", "/api/v1"} {
			detail := projectionJSON(t, h, prefix+"/transactions/"+o.TransactionID)
			want := map[string]any{"id": o.TransactionID, "bank": "KienlongBank", "provider": "PAYOS", "orderCode": strconv.FormatInt(o.OrderCode, 10), "credit": float64(50000), "debit": float64(0), "transactionDate": "2026-10-08T11:25:00Z", "transactionDay": "2026-10-08", "source": source, "description": o.Description}
			for k, v := range want {
				if detail[k] != v {
					t.Fatalf("%s detail[%s]=%v want=%v", prefix, k, detail[k], v)
				}
			}
			if prefix == "/api/public/v1" {
				assertProjectionSafe(t, detail, o)
			}
		}
	}
	legacyID := legacy.NewEvents[0].TransactionID
	legacyDetail := projectionJSON(t, h, "/api/public/v1/transactions/"+legacyID)
	if legacyDetail["bank"] != "ACB" || legacyDetail["credit"] != float64(25000) || legacyDetail["source"] != "REALTIME" || legacyDetail["transactionDay"] != "2026-09-12" {
		t.Fatalf("legacy detail=%v", legacyDetail)
	}
	for _, key := range []string{"provider", "orderCode", "balance"} {
		if _, ok := legacyDetail[key]; ok {
			t.Fatalf("legacy public detail contains %s", key)
		}
	}
	privateLegacy := projectionJSON(t, h, "/api/v1/transactions/"+legacyID)
	if privateLegacy["balance"] != float64(balance) || privateLegacy["bank"] != "ACB" {
		t.Fatalf("private legacy detail=%v", privateLegacy)
	}
	var debitID string
	if err := s.DB().QueryRow(`SELECT id FROM transactions WHERE semantic_key='ACB:legacy-debit'`).Scan(&debitID); err != nil {
		t.Fatal(err)
	}
	blocked := httptest.NewRecorder()
	h.ServeHTTP(blocked, httptest.NewRequest(http.MethodGet, "/api/public/v1/transactions/"+debitID, nil))
	if blocked.Code != http.StatusNotFound {
		t.Fatalf("public debit detail status=%d", blocked.Code)
	}
	page := projectionJSON(t, h, "/api/public/v1/transactions?direction=debit&limit=100")
	items := page["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("public count=%d", len(items))
	}
	for _, value := range items {
		item := value.(map[string]any)
		id := item["id"].(string)
		if item["debit"] != float64(0) || id == debitID {
			t.Fatalf("public list contains debit: %v", item)
		}
		if o, ok := orders[id]; ok {
			assertProjectionSafe(t, item, o)
			if item["bank"] != "KienlongBank" || item["orderCode"] != strconv.FormatInt(o.OrderCode, 10) {
				t.Fatalf("public list correlation=%v", item)
			}
		} else if id != legacyID || item["bank"] != "ACB" {
			t.Fatalf("unexpected public transaction=%v", item)
		}
	}
	summary := page["summary"].(map[string]any)
	if summary["count"] != float64(3) || summary["incoming"] != float64(125000) || summary["outgoing"] != float64(0) {
		t.Fatalf("public summary=%v", summary)
	}
}

func projectionSSEFrame(t *testing.T, scanner *bufio.Scanner) (string, string, map[string]any) {
	t.Helper()
	var id, event, data string
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if event == "" {
				continue
			}
			var payload map[string]any
			if err := json.Unmarshal([]byte(data), &payload); err != nil {
				t.Fatalf("SSE JSON: %v (%s)", err, data)
			}
			return id, event, payload
		}
		if strings.HasPrefix(line, "id: ") {
			id = strings.TrimPrefix(line, "id: ")
		} else if strings.HasPrefix(line, "event: ") {
			event = strings.TrimPrefix(line, "event: ")
		} else if strings.HasPrefix(line, "data: ") {
			data = strings.TrimPrefix(line, "data: ")
		}
	}
	t.Fatalf("SSE frame unavailable: %v", scanner.Err())
	return "", "", nil
}

func TestPublicPaymentSSELiveAndJournalReplaySecurity(t *testing.T) {
	for _, source := range []string{"REALTIME", "CATCH_UP"} {
		t.Run(source, func(t *testing.T) {
			s := projectionStore(t)
			hub := eventhub.New()
			server := New(config.Config{DevelopmentSubject: "dev@example.com"}, s).WithEventHub(hub)
			ts := httptest.NewServer(server.Handler())
			defer ts.Close()
			client := ts.Client()
			client.Timeout = 3 * time.Second
			live, err := client.Get(ts.URL + "/api/public/v1/events")
			if err != nil {
				t.Fatal(err)
			}
			defer live.Body.Close()
			scanner := bufio.NewScanner(live.Body)
			_, event, initial := projectionSSEFrame(t, scanner)
			if event != "initial_state" || initial["watermark"] != float64(0) {
				t.Fatalf("initial=%s %v", event, initial)
			}
			o, committed := projectionSettle(t, s, source)
			var payload map[string]any
			if err := json.Unmarshal(committed.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if payload["paymentLinkId"] != o.PaymentLinkID {
				t.Fatalf("internal event lost linkId: %v", payload)
			}
			// Seed future/private fields into both transports to test the allowlist,
			// not just today's settlement payload, which is already minimal.
			private := map[string]any{"id": o.ID, "idCapability": o.ID, "checkoutUrl": o.CheckoutURL, "channelId": o.ChannelID, "accountNumber": o.AccountNumber, "virtualAccountNumber": o.AccountNumber, "counterAccount": "projection-private-main-account", "clientId": o.ChannelID, "apiKey": "projection-private-api-key", "checksumKey": "projection-private-checksum-key", "balance": 987654321}
			for key, value := range private {
				payload[key] = value
			}
			committed.Payload, err = json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.DB().Exec(`UPDATE event_journal SET payload_json=? WHERE epoch=? AND seq=?`, string(committed.Payload), committed.Epoch, committed.JournalSeq); err != nil {
				t.Fatal(err)
			}
			hub.Publish(eventhub.Event{Epoch: committed.Epoch, Seq: committed.JournalSeq, EventType: committed.EventType, AggregateID: committed.TransactionID, Payload: committed.Payload, CommittedAt: committed.CommittedAt})
			id, kind, liveData := projectionSSEFrame(t, scanner)
			live.Body.Close()
			if kind != "bank.transaction.credit" || id != fmt.Sprintf("ep1:%d", committed.JournalSeq) {
				t.Fatalf("live id/type=%s %s", id, kind)
			}
			assertProjectionSafe(t, liveData, o)
			want := map[string]any{"provider": "PAYOS", "bank": "KienlongBank", "orderCode": strconv.FormatInt(o.OrderCode, 10), "paymentOrigin": "OPERATOR_DYNAMIC", "transactionId": o.TransactionID, "transactionNumber": "projection-reference-" + source, "credit": "50000", "debit": "0", "currency": "VND", "transactionDate": "2026-10-08T11:25:00Z", "transactionDay": "2026-10-08", "source": source, "description": o.Description, "datePrecision": "datetime"}
			for key, value := range want {
				if liveData[key] != value {
					t.Fatalf("live[%s]=%v want=%v", key, liveData[key], value)
				}
			}
			for _, prefix := range []string{"/api/public/v1", "/api/v1"} {
				req, err := http.NewRequest(http.MethodGet, ts.URL+prefix+"/events", nil)
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Last-Event-ID", "ep1:0")
				response, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				replayID, replayKind, replayData := projectionSSEFrame(t, bufio.NewScanner(response.Body))
				response.Body.Close()
				if replayID != id || replayKind != kind {
					t.Fatalf("replay id/type=%s %s", replayID, replayKind)
				}
				if prefix == "/api/public/v1" {
					assertProjectionSafe(t, replayData, o)
					liveJSON, _ := json.Marshal(liveData)
					replayJSON, _ := json.Marshal(replayData)
					if string(liveJSON) != string(replayJSON) {
						t.Fatalf("live/replay differ: %s %s", liveJSON, replayJSON)
					}
				} else {
					if replayData["paymentLinkId"] != o.PaymentLinkID || replayData["id"] != o.ID {
						t.Fatalf("private replay stripped internal fields: %v", replayData)
					}
				}
			}
		})
	}
}
