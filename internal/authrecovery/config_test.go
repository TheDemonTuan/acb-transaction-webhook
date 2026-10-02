package authrecovery

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/lock"
)

func clearRecoveryEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"APP_ENV", "AUTH_RECOVERY_ENABLED", "DATABASE_PATH", "AUTH_BROWSER_URL", "WORKER_RPC_URL", "PUBLIC_ORIGIN",
		"AI_CAPTCHA_ENABLED", "NINEROUTER_BASE_URL", "NINEROUTER_CAPTCHA_MODEL",
		"TELEGRAM_CHAT_ID", "TELEGRAM_USER_ID", "AUTH_RECOVERY_CAPTCHA_TTL_SECONDS", "AUTH_RECOVERY_OTP_TTL_SECONDS",
		"APP_MASTER_KEY", "APP_MASTER_KEY_FILE", "ACB_USERNAME", "ACB_USERNAME_FILE", "ACB_PASSWORD", "ACB_PASSWORD_FILE",
		"ACB_ACCOUNT", "ACB_ACCOUNT_FILE", "AUTH_BROWSER_INTERNAL_TOKEN", "AUTH_BROWSER_INTERNAL_TOKEN_FILE",
		"WORKER_INTERNAL_TOKEN", "WORKER_INTERNAL_TOKEN_FILE", "TELEGRAM_BOT_TOKEN", "TELEGRAM_BOT_TOKEN_FILE",
		"NINEROUTER_API_KEY", "NINEROUTER_API_KEY_FILE",
	} {
		t.Setenv(name, "")
	}
}

func productionRecoveryEnv(t *testing.T) string {
	t.Helper()
	clearRecoveryEnv(t)
	dir := t.TempDir()
	t.Setenv("APP_ENV", "production")
	t.Setenv("AUTH_RECOVERY_ENABLED", "true")
	t.Setenv("DATABASE_PATH", filepath.Join(dir, "gateway.db"))
	t.Setenv("PUBLIC_ORIGIN", "https://BANK.Example.com:443/")
	t.Setenv("TELEGRAM_CHAT_ID", "9223372036854775807")
	t.Setenv("TELEGRAM_USER_ID", "-9223372036854775808")
	for name, value := range map[string]string{
		"APP_MASTER_KEY": "01234567890123456789012345678901",
		"ACB_USERNAME":   "fixture-user\n", "ACB_PASSWORD": "  fixture password \t  \r\n", "ACB_ACCOUNT": "0012345678\n",
		"AUTH_BROWSER_INTERNAL_TOKEN": "browser-token\n", "WORKER_INTERNAL_TOKEN": "worker-token\n", "TELEGRAM_BOT_TOKEN": "bot-token\n",
	} {
		path := filepath.Join(dir, strings.ToLower(name))
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(name+"_FILE", path)
	}
	return dir
}

func TestDisabledRecoveryDoesNotReadSecretsOrAcquireSingleton(t *testing.T) {
	clearRecoveryEnv(t)
	dir := t.TempDir()
	t.Setenv("APP_ENV", "production")
	t.Setenv("DATABASE_PATH", filepath.Join(dir, "gateway.db"))
	// A directory is guaranteed to fail ReadFile even when tests run as root.
	for _, name := range []string{"APP_MASTER_KEY", "ACB_USERNAME", "ACB_PASSWORD", "ACB_ACCOUNT", "AUTH_BROWSER_INTERNAL_TOKEN", "WORKER_INTERNAL_TOKEN", "TELEGRAM_BOT_TOKEN", "NINEROUTER_API_KEY"} {
		t.Setenv(name+"_FILE", dir)
	}
	t.Setenv("AI_CAPTCHA_ENABLED", "true")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.ReadCredentials(); !errors.Is(err, ErrRecoveryDisabled) {
		t.Fatalf("disabled credentials: %v", err)
	}
	called := false
	if err := RunSingleton(context.Background(), cfg, func(context.Context) error {
		called = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("disabled recovery invoked the coordinator")
	}
	if _, err := os.Stat(cfg.DatabasePath + ".auth-recovery.lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("disabled recovery created a lock: %v", err)
	}
}

func TestRecoveryCredentialsPreservePasswordAndRotate(t *testing.T) {
	productionRecoveryEnv(t)
	// AI-off must not load a provisioned but unreadable key.
	t.Setenv("NINEROUTER_API_KEY_FILE", t.TempDir())
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PublicOrigin != "https://bank.example.com" {
		t.Fatalf("noncanonical admin origin: %q", cfg.PublicOrigin)
	}
	if cfg.TelegramChatID != int64(9223372036854775807) || cfg.TelegramUserID != int64(-9223372036854775808) {
		t.Fatal("operator IDs lost signed 64-bit precision")
	}
	credentials, err := cfg.ReadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if credentials.Password != "  fixture password \t  " || credentials.AccountNumber != "0012345678" {
		t.Fatal("credentials lost meaningful spaces or leading zeros")
	}
	if err := os.WriteFile(cfg.PasswordFile, []byte(" new password "), 0o600); err != nil {
		t.Fatal(err)
	}
	credentials, err = cfg.ReadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if credentials.Password != " new password " {
		t.Fatal("the next attempt did not reread the rotated password")
	}
	if err := os.Remove(cfg.PasswordFile); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.ReadCredentials(); err == nil {
		t.Fatal("missing rotated credentials were accepted")
	}
}

func TestEnabledRecoveryRejectsUnsafeConfiguration(t *testing.T) {
	cases := []struct {
		name  string
		env   string
		value string
	}{
		{"missing bot token", "TELEGRAM_BOT_TOKEN_FILE", ""},
		{"missing master key", "APP_MASTER_KEY_FILE", ""},
		{"missing credential file", "ACB_PASSWORD_FILE", "/does-not-exist/acb_password"},
		{"inline bot token", "TELEGRAM_BOT_TOKEN", "sensitive-marker"},
		{"inline browser token", "AUTH_BROWSER_INTERNAL_TOKEN", "sensitive-marker"},
		{"inline worker token", "WORKER_INTERNAL_TOKEN", "sensitive-marker"},
		{"inline master key", "APP_MASTER_KEY", "sensitive-marker"},
		{"inline password", "ACB_PASSWORD", "sensitive-marker"},
		{"chat zero", "TELEGRAM_CHAT_ID", "0"},
		{"user missing", "TELEGRAM_USER_ID", ""},
		{"chat overflow", "TELEGRAM_CHAT_ID", "9223372036854775808"},
		{"fractional user", "TELEGRAM_USER_ID", "100.5"},
		{"captcha TTL too short", "AUTH_RECOVERY_CAPTCHA_TTL_SECONDS", "29"},
		{"captcha TTL too long", "AUTH_RECOVERY_CAPTCHA_TTL_SECONDS", "181"},
		{"OTP TTL too short", "AUTH_RECOVERY_OTP_TTL_SECONDS", "29"},
		{"OTP TTL too long", "AUTH_RECOVERY_OTP_TTL_SECONDS", "121"},
		{"OTP TTL fractional", "AUTH_RECOVERY_OTP_TTL_SECONDS", "30.1"},
		{"nonstrict enabled flag", "AUTH_RECOVERY_ENABLED", "1"},
		{"nonstrict AI flag", "AI_CAPTCHA_ENABLED", "yes"},
		{"in-memory database", "DATABASE_PATH", ":memory:"},
		{"database URI", "DATABASE_PATH", "file:gateway.db"},
		{"missing origin", "PUBLIC_ORIGIN", ""},
		{"plaintext origin", "PUBLIC_ORIGIN", "http://bank.example.com"},
		{"origin path", "PUBLIC_ORIGIN", "https://bank.example.com/admin"},
		{"origin userinfo", "PUBLIC_ORIGIN", "https://sensitive-marker@bank.example.com"},
		{"browser query", "AUTH_BROWSER_URL", "http://auth-browser:8181?sensitive-marker"},
		{"browser empty query", "AUTH_BROWSER_URL", "http://auth-browser:8181?"},
		{"browser empty fragment", "AUTH_BROWSER_URL", "http://auth-browser:8181#"},
		{"browser path", "AUTH_BROWSER_URL", "http://auth-browser:8181/sessions"},
		{"malformed origin hostname", "PUBLIC_ORIGIN", "https://bank..example.com"},
		{"malformed service hostname", "AUTH_BROWSER_URL", "http://-browser:8181"},
		{"escaped URL path", "PUBLIC_ORIGIN", "https://bank.example.com/%2f"},
		{"public plaintext worker", "WORKER_RPC_URL", "http://worker.example.com:8190"},
		{"invalid worker port", "WORKER_RPC_URL", "http://worker:65536"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			productionRecoveryEnv(t)
			t.Setenv(tc.env, tc.value)
			_, err := LoadConfig()
			if err == nil {
				t.Fatal("unsafe configuration was accepted")
			}
			if strings.Contains(err.Error(), "sensitive-marker") {
				t.Fatal("configuration error leaked a secret or URL")
			}
		})
	}
}

func TestRecoveryCredentialValidation(t *testing.T) {
	cases := []struct {
		name string
		file string
		text string
	}{
		{"empty password", "ACB_PASSWORD_FILE", "\r\n"},
		{"multiple terminal newlines", "ACB_PASSWORD_FILE", "secret\n\n"},
		{"embedded newline", "ACB_PASSWORD_FILE", "secret\nother"},
		{"NUL password", "ACB_PASSWORD_FILE", "secret\x00"},
		{"masked account", "ACB_ACCOUNT_FILE", "***1234"},
		{"empty username", "ACB_USERNAME_FILE", "  \n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			productionRecoveryEnv(t)
			if err := os.WriteFile(os.Getenv(tc.file), []byte(tc.text), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadConfig(); err == nil {
				t.Fatal("invalid credentials were accepted")
			}
		})
	}
}

func TestRecoveryAIConfigurationAndURLContract(t *testing.T) {
	for _, base := range []string{"https://vision.example.com", "https://vision.example.com/v1", "https://vision.example.com/v1/", "http://ninerouter:20128/v1"} {
		t.Run(base, func(t *testing.T) {
			dir := productionRecoveryEnv(t)
			t.Setenv("AI_CAPTCHA_ENABLED", "true")
			t.Setenv("NINEROUTER_BASE_URL", base)
			t.Setenv("NINEROUTER_CAPTCHA_MODEL", "configured-vision-model")
			key := filepath.Join(dir, "ninerouter_key")
			if err := os.WriteFile(key, []byte("vision-key\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("NINEROUTER_API_KEY_FILE", key)
			t.Setenv("AUTH_RECOVERY_CAPTCHA_TTL_SECONDS", "30")
			t.Setenv("AUTH_RECOVERY_OTP_TTL_SECONDS", "30")
			cfg, err := LoadConfig()
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(cfg.NineRouterBaseURL, "/v1") != 1 || !strings.HasSuffix(cfg.NineRouterBaseURL, "/v1") {
				t.Fatal("model endpoint would not contain exactly one /v1 prefix")
			}
			if cfg.CaptchaTTL != 30*time.Second || cfg.OTPTTL != 30*time.Second {
				t.Fatal("minimum valid challenge TTL was not applied")
			}
		})
	}
	for _, tc := range []struct{ name, value string }{
		{"NINEROUTER_BASE_URL", ""},
		{"NINEROUTER_BASE_URL", "http://public.example.com/v1"},
		{"NINEROUTER_BASE_URL", "http://127.0.0.1:20128/v1"},
		{"NINEROUTER_BASE_URL", "https://vision.example.com/v1/v1"},
		{"NINEROUTER_BASE_URL", "https://sensitive-marker@vision.example.com/v1"},
		{"NINEROUTER_BASE_URL", "https://vision.example.com/v1?sensitive-marker"},
		{"NINEROUTER_CAPTCHA_MODEL", ""},
		{"NINEROUTER_API_KEY_FILE", ""},
		{"NINEROUTER_API_KEY", "sensitive-marker"},
	} {
		t.Run(tc.name+tc.value, func(t *testing.T) {
			dir := productionRecoveryEnv(t)
			t.Setenv("AI_CAPTCHA_ENABLED", "true")
			t.Setenv("NINEROUTER_BASE_URL", "https://vision.example.com/v1")
			t.Setenv("NINEROUTER_CAPTCHA_MODEL", "configured-model")
			key := filepath.Join(dir, "vision-key")
			if err := os.WriteFile(key, []byte("key"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("NINEROUTER_API_KEY_FILE", key)
			t.Setenv(tc.name, tc.value)
			if _, err := LoadConfig(); err == nil {
				t.Fatal("unsafe or incomplete AI configuration was accepted")
			} else if strings.Contains(err.Error(), "sensitive-marker") {
				t.Fatal("AI configuration leaked a secret")
			}
		})
	}
}

func TestRecoverySingletonBlocksOtherProcessAndReleasesOnFailure(t *testing.T) {
	cfg := Config{Enabled: true, DatabasePath: filepath.Join(t.TempDir(), "gateway.db")}
	failure := errors.New("coordinator stopped")
	called := 0
	err := RunSingleton(context.Background(), cfg, func(context.Context) error {
		called++
		if err := RunSingleton(context.Background(), cfg, func(context.Context) error {
			t.Fatal("second coordinator ran without the lock")
			return nil
		}); !errors.Is(err, ErrSingletonUnavailable) {
			t.Fatalf("second admission error: %v", err)
		}
		child := exec.Command(os.Args[0], "-test.run=^TestRecoverySingletonChildProcess$")
		child.Env = append(os.Environ(), "ACB_TEST_RECOVERY_LOCK="+cfg.DatabasePath+".auth-recovery.lock")
		if output, err := child.CombinedOutput(); err != nil {
			t.Fatalf("cross-process lock check: %v\n%s", err, output)
		}
		return failure
	})
	if !errors.Is(err, failure) || called != 1 {
		t.Fatalf("runner did not preserve coordinator outcome: %v, calls=%d", err, called)
	}
	if err := RunSingleton(context.Background(), cfg, func(context.Context) error {
		called++
		return nil
	}); err != nil || called != 2 {
		t.Fatalf("lock was not released after failure: %v, calls=%d", err, called)
	}
}

func TestRecoverySingletonChildProcess(t *testing.T) {
	path := os.Getenv("ACB_TEST_RECOVERY_LOCK")
	if path == "" {
		return
	}
	held, err := lock.Acquire(path)
	if err == nil {
		_ = held.Close()
		t.Fatal("another process acquired the held recovery lock")
	}
}

func TestRecoverySingletonCancellationAndInvalidRunner(t *testing.T) {
	cfg := Config{Enabled: true, DatabasePath: filepath.Join(t.TempDir(), "gateway.db")}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := RunSingleton(ctx, cfg, func(context.Context) error {
		t.Fatal("canceled coordinator was admitted")
		return nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	if _, err := os.Stat(cfg.DatabasePath + ".auth-recovery.lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled admission created a lock: %v", err)
	}
	if err := RunSingleton(context.Background(), cfg, nil); err == nil {
		t.Fatal("nil coordinator was accepted")
	}
	cfg.DatabasePath = filepath.Join(t.TempDir(), "missing", "gateway.db")
	if err := RunSingleton(context.Background(), cfg, func(context.Context) error {
		t.Fatal("coordinator ran with inaccessible lock storage")
		return nil
	}); !errors.Is(err, ErrSingletonUnavailable) {
		t.Fatalf("inaccessible lock storage: %v", err)
	}
}
