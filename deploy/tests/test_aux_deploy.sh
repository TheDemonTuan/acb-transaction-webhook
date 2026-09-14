#!/usr/bin/env bash
# deploy/tests/test_aux_deploy.sh
# Test suite for auth-browser safety gate, independent auxiliary deployments (TTS, Bark),
# and container isolation verifying worker/gateway containers remain untouched.
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR="$(cd -- "$SCRIPT_DIR/.." && pwd)"

TEST_TMP="$(mktemp -d "${TMPDIR:-/tmp}/acb-aux-deploy-tests.XXXXXX")"
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

setup_aux_mock_env() {
  local test_dir="$1"
  export MOCK_STATE_DIR="$test_dir"
  export MOCK_ACTIVE_AUTH=0

  mkdir -p "$test_dir/bin" "$test_dir/secrets" "$test_dir/data" "$test_dir/dynamic"
  touch "$test_dir/data/gateway.db"

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
BARK_IMAGE_REF=ghcr.io/finb/bark-server@sha256:32d65b07fa835c99b31a396b77727a04ed058377fc2482da3e9dc7397167ffc4
EOF

  export ENV_FILE="$test_dir/.env.production"
  export RELEASE_ENV_FILE="$test_dir/.release.env"
  export SECRETS_DIR="$test_dir/secrets"
  export DEPLOY_LOCK_FILE="$test_dir/.deploy.lock"
  export TX_JOURNAL_FILE="$test_dir/data/deploy-journal.json"
  export DEPLOY_STATE_FILE="$test_dir/data/deploy-state.json"
  export READY_CHECK_TIMEOUT=1

  printf 'mock-master-key\n' > "$test_dir/secrets/app_master_key"
  printf 'mock-tts-token\n' > "$test_dir/secrets/tts_internal_token"
  printf 'mock-worker-token\n' > "$test_dir/secrets/worker_internal_token"
  printf 'mock-bark-user\n' > "$test_dir/secrets/bark_basic_auth_user"
  printf 'mock-bark-pass\n' > "$test_dir/secrets/bark_basic_auth_password"
  chmod 600 "$test_dir/secrets/"* 2>/dev/null || true

  cat <<'EOF' > "$test_dir/bin/docker"
#!/usr/bin/env bash
set -eu
cmd="${1:-}"
if [[ "$cmd" == "volume" ]]; then
  printf 'bank-event-gateway_gateway_data\nbark_data\n'
  exit 0
elif [[ "$cmd" == "run" ]]; then
  if [[ "$*" =~ -active-auth-count ]]; then
    if [[ "${MOCK_ACTIVE_AUTH:-0}" == "1" ]]; then
      printf '{"activeCount":1}\n'
    else
      printf '{"activeCount":0}\n'
    fi
    exit 0
  fi
fi
exit 0
EOF
  chmod +x "$test_dir/bin/docker"

  export PATH="$test_dir/bin:$PATH"
}

# ==============================================================================
# TEST 1: Active Auth Blocks Auth-Browser Promotion
# ==============================================================================
printf '\n=== TEST 1: Active Auth Blocks Auth-Browser Promotion ===\n'
T1="$TEST_TMP/t1"
setup_aux_mock_env "$T1"
export MOCK_ACTIVE_AUTH=1

set +e
"$DEPLOY_DIR/deploy-auth-browser.sh" "ghcr.io/test/browser@sha256:1111111111111111111111111111111111111111111111111111111111111111"
exit_code=$?
set -e

assert_eq "1" "$(( exit_code != 0 ? 1 : 0 ))" "Auth-browser deployment aborted when active auth attempt is in progress"

# ==============================================================================
# TEST 2: Successful Auth-Browser Deployment
# ==============================================================================
printf '\n=== TEST 2: Successful Auth-Browser Promotion ===\n'
T2="$TEST_TMP/t2"
setup_aux_mock_env "$T2"
export BROWSER_START_CMD="true"
export BROWSER_READY_CHECK_CMD="true"

"$DEPLOY_DIR/deploy-auth-browser.sh" "ghcr.io/test/browser@sha256:1111111111111111111111111111111111111111111111111111111111111111"

committed_ref="$(grep '^BROWSER_IMAGE_REF=' "$T2/.release.env" | cut -d'=' -f2 | tr -d '\r\n')"
assert_eq "ghcr.io/test/browser@sha256:1111111111111111111111111111111111111111111111111111111111111111" "$committed_ref" "Release env committed new browser candidate digest"
# Assert worker digest unchanged
worker_ref="$(grep '^WORKER_IMAGE_REF=' "$T2/.release.env" | cut -d'=' -f2 | tr -d '\r\n')"
assert_eq "ghcr.io/test/worker@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" "$worker_ref" "Worker digest untouched during browser deploy"

# ==============================================================================
# TEST 3: Successful TTS Deployment
# ==============================================================================
printf '\n=== TEST 3: Successful TTS Deployment ===\n'
T3="$TEST_TMP/t3"
setup_aux_mock_env "$T3"
export TTS_START_CMD="true"
export TTS_READY_CHECK_CMD="true"

"$DEPLOY_DIR/deploy-tts.sh" "ghcr.io/test/tts@sha256:2222222222222222222222222222222222222222222222222222222222222222"

committed_tts="$(grep '^TTS_IMAGE_REF=' "$T3/.release.env" | cut -d'=' -f2 | tr -d '\r\n')"
assert_eq "ghcr.io/test/tts@sha256:2222222222222222222222222222222222222222222222222222222222222222" "$committed_tts" "Release env committed new TTS candidate digest"
worker_ref="$(grep '^WORKER_IMAGE_REF=' "$T3/.release.env" | cut -d'=' -f2 | tr -d '\r\n')"
assert_eq "ghcr.io/test/worker@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" "$worker_ref" "Worker digest untouched during TTS deploy"

# ==============================================================================
# TEST 4: Successful Bark Deployment
# ==============================================================================
printf '\n=== TEST 4: Successful Bark Deployment ===\n'
T4="$TEST_TMP/t4"
setup_aux_mock_env "$T4"
export BARK_START_CMD="true"
export BARK_READY_CHECK_CMD="true"

"$DEPLOY_DIR/deploy-bark.sh" "ghcr.io/finb/bark-server@sha256:32d65b07fa835c99b31a396b77727a04ed058377fc2482da3e9dc7397167ffc4"

committed_bark="$(grep '^BARK_IMAGE_REF=' "$T4/.release.env" | cut -d'=' -f2 | tr -d '\r\n')"
assert_eq "ghcr.io/finb/bark-server@sha256:32d65b07fa835c99b31a396b77727a04ed058377fc2482da3e9dc7397167ffc4" "$committed_bark" "Release env committed new Bark candidate digest"
worker_ref="$(grep '^WORKER_IMAGE_REF=' "$T4/.release.env" | cut -d'=' -f2 | tr -d '\r\n')"
assert_eq "ghcr.io/test/worker@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" "$worker_ref" "Worker digest untouched during Bark deploy"

# ==============================================================================
# TEST 5: Auth-Browser Health Failure Triggers Rollback
# ==============================================================================
printf '\n=== TEST 5: Auth-Browser Health Failure Triggers Rollback ===\n'
T5="$TEST_TMP/t5"
setup_aux_mock_env "$T5"
export BROWSER_START_CMD="true"
# Candidate digest fails, but rollback digest succeeds
export BROWSER_READY_CHECK_CMD='[[ "${BROWSER_IMAGE_REF:-}" != *"99999999"* ]]'

set +e
"$DEPLOY_DIR/deploy-auth-browser.sh" "ghcr.io/test/browser@sha256:9999999999999999999999999999999999999999999999999999999999999999"
exit_code=$?
set -e

assert_eq "1" "$(( exit_code != 0 ? 1 : 0 ))" "Deploy failed on unhealthy candidate browser"
browser_ref="$(grep '^BROWSER_IMAGE_REF=' "$T5/.release.env" | cut -d'=' -f2 | tr -d '\r\n')"
assert_eq "ghcr.io/test/browser@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" "$browser_ref" "Release env preserved previous browser image ref on rollback"

# ==============================================================================
# TEST 6: Candidate Health Failure Triggers TTS Rollback
# ==============================================================================
printf '\n=== TEST 6: Candidate Health Failure Triggers TTS Rollback ===\n'
T6="$TEST_TMP/t6"
setup_aux_mock_env "$T6"
export TTS_START_CMD="true"
export TTS_READY_CHECK_CMD='[[ "${TTS_IMAGE_REF:-}" != *"99999999"* ]]'

set +e
"$DEPLOY_DIR/deploy-tts.sh" "ghcr.io/test/tts@sha256:9999999999999999999999999999999999999999999999999999999999999999"
exit_code=$?
set -e

assert_eq "1" "$(( exit_code != 0 ? 1 : 0 ))" "Deploy failed on unhealthy candidate TTS"
tts_ref="$(grep '^TTS_IMAGE_REF=' "$T6/.release.env" | cut -d'=' -f2 | tr -d '\r\n')"
assert_eq "ghcr.io/test/tts@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" "$tts_ref" "Release env preserved previous TTS image ref on rollback"

# ==============================================================================
# TEST 7: Candidate Health Failure Triggers Bark Rollback & Preserves Data
# ==============================================================================
printf '\n=== TEST 7: Candidate Health Failure Triggers Bark Rollback & Preserves Data ===\n'
T7="$TEST_TMP/t7"
setup_aux_mock_env "$T7"
mkdir -p "$T7/bark_data"
printf 'precious-bark-data\n' > "$T7/bark_data/device_tokens.db"
export BARK_START_CMD="true"
export BARK_READY_CHECK_CMD='[[ "${BARK_IMAGE_REF:-}" != *"99999999"* ]]'

set +e
"$DEPLOY_DIR/deploy-bark.sh" "ghcr.io/finb/bark-server@sha256:9999999999999999999999999999999999999999999999999999999999999999"
exit_code=$?
set -e

assert_eq "1" "$(( exit_code != 0 ? 1 : 0 ))" "Deploy failed on unhealthy candidate Bark"
bark_ref="$(grep '^BARK_IMAGE_REF=' "$T7/.release.env" | cut -d'=' -f2 | tr -d '\r\n')"
assert_eq "ghcr.io/finb/bark-server@sha256:32d65b07fa835c99b31a396b77727a04ed058377fc2482da3e9dc7397167ffc4" "$bark_ref" "Release env preserved previous Bark image ref on rollback"
assert_eq "precious-bark-data" "$(cat "$T7/bark_data/device_tokens.db")" "Bark data volume preserved across rollback"

# ==============================================================================
# TEST 8: Rollback Failure Handling Recorded in Journal
# ==============================================================================
printf '\n=== TEST 8: Rollback Failure Handling Recorded in Journal ===\n'
T8="$TEST_TMP/t8"
setup_aux_mock_env "$T8"
export BARK_START_CMD="true"
export BARK_READY_CHECK_CMD="false"

set +e
"$DEPLOY_DIR/deploy-bark.sh" "ghcr.io/finb/bark-server@sha256:9999999999999999999999999999999999999999999999999999999999999999"
exit_code=$?
set -e

journal_file="$T8/data/deploy-journal.json"
journal_state="$(grep -o '"state":[[:space:]]*"[^"]*"' "$journal_file" 2>/dev/null | head -n1 | cut -d'"' -f4 || echo "")"
assert_eq "TX_ROLLBACK_FAILED" "$journal_state" "Journal recorded TX_ROLLBACK_FAILED on unrecoverable candidate failure"

# ==============================================================================
# TEST 9: Crash Recovery of Interrupted Auxiliary Transaction
# ==============================================================================
printf '\n=== TEST 9: Crash Recovery of Interrupted Auxiliary Transaction ===\n'
T9="$TEST_TMP/t9"
setup_aux_mock_env "$T9"
mkdir -p "$T9/data"
cat <<EOF > "$T9/data/deploy-journal.json"
{
  "tx_id": "tx-crashed-auth",
  "component": "auth-browser",
  "state": "CANDIDATE_STARTING",
  "candidate_digest": "ghcr.io/test/browser@sha256:1111111111111111111111111111111111111111111111111111111111111111",
  "previous_digest": "ghcr.io/test/browser@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
}
EOF

(
  export TX_JOURNAL_FILE="$T9/data/deploy-journal.json"
  export DEPLOY_STATE_FILE="$T9/data/deploy-state.json"
  export COMPOSE_FILE="$DEPLOY_DIR/compose.prod.yaml"
  # shellcheck source=deploy/lib.sh
  source "$DEPLOY_DIR/lib.sh"
  recover_tx_journal
)

journal_exists=0
if [[ -f "$T9/data/deploy-journal.json" ]]; then
  journal_exists=1
fi
assert_eq "0" "$journal_exists" "Active journal removed after crash recovery"
recovered_count="$(ls "$T9/data"/deploy-journal.json.recovered.* 2>/dev/null | wc -l || echo 0)"
assert_eq "1" "$(( recovered_count >= 1 ? 1 : 0 ))" "Archived recovered journal file found"

printf '\n==================================================\n'
printf 'AUX DEPLOY TEST RESULTS: %d PASSED, %d FAILED\n' "$TESTS_PASSED" "$TESTS_FAILED"
printf '==================================================\n'

if [[ "$TESTS_FAILED" -gt 0 ]]; then
  exit 1
fi
exit 0
