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
