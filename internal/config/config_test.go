package config

import (
	"os"
	"path/filepath"
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

func TestProductionRequiresTTSToken(t *testing.T) {
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
	t.Setenv("TTS_GATEWAY_URL", "http://tts-gateway:8081")
	t.Setenv("TTS_INTERNAL_TOKEN", "")
	t.Setenv("TTS_INTERNAL_TOKEN_FILE", "")

	_, err := Load()
	if err == nil {
		t.Fatal("expected production Load without TTS token to fail, but it succeeded")
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
