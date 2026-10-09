package storage

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/thedemontuan/acb-transaction-webhook/internal/security"
)

func TestEmitTransactionEventAndDeliveries(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	conn := historicalConnectionFixture(t, store, ctx, "***1234")

	ep, err := store.CreateEndpointWithSecret(ctx, "Test Hook", "https://example.com/webhook")
	if err != nil || ep.Secret == "" {
		t.Fatalf("create ep failed: %+v %v", ep, err)
	}
	if err := store.SetEndpointStatus(ctx, ep.ID, "ACTIVE"); err != nil {
		t.Fatal(err)
	}

	txn, err := store.IngestTransaction(ctx, TransactionInput{
		ConnectionID:  conn.ID,
		SemanticKey:   "ACB:999",
		CanonicalHash: "hash999",
		TransactionAt: "2026-09-10",
		EffectiveAt:   "2026-09-10",
		Credit:        100000,
		ParserVersion: "v1",
	})
	if err != nil || !txn.Inserted {
		t.Fatalf("ingest failed: %+v %v", txn, err)
	}

	eventID, err := store.EmitTransactionEvent(ctx, txn.TransactionID, "bank.transaction.credit", "acb", "ACB:999", map[string]any{
		"amount": 100000,
	})
	if err != nil || eventID == "" {
		t.Fatalf("emit event failed: %v", err)
	}

	summary, err := store.DeliverySummary(ctx)
	if err != nil || summary.Pending != 1 {
		t.Fatalf("summary unexpected: %+v %v", summary, err)
	}

	url, secret, err := store.EndpointDetails(ctx, ep.ID)
	if err != nil || url != "https://example.com/webhook" || string(secret) != ep.Secret {
		t.Fatalf("endpoint details: %s %s %v", url, string(secret), err)
	}
}

func TestEndpointSecretEncryptionWithKeyring(t *testing.T) {
	ctx := context.Background()
	keyPath := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(keyPath, []byte("0123456789012345678901234567890123456789012345678901234567890123"), 0o600); err != nil {
		t.Fatal(err)
	}
	keyring, err := security.LoadKeyring(keyPath)
	if err != nil {
		t.Fatal(err)
	}

	store, err := Open(ctx, filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.WithKeyring(keyring)

	ep, err := store.CreateEndpointWithSecret(ctx, "Encrypted Hook", "https://example.com/webhook")
	if err != nil || ep.Secret == "" {
		t.Fatalf("create encrypted ep failed: %+v %v", ep, err)
	}

	// Verify that the secret in database is an encrypted JSON envelope
	var rawEnvelope []byte
	err = store.DB().QueryRowContext(ctx, "SELECT envelope FROM endpoint_secrets WHERE endpoint_id = ?", ep.ID).Scan(&rawEnvelope)
	if err != nil {
		t.Fatalf("query raw envelope: %v", err)
	}
	if !bytes.HasPrefix(rawEnvelope, []byte(`{"Version":`)) {
		t.Fatalf("expected encrypted envelope JSON, got %s", string(rawEnvelope))
	}

	// Verify EndpointDetails decrypts it cleanly
	url, decrypted, err := store.EndpointDetails(ctx, ep.ID)
	if err != nil {
		t.Fatalf("decrypt endpoint details failed: %v", err)
	}
	if url != "https://example.com/webhook" || string(decrypted) != ep.Secret {
		t.Fatalf("mismatch decrypted secret: expected %s, got %s", ep.Secret, string(decrypted))
	}

	// Verify that without keyring, EndpointDetails fails with meaningful error
	storeNoKeyring, err := Open(ctx, filepath.Join(t.TempDir(), "gateway_nokey.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer storeNoKeyring.Close()
	// Insert raw encrypted envelope into storeNoKeyring
	_, _ = storeNoKeyring.DB().ExecContext(ctx, "INSERT INTO webhook_endpoints(id, name, status, current_revision, created_at, updated_at) VALUES(?, 'Hook', 'ACTIVE', 1, 'now', 'now')", ep.ID)
	_, _ = storeNoKeyring.DB().ExecContext(ctx, "INSERT INTO endpoint_versions(endpoint_id, revision, url, filters_json, created_at) VALUES(?, 1, 'https://example.com/webhook', '[]', 'now')", ep.ID)
	_, _ = storeNoKeyring.DB().ExecContext(ctx, "INSERT INTO endpoint_secrets(endpoint_id, key_id, envelope, status, created_at) VALUES(?, 'k1', ?, 'ACTIVE', 'now')", ep.ID, rawEnvelope)
	_, _, err = storeNoKeyring.EndpointDetails(ctx, ep.ID)
	if err == nil {
		t.Fatal("expected error decrypting without keyring, got nil")
	}
}
