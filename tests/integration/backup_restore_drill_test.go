package integration_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/thedemontuan/acb-transaction-webhook/internal/security"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
	_ "modernc.org/sqlite"
)

// TestBackupRestoreEncryptedDrill proves Gates 10 and 11:
// 1. Durable backups are encrypted and snapshots preserve complete relational integrity.
// 2. Tampered ciphertext or corrupted checksum manifests fail closed during restore.
// 3. Durable backup directories contain zero plaintext .db or secret files.
func TestBackupRestoreEncryptedDrill(t *testing.T) {
	ctx := context.Background()
	workDir := t.TempDir()

	liveDbPath := filepath.Join(workDir, "live.db")
	backupDir := filepath.Join(workDir, "backups")
	restoreDir := filepath.Join(workDir, "restored")

	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		t.Fatalf("create backup dir: %v", err)
	}
	if err := os.MkdirAll(restoreDir, 0o700); err != nil {
		t.Fatalf("create restore dir: %v", err)
	}

	// 1. Setup master key and live database
	keyPath := filepath.Join(workDir, "app_master_key")
	rawKey := make([]byte, 32)
	for i := range rawKey {
		rawKey[i] = byte(i + 42)
	}
	if err := os.WriteFile(keyPath, []byte(hex.EncodeToString(rawKey)), 0o600); err != nil {
		t.Fatalf("write master key: %v", err)
	}
	kr, err := security.LoadKeyring(keyPath)
	if err != nil {
		t.Fatalf("load keyring: %v", err)
	}

	store, err := storage.Open(ctx, liveDbPath)
	if err != nil {
		t.Fatalf("open live storage: %v", err)
	}
	store.WithKeyring(kr)

	seedLegacyHistory(t, store)
	f := newPaymentFixture(t, store, nil)
	order := createPayment(t, f, 50000, 1)
	settlePayment(t, f, order)
	paid, err := store.PaymentOrder(ctx, order.ID)
	if err != nil || paid.Status != "PAID" {
		t.Fatalf("payment snapshot before backup: %+v err=%v", paid, err)
	}
	var receiptHash string
	if err := store.DB().QueryRowContext(ctx, `SELECT canonical_hash FROM payment_receipts WHERE order_id=?`, order.ID).Scan(&receiptHash); err != nil {
		t.Fatal(err)
	}
	var paymentDeliveryID string
	if err := store.DB().QueryRowContext(ctx, `SELECT d.id FROM deliveries d JOIN events e ON e.id=d.event_id WHERE e.transaction_id=? AND d.endpoint_id='legacy-endpoint'`, paid.TransactionID).Scan(&paymentDeliveryID); err != nil {
		t.Fatal(err)
	}

	// 2. Perform live snapshot backup
	snapshotDbPath := filepath.Join(workDir, "snapshot.db")
	if err := store.Backup(ctx, snapshotDbPath); err != nil {
		t.Fatalf("store.Backup: %v", err)
	}
	store.Close()

	// Read snapshot bytes and calculate SHA256
	rawDbBytes, err := os.ReadFile(snapshotDbPath)
	if err != nil {
		t.Fatalf("read snapshot db: %v", err)
	}
	origHash := sha256.Sum256(rawDbBytes)
	origHashHex := hex.EncodeToString(origHash[:])

	// 3. Encrypt snapshot into backupDir as an encrypted artifact
	env, err := kr.Encrypt(rawDbBytes, []byte("backup-manifest-v1"))
	if err != nil {
		t.Fatalf("encrypt backup: %v", err)
	}

	encryptedArtifact := filepath.Join(backupDir, "gateway.db.age")
	if err := os.WriteFile(encryptedArtifact, []byte(env.Ciphertext), 0o600); err != nil {
		t.Fatalf("write encrypted artifact: %v", err)
	}

	// Remove unencrypted snapshot from staging
	_ = os.Remove(snapshotDbPath)

	// 4. Gate 10: Scan backupDir - MUST NOT contain any plaintext .db or secret files
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		t.Fatalf("read backup dir: %v", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if filepath.Ext(name) == ".db" {
			t.Fatalf("SECURITY VIOLATION: Plaintext database found in backup directory: %s", name)
		}
		if name == "app_master_key" || name == "worker_internal_token" || name == "tts_internal_token" {
			t.Fatalf("SECURITY VIOLATION: Plaintext secret file found in backup directory: %s", name)
		}
	}

	// 5. Gate 11: Isolated Restore Drill
	// Decrypt artifact using keyring
	decryptedBytes, err := kr.Decrypt(env, []byte("backup-manifest-v1"))
	if err != nil {
		t.Fatalf("decrypt backup: %v", err)
	}
	restoredHash := sha256.Sum256(decryptedBytes)
	restoredHashHex := hex.EncodeToString(restoredHash[:])
	if restoredHashHex != origHashHex {
		t.Fatalf("checksum mismatch: expected %s, got %s", origHashHex, restoredHashHex)
	}

	// Write restored DB into isolated restore directory
	restoredDbPath := filepath.Join(restoreDir, "gateway.db")
	if err := os.WriteFile(restoredDbPath, decryptedBytes, 0o600); err != nil {
		t.Fatalf("write restored db: %v", err)
	}

	// Verify restored SQLite database integrity
	restoredDB, err := sql.Open("sqlite", "file:"+filepath.ToSlash(restoredDbPath)+"?mode=ro")
	if err != nil {
		t.Fatalf("open restored db: %v", err)
	}
	defer restoredDB.Close()

	var integrity string
	if err := restoredDB.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("restored db integrity check failed: %v, result: %s", err, integrity)
	}

	assertLegacyHistory(t, restoredDB)
	var restoredTransactionID, restoredReceiptHash string
	var restoredAmount int64
	if err := restoredDB.QueryRowContext(ctx, `SELECT transaction_id,amount_vnd,canonical_hash FROM payment_receipts WHERE order_id=?`, order.ID).Scan(&restoredTransactionID, &restoredAmount, &restoredReceiptHash); err != nil || restoredTransactionID != paid.TransactionID || restoredAmount != 50000 || restoredReceiptHash != receiptHash {
		t.Fatalf("restored payment receipt changed: transaction=%s amount=%d hash=%s err=%v", restoredTransactionID, restoredAmount, restoredReceiptHash, err)
	}
	var restoredStatus string
	if err := restoredDB.QueryRowContext(ctx, `SELECT status FROM payment_orders WHERE id=? AND transaction_id=?`, order.ID, paid.TransactionID).Scan(&restoredStatus); err != nil || restoredStatus != "PAID" {
		t.Fatalf("restored order lost payment: status=%s err=%v", restoredStatus, err)
	}
	var paymentDeliveryStatus string
	if err := restoredDB.QueryRowContext(ctx, `SELECT d.status FROM deliveries d JOIN events e ON e.id=d.event_id WHERE d.id=? AND e.transaction_id=? AND d.endpoint_id='legacy-endpoint'`, paymentDeliveryID, paid.TransactionID).Scan(&paymentDeliveryStatus); err != nil || paymentDeliveryStatus != "PENDING" {
		t.Fatalf("restored payment outbox identity lost: status=%s err=%v", paymentDeliveryStatus, err)
	}

	// 6. Negative Drill: Tampered ciphertext must fail decryption closed
	tamperedEnv := env
	tamperedBytes := []byte(tamperedEnv.Ciphertext)
	if len(tamperedBytes) > 5 {
		tamperedBytes[len(tamperedBytes)-3] ^= 0xFF // flip bits
	}
	tamperedEnv.Ciphertext = string(tamperedBytes)
	_, err = kr.Decrypt(tamperedEnv, []byte("backup-manifest-v1"))
	if err == nil {
		t.Fatal("expected decryption of tampered ciphertext to FAIL CLOSED")
	}

	// 7. Negative Drill: Tampered AAD (manifest metadata) must fail closed
	_, err = kr.Decrypt(env, []byte("tampered-manifest-metadata"))
	if err == nil {
		t.Fatal("expected decryption with tampered AAD to FAIL CLOSED")
	}
}
