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

printf "========================================================\n"
printf "Running CI Workflow Security & Timeout Invariant Tests\n"
printf "========================================================\n\n"

deploy_yml="$REPO_ROOT/.github/workflows/deploy.yml"
ci_yml="$REPO_ROOT/.github/workflows/ci.yml"
codacy_yml="$REPO_ROOT/.github/workflows/codacy.yml"

# 1. Workflow YAML existence and syntax
printf "1. Testing Workflow File Existence...\n"
[[ -f "$deploy_yml" ]] && assert_eq "true" "true" ".github/workflows/deploy.yml exists"
[[ -f "$ci_yml" ]] && assert_eq "true" "true" ".github/workflows/ci.yml exists"
[[ -f "$codacy_yml" ]] && assert_eq "true" "true" ".github/workflows/codacy.yml exists"

# 2. Concurrency Safety
printf "\n2. Testing Concurrency Protection & Non-Cancellation...\n"
deploy_content="$(cat "$deploy_yml")"
assert_contains "$deploy_content" "group: acb-transaction-webhook-production" "Deploy concurrency group is acb-transaction-webhook-production"
assert_contains "$deploy_content" "cancel-in-progress: false" "Deploy concurrency cancel-in-progress is false (never cancels in flight)"
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

scan_checkout_context="$(grep -A 25 '^  scan-and-attest:' "$deploy_yml" || true)"
assert_contains "$scan_checkout_context" "fetch-depth: 0" "Scan and attest job fetches full history for promotion scope"

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
assert_contains "$deploy_content" "stable-deployer.sh" "Deploy step invokes stable-deployer.sh"
stable_deployer_content="$(cat "$REPO_ROOT/deploy/stable-deployer.sh")"
assert_contains "$stable_deployer_content" "dispatch-rollout.sh" "Stable deployer invokes dispatch-rollout.sh"
assert_contains "$stable_deployer_content" "--require-cosign" "Stable deployer enforces --require-cosign"
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

printf "\n========================================================\n"
printf "Results: %d passed, %d failed\n" "$TESTS_PASSED" "$TESTS_FAILED"
printf "========================================================\n"

if [[ $TESTS_FAILED -gt 0 ]]; then
  exit 1
fi
exit 0
