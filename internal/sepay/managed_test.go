package sepay

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/security"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func adminFixture(t *testing.T) AdminFields {
	f := fieldsFromConfig(protocolConfig(t))
	f.ReceiverVerified, f.SourceSeparated = true, true
	return f
}

func TestManagedConfigDraftSecretsAndActivation(t *testing.T) {
	ctx := context.Background()
	store := protocolStore(t)
	service := NewService(Config{Mode: ModeDisabled}, store, nil)
	token := "900001:private_token_abcdefghijklmnopqrstuvwxyz"
	saved, err := service.SaveAdminConfig(ctx, 0, AdminFields{Mode: ModeDisabled}, token, storage.PaymentProviderActor{})
	if err != nil || saved.Revision != 1 || !saved.HasBotToken || !saved.HasWebhookSecret || saved.Config.Mode != ModeDisabled {
		t.Fatalf("draft save failed: %v", err)
	}
	snapshot, err := service.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	secret := snapshot.cfg.WebhookSecret
	decoded, err := base64.RawURLEncoding.DecodeString(secret)
	if err != nil || len(decoded) != 32 {
		t.Fatal("webhook secret is not 32 random bytes")
	}
	raw, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), token) || strings.Contains(string(raw), secret) || strings.Contains(string(raw), `"webhookSecret"`) || strings.Contains(string(raw), `"botToken"`) {
		t.Fatal("admin readback exposed a secret")
	}
	fields := adminFixture(t)
	fields.ActivationAt = ""
	fields.SourceSeparated = false
	if _, err := service.SaveAdminConfig(ctx, 1, fields, "", storage.PaymentProviderActor{}); err == nil {
		t.Fatal("active missing attestation accepted")
	}
	fields.Mode = ModeObserve
	observe, err := service.SaveAdminConfig(ctx, 1, fields, "", storage.PaymentProviderActor{})
	if err != nil || observe.Config.ActivationAt == "" {
		t.Fatalf("observe did not stamp UTC cutoff: %v", err)
	}
	snapshot, err = service.Snapshot(ctx)
	if err != nil || snapshot.cfg.WebhookSecret != secret || snapshot.token != token {
		t.Fatal("blank token did not preserve encrypted credentials")
	}
	fields = observe.Config
	fields.Mode, fields.SourceSeparated = ModeActive, true
	fields.ActivationAt = ""
	active, err := service.SaveAdminConfig(ctx, 2, fields, "", storage.PaymentProviderActor{})
	if err != nil {
		t.Fatal(err)
	}
	activation, err := time.Parse(time.RFC3339, active.Config.ActivationAt)
	if err != nil || time.Since(activation) > 5*time.Second {
		t.Fatal("new activation has no fresh cutoff")
	}
	if _, err := service.SaveAdminConfig(ctx, 2, fields, "", storage.PaymentProviderActor{}); !errors.Is(err, storage.ErrSePayRevisionConflict) {
		t.Fatalf("stale config revision accepted: %v", err)
	}
}

func TestManagedConfigRuntimeAndObsoleteAuthenticatedSnapshot(t *testing.T) {
	ctx := context.Background()
	store := protocolStore(t)
	legacy := protocolConfig(t)
	service := NewService(legacy, store, nil)
	fields := adminFixture(t)
	if _, err := service.SaveAdminConfig(ctx, 0, fields, "", storage.PaymentProviderActor{}); err != nil {
		t.Fatal(err)
	}
	authenticated, err := service.Snapshot(ctx)
	if err != nil || !authenticated.Authenticate(legacy.WebhookSecret) {
		t.Fatal("managed snapshot authentication failed")
	}
	fields.Mode = ModeDisabled
	if _, err := service.SaveAdminConfig(ctx, 1, fields, "", storage.PaymentProviderActor{}); err != nil {
		t.Fatal(err)
	}
	current, err := service.Snapshot(ctx)
	if err != nil || current.Enabled() || current.Authenticate(legacy.WebhookSecret) {
		t.Fatal("runtime ignored managed pause")
	}
	update := decodeFixture(t, telegramFixture(t, legacy, 1, 1, fixtureNotification))
	if err := authenticated.HandleUpdate(ctx, update); !errors.Is(err, storage.ErrSePayRevisionConflict) {
		t.Fatalf("obsolete authenticated snapshot committed: %v", err)
	}
	protocolCount(t, store, "sepay_telegram_inbox", 0)
	protocolCount(t, store, "transactions", 0)
	cfg, err := BootstrapConfig(ctx, store, "this legacy file is now irrelevant")
	if err != nil || cfg.Mode != ModeDisabled {
		t.Fatalf("managed bootstrap consulted legacy file: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Snapshot(ctx); err == nil {
		t.Fatal("DB failure silently fell back to file config")
	}
}

func TestManagedFieldsRejectUnsafeDraftsAndInt64Loss(t *testing.T) {
	fields := adminFixture(t)
	fields.BotID = "9007199254740993"
	cfg, err := adminRuntime(fields, protocolConfig(t).WebhookSecret)
	if err != nil || cfg.BotID != 9007199254740993 {
		t.Fatal("decimal trust ID lost precision")
	}
	for _, bad := range []string{"9e15", "9007199254740993.0", "+900001", "9223372036854775808"} {
		candidate := fields
		candidate.BotID = bad
		if _, err := adminRuntime(candidate, cfg.WebhookSecret); err == nil {
			t.Fatal("noncanonical Telegram ID accepted")
		}
	}
	for _, payload := range []string{"https://bank.example/qr", "javascript:alert(1)", "/pay", strings.Repeat("1", 4097)} {
		candidate := AdminFields{Mode: ModeDisabled, QRPayload: payload}
		if _, err := adminRuntime(candidate, cfg.WebhookSecret); err == nil {
			t.Fatal("unsafe QR draft accepted")
		}
	}
}

func TestManagedConfigurationRefreshesIndependentRuntimeHandles(t *testing.T) {
	ctx := context.Background()
	first := protocolStore(t)
	var path string
	if err := first.DB().QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	keyring, err := security.LoadKeyring(filepath.Join(filepath.Dir(path), "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	second.WithKeyring(keyring)
	a := NewService(Config{Mode: ModeDisabled}, first, nil)
	b := NewService(Config{Mode: ModeDisabled}, second, nil)
	fields := adminFixture(t)
	if _, err := a.SaveAdminConfig(ctx, 0, fields, testBotToken, storage.PaymentProviderActor{}); err != nil {
		t.Fatal(err)
	}
	before, err := b.Snapshot(ctx)
	if err != nil || !before.Enabled() || before.cfg.StoreName != fields.StoreName {
		t.Fatal("second runtime missed managed config")
	}
	fields.Mode = ModeDisabled
	if _, err := b.SaveAdminConfig(ctx, 1, fields, "", storage.PaymentProviderActor{}); err != nil {
		t.Fatal(err)
	}
	after, err := a.Snapshot(ctx)
	if err != nil || after.Enabled() || after.revision != 2 {
		t.Fatal("first runtime did not observe cross-handle pause")
	}
	update := decodeFixture(t, telegramFixture(t, before.cfg, 1, 1, fixtureNotification))
	if err := before.HandleUpdate(ctx, update); !errors.Is(err, storage.ErrSePayRevisionConflict) {
		t.Fatalf("old process snapshot wrote after another process pause: %v", err)
	}
	protocolCount(t, first, "transactions", 0)
}

func TestManagedNotificationAccountCutoverNeverReplaysMoney(t *testing.T) {
	ctx := context.Background()
	store := protocolStore(t)
	cfg := protocolConfig(t)
	cfg.AccountNumber, cfg.BankName = "VA101499100004639250", "KienLongBank"
	hints := 0
	service := NewService(cfg, store, func(storage.EventNotification) { hints++ })
	textFor := func(account, reference string) string {
		text := strings.Replace(fixtureNotification, "VA012345", account, 1)
		text = strings.Replace(text, "Fixture Bank", cfg.BankName, 1)
		return strings.Replace(text, "SEPAY_TEST_001", reference, 1)
	}
	mainText := textFor("2210112002", "REJECTED_BEFORE_CONFIG")
	oldText := strings.Replace(textFor(cfg.AccountNumber, "BEFORE_ACTIVATION"), "10/10/2026", "09/10/2026", 1)
	baselineText := textFor(cfg.AccountNumber, "BASELINE_VA")
	handle := func(updateID, messageID int64, text string) {
		t.Helper()
		if err := service.HandleUpdate(ctx, decodeFixture(t, telegramFixture(t, cfg, updateID, messageID, text))); err != nil {
			t.Fatal(err)
		}
	}
	handle(1, 1, mainText)
	handle(2, 2, oldText)
	handle(3, 3, baselineText)
	protocolCount(t, store, "sepay_receipts", 1)
	var baselineHash string
	if err := store.DB().QueryRow(`SELECT canonical_hash FROM sepay_receipts WHERE reference='BASELINE_VA'`).Scan(&baselineHash); err != nil {
		t.Fatal(err)
	}
	fields := fieldsFromConfig(cfg)
	fields.ReceiverVerified, fields.SourceSeparated = true, false
	fields.NotificationAccountNumber = "2210112002"
	var fieldError *FieldError
	if _, err := service.SaveAdminConfig(ctx, 0, fields, "", storage.PaymentProviderActor{}); !errors.As(err, &fieldError) || fieldError.Field != "sourceSeparated" {
		t.Fatalf("main account changed without owner source attestation: %v", err)
	}
	fields.SourceSeparated = true
	fields.ActivationAt = "2026-09-01T00:00:00Z"
	saved, err := service.SaveAdminConfig(ctx, 0, fields, "", storage.PaymentProviderActor{})
	if err != nil || saved.Config.NotificationAccountNumber != "2210112002" || saved.Config.AccountNumber != cfg.AccountNumber || saved.Config.QRPayload != cfg.QRPayload || saved.Config.ActivationAt != cfg.ActivationAt.Format(time.RFC3339) {
		t.Fatalf("main notification save changed receiver/cutoff: %v", err)
	}
	loaded := NewService(Config{Mode: ModeDisabled}, store, func(storage.EventNotification) { hints++ })
	snapshot, err := loaded.Snapshot(ctx)
	if err != nil || snapshot.Config().NotificationAccountNumber != "2210112002" || snapshot.Config().AccountNumber != cfg.AccountNumber || !snapshot.Config().ActivationAt.Equal(cfg.ActivationAt) {
		t.Fatalf("persisted notification config not loaded: %v", err)
	}
	record, found, err := store.SePayManagedConfig(ctx)
	if err != nil || !found || !strings.Contains(string(record.ConfigJSON), `"notificationAccountNumber":"2210112002"`) {
		t.Fatalf("notification account missing from encrypted persistence readback: %v", err)
	}
	var encrypted []byte
	if err := store.DB().QueryRow(`SELECT config_envelope FROM sepay_managed_config WHERE id=1`).Scan(&encrypted); err != nil || strings.Contains(string(encrypted), "2210112002") || strings.Contains(string(encrypted), "notificationAccountNumber") {
		t.Fatalf("notification account persisted outside encryption: %v", err)
	}
	service = loaded
	protocolCount(t, store, "sepay_receipts", 1)
	// Retrying rejected updates and resending their original message IDs cannot credit them.
	handle(1, 1, mainText)
	handle(4, 1, mainText)
	handle(2, 2, oldText)
	handle(5, 2, oldText)
	handle(6, 6, strings.Replace(textFor("2210112002", "FRESH_PREACTIVATION"), "10/10/2026", "09/10/2026", 1))
	protocolCount(t, store, "sepay_receipts", 1)
	// The old VA receipt still dedupes when its bank reference arrives on the main account.
	handle(7, 7, textFor("2210112002", "BASELINE_VA"))
	freshText := textFor("2210112002", "FRESH_MAIN")
	handle(8, 8, freshText)
	handle(8, 8, freshText)
	handle(9, 8, freshText)
	handle(10, 10, freshText)
	var hash string
	if err := store.DB().QueryRow(`SELECT canonical_hash FROM sepay_receipts WHERE reference='BASELINE_VA'`).Scan(&hash); err != nil || hash != baselineHash {
		t.Fatalf("notification account changed canonical receipt identity: %v", err)
	}
	for _, tc := range []struct {
		id     int64
		reason string
	}{{1, ReasonAccountMismatch}, {4, ReasonAccountMismatch}, {2, "PRE_ACTIVATION"}, {5, "PRE_ACTIVATION"}, {6, "PRE_ACTIVATION"}, {7, "DUPLICATE"}, {8, "ACCEPTED"}, {9, "DUPLICATE"}, {10, "DUPLICATE"}} {
		var reason string
		if err := store.DB().QueryRow(`SELECT reason FROM sepay_telegram_inbox WHERE update_id=?`, tc.id).Scan(&reason); err != nil || reason != tc.reason {
			t.Fatalf("update %d reason=%s want=%s: %v", tc.id, reason, tc.reason, err)
		}
	}
	fields = saved.Config
	fields.NotificationAccountNumber, fields.ActivationAt = "", ""
	cleared, err := service.SaveAdminConfig(ctx, 1, fields, "", storage.PaymentProviderActor{})
	if err != nil || cleared.Config.NotificationAccountNumber != "" || cleared.Config.ActivationAt != cfg.ActivationAt.Format(time.RFC3339) {
		t.Fatalf("clearing override moved active cutoff: %v", err)
	}
	handle(11, 11, textFor("2210112002", "MAIN_AFTER_CLEAR"))
	handle(12, 12, textFor(cfg.AccountNumber, "FRESH_MAIN"))
	protocolCount(t, store, "transactions", 2)
	protocolCount(t, store, "sepay_receipts", 2)
	protocolCount(t, store, "payment_receipts", 0)
	var credit int64
	var wrongReceiver int
	if err := store.DB().QueryRow(`SELECT SUM(credit) FROM transactions`).Scan(&credit); err != nil || credit != 100000 || hints != 2 {
		t.Fatalf("cutover/replay changed money: credit=%d hints=%d err=%v", credit, hints, err)
	}
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM sepay_receipts WHERE account_number<>?`, cfg.AccountNumber).Scan(&wrongReceiver); err != nil || wrongReceiver != 0 {
		t.Fatalf("main account replaced receipt VA identity: %v", err)
	}
}

func TestManagedNotificationAccountDraftValidation(t *testing.T) {
	for _, account := range []string{" account", "account ", "123-456", "１２３", "abc\n", strings.Repeat("A", 201)} {
		var field *FieldError
		_, err := adminRuntime(AdminFields{Mode: ModeDisabled, NotificationAccountNumber: account}, "")
		if !errors.As(err, &field) || field.Field != "notificationAccountNumber" {
			t.Fatalf("invalid draft notification account not field-rejected: %v", err)
		}
	}
	for _, account := range []string{"", "2210112002", strings.Repeat("A", 200)} {
		if _, err := adminRuntime(AdminFields{Mode: ModeDisabled, NotificationAccountNumber: account}, ""); err != nil {
			t.Fatalf("valid draft notification account rejected: %v", err)
		}
	}
}
