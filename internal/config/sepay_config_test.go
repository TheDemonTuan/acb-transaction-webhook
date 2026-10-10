package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadSePayStoreConfigOnlyFromFile(t *testing.T) {
	for _, role := range []string{"gateway", "monolith-dev"} {
		t.Run(role, func(t *testing.T) {
			setupPaymentDevelopmentEnv(t)
			t.Setenv("RUNTIME_ROLE", role)
			t.Setenv("SEPAY_STORE_CONFIG_FILE", "")
			t.Setenv("SEPAY_STORE_CONFIG", `{"mode":"active","webhookSecret":"ignored-env-secret"}`)
			t.Setenv("SEPAY_STORE_CONFIG_JSON", `{"mode":"active","webhookSecret":"ignored-env-secret"}`)
			cfg, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.SePayStoreConfigJSON != "" {
				t.Fatal("SePay config loaded from inline environment")
			}
			path := filepath.Join(t.TempDir(), "sepay_store_config")
			if err := os.WriteFile(path, []byte(" \r\n{\"mode\":\"disabled\"}\r\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("SEPAY_STORE_CONFIG_FILE", path)
			cfg, err = Load()
			if err != nil || cfg.SePayStoreConfigJSON != `{"mode":"disabled"}` {
				t.Fatalf("file config was not loaded: %v", err)
			}
			if cfg.PayOSClientID != "" || cfg.PayOSAPIKey != "" || cfg.PaymentStaticURL() != "http://localhost:5173/pay" {
				t.Fatal("SePay config changed payOS configuration")
			}
		})
	}
}

func TestLoadRejectsUnreadableOrEmptySePayConfigFile(t *testing.T) {
	setupPaymentDevelopmentEnv(t)
	path := filepath.Join(t.TempDir(), "sepay_store_config")
	t.Setenv("SEPAY_STORE_CONFIG_FILE", path)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SEPAY_STORE_CONFIG_FILE") {
		t.Fatal("configured missing file did not fail loading")
	}
	if err := os.WriteFile(path, []byte(" \r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SEPAY_STORE_CONFIG_FILE") {
		t.Fatal("configured empty file did not fail loading")
	}
}

func TestSePayRawConfigExcludedFromJSON(t *testing.T) {
	raw, err := json.Marshal(Config{SePayStoreConfigJSON: `{"webhookSecret":"private-config-secret"}`})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "private-config-secret") || strings.Contains(string(raw), "SePayStoreConfigJSON") {
		t.Fatal("config JSON leaked SePay secret material")
	}
}
