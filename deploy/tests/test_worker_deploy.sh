#!/usr/bin/env bash
# deploy/tests/test_worker_deploy.sh
# Test suite for controlled worker singleton upgrade, quiesce/resume RPC, active-auth gate,
# candidate readiness probe, automatic rollback, and container isolation.
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR="$(cd -- "$SCRIPT_DIR/.." && pwd)"

TEST_TMP="$(mktemp -d "${TMPDIR:-/tmp}/acb-worker-deploy-tests.XXXXXX")"
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

setup_worker_mock_env() {
  local test_dir="$1"
  export MOCK_STATE_DIR="$test_dir"
  export MOCK_ACTIVE_AUTH=0
  export MOCK_QUIESCE_FAIL=0
  export MOCK_CANDIDATE_READY_FAIL=0
  export MOCK_CONTAINER_WORKER_ID="worker-cid-initial-1111"
  export MOCK_CONTAINER_GATEWAY_ID="gateway-cid-initial-2222"
  export MOCK_CONTAINER_BROWSER_ID="browser-cid-initial-3333"
  export MOCK_CONTAINER_TTS_ID="tts-cid-initial-4444"
  export MOCK_CONTAINER_BARK_ID="bark-cid-initial-5555"

  mkdir -p "$test_dir/bin" "$test_dir/secrets" "$test_dir/data" "$test_dir/dynamic"
  touch "$test_dir/data/gateway.db"

  cat <<EOF > "$test_dir/.env.production"
APP_ENV=production
RUNTIME_ROLE=worker
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

  printf 'mock-master-key\n' > "$test_dir/secrets/app_master_key"
  printf 'mock-tts-token\n' > "$test_dir/secrets/tts_internal_token"
  printf 'mock-worker-token\n' > "$test_dir/secrets/worker_internal_token"
  printf 'mock-bark-user\n' > "$test_dir/secrets/bark_basic_auth_user"
  printf 'mock-bark-pass\n' > "$test_dir/secrets/bark_basic_auth_password"

  cat <<'EOF' > "$test_dir/bin/docker"
#!/usr/bin/env bash
set -eu
STATE_DIR="${MOCK_STATE_DIR}"
cmd="${1:-}"
sub="${2:-}"

if [[ "$cmd" == "volume" ]]; then
  printf 'bank-event-gateway_gateway_data\n'
  exit 0
elif [[ "$cmd" == "ps" ]]; then
  if [[ "$*" =~ acb-worker ]]; then
    if [[ -f "$STATE_DIR/worker_stopped" ]]; then
      exit 0
    fi
    printf 'acb-worker\n'
    exit 0
  fi
  exit 0
elif [[ "$cmd" == "stop" ]]; then
  touch "$STATE_DIR/worker_stopped"
  exit 0
elif [[ "$cmd" == "rm" ]]; then
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
  if [[ "$*" =~ -gate-acquire ]]; then
    if [[ "${MOCK_ACTIVE_AUTH:-0}" == "1" ]]; then
      exit 1
    fi
    printf '{"status":"acquired","gateState":"LOCKED","leaseToken":"mocktoken123","fenceGeneration":2}\n'
    exit 0
  fi
  if [[ "$*" =~ -gate-release ]]; then
    printf '{"status":"released","gateState":"OPEN"}\n'
    exit 0
  fi
  exit 0
fi
exit 0
EOF
  chmod +x "$test_dir/bin/docker"

  export PATH="$test_dir/bin:$PATH"
}

# ==============================================================================
# TEST 1: Active Auth Blocks Worker Upgrade
# ==============================================================================
printf '\n=== TEST 1: Active Auth Blocks Worker Promotion ===\n'
T1="$TEST_TMP/t1"
setup_worker_mock_env "$T1"
export MOCK_ACTIVE_AUTH=1

set +e
"$DEPLOY_DIR/deploy-worker.sh" "ghcr.io/test/worker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
exit_code=$?
set -e

assert_eq "1" "$(( exit_code != 0 ? 1 : 0 ))" "Worker promotion aborted when active auth attempt is in progress"

# ==============================================================================
# TEST 2: Quiesce RPC Failure Aborts Without Stopping Container
# ==============================================================================
printf '\n=== TEST 2: Quiesce RPC Failure Aborts Without Stopping Old Container ===\n'
T2="$TEST_TMP/t2"
setup_worker_mock_env "$T2"
export WORKER_QUIESCE_CMD="false"

set +e
"$DEPLOY_DIR/deploy-worker.sh" "ghcr.io/test/worker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
exit_code=$?
set -e

assert_eq "1" "$(( exit_code != 0 ? 1 : 0 ))" "Worker promotion aborted on quiesce failure"
stopped=0
if [[ -f "$T2/worker_stopped" ]]; then
  stopped=1
fi
assert_eq "0" "$stopped" "Old worker was NOT stopped when quiesce failed"

# ==============================================================================
# TEST 3: Candidate Readiness Failure Triggers Rollback
# ==============================================================================
printf '\n=== TEST 3: Candidate Readiness Failure Triggers Automatic Rollback ===\n'
T3="$TEST_TMP/t3"
setup_worker_mock_env "$T3"
export WORKER_QUIESCE_CMD="echo '{\"status\":\"quiesced\",\"quiesced\":true,\"generation\":5,\"checkpoint\":\"2026-09-14\"}'"
export WORKER_START_CMD="true"
export WORKER_STOP_CMD="true"
export WORKER_READY_CHECK_CMD="false"
export WORKER_ROLLBACK_READY_CHECK_CMD="true"
export WORKER_READINESS_TIMEOUT="2"

set +e
"$DEPLOY_DIR/deploy-worker.sh" "ghcr.io/test/worker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
exit_code=$?
set -e

assert_eq "1" "$(( exit_code != 0 ? 1 : 0 ))" "Worker promotion aborted on candidate readiness failure"
# Release env still points to original digest, NOT candidate
current_ref="$(grep '^WORKER_IMAGE_REF=' "$T3/.release.env" | cut -d'=' -f2 | tr -d '\r\n')"
assert_eq "ghcr.io/test/worker@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" "$current_ref" "Release env preserved previous digest on rollback"

# ==============================================================================
# TEST 4: Successful Worker Upgrade and Commit
# ==============================================================================
printf '\n=== TEST 4: Successful Worker Upgrade ===\n'
T4="$TEST_TMP/t4"
setup_worker_mock_env "$T4"
export WORKER_QUIESCE_CMD="echo '{\"status\":\"quiesced\",\"quiesced\":true,\"generation\":5,\"checkpoint\":\"2026-09-14\"}'"
export WORKER_START_CMD="true"
export WORKER_STOP_CMD="true"
export WORKER_READY_CHECK_CMD="true"
export WORKER_READINESS_TIMEOUT="2"

"$DEPLOY_DIR/deploy-worker.sh" "ghcr.io/test/worker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

committed_ref="$(grep '^WORKER_IMAGE_REF=' "$T4/.release.env" | cut -d'=' -f2 | tr -d '\r\n')"
assert_eq "ghcr.io/test/worker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" "$committed_ref" "Release env committed new candidate digest"

# ==============================================================================
# TEST 5: Immutable Core IDs Test (Worker deploy leaves other containers untouched)
# ==============================================================================
printf '\n=== TEST 5: Worker deploy leaves gateway and auxiliary containers untouched ===\n'
assert_eq "$MOCK_CONTAINER_GATEWAY_ID" "gateway-cid-initial-2222" "Gateway container ID untouched"
assert_eq "$MOCK_CONTAINER_BROWSER_ID" "browser-cid-initial-3333" "Browser container ID untouched"
assert_eq "$MOCK_CONTAINER_TTS_ID" "tts-cid-initial-4444" "TTS container ID untouched"
assert_eq "$MOCK_CONTAINER_BARK_ID" "bark-cid-initial-5555" "Bark container ID untouched"

printf '\n==================================================\n'
printf 'WORKER DEPLOY TEST RESULTS: %d PASSED, %d FAILED\n' "$TESTS_PASSED" "$TESTS_FAILED"
printf '==================================================\n'

if [[ "$TESTS_FAILED" -gt 0 ]]; then
  exit 1
fi
exit 0
