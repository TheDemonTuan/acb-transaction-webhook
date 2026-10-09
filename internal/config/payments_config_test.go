package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setPayOSTestSecrets(t *testing.T) {
	t.Helper()
	t.Setenv("PAYOS_CLIENT_ID", "test-payos-client")
	t.Setenv("PAYOS_API_KEY", "test-payos-api")
	t.Setenv("PAYOS_CHECKSUM_KEY", "test-payos-checksum")
	for _, key := range []string{"PAYOS_CLIENT_ID_FILE", "PAYOS_API_KEY_FILE", "PAYOS_CHECKSUM_KEY_FILE"} {
		t.Setenv(key, "")
	}
}

func setupPaymentDevelopmentEnv(t *testing.T) {
	t.Helper()
	t.Setenv("APP_ENV", "development")
	t.Setenv("RUNTIME_ROLE", "monolith-dev")
	t.Setenv("DATA_DIR", t.TempDir())
	for _, key := range []string{
		"PAYOS_CLIENT_ID", "PAYOS_CLIENT_ID_FILE", "PAYOS_API_KEY", "PAYOS_API_KEY_FILE",
		"PAYOS_CHECKSUM_KEY", "PAYOS_CHECKSUM_KEY_FILE", "PAYMENT_PUBLIC_ORIGIN",
		"PAYMENTS_ENABLED", "PAYOS_WEBHOOK_CONFIRMED", "PAYMENT_MAX_AMOUNT_VND",
	} {
		t.Setenv(key, "")
	}
}

func TestLoadPaymentDefaultsWithoutDevelopmentCredentials(t *testing.T) {
	setupPaymentDevelopmentEnv(t)
	// The payment origin is independent of the administrator's PUBLIC_ORIGIN.
	t.Setenv("PUBLIC_ORIGIN", "https://admin.example.com")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.PayOSClientID != "" || cfg.PayOSAPIKey != "" || cfg.PayOSChecksumKey != "" {
		t.Fatal("expected development to permit unconfigured payment credentials")
	}
	if cfg.PaymentsEnabled || cfg.PayOSWebhookConfirmed || cfg.PaymentMaxAmountVND != 500000000 {
		t.Fatalf("unexpected payment defaults: enabled=%t confirmed=%t max=%d", cfg.PaymentsEnabled, cfg.PayOSWebhookConfirmed, cfg.PaymentMaxAmountVND)
	}
	if cfg.PaymentPublicOrigin != "http://localhost:5173" || cfg.PaymentStaticURL() != "http://localhost:5173/pay" || cfg.PayOSWebhookURL() != "http://localhost:5173/api/integrations/payos/webhook" {
		t.Fatal("unexpected default payment URLs")
	}
}

func TestLoadIgnoresLegacyPayOSSecrets(t *testing.T) {
	for _, role := range []string{"gateway", "worker"} {
		t.Run(role, func(t *testing.T) {
			setupPaymentDevelopmentEnv(t)
			setupBaseProductionEnv(t)
			t.Setenv("RUNTIME_ROLE", role)
			t.Setenv("WORKER_INTERNAL_TOKEN", "worker-token")
			t.Setenv("WORKER_RPC_URL", "http://worker:8190")
			t.Setenv("BARK_SERVER_URL", "")
			for _, key := range []string{"PAYOS_CLIENT_ID", "PAYOS_API_KEY", "PAYOS_CHECKSUM_KEY"} {
				t.Setenv(key, "unused-legacy-credential")
				t.Setenv(key+"_FILE", filepath.Join(t.TempDir(), "not-mounted"))
			}
			cfg, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.PayOSClientID != "" || cfg.PayOSAPIKey != "" || cfg.PayOSChecksumKey != "" {
				t.Fatal("legacy credentials were loaded")
			}
			for _, key := range []string{"PAYOS_CLIENT_ID", "PAYOS_API_KEY", "PAYOS_CHECKSUM_KEY"} {
				t.Setenv(key, "")
			}
			if _, err := Load(); err != nil {
				t.Fatalf("unconfigured production must boot: %v", err)
			}
		})
	}
}

func TestLoadProductionPaymentURLs(t *testing.T) {
	setupPaymentDevelopmentEnv(t)
	setupBaseProductionEnv(t)
	t.Setenv("RUNTIME_ROLE", "worker")
	t.Setenv("WORKER_INTERNAL_TOKEN", "worker-token")
	t.Setenv("BARK_SERVER_URL", "")
	t.Setenv("PUBLIC_ORIGIN", "https://admin.example.com")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.PaymentPublicOrigin != "https://transactions.tuannguyenviet.site" || cfg.PaymentStaticURL() != "https://transactions.tuannguyenviet.site/pay" || cfg.PayOSWebhookURL() != "https://transactions.tuannguyenviet.site/api/integrations/payos/webhook" {
		t.Fatal("unexpected canonical production payment URLs")
	}
}

func TestProductionIgnoresMountedLegacyPayOSSecrets(t *testing.T) {
	for _, role := range []string{"gateway", "worker"} {
		t.Run(role, func(t *testing.T) {
			setupPaymentDevelopmentEnv(t)
			setupBaseProductionEnv(t)
			t.Setenv("RUNTIME_ROLE", role)
			t.Setenv("WORKER_INTERNAL_TOKEN", "worker-token")
			t.Setenv("WORKER_RPC_URL", "http://worker:8190")
			t.Setenv("BARK_SERVER_URL", "")
			for _, key := range []string{"PAYOS_CLIENT_ID", "PAYOS_API_KEY", "PAYOS_CHECKSUM_KEY"} {
				path := filepath.Join(t.TempDir(), key)
				if err := os.WriteFile(path, []byte("test-"+key+"\r\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				t.Setenv(key, "")
				t.Setenv(key+"_FILE", path)
			}
			cfg, err := Load()
			if err != nil {
				t.Fatalf("unused mounted legacy files prevented boot: %v", err)
			}
			if cfg.PayOSClientID != "" || cfg.PayOSAPIKey != "" || cfg.PayOSChecksumKey != "" {
				t.Fatal("legacy mounted credentials loaded")
			}
		})
	}
}

func TestLoadCanonicalPaymentOrigins(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  string
	}{
		{" https://PAYMENTS.Example.com:443 ", "https://payments.example.com"},
		{"https://payments.example.com:8443", "https://payments.example.com:8443"},
		{"http://LOCALHOST:80", "http://localhost"},
		{"http://127.0.0.1:5173", "http://127.0.0.1:5173"},
		{"http://[::1]:5173", "http://[::1]:5173"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			setupPaymentDevelopmentEnv(t)
			t.Setenv("PAYMENT_PUBLIC_ORIGIN", tc.input)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("load config: %v", err)
			}
			if cfg.PaymentPublicOrigin != tc.want || cfg.PaymentStaticURL() != tc.want+"/pay" || cfg.PayOSWebhookURL() != tc.want+"/api/integrations/payos/webhook" {
				t.Fatalf("payment origin was not canonicalized to %q", tc.want)
			}
		})
	}
}

func TestLoadRejectsInvalidPaymentOrigins(t *testing.T) {
	for _, origin := range []string{
		"payments.example.com", "//payments.example.com", "https://", "ftp://payments.example.com",
		"https://user:password@payments.example.com", "https://payments.example.com/",
		"https://payments.example.com/pay", "https://payments.example.com/%2f",
		"https://payments.example.com?amount=1", "https://payments.example.com?",
		"https://payments.example.com#section", "https://payments.example.com#",
		"https://payments.example.com:", "https://payments.example.com:0",
		"https://payments.example.com:65536", "https://payments.example.com:abc",
		"https://[not-an-ip]", "https://payments.example.com:443:443",
		"http://payments.example.com", "http://localhost.evil.example", "http://192.168.1.1",
	} {
		t.Run(origin, func(t *testing.T) {
			setupPaymentDevelopmentEnv(t)
			t.Setenv("PAYMENT_PUBLIC_ORIGIN", origin)
			if _, err := Load(); err == nil {
				t.Fatal("expected invalid payment origin to be rejected")
			}
		})
	}
	for _, origin := range []string{"http://localhost:5173", "http://127.0.0.1", "http://[::1]"} {
		t.Run("production/"+origin, func(t *testing.T) {
			setupPaymentDevelopmentEnv(t)
			t.Setenv("APP_ENV", "production")
			t.Setenv("RUNTIME_ROLE", "worker")
			t.Setenv("PAYMENT_PUBLIC_ORIGIN", origin)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), "PAYMENT_PUBLIC_ORIGIN") {
				t.Fatalf("expected production HTTP origin to be rejected, got %v", err)
			}
		})
	}
}

func TestLoadIgnoresLegacyPaymentGatesAndAcceptsAmountOverrides(t *testing.T) {
	setupPaymentDevelopmentEnv(t)
	t.Setenv("PAYMENTS_ENABLED", "false")
	t.Setenv("PAYOS_WEBHOOK_CONFIRMED", "true")
	t.Setenv("PAYMENT_MAX_AMOUNT_VND", "123456")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.PaymentsEnabled || cfg.PayOSWebhookConfirmed || cfg.PaymentMaxAmountVND != 123456 {
		t.Fatal("legacy payment gates affected managed configuration or amount override was lost")
	}
	for _, limit := range []string{"1", "9007199254740991"} {
		t.Setenv("PAYMENT_MAX_AMOUNT_VND", limit)
		if _, err := Load(); err != nil {
			t.Fatalf("valid boundary %s rejected: %v", limit, err)
		}
	}
}

func TestLoadIgnoresInvalidLegacyGatesAndRejectsInvalidAmountLimits(t *testing.T) {
	for _, key := range []string{"PAYMENTS_ENABLED", "PAYOS_WEBHOOK_CONFIRMED"} {
		t.Run(key, func(t *testing.T) {
			setupPaymentDevelopmentEnv(t)
			t.Setenv(key, "not-a-boolean")
			if _, err := Load(); err != nil {
				t.Fatal("unused legacy payment gate prevented boot")
			}
		})
	}
	for _, limit := range []string{"0", "-1", "1.5", "\"5000\"", "invalid", "9007199254740992", "9223372036854775808"} {
		t.Run(limit, func(t *testing.T) {
			setupPaymentDevelopmentEnv(t)
			t.Setenv("PAYMENT_MAX_AMOUNT_VND", limit)
			if _, err := Load(); err == nil {
				t.Fatal("expected invalid amount limit to be rejected")
			}
		})
	}
}
