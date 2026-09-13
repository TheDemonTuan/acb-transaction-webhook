package config

import (
	"fmt"
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
	RuntimeRole           RuntimeRole
	Address               string
	DatabasePath          string
	MasterKeyFile         string
	Timezone              *time.Location
	PollMinInterval       time.Duration
	PollMaxInterval       time.Duration
	CloudflareIssuer      string
	CloudflareAudience    string
	CloudflareJWKSURL     string
	Roles                 RoleSubjects
	DevelopmentSubject    string
	Production            bool
	PublicOrigin          string
	AuthBrowserURL        string
	AuthBrowserVNCURL     string
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
	WorkerInternalToken   string
	Slot                  string
	ReleaseCommit         string
}

func Load() (Config, error) {
	loc, err := time.LoadLocation(value("TZ", "Asia/Ho_Chi_Minh"))
	if err != nil {
		return Config{}, fmt.Errorf("load timezone: %w", err)
	}
	pollMin, err := seconds("POLL_MIN_INTERVAL_SEC", 3, 2, 300)
	if err != nil {
		return Config{}, err
	}
	pollMax, err := seconds("POLL_MAX_INTERVAL_SEC", 10, 2, 300)
	if err != nil {
		return Config{}, err
	}
	if pollMin > pollMax {
		return Config{}, fmt.Errorf("POLL_MIN_INTERVAL_SEC must be less than or equal to POLL_MAX_INTERVAL_SEC")
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
		PollMinInterval:    pollMin,
		PollMaxInterval:    pollMax,
		CloudflareIssuer:   cfIssuer,
		CloudflareAudience: cfAud,
		CloudflareJWKSURL:  cfJWKS,
		Roles: RoleSubjects{
			Owners:    owners,
			Operators: set("OPERATOR_SUBJECTS"),
			Viewers:   set("VIEWER_SUBJECTS"),
		},
		DevelopmentSubject:    value("DEVELOPMENT_SUBJECT", "local-owner"),
		Production:            production,
		PublicOrigin:          publicOrigin,
		AuthBrowserURL:        value("AUTH_BROWSER_URL", "http://auth-browser:8181"),
		AuthBrowserVNCURL:     value("AUTH_BROWSER_VNC_URL", "http://auth-browser:6080"),
		TTSGatewayURL:         ttsGatewayURL,
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
func seconds(key string, fallback, minimum, maximum int) (time.Duration, error) {
	v, err := strconv.Atoi(value(key, strconv.Itoa(fallback)))
	if err != nil || v < minimum || v > maximum {
		return 0, fmt.Errorf("%s must be an integer from %d to %d", key, minimum, maximum)
	}
	return time.Duration(v) * time.Second, nil
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

