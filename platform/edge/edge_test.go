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
	if !strings.Contains(cfg, "headers:\n      defaultMode: drop") && !strings.Contains(cfg, "headers:\r\n      defaultMode: drop") {
		t.Errorf("traefik.yml accessLog headers must have defaultMode: drop")
	}

	// AccessLog queryParameters defaultMode: drop
	if !strings.Contains(cfg, "queryParameters:\n      defaultMode: drop") && !strings.Contains(cfg, "queryParameters:\r\n      defaultMode: drop") {
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
	if !strings.Contains(mw, "Content-Security-Policy") {
		t.Errorf("middlewares.yml must apply CSP at the edge for the standalone frontend")
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
	if !strings.Contains(acb, "acb-api-router:") || !strings.Contains(acb, "PathPrefix(`/api`)") {
		t.Errorf("acb.yml must route only API and health paths to the blue/green gateway")
	}
	if !strings.Contains(acb, "acb-frontend-router:") || !strings.Contains(acb, "http://acb-frontend:8080") {
		t.Errorf("acb.yml must route public frontend paths to the isolated frontend service")
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

func getBashPath(t *testing.T) string {
	bashPath, err := exec.LookPath("bash")
	if err == nil {
		return bashPath
	}
	gitBash := `C:\Program Files\Git\bin\bash.exe`
	if _, errStat := os.Stat(gitBash); errStat == nil {
		return gitBash
	}
	t.Skip("bash executable not found; skipping bash-dependent tests")
	return ""
}

func createMockDocker(t *testing.T) (mockDir string, logFile string) {
	mockDir = t.TempDir()
	logFile = filepath.Join(mockDir, "docker.log")

	mockScript := filepath.Join(mockDir, "docker")
	content := `#!/usr/bin/env bash
set -e

if [[ -n "${MOCK_DOCKER_LOG:-}" ]]; then
  printf "%s\n" "$*" >> "${MOCK_DOCKER_LOG}"
fi

cmd="${1:-}"
shift || true

case "$cmd" in
  info)
    exit 0
    ;;
  inspect)
    if [[ "${MOCK_CONTAINER_RUNNING:-true}" == "true" ]]; then
      echo "true"
      exit 0
    else
      echo "false"
      exit 0
    fi
    ;;
  image)
    sub="${1:-}"
    shift || true
    if [[ "$sub" == "inspect" ]]; then
      if [[ "${MOCK_IMAGE_EXISTS:-true}" == "true" ]]; then
        echo '{"Id": "curlimages/curl:8.12.1"}'
        exit 0
      else
        echo "Error: No such image" >&2
        exit 1
      fi
    fi
    ;;
  run)
    if [[ "${MOCK_FAIL_DOCKER:-0}" == "1" ]]; then
      echo "Error from mock docker daemon" >&2
      exit 1
    fi
    status="${MOCK_HTTP_CODE:-200}"
    slot="${MOCK_SLOT_HEADER:-blue}"
    commit="${MOCK_COMMIT_HEADER:-sha256-test1234}"
    role="${MOCK_ROLE_HEADER:-gateway}"
    body="${MOCK_RESPONSE_BODY:-{\"status\":\"ok\"}}"

    cat <<RESP
HTTP/1.1 ${status} OK
Content-Type: application/json
X-Platform-Slot: ${slot}
X-Release-Commit: ${commit}
X-Runtime-Role: ${role}
Date: Mon, 14 Sep 2026 12:00:00 GMT

${body}
__STATUS_SENTINEL__:${status}
RESP
    exit 0
    ;;
  *)
    echo "Unknown mock docker command: $cmd" >&2
    exit 1
    ;;
esac
`
	if err := os.WriteFile(mockScript, []byte(content), 0755); err != nil {
		t.Fatalf("failed to write mock docker script: %v", err)
	}

	return mockDir, logFile
}

func TestProbeScriptOptionsAndDryRun(t *testing.T) {
	bashPath := getBashPath(t)
	edgeDir := getPlatformEdgeDir(t)
	probeScript := filepath.Join(edgeDir, "probe.sh")

	// Ensure probe.sh exists
	if _, err := os.Stat(probeScript); err != nil {
		t.Fatalf("probe.sh missing at %s", probeScript)
	}

	// 1. Slot-probe target dry-run
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

	// 2. Production target dry-run with --expected-slot and --expected-commit
	cmdProd := exec.Command(bashPath, probeScript, "--dry-run", "--target", "production", "--expected-slot", "blue", "--expected-commit", "sha256-prodcommit123")
	outProd, errProd := cmdProd.CombinedOutput()
	if errProd != nil {
		t.Fatalf("probe.sh production --dry-run failed: %v, output: %s", errProd, string(outProd))
	}
	outputProd := string(outProd)
	if !strings.Contains(outputProd, "target=production") || !strings.Contains(outputProd, "container_ns=edge-cloudflared") {
		t.Errorf("probe.sh production dry-run missing expected container_ns: %s", outputProd)
	}
	if !strings.Contains(outputProd, "Expecting slot: blue") || !strings.Contains(outputProd, "Expecting commit: sha256-prodcommit123") {
		t.Errorf("probe.sh production dry-run missing expected slot or commit: %s", outputProd)
	}

	// 3. Preflight check-runtime dry-run
	cmdCheck := exec.Command(bashPath, probeScript, "--check-runtime", "--dry-run", "--target", "production")
	outCheck, errCheck := cmdCheck.CombinedOutput()
	if errCheck != nil {
		t.Fatalf("probe.sh --check-runtime --dry-run failed: %v, output: %s", errCheck, string(outCheck))
	}
	if !strings.Contains(string(outCheck), "Preflight runtime check validated") {
		t.Errorf("probe.sh --check-runtime --dry-run output unexpected: %s", string(outCheck))
	}
}

func TestProbeScriptArgumentValidation(t *testing.T) {
	bashPath := getBashPath(t)
	edgeDir := getPlatformEdgeDir(t)
	probeScript := filepath.Join(edgeDir, "probe.sh")

	tests := []struct {
		name        string
		args        []string
		wantContain string
	}{
		{"empty expected-slot", []string{"--dry-run", "--expected-slot", ""}, "--expected-slot must not be empty or 'unknown'"},
		{"unknown expected-slot", []string{"--dry-run", "--expected-slot", "unknown"}, "--expected-slot must not be empty or 'unknown'"},
		{"empty expected-commit", []string{"--dry-run", "--expected-commit", ""}, "--expected-commit must not be empty or 'unknown'"},
		{"unknown expected-commit", []string{"--dry-run", "--expected-commit", "unknown"}, "--expected-commit must not be empty or 'unknown'"},
		{"invalid target", []string{"--dry-run", "--target", "staging"}, "Invalid target: staging"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmdArgs := append([]string{probeScript}, tc.args...)
			cmd := exec.Command(bashPath, cmdArgs...)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("expected error for %s, but command succeeded: %s", tc.name, string(out))
			}
			if !strings.Contains(string(out), tc.wantContain) {
				t.Errorf("%s output missing expected string %q, got: %s", tc.name, tc.wantContain, string(out))
			}
		})
	}
}

func TestProbeScriptPreflightCheckRuntime(t *testing.T) {
	bashPath := getBashPath(t)
	edgeDir := getPlatformEdgeDir(t)
	probeScript := filepath.Join(edgeDir, "probe.sh")
	mockDir, logFile := createMockDocker(t)

	// Subtest 1: Check runtime passes when container is running and image exists
	t.Run("runtime check pass", func(t *testing.T) {
		cmd := exec.Command(bashPath, probeScript, "--check-runtime", "--target", "production")
		cmd.Env = append(os.Environ(),
			"PATH="+mockDir+string(filepath.ListSeparator)+os.Getenv("PATH"),
			"MOCK_DOCKER_LOG="+logFile,
			"MOCK_CONTAINER_RUNNING=true",
			"MOCK_IMAGE_EXISTS=true",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("check-runtime failed: %v, output: %s", err, string(out))
		}
		if !strings.Contains(string(out), "Runtime check passed") || !strings.Contains(string(out), "edge-cloudflared") {
			t.Errorf("unexpected output from check-runtime: %s", string(out))
		}
	})

	// Subtest 2: Check runtime fails when container is not running
	t.Run("runtime check container not running", func(t *testing.T) {
		cmd := exec.Command(bashPath, probeScript, "--check-runtime", "--target", "production")
		cmd.Env = append(os.Environ(),
			"PATH="+mockDir+string(filepath.ListSeparator)+os.Getenv("PATH"),
			"MOCK_DOCKER_LOG="+logFile,
			"MOCK_CONTAINER_RUNNING=false",
			"MOCK_IMAGE_EXISTS=true",
		)
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("expected error when container not running, but command succeeded: %s", string(out))
		}
		if !strings.Contains(string(out), "Target namespace container 'edge-cloudflared' is not running") {
			t.Errorf("output missing expected container not running message: %s", string(out))
		}
	})

	// Subtest 3: Check runtime fails when image is missing
	t.Run("runtime check helper image missing", func(t *testing.T) {
		cmd := exec.Command(bashPath, probeScript, "--check-runtime", "--target", "production")
		cmd.Env = append(os.Environ(),
			"PATH="+mockDir+string(filepath.ListSeparator)+os.Getenv("PATH"),
			"MOCK_DOCKER_LOG="+logFile,
			"MOCK_CONTAINER_RUNNING=true",
			"MOCK_IMAGE_EXISTS=false",
		)
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("expected error when image missing, but command succeeded: %s", string(out))
		}
		if !strings.Contains(string(out), "Helper image 'curlimages/curl:8.12.1' not found locally") ||
			!strings.Contains(string(out), "docker pull curlimages/curl:8.12.1") {
			t.Errorf("output missing expected image missing instruction: %s", string(out))
		}
	})
}

func TestProbeScriptMockedDocker(t *testing.T) {
	bashPath := getBashPath(t)
	edgeDir := getPlatformEdgeDir(t)
	probeScript := filepath.Join(edgeDir, "probe.sh")
	mockDir, logFile := createMockDocker(t)

	// 1. Slot-probe target routing and security flags verification
	t.Run("slot-probe success and docker args", func(t *testing.T) {
		_ = os.Remove(logFile)
		cmd := exec.Command(bashPath, probeScript, "--target", "slot-probe", "--slot", "blue", "--expected-slot", "blue", "--expected-commit", "sha256-test1234")
		cmd.Env = append(os.Environ(),
			"PATH="+mockDir+string(filepath.ListSeparator)+os.Getenv("PATH"),
			"MOCK_DOCKER_LOG="+logFile,
			"MOCK_SLOT_HEADER=blue",
			"MOCK_COMMIT_HEADER=sha256-test1234",
			"MOCK_HTTP_CODE=200",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("probe.sh slot-probe failed: %v, output: %s", err, string(out))
		}
		output := string(out)
		if !strings.Contains(output, "Verified route identity slot: blue") {
			t.Errorf("output missing verified slot: %s", output)
		}
		if !strings.Contains(output, "Verified route identity commit: sha256-test1234") {
			t.Errorf("output missing verified commit: %s", output)
		}
		if !strings.Contains(output, "Probe SUCCESS") {
			t.Errorf("output missing success message: %s", output)
		}

		// Read mock docker log and verify docker run arguments
		logBytes, err := os.ReadFile(logFile)
		if err != nil {
			t.Fatalf("failed to read mock docker log: %v", err)
		}
		logContent := string(logBytes)

		// Verify security arguments
		for _, arg := range []string{"--read-only", "--cap-drop ALL", "--security-opt no-new-privileges"} {
			if !strings.Contains(logContent, arg) {
				t.Errorf("docker run missing required security arg %s; log: %s", arg, logContent)
			}
		}

		// Verify network namespace
		if !strings.Contains(logContent, "--network container:edge-traefik") {
			t.Errorf("docker run missing container:edge-traefik network; log: %s", logContent)
		}

		// Verify pinned image
		if !strings.Contains(logContent, "curlimages/curl:8.12.1") {
			t.Errorf("docker run missing pinned curl image; log: %s", logContent)
		}

		// Verify curl argument is NOT duplicated (curlimages/curl already has ENTRYPOINT ["curl"])
		if strings.Contains(logContent, "curlimages/curl:8.12.1 curl") {
			t.Errorf("docker run supplied duplicate 'curl' argument after image; log: %s", logContent)
		}
		if !strings.Contains(logContent, "curlimages/curl:8.12.1 -sS") {
			t.Errorf("docker run missing direct curl options after image; log: %s", logContent)
		}

		// Verify target URL and host header
		if !strings.Contains(logContent, "Host: acb-blue.internal.invalid") {
			t.Errorf("docker run missing expected slot Host header; log: %s", logContent)
		}
		if !strings.Contains(logContent, "http://127.0.0.1:18080/readyz") {
			t.Errorf("docker run missing expected slot destination URL; log: %s", logContent)
		}
	})

	// 2. Production target routing
	t.Run("production target acb routing", func(t *testing.T) {
		_ = os.Remove(logFile)
		cmd := exec.Command(bashPath, probeScript, "--target", "production", "--expected-slot", "green", "--expected-commit", "sha256-prod999")
		cmd.Env = append(os.Environ(),
			"PATH="+mockDir+string(filepath.ListSeparator)+os.Getenv("PATH"),
			"MOCK_DOCKER_LOG="+logFile,
			"MOCK_SLOT_HEADER=green",
			"MOCK_COMMIT_HEADER=sha256-prod999",
			"MOCK_HTTP_CODE=200",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("probe.sh production failed: %v, output: %s", err, string(out))
		}

		logBytes, err := os.ReadFile(logFile)
		if err != nil {
			t.Fatalf("failed to read mock docker log: %v", err)
		}
		logContent := string(logBytes)

		if !strings.Contains(logContent, "--network container:edge-cloudflared") {
			t.Errorf("production probe missing container:edge-cloudflared network; log: %s", logContent)
		}
		if !strings.Contains(logContent, "http://172.31.250.4:8080/readyz") {
			t.Errorf("production probe missing 172.31.250.4:8080/readyz URL; log: %s", logContent)
		}
		if !strings.Contains(logContent, "Host: bank.tuannguyenviet.site") {
			t.Errorf("production probe missing bank.tuannguyenviet.site Host; log: %s", logContent)
		}
	})

	// 3. Production bark routing
	t.Run("production target bark routing", func(t *testing.T) {
		_ = os.Remove(logFile)
		cmd := exec.Command(bashPath, probeScript, "--target", "production", "--service", "bark")
		cmd.Env = append(os.Environ(),
			"PATH="+mockDir+string(filepath.ListSeparator)+os.Getenv("PATH"),
			"MOCK_DOCKER_LOG="+logFile,
			"MOCK_HTTP_CODE=200",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("probe.sh bark failed: %v, output: %s", err, string(out))
		}

		logBytes, err := os.ReadFile(logFile)
		if err != nil {
			t.Fatalf("failed to read mock docker log: %v", err)
		}
		logContent := string(logBytes)

		if !strings.Contains(logContent, "Host: bark.tuannguyenviet.site") {
			t.Errorf("bark probe missing bark.tuannguyenviet.site Host; log: %s", logContent)
		}
		if !strings.Contains(logContent, "http://172.31.250.4:8080/ping") {
			t.Errorf("bark probe missing /ping URL; log: %s", logContent)
		}
	})

	// 4. Slot mismatch failure
	t.Run("slot mismatch rejected", func(t *testing.T) {
		cmd := exec.Command(bashPath, probeScript, "--target", "slot-probe", "--expected-slot", "blue", "--expected-commit", "sha256-test1234")
		cmd.Env = append(os.Environ(),
			"PATH="+mockDir+string(filepath.ListSeparator)+os.Getenv("PATH"),
			"MOCK_SLOT_HEADER=green", // mismatch
			"MOCK_COMMIT_HEADER=sha256-test1234",
			"MOCK_HTTP_CODE=200",
		)
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("expected error on slot mismatch, but probe succeeded: %s", string(out))
		}
		if !strings.Contains(string(out), "Route identity ACK slot mismatch: expected 'blue', got 'green'") {
			t.Errorf("output missing expected slot mismatch error: %s", string(out))
		}
	})

	// 5. Commit mismatch failure
	t.Run("commit mismatch rejected", func(t *testing.T) {
		cmd := exec.Command(bashPath, probeScript, "--target", "slot-probe", "--expected-slot", "blue", "--expected-commit", "sha256-expected123")
		cmd.Env = append(os.Environ(),
			"PATH="+mockDir+string(filepath.ListSeparator)+os.Getenv("PATH"),
			"MOCK_SLOT_HEADER=blue",
			"MOCK_COMMIT_HEADER=sha256-other456", // mismatch
			"MOCK_HTTP_CODE=200",
		)
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("expected error on commit mismatch, but probe succeeded: %s", string(out))
		}
		if !strings.Contains(string(out), "Route identity ACK commit mismatch: expected 'sha256-expected123', got 'sha256-other456'") {
			t.Errorf("output missing expected commit mismatch error: %s", string(out))
		}
	})

	// 6. Unknown or missing slot header rejected
	t.Run("unknown slot header rejected", func(t *testing.T) {
		cmd := exec.Command(bashPath, probeScript, "--target", "slot-probe", "--expected-slot", "blue")
		cmd.Env = append(os.Environ(),
			"PATH="+mockDir+string(filepath.ListSeparator)+os.Getenv("PATH"),
			"MOCK_SLOT_HEADER=unknown",
			"MOCK_HTTP_CODE=200",
		)
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("expected error on unknown slot header, but probe succeeded: %s", string(out))
		}
		if !strings.Contains(string(out), "Route identity ACK missing or unknown X-Platform-Slot header in response") {
			t.Errorf("output missing unknown slot header error: %s", string(out))
		}
	})

	// 7. Unknown or missing commit header rejected
	t.Run("unknown commit header rejected", func(t *testing.T) {
		cmd := exec.Command(bashPath, probeScript, "--target", "slot-probe", "--expected-commit", "sha256-test1234")
		cmd.Env = append(os.Environ(),
			"PATH="+mockDir+string(filepath.ListSeparator)+os.Getenv("PATH"),
			"MOCK_COMMIT_HEADER=unknown",
			"MOCK_HTTP_CODE=200",
		)
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("expected error on unknown commit header, but probe succeeded: %s", string(out))
		}
		if !strings.Contains(string(out), "Route identity ACK missing or unknown X-Release-Commit header in response") {
			t.Errorf("output missing unknown commit header error: %s", string(out))
		}
	})

	// 8. Non-200 HTTP code rejected
	t.Run("non-200 HTTP response rejected", func(t *testing.T) {
		cmd := exec.Command(bashPath, probeScript, "--target", "slot-probe", "--expected-slot", "blue", "--expected-commit", "sha256-test1234")
		cmd.Env = append(os.Environ(),
			"PATH="+mockDir+string(filepath.ListSeparator)+os.Getenv("PATH"),
			"MOCK_SLOT_HEADER=blue",
			"MOCK_COMMIT_HEADER=sha256-test1234",
			"MOCK_HTTP_CODE=502",
		)
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("expected error on HTTP 502, but probe succeeded: %s", string(out))
		}
		if !strings.Contains(string(out), "Status mismatch: got 502, expected 200") {
			t.Errorf("output missing status mismatch error: %s", string(out))
		}
	})
}

func TestProbeScriptDockerIntegration(t *testing.T) {
	bashPath := getBashPath(t)
	edgeDir := getPlatformEdgeDir(t)
	probeScript := filepath.Join(edgeDir, "probe.sh")

	dockerPath, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("docker binary not found; skipping live Docker integration test")
	}
	checkCmd := exec.Command(dockerPath, "info")
	if err := checkCmd.Run(); err != nil {
		t.Skip("docker daemon not running or not accessible; skipping live Docker integration test")
	}

	cmd := exec.Command(bashPath, probeScript, "--check-runtime", "--dry-run")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("probe.sh --check-runtime --dry-run failed: %v, output: %s", err, string(out))
	}
	if !strings.Contains(string(out), "Preflight runtime check validated") {
		t.Errorf("unexpected output from check-runtime dry-run: %s", string(out))
	}
}
