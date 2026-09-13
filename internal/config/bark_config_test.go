package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setProductionEnv(t *testing.T) {
	t.Helper()
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte("32byteslongkeyforproductiontest!"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("APP_ENV", "production")
	t.Setenv("APP_MASTER_KEY_FILE", keyFile)
	t.Setenv("OWNER_SUBJECTS", "owner@example.com")
	t.Setenv("CF_ACCESS_ISSUER", "https://test.cloudflareaccess.com")
	t.Setenv("CF_ACCESS_AUDIENCE", "aud123")
	t.Setenv("CF_ACCESS_JWKS_URL", "https://test.cloudflareaccess.com/certs")
	t.Setenv("TTS_GATEWAY_URL", "http://tts-gateway:8081")
	t.Setenv("TTS_INTERNAL_TOKEN", "test-token")
	t.Setenv("RUNTIME_ROLE", "worker")
	t.Setenv("WORKER_INTERNAL_TOKEN", "test-worker-token")
}

func TestLoadRejectsInvalidBarkConfiguration(t *testing.T) {
	tests := []struct {
		name   string
		server string
		public string
		user   string
		pass   string
		want   string
	}{
		{"unsupported server scheme", "ftp://bark:8080", "", "user", "pass", "http or https"},
		{"production public http", "http://bark:8080", "http://push.example.com", "user", "pass", "use https"},
		{"missing password", "http://bark:8080", "https://push.example.com", "user", "", "configured together"},
		{"missing credentials", "http://bark:8080", "https://push.example.com", "", "", "required in production"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setProductionEnv(t)
			t.Setenv("BARK_SERVER_URL", tc.server)
			t.Setenv("BARK_PUBLIC_URL", tc.public)
			t.Setenv("BARK_BASIC_AUTH_USER", tc.user)
			t.Setenv("BARK_BASIC_AUTH_PASSWORD", tc.pass)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestLoadAcceptsProductionBarkConfiguration(t *testing.T) {
	setProductionEnv(t)
	t.Setenv("BARK_SERVER_URL", "http://bark:8080")
	t.Setenv("BARK_PUBLIC_URL", "https://push.example.com")
	t.Setenv("BARK_BASIC_AUTH_USER", "user")
	t.Setenv("BARK_BASIC_AUTH_PASSWORD", "pass")
	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
}
