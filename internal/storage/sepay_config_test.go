package storage

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSePayManagedConfigEncryptionCASAndCrossHandle(t *testing.T) {
	ctx := context.Background()
	first, keyring := setupTestStoreWithKeyring(t)
	defer first.Close()
	var path string
	if err := first.DB().QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	second, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	second.WithKeyring(keyring)
	if _, found, err := second.SePayManagedConfig(ctx); err != nil || found {
		t.Fatalf("fresh config: %v %v", found, err)
	}
	config := SePayManagedConfig{ConfigJSON: json.RawMessage(`{"private":"receiver-secret-567"}`), BotToken: "987654:token-encrypted-only-abcdefghi"}
	next, err := first.SaveSePayManagedConfig(ctx, config, "store", "VCB", "TEST123", PaymentProviderActor{Subject: "owner", Role: "OWNER", RequestID: "config-save"})
	if err != nil || next != 1 {
		t.Fatalf("save: revision=%d err=%v", next, err)
	}
	record, found, err := second.SePayManagedConfig(ctx)
	if err != nil || !found || record.Revision != 1 || record.BotToken != config.BotToken || string(record.ConfigJSON) != string(config.ConfigJSON) {
		t.Fatalf("another handle did not read committed config: found=%v err=%v", found, err)
	}
	var encrypted []byte
	var audit string
	if err := first.DB().QueryRow(`SELECT config_envelope FROM sepay_managed_config`).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	if err := first.DB().QueryRow(`SELECT details_json FROM audit_logs WHERE request_id='config-save'`).Scan(&audit); err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{config.BotToken, "receiver-secret-567"} {
		if strings.Contains(string(encrypted)+audit, private) {
			t.Fatal("plaintext escaped encrypted record")
		}
	}
	if _, err := second.SaveSePayManagedConfig(ctx, config, "store", "VCB", "TEST123", PaymentProviderActor{}); !errors.Is(err, ErrSePayRevisionConflict) {
		t.Fatalf("stale CAS accepted: %v", err)
	}
	record.BotToken = "rotated-token"
	if _, err := second.SaveSePayManagedConfig(ctx, record, "store", "VCB", "TEST123", PaymentProviderActor{}); err != nil {
		t.Fatal(err)
	}
	latest, _, err := first.SePayManagedConfig(ctx)
	if err != nil || latest.Revision != 2 || latest.BotToken != "rotated-token" {
		t.Fatal("first handle cached old credentials")
	}
}

func TestSePayManagedRevisionPinsMoneyAndLegacyFallback(t *testing.T) {
	ctx := context.Background()
	s := sepayTestStore(t)
	input := sepayTestInput(1, 1, "revision-guard")
	cfg := SePayManagedConfig{ConfigJSON: json.RawMessage(`{}`), BotToken: "private"}
	if _, err := s.SaveSePayManagedConfig(ctx, cfg, input.StoreKey, input.BankCode, input.AccountNumber, PaymentProviderActor{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.IngestSePayNotification(ctx, input); !errors.Is(err, ErrSePayRevisionConflict) {
		t.Fatalf("legacy request committed after managed save: %v", err)
	}
	sepayTestCount(t, s, "sepay_telegram_inbox", 0)
	input.ConfigRevision = 1
	if _, err := s.IngestSePayNotification(ctx, input); err != nil {
		t.Fatal(err)
	}
	cfg.Revision = 1
	if _, err := s.SaveSePayManagedConfig(ctx, cfg, input.StoreKey, "OTHER", input.AccountNumber, PaymentProviderActor{}); !errors.Is(err, ErrSePayReceiverMismatch) {
		t.Fatalf("receiver relabelled: %v", err)
	}
	if _, err := s.SaveSePayManagedConfig(ctx, cfg, input.StoreKey, input.BankCode, input.AccountNumber, PaymentProviderActor{}); err != nil {
		t.Fatal(err)
	}
	input.UpdateID, input.MessageID = 2, 2
	input.Credit.Reference = "new-money-under-old-revision"
	sepayTestPayload(&input, "original source")
	if _, err := s.IngestSePayNotification(ctx, input); !errors.Is(err, ErrSePayRevisionConflict) {
		t.Fatalf("obsolete revision committed: %v", err)
	}
	sepayTestCount(t, s, "transactions", 1)
	sepayTestCount(t, s, "sepay_telegram_inbox", 1)
}

func TestSePayManagedConfigGateAndAuditRollback(t *testing.T) {
	ctx := context.Background()
	s := sepayTestStore(t)
	cfg := SePayManagedConfig{ConfigJSON: json.RawMessage(`{}`)}
	gate, err := s.AcquireMutationGate(ctx, "sepay-config-test", time.Minute, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveSePayManagedConfig(ctx, cfg, "", "", "", PaymentProviderActor{}); !errors.Is(err, ErrMutationGateLocked) {
		t.Fatalf("mutation gate bypass: %v", err)
	}
	if err := s.ReleaseMutationGate(ctx, "sepay-config-test", gate.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`CREATE TRIGGER fail_sepay_audit BEFORE INSERT ON audit_logs BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveSePayManagedConfig(ctx, cfg, "", "", "", PaymentProviderActor{}); err == nil {
		t.Fatal("save ignored audit failure")
	}
	if _, found, err := s.SePayManagedConfig(ctx); err != nil || found {
		t.Fatal("failed audit left managed record")
	}
	plain, err := Open(ctx, filepath.Join(t.TempDir(), "no-key.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	if _, err := plain.SaveSePayManagedConfig(ctx, cfg, "", "", "", PaymentProviderActor{}); !errors.Is(err, ErrSePayConfigUnavailable) {
		t.Fatalf("saved secret without keyring: %v", err)
	}
}
