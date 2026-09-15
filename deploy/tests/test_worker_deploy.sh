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
  unset WORKER_STOP_CMD WORKER_START_CMD WORKER_READY_CHECK_CMD WORKER_QUIESCE_CMD WORKER_RESUME_CMD WORKER_ROLLBACK_READY_CHECK_CMD
  export MOCK_STATE_DIR="$test_dir"
  export MOCK_ACTIVE_AUTH=0
  export MOCK_QUIESCE_FAIL=0
  export MOCK_CANDIDATE_READY_FAIL=0
  export MOCK_EXEC_LEGACY_WORKER=0
  export MOCK_CANDIDATE_QUIESCE_BAD_RESPONSE=0
  export MOCK_CANDIDATE_QUIESCE_404=0
  export MOCK_CANDIDATE_QUIESCE_INVALID_JSON=0
  export MOCK_CANDIDATE_QUIESCE_FAIL=0
  export MOCK_INSPECT_EMPTY=0
  export MOCK_INSPECT_UNPINNED=0
  export MOCK_NO_RUNNING_WORKER=0
  export MOCK_INSPECT_IMAGE=""
  export MOCK_CONTAINER_WORKER_ID="worker-cid-initial-1111"
  export MOCK_CONTAINER_GATEWAY_ID="gateway-cid-initial-2222"
  export MOCK_CONTAINER_BROWSER_ID="browser-cid-initial-3333"
  export MOCK_CONTAINER_TTS_ID="tts-cid-initial-4444"
  export MOCK_CONTAINER_BARK_ID="bark-cid-initial-5555"

  mkdir -p "$test_dir/bin" "$test_dir/secrets" "$test_dir/data" "$test_dir/dynamic"
  touch "$test_dir/data/gateway.db"
  chmod 700 "$test_dir/secrets"

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
  chmod 600 "$test_dir/secrets/"*

  cat <<'EOF' > "$test_dir/bin/docker"
#!/usr/bin/env bash
set -eu
STATE_DIR="${MOCK_STATE_DIR}"
cmd="${1:-}"

if [[ "$cmd" == "volume" ]]; then
  printf 'bank-event-gateway_gateway_data\n'
  exit 0
elif [[ "$cmd" == "ps" ]]; then
  if [[ "${MOCK_NO_RUNNING_WORKER:-0}" == "1" ]]; then
    exit 0
  fi
  if [[ "$*" =~ -a ]]; then
    printf 'acb-worker\n'
    exit 0
  fi
  if [[ "$*" =~ acb-worker || "$*" =~ '{{.Names}}' ]]; then
    if [[ -f "$STATE_DIR/worker_stopped" ]]; then
      exit 0
    fi
    printf 'acb-worker\n'
    exit 0
  fi
  exit 0
elif [[ "$cmd" == "stop" ]]; then
  touch "$STATE_DIR/worker_stopped"
  touch "$STATE_DIR/stop_was_called"
  exit 0
elif [[ "$cmd" == "rm" ]]; then
  exit 0
elif [[ "$cmd" == "inspect" ]]; then
  if [[ "${MOCK_INSPECT_EMPTY:-0}" == "1" ]]; then
    exit 0
  fi
  if [[ "${MOCK_INSPECT_UNPINNED:-0}" == "1" ]]; then
    printf 'ghcr.io/test/worker:latest\n'
    exit 0
  fi
  if [[ -n "${MOCK_INSPECT_IMAGE:-}" ]]; then
    printf '%s\n' "$MOCK_INSPECT_IMAGE"
    exit 0
  fi
  printf 'ghcr.io/test/worker@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n'
  exit 0
elif [[ "$cmd" == "compose" ]]; then
  if [[ "$*" =~ up ]]; then
    rm -f "$STATE_DIR/worker_stopped"
  fi
  exit 0
elif [[ "$cmd" == "run" ]]; then
  if [[ "$*" =~ -deploy-capabilities ]]; then
    printf '{"protocol":2,"quiesce":true,"drain":true,"resume":true,"notificationDrain":true,"sessionCheckpoint":true,"journalCheckpoint":true}\n'
    exit 0
  fi
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
  if [[ "$*" =~ container:acb-worker && "$*" =~ -quiesce ]]; then
    if [[ ! "$*" =~ --entrypoint[[:space:]]+/worker ]] || [[ ! "$*" =~ worker_internal_token:/run/secrets/worker_internal_token:ro ]]; then
      printf 'ERROR: candidate rpc client missing explicit entrypoint or token mount: %s\n' "$*" >&2
      exit 1
    fi
    touch "$STATE_DIR/candidate_mount_checked"
    touch "$STATE_DIR/candidate_client_called"
    if [[ "${MOCK_CANDIDATE_QUIESCE_BAD_RESPONSE:-0}" == "1" ]]; then
      printf '{"status":"quiesced","quiesced":false}\n'
      exit 0
    fi
    if [[ "${MOCK_CANDIDATE_QUIESCE_404:-0}" == "1" ]]; then
      printf 'worker rpc returned status 404: 404 page not found\n' >&2
      exit 1
    fi
    if [[ "${MOCK_CANDIDATE_QUIESCE_INVALID_JSON:-0}" == "1" ]]; then
      printf 'Internal server error\n' >&2
      exit 1
    fi
    if [[ "${MOCK_CANDIDATE_QUIESCE_FAIL:-0}" == "1" ]]; then
      exit 1
    fi
    printf '{"status":"quiesced","quiesced":true,"generation":5,"dispatcher":"IDLE","activeDeliveries":0,"activePoll":false,"journalSeq":12,"sessionCheckpointed":true}\n'
    exit 0
  fi
  if [[ "$*" =~ container:acb-worker && "$*" =~ -resume ]]; then
    if [[ ! "$*" =~ --entrypoint[[:space:]]+/worker ]] || [[ ! "$*" =~ worker_internal_token:/run/secrets/worker_internal_token:ro ]]; then
      printf 'ERROR: candidate rpc client missing explicit entrypoint or token mount: %s\n' "$*" >&2
      exit 1
    fi
    touch "$STATE_DIR/candidate_mount_checked"
    touch "$STATE_DIR/worker_resumed"
    printf '{"status":"ok","resumed":true}\n'
    exit 0
  fi
  exit 0
elif [[ "$cmd" == "exec" ]]; then
  if [[ "$*" =~ -deploy-capabilities ]]; then
    printf '{"protocol":2,"quiesce":true,"drain":true,"resume":true,"notificationDrain":true,"sessionCheckpoint":true,"journalCheckpoint":true}\n'
    exit 0
  fi
  if [[ "$*" =~ -quiesce ]]; then
    if [[ "${MOCK_EXEC_LEGACY_WORKER:-0}" == "1" ]]; then
      printf 'flag provided but not defined: -quiesce\n' >&2
      exit 2
    fi
    if [[ "${MOCK_QUIESCE_FAIL:-0}" == "1" ]]; then
      exit 1
    fi
    printf '{"status":"quiesced","quiesced":true,"generation":5,"dispatcher":"IDLE","activeDeliveries":0,"activePoll":false,"journalSeq":12,"sessionCheckpointed":true}\n'
    exit 0
  fi
  if [[ "$*" =~ -resume ]]; then
    touch "$STATE_DIR/worker_resumed"
    printf '{"status":"ok","resumed":true}\n'
    exit 0
  fi
  if [[ "$*" =~ --readiness-check ]]; then
    if [[ "${MOCK_CANDIDATE_READY_FAIL:-0}" == "1" ]]; then
      exit 1
    fi
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
export WORKER_QUIESCE_CMD="echo '{\"status\":\"quiesced\",\"quiesced\":true,\"generation\":5,\"checkpoint\":\"2026-09-14\",\"dispatcher\":\"IDLE\",\"activeDeliveries\":0,\"activePoll\":false,\"journalSeq\":12,\"sessionCheckpointed\":true}'"
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
current_ref="$(grep '^WORKER_IMAGE_REF=' "$T3/.release.env" | cut -d'=' -f2 | tr -d '\r\n')"
assert_eq "ghcr.io/test/worker@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" "$current_ref" "Release env preserved previous digest on rollback"

# ==============================================================================
# TEST 4: Successful Worker Upgrade and Commit
# ==============================================================================
printf '\n=== TEST 4: Successful Worker Upgrade ===\n'
T4="$TEST_TMP/t4"
setup_worker_mock_env "$T4"
export WORKER_QUIESCE_CMD="echo '{\"status\":\"quiesced\",\"quiesced\":true,\"generation\":5,\"checkpoint\":\"2026-09-14\",\"dispatcher\":\"IDLE\",\"activeDeliveries\":0,\"activePoll\":false,\"journalSeq\":12,\"sessionCheckpointed\":true}'"
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

# ==============================================================================
# TEST 6: Container Exec Loopback Quiesce (Modern worker with -quiesce)
# ==============================================================================
printf '\n=== TEST 6: Container Exec Quiesce With Modern Worker ===\n'
T6="$TEST_TMP/t6"
setup_worker_mock_env "$T6"
unset WORKER_QUIESCE_CMD
export WORKER_START_CMD="true"
export WORKER_STOP_CMD="true"
export WORKER_READY_CHECK_CMD="true"
export WORKER_READINESS_TIMEOUT="2"

"$DEPLOY_DIR/deploy-worker.sh" "ghcr.io/test/worker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
committed_ref="$(grep '^WORKER_IMAGE_REF=' "$T6/.release.env" | cut -d'=' -f2 | tr -d '\r\n')"
assert_eq "ghcr.io/test/worker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" "$committed_ref" "Container exec loopback quiesced successfully and committed"

# ==============================================================================
# TEST 7: Legacy Worker Lacks -quiesce Flag -> Fallback to Candidate Container RPC
# ==============================================================================
printf '\n=== TEST 7: Legacy Worker Lacks -quiesce Flag -> Candidate Container RPC ===\n'
T7="$TEST_TMP/t7"
setup_worker_mock_env "$T7"
unset WORKER_QUIESCE_CMD
export MOCK_EXEC_LEGACY_WORKER=1
export WORKER_START_CMD="true"
export WORKER_STOP_CMD="true"
export WORKER_READY_CHECK_CMD="true"
export WORKER_READINESS_TIMEOUT="2"

"$DEPLOY_DIR/deploy-worker.sh" "ghcr.io/test/worker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
committed_ref="$(grep '^WORKER_IMAGE_REF=' "$T7/.release.env" | cut -d'=' -f2 | tr -d '\r\n')"
assert_eq "ghcr.io/test/worker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" "$committed_ref" "Upgraded successfully via candidate container RPC fallback"

candidate_called=0
if [[ -f "$T7/candidate_client_called" ]]; then
  candidate_called=1
fi
assert_eq "1" "$candidate_called" "Candidate container RPC client (--network container:acb-worker) was invoked"

mount_checked=0
if [[ -f "$T7/candidate_mount_checked" ]]; then
  mount_checked=1
fi
assert_eq "1" "$mount_checked" "Candidate container uses explicit /worker entrypoint and read-only token mount"

# ==============================================================================
# TEST 8: Non-Published Host Port -> Does Not Depend On Host Curl
# ==============================================================================
printf '\n=== TEST 8: Non-Published Host Port -> Candidate Container RPC Operates Without Host Port ===\n'
T8="$TEST_TMP/t8"
setup_worker_mock_env "$T8"
unset WORKER_QUIESCE_CMD
export MOCK_EXEC_LEGACY_WORKER=1
export WORKER_RPC_URL="http://127.0.0.1:19999" # Port not listening on host
export WORKER_START_CMD="true"
export WORKER_STOP_CMD="true"
export WORKER_READY_CHECK_CMD="true"
export WORKER_READINESS_TIMEOUT="2"

"$DEPLOY_DIR/deploy-worker.sh" "ghcr.io/test/worker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
committed_ref="$(grep '^WORKER_IMAGE_REF=' "$T8/.release.env" | cut -d'=' -f2 | tr -d '\r\n')"
assert_eq "ghcr.io/test/worker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" "$committed_ref" "Upgraded successfully even when host RPC port is unreachable"

# ==============================================================================
# TEST 9: Bad Quiesce Responses Fail Closed Without Stopping Old Container
# ==============================================================================
printf '\n=== TEST 9: Bad Quiesce Responses Fail Closed Without Stopping Old Container ===\n'

# 9a: Candidate RPC client observes a legacy worker without the quiesce endpoint.
T9A="$TEST_TMP/t9a"
setup_worker_mock_env "$T9A"
unset WORKER_QUIESCE_CMD
export MOCK_EXEC_LEGACY_WORKER=1
export MOCK_CANDIDATE_QUIESCE_404=1

set +e
"$DEPLOY_DIR/deploy-worker.sh" "ghcr.io/test/worker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
exit_code=$?
set -e

assert_eq "1" "$(( exit_code != 0 ? 1 : 0 ))" "Deploy fails closed when the running worker lacks safe quiesce"
stopped=0
if [[ -f "$T9A/stop_was_called" ]]; then
  stopped=1
fi
assert_eq "0" "$stopped" "Old worker container was not stopped after a legacy 404 response"

# 9b: Candidate container returns quiesced: false
T9B="$TEST_TMP/t9b"
setup_worker_mock_env "$T9B"
unset WORKER_QUIESCE_CMD
export MOCK_EXEC_LEGACY_WORKER=1
export MOCK_CANDIDATE_QUIESCE_BAD_RESPONSE=1

set +e
"$DEPLOY_DIR/deploy-worker.sh" "ghcr.io/test/worker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
exit_code=$?
set -e

assert_eq "1" "$(( exit_code != 0 ? 1 : 0 ))" "Deploy aborted on quiesced: false response"
stopped=0
if [[ -f "$T9B/worker_stopped" ]]; then
  stopped=1
fi
assert_eq "0" "$stopped" "Old worker container was NOT stopped on quiesced: false response"

# 9c: Candidate container returns invalid/error output
T9C="$TEST_TMP/t9c"
setup_worker_mock_env "$T9C"
unset WORKER_QUIESCE_CMD
export MOCK_EXEC_LEGACY_WORKER=1
export MOCK_CANDIDATE_QUIESCE_INVALID_JSON=1

set +e
"$DEPLOY_DIR/deploy-worker.sh" "ghcr.io/test/worker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
exit_code=$?
set -e

assert_eq "1" "$(( exit_code != 0 ? 1 : 0 ))" "Deploy aborted on invalid JSON quiesce response"
stopped=0
if [[ -f "$T9C/worker_stopped" ]]; then
  stopped=1
fi
assert_eq "0" "$stopped" "Old worker container was NOT stopped on invalid JSON quiesce response"

# ==============================================================================
# TEST 10: Missing Previous Worker Digest Aborts Before Quiesce and Stop
# ==============================================================================
printf '\n=== TEST 10: Missing Previous Worker Digest Aborts Before Quiesce and Stop ===\n'
T10="$TEST_TMP/t10"
setup_worker_mock_env "$T10"
# Remove WORKER_IMAGE_REF from release.env
sed -i '/WORKER_IMAGE_REF/d' "$T10/.release.env"
# Simulate docker inspect returning unpinned tag instead of immutable digest
export MOCK_INSPECT_UNPINNED=1

set +e
"$DEPLOY_DIR/deploy-worker.sh" "ghcr.io/test/worker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
exit_code=$?
set -e

assert_eq "1" "$(( exit_code != 0 ? 1 : 0 ))" "Deploy aborted before quiesce/stop when running worker has no rollback digest"
stopped=0
if [[ -f "$T10/worker_stopped" ]]; then
  stopped=1
fi
assert_eq "0" "$stopped" "Old worker container was NOT stopped when previous digest missing"

# ==============================================================================
# TEST 11: Resolves Previous Digest from Running Container When release.env Missing
# ==============================================================================
printf '\n=== TEST 11: Resolves Previous Digest from Running Container When release.env Missing ===\n'
T11="$TEST_TMP/t11"
setup_worker_mock_env "$T11"
# Remove WORKER_IMAGE_REF from release.env
sed -i '/WORKER_IMAGE_REF/d' "$T11/.release.env"
# Docker inspect will return valid running digest: ghcr.io/test/worker@sha256:aaaaaaaa...
export WORKER_QUIESCE_CMD="echo '{\"status\":\"quiesced\",\"quiesced\":true,\"generation\":5,\"dispatcher\":\"IDLE\",\"activeDeliveries\":0,\"activePoll\":false,\"journalSeq\":12,\"sessionCheckpointed\":true}'"
export WORKER_START_CMD="true"
export WORKER_STOP_CMD="true"
export WORKER_READY_CHECK_CMD="false" # Candidate fails readiness to trigger rollback
export WORKER_ROLLBACK_READY_CHECK_CMD="true"
export WORKER_READINESS_TIMEOUT="2"

set +e
"$DEPLOY_DIR/deploy-worker.sh" "ghcr.io/test/worker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
exit_code=$?
set -e

assert_eq "1" "$(( exit_code != 0 ? 1 : 0 ))" "Candidate failure triggers rollback"
# Verify rollback succeeded and release.env still does not have the failed candidate digest
if grep -q "bbbbbbbbbbbbbbbb" "$T11/.release.env"; then
  assert_eq "0" "1" "Candidate digest was wrongly committed to release.env on rollback"
else
  assert_eq "1" "1" "Candidate digest was NOT committed to release.env on rollback"
fi

# ==============================================================================
# TEST 12: Cleanup Resumes Worker If Quiesced But Container Not Stopped On Abort
# ==============================================================================
printf '\n=== TEST 12: Cleanup Resumes Worker If Quiesced But Not Stopped On Abort ===\n'
T12="$TEST_TMP/t12"
setup_worker_mock_env "$T12"
export WORKER_QUIESCE_CMD="echo '{\"status\":\"quiesced\",\"quiesced\":true,\"generation\":5,\"dispatcher\":\"IDLE\",\"activeDeliveries\":0,\"activePoll\":false,\"journalSeq\":12,\"sessionCheckpointed\":true}'"
export WORKER_STOP_CMD="false" # Stop fails, so container remains unstopped

set +e
"$DEPLOY_DIR/deploy-worker.sh" "ghcr.io/test/worker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
exit_code=$?
set -e

assert_eq "1" "$(( exit_code != 0 ? 1 : 0 ))" "Deploy aborted when container stop failed"
resumed=0
if [[ -f "$T12/worker_resumed" ]]; then
  resumed=1
fi
assert_eq "1" "$resumed" "Quiesced old worker was cleanly resumed during cleanup"

# ==============================================================================
# TEST 13: verify_quiesce_response works when PATH excludes jq
# ==============================================================================
printf '\n=== TEST 13: verify_quiesce_response Works When PATH Excludes jq ===\n'
T13="$TEST_TMP/t13"
setup_worker_mock_env "$T13"
unset WORKER_QUIESCE_CMD
export MOCK_EXEC_LEGACY_WORKER=1
export WORKER_START_CMD="true"
export WORKER_STOP_CMD="true"
export WORKER_READY_CHECK_CMD="true"
export WORKER_READINESS_TIMEOUT="2"

# Create a restricted PATH environment containing required utilities, but strictly NO jq
mkdir -p "$T13/no-jq-bin"
for bin in bash sh env date tr grep egrep sed cut rm mv cp wc mkdir chmod cat touch sleep mktemp tail head awk python3 uname sort uniq dirname pwd basename flock false true; do
  bin_path="$(command -v "$bin" 2>/dev/null || true)"
  if [[ -n "$bin_path" && -x "$bin_path" ]]; then
    ln -sf "$bin_path" "$T13/no-jq-bin/$bin"
  fi
done
# Copy mock docker
cp "$T13/bin/docker" "$T13/no-jq-bin/docker"
chmod +x "$T13/no-jq-bin/docker"

(
  export PATH="$T13/no-jq-bin"
  "$DEPLOY_DIR/deploy-worker.sh" "ghcr.io/test/worker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)
committed_ref="$(grep '^WORKER_IMAGE_REF=' "$T13/.release.env" | cut -d'=' -f2 | tr -d '\r\n')"
assert_eq "ghcr.io/test/worker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" "$committed_ref" "Upgraded successfully without jq in PATH"

# Also directly test verify_quiesce_response fail-closed and parsing logic in no-jq environment
(
  export PATH="$T13/no-jq-bin"
  eval "$(sed -n '/^verify_quiesce_response() {/,/^}/p' "$DEPLOY_DIR/deploy-worker.sh")"

  rc_ok=0
  verify_quiesce_response '{"status":"quiesced","quiesced":true,"generation":5,"dispatcher":"IDLE","activeDeliveries":0,"activePoll":false,"journalSeq":12,"sessionCheckpointed":true}' || rc_ok=$?
  assert_eq "0" "$rc_ok" "verify_quiesce_response accepts quiesced true without jq"

  rc_false=0
  verify_quiesce_response '{"status":"quiesced","quiesced":false}' || rc_false=$?
  assert_eq "1" "$(( rc_false != 0 ? 1 : 0 ))" "verify_quiesce_response rejects quiesced false without jq"

  rc_404=0
  verify_quiesce_response 'worker rpc returned status 404: not found' || rc_404=$?
  assert_eq "1" "$(( rc_404 != 0 ? 1 : 0 ))" "verify_quiesce_response rejects 404 error without jq"

  rc_invalid=0
  verify_quiesce_response '<html>error</html>' || rc_invalid=$?
  assert_eq "1" "$(( rc_invalid != 0 ? 1 : 0 ))" "verify_quiesce_response rejects invalid HTML/text without jq"
)

printf '\n==================================================\n'
printf 'All worker deploy tests completed: %d passed, %d failed\n' "$TESTS_PASSED" "$TESTS_FAILED"
printf '==================================================\n'

if [[ "$TESTS_FAILED" -gt 0 ]]; then
  exit 1
fi
exit 0
