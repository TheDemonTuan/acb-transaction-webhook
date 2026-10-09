package storage

import (
	"context"
	"encoding/json"
	"reflect"
	"strconv"
	"testing"
)

func TestTransactionQueriesPreserveLegacyBankAndPaymentCorrelation(t *testing.T) {
	ctx := context.Background()
	s := paymentOrderTestStore(t)
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
	for _, source := range []string{"REALTIME", "CATCH_UP"} {
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
	if err != nil || page.Summary == nil || page.Summary.TotalCount != 3 || page.Summary.Incoming != 125000 {
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
	if len(seen) != 3 {
		t.Fatalf("paginated count=%d", len(seen))
	}
	filtered, err := s.ListTransactionsFiltered(ctx, TransactionFilter{From: "2026-10-08", To: "2026-10-08", Direction: "credit", Query: "DH", Limit: 100})
	if err != nil || len(filtered.Items) != 2 || filtered.Summary.Incoming != 100000 {
		t.Fatalf("date/search filter: %+v %v", filtered, err)
	}
}
