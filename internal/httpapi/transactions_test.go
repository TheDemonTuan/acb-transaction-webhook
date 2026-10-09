package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/config"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func TestTransactionsFilteredAndSummaryAndDetail(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test_txns.db")
	store, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("Open store: %v", err)
	}
	defer store.Close()

	connID := "conn_txn_test"
	if _, err := store.DB().ExecContext(ctx, `
		INSERT INTO connections(id, state, generation, created_at, updated_at)
		VALUES(?, 'MONITORING', 1, '2026-09-12T00:00:00Z', '2026-09-12T00:00:00Z')
	`, connID); err != nil {
		t.Fatalf("insert connection: %v", err)
	}

	// Seed 150 transactions today (100 credit of 10,000, 50 debit of 5,000)
	// and 10 transactions yesterday
	now := time.Now().UTC()
	var seededDetailID string
	for i := 1; i <= 150; i++ {
		credit := int64(0)
		debit := int64(0)
		if i <= 100 {
			credit = 10000
		} else {
			debit = 5000
		}
		item := []historicalTransactionFixture{
			{
				Number:        fmt.Sprintf("TXN_TODAY_%03d", i),
				Credit:        credit,
				Debit:         debit,
				TransactionAt: "12/09/2026 10:00:00",
				EffectiveAt:   "12/09/2026",
				Description:   fmt.Sprintf("Payment order %d for coffee", i),
			},
		}
		res, err := seedHistoricalTransactions(ctx, store, connID, "123***789", item, "REALTIME")
		if err != nil {
			t.Fatalf("ingest item %d: %v", i, err)
		}
		if i == 42 && len(res.NewEvents) > 0 {
			seededDetailID = res.NewEvents[0].TransactionID
		}
	}

	// Ingest 10 yesterday transactions
	for i := 1; i <= 10; i++ {
		item := []historicalTransactionFixture{
			{
				Number:        fmt.Sprintf("TXN_YEST_%03d", i),
				Credit:        20000,
				Debit:         0,
				TransactionAt: "11/09/2026 14:00:00",
				EffectiveAt:   "11/09/2026",
				Description:   fmt.Sprintf("Yesterday transaction %d", i),
			},
		}
		if _, err := seedHistoricalTransactions(ctx, store, connID, "123***789", item, "REALTIME"); err != nil {
			t.Fatalf("ingest yesterday item %d: %v", i, err)
		}
	}

	srv := New(config.Config{Timezone: time.UTC, DevelopmentSubject: "owner"}, store)

	// 1. Query today: from=2026-09-12&to=2026-09-12&limit=50
	req := httptest.NewRequest(http.MethodGet, "/api/v1/transactions?from=2026-09-12&to=2026-09-12&limit=50", nil)
	rec := httptest.NewRecorder()
	srv.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Items      []storage.TransactionView   `json:"items"`
		NextCursor string                      `json:"nextCursor"`
		Summary    *storage.TransactionSummary `json:"summary"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if len(resp.Items) != 50 {
		t.Errorf("expected 50 items on page, got %d", len(resp.Items))
	}
	if resp.NextCursor == "" {
		t.Errorf("expected non-empty nextCursor")
	}
	if resp.Summary == nil {
		t.Fatalf("expected non-nil summary")
	}
	if resp.Summary.TotalCount != 150 {
		t.Errorf("expected summary count 150, got %d", resp.Summary.TotalCount)
	}
	expectedIncoming := int64(100 * 10000)
	expectedOutgoing := int64(50 * 5000)
	if resp.Summary.Incoming != expectedIncoming {
		t.Errorf("expected incoming %d, got %d", expectedIncoming, resp.Summary.Incoming)
	}
	if resp.Summary.Outgoing != expectedOutgoing {
		t.Errorf("expected outgoing %d, got %d", expectedOutgoing, resp.Summary.Outgoing)
	}

	// 2. Query direction=credit
	reqCredit := httptest.NewRequest(http.MethodGet, "/api/v1/transactions?from=2026-09-12&to=2026-09-12&direction=credit", nil)
	recCredit := httptest.NewRecorder()
	srv.handler.ServeHTTP(recCredit, reqCredit)
	if recCredit.Code != http.StatusOK {
		t.Fatalf("expected 200 for credit, got %d", recCredit.Code)
	}
	var respCredit struct {
		Summary *storage.TransactionSummary `json:"summary"`
	}
	_ = json.Unmarshal(recCredit.Body.Bytes(), &respCredit)
	if respCredit.Summary.TotalCount != 100 {
		t.Errorf("expected credit count 100, got %d", respCredit.Summary.TotalCount)
	}

	// 3. Query search: q=coffee
	reqSearch := httptest.NewRequest(http.MethodGet, "/api/v1/transactions?from=2026-09-12&to=2026-09-12&q=coffee", nil)
	recSearch := httptest.NewRecorder()
	srv.handler.ServeHTTP(recSearch, reqSearch)
	if recSearch.Code != http.StatusOK {
		t.Fatalf("expected 200 for search, got %d", recSearch.Code)
	}
	var respSearch struct {
		Summary *storage.TransactionSummary `json:"summary"`
	}
	_ = json.Unmarshal(recSearch.Body.Bytes(), &respSearch)
	if respSearch.Summary.TotalCount != 150 {
		t.Errorf("expected search count 150, got %d", respSearch.Summary.TotalCount)
	}

	// 4. Test Transaction Detail API
	if seededDetailID == "" {
		t.Fatalf("seededDetailID is empty")
	}
	reqDetail := httptest.NewRequest(http.MethodGet, "/api/v1/transactions/"+seededDetailID, nil)
	recDetail := httptest.NewRecorder()
	srv.handler.ServeHTTP(recDetail, reqDetail)
	if recDetail.Code != http.StatusOK {
		t.Fatalf("expected 200 for detail, got %d: %s", recDetail.Code, recDetail.Body.String())
	}
	var detail storage.TransactionView
	if err := json.Unmarshal(recDetail.Body.Bytes(), &detail); err != nil {
		t.Fatalf("unmarshal detail: %v", err)
	}
	if detail.ID != seededDetailID {
		t.Errorf("expected ID %q, got %q", seededDetailID, detail.ID)
	}
	if detail.Credit != 10000 {
		t.Errorf("expected credit 10000, got %d", detail.Credit)
	}
	if detail.TransactionDay != "2026-09-12" {
		t.Errorf("expected day 2026-09-12, got %q", detail.TransactionDay)
	}

	// Unknown detail returns 404
	req404 := httptest.NewRequest(http.MethodGet, "/api/v1/transactions/nonexistent_id", nil)
	rec404 := httptest.NewRecorder()
	srv.handler.ServeHTTP(rec404, req404)
	if rec404.Code != http.StatusNotFound {
		t.Errorf("expected 404 for unknown transaction, got %d", rec404.Code)
	}
	_ = now
}
