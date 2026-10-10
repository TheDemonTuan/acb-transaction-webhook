package storage

import (
	"context"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestTransactionQueriesPreserveLegacyBankAndPaymentCorrelation(t *testing.T) {
	ctx := context.Background()
	s := sepayTestStore(t)
	if _, err := s.DB().ExecContext(ctx, `INSERT INTO connections(id,bank_code,state,generation,created_at,updated_at) VALUES('legacy-query','ACB','PAUSED',1,'2026-09-12T00:00:00Z','2026-09-12T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	balance := int64(987654321)
	legacy, err := s.IngestTransaction(ctx, TransactionInput{
		ConnectionID: "legacy-query", SemanticKey: "ACB:legacy-query-credit", CanonicalHash: "legacy-query-hash",
		TransactionAt: "12/09/2026 10:00:00", EffectiveAt: "12/09/2026", Credit: 25000,
		Balance: &balance, Description: []byte("Legacy receipt"), ParserVersion: "v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	for index, source := range []string{"REALTIME", "CATCH_UP"} {
		o, in := settlementTestOrder(t, s, "projection-"+source, 50000, true)
		in.Source = source
		if source == "CATCH_UP" {
			in.VerifiedGet = settlementTestEvidence(in)
		}
		settled, err := s.SettlePayment(ctx, in)
		if err != nil || settled.Event == nil {
			t.Fatalf("settlement: %+v %v", settled, err)
		}
		detail, err := s.GetTransactionByID(ctx, settled.Order.TransactionID)
		if err != nil {
			t.Fatal(err)
		}
		if detail.Bank != "KienlongBank" || detail.Provider != "PAYOS" || detail.OrderCode != strconv.FormatInt(o.OrderCode, 10) || detail.Credit != 50000 || detail.Debit != 0 || detail.Balance != nil || detail.Source != source || detail.TransactionAt != "2026-10-08T11:25:00Z" || detail.TransactionDay != "2026-10-08" || detail.Description != o.Description {
			t.Fatalf("payment detail: %+v", detail)
		}
		inSePay := sepayTestInput(int64(index+1), int64(index+1), "projection-"+source)
		if source == "CATCH_UP" {
			inSePay.MessageAt = time.Now().UTC().Add(-time.Hour)
			sepayTestPayload(&inSePay, "private sender memo")
		}
		sepay, err := s.IngestSePayNotification(ctx, inSePay)
		if err != nil || sepay.Event == nil {
			t.Fatalf("SePay ingest: %+v %v", sepay, err)
		}
		detail, err = s.GetTransactionByID(ctx, sepay.Event.TransactionID)
		if err != nil {
			t.Fatal(err)
		}
		if detail.Bank != "VCB" || detail.Provider != "SEPAY" || detail.OrderCode != "" || detail.Credit != 50000 || detail.Debit != 0 || detail.Balance != nil || detail.Source != source || detail.TransactionAt != "2026-10-09T17:05:00Z" || detail.TransactionDay != "2026-10-10" || detail.Description != "Thanh toán QR cửa hàng" {
			t.Fatalf("SePay detail: %+v", detail)
		}
		raw, err := json.Marshal(detail)
		if err != nil {
			t.Fatal(err)
		}
		for _, private := range []string{"private sender memo", "TEST123", "900001", "900002", "-100900003", `"orderCode"`, `"balance"`, "rawPayload", "payloadEnvelope"} {
			if strings.Contains(string(raw), private) {
				t.Fatalf("SePay projection leaked %q: %s", private, raw)
			}
		}
	}
	legacyDetail, err := s.GetTransactionByID(ctx, legacy.TransactionID)
	if err != nil {
		t.Fatal(err)
	}
	if legacyDetail.Bank != "ACB" || legacyDetail.Provider != "" || legacyDetail.OrderCode != "" || legacyDetail.Balance == nil || *legacyDetail.Balance != balance {
		t.Fatalf("legacy detail: %+v", legacyDetail)
	}
	raw, err := json.Marshal(legacyDetail)
	if err != nil {
		t.Fatal(err)
	}
	var legacyJSON map[string]any
	if err := json.Unmarshal(raw, &legacyJSON); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"provider", "orderCode"} {
		if _, ok := legacyJSON[key]; ok {
			t.Fatalf("legacy JSON includes %s: %s", key, raw)
		}
	}
	page, err := s.ListTransactionsFiltered(ctx, TransactionFilter{Direction: "credit", Limit: 1})
	if err != nil || page.Summary == nil || page.Summary.TotalCount != 5 || page.Summary.Incoming != 225000 {
		t.Fatalf("page: %+v %v", page, err)
	}
	seen := map[string]bool{}
	for {
		for _, item := range page.Items {
			if seen[item.ID] {
				t.Fatalf("duplicate paginated transaction %s", item.ID)
			}
			seen[item.ID] = true
			detail, err := s.GetTransactionByID(ctx, item.ID)
			if err != nil || !reflect.DeepEqual(detail, &item) {
				t.Fatalf("list/detail differ: %+v %+v %v", item, detail, err)
			}
		}
		if page.NextCursor == "" {
			break
		}
		page, err = s.ListTransactionsFiltered(ctx, TransactionFilter{Direction: "credit", Limit: 1, Cursor: page.NextCursor})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 5 {
		t.Fatalf("paginated count=%d", len(seen))
	}
	filtered, err := s.ListTransactionsFiltered(ctx, TransactionFilter{From: "2026-10-08", To: "2026-10-08", Direction: "credit", Query: "DH", Limit: 100})
	if err != nil || len(filtered.Items) != 2 || filtered.Summary.Incoming != 100000 {
		t.Fatalf("date/search filter: %+v %v", filtered, err)
	}
}

func TestSePayHistoryDoesNotCorrelateSameAmountPayOSOrders(t *testing.T) {
	ctx := context.Background()
	s := sepayTestStore(t)
	first, _ := settlementTestOrder(t, s, "same-amount-first", 50000, true)
	second, _ := settlementTestOrder(t, s, "same-amount-second", 50000, true)
	in := sepayTestInput(1, 1, "SAME_AMOUNT")
	sepayTestPayload(&in, "DH"+strconv.FormatInt(first.OrderCode, 10))
	result, err := s.IngestSePayNotification(ctx, in)
	if err != nil || result.Event == nil {
		t.Fatalf("SePay ingest: %+v %v", result, err)
	}
	for _, order := range []PaymentOrder{first, second} {
		var status, transactionID string
		if err := s.DB().QueryRowContext(ctx, `SELECT status,COALESCE(transaction_id,'') FROM payment_orders WHERE id=?`, order.ID).Scan(&status, &transactionID); err != nil || status != "PENDING" || transactionID != "" {
			t.Fatalf("same-amount order changed: %s status=%s transaction=%s err=%v", order.ID, status, transactionID, err)
		}
	}
	page, err := s.ListTransactionsFiltered(ctx, TransactionFilter{Direction: "credit", Limit: 100})
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != result.Event.TransactionID || page.Items[0].Provider != "SEPAY" || page.Items[0].OrderCode != "" || page.Summary.Incoming != 50000 {
		t.Fatalf("same-amount snapshot: %+v %v", page, err)
	}
	sepayTestCount(t, s, "payment_receipts", 0)
}

func TestPaymentProjectionTakesPrecedenceOverSePayReceipt(t *testing.T) {
	ctx := context.Background()
	s := paymentOrderTestStore(t)
	o, in := settlementTestOrder(t, s, "provider-priority", 50000, true)
	settled, err := s.SettlePayment(ctx, in)
	if err != nil || settled.Event == nil {
		t.Fatalf("settlement: %+v %v", settled, err)
	}
	// An overlapping receipt must not override authoritative payOS correlation.
	if _, err := s.DB().ExecContext(ctx, `INSERT INTO sepay_receipts(store_key,bank_code,account_number,reference,canonical_hash,transaction_id,message_id,received_at) VALUES('projection-overlap','KienlongBank','receiver','overlap','hash',?,1,?)`, settled.Order.TransactionID, settled.Event.CommittedAt); err != nil {
		t.Fatal(err)
	}
	detail, err := s.GetTransactionByID(ctx, settled.Order.TransactionID)
	if err != nil || detail.Provider != "PAYOS" || detail.OrderCode != strconv.FormatInt(o.OrderCode, 10) {
		t.Fatalf("provider precedence detail: %+v %v", detail, err)
	}
	page, err := s.ListTransactionsFiltered(ctx, TransactionFilter{Direction: "credit", Limit: 100})
	if err != nil || len(page.Items) != 1 || !reflect.DeepEqual(detail, &page.Items[0]) || page.Summary.Incoming != 50000 {
		t.Fatalf("provider precedence list: %+v %v", page, err)
	}
}
