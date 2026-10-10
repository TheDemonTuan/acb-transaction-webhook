package integration_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
	_ "modernc.org/sqlite"
)

// Run the actual migration runner, failing its final connection insert after
// tables have been created. A retry must preserve every historical money row.
func TestSQLiteMigrationRollbackOnFailure(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gateway.db")
	store, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	seedLegacyHistory(t, store)
	// Construct the retained v14 schema without editing historical migrations.
	if _, err := store.DB().ExecContext(ctx, `
DROP TABLE sepay_receipts;
DROP TABLE sepay_telegram_inbox;
DROP TABLE payment_provider_operations;
DROP TABLE payment_provider_runtime;
DROP TABLE payment_provider_credential_versions;
DROP TABLE payment_provider_config;
DROP TABLE payment_receipts;
DROP TABLE payos_webhook_inbox;
DROP TABLE payment_orders;
DELETE FROM connections WHERE id='payos-klb';
DELETE FROM schema_migrations WHERE version >= 15;
CREATE TRIGGER reject_payos_migration BEFORE INSERT ON connections
WHEN NEW.id='payos-klb' BEGIN SELECT RAISE(ABORT,'migration fixture failure'); END;`); err != nil {
		t.Fatal(err)
	}
	before, err := store.SchemaVersion(ctx)
	if err != nil || before.Version != 14 {
		t.Fatalf("v14 fixture: schema=%+v err=%v", before, err)
	}
	if err := store.Migrate(ctx); err == nil {
		t.Fatal("expected actual payOS migration to fail")
	}
	after, err := store.SchemaVersion(ctx)
	if err != nil || after.Version != before.Version || after.AppliedCount != before.AppliedCount {
		t.Fatalf("failed migration changed version: before=%+v after=%+v err=%v", before, after, err)
	}
	var count int
	if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE name IN ('payment_orders','payment_receipts','payos_webhook_inbox','idx_payment_orders_reconcile','idx_payment_orders_page')`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial payOS schema survived rollback: count=%d err=%v", count, err)
	}
	assertLegacyHistory(t, store.DB())
	if _, err := store.DB().ExecContext(ctx, `DROP TRIGGER reject_payos_migration`); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("retry migration: %v", err)
	}
	assertLegacyHistory(t, store.DB())
	var bank, state string
	if err := store.DB().QueryRowContext(ctx, `SELECT bank_code,state FROM connections WHERE id='payos-klb'`).Scan(&bank, &state); err != nil || bank != "KienlongBank" || state != "WEBHOOK" {
		t.Fatalf("payOS source: bank=%s state=%s err=%v", bank, state, err)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT bank_code FROM connections WHERE id='legacy-acb'`).Scan(&bank); err != nil || bank != "ACB" {
		t.Fatalf("historical bank label changed: bank=%s err=%v", bank, err)
	}
	// A pre-issuance snapshot is no longer financially authoritative once an
	// order/receipt exists: restoring it would lose this reference and credit.
	beforePayments := filepath.Join(t.TempDir(), "before-payments.db")
	if err := store.Backup(ctx, beforePayments); err != nil {
		t.Fatal(err)
	}
	f := newPaymentFixture(t, store, nil)
	order := createPayment(t, f, 50000, 1)
	settlePayment(t, f, order)
	var transactionID string
	if err := store.DB().QueryRowContext(ctx, `SELECT transaction_id FROM payment_receipts WHERE order_id=? AND amount_vnd=50000`, order.ID).Scan(&transactionID); err != nil || transactionID == "" {
		t.Fatalf("live payment receipt missing: transaction=%s err=%v", transactionID, err)
	}
	oldSnapshot, err := sql.Open("sqlite", "file:"+filepath.ToSlash(beforePayments)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer oldSnapshot.Close()
	assertLegacyHistory(t, oldSnapshot)
	if err := oldSnapshot.QueryRowContext(ctx, `SELECT count(*) FROM payment_orders`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("pre-issuance snapshot unexpectedly contains order: count=%d err=%v", count, err)
	}
	if err := oldSnapshot.QueryRowContext(ctx, `SELECT count(*) FROM payment_receipts WHERE transaction_id=?`, transactionID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rollback boundary fixture invalid: count=%d err=%v", count, err)
	}
	assertLegacyHistory(t, store.DB())
	var integrity string
	if err := store.DB().QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity=%s err=%v", integrity, err)
	}
}
