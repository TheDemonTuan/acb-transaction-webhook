package authrecovery

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/config"
	"github.com/thedemontuan/acb-transaction-webhook/internal/lock"
	"github.com/thedemontuan/acb-transaction-webhook/internal/security"
)

var (
	ErrRecoveryDisabled     = errors.New("automatic ACB recovery is disabled")
	ErrSingletonUnavailable = errors.New("ACB recovery singleton is unavailable")
)

// Config belongs only to the recovery controller. Do not log it: token fields
// contain secrets. Credentials are deliberately not retained here.
type Config struct {
	Enabled                  bool
	Production               bool
	DatabasePath             string
	AuthBrowserURL           string
	WorkerRPCURL             string
	MasterKeyFile            string
	AuthBrowserInternalToken string
	WorkerInternalToken      string
	TelegramBotToken         string
	TelegramChatID           int64
	TelegramUserID           int64
	AICaptchaEnabled         bool
	NineRouterBaseURL        string
	NineRouterCaptchaModel   string
	NineRouterAPIKey         string
	CaptchaTTL               time.Duration
	OTPTTL                   time.Duration
	PublicOrigin             string
}

// LoadConfig validates local configuration only; it makes no network calls.
// Disabled recovery returns before reading any integration secret or credential.
func LoadConfig() (Config, error) {
	enabled, err := strictFlag("AUTH_RECOVERY_ENABLED")
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		Enabled:        enabled,
		Production:     strings.TrimSpace(os.Getenv("APP_ENV")) == "production",
		DatabasePath:   envValue("DATABASE_PATH", "/data/gateway.db"),
		AuthBrowserURL: envValue("AUTH_BROWSER_URL", "http://auth-browser:8181"),
		WorkerRPCURL:   envValue("WORKER_RPC_URL", "http://worker:8190"),
		CaptchaTTL:     180 * time.Second,
		OTPTTL:         120 * time.Second,
	}
	if !enabled {
		return cfg, nil
	}
	if cfg.DatabasePath == ":memory:" || strings.HasPrefix(cfg.DatabasePath, "file:") {
		return Config{}, errors.New("DATABASE_PATH must be a persistent filesystem path")
	}
	cfg.AICaptchaEnabled, err = strictFlag("AI_CAPTCHA_ENABLED")
	if err != nil {
		return Config{}, err
	}
	cfg.CaptchaTTL, err = boundedSeconds("AUTH_RECOVERY_CAPTCHA_TTL_SECONDS", 180, 30, 180)
	if err != nil {
		return Config{}, err
	}
	cfg.OTPTTL, err = boundedSeconds("AUTH_RECOVERY_OTP_TTL_SECONDS", 120, 30, 120)
	if err != nil {
		return Config{}, err
	}
	cfg.AuthBrowserURL, err = validateURL("AUTH_BROWSER_URL", cfg.AuthBrowserURL, cfg.Production, false, false)
	if err != nil {
		return Config{}, err
	}
	cfg.WorkerRPCURL, err = validateURL("WORKER_RPC_URL", cfg.WorkerRPCURL, cfg.Production, false, false)
	if err != nil {
		return Config{}, err
	}
	cfg.PublicOrigin, err = validateURL("PUBLIC_ORIGIN", strings.TrimSpace(os.Getenv("PUBLIC_ORIGIN")), cfg.Production, true, false)
	if err != nil {
		return Config{}, err
	}
	cfg.MasterKeyFile = strings.TrimSpace(os.Getenv("APP_MASTER_KEY_FILE"))
	if cfg.MasterKeyFile == "" || os.Getenv("APP_MASTER_KEY") != "" {
		return Config{}, errors.New("APP_MASTER_KEY_FILE is required; inline APP_MASTER_KEY is not supported by recovery")
	}
	if _, err := config.ReadSecret("APP_MASTER_KEY", "APP_MASTER_KEY_FILE"); err != nil {
		return Config{}, errors.New("APP_MASTER_KEY_FILE cannot be read or is empty")
	}
	if _, err := security.LoadKeyring(cfg.MasterKeyFile); err != nil {
		return Config{}, errors.New("APP_MASTER_KEY_FILE must contain a valid master key")
	}
	cfg.AuthBrowserInternalToken, err = readToken("AUTH_BROWSER_INTERNAL_TOKEN", cfg.Production)
	if err != nil {
		return Config{}, err
	}
	cfg.WorkerInternalToken, err = readToken("WORKER_INTERNAL_TOKEN", cfg.Production)
	if err != nil {
		return Config{}, err
	}
	cfg.TelegramBotToken, err = readToken("TELEGRAM_BOT_TOKEN", cfg.Production)
	if err != nil {
		return Config{}, err
	}
	cfg.TelegramChatID, err = exactID("TELEGRAM_CHAT_ID")
	if err != nil {
		return Config{}, err
	}
	cfg.TelegramUserID, err = exactID("TELEGRAM_USER_ID")
	if err != nil {
		return Config{}, err
	}
	if cfg.AICaptchaEnabled {
		if err := loadAIConfig(&cfg); err != nil {
			return Config{}, err
		}
	}
	return cfg, nil
}

// LoadAIConfig reads only provider configuration; it never reads bank or bot secrets.
func LoadAIConfig() (Config, error) {
	cfg := Config{Production: strings.TrimSpace(os.Getenv("APP_ENV")) == "production"}
	var err error
	cfg.AICaptchaEnabled, err = strictFlag("AI_CAPTCHA_ENABLED")
	if err != nil {
		return Config{}, err
	}
	if !cfg.AICaptchaEnabled {
		return Config{}, errors.New("AI_CAPTCHA_DISABLED")
	}
	if err := loadAIConfig(&cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}
func loadAIConfig(cfg *Config) error {
	var err error
	cfg.NineRouterBaseURL, err = validateURL("NINEROUTER_BASE_URL", strings.TrimSpace(os.Getenv("NINEROUTER_BASE_URL")), cfg.Production, false, true)
	if err != nil {
		return err
	}
	cfg.NineRouterCaptchaModel = strings.TrimSpace(os.Getenv("NINEROUTER_CAPTCHA_MODEL"))
	if cfg.NineRouterCaptchaModel == "" || strings.ContainsAny(cfg.NineRouterCaptchaModel, "\r\n\x00") {
		return errors.New("NINEROUTER_CAPTCHA_MODEL is required and must be a single model ID")
	}
	cfg.NineRouterAPIKey, err = readToken("NINEROUTER_API_KEY", cfg.Production)
	return err
}

// RunSingleton admits the real coordinator only after acquiring its own lock.
// The callback must block until shutdown; the lock is held until it returns.
// Disabled recovery neither acquires the lock nor invokes the callback.
func RunSingleton(ctx context.Context, cfg Config, run func(context.Context) error) error {
	if !cfg.Enabled {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if run == nil || cfg.DatabasePath == "" || cfg.DatabasePath == ":memory:" || strings.HasPrefix(cfg.DatabasePath, "file:") {
		return errors.New("recovery runner requires a coordinator and persistent DATABASE_PATH")
	}
	held, err := lock.Acquire(cfg.DatabasePath + ".auth-recovery.lock")
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSingletonUnavailable, err)
	}
	defer held.Close()
	if err := ctx.Err(); err != nil {
		return err
	}
	return run(ctx)
}

func readToken(name string, production bool) (string, error) {
	if production && os.Getenv(name) != "" {
		return "", fmt.Errorf("%s must use %s_FILE in production", name, name)
	}
	if production && strings.TrimSpace(os.Getenv(name+"_FILE")) == "" {
		return "", fmt.Errorf("%s_FILE is required in production", name)
	}
	secret, err := config.ReadSecret(name, name+"_FILE")
	if err != nil || secret == "" || strings.ContainsAny(secret, "\r\n\x00") {
		return "", fmt.Errorf("%s_FILE must provide a nonempty single-line secret", name)
	}
	return secret, nil
}

func strictFlag(name string) (bool, error) {
	switch strings.TrimSpace(os.Getenv(name)) {
	case "", "false":
		return false, nil
	case "true":
		return true, nil
	default:
		return false, fmt.Errorf("%s must be true or false", name)
	}
}

func boundedSeconds(name string, fallback, min, max int) (time.Duration, error) {
	n, err := strconv.Atoi(envValue(name, strconv.Itoa(fallback)))
	if err != nil || n < min || n > max {
		return 0, fmt.Errorf("%s must be an integer from %d to %d", name, min, max)
	}
	return time.Duration(n) * time.Second, nil
}

func exactID(name string) (int64, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id == 0 {
		return 0, fmt.Errorf("%s must be a nonzero signed 64-bit integer", name)
	}
	return id, nil
}

func envValue(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func validateURL(name, raw string, production, origin, vision bool) (string, error) {
	invalid := func() (string, error) {
		return "", fmt.Errorf("%s must be a safe absolute HTTP(S) URL without userinfo, query, or fragment", name)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") || u.Opaque != "" {
		return invalid()
	}
	if !validURLHost(u.Hostname()) {
		return invalid()
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return invalid()
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return invalid()
	}
	if u.RawPath != "" || (u.Path != "" && u.Path != "/" && (!vision || u.Path != "/v1" && u.Path != "/v1/")) {
		return "", fmt.Errorf("%s has an unsupported URL path", name)
	}
	if production && origin && u.Scheme != "https" {
		return "", errors.New("PUBLIC_ORIGIN must use HTTPS in production")
	}
	if production && u.Scheme == "http" && !privateServiceHost(u.Hostname()) {
		return "", fmt.Errorf("%s HTTP requires a private Docker service hostname in production", name)
	}
	u.Host = strings.ToLower(u.Host)
	if u.Scheme == "https" && u.Port() == "443" || u.Scheme == "http" && u.Port() == "80" {
		host := u.Hostname()
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		u.Host = host
	}
	u.Path = ""
	if vision {
		u.Path = "/v1"
	}
	return u.String(), nil
}

func validURLHost(host string) bool {
	if net.ParseIP(host) != nil {
		return true
	}
	if len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if !privateServiceHost(label) {
			return false
		}
	}
	return true
}

func privateServiceHost(host string) bool {
	if net.ParseIP(host) != nil || strings.Contains(host, ".") || len(host) > 63 || host == "" || host[0] == '-' || host[len(host)-1] == '-' {
		return false
	}
	for _, char := range host {
		if char != '-' && (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') {
			return false
		}
	}
	return true
}
