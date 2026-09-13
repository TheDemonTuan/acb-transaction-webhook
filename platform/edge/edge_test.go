package edge_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func getPlatformEdgeDir(t *testing.T) string {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get working dir: %v", err)
	}
	// If tests run in repo root or platform/edge
	if strings.HasSuffix(wd, filepath.Join("platform", "edge")) {
		return wd
	}
	return filepath.Join(wd, "platform", "edge")
}

// simulateHAProxyRule evaluates HAProxy ACL rules against an HTTP method and URI.
// Returns (allowed bool, matchedRule string).
func simulateHAProxyRule(method, uri string, envContainers, envEvents, envPing, envVersion bool) (bool, string) {
	// 1. Path normalization (replace older docker api versions to v1.44)
	reReplace := regexp.MustCompile(`^/v1\.[0-3][0-9]/(.*)`)
	normalizedURI := reReplace.ReplaceAllString(uri, "/v1.44/$1")

	// Strip query string to get path (HAProxy 'path' sample fetch ends before '?')
	path := normalizedURI
	if idx := strings.Index(path, "?"); idx != -1 {
		path = path[:idx]
	}

	// 2. Deny unless METH_GET
	if method != "GET" {
		return false, "deny-unless-get"
	}

	// 3. Explicit deny for container sensitive sub-resources (logs, archive, export, attach, exec)
	reSensitiveContainer := regexp.MustCompile(`(?i)/containers/[^/]+/(logs|archive|export|attach|exec)`)
	if reSensitiveContainer.MatchString(path) {
		return false, "deny-sensitive-container-subresource"
	}

	// 4. Explicit deny for sensitive APIs
	reSensitiveAPI := regexp.MustCompile(`(?i)^(/v[\d\.]+)?/(secrets|exec|volumes|networks|plugins|swarm|nodes|services|tasks|build|commit|configs|distribution)`)
	if reSensitiveAPI.MatchString(path) {
		return false, "deny-sensitive-api"
	}

	// 5. Strict allowlist for Traefik Docker provider
	rePing := regexp.MustCompile(`(?i)^(/v[\d\.]+)?/_?ping$`)
	if envPing && rePing.MatchString(path) {
		return true, "allow-ping"
	}

	reVersion := regexp.MustCompile(`(?i)^(/v[\d\.]+)?/version$`)
	if envVersion && reVersion.MatchString(path) {
		return true, "allow-version"
	}

	reEvents := regexp.MustCompile(`(?i)^(/v[\d\.]+)?/events$`)
	if envEvents && reEvents.MatchString(path) {
		return true, "allow-events"
	}

	reContainersList := regexp.MustCompile(`(?i)^(/v[\d\.]+)?/containers/json$`)
	if envContainers && reContainersList.MatchString(path) {
		return true, "allow-containers-list"
	}

	reContainersInspect := regexp.MustCompile(`(?i)^(/v[\d\.]+)?/containers/[a-zA-Z0-9_.-]+/json$`)
	if envContainers && reContainersInspect.MatchString(path) {
		return true, "allow-containers-inspect"
	}

	// 6. Default deny
	return false, "default-deny"
}

func TestHAProxySocketProxyACLs(t *testing.T) {
	edgeDir := getPlatformEdgeDir(t)
	haproxyPath := filepath.Join(edgeDir, "haproxy.cfg.template")
	content, err := os.ReadFile(haproxyPath)
	if err != nil {
		t.Fatalf("failed to read haproxy.cfg.template: %v", err)
	}
	cfg := string(content)

	// Verify required hardening directives exist in template
	expectedDirectives := []string{
		"http-request replace-path ^/v1\\.[0-3][0-9]/(.*) /v1.44/\\1",
		"http-request deny unless METH_GET",
		"http-request deny if { path,url_dec -m reg -i /containers/[^/]+/(logs|archive|export|attach|exec) }",
		"http-request deny if { path,url_dec -m reg -i ^(/v[\\d\\.]+)?/(secrets|exec|volumes|networks|plugins|swarm|nodes|services|tasks|build|commit|configs|distribution) }",
		"http-request allow if METH_GET { path,url_dec -m reg -i ^(/v[\\d\\.]+)?/_?ping$ } { env(PING) -m bool }",
		"http-request allow if METH_GET { path,url_dec -m reg -i ^(/v[\\d\\.]+)?/version$ } { env(VERSION) -m bool }",
		"http-request allow if METH_GET { path,url_dec -m reg -i ^(/v[\\d\\.]+)?/events$ } { env(EVENTS) -m bool }",
		"http-request allow if METH_GET { path,url_dec -m reg -i ^(/v[\\d\\.]+)?/containers/json$ } { env(CONTAINERS) -m bool }",
		"http-request allow if METH_GET { path,url_dec -m reg -i ^(/v[\\d\\.]+)?/containers/[a-zA-Z0-9_.-]+/json$ } { env(CONTAINERS) -m bool }",
		"http-request deny",
	}

	for _, d := range expectedDirectives {
		if !strings.Contains(cfg, d) {
			t.Errorf("haproxy.cfg.template missing required security directive: %s", d)
		}
	}

	// Verify broad container path is NOT present
	broadPattern := "http-request allow if { path,url_dec -m reg -i ^(/v[\\d\\.]+)?/containers } { env(CONTAINERS) -m bool }"
	if strings.Contains(cfg, broadPattern) {
		t.Errorf("haproxy.cfg.template contains vulnerable broad container path allow rule")
	}

	// Table-driven simulation of ACL behavior
	tests := []struct {
		name        string
		method      string
		uri         string
		wantAllowed bool
	}{
		// Allowed Traefik Docker Provider requests
		{"ping without version", "GET", "/_ping", true},
		{"ping with v1.44", "GET", "/v1.44/_ping", true},
		{"version without version prefix", "GET", "/version", true},
		{"version with v1.44", "GET", "/v1.44/version", true},
		{"events without query", "GET", "/events", true},
		{"events with query parameters", "GET", "/v1.44/events?filters=%7B%22type%22%3A%5B%22container%22%5D%7D", true},
		{"containers list without version", "GET", "/containers/json", true},
		{"containers list with query all=1", "GET", "/v1.44/containers/json?all=1", true},
		{"containers list with filters", "GET", "/v1.44/containers/json?all=1&filters=%7B%7D", true},
		{"container inspect by name", "GET", "/v1.44/containers/acb-web-blue/json", true},
		{"container inspect by id", "GET", "/v1.44/containers/a1b2c3d4e5f6/json", true},
		{"container inspect with query", "GET", "/v1.44/containers/acb-web-green/json?size=false", true},

		// Denied mutation operations
		{"post container create", "POST", "/v1.44/containers/create", false},
		{"post container restart", "POST", "/v1.44/containers/acb-web-blue/restart", false},
		{"post container stop", "POST", "/v1.44/containers/acb-web-blue/stop", false},
		{"post container start", "POST", "/v1.44/containers/acb-web-blue/start", false},
		{"post exec create", "POST", "/v1.44/containers/acb-web-blue/exec", false},
		{"delete container", "DELETE", "/v1.44/containers/acb-web-blue", false},

		// Denied sensitive read operations
		{"container logs without version", "GET", "/containers/acb-web-blue/logs", false},
		{"container logs with query", "GET", "/v1.44/containers/acb-web-blue/logs?stdout=1&stderr=1", false},
		{"container archive", "GET", "/v1.44/containers/acb-web-blue/archive?path=/etc", false},
		{"container export", "GET", "/v1.44/containers/acb-web-blue/export", false},
		{"container attach", "GET", "/v1.44/containers/acb-web-blue/attach", false},
		{"exec inspect", "GET", "/v1.44/exec/123/json", false},
		{"secrets list", "GET", "/v1.44/secrets", false},
		{"volumes list", "GET", "/v1.44/volumes", false},
		{"networks list", "GET", "/v1.44/networks", false},

		// Denied broad / unlisted endpoints
		{"broad container path", "GET", "/containers", false},
		{"broad container slash", "GET", "/containers/", false},
		{"broad container versioned", "GET", "/v1.44/containers", false},
		{"info endpoint", "GET", "/v1.44/info", false},
		{"system df", "GET", "/v1.44/system/df", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			allowed, rule := simulateHAProxyRule(tc.method, tc.uri, true, true, true, true)
			if allowed != tc.wantAllowed {
				t.Errorf("%s %s got allowed=%v (rule=%s), want %v", tc.method, tc.uri, allowed, rule, tc.wantAllowed)
			}
		})
	}
}

func TestTraefikStaticConfiguration(t *testing.T) {
	edgeDir := getPlatformEdgeDir(t)
	traefikCfgPath := filepath.Join(edgeDir, "traefik.yml")
	content, err := os.ReadFile(traefikCfgPath)
	if err != nil {
		t.Fatalf("failed to read traefik.yml: %v", err)
	}
	cfg := string(content)

	// AccessLog JSON format
	if !strings.Contains(cfg, "format: json") {
		t.Errorf("traefik.yml accessLog must use json format")
	}

	// AccessLog headers defaultMode: drop
	if !strings.Contains(cfg, "headers:\n      defaultMode: drop") {
		t.Errorf("traefik.yml accessLog headers must have defaultMode: drop")
	}

	// AccessLog queryParameters defaultMode: drop
	if !strings.Contains(cfg, "queryParameters:\n      defaultMode: drop") {
		t.Errorf("traefik.yml accessLog queryParameters must have defaultMode: drop")
	}

	// Sensitive fields dropped
	for _, field := range []string{"ClientUsername: drop", "RequestPath: drop", "RequestLine: drop"} {
		if !strings.Contains(cfg, field) {
			t.Errorf("traefik.yml accessLog must drop sensitive field %s", field)
		}
	}

	// Cloudflare ingress IP restriction under trustedIPs
	if !strings.Contains(cfg, "172.31.250.2/32") {
		t.Errorf("traefik.yml web entrypoint must trust only Cloudflare tunnel IP 172.31.250.2/32")
	}

	// Slot-probe internal entrypoint
	if !strings.Contains(cfg, "slot-probe:") || !strings.Contains(cfg, "127.0.0.1:18080") {
		t.Errorf("traefik.yml must define internal slot-probe on 127.0.0.1:18080")
	}
}

func TestTraefikDynamicMiddlewaresAndRouteProtection(t *testing.T) {
	edgeDir := getPlatformEdgeDir(t)

	// 1. Check middlewares.yml
	mwPath := filepath.Join(edgeDir, "dynamic", "middlewares.yml")
	mwContent, err := os.ReadFile(mwPath)
	if err != nil {
		t.Fatalf("failed to read middlewares.yml: %v", err)
	}
	mw := string(mwContent)

	if !strings.Contains(mw, "172.31.250.2/32") {
		t.Errorf("middlewares.yml tunnel-only must restrict sourceRange strictly to 172.31.250.2/32")
	}
	if strings.Contains(mw, "172.31.250.0/28") {
		t.Errorf("middlewares.yml tunnel-only must not allow broad subnet 172.31.250.0/28")
	}
	if !strings.Contains(mw, "deny-internal:") || !strings.Contains(mw, "127.0.0.1/32") {
		t.Errorf("middlewares.yml must contain deny-internal middleware with 127.0.0.1/32")
	}

	// 2. Check acb.yml
	acbPath := filepath.Join(edgeDir, "dynamic", "acb.yml")
	acbContent, err := os.ReadFile(acbPath)
	if err != nil {
		t.Fatalf("failed to read acb.yml: %v", err)
	}
	acb := string(acbContent)

	if !strings.Contains(acb, "acb-deny-internal:") || !strings.Contains(acb, "PathPrefix(`/internal`)") {
		t.Errorf("acb.yml must contain higher-priority deny router for /internal")
	}
	if !strings.Contains(acb, "!PathPrefix(`/internal`)") {
		t.Errorf("acb.yml production acb-router must explicitly exclude /internal via !PathPrefix")
	}
	if !strings.Contains(acb, "priority: 1000") {
		t.Errorf("acb.yml deny router must have priority 1000")
	}
	if !strings.Contains(acb, "healthCheck:") || !strings.Contains(acb, "/readyz") {
		t.Errorf("acb.yml must configure /readyz healthcheck for acb-service")
	}

	// 3. Check bark.yml
	barkPath := filepath.Join(edgeDir, "dynamic", "bark.yml")
	barkContent, err := os.ReadFile(barkPath)
	if err != nil {
		t.Fatalf("failed to read bark.yml: %v", err)
	}
	bark := string(barkContent)

	if !strings.Contains(bark, "bark-deny-internal:") || !strings.Contains(bark, "PathPrefix(`/internal`)") {
		t.Errorf("bark.yml must contain higher-priority deny router for /internal")
	}
	if !strings.Contains(bark, "!PathPrefix(`/internal`)") {
		t.Errorf("bark.yml production bark-router must explicitly exclude /internal via !PathPrefix")
	}
	if !strings.Contains(bark, "priority: 1000") {
		t.Errorf("bark.yml deny router must have priority 1000")
	}
	if !strings.Contains(bark, "healthCheck:") || !strings.Contains(bark, "/ping") {
		t.Errorf("bark.yml must configure /ping healthcheck for bark-service")
	}
}

func TestComposeSecurityHardeningAndProbeContainers(t *testing.T) {
	edgeDir := getPlatformEdgeDir(t)
	composePath := filepath.Join(edgeDir, "compose.yml")
	content, err := os.ReadFile(composePath)
	if err != nil {
		t.Fatalf("failed to read compose.yml: %v", err)
	}
	compose := string(content)

	// Resource bounds on socket-proxy
	if !strings.Contains(compose, "pids_limit: 100") || !strings.Contains(compose, "mem_limit: 128m") {
		t.Errorf("compose.yml socket-proxy must have pids_limit and mem_limit bounds")
	}

	// Resource bounds on traefik
	if !strings.Contains(compose, "pids_limit: 200") || !strings.Contains(compose, "mem_limit: 256m") {
		t.Errorf("compose.yml traefik must have pids_limit and mem_limit bounds")
	}

	// Log bounds on both
	if strings.Count(compose, "max-size: \"10m\"") < 3 {
		t.Errorf("compose.yml must have log max-size bounds on all edge services")
	}

	// Drop NET_BIND_SERVICE on traefik (ports 8080 and 18080 are > 1024)
	if strings.Contains(compose, "NET_BIND_SERVICE") {
		t.Errorf("compose.yml traefik must not include NET_BIND_SERVICE capability")
	}

	// Pinned helper probe containers
	if !strings.Contains(compose, "probe-traefik:") || !strings.Contains(compose, "network_mode: \"container:edge-traefik\"") {
		t.Errorf("compose.yml must define probe-traefik in edge-traefik namespace")
	}
	if !strings.Contains(compose, "probe-cloudflared:") || !strings.Contains(compose, "network_mode: \"container:edge-cloudflared\"") {
		t.Errorf("compose.yml must define probe-cloudflared in edge-cloudflared namespace")
	}
	if !strings.Contains(compose, "image: curlimages/curl:8.12.1") {
		t.Errorf("compose.yml helper probe containers must use pinned image curlimages/curl:8.12.1")
	}
}

func TestProbeScriptOptionsAndDryRun(t *testing.T) {
	edgeDir := getPlatformEdgeDir(t)
	probeScript := filepath.Join(edgeDir, "probe.sh")

	// Ensure probe.sh exists
	if _, err := os.Stat(probeScript); err != nil {
		t.Fatalf("probe.sh missing at %s", probeScript)
	}

	// Run dry-run verification
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		// Try Git bash on Windows if available
		gitBash := `C:\Program Files\Git\bin\bash.exe`
		if _, errStat := os.Stat(gitBash); errStat == nil {
			bashPath = gitBash
		} else {
			t.Skip("bash executable not found; skipping CLI execution test")
		}
	}

	cmd := exec.Command(bashPath, probeScript, "--dry-run", "--target", "slot-probe", "--slot", "green", "--expected-digest", "sha-abc1234")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("probe.sh --dry-run failed: %v, output: %s", err, string(out))
	}

	output := string(out)
	if !strings.Contains(output, "target=slot-probe") || !strings.Contains(output, "acb-green.internal.invalid") {
		t.Errorf("probe.sh output missing expected slot-probe target details: %s", output)
	}
	if !strings.Contains(output, "sha-abc1234") {
		t.Errorf("probe.sh output missing expected digest: %s", output)
	}
}
