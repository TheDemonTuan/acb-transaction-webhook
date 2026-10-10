package sepay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/security"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func protocolStore(t *testing.T) *storage.Store {
	t.Helper()
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "master.key")
	if err := os.WriteFile(keyPath, []byte(strings.Repeat("01", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	keyring, err := security.LoadKeyring(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(context.Background(), filepath.Join(dir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	store.WithKeyring(keyring)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func protocolCount(t *testing.T, store *storage.Store, table string, want int) {
	t.Helper()
	var count int
	if err := store.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != want {
		t.Fatalf("%s count=%d want=%d err=%v", table, count, want, err)
	}
}

func TestServiceDisabledAndAuthentication(t *testing.T) {
	cfg := protocolConfig(t)
	service := NewService(cfg, nil, nil)
	if !service.Enabled() || !service.Authenticate(cfg.WebhookSecret) {
		t.Fatal("enabled service rejected configured secret")
	}
	for _, secret := range []string{"", "wrong", cfg.WebhookSecret + "x", "x" + cfg.WebhookSecret[1:]} {
		if service.Authenticate(secret) {
			t.Fatal("wrong secret accepted")
		}
	}
	copy := service.Config()
	copy.Mode = ModeDisabled
	if service.Config().Mode != ModeActive {
		t.Fatal("config accessor leaked mutable state")
	}
	var missing *Service
	for _, unavailable := range []*Service{missing, NewService(Config{Mode: ModeDisabled}, nil, nil), service} {
		if !errors.Is(unavailable.HandleUpdate(context.Background(), TelegramUpdate{}), ErrUnavailable) {
			t.Fatal("disabled or missing storage was acknowledged")
		}
	}
	if missing.Enabled() || missing.Authenticate(cfg.WebhookSecret) || missing.Config().Mode != ModeDisabled {
		t.Fatal("nil service not disabled")
	}
}

func TestServiceCommitThenHintAndReplay(t *testing.T) {
	ctx := context.Background()
	cfg := protocolConfig(t)
	store := protocolStore(t)
	var committed []storage.EventNotification
	service := NewService(cfg, store, func(event storage.EventNotification) {
		var count int
		if err := store.DB().QueryRow(`SELECT COUNT(*) FROM sepay_receipts WHERE transaction_id=?`, event.TransactionID).Scan(&count); err != nil || count != 1 {
			t.Fatalf("hint preceded committed receipt: count=%d err=%v", count, err)
		}
		var persistedID string
		if err := store.DB().QueryRow(`SELECT transaction_id FROM sepay_telegram_inbox WHERE bot_id=? AND update_id=1`, cfg.BotID).Scan(&persistedID); err != nil || persistedID != event.TransactionID {
			t.Fatalf("hint preceded committed inbox: %v", err)
		}
		var payload map[string]any
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["provider"] != "SEPAY" || payload["credit"] != "50000" || event.EventType != "bank.transaction.credit" {
			t.Fatalf("wrong credit event: %s", event.Payload)
		}
		if bytes.Contains(event.Payload, []byte("PRIVATE")) || bytes.Contains(event.Payload, []byte("orderCode")) || bytes.Contains(event.Payload, []byte("paymentOrigin")) {
			t.Fatal("credit leaked private memo or payOS correlation")
		}
		committed = append(committed, event)
	})
	update := decodeFixture(t, telegramFixture(t, cfg, 1, 1, fixtureNotification))
	for range 10 {
		if err := service.HandleUpdate(ctx, update); err != nil {
			t.Fatal(err)
		}
	}
	if err := service.HandleUpdate(ctx, decodeFixture(t, telegramFixture(t, cfg, 2, 2, fixtureNotification))); err != nil {
		t.Fatal(err)
	}
	if len(committed) != 1 {
		t.Fatalf("replay emitted %d hints", len(committed))
	}
	protocolCount(t, store, "transactions", 1)
	protocolCount(t, store, "sepay_receipts", 1)
	protocolCount(t, store, "sepay_telegram_inbox", 2)
	protocolCount(t, store, "payment_orders", 0)
	protocolCount(t, store, "payment_receipts", 0)
	var encrypted []byte
	if err := store.DB().QueryRow(`SELECT payload_envelope FROM sepay_telegram_inbox WHERE bot_id=? AND update_id=1`, cfg.BotID).Scan(&encrypted); err != nil || bytes.Contains(encrypted, []byte("PRIVATE")) {
		t.Fatal("raw evidence missing or unencrypted")
	}
}

func TestServiceUntrustedUpdatesDoNotPersist(t *testing.T) {
	cfg := protocolConfig(t)
	store := protocolStore(t)
	service := NewService(cfg, store, func(storage.EventNotification) { t.Fatal("untrusted hint") })
	for _, field := range []string{"sender_chat", "forward_origin", "via_bot", "reply_to_message", "new_chat_members"} {
		fields := telegramFixture(t, cfg, 1, 1, fixtureNotification)
		fields["message"].(map[string]any)[field] = map[string]any{"private": "PERSONAL INFORMATION"}
		if err := service.HandleUpdate(context.Background(), decodeFixture(t, fields)); err != nil {
			t.Fatal(err)
		}
	}
	fields := telegramFixture(t, cfg, 1, 1, fixtureNotification)
	fields["message"].(map[string]any)["from"].(map[string]any)["is_bot"] = false
	if err := service.HandleUpdate(context.Background(), decodeFixture(t, fields)); err != nil {
		t.Fatal(err)
	}
	protocolCount(t, store, "sepay_telegram_inbox", 0)
	protocolCount(t, store, "transactions", 0)
	protocolCount(t, store, "transaction_quarantine", 0)
}

func TestServiceReviewObserveAndPreactivation(t *testing.T) {
	cases := []struct {
		name, text, mode, wantReason string
		edited                       bool
		preactivation                bool
	}{
		{"invalid template", "free-form transfer 50000", ModeActive, ReasonInvalidTemplate, false, false},
		{"outgoing", strings.Replace(fixtureNotification, "direction=vào", "direction=ra", 1), ModeActive, ReasonOutgoing, false, false},
		{"amount", strings.Replace(fixtureNotification, "50,000", "50.00", 1), ModeActive, ReasonInvalidAmount, false, false},
		{"date", strings.Replace(fixtureNotification, "10/10/2026 10:30:00", "10/10/2026", 1), ModeActive, ReasonInvalidTransactionDate, false, false},
		{"reference", strings.Replace(fixtureNotification, "SEPAY_TEST_001", "", 1), ModeActive, ReasonMissingReference, false, false},
		{"account", strings.Replace(fixtureNotification, "VA012345", "VA999999", 1), ModeActive, ReasonAccountMismatch, false, false},
		{"bank", strings.Replace(fixtureNotification, "Fixture Bank", "Different Bank", 1), ModeActive, ReasonAccountMismatch, false, false},
		{"edited", strings.Replace(fixtureNotification, "50,000", "120000", 1), ModeActive, ReasonEditedMessage, true, false},
		{"observe", fixtureNotification, ModeObserve, "OBSERVATION", false, false},
		{"before activation", fixtureNotification, ModeActive, "PRE_ACTIVATION", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := protocolConfig(t)
			cfg.Mode = tc.mode
			if tc.preactivation {
				cfg.ActivationAt = time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
			}
			store := protocolStore(t)
			service := NewService(cfg, store, func(storage.EventNotification) { t.Fatal("review published credit") })
			fields := telegramFixture(t, cfg, 1, 1, tc.text)
			if tc.edited {
				fields["edited_message"] = fields["message"]
				delete(fields, "message")
			}
			if err := service.HandleUpdate(context.Background(), decodeFixture(t, fields)); err != nil {
				t.Fatal(err)
			}
			var reason string
			if err := store.DB().QueryRow(`SELECT reason FROM sepay_telegram_inbox`).Scan(&reason); err != nil || reason != tc.wantReason {
				t.Fatalf("review reason=%s want=%s err=%v", reason, tc.wantReason, err)
			}
			protocolCount(t, store, "transactions", 0)
			protocolCount(t, store, "sepay_receipts", 0)
		})
	}
}

func TestServiceGateFailureNoHintThenRetry(t *testing.T) {
	ctx := context.Background()
	cfg := protocolConfig(t)
	store := protocolStore(t)
	hints := 0
	service := NewService(cfg, store, func(storage.EventNotification) { hints++ })
	gate, err := store.AcquireMutationGate(ctx, "sepay-service-test", time.Minute, "test")
	if err != nil {
		t.Fatal(err)
	}
	update := decodeFixture(t, telegramFixture(t, cfg, 1, 1, fixtureNotification))
	if err := service.HandleUpdate(ctx, update); !errors.Is(err, storage.ErrMutationGateLocked) {
		t.Fatalf("gate did not fail ingest: %v", err)
	}
	protocolCount(t, store, "sepay_telegram_inbox", 0)
	if hints != 0 {
		t.Fatal("failed ingest emitted hint")
	}
	if err := store.ReleaseMutationGate(ctx, "sepay-service-test", gate.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if err := service.HandleUpdate(ctx, update); err != nil {
		t.Fatal(err)
	}
	if hints != 1 {
		t.Fatal("retry did not publish exactly one committed hint")
	}
}

func TestServiceMissingKeyringAndDBFailureDoNotAcknowledge(t *testing.T) {
	cfg := protocolConfig(t)
	store, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(cfg, store, func(storage.EventNotification) { t.Fatal("failed ingest emitted hint") })
	update := decodeFixture(t, telegramFixture(t, cfg, 1, 1, fixtureNotification))
	if err := service.HandleUpdate(context.Background(), update); err == nil {
		t.Fatal("missing keyring was acknowledged")
	}
	protocolCount(t, store, "sepay_telegram_inbox", 0)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := service.HandleUpdate(context.Background(), update); err == nil {
		t.Fatal("closed database was acknowledged")
	}
}

func TestServiceEditedMessagePreservesFirstMoney(t *testing.T) {
	ctx := context.Background()
	cfg := protocolConfig(t)
	store := protocolStore(t)
	hints := 0
	service := NewService(cfg, store, func(storage.EventNotification) { hints++ })
	if err := service.HandleUpdate(ctx, decodeFixture(t, telegramFixture(t, cfg, 1, 1, fixtureNotification))); err != nil {
		t.Fatal(err)
	}
	fields := telegramFixture(t, cfg, 2, 1, strings.Replace(fixtureNotification, "50,000", "120000", 1))
	fields["edited_message"] = fields["message"]
	delete(fields, "message")
	if err := service.HandleUpdate(ctx, decodeFixture(t, fields)); err != nil {
		t.Fatal(err)
	}
	var credit int64
	if err := store.DB().QueryRow(`SELECT SUM(credit) FROM transactions`).Scan(&credit); err != nil || credit != 50000 {
		t.Fatalf("edit changed first credit: %d %v", credit, err)
	}
	var reason string
	if err := store.DB().QueryRow(`SELECT reason FROM sepay_telegram_inbox WHERE update_id=2`).Scan(&reason); err != nil || reason != ReasonEditedMessage {
		t.Fatalf("edit not reviewed: %s %v", reason, err)
	}
	if hints != 1 {
		t.Fatal("edit produced another money hint")
	}
	protocolCount(t, store, "sepay_receipts", 1)
}

func TestServiceFailedCommitRollsBackBeforeHint(t *testing.T) {
	ctx := context.Background()
	cfg := protocolConfig(t)
	store := protocolStore(t)
	hints := 0
	service := NewService(cfg, store, func(storage.EventNotification) { hints++ })
	if _, err := store.DB().Exec(`CREATE TRIGGER reject_sepay_receipt BEFORE INSERT ON sepay_receipts BEGIN SELECT RAISE(ABORT, 'test receipt failure'); END`); err != nil {
		t.Fatal(err)
	}
	update := decodeFixture(t, telegramFixture(t, cfg, 1, 1, fixtureNotification))
	if err := service.HandleUpdate(ctx, update); err == nil {
		t.Fatal("failed receipt commit was acknowledged")
	}
	protocolCount(t, store, "sepay_telegram_inbox", 0)
	protocolCount(t, store, "transactions", 0)
	protocolCount(t, store, "sepay_receipts", 0)
	if hints != 0 {
		t.Fatal("rolled-back transaction emitted hint")
	}
	if _, err := store.DB().Exec(`DROP TRIGGER reject_sepay_receipt`); err != nil {
		t.Fatal(err)
	}
	if err := service.HandleUpdate(ctx, update); err != nil {
		t.Fatal(err)
	}
	if hints != 1 {
		t.Fatal("recovered commit did not emit one hint")
	}
}

func TestServiceNotificationAccountMatchesExactlyAndKeepsVAIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, override, account, bank string
		accepted                      bool
	}{
		{"legacy absent", "", "VA012345", "Fixture Bank", true},
		{"legacy main rejected", "", "2210112002", "Fixture Bank", false},
		{"explicit main", "2210112002", "2210112002", "Fixture Bank", true},
		{"VA not main", "2210112002", "VA012345", "Fixture Bank", false},
		{"other main", "2210112002", "2210112003", "Fixture Bank", false},
		{"bank exact", "2210112002", "2210112002", "fixture bank", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := protocolConfig(t)
			cfg.NotificationAccountNumber = tc.override
			store := protocolStore(t)
			hints := 0
			service := NewService(cfg, store, func(storage.EventNotification) { hints++ })
			text := strings.Replace(fixtureNotification, "VA012345", tc.account, 1)
			text = strings.Replace(text, "Fixture Bank", tc.bank, 1)
			update := decodeFixture(t, telegramFixture(t, cfg, 1, 1, text))
			for range 2 {
				if err := service.HandleUpdate(context.Background(), update); err != nil {
					t.Fatal(err)
				}
			}
			want, reason := 0, ReasonAccountMismatch
			if tc.accepted {
				want, reason = 1, "ACCEPTED"
				var account, bank, connection string
				if err := store.DB().QueryRow(`SELECT r.account_number,r.bank_code,t.connection_id FROM sepay_receipts r JOIN transactions t ON t.id=r.transaction_id`).Scan(&account, &bank, &connection); err != nil || account != cfg.AccountNumber || bank != cfg.BankCode || connection != "sepay-store:"+cfg.StoreKey {
					t.Fatalf("notification override changed persisted VA identity: %v", err)
				}
			}
			var actualReason string
			if err := store.DB().QueryRow(`SELECT reason FROM sepay_telegram_inbox`).Scan(&actualReason); err != nil || actualReason != reason {
				t.Fatalf("wrong inbox reason %s: %v", actualReason, err)
			}
			protocolCount(t, store, "transactions", want)
			protocolCount(t, store, "sepay_receipts", want)
			protocolCount(t, store, "sepay_telegram_inbox", 1)
			if hints != want {
				t.Fatal("replay changed committed hint count")
			}
		})
	}
}
