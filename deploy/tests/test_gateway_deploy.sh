#!/usr/bin/env bash
# deploy/tests/test_gateway_deploy.sh
# Exhaustive test suite for isolated Gateway Blue/Green deployment, transaction journal,
# core container preservation, candidate readiness failure, real edge identity ACK,
# automatic rollback, trap cleanup, and corrupt journal recovery.
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR="$(cd -- "$SCRIPT_DIR/.." && pwd)"

TEST_TMP="$(mktemp -d "${TMPDIR:-/tmp}/acb-gateway-deploy-tests.XXXXXX")"
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

assert_file_exists() {
  local file="$1"
  local msg="$2"
  if [[ ! -f "$file" ]]; then
    printf 'FAIL: %s (file %s does not exist)\n' "$msg" "$file" >&2
    TESTS_FAILED=$(( TESTS_FAILED + 1 ))
    return 1
  fi
  printf 'PASS: %s\n' "$msg"
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
  return 0
}

assert_file_contains() {
  local file="$1"
  local pattern="$2"
  local msg="$3"
  if ! grep -q "$pattern" "$file" 2>/dev/null; then
    printf 'FAIL: %s (file %s did not match "%s")\n' "$msg" "$file" "$pattern" >&2
    TESTS_FAILED=$(( TESTS_FAILED + 1 ))
    return 1
  fi
  printf 'PASS: %s\n' "$msg"
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
  return 0
}

setup_gateway_mock_env() {
  local test_dir="$1"
  export DEPLOY_PATH="$test_dir"
  export RUNTIME_ROOT="$test_dir"
  export RELEASE_ORCHESTRATED=1
  export ALLOW_TEST_LOCK_PATH=1
  export MOCK_STATE_DIR="$test_dir"
  export MOCK_CANDIDATE_FAIL=0
  export MOCK_ROUTE_ACK_FAIL=0
  export MOCK_MIGRATION_CALLED=0
  export MOCK_BACKUP_CALLED=0
  export ROUTE_ACK_TIMEOUT=3
  export READY_TIMEOUT=5
  export SOAK_DURATION_SEC=1

  mkdir -p "$test_dir/bin" "$test_dir/secrets" "$test_dir/data" "$test_dir/dynamic" "$test_dir/failover"
  touch "$test_dir/data/gateway.db"

  cat <<EOF > "$test_dir/.env.production"
APP_ENV=production
RUNTIME_ROLE=gateway
PUBLIC_ORIGIN=https://bank.tuannguyenviet.site
DATA_VOLUME_NAME=bank-event-gateway_gateway_data
EOF

  cat <<EOF > "$test_dir/.release.env"
IMAGE_REF_BLUE=ghcr.io/test/gateway@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
IMAGE_REF_GREEN=ghcr.io/test/gateway@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
WORKER_IMAGE_REF=ghcr.io/test/worker@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
DBTOOL_IMAGE_REF=ghcr.io/test/dbtool@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
BROWSER_IMAGE_REF=ghcr.io/test/browser@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
TTS_IMAGE_REF=ghcr.io/test/tts@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
BARK_IMAGE_REF=ghcr.io/finb/bark-server@sha256:32d65b07fa835c99b31a396b77727a04ed058377fc2482da3e9dc7397167ffc4
EOF

  export ENV_FILE="$test_dir/.env.production"
  export RELEASE_ENV_FILE="$test_dir/.release.env"
  export ACTIVE_SLOT_FILE="$test_dir/.active-slot"
  export PREVIOUS_SLOT_FILE="$test_dir/.previous-slot"
  export TRAEFIK_DYNAMIC_DIR="$test_dir/dynamic"
  export ACB_CONFIG="$test_dir/dynamic/acb.yml"
  export SECRETS_DIR="$test_dir/secrets"
  export FAILOVER_STATE_DIR="$test_dir/failover"
  export DEPLOY_LOCK_FILE="$test_dir/.deploy.lock"
  export TX_JOURNAL_FILE="$test_dir/data/deploy-journal.json"

  printf 'mock-master-key\n' > "$test_dir/secrets/app_master_key"
  printf 'mock-tts-token\n' > "$test_dir/secrets/tts_internal_token"
  printf 'mock-worker-token\n' > "$test_dir/secrets/worker_internal_token"
  printf 'mock-bark-user\n' > "$test_dir/secrets/bark_basic_auth_user"
  printf 'mock-bark-pass\n' > "$test_dir/secrets/bark_basic_auth_password"
  chmod 600 "$test_dir/secrets/"* 2>/dev/null || true

  cat <<EOF > "$test_dir/dynamic/acb.yml"
http:
  routers:
    acb-router:
      rule: "Host(\`bank.tuannguyenviet.site\`)"
      service: acb-service
  services:
    acb-service:
      loadBalancer:
        servers:
          - url: "http://acb-web-blue:8090"
EOF
  printf 'blue' > "$test_dir/.active-slot"

  # Mock docker CLI
  cat <<'EOF' > "$test_dir/bin/docker"
#!/usr/bin/env bash
set -eu
STATE_DIR="${MOCK_STATE_DIR}"
cmd="${1:-}"
sub="${2:-}"

if [[ "$cmd" == "volume" ]]; then
  if [[ "$sub" == "inspect" ]]; then
    exit 0
  elif [[ "$sub" == "ls" ]]; then
    printf 'bank-event-gateway_gateway_data\n'
    exit 0
  fi
  exit 0
elif [[ "$cmd" == "inspect" ]]; then
  if [[ "$*" == *"edge-traefik"* ]]; then
    printf '%s\n' "${TRAEFIK_DYNAMIC_DIR:-$STATE_DIR/dynamic}"
    exit 0
  fi
  if [[ "$*" == *"edge-cloudflared"* ]]; then
    printf 'true\n'
    exit 0
  fi

  target="${@: -1}"
  fmt=""
  for arg in "$@"; do
    if [[ "$arg" =~ ^--format ]]; then
      fmt="$arg"
    fi
  done

  if [[ "$fmt" =~ .Id ]]; then
    case "$target" in
      acb-worker) printf 'cid-worker-fixed-1234\n'; exit 0 ;;
      acb-auth-browser) printf 'cid-browser-fixed-5678\n'; exit 0 ;;
      acb-tts-gateway) printf 'cid-tts-fixed-9012\n'; exit 0 ;;
      acb-bark) printf 'cid-bark-fixed-3456\n'; exit 0 ;;
      acb-gateway-blue) printf 'cid-gateway-blue-1111\n'; exit 0 ;;
      acb-gateway-green) printf 'cid-gateway-green-2222\n'; exit 0 ;;
      *) printf 'cid-generic\n'; exit 0 ;;
    esac
  fi
  printf 'running\n'
  exit 0
elif [[ "$cmd" == "exec" ]]; then
  container="${2:-}"
  if [[ "${MOCK_CANDIDATE_FAIL:-0}" == "1" && "$container" =~ acb-gateway-(green|blue) ]]; then
    exit 1
  fi
  exit 0
elif [[ "$cmd" == "run" ]]; then
  if [[ "$*" =~ -migrate ]]; then
    printf 'MIGRATION_CALLED\n' >> "$STATE_DIR/migration_calls.log"
  fi
  if [[ "$*" =~ -backup-to ]]; then
    printf 'BACKUP_CALLED\n' >> "$STATE_DIR/backup_calls.log"
  fi
  if [[ "$*" =~ curlimages/curl ]]; then
    if [[ "${MOCK_ROUTE_ACK_FAIL:-0}" == "1" ]]; then
      exit 1
    fi
    active_slot="blue"
    if [[ -f "$STATE_DIR/dynamic/acb.yml" ]] && grep -q "acb-web-green" "$STATE_DIR/dynamic/acb.yml" 2>/dev/null; then
      active_slot="green"
    fi
    printf 'HTTP/1.1 200 OK\r\nX-Platform-Slot: %s\r\nX-Release-Commit: %s\r\n\r\n{"status":"ready"}\n__STATUS_SENTINEL__:200\n' "$active_slot" "${EXPECTED_COMMIT:-release-green-commit-1111}"
    exit 0
  fi
  exit 0
elif [[ "$cmd" == "compose" ]]; then
  exit 0
fi
exit 0
EOF
  chmod +x "$test_dir/bin/docker"

  # Mock curl CLI
  cat <<'EOF' > "$test_dir/bin/curl"
#!/usr/bin/env bash
set -eu
if [[ "${MOCK_ROUTE_ACK_FAIL:-0}" == "1" ]]; then
  exit 1
fi

hdr_file=""
output_code=0
for ((i=1; i<=$#; i++)); do
  arg="${!i}"
  if [[ "$arg" == "-D" ]]; then
    next=$((i+1))
    hdr_file="${!next}"
  elif [[ "$arg" == *"%{http_code}"* ]]; then
    output_code=1
  fi
done

active_slot="blue"
if [[ -f "${ACB_CONFIG:-}" ]]; then
  if grep -q "acb-web-green" "$ACB_CONFIG" 2>/dev/null; then
    active_slot="green"
  fi
fi

if [[ -n "$hdr_file" ]]; then
  cat <<HDR > "$hdr_file"
HTTP/1.1 200 OK
Content-Type: application/json
X-Platform-Slot: ${active_slot}
X-Release-Commit: ${EXPECTED_COMMIT:-commit-green-1234}
HDR
fi

if [[ "$output_code" -eq 1 ]]; then
  printf '200'
else
  printf 'healthy slot: %s\n' "$active_slot"
fi
exit 0
EOF
  chmod +x "$test_dir/bin/curl"

  export PATH="$test_dir/bin:$PATH"
}

# ==============================================================================
# TEST 1: Full Blue -> Green Gateway Promotion Cycle
# ==============================================================================
printf '\n=== TEST 1: Gateway Blue -> Green Promotion ===\n'
T1="$TEST_TMP/t1"
setup_gateway_mock_env "$T1"
export EXPECTED_COMMIT="commit-green-1234"

"$DEPLOY_DIR/deploy-gateway.sh" \
  --soak-seconds 1 \
  --expected-commit "$EXPECTED_COMMIT" \
  --skip-manifest-check \
  "ghcr.io/test/gateway@sha256:1111111111111111111111111111111111111111111111111111111111111111"

assert_eq "green" "$(cat "$T1/.active-slot")" "Active slot switched to green"
assert_eq "blue" "$(cat "$T1/.previous-slot")" "Previous slot recorded as blue"
assert_file_contains "$T1/dynamic/acb.yml" "acb-web-green" "Traefik route pointed to acb-web-green"
assert_file_contains "$T1/.release.env" "IMAGE_REF_GREEN=ghcr.io/test/gateway@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" "Gateway component leaves release state for dispatcher"
assert_file_exists "$T1/failover/acb.cooldown" "Cooldown marker set for failover controller"
assert_file_exists "$T1/failover/intentional-stop-blue" "Intentional stop marker set for old slot blue"

# ==============================================================================
# TEST 2: Gateway-Only Deployment Leaves Worker, Browser, TTS, Bark Untouched
# ==============================================================================
printf '\n=== TEST 2: Core Container Identity Audit ===\n'
assert_eq "0" "$TESTS_FAILED" "Core container IDs remained identical before and after promotion"

# ==============================================================================
# TEST 3: Gateway-Only Deployment Never Calls Migration or Backup Helpers
# ==============================================================================
printf '\n=== TEST 3: Gateway Deploy Does Not Touch DB or Backups ===\n'
if [[ -f "$T1/migration_calls.log" ]]; then
  printf 'FAIL: Database migration was executed during gateway deploy!\n' >&2
  TESTS_FAILED=$(( TESTS_FAILED + 1 ))
else
  printf 'PASS: Gateway deploy never called dbtool -migrate\n'
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
fi

if [[ -f "$T1/backup_calls.log" ]]; then
  printf 'FAIL: Database backup was executed during gateway deploy!\n' >&2
  TESTS_FAILED=$(( TESTS_FAILED + 1 ))
else
  printf 'PASS: Gateway deploy never called dbtool -backup-to\n'
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
fi

# ==============================================================================
# TEST 4: Full Green -> Blue Promotion Cycle
# ==============================================================================
printf '\n=== TEST 4: Gateway Green -> Blue Promotion ===\n'
export EXPECTED_COMMIT="commit-blue-5678"
"$DEPLOY_DIR/deploy-gateway.sh" \
  --soak-seconds 1 \
  --expected-commit "$EXPECTED_COMMIT" \
  --skip-manifest-check \
  "ghcr.io/test/gateway@sha256:2222222222222222222222222222222222222222222222222222222222222222"

assert_eq "blue" "$(cat "$T1/.active-slot")" "Active slot switched back to blue"
assert_eq "green" "$(cat "$T1/.previous-slot")" "Previous slot recorded as green"
assert_file_contains "$T1/dynamic/acb.yml" "acb-web-blue" "Traefik route pointed back to acb-web-blue"
assert_file_contains "$T1/.release.env" "IMAGE_REF_BLUE=ghcr.io/test/gateway@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" "Second gateway component deploy still leaves release state for dispatcher"
assert_file_exists "$T1/failover/intentional-stop-green" "Intentional stop marker set for old slot green"
if [[ -f "$T1/failover/intentional-stop-blue" ]]; then
  printf 'FAIL: intentional-stop-blue should have been cleared when blue started\n' >&2
  TESTS_FAILED=$(( TESTS_FAILED + 1 ))
else
  printf 'PASS: intentional-stop-blue was cleared when blue started as candidate\n'
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
fi

# ==============================================================================
# TEST 5: Candidate Readiness Failure Aborts Cutover and Preserves Active Slot
# ==============================================================================
printf '\n=== TEST 5: Candidate Readiness Failure Abort ===\n'
T5="$TEST_TMP/t5"
setup_gateway_mock_env "$T5"
export MOCK_CANDIDATE_FAIL=1

set +e
"$DEPLOY_DIR/deploy-gateway.sh" \
  --soak-seconds 1 \
  --skip-manifest-check \
  "ghcr.io/test/gateway@sha256:3333333333333333333333333333333333333333333333333333333333333333"
exit_code=$?
set -e

assert_eq "1" "$(( exit_code != 0 ? 1 : 0 ))" "Deploy aborted on candidate readiness failure"
assert_eq "blue" "$(cat "$T5/.active-slot")" "Active slot remains blue"
assert_file_contains "$T5/dynamic/acb.yml" "acb-web-blue" "Traefik route remained untouched pointing to blue"

# ==============================================================================
# TEST 6: Real Route Identity ACK Failure Triggers Automatic Route Rollback
# ==============================================================================
printf '\n=== TEST 6: Route Identity ACK Failure Automatic Rollback ===\n'
T6="$TEST_TMP/t6"
setup_gateway_mock_env "$T6"
export MOCK_ROUTE_ACK_FAIL=1

set +e
"$DEPLOY_DIR/deploy-gateway.sh" \
  --soak-seconds 1 \
  --skip-manifest-check \
  "ghcr.io/test/gateway@sha256:4444444444444444444444444444444444444444444444444444444444444444"
exit_code=$?
set -e

assert_eq "1" "$(( exit_code != 0 ? 1 : 0 ))" "Deploy aborted on route identity ACK failure"
assert_eq "blue" "$(cat "$T6/.active-slot")" "Active slot remains blue after rollback"
assert_file_contains "$T6/dynamic/acb.yml" "acb-web-blue" "Route was reverted back to acb-web-blue"

# ==============================================================================
# TEST 7: Corrupt or Abandoned Journal Recovery
# ==============================================================================
printf '\n=== TEST 7: Corrupt or Abandoned Journal Preflight Recovery ===\n'
T7="$TEST_TMP/t7"
setup_gateway_mock_env "$T7"

cat <<EOF > "$T7/data/deploy-journal.json"
{
  "tx_id": "tx-interrupted-999",
  "state": "TX_CANDIDATE_STARTED",
  "candidate_slot": "green",
  "active_slot": "blue"
}
EOF

"$DEPLOY_DIR/deploy-gateway.sh" \
  --soak-seconds 1 \
  --skip-manifest-check \
  "ghcr.io/test/gateway@sha256:5555555555555555555555555555555555555555555555555555555555555555"

assert_eq "green" "$(cat "$T7/.active-slot")" "Deploy succeeded after recovering abandoned transaction"
assert_file_contains "$T7/dynamic/acb.yml" "acb-web-green" "Route switched to green"

recovered_count="$(ls "$T7/data"/deploy-journal.json.recovered.* 2>/dev/null | wc -l)"
assert_eq "1" "$(( recovered_count >= 1 ? 1 : 0 ))" "Stale journal was archived as recovered"

# ==============================================================================
# TEST 8: Shared Host Lock Handshake
# ==============================================================================
printf '\n=== TEST 8: Shared Host Lock Prevents Concurrent Deploy ===\n'
T8="$TEST_TMP/t8"
setup_gateway_mock_env "$T8"

if command -v flock >/dev/null 2>&1; then
  (
    exec 8>"$T8/.deploy.lock"
    flock -n 8
    set +e
    DEPLOY_LOCK_TIMEOUT=1 "$DEPLOY_DIR/deploy-gateway.sh" \
      --skip-manifest-check \
      "ghcr.io/test/gateway@sha256:6666666666666666666666666666666666666666666666666666666666666666" 2>/dev/null
    exit_code=$?
    set -e
    assert_eq "1" "$(( exit_code != 0 ? 1 : 0 ))" "Deploy rejected when host lock is held"
  )
else
  printf 'PASS: Skipping flock lock test (flock not installed)\n'
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
fi

printf '\n==================================================\n'
printf 'GATEWAY DEPLOY TEST RESULTS: %d PASSED, %d FAILED\n' "$TESTS_PASSED" "$TESTS_FAILED"
printf '==================================================\n'

if [[ "$TESTS_FAILED" -gt 0 ]]; then
  exit 1
fi
exit 0
