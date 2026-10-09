package storage

import (
	"context"
	"path/filepath"
	"testing"
)

func TestIngestTransactionDeduplicatesAndQuarantinesConflict(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	connection := historicalConnectionFixture(t, store, ctx, "***1234")
	input := TransactionInput{ConnectionID: connection.ID, SemanticKey: "ACB:123", CanonicalHash: "first", TransactionAt: "2026-09-10", EffectiveAt: "2026-09-10", Credit: 50000, ParserVersion: "v1"}
	first, err := store.IngestTransaction(ctx, input)
	if err != nil || !first.Inserted {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	second, err := store.IngestTransaction(ctx, input)
	if err != nil || second.Inserted || second.Conflict || second.TransactionID != first.TransactionID {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	input.CanonicalHash = "changed"
	conflict, err := store.IngestTransaction(ctx, input)
	if err != nil || !conflict.Conflict || conflict.Inserted {
		t.Fatalf("conflict=%+v err=%v", conflict, err)
	}
	var quarantined int
	if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM transaction_quarantine`).Scan(&quarantined); err != nil || quarantined != 1 {
		t.Fatalf("quarantine=%d err=%v", quarantined, err)
	}
}

func TestHistoricalTransactionsPreserveBaselineAndOutbox(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "historical.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	conn := historicalConnectionFixture(t, store, ctx, "***5678")
	ep, err := store.CreateEndpointWithSecret(ctx, "Hook 1", "https://example.com/webhook")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetEndpointStatus(ctx, ep.ID, "ACTIVE"); err != nil {
		t.Fatal(err)
	}
	credit := historicalTransactionFixture(t, store, ctx, TransactionInput{
		ConnectionID: conn.ID, SemanticKey: "ACB:1001", CanonicalHash: "credit-hash",
		TransactionAt: "2026-09-12 10:00:00", EffectiveAt: "2026-09-12", Credit: 150000,
		Description: []byte("Deposit 1"), ParserVersion: "v1",
	}, "REALTIME", "NONE")
	historicalTransactionFixture(t, store, ctx, TransactionInput{
		ConnectionID: conn.ID, SemanticKey: "ACB:1002", CanonicalHash: "debit-hash",
		TransactionAt: "2026-09-12 10:05:00", EffectiveAt: "2026-09-12", Debit: 50000,
		Description: []byte("Withdraw 1"), ParserVersion: "v1",
	}, "REALTIME", "NONE")
	baseline := historicalTransactionFixture(t, store, ctx, TransactionInput{
		ConnectionID: conn.ID, SemanticKey: "ACB:1003", CanonicalHash: "baseline-hash",
		TransactionAt: "2026-09-12 09:00:00", EffectiveAt: "2026-09-12", Credit: 500000,
		Description: []byte("Historical Deposit"), ParserVersion: "v1",
	}, "REALTIME", "BASELINE")
	_, err = store.EmitTransactionEvent(ctx, credit, "bank.transaction.credit", "acb", "ACB:1001", map[string]any{"credit": "150000"})
	if err != nil {
		t.Fatal(err)
	}
	var deliveryCount, journalCount, transactionCount, baselineEvents int
	if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM deliveries WHERE status='PENDING'`).Scan(&deliveryCount); err != nil || deliveryCount != 1 {
		t.Fatalf("pending deliveries=%d err=%v", deliveryCount, err)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM event_journal WHERE aggregate_id=?`, credit).Scan(&journalCount); err != nil || journalCount != 1 {
		t.Fatalf("journal entries=%d err=%v", journalCount, err)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM transactions`).Scan(&transactionCount); err != nil || transactionCount != 3 {
		t.Fatalf("historical transactions=%d err=%v", transactionCount, err)
	}
	var baselineState string
	if err := store.DB().QueryRowContext(ctx, `SELECT baseline_state FROM transactions WHERE id=?`, baseline).Scan(&baselineState); err != nil || baselineState != "BASELINE" {
		t.Fatalf("baseline state=%s err=%v", baselineState, err)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM events WHERE transaction_id<>?`, credit).Scan(&baselineEvents); err != nil || baselineEvents != 0 {
		t.Fatalf("unexpected debit/baseline events=%d err=%v", baselineEvents, err)
	}
}
