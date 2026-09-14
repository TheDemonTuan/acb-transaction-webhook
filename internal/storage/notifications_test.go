package storage

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/security"
)

func setupTestStoreWithKeyring(t *testing.T) (*Store, *security.Keyring) {
	t.Helper()
	ctx := context.Background()
	keyPath := filepath.Join(t.TempDir(), "master.key")
	rawKey := make([]byte, 32)
	for i := range rawKey {
		rawKey[i] = byte(i + 1)
	}
	if err := os.WriteFile(keyPath, []byte(hex.EncodeToString(rawKey)), 0o600); err != nil {
		t.Fatal(err)
	}
	kr, err := security.LoadKeyring(keyPath)
	if err != nil {
		t.Fatal(err)
	}

	store, err := Open(ctx, filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	store.WithKeyring(kr)
	return store, kr
}

func TestBarkChannelCreationAndEncryption(t *testing.T) {
	ctx := context.Background()
	store, _ := setupTestStoreWithKeyring(t)
	defer store.Close()

	// 1. Without keyring -> fails
	noKeyStore, err := Open(ctx, filepath.Join(t.TempDir(), "nokey.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer noKeyStore.Close()

	_, err = noKeyStore.CreateBarkChannel(ctx, "iPhone Without Key", "test_key_123", nil)
	if err == nil || !strings.Contains(err.Error(), "keyring required") {
		t.Fatalf("expected error without keyring, got: %v", err)
	}

	// 2. With keyring -> succeeds
	cfg := DefaultBarkConfig()
	cfg.Group = "Finance"
	cfg.Level = "timeSensitive"
	cfg.Sound = "bell"
	cfg.IncludeBalance = true
	cfg.IncludeDescription = false

	ch, err := store.CreateBarkChannel(ctx, "Tuan iPhone", "secret_device_key_xyz", &cfg)
	if err != nil {
		t.Fatalf("CreateBarkChannel failed: %v", err)
	}
	if ch.ID == "" || ch.Provider != "BARK" || ch.Status != "DISABLED" || ch.Revision != 1 || !ch.HasDeviceKey || ch.Secret != "" {
		t.Fatalf("unexpected channel: %+v", ch)
	}
	if ch.BarkConfig == nil || ch.BarkConfig.Group != "Finance" || !ch.BarkConfig.IncludeBalance || ch.BarkConfig.IncludeDescription {
		t.Fatalf("unexpected bark config: %+v", ch.BarkConfig)
	}

	// 3. Verify device key is NOT stored in plaintext anywhere in the database
	var envelopeBlob []byte
	err = store.DB().QueryRowContext(ctx, `
		SELECT envelope FROM endpoint_secrets WHERE endpoint_id = ? AND status = 'ACTIVE'
	`, ch.ID).Scan(&envelopeBlob)
	if err != nil {
		t.Fatalf("query envelope failed: %v", err)
	}
	if strings.Contains(string(envelopeBlob), "secret_device_key_xyz") {
		t.Fatalf("device key was found in plaintext in database envelope!")
	}

	// 4. Read back via DeliveryTargetForDelivery
	delivery := Delivery{
		ID:               "del_test",
		EndpointID:       ch.ID,
		EndpointRevision: 1,
		KeyID:            "k1",
	}
	target, err := store.DeliveryTargetForDelivery(ctx, delivery)
	if err != nil {
		t.Fatalf("DeliveryTargetForDelivery failed: %v", err)
	}
	if target.Provider != "BARK" || string(target.Secret) != "secret_device_key_xyz" {
		t.Fatalf("target mismatch: provider=%s secret=%s", target.Provider, string(target.Secret))
	}
	if target.BarkConfig.Group != "Finance" || target.BarkConfig.Level != "timeSensitive" {
		t.Fatalf("target config mismatch: %+v", target.BarkConfig)
	}
}

func TestBarkChannelUpdateAndRevisionConflict(t *testing.T) {
	ctx := context.Background()
	store, _ := setupTestStoreWithKeyring(t)
	defer store.Close()

	ch, err := store.CreateBarkChannel(ctx, "Work Phone", "work_key_999", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Stale revision -> conflict
	_, err = store.UpdateChannel(ctx, ch.ID, 0, "Work Phone Updated", "", nil)
	if err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("expected revision conflict, got: %v", err)
	}

	// Valid update -> revision 2
	newCfg := DefaultBarkConfig()
	newCfg.Sound = "minuet"
	updated, err := store.UpdateChannel(ctx, ch.ID, 1, "Work Phone Renamed", "", &newCfg)
	if err != nil {
		t.Fatalf("UpdateChannel failed: %v", err)
	}
	if updated.Revision != 2 || updated.Name != "Work Phone Renamed" || updated.BarkConfig.Sound != "minuet" {
		t.Fatalf("unexpected updated channel: %+v", updated)
	}

	// Query by ID
	fetched, err := store.NotificationChannelByID(ctx, ch.ID)
	if err != nil {
		t.Fatalf("NotificationChannelByID failed: %v", err)
	}
	if fetched.Revision != 2 || fetched.BarkConfig.Sound != "minuet" {
		t.Fatalf("fetched mismatch: %+v", fetched)
	}
}

func TestBarkDeviceKeyRotation(t *testing.T) {
	ctx := context.Background()
	store, _ := setupTestStoreWithKeyring(t)
	defer store.Close()

	ch, err := store.CreateBarkChannel(ctx, "Personal Phone", "old_key_111", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Rotate key
	secretRet, err := store.RotateSecret(ctx, ch.ID, "new_key_222")
	if err != nil {
		t.Fatalf("RotateSecret failed: %v", err)
	}
	if secretRet != "" {
		t.Fatalf("RotateSecret for Bark must NOT return secret, got: %s", secretRet)
	}

	// Check that there is 1 active and 1 retired secret
	var activeCount, retiredCount int
	var activeKeyID string
	_ = store.DB().QueryRowContext(ctx, `SELECT count(*), max(key_id) FROM endpoint_secrets WHERE endpoint_id = ? AND status = 'ACTIVE'`, ch.ID).Scan(&activeCount, &activeKeyID)
	_ = store.DB().QueryRowContext(ctx, `SELECT count(*) FROM endpoint_secrets WHERE endpoint_id = ? AND status = 'RETIRED'`, ch.ID).Scan(&retiredCount)

	if activeCount != 1 || retiredCount != 1 {
		t.Fatalf("expected 1 active and 1 retired secret, got active=%d retired=%d", activeCount, retiredCount)
	}

	// Delivery target with old key_id decrypts old key
	oldTarget, err := store.DeliveryTargetForDelivery(ctx, Delivery{
		EndpointID:       ch.ID,
		EndpointRevision: 1,
		KeyID:            "k1",
	})
	if err != nil || string(oldTarget.Secret) != "old_key_111" {
		t.Fatalf("old target decryption failed: %s %v", string(oldTarget.Secret), err)
	}

	// Delivery target with new key_id decrypts new key
	newTarget, err := store.DeliveryTargetForDelivery(ctx, Delivery{
		EndpointID:       ch.ID,
		EndpointRevision: 1,
		KeyID:            activeKeyID,
	})
	if err != nil || string(newTarget.Secret) != "new_key_222" {
		t.Fatalf("new target decryption failed: %s %v", string(newTarget.Secret), err)
	}
}

func TestWebhookSecretRotation(t *testing.T) {
	ctx := context.Background()
	store, _ := setupTestStoreWithKeyring(t)
	defer store.Close()

	ep, err := store.CreateEndpointWithSecret(ctx, "Webhook Service", "https://api.example.com/webhook")
	if err != nil {
		t.Fatal(err)
	}

	newSecret, err := store.RotateSecret(ctx, ep.ID, "")
	if err != nil {
		t.Fatalf("RotateSecret failed: %v", err)
	}
	if newSecret == "" || newSecret == ep.Secret {
		t.Fatalf("expected new secret hex, got: %s", newSecret)
	}

	// Verify new secret is returned by DeliveryTargetForDelivery
	target, err := store.DeliveryTargetForDelivery(ctx, Delivery{
		EndpointID:       ep.ID,
		EndpointRevision: 1,
	})
	if err != nil || string(target.Secret) != newSecret {
		t.Fatalf("target secret mismatch: expected=%s got=%s err=%v", newSecret, string(target.Secret), err)
	}
}

func TestReplayDeliveryGating(t *testing.T) {
	ctx := context.Background()
	store, _ := setupTestStoreWithKeyring(t)
	defer store.Close()

	ch, err := store.CreateBarkChannel(ctx, "Alert Phone", "alert_key_001", nil)
	if err != nil {
		t.Fatal(err)
	}

	nowAt := time.Now().UTC().Truncate(time.Second)
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO events(id, event_type, payload, payload_hash, created_at)
		VALUES('evt_rp', 'bank.transaction.credit', X'7B7D', 'hash', ?)
	`, now())
	if err != nil {
		t.Fatal(err)
	}

	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO deliveries(id, event_id, endpoint_id, endpoint_revision, key_id, status, attempts, next_attempt_at, created_at, updated_at)
		VALUES('del_rp', 'evt_rp', ?, 1, 'k1', 'PENDING', 2, ?, ?, ?)
	`, ch.ID, nowAt.Format(time.RFC3339Nano), now(), now())
	if err != nil {
		t.Fatal(err)
	}

	// 1. Replay when status is PENDING -> ErrDeliveryNotDeadLetter
	err = store.ReplayDelivery(ctx, "del_rp")
	if err != ErrDeliveryNotDeadLetter {
		t.Fatalf("expected ErrDeliveryNotDeadLetter, got: %v", err)
	}

	// 2. Set to DEAD_LETTER, but channel is DISABLED -> ErrEndpointNotActive
	_, _ = store.DB().ExecContext(ctx, `UPDATE deliveries SET status = 'DEAD_LETTER' WHERE id = 'del_rp'`)
	err = store.ReplayDelivery(ctx, "del_rp")
	if err != ErrEndpointNotActive {
		t.Fatalf("expected ErrEndpointNotActive, got: %v", err)
	}

	// 3. Enable channel -> Replay succeeds
	_ = store.SetEndpointStatus(ctx, ch.ID, "ACTIVE")
	err = store.ReplayDelivery(ctx, "del_rp")
	if err != nil {
		t.Fatalf("ReplayDelivery failed: %v", err)
	}

	// Verify status is PENDING and retry_cycle_start_attempt == 2
	var status string
	var attempts, startAttempt int
	err = store.DB().QueryRowContext(ctx, `
		SELECT status, attempts, retry_cycle_start_attempt FROM deliveries WHERE id = 'del_rp'
	`).Scan(&status, &attempts, &startAttempt)
	if err != nil {
		t.Fatal(err)
	}
	if status != "PENDING" || attempts != 2 || startAttempt != 2 {
		t.Fatalf("unexpected state after replay: status=%s attempts=%d startAttempt=%d", status, attempts, startAttempt)
	}
}

func TestMixedProviderFanOut(t *testing.T) {
	ctx := context.Background()
	store, _ := setupTestStoreWithKeyring(t)
	defer store.Close()

	conn, _ := store.ConfigureConnection(ctx, "***9999")
	_, _ = store.DB().ExecContext(ctx, `UPDATE connections SET state = 'MONITORING' WHERE id = ?`, conn.ID)

	// 1 webhook channel, 2 Bark channels
	wh, err := store.CreateEndpointWithSecret(ctx, "Webhook 1", "https://hook.example.com")
	if err != nil {
		t.Fatal(err)
	}
	_ = store.SetEndpointStatus(ctx, wh.ID, "ACTIVE")

	bark1, err := store.CreateBarkChannel(ctx, "iPhone 1", "key_iphone_1", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = store.SetEndpointStatus(ctx, bark1.ID, "ACTIVE")

	bark2, err := store.CreateBarkChannel(ctx, "iPhone 2", "key_iphone_2", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = store.SetEndpointStatus(ctx, bark2.ID, "ACTIVE")

	// Ingest a credit transaction
	res, err := store.IngestTransactionsBatch(ctx, conn.ID, conn.Generation, "***9999", []BatchTransactionItem{
		{
			Number:        "TXN_MIX_1",
			TransactionAt: "2026-09-13T10:00:00Z",
			Credit:        750000,
			Debit:         0,
			Description:   "Chuyen khoan mua hang",
		},
	}, false)
	if err != nil {
		t.Fatalf("IngestTransactionsBatch failed: %v", err)
	}
	if res.InsertedCount != 1 {
		t.Fatalf("expected 1 inserted txn, got: %d", res.InsertedCount)
	}

	// Verify exactly 3 deliveries created (1 webhook + 2 bark) for 1 credit event
	var delivCount int
	err = store.DB().QueryRowContext(ctx, `SELECT count(*) FROM deliveries`).Scan(&delivCount)
	if err != nil || delivCount != 3 {
		t.Fatalf("expected 3 deliveries for 3 active channels, got: %d (err=%v)", delivCount, err)
	}

	// Verify each delivery has the right endpoint_id and provider
	rows, err := store.DB().QueryContext(ctx, `
		SELECT d.endpoint_id, COALESCE(e.provider, 'WEBHOOK')
		FROM deliveries d
		JOIN webhook_endpoints e ON e.id = d.endpoint_id
	`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	providerCounts := make(map[string]int)
	for rows.Next() {
		var epID, prov string
		if err := rows.Scan(&epID, &prov); err != nil {
			t.Fatal(err)
		}
		providerCounts[prov]++
	}
	if providerCounts["WEBHOOK"] != 1 || providerCounts["BARK"] != 2 {
		t.Fatalf("unexpected provider delivery counts: %+v", providerCounts)
	}
}

func TestBarkChannelCreateReadDetailed(t *testing.T) {
	ctx := context.Background()
	store, _ := setupTestStoreWithKeyring(t)
	defer store.Close()

	cfg := DefaultBarkConfig()
	cfg.Group = "Finance"
	cfg.Level = "timeSensitive"
	cfg.Sound = "bell"
	cfg.IncludeBalance = true
	cfg.IncludeDescription = true

	deviceKey := "device_token_alice_12345"
	ch, err := store.CreateBarkChannel(ctx, "iPhone Alice", deviceKey, &cfg)
	if err != nil {
		t.Fatalf("CreateBarkChannel failed: %v", err)
	}
	if !strings.HasPrefix(ch.ID, "ch_bark") {
		t.Fatalf("expected prefix ch_bark, got: %s", ch.ID)
	}
	if !ch.HasDeviceKey {
		t.Fatal("expected HasDeviceKey to be true")
	}
	if ch.Secret != "" {
		t.Fatalf("channel creation must not return plaintext secret, got: %s", ch.Secret)
	}

	// Read via DeliveryTargetForDelivery with explicit KeyID "k1"
	targetWithKeyID, err := store.DeliveryTargetForDelivery(ctx, Delivery{
		EndpointID:       ch.ID,
		EndpointRevision: ch.Revision,
		KeyID:            "k1",
	})
	if err != nil {
		t.Fatalf("DeliveryTargetForDelivery with KeyID failed: %v", err)
	}
	if targetWithKeyID.Provider != "BARK" {
		t.Fatalf("expected provider BARK, got: %s", targetWithKeyID.Provider)
	}
	if string(targetWithKeyID.Secret) != deviceKey {
		t.Fatalf("decrypted device key mismatch: got %q, want %q", string(targetWithKeyID.Secret), deviceKey)
	}
	if targetWithKeyID.KeyID != "k1" {
		t.Fatalf("expected KeyID k1, got: %s", targetWithKeyID.KeyID)
	}

	// Read via DeliveryTargetForDelivery with empty KeyID (resolves to active secret)
	targetActive, err := store.DeliveryTargetForDelivery(ctx, Delivery{
		EndpointID:       ch.ID,
		EndpointRevision: ch.Revision,
	})
	if err != nil {
		t.Fatalf("DeliveryTargetForDelivery with empty KeyID failed: %v", err)
	}
	if string(targetActive.Secret) != deviceKey {
		t.Fatalf("decrypted active device key mismatch: got %q, want %q", string(targetActive.Secret), deviceKey)
	}
}

func TestBarkRotationOldDeliverySnapshotVsNewActive(t *testing.T) {
	ctx := context.Background()
	store, _ := setupTestStoreWithKeyring(t)
	defer store.Close()

	oldKey := "bark_device_key_snapshot_v1"
	ch, err := store.CreateBarkChannel(ctx, "Snapshot Phone", oldKey, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Delivery 1 references the initial secret snapshot (key_id = "k1")
	deliv1 := Delivery{
		ID:               "del_snapshot_old",
		EndpointID:       ch.ID,
		EndpointRevision: 1,
		KeyID:            "k1",
	}

	// Rotate channel secret to new active key
	newKey := "bark_device_key_snapshot_v2"
	secretRet, err := store.RotateSecret(ctx, ch.ID, newKey)
	if err != nil {
		t.Fatalf("RotateSecret failed: %v", err)
	}
	if secretRet != "" {
		t.Fatalf("RotateSecret for Bark must return empty string, got: %s", secretRet)
	}

	// Get active key_id
	var activeKeyID string
	err = store.DB().QueryRowContext(ctx, `SELECT key_id FROM endpoint_secrets WHERE endpoint_id = ? AND status = 'ACTIVE'`, ch.ID).Scan(&activeKeyID)
	if err != nil {
		t.Fatal(err)
	}
	if activeKeyID == "k1" || activeKeyID == "" {
		t.Fatalf("expected new active key_id distinct from k1, got: %s", activeKeyID)
	}

	// Delivery 2 references the new active secret
	deliv2 := Delivery{
		ID:               "del_snapshot_new",
		EndpointID:       ch.ID,
		EndpointRevision: 1,
		KeyID:            activeKeyID,
	}

	// 1. Old delivery target STILL decrypts the old key according to snapshot semantics
	targetOld, err := store.DeliveryTargetForDelivery(ctx, deliv1)
	if err != nil {
		t.Fatalf("DeliveryTargetForDelivery for old delivery snapshot failed: %v", err)
	}
	if got := string(targetOld.Secret); got != oldKey {
		t.Fatalf("old delivery snapshot decrypted = %q, want %q", got, oldKey)
	}
	if targetOld.KeyID != "k1" {
		t.Fatalf("old target KeyID = %s, want k1", targetOld.KeyID)
	}

	// 2. New delivery target decrypts the new key
	targetNew, err := store.DeliveryTargetForDelivery(ctx, deliv2)
	if err != nil {
		t.Fatalf("DeliveryTargetForDelivery for new delivery failed: %v", err)
	}
	if got := string(targetNew.Secret); got != newKey {
		t.Fatalf("new delivery decrypted = %q, want %q", got, newKey)
	}
	if targetNew.KeyID != activeKeyID {
		t.Fatalf("new target KeyID = %s, want %s", targetNew.KeyID, activeKeyID)
	}

	// 3. Delivery target without explicit KeyID automatically resolves to new active key
	targetDefaultActive, err := store.DeliveryTargetForDelivery(ctx, Delivery{
		EndpointID:       ch.ID,
		EndpointRevision: 1,
	})
	if err != nil {
		t.Fatalf("DeliveryTargetForDelivery default active failed: %v", err)
	}
	if got := string(targetDefaultActive.Secret); got != newKey {
		t.Fatalf("default active delivery decrypted = %q, want %q", got, newKey)
	}
}

func TestBarkReopenDatabaseSameAndWrongKey(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "reopen_test.db")

	rawKeyA := make([]byte, 32)
	for i := range rawKeyA {
		rawKeyA[i] = byte(i + 1)
	}
	keyPathA := filepath.Join(tempDir, "masterA.key")
	if err := os.WriteFile(keyPathA, []byte(hex.EncodeToString(rawKeyA)), 0o600); err != nil {
		t.Fatal(err)
	}
	krA, err := security.LoadKeyring(keyPathA)
	if err != nil {
		t.Fatal(err)
	}

	rawKeyB := make([]byte, 32)
	for i := range rawKeyB {
		rawKeyB[i] = byte(255 - i)
	}
	keyPathB := filepath.Join(tempDir, "masterB.key")
	if err := os.WriteFile(keyPathB, []byte(hex.EncodeToString(rawKeyB)), 0o600); err != nil {
		t.Fatal(err)
	}
	krB, err := security.LoadKeyring(keyPathB)
	if err != nil {
		t.Fatal(err)
	}

	secretDeviceKey := "bark_secret_reopen_drill_token_999"
	var channelID string

	// Phase 1: Initialize database with Key A and create Bark channel
	{
		store1, err := Open(ctx, dbPath)
		if err != nil {
			t.Fatal(err)
		}
		store1.WithKeyring(krA)
		ch, err := store1.CreateBarkChannel(ctx, "Reopen Phone", secretDeviceKey, nil)
		if err != nil {
			store1.Close()
			t.Fatalf("CreateBarkChannel failed: %v", err)
		}
		channelID = ch.ID
		if err := store1.Close(); err != nil {
			t.Fatal(err)
		}
	}

	// Phase 2: Reopen with the SAME Key A -> Decryption must succeed
	{
		storeSameKey, err := Open(ctx, dbPath)
		if err != nil {
			t.Fatal(err)
		}
		storeSameKey.WithKeyring(krA)

		target, err := storeSameKey.DeliveryTargetForDelivery(ctx, Delivery{
			EndpointID:       channelID,
			EndpointRevision: 1,
		})
		if err != nil {
			storeSameKey.Close()
			t.Fatalf("reopen with same key failed to decrypt: %v", err)
		}
		if got := string(target.Secret); got != secretDeviceKey {
			storeSameKey.Close()
			t.Fatalf("decrypted secret = %q, want %q", got, secretDeviceKey)
		}
		if err := storeSameKey.Close(); err != nil {
			t.Fatal(err)
		}
	}

	// Phase 3: Reopen with WRONG Key B -> Decryption must fail with typed sentinel errors
	{
		storeWrongKey, err := Open(ctx, dbPath)
		if err != nil {
			t.Fatal(err)
		}
		storeWrongKey.WithKeyring(krB)

		_, err = storeWrongKey.DeliveryTargetForDelivery(ctx, Delivery{
			EndpointID:       channelID,
			EndpointRevision: 1,
		})
		if err == nil {
			storeWrongKey.Close()
			t.Fatal("expected decryption failure when opening database with wrong master key")
		}

		if !errors.Is(err, ErrBarkDecryptionFailed) {
			t.Fatalf("expected ErrBarkDecryptionFailed, got: %v", err)
		}
		if !errors.Is(err, security.ErrAuthenticationFailed) {
			t.Fatalf("expected security.ErrAuthenticationFailed, got: %v", err)
		}

		// Ensure error message does NOT leak the plaintext secret
		if strings.Contains(err.Error(), secretDeviceKey) {
			t.Fatalf("error message leaked plaintext secret: %s", err.Error())
		}
		if err := storeWrongKey.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBarkNoPlaintextLeak(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "no_leak.db")

	keyPath := filepath.Join(tempDir, "master.key")
	rawKey := make([]byte, 32)
	for i := range rawKey {
		rawKey[i] = byte(i + 42)
	}
	if err := os.WriteFile(keyPath, []byte(hex.EncodeToString(rawKey)), 0o600); err != nil {
		t.Fatal(err)
	}
	kr, err := security.LoadKeyring(keyPath)
	if err != nil {
		t.Fatal(err)
	}

	store, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.WithKeyring(kr)

	leakSecret1 := "super_confidential_bark_key_never_leak_99999"
	leakSecret2 := "super_confidential_bark_key_never_leak_88888"

	// 1. Create channel
	ch, err := store.CreateBarkChannel(ctx, "Private iPhone", leakSecret1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ch.Secret != "" {
		t.Fatalf("CreateBarkChannel returned non-empty Secret: %q", ch.Secret)
	}

	// 2. Rotate channel
	rotRet, err := store.RotateSecret(ctx, ch.ID, leakSecret2)
	if err != nil {
		t.Fatal(err)
	}
	if rotRet != "" {
		t.Fatalf("RotateSecret returned non-empty string: %q", rotRet)
	}

	// 3. List and get channel
	channels, err := store.NotificationChannels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range channels {
		if c.Secret != "" {
			t.Fatalf("NotificationChannels leaked secret: %q", c.Secret)
		}
	}
	singleCh, err := store.NotificationChannelByID(ctx, ch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if singleCh.Secret != "" {
		t.Fatalf("NotificationChannelByID leaked secret: %q", singleCh.Secret)
	}

	// 4. Inspect all tables in DB
	tables := []string{"webhook_endpoints", "endpoint_secrets", "endpoint_versions"}
	for _, table := range tables {
		rows, err := store.DB().QueryContext(ctx, fmt.Sprintf("SELECT * FROM %s", table))
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		for rows.Next() {
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			for _, val := range vals {
				var strVal string
				switch v := val.(type) {
				case []byte:
					strVal = string(v)
				case string:
					strVal = v
				}
				if strings.Contains(strVal, leakSecret1) {
					t.Fatalf("plaintext leakSecret1 found in table %s", table)
				}
				if strings.Contains(strVal, leakSecret2) {
					t.Fatalf("plaintext leakSecret2 found in table %s", table)
				}
			}
		}
		rows.Close()
	}

	// 5. Read SQLite binary file bytes from disk
	fileBytes, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(fileBytes), leakSecret1) {
		t.Fatalf("leakSecret1 found in SQLite database binary file!")
	}
	if strings.Contains(string(fileBytes), leakSecret2) {
		t.Fatalf("leakSecret2 found in SQLite database binary file!")
	}
}
