package storage

import (
	"context"
	"path/filepath"
	"testing"
)

func TestBackfillCanonicalDatesAndSourcePolicy(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test_backfill.db")
	store, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	conn := historicalConnectionFixture(t, store, ctx, "***1234")
	connID := conn.ID
	// Insert a legacy row without canonical day/ISO.
	legacyTxnID := historicalTransactionFixture(t, store, ctx, TransactionInput{
		ConnectionID: connID, SemanticKey: "ACB:1111", CanonicalHash: "hash1",
		TransactionAt: "12/09/2026 10:32:15", EffectiveAt: "12/09/2026", Credit: 100000,
		ParserVersion: "v1",
	}, "REALTIME", "NONE")

	// Run backfill
	updated, err := store.BackfillCanonicalDates(ctx)
	if err != nil {
		t.Fatalf("BackfillCanonicalDates: %v", err)
	}
	if updated != 1 {
		t.Errorf("expected 1 row updated, got %d", updated)
	}

	var day, iso, precision string
	err = store.db.QueryRowContext(ctx, `
		SELECT transaction_day, transaction_at_iso, date_precision 
		FROM transactions WHERE id = ?
	`, legacyTxnID).Scan(&day, &iso, &precision)
	if err != nil {
		t.Fatalf("query backfilled row: %v", err)
	}
	if day != "2026-09-12" {
		t.Errorf("expected day 2026-09-12, got %q", day)
	}
	if precision != "datetime" {
		t.Errorf("expected precision datetime, got %q", precision)
	}

	// Re-run backfill should update 0 rows
	updated2, err := store.BackfillCanonicalDates(ctx)
	if err != nil {
		t.Fatalf("BackfillCanonicalDates second run: %v", err)
	}
	if updated2 != 0 {
		t.Errorf("expected 0 rows updated on rerun, got %d", updated2)
	}

	filterTxnID := historicalTransactionFixture(t, store, ctx, TransactionInput{
		ConnectionID: connID, SemanticKey: "ACB:2222", CanonicalHash: "hash2",
		TransactionAt: "01/02/2026", EffectiveAt: "01/02/2026", Credit: 50000,
		Description: []byte("Old history sync"), ParserVersion: "v1",
	}, "FILTER_SYNC", "NONE")
	if updated, err := store.BackfillCanonicalDates(ctx); err != nil || updated != 1 {
		t.Fatalf("backfill historical filter row: updated=%d err=%v", updated, err)
	}
	var eventCount, deliveryCount int
	if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM events WHERE transaction_id=?`, filterTxnID).Scan(&eventCount); err != nil || eventCount != 0 {
		t.Fatalf("historical FILTER_SYNC events=%d err=%v", eventCount, err)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM deliveries`).Scan(&deliveryCount); err != nil || deliveryCount != 0 {
		t.Fatalf("historical FILTER_SYNC deliveries=%d err=%v", deliveryCount, err)
	}

	var filterDay, filterSource string
	err = store.db.QueryRowContext(ctx, `
		SELECT transaction_day, ingest_source 
		FROM transactions WHERE semantic_key = 'ACB:2222'
	`).Scan(&filterDay, &filterSource)
	if err != nil {
		t.Fatalf("query FILTER_SYNC row: %v", err)
	}
	if filterDay != "2026-02-01" {
		t.Errorf("expected filterDay 2026-02-01, got %q", filterDay)
	}
	if filterSource != "FILTER_SYNC" {
		t.Errorf("expected filterSource FILTER_SYNC, got %q", filterSource)
	}
}
