package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type RuntimeRole string

const (
	RuntimeRoleGateway     RuntimeRole = "gateway"
	RuntimeRoleWorker      RuntimeRole = "worker"
	RuntimeRoleMonolithDev RuntimeRole = "monolith-dev"
)

type RoleSubjects struct {
	Owners    map[string]struct{}
	Operators map[string]struct{}
	Viewers   map[string]struct{}
}

type Config struct {
	RuntimeRole        RuntimeRole
	Address            string
	DatabasePath       string
	MasterKeyFile      string
	Timezone           *time.Location
	CloudflareIssuer   string
	CloudflareAudience string
	CloudflareJWKSURL  string
	Roles              RoleSubjects
	DevelopmentSubject string
	Production         bool
	PublicOrigin       string
	// payOS credential fields support immutable service snapshots and injected
	// fixtures only. Load never populates these or the legacy payment flags;
	// production reads owner-managed encrypted configuration from shared storage.
	PayOSClientID         string
	PayOSAPIKey           string
	PayOSChecksumKey      string
	PaymentPublicOrigin   string
	PaymentsEnabled       bool
	PayOSWebhookConfirmed bool
	PaymentMaxAmountVND   int64
	SePayStoreConfigJSON  string `json:"-"`
	TTSGatewayURL         string
	TTSInternalToken      string
	BarkServerURL         string
	BarkPublicURL         string
	BarkBasicAuthUser     string
	BarkBasicAuthPassword string
	BarkTimeout           time.Duration
	BarkDefaultGroup      string
	BarkDefaultLevel      string
	BarkDefaultSound      string
	WorkerRPCURL          string
	WorkerRealtimeURL     string
	WorkerRealtimeEnabled bool
	WorkerInternalToken   string
	Slot                  string
	ReleaseCommit         string
}

func Load() (Config, error) {
	loc, err := time.LoadLocation(value("TZ", "Asia/Ho_Chi_Minh"))
	if err != nil {
		return Config{}, fmt.Errorf("load timezone: %w", err)
	}
	dataDir := value("DATA_DIR", "data")
	production := value("APP_ENV", "development") == "production"

	rawRole := strings.TrimSpace(os.Getenv("RUNTIME_ROLE"))
	var runtimeRole RuntimeRole
	if production {
		if rawRole == "" {
			return Config{}, fmt.Errorf("RUNTIME_ROLE is required in production (must be %q or %q)", RuntimeRoleGateway, RuntimeRoleWorker)
		}
		switch RuntimeRole(rawRole) {
		case RuntimeRoleGateway:
			runtimeRole = RuntimeRoleGateway
		case RuntimeRoleWorker:
			runtimeRole = RuntimeRoleWorker
		case RuntimeRoleMonolithDev:
			return Config{}, fmt.Errorf("monolith-dev role is forbidden in production")
		default:
			return Config{}, fmt.Errorf("invalid RUNTIME_ROLE: %q (must be %q or %q in production)", rawRole, RuntimeRoleGateway, RuntimeRoleWorker)
		}
	} else {
		if rawRole == "" {
			runtimeRole = RuntimeRoleMonolithDev
		} else {
			switch RuntimeRole(rawRole) {
			case RuntimeRoleGateway:
				runtimeRole = RuntimeRoleGateway
			case RuntimeRoleWorker:
				runtimeRole = RuntimeRoleWorker
			case RuntimeRoleMonolithDev:
				runtimeRole = RuntimeRoleMonolithDev
			default:
				return Config{}, fmt.Errorf("invalid RUNTIME_ROLE: %q (must be %q, %q, or %q)", rawRole, RuntimeRoleGateway, RuntimeRoleWorker, RuntimeRoleMonolithDev)
			}
		}
	}

	cfTeam := os.Getenv("CLOUDFLARE_ACCESS_TEAM_NAME")
	cfIssuer := strings.TrimSuffix(os.Getenv("CF_ACCESS_ISSUER"), "/")
	if cfIssuer == "" {
		cfIssuer = strings.TrimSuffix(os.Getenv("CLOUDFLARE_ACCESS_ISSUER"), "/")
	}
	if cfIssuer == "" && cfTeam != "" {
		cfIssuer = fmt.Sprintf("https://%s.cloudflareaccess.com", cfTeam)
	}
	cfAud := os.Getenv("CF_ACCESS_AUDIENCE")
	if cfAud == "" {
		cfAud = os.Getenv("CLOUDFLARE_ACCESS_AUD")
	}
	cfJWKS := os.Getenv("CF_ACCESS_JWKS_URL")
	if cfJWKS == "" {
		cfJWKS = os.Getenv("CLOUDFLARE_ACCESS_JWKS_URL")
	}
	if cfJWKS == "" && cfTeam != "" {
		cfJWKS = fmt.Sprintf("https://%s.cloudflareaccess.com/cdn-cgi/access/certs", cfTeam)
	}

	masterKeyFile := os.Getenv("APP_MASTER_KEY_FILE")
	if !production {
		if masterKeyFile == "" && os.Getenv("APP_MASTER_KEY") != "" {
			absDataDir, _ := filepath.Abs(dataDir)
			autoKey := filepath.Join(absDataDir, "app_master_key")
			_ = os.MkdirAll(absDataDir, 0o700)
			_ = os.WriteFile(autoKey, []byte(os.Getenv("APP_MASTER_KEY")), 0o600)
			masterKeyFile = autoKey
		} else if masterKeyFile == "" {
			absDataDir, _ := filepath.Abs(dataDir)
			autoKey := filepath.Join(absDataDir, "dev_master.key")
			if _, err := os.Stat(autoKey); os.IsNotExist(err) {
				_ = os.MkdirAll(absDataDir, 0o700)
				_ = os.WriteFile(autoKey, []byte("0123456789012345678901234567890123456789012345678901234567890123"), 0o600)
			}
			masterKeyFile = autoKey
		}
	}

	owners := set("OWNER_SUBJECTS")
	if len(owners) == 0 {
		if ownerDefault := os.Getenv("CLOUDFLARE_ACCESS_OWNER_EMAIL"); ownerDefault != "" {
			owners[ownerDefault] = struct{}{}
		}
	}

	publicOrigin := strings.TrimSpace(os.Getenv("PUBLIC_ORIGIN"))
	if publicOrigin != "" {
		if u, err := url.Parse(publicOrigin); err == nil && u.Scheme != "" && u.Host != "" {
			publicOrigin = strings.TrimSuffix(fmt.Sprintf("%s://%s", strings.ToLower(u.Scheme), u.Host), "/")
		}
	}

	// payOS credentials are supplied exclusively through the encrypted owner API.
	// Ignore stale environment values and secret-file paths during cutover.
	paymentOriginDefault := "http://localhost:5173"
	if production {
		paymentOriginDefault = "https://transactions.tuannguyenviet.site"
	}
	paymentPublicOrigin, err := paymentOrigin(value("PAYMENT_PUBLIC_ORIGIN", paymentOriginDefault), production)
	if err != nil {
		return Config{}, err
	}
	paymentMaxAmountVND, err := strconv.ParseInt(value("PAYMENT_MAX_AMOUNT_VND", "500000000"), 10, 64)
	if err != nil || paymentMaxAmountVND < 1 || paymentMaxAmountVND > 9007199254740991 {
		return Config{}, fmt.Errorf("PAYMENT_MAX_AMOUNT_VND must be an integer from 1 to 9007199254740991")
	}

	sepayStoreConfigJSON, err := ReadSecret("", "SEPAY_STORE_CONFIG_FILE")
	if err != nil {
		return Config{}, err
	}

	ttsToken, err := ReadSecret("TTS_INTERNAL_TOKEN", "TTS_INTERNAL_TOKEN_FILE")
	if err != nil {
		return Config{}, err
	}

	barkServerURL := strings.TrimSpace(os.Getenv("BARK_SERVER_URL"))
	barkPublicURL := strings.TrimSpace(os.Getenv("BARK_PUBLIC_URL"))
	barkAuthUser, err := ReadSecret("BARK_BASIC_AUTH_USER", "BARK_BASIC_AUTH_USER_FILE")
	if err != nil {
		return Config{}, err
	}
	barkAuthPassword, err := ReadSecret("BARK_BASIC_AUTH_PASSWORD", "BARK_BASIC_AUTH_PASSWORD_FILE")
	if err != nil {
		return Config{}, err
	}

	barkTimeoutMs := 5000
	if raw := strings.TrimSpace(os.Getenv("BARK_TIMEOUT_MS")); raw != "" {
		ms, err := strconv.Atoi(raw)
		if err != nil || ms < 500 || ms > 15000 {
			return Config{}, fmt.Errorf("BARK_TIMEOUT_MS must be an integer from 500 to 15000")
		}
		barkTimeoutMs = ms
	}
	barkGroup := value("BARK_DEFAULT_GROUP", "ACB")
	barkLevel := value("BARK_DEFAULT_LEVEL", "timeSensitive")
	barkSound := value("BARK_DEFAULT_SOUND", "shake")

	if barkServerURL != "" {
		u, err := url.Parse(barkServerURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return Config{}, fmt.Errorf("BARK_SERVER_URL must be an absolute http or https URL")
		}
		if u.Fragment != "" || u.RawQuery != "" || u.User != nil {
			return Config{}, fmt.Errorf("BARK_SERVER_URL must not contain userinfo, query, or fragment")
		}
	}
	if barkPublicURL != "" {
		u, err := url.Parse(barkPublicURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return Config{}, fmt.Errorf("BARK_PUBLIC_URL must be an absolute http or https URL")
		}
		if u.Fragment != "" || u.RawQuery != "" || u.User != nil {
			return Config{}, fmt.Errorf("BARK_PUBLIC_URL must not contain userinfo, query, or fragment")
		}
		if production && u.Scheme != "https" {
			return Config{}, fmt.Errorf("BARK_PUBLIC_URL must use https in production")
		}
	}
	if (barkAuthUser == "") != (barkAuthPassword == "") {
		return Config{}, fmt.Errorf("Bark basic auth user and password must be configured together")
	}
	if production && runtimeRole == RuntimeRoleWorker && barkServerURL != "" && barkAuthUser == "" {
		return Config{}, fmt.Errorf("Bark basic auth user and password are required in production")
	}

	workerToken, err := ReadSecret("WORKER_INTERNAL_TOKEN", "WORKER_INTERNAL_TOKEN_FILE")
	if err != nil {
		return Config{}, err
	}

	workerRealtimeEnabled, err := boolean("WORKER_REALTIME_ENABLED", false)
	if err != nil {
		return Config{}, err
	}
	workerRealtimeURL := strings.TrimRight(value("WORKER_REALTIME_URL", "http://acb-worker:8191"), "/")
	if workerRealtimeEnabled {
		u, parseErr := url.Parse(workerRealtimeURL)
		if parseErr != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return Config{}, fmt.Errorf("WORKER_REALTIME_URL must be an absolute http or https URL")
		}
		if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return Config{}, fmt.Errorf("WORKER_REALTIME_URL must not contain userinfo, query, or fragment")
		}
	}

	slot := value("PLATFORM_SLOT", value("APP_SLOT", value("SLOT", "monolith")))
	releaseCommit := value("RELEASE_COMMIT", value("APP_RELEASE_COMMIT", value("GIT_COMMIT", "unknown")))

	ttsGatewayURL := ""
	if runtimeRole != RuntimeRoleWorker {
		ttsGatewayURL = value("TTS_GATEWAY_URL", "http://tts-gateway:8081")
	} else if v := strings.TrimSpace(os.Getenv("TTS_GATEWAY_URL")); v != "" {
		ttsGatewayURL = v
	}

	cfg := Config{
		RuntimeRole:        runtimeRole,
		Address:            value("LISTEN_ADDR", "0.0.0.0:"+value("PORT", "8090")),
		DatabasePath:       value("DATABASE_PATH", filepath.Join(dataDir, "gateway.db")),
		MasterKeyFile:      masterKeyFile,
		Timezone:           loc,
		CloudflareIssuer:   cfIssuer,
		CloudflareAudience: cfAud,
		CloudflareJWKSURL:  cfJWKS,
		Roles: RoleSubjects{
			Owners:    owners,
			Operators: set("OPERATOR_SUBJECTS"),
			Viewers:   set("VIEWER_SUBJECTS"),
		},
		DevelopmentSubject:   value("DEVELOPMENT_SUBJECT", "local-owner"),
		Production:           production,
		PublicOrigin:         publicOrigin,
		PaymentPublicOrigin:  paymentPublicOrigin,
		PaymentMaxAmountVND:  paymentMaxAmountVND,
		SePayStoreConfigJSON: sepayStoreConfigJSON,
		TTSGatewayURL:        ttsGatewayURL,

		TTSInternalToken:      ttsToken,
		BarkServerURL:         barkServerURL,
		BarkPublicURL:         barkPublicURL,
		BarkBasicAuthUser:     barkAuthUser,
		BarkBasicAuthPassword: barkAuthPassword,
		BarkTimeout:           time.Duration(barkTimeoutMs) * time.Millisecond,
		BarkDefaultGroup:      barkGroup,
		BarkDefaultLevel:      barkLevel,
		BarkDefaultSound:      barkSound,
		WorkerRPCURL:          value("WORKER_RPC_URL", ""),
		WorkerRealtimeURL:     workerRealtimeURL,
		WorkerRealtimeEnabled: workerRealtimeEnabled,
		WorkerInternalToken:   workerToken,
		Slot:                  slot,
		ReleaseCommit:         releaseCommit,
	}
	if production {
		if cfg.MasterKeyFile == "" {
			return Config{}, fmt.Errorf("APP_MASTER_KEY_FILE is required in production")
		}
		if cfg.RuntimeRole == RuntimeRoleGateway {
			if len(cfg.Roles.Owners) == 0 {

				return Config{}, fmt.Errorf("OWNER_SUBJECTS is required in production")
			}
			if cfg.CloudflareIssuer == "" || cfg.CloudflareAudience == "" || cfg.CloudflareJWKSURL == "" || cfg.CloudflareAudience == "*" || strings.EqualFold(cfg.CloudflareAudience, "any") {
				return Config{}, fmt.Errorf("specific Cloudflare Access issuer, audience, and JWKS URL are required in production")
			}
			if cfg.TTSGatewayURL != "" && cfg.TTSInternalToken == "" {
				return Config{}, fmt.Errorf("TTS_INTERNAL_TOKEN or TTS_INTERNAL_TOKEN_FILE is required in production when TTS_GATEWAY_URL is configured")
			}
			if cfg.WorkerRPCURL == "" {
				return Config{}, fmt.Errorf("WORKER_RPC_URL is required for gateway in production")
			}
			if cfg.WorkerRealtimeEnabled && cfg.WorkerRealtimeURL == "" {
				return Config{}, fmt.Errorf("WORKER_REALTIME_URL is required when WORKER_REALTIME_ENABLED is true")
			}
			if cfg.WorkerInternalToken == "" {
				return Config{}, fmt.Errorf("WORKER_INTERNAL_TOKEN or WORKER_INTERNAL_TOKEN_FILE is required for gateway in production")
			}
		}
		if cfg.RuntimeRole == RuntimeRoleWorker {
			if cfg.WorkerInternalToken == "" {
				return Config{}, fmt.Errorf("WORKER_INTERNAL_TOKEN or WORKER_INTERNAL_TOKEN_FILE is required for worker in production")
			}
		}
	}
	if cfg.MasterKeyFile != "" && !filepath.IsAbs(cfg.MasterKeyFile) {
		return Config{}, fmt.Errorf("APP_MASTER_KEY_FILE must be an absolute path")
	}
	return cfg, nil
}

// PaymentStaticURL is the fixed customer-facing URL, never derived from request headers.
func (c Config) PaymentStaticURL() string {
	return c.PaymentPublicOrigin + "/pay"
}

// PayOSWebhookURL is the callback registered on the application's dedicated payOS channel.
func (c Config) PayOSWebhookURL() string {
	return c.PaymentPublicOrigin + "/api/integrations/payos/webhook"
}

func paymentOrigin(raw string, production bool) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Hostname() == "" || u.Opaque != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", fmt.Errorf("PAYMENT_PUBLIC_ORIGIN must be an absolute HTTPS origin (localhost HTTP is allowed in development)")
	}
	if u.User != nil || u.Path != "" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") {
		return "", fmt.Errorf("PAYMENT_PUBLIC_ORIGIN must not contain userinfo, path, query, or fragment")
	}
	host := strings.ToLower(u.Hostname())
	ip := net.ParseIP(host)
	if (strings.Contains(host, ":") || strings.HasPrefix(u.Host, "[")) && ip == nil {
		return "", fmt.Errorf("PAYMENT_PUBLIC_ORIGIN must contain a valid hostname or IP address")
	}
	if u.Scheme == "http" {
		if production || (host != "localhost" && (ip == nil || !ip.IsLoopback())) {
			return "", fmt.Errorf("PAYMENT_PUBLIC_ORIGIN must use HTTPS except for localhost in development")
		}
	}
	port := u.Port()
	if strings.HasSuffix(u.Host, ":") {
		return "", fmt.Errorf("PAYMENT_PUBLIC_ORIGIN must contain a valid port")
	}
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", fmt.Errorf("PAYMENT_PUBLIC_ORIGIN must contain a valid port")
		}
		port = strconv.Itoa(n)
		if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
			port = ""
		}
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return u.Scheme + "://" + host, nil
}

func value(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
func set(key string) map[string]struct{} {
	result := map[string]struct{}{}
	for _, v := range strings.Split(os.Getenv(key), ",") {
		if v = strings.TrimSpace(v); v != "" {
			result[v] = struct{}{}
		}
	}
	return result
}
func boolean(key string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean", key)
	}
	return value, nil
}

// ReadSecret reads a secret from an environment variable or secret file.
// Consistently trims whitespace, newlines, and carriage returns.
func ReadSecret(envVar, fileEnvVar string) (string, error) {
	if v := strings.TrimSpace(os.Getenv(envVar)); v != "" {
		return v, nil
	}
	filePath := strings.TrimSpace(os.Getenv(fileEnvVar))
	if filePath == "" {
		return "", nil
	}
	data, err := os.ReadFile(filePath)
	if err != nil {
		return "", fmt.Errorf("read %s (%s): %w", fileEnvVar, filePath, err)
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return "", fmt.Errorf("%s (%s) is empty", fileEnvVar, filePath)
	}
	return trimmed, nil
}
