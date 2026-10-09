package payments

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/thedemontuan/acb-transaction-webhook/internal/config"
)

func TestUnconfiguredServiceDoesNotExposeCredentials(t *testing.T) {
	cfg := config.Config{PayOSClientID: "private-channel", PayOSAPIKey: "private-api", PaymentPublicOrigin: "https://transactions.tuannguyenviet.site", PaymentsEnabled: true, PaymentMaxAmountVND: 500000000}
	s := NewService(cfg, nil, nil, nil)
	got := s.Config()
	if got.Ready || got.Status != "UNCONFIGURED" {
		t.Fatalf("unconfigured service accepts payments: %+v", got)
	}
	if got.StaticURL != "https://transactions.tuannguyenviet.site/pay" {
		t.Fatalf("static URL: %q", got.StaticURL)
	}
	body, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{cfg.PayOSClientID, cfg.PayOSAPIKey} {
		if strings.Contains(string(body), secret) {
			t.Fatal("public config exposes credential")
		}
	}
}

func TestPaymentGateNeverChangesProvider(t *testing.T) {
	cfg := config.Config{PayOSClientID: "channel", PayOSAPIKey: "api", PayOSChecksumKey: "checksum", PaymentPublicOrigin: "https://transactions.tuannguyenviet.site", PaymentMaxAmountVND: 500000000}
	s := NewService(cfg, nil, nil, nil)
	if got := s.Config(); got.Status != "DISABLED" || got.Provider != "PAYOS" || got.Bank != "KienlongBank" || got.Ready {
		t.Fatalf("disabled config: %+v", got)
	}
	cfg.PaymentsEnabled = true
	s = NewService(cfg, nil, nil, nil)
	if got := s.Config(); got.Status != "WEBHOOK_UNCONFIRMED" || got.Ready {
		t.Fatalf("unconfirmed config: %+v", got)
	}
	cfg.PayOSWebhookConfirmed = true
	s = NewService(cfg, nil, nil, nil)
	if got := s.Config(); got.Status != "UNAVAILABLE" || got.Ready {
		t.Fatalf("missing adapter config: %+v", got)
	}
}
