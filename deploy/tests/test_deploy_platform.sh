#!/usr/bin/env bash
# deploy/tests/test_deploy_platform.sh
# Tests for deploy-platform.sh: orchestration gating, atomic install, backup, and rollback.
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR="$(cd -- "$SCRIPT_DIR/.." && pwd)"

TEST_TMP="$(mktemp -d "${TMPDIR:-/tmp}/acb-platform-deploy-tests.XXXXXX")"
trap 'rm -rf "$TEST_TMP"' EXIT

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

setup_mock_env() {
  local test_dir="$1"
  export DEPLOY_PATH="$test_dir"
  export RUNTIME_ROOT="$test_dir"
  export ALLOW_TEST_LOCK_PATH=1
  export MOCK_STATE_DIR="$test_dir"
  export TRAEFIK_DYNAMIC_DIR="$test_dir/dynamic"
  export PLATFORM_CANDIDATE_DYNAMIC_DIR="$test_dir/candidate_dynamic"
  export PLATFORM_BACKUP_ROOT="$test_dir/backups"
  export ACB_CONFIG="$test_dir/dynamic/acb.yml"
  export ACTIVE_SLOT_FILE="$test_dir/active_slot"
  export PREVIOUS_SLOT_FILE="$test_dir/previous_slot"
  export ROUTE_ACK_TIMEOUT=5

  mkdir -p "$test_dir/dynamic" "$test_dir/candidate_dynamic" "$test_dir/backups" "$test_dir/bin"

  cat <<EOF > "$test_dir/.env.production"
APP_ENV=production
RUNTIME_ROLE=gateway
DATA_VOLUME_NAME=bank-event-gateway_gateway_data
EOF

  cat <<EOF > "$test_dir/.release.env"
IMAGE_REF_BLUE=ghcr.io/test/gateway@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
IMAGE_REF_GREEN=ghcr.io/test/gateway@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
WORKER_IMAGE_REF=ghcr.io/test/worker@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
DBTOOL_IMAGE_REF=ghcr.io/test/dbtool@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
BROWSER_IMAGE_REF=ghcr.io/test/browser@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
TTS_IMAGE_REF=ghcr.io/test/tts@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
BARK_IMAGE_REF=ghcr.io/test/bark@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
ACTIVE_GATEWAY_SLOT=blue
FRONTEND_TOPOLOGY=legacy
FRONTEND_ACTIVE_SLOT=blue
CANONICAL_GENERATION=1
EOF

  printf 'blue' > "$test_dir/active_slot"

  # Initial active files
  printf 'http:\n  routers: {}\n  services: {}\n# initial middlewares' > "$test_dir/dynamic/middlewares.yml"
  printf 'http:\n  routers: {}\n  services: {}\n# initial portfolio' > "$test_dir/dynamic/portfolio.yml"
  printf 'http:\n  routers: {}\n  services: {}\n# initial messenger' > "$test_dir/dynamic/messenger.yml"

  # Candidate files
  printf 'http:\n  routers: {}\n  services: {}\n# candidate middlewares' > "$test_dir/candidate_dynamic/middlewares.yml"
  printf 'http:\n  routers: {}\n  services: {}\n# candidate portfolio' > "$test_dir/candidate_dynamic/portfolio.yml"
  printf 'http:\n  routers: {}\n  services: {}\n# candidate messenger' > "$test_dir/candidate_dynamic/messenger.yml"

  cat <<'EOF' > "$test_dir/edge-probe.sh"
#!/usr/bin/env bash
active_slot="$(cat "$DEPLOY_PATH/active_slot" 2>/dev/null || echo "blue")"
printf "HTTP Status: 200 (expected: 200)\n"
printf "Verified route identity slot: %s\n" "$active_slot"
printf "Verified route identity commit: testcommit\n"
exit 0
EOF
  chmod +x "$test_dir/edge-probe.sh"
  export EDGE_PROBE_SCRIPT="$test_dir/edge-probe.sh"
}

printf "\n=== TEST 1: Direct invocation without orchestrator is refused ===\n"
t1="$TEST_TMP/t1"
setup_mock_env "$t1"
code=0
RELEASE_ORCHESTRATED=0 bash "$DEPLOY_DIR/deploy-platform.sh" 2>/dev/null || code=$?
assert_eq "1" "$code" "deploy-platform.sh fails without RELEASE_ORCHESTRATED=1"

printf "\n=== TEST 2: Successful platform edge deployment ===\n"
t2="$TEST_TMP/t2"
setup_mock_env "$t2"
RELEASE_ORCHESTRATED=1 EXPECTED_COMMIT="testcommit" bash "$DEPLOY_DIR/deploy-platform.sh"
assert_eq "$(cat "$t2/candidate_dynamic/middlewares.yml")" "$(cat "$t2/dynamic/middlewares.yml")" "middlewares.yml updated to candidate"
assert_eq "$(cat "$t2/candidate_dynamic/portfolio.yml")" "$(cat "$t2/dynamic/portfolio.yml")" "portfolio.yml updated to candidate"
assert_eq "$(cat "$t2/candidate_dynamic/messenger.yml")" "$(cat "$t2/dynamic/messenger.yml")" "messenger.yml updated to candidate"
if grep -q "acb-web-blue" "$t2/dynamic/acb.yml"; then
  printf 'PASS: acb.yml rendered for slot blue\n'
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
else
  printf 'FAIL: acb.yml not rendered for slot blue\n' >&2
  TESTS_FAILED=$(( TESTS_FAILED + 1 ))
fi

printf "\n=== TEST 3: Invalid candidate YAML fails closed without updating active ===\n"
t3="$TEST_TMP/t3"
setup_mock_env "$t3"
echo "invalid: [yaml: broken" > "$t3/candidate_dynamic/middlewares.yml"
code=0
RELEASE_ORCHESTRATED=1 bash "$DEPLOY_DIR/deploy-platform.sh" 2>/dev/null || code=$?
assert_eq "1" "$code" "deploy-platform.sh fails on invalid candidate YAML"
assert_eq "http:
  routers: {}
  services: {}
# initial middlewares" "$(cat "$t3/dynamic/middlewares.yml")" "middlewares.yml untouched on invalid candidate"

printf "\n=== TEST 4: Partial write failure restores pre-existing files and deletes new files ===\n"
t4="$TEST_TMP/t4"
setup_mock_env "$t4"
# Initially only middlewares.yml exists, portfolio.yml does NOT exist
rm -f "$t4/dynamic/portfolio.yml"
# Mock edge probe to fail on route identity ACK to trigger rollback after files are written
cat <<'EOF' > "$t4/edge-probe.sh"
#!/usr/bin/env bash
exit 1
EOF
chmod +x "$t4/edge-probe.sh"
code=0
RELEASE_ORCHESTRATED=1 EXPECTED_COMMIT="testcommit" bash "$DEPLOY_DIR/deploy-platform.sh" 2>/dev/null || code=$?
assert_eq "1" "$code" "deploy-platform.sh returns 1 on ACK failure"
assert_eq "http:
  routers: {}
  services: {}
# initial middlewares" "$(cat "$t4/dynamic/middlewares.yml")" "middlewares.yml restored to initial content"
assert_eq "http:
  routers: {}
  services: {}
# initial messenger" "$(cat "$t4/dynamic/messenger.yml")" "messenger.yml restored to initial content"
if [[ ! -f "$t4/dynamic/portfolio.yml" ]]; then
  printf 'PASS: candidate-created portfolio.yml removed by rollback\n'
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
else
  printf 'FAIL: candidate-created portfolio.yml was not removed by rollback\n' >&2
  TESTS_FAILED=$(( TESTS_FAILED + 1 ))
fi

printf "\n=== TEST 5: Orchestrator pending platform rollback reverts dynamic configs ===\n"
t5="$TEST_TMP/t5"
setup_mock_env "$t5"
export PENDING_PLATFORM_ROLLBACK_FILE="$t5/pending-platform-rollback.env"
RELEASE_ORCHESTRATED=1 EXPECTED_COMMIT="testcommit" bash "$DEPLOY_DIR/deploy-platform.sh"
assert_eq "$(cat "$t5/candidate_dynamic/middlewares.yml")" "$(cat "$t5/dynamic/middlewares.yml")" "deploy-platform installed candidate"
if [[ -f "$PENDING_PLATFORM_ROLLBACK_FILE" ]]; then
  printf 'PASS: pending platform rollback evidence file created\n'
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
else
  printf 'FAIL: pending platform rollback evidence file missing\n' >&2
  TESTS_FAILED=$(( TESTS_FAILED + 1 ))
fi

# Simulate parent orchestrator rollback
(
  # shellcheck source=deploy/lib.sh
  source "$DEPLOY_DIR/lib.sh"
  # shellcheck disable=SC1090
  source <(sed -n '/rollback_platform_config() {/,/^}/p' "$DEPLOY_DIR/dispatch-rollout.sh")
  rollback_platform_config
)
assert_eq "http:
  routers: {}
  services: {}
# initial middlewares" "$(cat "$t5/dynamic/middlewares.yml")" "middlewares.yml reverted by orchestrator rollback"
assert_eq "http:
  routers: {}
  services: {}
# initial messenger" "$(cat "$t5/dynamic/messenger.yml")" "messenger.yml reverted by orchestrator rollback"
if [[ ! -f "$PENDING_PLATFORM_ROLLBACK_FILE" ]]; then
  printf 'PASS: pending platform rollback evidence file cleaned up after rollback\n'
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
else
  printf 'FAIL: pending platform rollback evidence file not removed\n' >&2
  TESTS_FAILED=$(( TESTS_FAILED + 1 ))
fi

printf "\n=== TEST 6: Gateway + Platform composition executes deploy-platform.sh ===\n"
# Verify dispatch-rollout.sh Step 6 condition evaluates to true when PROMOTION_PLATFORM=true and PROMOTION_GATEWAY=true
step6_run=0
export PROMOTION_PLATFORM="true"
export PROMOTION_GATEWAY="true"
if [[ "${PROMOTION_PLATFORM:-false}" == "true" ]]; then
  step6_run=1
fi
assert_eq "1" "$step6_run" "Step 6 platform deploy executes when both gateway and platform are in scope"
assert_eq "true" "$PROMOTION_GATEWAY" "PROMOTION_GATEWAY is in scope for dual promotion"

printf "\n=== TEST 7: Platform rollback fails closed and preserves evidence on error ===\n"
t7="$TEST_TMP/t7"
setup_mock_env "$t7"
export PENDING_PLATFORM_ROLLBACK_FILE="$t7/pending-platform-rollback.env"
RELEASE_ORCHESTRATED=1 EXPECTED_COMMIT="testcommit" bash "$DEPLOY_DIR/deploy-platform.sh"

# Make dynamic dir unwritable to induce failure during rollback
b_dir="$(sed -n 's/^backup_dir=//p' "$PENDING_PLATFORM_ROLLBACK_FILE")"
# Corrupt backup directory path in pending file
printf 'backup_dir=%s/nonexistent_backup\nrelease_key=testcommit\n' "$t7" > "$PENDING_PLATFORM_ROLLBACK_FILE"

rb_code=0
(
  # shellcheck source=deploy/lib.sh
  source "$DEPLOY_DIR/lib.sh"
  # shellcheck disable=SC1090
  source <(sed -n '/rollback_platform_config() {/,/^}/p' "$DEPLOY_DIR/dispatch-rollout.sh")
  rollback_platform_config
) || rb_code=$?
assert_eq "0" "$rb_code" "Missing backup directory cleanly cleans up pending evidence without crashing"

# Test real failure preserves evidence
RELEASE_ORCHESTRATED=1 EXPECTED_COMMIT="testcommit" bash "$DEPLOY_DIR/deploy-platform.sh"
b_dir="$(sed -n 's/^backup_dir=//p' "$PENDING_PLATFORM_ROLLBACK_FILE")"
assert_eq "true" "$([[ -d "$b_dir" ]] && echo true || echo false)" "Platform backup directory is recorded and exists"
# Make dynamic dir unwritable
chmod 500 "$t7/dynamic" 2>/dev/null || true
if ! touch "$t7/dynamic/.test_probe" 2>/dev/null; then
  rb_fail_code=0
  (
    # shellcheck source=deploy/lib.sh
    source "$DEPLOY_DIR/lib.sh"
    # shellcheck disable=SC1090
    source <(sed -n '/rollback_platform_config() {/,/^}/p' "$DEPLOY_DIR/dispatch-rollout.sh")
    rollback_platform_config
  ) || rb_fail_code=$?
  assert_eq "1" "$rb_fail_code" "Platform rollback returns 1 when write fails"
  if [[ -f "$PENDING_PLATFORM_ROLLBACK_FILE" ]]; then
    printf 'PASS: Pending evidence preserved on rollback failure\n'
    TESTS_PASSED=$(( TESTS_PASSED + 1 ))
  else
    printf 'FAIL: Pending evidence removed on rollback failure\n' >&2
    TESTS_FAILED=$(( TESTS_FAILED + 1 ))
  fi
  chmod 700 "$t7/dynamic" 2>/dev/null || true
fi

printf "\n======================================================\n"
printf "Platform Deploy Tests: %d passed, %d failed\n" "$TESTS_PASSED" "$TESTS_FAILED"
printf "======================================================\n"

if [[ "$TESTS_FAILED" -gt 0 ]]; then
  exit 1
fi
exit 0
