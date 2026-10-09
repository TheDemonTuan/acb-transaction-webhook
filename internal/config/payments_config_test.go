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
	if !cfg.PaymentsEnabled || cfg.PayOSWebhookConfirmed || cfg.PaymentMaxAmountVND != 500000000 {
		t.Fatalf("unexpected payment defaults: enabled=%t confirmed=%t max=%d", cfg.PaymentsEnabled, cfg.PayOSWebhookConfirmed, cfg.PaymentMaxAmountVND)
	}
	if cfg.PaymentPublicOrigin != "http://localhost:5173" || cfg.PaymentStaticURL() != "http://localhost:5173/pay" || cfg.PayOSWebhookURL() != "http://localhost:5173/api/integrations/payos/webhook" {
		t.Fatal("unexpected default payment URLs")
	}
}

func TestLoadPayOSSecretSources(t *testing.T) {
	for _, source := range []string{"environment", "file", "environment takes precedence"} {
		t.Run(source, func(t *testing.T) {
			setupPaymentDevelopmentEnv(t)
			for _, key := range []string{"PAYOS_CLIENT_ID", "PAYOS_API_KEY", "PAYOS_CHECKSUM_KEY"} {
				if source != "file" {
					t.Setenv(key, "  test-"+key+" \r\n")
				}
				if source == "file" {
					path := filepath.Join(t.TempDir(), key)
					if err := os.WriteFile(path, []byte("  test-"+key+" \r\n"), 0o600); err != nil {
						t.Fatal(err)
					}
					t.Setenv(key+"_FILE", path)
				} else if source == "environment takes precedence" {
					t.Setenv(key+"_FILE", filepath.Join(t.TempDir(), "not-mounted"))
				}
			}
			cfg, err := Load()
			if err != nil {
				t.Fatalf("load config: %v", err)
			}
			if cfg.PayOSClientID != "test-PAYOS_CLIENT_ID" || cfg.PayOSAPIKey != "test-PAYOS_API_KEY" || cfg.PayOSChecksumKey != "test-PAYOS_CHECKSUM_KEY" {
				t.Fatal("payment credentials did not resolve and trim all three sources")
			}
		})
	}
}

func TestLoadRejectsUnreadableOrEmptyPayOSSecretFiles(t *testing.T) {
	for _, key := range []string{"PAYOS_CLIENT_ID", "PAYOS_API_KEY", "PAYOS_CHECKSUM_KEY"} {
		for _, contents := range []string{"missing", "empty"} {
			t.Run(key+"/"+contents, func(t *testing.T) {
				setupPaymentDevelopmentEnv(t)
				path := filepath.Join(t.TempDir(), "secret")
				if contents == "empty" {
					if err := os.WriteFile(path, []byte(" \r\n\t"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				t.Setenv(key+"_FILE", path)
				if _, err := Load(); err == nil {
					t.Fatal("expected invalid secret file to fail configuration loading")
				}
			})
		}
	}
}

func TestProductionRequiresEachPayOSCredentialEvenWhenPaymentsDisabled(t *testing.T) {
	for _, role := range []string{"gateway", "worker"} {
		for _, key := range []string{"PAYOS_CLIENT_ID", "PAYOS_API_KEY", "PAYOS_CHECKSUM_KEY"} {
			t.Run(role+"/"+key, func(t *testing.T) {
				setupPaymentDevelopmentEnv(t)
				setupBaseProductionEnv(t)
				t.Setenv("RUNTIME_ROLE", role)
				t.Setenv("WORKER_INTERNAL_TOKEN", "worker-token")
				t.Setenv("WORKER_RPC_URL", "http://worker:8190")
				t.Setenv("BARK_SERVER_URL", "")
				t.Setenv("PAYMENTS_ENABLED", "false")
				t.Setenv("PAYOS_WEBHOOK_CONFIRMED", "false")
				t.Setenv(key, "")
				_, err := Load()
				if err == nil || !strings.Contains(err.Error(), key) {
					t.Fatalf("expected missing %s to reject production config, got %v", key, err)
				}
				for _, secret := range []string{"test-payos-client", "test-payos-api", "test-payos-checksum"} {
					if strings.Contains(err.Error(), secret) {
						t.Fatal("configuration error exposed a credential")
					}
				}
			})
		}
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

func TestProductionAcceptsPayOSSecretFiles(t *testing.T) {
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
			if _, err := Load(); err != nil {
				t.Fatalf("production role %s rejected mounted payment secrets: %v", role, err)
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

func TestLoadPaymentGateAndAmountOverrides(t *testing.T) {
	setupPaymentDevelopmentEnv(t)
	t.Setenv("PAYMENTS_ENABLED", "false")
	t.Setenv("PAYOS_WEBHOOK_CONFIRMED", "true")
	t.Setenv("PAYMENT_MAX_AMOUNT_VND", "123456")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.PaymentsEnabled || !cfg.PayOSWebhookConfirmed || cfg.PaymentMaxAmountVND != 123456 {
		t.Fatal("payment gate or amount overrides were not applied")
	}
	for _, limit := range []string{"1", "9007199254740991"} {
		t.Setenv("PAYMENT_MAX_AMOUNT_VND", limit)
		if _, err := Load(); err != nil {
			t.Fatalf("valid boundary %s rejected: %v", limit, err)
		}
	}
}

func TestLoadRejectsInvalidPaymentGatesAndAmountLimits(t *testing.T) {
	for _, key := range []string{"PAYMENTS_ENABLED", "PAYOS_WEBHOOK_CONFIRMED"} {
		t.Run(key, func(t *testing.T) {
			setupPaymentDevelopmentEnv(t)
			t.Setenv(key, "not-a-boolean")
			if _, err := Load(); err == nil {
				t.Fatal("expected invalid payment gate to be rejected")
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
