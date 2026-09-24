#!/usr/bin/env bash
# deploy/tests/test_ci_workflow.sh
# Validates CI/CD workflow security invariants, bounded timeouts, SSH keepalives, and action pinning.
set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "$SCRIPT_DIR/../.." && pwd)"

TESTS_PASSED=0
TESTS_FAILED=0

assert_eq() {
  local expected="$1"
  local actual="$2"
  local msg="$3"
  if [[ "$expected" != "$actual" ]]; then
    printf 'FAIL: %s (expected: "%s", got: "%s")\n' "$msg" "$expected" "$actual" >&2
    TESTS_FAILED=$(( TESTS_FAILED + 1 ))
    return 1
  fi
  printf 'PASS: %s\n' "$msg"
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
  return 0
}

assert_contains() {
  local haystack="$1"
  local needle="$2"
  local msg="$3"
  if ! grep -F -q -- "$needle" <<< "$haystack"; then
    printf 'FAIL: %s (text did not contain "%s")\n' "$msg" "$needle" >&2
    TESTS_FAILED=$(( TESTS_FAILED + 1 ))
    return 1
  fi
  printf 'PASS: %s\n' "$msg"
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
  return 0
}
assert_not_contains() {
  local haystack="$1"
  local needle="$2"
  local msg="$3"
  if grep -F -q -- "$needle" <<< "$haystack"; then
    printf 'FAIL: %s (text contained forbidden "%s")\n' "$msg" "$needle" >&2
    TESTS_FAILED=$(( TESTS_FAILED + 1 ))
    return 1
  fi
  printf 'PASS: %s\n' "$msg"
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
  return 0
}

printf "========================================================\n"
printf "Running CI Workflow Security & Timeout Invariant Tests\n"
printf "========================================================\n\n"

deploy_yml="$REPO_ROOT/.github/workflows/deploy.yml"
ci_yml="$REPO_ROOT/.github/workflows/ci.yml"
codacy_yml="$REPO_ROOT/.github/workflows/codacy.yml"
reconcile_yml="$REPO_ROOT/.github/workflows/reconcile.yml"

# 1. Workflow YAML existence and syntax
printf "1. Testing Workflow File Existence...\n"
[[ -f "$deploy_yml" ]] && assert_eq "true" "true" ".github/workflows/deploy.yml exists"
[[ -f "$ci_yml" ]] && assert_eq "true" "true" ".github/workflows/ci.yml exists"
[[ -f "$codacy_yml" ]] && assert_eq "true" "true" ".github/workflows/codacy.yml exists"
[[ -f "$reconcile_yml" ]] && assert_eq "true" "true" ".github/workflows/reconcile.yml exists"

# 2. Concurrency Safety
printf "\n2. Testing Concurrency Protection & Non-Cancellation...\n"
deploy_content="$(cat "$deploy_yml")"
assert_contains "$deploy_content" "group: acb-transaction-webhook-production" "Deploy concurrency group is acb-transaction-webhook-production"
assert_contains "$deploy_content" "cancel-in-progress: false" "Deploy concurrency cancel-in-progress is false (never cancels in flight)"
reconcile_content="$(cat "$reconcile_yml")"
assert_contains "$reconcile_content" "group: acb-transaction-webhook-production" "Reconcile concurrency group is acb-transaction-webhook-production"
assert_contains "$reconcile_content" "cancel-in-progress: false" "Reconcile concurrency cancel-in-progress is false (never cancels in flight)"
assert_contains "$deploy_content" "bark_basic_auth_user|bark_basic_auth_password) ;;" "Workflow preserves runtime group access for Bark secrets"
if grep -q 'find .*secrets.*chmod 600' "$deploy_yml"; then
  printf 'FAIL: deploy workflow still forces every secret to mode 0600\n' >&2
  TESTS_FAILED=$(( TESTS_FAILED + 1 ))
else
  printf 'PASS: deploy workflow does not force Bark secrets back to mode 0600\n'
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
fi

# 3. Environment Protection
printf "\n2b. Testing authoritative production state inputs...\n"
assert_contains "$deploy_content" "  production-state:" "Workflow defines production-state job"
assert_contains "$deploy_content" "name: production-state" "Baseline is transferred through a production-state artifact"
assert_contains "$deploy_content" "PRODUCTION_BASE_SHA=\"\$(sed -n 's/^sha=//p' .production-state/production-state.env)\"" "Baseline comes from downloaded VPS production state"
if grep -q 'EVENT_BEFORE:' "$deploy_yml" || grep -q 'HEAD~1' "$deploy_yml"; then
  printf 'FAIL: deploy workflow retains an inferred baseline fallback\n' >&2
  TESTS_FAILED=$(( TESTS_FAILED + 1 ))
else
  printf 'PASS: deploy workflow has no event.before or HEAD~1 baseline fallback\n'
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
fi
if grep -Eq 'vars\.(FRONTEND|GATEWAY|WORKER|DBTOOL|BROWSER|TTS|BARK)_IMAGE_REF' "$deploy_yml"; then
  printf 'FAIL: GitHub Variables remain a production image source of truth\n' >&2
  TESTS_FAILED=$(( TESTS_FAILED + 1 ))
else
  printf 'PASS: production image refs never fall back to GitHub Variables\n'
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
fi
for key in FRONTEND GATEWAY WORKER DBTOOL AUTH_BROWSER TTS BARK; do
  assert_contains "$deploy_content" "PRODUCTION_${key}_IMAGE" "VPS state provides the ${key} image fallback"
done
assert_contains "$deploy_content" "stable-deployer.sh" "VPS rollout executes through the stable deployer"
assert_not_contains "$deploy_content" "ALLOW_LEGACY_WORKER_RESTART" "Production deploy cannot bypass worker protocol v2"
assert_contains "$deploy_content" 'releases/$release_id' "Candidate is staged in an immutable release directory"

printf "\n3. Testing Environment Protection...\n"
assert_contains "$deploy_content" "environment: production" "Deploy job enforces production environment protection"

# 4. Job-Level Bounded Timeouts
printf "\n4. Testing Job-Level Bounded Timeouts...\n"
check_job_timeout() {
  local file="$1"
  local job="$2"
  local has_timeout="false"
  if command -v node >/dev/null 2>&1; then
    has_timeout="$(node - "$file" "$job" <<'JSEOF'
const fs = require('fs');
const [,, fPath, jobName] = process.argv;
const lines = fs.readFileSync(fPath, 'utf8').split('\n');
let inJob = false;
let foundTimeout = false;
for (const line of lines) {
  if (line.startsWith(`  ${jobName}:`)) {
    inJob = true;
    continue;
  }
  if (inJob && /^  [a-zA-Z0-9_-]+:/.test(line)) {
    break;
  }
  if (inJob && /timeout-minutes:\s*[0-9]+/.test(line)) {
    foundTimeout = true;
    break;
  }
}
console.log(foundTimeout ? 'true' : 'false');
JSEOF
)"
  else
    # Fallback to grep check in job section
    if grep -A 10 "^  ${job}:" "$file" | grep -q "timeout-minutes:"; then
      has_timeout="true"
    fi
  fi
  assert_eq "true" "$has_timeout" "$file job [$job] has bounded timeout-minutes"
}

check_job_timeout "$deploy_yml" "verify"
check_job_timeout "$deploy_yml" "build-gateway-image"
check_job_timeout "$deploy_yml" "build-worker-image"
check_job_timeout "$deploy_yml" "build-dbtool-image"
check_job_timeout "$deploy_yml" "build-auth-browser-image"
check_job_timeout "$deploy_yml" "build-tts-gateway-image"
check_job_timeout "$deploy_yml" "scan-and-attest"
check_job_timeout "$deploy_yml" "deploy"

ci_content="$(cat "$ci_yml")"
check_job_timeout "$ci_yml" "verify"
check_job_timeout "$ci_yml" "docker-smoke-gateway"
check_job_timeout "$ci_yml" "docker-smoke-auth-browser"
check_job_timeout "$ci_yml" "docker-smoke-tts-gateway"
check_job_timeout "$reconcile_yml" "reconcile"

scan_checkout_context="$(grep -A 25 '^  scan-and-attest:' "$deploy_yml" || true)"
assert_contains "$scan_checkout_context" "fetch-depth: 0" "Scan and attest job fetches full history for promotion scope"

ci_checkout_context="$(grep -A 25 '^  verify:' "$ci_yml" || true)"
assert_contains "$ci_checkout_context" "fetch-depth: 0" "CI verify job fetches full history for PR promotion scope diff"
assert_contains "$ci_content" "Validate pull request promotion scope diff" "CI workflow includes PR promotion scope validation step"
assert_contains "$ci_content" "compute-promotion-scope.sh" "CI workflow executes promotion scope classifier on PR diff"
assert_contains "$ci_content" "github.event_name == 'pull_request'" "CI workflow runs promotion scope validation only on pull requests"
assert_contains "$ci_content" "pip install -r requirements.lock -r requirements-dev.lock" "TTS tests install committed lock files"

# 4b. ARM64 TTS Image Security Gate
printf "\n4b. Testing ARM64 TTS Image Security Gate...\n"
tts_dockerfile="$REPO_ROOT/tts-gateway/Dockerfile"
allowlist_content="$(cat "$REPO_ROOT/deploy/cve-allowlist.json")"
tts_dockerfile_content="$(cat "$tts_dockerfile")"
assert_contains "$tts_dockerfile_content" "FROM python:3.13.15-alpine3.24 AS builder" "TTS image installs locked dependencies in a builder stage"
assert_contains "$tts_dockerfile_content" "COPY --from=builder /opt/venv /opt/venv" "TTS runtime receives only the prepared virtual environment"
assert_contains "$tts_dockerfile_content" "rm -rf /usr/local/bin/pip*" "TTS runtime removes base pip executables"
assert_contains "$tts_dockerfile_content" "/usr/local/lib/python3.13/site-packages/setuptools*" "TTS runtime removes base setuptools payload"
assert_contains "$tts_dockerfile_content" "/opt/venv/lib/python3.13/site-packages/pkg_resources*" "TTS runtime removes virtual environment packaging payload"
assert_not_contains "$tts_dockerfile_content" "setuptools==" "TTS image does not pin setuptools into the runtime"
assert_not_contains "$allowlist_content" "CVE-2026-23949" "CVE allowlist does not mask vendored jaraco.context vulnerability"
assert_not_contains "$allowlist_content" "CVE-2026-24049" "CVE allowlist does not mask vendored wheel vulnerability"
tts_job_context="$(grep -A 65 '^  docker-smoke-tts-gateway:' "$ci_yml" || true)"
assert_contains "$tts_job_context" "runs-on: ubuntu-24.04-arm" "TTS image gate runs on native ARM64"
assert_contains "$tts_job_context" "context: ./tts-gateway" "TTS image gate builds the production Docker context"
assert_contains "$tts_job_context" "tags: acb-tts-gateway:smoke-test" "TTS image gate tags the loaded production-equivalent image"
assert_contains "$tts_job_context" "load: true" "TTS image gate loads the exact built image"
assert_contains "$tts_job_context" "Run tts-gateway image smoke test twice" "TTS image gate labels repeated smoke execution"
smoke_invocations="$(grep -F -c 'bash deploy/smoke-test-tts-gateway.sh acb-tts-gateway:smoke-test' <<< "$tts_job_context" || true)"
assert_eq "2" "$smoke_invocations" "TTS image gate runs two independent real health and auth smoke paths"
assert_contains "$tts_job_context" "TRIVY_VERSION: 0.74.0" "TTS image gate installs a pinned Trivy release"
assert_contains "$tts_job_context" "trivy_\${TRIVY_VERSION}_checksums.txt" "TTS image gate verifies the Trivy archive checksum"
assert_contains "$tts_job_context" "--platform linux/arm64" "TTS image gate scans ARM64 image metadata"
assert_contains "$tts_job_context" "--scanners vuln" "TTS image gate scans vulnerabilities"
assert_contains "$tts_job_context" "--exit-code 1" "TTS image gate fails on policy violations"
assert_contains "$tts_job_context" "--severity HIGH,CRITICAL" "TTS image gate blocks HIGH and CRITICAL findings"
assert_contains "$tts_job_context" "--ignorefile .trivyignore-tts-gateway" "TTS image gate uses the validated TTS exception file"
assert_contains "$tts_job_context" "acb-tts-gateway:smoke-test" "TTS scan targets the smoke-tested image"

# 5. Step-Level Timeouts on Deploy Job
printf "\n5. Testing Step-Level Timeouts on Critical Steps...\n"
assert_contains "$deploy_content" "name: Stage signed release without touching the trusted deployer" "Stage release step exists"
assert_contains "$deploy_content" "name: Deploy immutable image" "Deploy step exists"

has_sync_timeout="false"
has_deploy_step_timeout="false"
if command -v node >/dev/null 2>&1; then
  has_sync_timeout="$(node - "$deploy_yml" <<'JSEOF'
const fs = require('fs');
const lines = fs.readFileSync(process.argv[2], 'utf8').split('\n');
let inSync = false;
let timeout = false;
for (const l of lines) {
  if (l.includes('name: Stage signed release without touching the trusted deployer')) inSync = true;
  else if (inSync && l.includes('name: ')) break;
  else if (inSync && l.includes('timeout-minutes:')) { timeout = true; break; }
}
console.log(timeout ? 'true' : 'false');
JSEOF
)"
  has_deploy_step_timeout="$(node - "$deploy_yml" <<'JSEOF'
const fs = require('fs');
const lines = fs.readFileSync(process.argv[2], 'utf8').split('\n');
let inDep = false;
let timeout = false;
for (const l of lines) {
  if (l.includes('name: Deploy immutable image')) inDep = true;
  else if (inDep && l.includes('name: ')) break;
  else if (inDep && l.includes('timeout-minutes:')) { timeout = true; break; }
}
console.log(timeout ? 'true' : 'false');
JSEOF
)"
fi
assert_eq "true" "$has_sync_timeout" "Stage release step has timeout-minutes"
assert_eq "true" "$has_deploy_step_timeout" "Deploy immutable image step has timeout-minutes"

# 6. SSH Hardening & Keepalives
printf "\n6. Testing SSH Keepalive and Timeout Options...\n"
assert_contains "$deploy_content" "ServerAliveInterval 15" "SSH config includes ServerAliveInterval 15"
assert_contains "$deploy_content" "ServerAliveCountMax 10" "SSH config includes ServerAliveCountMax 10"
assert_contains "$deploy_content" "TCPKeepAlive yes" "SSH config includes TCPKeepAlive yes"
assert_contains "$deploy_content" "ConnectTimeout 15" "SSH config includes ConnectTimeout 15"
assert_contains "$deploy_content" "-o ConnectTimeout=15" "SSH execution passes explicit ConnectTimeout"

# 7. Promotion Dispatcher Invocation (No Compose Up/Down Shortcuts)
printf "\n7. Testing Promotion Dispatcher Contract...\n"
assert_contains "$deploy_content" "branches: [main]" "Deploy workflow triggers on main push"
assert_contains "$deploy_content" "github.event_name == 'push' && github.ref == 'refs/heads/main'" "Deploy job permits auto-deploy on main push"
assert_contains "$deploy_content" "github.event_name == 'workflow_dispatch' && inputs.deploy == true" "Deploy job permits manual dispatch deploy"
assert_contains "$deploy_content" "stable-deployer.sh" "Deploy step invokes stable-deployer.sh"
assert_not_contains "$deploy_content" 'bash "$trusted_preflight"' "Production-state is read-only and does not invoke preflight"
assert_contains "$deploy_content" 'bash "$DEPLOY_PATH/deploy/preflight-runtime.sh" --check-only' "Deploy-only reruns verify runtime with --check-only before staging"
assert_not_contains "$deploy_content" 'bash "$DEPLOY_PATH/deploy/preflight-runtime.sh" --reconcile' "Normal deploy does not auto-reconcile production runtime"
assert_contains "$deploy_content" '--base-generation' "Signed manifest includes canonical baseline generation"
assert_contains "$deploy_content" 'verify-release-baseline.sh' "Deploy checks signed baseline before mutation"
assert_contains "$deploy_content" 'Accept exact signed staged release before SSH' "Staged bundle acceptance precedes SSH mutation"
assert_not_contains "$deploy_content" "rm -rf '\$remote_release'" "Immutable release is never replaced on collision"
stable_deployer_content="$(cat "$REPO_ROOT/deploy/stable-deployer.sh")"
assert_contains "$stable_deployer_content" "dispatch-rollout.sh" "Stable deployer invokes dispatch-rollout.sh"
assert_contains "$stable_deployer_content" "--require-cosign" "Stable deployer enforces --require-cosign"
assert_contains "$stable_deployer_content" 'EDGE_PROBE_SCRIPT="$RELEASE_DIR/edge-probe.sh"' "Stable deployer uses the verified release edge probe"
traefik_lib_content="$(cat "$REPO_ROOT/deploy/lib/traefik.sh")"
assert_contains "$traefik_lib_content" 'bash "$probe_script"' "Frontend acknowledgement invokes the non-executable release probe through Bash"
assert_contains "$deploy_content" "--expected-identity" "Deploy step passes exact --expected-identity"
assert_contains "$deploy_content" "--expected-issuer" "Deploy step passes --expected-issuer"

# Ensure no bare docker compose up shortcuts in deploy script
remote_script="$(grep -A 35 'bash -s' "$deploy_yml" || true)"
assert_eq "false" "$(grep -q 'docker compose .* up' <<< "$remote_script" && echo "true" || echo "false")" "No whole-stack compose up shortcut in deploy step"

# 8. GitHub Actions Full SHA Pinning
printf "\n8. Testing All GitHub Actions Pinned to Full Commit SHAs...\n"
if bash "$REPO_ROOT/scripts/verify-actions-pinned.sh" >/dev/null 2>&1; then
  assert_eq "true" "true" "verify-actions-pinned.sh succeeds (all actions pinned to 40-char commit SHA)"
else
  assert_eq "true" "false" "verify-actions-pinned.sh failed unpinned action check"
fi

# 9. Static Validation and ShellCheck Fail-Fast Invariants
printf "\n9. Testing Static Validation and Fail-Fast Order...\n"
ci_content="$(cat "$ci_yml")"
assert_contains "$ci_content" "git ls-files '*.sh'" "CI ShellCheck discovers scripts from git ls-files to include deploy/tests and platform"
assert_contains "$ci_content" "shellcheck --severity=error" "CI enforces ShellCheck with error severity"

bun_pos="$(grep -n 'setup-bun' "$ci_yml" | head -n1 | cut -d: -f1)"
syntax_pos="$(grep -n 'Verify deployment and platform script syntax' "$ci_yml" | head -n1 | cut -d: -f1)"
shellcheck_pos="$(grep -n 'Lint deployment and platform scripts' "$ci_yml" | head -n1 | cut -d: -f1)"

if (( syntax_pos < bun_pos && shellcheck_pos < bun_pos )); then
  printf 'PASS: static syntax and shellcheck run before expensive build/test phases\n'
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
else
  printf 'FAIL: static checks (syntax:%d, shellcheck:%d) must run before setup-bun (%d)\n' "$syntax_pos" "$shellcheck_pos" "$bun_pos" >&2
  TESTS_FAILED=$(( TESTS_FAILED + 1 ))
fi

top_level_locals="$(python3 - <<'PY'
import os, re

top_locals = []
for root, dirs, files in os.walk("."):
    if ".git" in root or "node_modules" in root:
        continue
    for f in files:
        if f.endswith(".sh"):
            p = os.path.join(root, f)
            with open(p, errors="ignore") as fp:
                lines = fp.readlines()
            in_func = 0
            for i, line in enumerate(lines, 1):
                s = line.strip()
                if s.startswith("#"):
                    continue
                if re.match(r"^(function\s+[a-zA-Z0-9_-]+|[a-zA-Z0-9_-]+\s*\(\))\s*\{?", s):
                    in_func += 1
                elif "{" in s and in_func > 0:
                    in_func += s.count("{")
                if "}" in s and in_func > 0:
                    in_func -= s.count("}")
                    if in_func < 0: in_func = 0
                if in_func == 0 and re.match(r"^local\s+", s):
                    top_locals.append(f"{p}:{i}")
print(", ".join(top_locals))
PY
)"
assert_eq "" "$top_level_locals" "No top-level local declarations in shell scripts"

# 10. Production Contract Fail-Closed Gate Invariants
printf "\n10. Testing Production Contract Job Invariants...\n"
assert_contains "$ci_content" "production-contract:" "CI defines production-contract gate job"
assert_contains "$ci_content" "if: always()" "Production Contract uses if: always() to catch all outcomes"
assert_contains "$ci_content" "needs: [verify, docker-smoke-gateway, docker-smoke-auth-browser, docker-smoke-tts-gateway]" "Production Contract requires all mandatory upstream jobs"
assert_contains "$ci_content" '[[ "$result" == "success" ]]' "Production Contract fails closed on any non-success (rejects skipped/failed)"

# 11. Candidate Pre-Pull Promotion Contract Invariants
printf "\n11. Testing Candidate Pre-Pull Promotion Contract Invariants...\n"
prepull_test_output="$(python3 - "$deploy_yml" <<'PY'
import json, os, re, subprocess, sys, tempfile, textwrap

deploy_file = sys.argv[1]
with open(deploy_file, "r", encoding="utf-8") as f:
    deploy_content = f.read()

pattern = r"read -r candidate_images < <\(python3 - \"\$RELEASE_DIR/release-manifest\.json\" <<'PY'\n(.*?)\n\s*PY\n"
m = re.search(pattern, deploy_content, re.DOTALL)
if not m:
    print("FAILED_TO_EXTRACT")
    sys.exit(1)

code = textwrap.dedent(m.group(1))

def run_candidate(manifest_dict):
    with tempfile.NamedTemporaryFile(mode="w", suffix=".json", delete=False, encoding="utf-8") as tf:
        json.dump(manifest_dict, tf)
        tf_name = tf.name
    try:
        proc = subprocess.run(
            [sys.executable, "-c", code, tf_name],
            capture_output=True,
            text=True
        )
        return proc.returncode, proc.stdout.strip()
    finally:
        if os.path.exists(tf_name):
            os.remove(tf_name)

code_exit, stdout = run_candidate({
    "promotion": {"schema": True, "worker": True, "gateway": True, "frontend": True},
    "promotion_scope": ["frontend", "gateway", "worker", "schema"],
    "images": {
        "dbtool": "ghcr.io/repo/dbtool:canonical",
        "worker": "ghcr.io/repo/worker:canonical",
        "gateway": "ghcr.io/repo/gateway:canonical",
        "frontend": "ghcr.io/repo/frontend:canonical"
    }
})
expected_canonical = "ghcr.io/repo/dbtool:canonical ghcr.io/repo/worker:canonical ghcr.io/repo/gateway:canonical ghcr.io/repo/frontend:canonical"
if code_exit != 0 or stdout != expected_canonical:
    print(f"FAIL canonical: exit={code_exit} stdout={stdout}")
    sys.exit(1)

code_exit, stdout = run_candidate({
    "promotion": {"bark": True},
    "promotion_scope": ["bark"],
    "images": {"bark": "ghcr.io/repo/bark:canonical"}
})
if code_exit != 0 or stdout != "ghcr.io/repo/bark:canonical":
    print(f"FAIL bark-only: exit={code_exit} stdout={stdout}")
    sys.exit(1)

code_exit, stdout = run_candidate({
    "promotion": {"platform": True},
    "promotion_scope": ["platform"],
    "images": {}
})
if code_exit != 0 or stdout != "":
    print(f"FAIL platform-only: exit={code_exit} stdout={stdout}")
    sys.exit(1)

code_exit, _ = run_candidate({
    "promotion": {"schema": True},
    "promotion_scope": {"schema": True},
    "images": {"dbtool": "ghcr.io/repo/dbtool:canonical"}
})
if code_exit == 0:
    print("FAIL list-as-dict-regression")
    sys.exit(1)

code_exit, _ = run_candidate({
    "promotion": {"schema": True},
    "promotion_scope": ["worker"],
    "images": {"dbtool": "ghcr.io/repo/dbtool:canonical", "worker": "ghcr.io/repo/worker:canonical"}
})
if code_exit == 0:
    print("FAIL disagree")
    sys.exit(1)

code_exit, _ = run_candidate({
    "promotion": {"schema": True},
    "promotion_scope": ["schema", "schema"],
    "images": {"dbtool": "ghcr.io/repo/dbtool:canonical"}
})
if code_exit == 0:
    print("FAIL duplicate")
    sys.exit(1)

code_exit, _ = run_candidate({
    "promotion": {"unsupported_component": True},
    "promotion_scope": ["unsupported_component"],
    "images": {}
})
if code_exit == 0:
    print("FAIL unknown")
    sys.exit(1)

print("SUCCESS")
PY
)"
assert_eq "SUCCESS" "$prepull_test_output" "Candidate pre-pull follows signed manifest contract, accepts list promotion_scope, includes Bark, and fails closed on malformed scope"

# 12. Preflight Security Drift Scan Invariants
printf "\n12. Testing Preflight Security Drift Scan Invariants...\n"
assert_contains "$deploy_content" "  preflight-scan:" "Deploy workflow defines preflight-scan job"
check_job_timeout "$deploy_yml" "preflight-scan"
assert_contains "$deploy_content" "needs: production-state" "Preflight scan job depends on production-state"
assert_contains "$deploy_content" "preflight-scan-images.sh" "Preflight scan invokes preflight-scan-images.sh"

for comp in frontend gateway worker auth-browser; do
  assert_contains "$deploy_content" "needs.preflight-scan.outputs.security_rebuild_${comp//-/_}" "Build job build-${comp}-image checks preflight security rebuild output"
done
assert_contains "$deploy_content" "needs.preflight-scan.outputs.security_rebuild_schema" "Build job build-dbtool-image checks preflight security rebuild schema output"
assert_contains "$deploy_content" "needs.preflight-scan.outputs.security_rebuild_tts" "Build job build-tts-gateway-image checks preflight security rebuild tts output"

assert_contains "$deploy_content" "no_cache=true" "Security rebuild forces no_cache=true"
assert_contains "$deploy_content" "pull=true" "Security rebuild forces pull=true"
assert_contains "$deploy_content" "cache_from=" "Security rebuild clears cache-from"

scan_needs="$(grep -A 15 '^  scan-and-attest:' "$deploy_yml" || true)"
assert_contains "$scan_needs" "preflight-scan" "Scan-and-attest job requires preflight-scan"

component_map_content="$(cat "$REPO_ROOT/deploy/component-map.json")"
assert_contains "$component_map_content" "preflight-scan-images" "deploy/component-map.json maps preflight-scan-images to platform scope"

printf "\n========================================================\n"
printf "Results: %d passed, %d failed\n" "$TESTS_PASSED" "$TESTS_FAILED"
printf "========================================================\n"

if [[ $TESTS_FAILED -gt 0 ]]; then
  exit 1
fi
exit 0
