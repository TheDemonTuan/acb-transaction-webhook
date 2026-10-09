package storage

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func providerTestKeys() PaymentProviderCredentials {
	return PaymentProviderCredentials{ClientID: "private-provider-channel-887", APIKey: "private-provider-api-key-889", ChecksumKey: "private-provider-checksum-991"}
}

func TestPaymentProviderConfigEncryptedAndDurable(t *testing.T) {
	ctx := context.Background()
	store, keyring := setupTestStoreWithKeyring(t)
	defer store.Close()
	initial, err := store.PaymentProviderConfig(ctx)
	if err != nil || initial.Revision != 0 || initial.Enabled || initial.WebhookConfirmed || initial.Credentials.ClientID != "" {
		t.Fatalf("initial config: %+v %v", initial, err)
	}
	keys := providerTestKeys()
	saved, err := store.SavePaymentProviderConfig(ctx, keys, true, PaymentProviderActor{Subject: "owner", Role: "OWNER", RequestID: "save-1"})
	if err != nil {
		t.Fatal(err)
	}
	if saved.Revision != 1 || !saved.Enabled || saved.WebhookConfirmed || saved.Credentials != keys {
		t.Fatal("saved configuration does not match")
	}
	var envelope, history, audit string
	if err := store.DB().QueryRowContext(ctx, `SELECT credentials_envelope FROM payment_provider_config WHERE id=1`).Scan(&envelope); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT credentials_envelope FROM payment_provider_credential_versions WHERE revision=1`).Scan(&history); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT details_json FROM audit_logs WHERE request_id='save-1'`).Scan(&audit); err != nil {
		t.Fatal(err)
	}
	marshalled, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{keys.ClientID, keys.APIKey, keys.ChecksumKey} {
		if strings.Contains(envelope+history+audit+string(marshalled), secret) {
			t.Fatal("credential escaped encrypted storage or internal JSON boundary")
		}
	}
	var path string
	rows, err := store.DB().QueryContext(ctx, `PRAGMA database_list`)
	if err != nil {
		t.Fatal(err)
	}
	if rows.Next() {
		var seq int
		var name string
		if err := rows.Scan(&seq, &name, &path); err != nil {
			t.Fatal(err)
		}
	}
	rows.Close()
	second, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	second.WithKeyring(keyring)
	reloaded, err := second.PaymentProviderConfig(ctx)
	if err != nil || reloaded.Credentials != keys || reloaded.Revision != saved.Revision {
		t.Fatal("another process did not load encrypted configuration")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reopened.WithKeyring(keyring)
	restarted, err := reopened.PaymentProviderConfig(ctx)
	if err != nil || restarted.Credentials != keys {
		t.Fatal("credentials did not survive restart")
	}
}

func TestPaymentProviderConfigKeysAndChannelSafety(t *testing.T) {
	ctx := context.Background()
	store, _ := setupTestStoreWithKeyring(t)
	defer store.Close()
	keys := providerTestKeys()
	for _, invalid := range []PaymentProviderCredentials{{ClientID: keys.ClientID}, {ClientID: keys.ClientID, APIKey: keys.APIKey}, {ClientID: keys.ClientID, ChecksumKey: keys.ChecksumKey}} {
		if _, err := store.SavePaymentProviderConfig(ctx, invalid, true, PaymentProviderActor{}); !errors.Is(err, ErrProviderConfigInvalid) {
			t.Fatalf("initial missing key allowed: %v", err)
		}
	}
	saved, err := store.SavePaymentProviderConfig(ctx, keys, true, PaymentProviderActor{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ConfirmPaymentProviderWebhook(ctx, saved.Revision, PaymentProviderActor{}); err != nil {
		t.Fatal(err)
	}
	preserved, err := store.SavePaymentProviderConfig(ctx, PaymentProviderCredentials{ClientID: keys.ClientID}, false, PaymentProviderActor{})
	if err != nil || preserved.Credentials != keys || !preserved.WebhookConfirmed || preserved.Enabled {
		t.Fatal("blank same-channel keys were not preserved")
	}
	changed := keys
	changed.ChecksumKey = "new-private-checksum-778"
	rotated, err := store.SavePaymentProviderConfig(ctx, changed, true, PaymentProviderActor{})
	if err != nil || rotated.WebhookConfirmed {
		t.Fatalf("rotation did not clear confirmation: %v", err)
	}
	previous, err := store.PaymentProviderVerificationKeys(ctx, keys.ClientID, rotated.Revision)
	if err != nil || len(previous) != 1 || previous[0] != keys {
		t.Fatal("old verification credentials lost after rotation")
	}
	if _, _, err := store.ReservePaymentOrder(ctx, PaymentOrderIntent{ChannelID: keys.ClientID, IdempotencyKey: "provider-test-intent", RequestHash: "test", AmountVnd: 50000, Origin: "STATIC_URL"}); err != nil {
		t.Fatal(err)
	}
	other := keys
	other.ClientID = "other-private-channel"
	if _, err := store.SavePaymentProviderConfig(ctx, other, true, PaymentProviderActor{}); !errors.Is(err, ErrProviderChannelLocked) {
		t.Fatalf("issued orders allowed channel replacement: %v", err)
	}
	if _, err := store.SavePaymentProviderConfig(ctx, changed, true, PaymentProviderActor{}); err != nil {
		t.Fatalf("same-channel save blocked by historical orders: %v", err)
	}
}

func TestPaymentProviderOperationRevisionAndSharedGate(t *testing.T) {
	ctx := context.Background()
	store, keyring := setupTestStoreWithKeyring(t)
	defer store.Close()
	keys := providerTestKeys()
	saved, err := store.SavePaymentProviderConfig(ctx, keys, true, PaymentProviderActor{})
	if err != nil {
		t.Fatal(err)
	}
	var seq int
	var name, path string
	if err := store.DB().QueryRowContext(ctx, `PRAGMA database_list`).Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	other, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	other.WithKeyring(keyring)
	pinned, token, err := other.BeginPaymentProviderOperation(ctx)
	if err != nil || pinned.Credentials != keys || pinned.Revision != saved.Revision {
		t.Fatal("operation was not pinned")
	}
	rotated := keys
	rotated.APIKey = "rotated-private-api-key"
	if _, err := store.SavePaymentProviderConfig(ctx, rotated, true, PaymentProviderActor{}); !errors.Is(err, ErrProviderConfigBusy) {
		t.Fatalf("cross-process active keys changed: %v", err)
	}
	newer, err := store.SavePaymentProviderConfig(ctx, PaymentProviderCredentials{ClientID: keys.ClientID}, false, PaymentProviderActor{})
	if err != nil {
		t.Fatal(err)
	}
	if err := other.ConfirmPaymentProviderWebhook(ctx, pinned.Revision, PaymentProviderActor{}); !errors.Is(err, ErrProviderRevisionConflict) {
		t.Fatalf("stale confirmation accepted: %v", err)
	}
	if err := other.EndPaymentProviderOperation(ctx, token); err != nil {
		t.Fatal(err)
	}
	if err := store.ConfirmPaymentProviderWebhook(ctx, newer.Revision, PaymentProviderActor{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SavePaymentProviderConfig(ctx, rotated, true, PaymentProviderActor{}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetPaymentProviderQuiesced(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := other.BeginPaymentProviderOperation(ctx); err == nil {
		t.Fatal("cross-process quiesce allowed operation")
	}
	if err := other.SetPaymentProviderQuiesced(ctx, false); err != nil {
		t.Fatal(err)
	}
	first, err := store.PaymentProviderRequestSlot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	second, err := other.PaymentProviderRequestSlot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if second.Sub(first) < time.Second {
		t.Fatal("provider rate slot not shared across processes")
	}
}

func TestPaymentProviderConfigMutationGateAndAuditAtomicity(t *testing.T) {
	ctx := context.Background()
	store, _ := setupTestStoreWithKeyring(t)
	defer store.Close()
	if _, err := store.DB().ExecContext(ctx, `CREATE TRIGGER fail_provider_audit BEFORE INSERT ON audit_logs BEGIN SELECT RAISE(ABORT,'audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SavePaymentProviderConfig(ctx, providerTestKeys(), true, PaymentProviderActor{}); err == nil {
		t.Fatal("save ignored audit failure")
	}
	saved, err := store.PaymentProviderConfig(ctx)
	if err != nil || saved.Revision != 0 {
		t.Fatal("failed audit did not roll back credentials")
	}
	if _, err := store.DB().ExecContext(ctx, `DROP TRIGGER fail_provider_audit`); err != nil {
		t.Fatal(err)
	}
	gate, err := store.AcquireMutationGate(ctx, "provider-test", time.Minute, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer store.ReleaseMutationGate(ctx, "provider-test", gate.LeaseToken)
	if _, err := store.SavePaymentProviderConfig(ctx, providerTestKeys(), true, PaymentProviderActor{}); !errors.Is(err, ErrMutationGateLocked) {
		t.Fatalf("provider save bypassed deployment gate: %v", err)
	}
}

func TestPaymentProviderConfigMissingMasterKeyFailsClosed(t *testing.T) {
	ctx := context.Background()
	store, keyring := setupTestStoreWithKeyring(t)
	defer store.Close()
	store.WithKeyring(nil)
	if _, err := store.SavePaymentProviderConfig(ctx, providerTestKeys(), true, PaymentProviderActor{}); !errors.Is(err, ErrProviderConfigUnavailable) {
		t.Fatalf("missing master key allowed plaintext save: %v", err)
	}
	store.WithKeyring(keyring)
	if _, err := store.SavePaymentProviderConfig(ctx, providerTestKeys(), true, PaymentProviderActor{}); err != nil {
		t.Fatal(err)
	}
	store.WithKeyring(nil)
	if _, err := store.PaymentProviderConfig(ctx); !errors.Is(err, ErrProviderConfigUnavailable) {
		t.Fatalf("encrypted credentials exposed without master key: %v", err)
	}
	if _, err := store.SavePaymentProviderConfig(ctx, PaymentProviderCredentials{ClientID: providerTestKeys().ClientID}, true, PaymentProviderActor{}); !errors.Is(err, ErrProviderConfigUnavailable) {
		t.Fatal("missing encryption key silently replaced credentials")
	}
	store.WithKeyring(keyring)
	saved, err := store.PaymentProviderConfig(ctx)
	if err != nil || saved.Credentials != providerTestKeys() {
		t.Fatal("failed decryption attempt destroyed saved credentials")
	}
}

func TestInitialManagedSaveCanOnlyAdoptExactIssuedChannel(t *testing.T) {
	ctx := context.Background()
	store, _ := setupTestStoreWithKeyring(t)
	defer store.Close()
	keys := providerTestKeys()
	if _, _, err := store.ReservePaymentOrder(ctx, PaymentOrderIntent{ChannelID: keys.ClientID, IdempotencyKey: "existing-issued-intent", RequestHash: "hash", AmountVnd: 50000, Origin: "STATIC_URL"}); err != nil {
		t.Fatal(err)
	}
	wrong := keys
	wrong.ClientID = "unrelated-channel"
	if _, err := store.SavePaymentProviderConfig(ctx, wrong, true, PaymentProviderActor{}); !errors.Is(err, ErrProviderChannelLocked) {
		t.Fatal("initial configuration abandoned existing channel")
	}
	if _, err := store.SavePaymentProviderConfig(ctx, keys, true, PaymentProviderActor{}); err != nil {
		t.Fatalf("exact-channel adoption blocked: %v", err)
	}
}
