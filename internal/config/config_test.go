package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadTTSInternalTokenFile(t *testing.T) {
	tempDir := t.TempDir()
	tokenPath := filepath.Join(tempDir, "token.txt")
	if err := os.WriteFile(tokenPath, []byte("  secret-token-123  \n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	t.Setenv("TTS_INTERNAL_TOKEN_FILE", tokenPath)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("expected Load to succeed, got: %v", err)
	}
	if cfg.TTSInternalToken != "secret-token-123" {
		t.Fatalf("expected 'secret-token-123', got %q", cfg.TTSInternalToken)
	}
}

func TestLoadTTSInternalTokenFileMissing(t *testing.T) {
	t.Setenv("TTS_INTERNAL_TOKEN_FILE", "/non/existent/path/to/token.txt")
	_, err := Load()
	if err == nil {
		t.Fatal("expected Load to fail on missing token file, but it succeeded")
	}
}

func TestLoadTTSInternalTokenFileEmpty(t *testing.T) {
	tempDir := t.TempDir()
	tokenPath := filepath.Join(tempDir, "empty_token.txt")
	if err := os.WriteFile(tokenPath, []byte("   \n"), 0o600); err != nil {
		t.Fatalf("write empty token file: %v", err)
	}

	t.Setenv("TTS_INTERNAL_TOKEN_FILE", tokenPath)
	_, err := Load()
	if err == nil {
		t.Fatal("expected Load to fail on empty token file, but it succeeded")
	}
}

func TestProductionWorkerDoesNotRequireTTSToken(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte("32byteslongkeyforproductiontest!"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	t.Setenv("APP_ENV", "production")
	t.Setenv("APP_MASTER_KEY_FILE", keyFile)
	t.Setenv("RUNTIME_ROLE", "worker")
	t.Setenv("WORKER_INTERNAL_TOKEN", "worker-token")
	t.Setenv("TTS_INTERNAL_TOKEN", "")
	t.Setenv("TTS_INTERNAL_TOKEN_FILE", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("expected worker Load without TTS token to succeed, got: %v", err)
	}
	if cfg.TTSGatewayURL != "" {
		t.Fatalf("expected worker TTSGatewayURL to default to empty, got %q", cfg.TTSGatewayURL)
	}
}

func TestProductionGatewayRequiresTTSToken(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte("32byteslongkeyforproductiontest!"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	t.Setenv("APP_ENV", "production")
	t.Setenv("APP_MASTER_KEY_FILE", keyFile)
	t.Setenv("OWNER_SUBJECTS", "owner@example.com")
	t.Setenv("CLOUDFLARE_ACCESS_ISSUER", "https://test.cloudflareaccess.com")
	t.Setenv("CLOUDFLARE_ACCESS_AUD", "aud123")
	t.Setenv("CLOUDFLARE_ACCESS_JWKS_URL", "https://test.cloudflareaccess.com/certs")
	t.Setenv("RUNTIME_ROLE", "gateway")
	t.Setenv("WORKER_RPC_URL", "http://worker:8190")
	t.Setenv("WORKER_INTERNAL_TOKEN", "worker-token")
	t.Setenv("TTS_GATEWAY_URL", "http://tts-gateway:8081")
	t.Setenv("TTS_INTERNAL_TOKEN", "")
	t.Setenv("TTS_INTERNAL_TOKEN_FILE", "")

	_, err := Load()
	if err == nil {
		t.Fatal("expected production gateway Load without TTS token to fail, but it succeeded")
	}
}

func TestProductionGatewayDoesNotRequireBarkCredentials(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte("32byteslongkeyforproductiontest!"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	t.Setenv("APP_ENV", "production")
	t.Setenv("APP_MASTER_KEY_FILE", keyFile)
	t.Setenv("OWNER_SUBJECTS", "owner@example.com")
	t.Setenv("CLOUDFLARE_ACCESS_ISSUER", "https://test.cloudflareaccess.com")
	t.Setenv("CLOUDFLARE_ACCESS_AUD", "aud123")
	t.Setenv("CLOUDFLARE_ACCESS_JWKS_URL", "https://test.cloudflareaccess.com/certs")
	t.Setenv("RUNTIME_ROLE", "gateway")
	t.Setenv("WORKER_RPC_URL", "http://worker:8190")
	t.Setenv("WORKER_INTERNAL_TOKEN", "worker-token")
	t.Setenv("TTS_INTERNAL_TOKEN", "mock-tts-token")
	t.Setenv("BARK_SERVER_URL", "http://bark:8080")
	t.Setenv("BARK_BASIC_AUTH_USER", "")
	t.Setenv("BARK_BASIC_AUTH_PASSWORD", "")

	_, err := Load()
	if err != nil {
		t.Fatalf("expected gateway Load without Bark credentials to succeed, got: %v", err)
	}
}

func TestProductionWorkerRequiresBarkCredentialsWhenBarkConfigured(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte("32byteslongkeyforproductiontest!"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	t.Setenv("APP_ENV", "production")
	t.Setenv("APP_MASTER_KEY_FILE", keyFile)
	t.Setenv("RUNTIME_ROLE", "worker")
	t.Setenv("WORKER_INTERNAL_TOKEN", "worker-token")
	t.Setenv("BARK_SERVER_URL", "http://bark:8080")
	t.Setenv("BARK_BASIC_AUTH_USER", "")
	t.Setenv("BARK_BASIC_AUTH_PASSWORD", "")

	_, err := Load()
	if err == nil {
		t.Fatal("expected production worker Load with Bark server but no credentials to fail")
	}
}

func TestProductionRejectsDirectAppMasterKeyWithoutFile(t *testing.T) {
	t.Setenv("APP_ENV", "production")
	t.Setenv("RUNTIME_ROLE", "worker")
	t.Setenv("WORKER_INTERNAL_TOKEN", "worker-token")
	t.Setenv("APP_MASTER_KEY", "32byteslongkeyforproductiontest!")
	t.Setenv("APP_MASTER_KEY_FILE", "")

	_, err := Load()
	if err == nil {
		t.Fatal("expected production Load with direct APP_MASTER_KEY and no file to fail")
	}
}

func TestLoadWorkerInternalTokenFileTrimCRLF(t *testing.T) {
	tempDir := t.TempDir()
	tokenPath := filepath.Join(tempDir, "worker_token.txt")
	if err := os.WriteFile(tokenPath, []byte("  worker-secret-456 \r\n\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	t.Setenv("WORKER_INTERNAL_TOKEN_FILE", tokenPath)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("expected Load to succeed, got: %v", err)
	}
	if cfg.WorkerInternalToken != "worker-secret-456" {
		t.Fatalf("expected 'worker-secret-456', got %q", cfg.WorkerInternalToken)
	}
}

func TestProductionRequiresWorkerTokenWhenRPCConfigured(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte("32byteslongkeyforproductiontest!"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	t.Setenv("APP_ENV", "production")
	t.Setenv("RUNTIME_ROLE", "gateway")
	t.Setenv("APP_MASTER_KEY_FILE", keyFile)
	t.Setenv("OWNER_SUBJECTS", "owner@example.com")
	t.Setenv("CLOUDFLARE_ACCESS_ISSUER", "https://test.cloudflareaccess.com")
	t.Setenv("CLOUDFLARE_ACCESS_AUD", "aud123")
	t.Setenv("CLOUDFLARE_ACCESS_JWKS_URL", "https://test.cloudflareaccess.com/certs")
	t.Setenv("WORKER_RPC_URL", "http://acb-worker:8190")
	t.Setenv("WORKER_INTERNAL_TOKEN", "")
	t.Setenv("WORKER_INTERNAL_TOKEN_FILE", "")

	_, err := Load()
	if err == nil {
		t.Fatal("expected Load to fail when WORKER_INTERNAL_TOKEN is missing in production with WORKER_RPC_URL, but it succeeded")
	}
}

func setupBaseProductionEnv(t *testing.T) {
	t.Helper()
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte("32byteslongkeyforproductiontest!"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	t.Setenv("APP_ENV", "production")
	t.Setenv("APP_MASTER_KEY_FILE", keyFile)
	t.Setenv("OWNER_SUBJECTS", "owner@example.com")
	t.Setenv("CF_ACCESS_ISSUER", "https://test.cloudflareaccess.com")
	t.Setenv("CF_ACCESS_AUDIENCE", "aud123")
	t.Setenv("CF_ACCESS_JWKS_URL", "https://test.cloudflareaccess.com/certs")
	t.Setenv("TTS_INTERNAL_TOKEN", "mock-tts-token")
}

func TestProductionRejectsEmptyRole(t *testing.T) {
	setupBaseProductionEnv(t)
	t.Setenv("RUNTIME_ROLE", "")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "RUNTIME_ROLE is required in production") {
		t.Fatalf("expected error mentioning RUNTIME_ROLE required in production, got %v", err)
	}
}

func TestProductionRejectsMonolithDev(t *testing.T) {
	setupBaseProductionEnv(t)
	t.Setenv("RUNTIME_ROLE", "monolith-dev")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "monolith-dev role is forbidden in production") {
		t.Fatalf("expected error mentioning monolith-dev role is forbidden in production, got %v", err)
	}
}

func TestProductionRejectsUnknownRole(t *testing.T) {
	setupBaseProductionEnv(t)
	t.Setenv("RUNTIME_ROLE", "invalid-worker-role")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "invalid RUNTIME_ROLE") {
		t.Fatalf("expected error mentioning invalid RUNTIME_ROLE, got %v", err)
	}
}

func TestProductionGatewayRequiresWorkerRPCAndToken(t *testing.T) {
	setupBaseProductionEnv(t)
	t.Setenv("RUNTIME_ROLE", "gateway")

	// 1. Missing WORKER_RPC_URL
	t.Setenv("WORKER_RPC_URL", "")
	t.Setenv("WORKER_INTERNAL_TOKEN", "some-token")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "WORKER_RPC_URL is required for gateway in production") {
		t.Fatalf("expected error mentioning WORKER_RPC_URL required for gateway, got %v", err)
	}

	// 2. Missing WORKER_INTERNAL_TOKEN
	t.Setenv("WORKER_RPC_URL", "http://acb-worker:8190")
	t.Setenv("WORKER_INTERNAL_TOKEN", "")
	t.Setenv("WORKER_INTERNAL_TOKEN_FILE", "")
	_, err = Load()
	if err == nil || !strings.Contains(err.Error(), "WORKER_INTERNAL_TOKEN or WORKER_INTERNAL_TOKEN_FILE is required for gateway in production") {
		t.Fatalf("expected error mentioning WORKER_INTERNAL_TOKEN required for gateway, got %v", err)
	}

	// 3. Both present -> succeeds
	t.Setenv("WORKER_INTERNAL_TOKEN", "worker-token")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("expected valid gateway config to succeed, got %v", err)
	}
	if cfg.RuntimeRole != RuntimeRoleGateway {
		t.Fatalf("expected RuntimeRoleGateway, got %q", cfg.RuntimeRole)
	}
}

func TestProductionWorkerRequiresToken(t *testing.T) {
	setupBaseProductionEnv(t)
	t.Setenv("RUNTIME_ROLE", "worker")

	// 1. Missing token
	t.Setenv("WORKER_INTERNAL_TOKEN", "")
	t.Setenv("WORKER_INTERNAL_TOKEN_FILE", "")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "WORKER_INTERNAL_TOKEN or WORKER_INTERNAL_TOKEN_FILE is required for worker in production") {
		t.Fatalf("expected error mentioning WORKER_INTERNAL_TOKEN required for worker, got %v", err)
	}

	// 2. Token present -> succeeds
	t.Setenv("WORKER_INTERNAL_TOKEN", "worker-token")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("expected valid worker config to succeed, got %v", err)
	}
	if cfg.RuntimeRole != RuntimeRoleWorker {
		t.Fatalf("expected RuntimeRoleWorker, got %q", cfg.RuntimeRole)
	}
}

func TestNonProductionRoleDefaultsAndValidation(t *testing.T) {
	t.Setenv("APP_ENV", "development")

	// 1. Empty RUNTIME_ROLE defaults to monolith-dev
	t.Setenv("RUNTIME_ROLE", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("expected dev Load to succeed, got %v", err)
	}
	if cfg.RuntimeRole != RuntimeRoleMonolithDev {
		t.Fatalf("expected default RuntimeRoleMonolithDev in dev, got %q", cfg.RuntimeRole)
	}

	// 2. Explicit monolith-dev allowed
	t.Setenv("RUNTIME_ROLE", "monolith-dev")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("expected explicit monolith-dev to succeed, got %v", err)
	}
	if cfg.RuntimeRole != RuntimeRoleMonolithDev {
		t.Fatalf("expected RuntimeRoleMonolithDev, got %q", cfg.RuntimeRole)
	}

	// 3. Explicit gateway allowed
	t.Setenv("RUNTIME_ROLE", "gateway")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("expected explicit gateway to succeed in dev, got %v", err)
	}
	if cfg.RuntimeRole != RuntimeRoleGateway {
		t.Fatalf("expected RuntimeRoleGateway, got %q", cfg.RuntimeRole)
	}

	// 4. Explicit worker allowed
	t.Setenv("RUNTIME_ROLE", "worker")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("expected explicit worker to succeed in dev, got %v", err)
	}
	if cfg.RuntimeRole != RuntimeRoleWorker {
		t.Fatalf("expected RuntimeRoleWorker, got %q", cfg.RuntimeRole)
	}

	// 5. Invalid role rejected in non-production
	t.Setenv("RUNTIME_ROLE", "invalid-role")
	_, err = Load()
	if err == nil || !strings.Contains(err.Error(), "invalid RUNTIME_ROLE") {
		t.Fatalf("expected error for invalid RUNTIME_ROLE in dev, got %v", err)
	}
}
