package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/payments"
	"github.com/thedemontuan/acb-transaction-webhook/internal/security"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func managedHTTPFixture(t *testing.T) *paymentHTTPFixture {
	t.Helper()
	f := newPaymentHTTPFixture(t)
	keyring, err := security.NewKeyring([]byte("01234567890123456789012345678901"))
	if err != nil {
		t.Fatal(err)
	}
	f.store.WithKeyring(keyring)
	f.service = payments.NewManagedService(f.cfg, f.store, nil)
	f.handler = f.roleHandler("owner")
	return f
}

const ownerProviderBody = `{"clientId":"owner-private-channel","apiKey":"owner-private-api-key","checksumKey":"owner-private-checksum","enabled":true}`

func TestProviderConfigOwnerRBACCSRFAndSafeSnapshots(t *testing.T) {
	f := managedHTTPFixture(t)
	for _, role := range []string{"operator", "viewer"} {
		for _, method := range []string{"GET", "PUT"} {
			w := paymentRequest(f.roleHandler(role), method, "/api/v1/payment-provider/config", ownerProviderBody, "", true)
			if w.Code != 403 {
				t.Fatalf("%s %s gained provider config access: %d", role, method, w.Code)
			}
		}
	}
	if w := paymentRequest(f.handler, "PUT", "/api/v1/payment-provider/config", ownerProviderBody, "", false); w.Code != 403 {
		t.Fatal("provider save did not require CSRF")
	}
	for _, role := range []string{"owner", "operator", "viewer"} {
		w := paymentRequest(f.roleHandler(role), "GET", "/api/v1/status", "", "", false)
		var status struct {
			UserRole string `json:"userRole"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
			t.Fatal(err)
		}
		if w.Code != 200 || status.UserRole != strings.ToUpper(role) {
			t.Fatalf("role metadata mismatch: %d %s", w.Code, w.Body.String())
		}
	}
	initial := paymentRequest(f.handler, "GET", "/api/v1/payment-provider/config", "", "", false)
	if initial.Code != 200 || !strings.Contains(initial.Body.String(), `"configured":false`) {
		t.Fatalf("unconfigured owner GET: %d %s", initial.Code, initial.Body.String())
	}
	saved := paymentRequest(f.handler, "PUT", "/api/v1/payment-provider/config", ownerProviderBody, "", true)
	if saved.Code != 200 {
		t.Fatalf("provider save: %d %s", saved.Code, saved.Body.String())
	}
	var snapshot payments.ProviderConfig
	if err := json.Unmarshal(saved.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if !snapshot.Configured || !snapshot.Enabled || snapshot.WebhookConfirmed || snapshot.ClientID != "owner-private-channel" || !snapshot.APIKeyConfigured || !snapshot.ChecksumKeyConfigured {
		t.Fatalf("safe snapshot mismatch: %+v", snapshot)
	}
	for _, path := range []string{"/api/v1/payment-provider/config", "/api/public/v1/payment-config", "/api/v1/status", "/api/v1/audit"} {
		w := paymentRequest(f.handler, "GET", path, "", "", false)
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" && path != "/api/v1/audit" {
			t.Fatalf("snapshot %s failed or cached: %d", path, w.Code)
		}
		for _, secret := range []string{"owner-private-api-key", "owner-private-checksum"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatal("provider secret leaked from API")
			}
		}
		if path == "/api/public/v1/payment-config" && strings.Contains(w.Body.String(), "owner-private-channel") {
			t.Fatal("public config leaked raw channel ID")
		}
	}
	if w := paymentRequest(f.handler, "POST", "/api/public/v1/payments", `{"amountVnd":50000,"origin":"STATIC_URL"}`, paymentKey(9), false); w.Code != 503 || !strings.Contains(w.Body.String(), "WEBHOOK_UNCONFIRMED") {
		t.Fatal("save enabled created payments before real confirmation")
	}
	var audits int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM audit_logs WHERE action='payment-provider.config.save' AND actor_subject='owner' AND actor_role='OWNER'`).Scan(&audits); err != nil || audits != 1 {
		t.Fatal("owner provider save missing audit")
	}
	blank := `{"clientId":"owner-private-channel","apiKey":"","checksumKey":"","enabled":false}`
	if w := paymentRequest(f.handler, "PUT", "/api/v1/payment-provider/config", blank, "", true); w.Code != 200 {
		t.Fatalf("blank keep-current save: %d %s", w.Code, w.Body.String())
	}
	keys, err := f.store.PaymentProviderConfig(context.Background())
	if err != nil || keys.Credentials.APIKey != "owner-private-api-key" || keys.Credentials.ChecksumKey != "owner-private-checksum" || keys.Enabled {
		t.Fatal("blank input lost keys or failed to disable")
	}
}

func TestProviderConfigRejectsMalformedGateAndChannelChange(t *testing.T) {
	f := managedHTTPFixture(t)
	for _, body := range []string{
		`{"clientId":"owner-private-channel","apiKey":"","checksumKey":"","enabled":true}`,
		`{"clientId":"owner-private-channel","apiKey":"private","checksumKey":"private","enabled":"true"}`,
		`{"clientId":"owner-private-channel","apiKey":"private","checksumKey":"private","enabled":true,"url":"https://evil.example"}`,
		`{"clientId":"owner-private-channel","apiKey":"private","checksumKey":"private","enabled":true,"enabled":false}`,
		ownerProviderBody + ` {}`,
	} {
		w := paymentRequest(f.handler, "PUT", "/api/v1/payment-provider/config", body, "", true)
		if w.Code != 400 || strings.Contains(w.Body.String(), "owner-private") {
			t.Fatalf("invalid provider input accepted or reflected: %d %s", w.Code, w.Body.String())
		}
	}
	if w := paymentRequest(f.handler, "PUT", "/api/v1/payment-provider/config", ownerProviderBody, "", true); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	ctx := context.Background()
	_, token, err := f.store.BeginPaymentProviderOperation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(ownerProviderBody, "owner-private-api-key", "replacement-private-key", 1)
	if w := paymentRequest(f.handler, "PUT", "/api/v1/payment-provider/config", changed, "", true); w.Code != 409 || !strings.Contains(w.Body.String(), "PROVIDER_CONFIG_BUSY") {
		t.Fatalf("active operation key replacement: %d %s", w.Code, w.Body.String())
	}
	if err := f.store.EndPaymentProviderOperation(ctx, token); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.store.ReservePaymentOrder(ctx, storage.PaymentOrderIntent{ChannelID: "owner-private-channel", IdempotencyKey: "test-provider-channel-lock", RequestHash: "hash", AmountVnd: 1000, Origin: "STATIC_URL"}); err != nil {
		t.Fatal(err)
	}
	changed = strings.Replace(ownerProviderBody, "owner-private-channel", "replacement-private-channel", 1)
	if w := paymentRequest(f.handler, "PUT", "/api/v1/payment-provider/config", changed, "", true); w.Code != 409 || !strings.Contains(w.Body.String(), "PROVIDER_CHANNEL_LOCKED") {
		t.Fatalf("issued orders abandoned by channel change: %d %s", w.Code, w.Body.String())
	}
	gate, err := f.store.AcquireMutationGate(ctx, "provider-http", time.Minute, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer f.store.ReleaseMutationGate(ctx, "provider-http", gate.LeaseToken)
	if w := paymentRequest(f.handler, "PUT", "/api/v1/payment-provider/config", ownerProviderBody, "", true); w.Code != http.StatusServiceUnavailable {
		t.Fatal("configuration save bypassed mutation gate")
	}
	if w := paymentRequest(f.handler, "GET", "/api/v1/payment-provider/config", "", "", false); w.Code != 200 {
		t.Fatal("deployment gate prevented owner reading safe configuration")
	}
}
