#!/usr/bin/env bash
# deploy/tests/test_traefik_switch.sh
# Tests for Traefik route rendering from PUBLIC_ORIGIN, YAML validation,
# atomic dynamic route cutover, real edge identity ACK, and automatic route rollback.
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR="$(cd -- "$SCRIPT_DIR/.." && pwd)"

TEST_TMP="$(mktemp -d "${TMPDIR:-/tmp}/acb-traefik-switch-tests.XXXXXX")"
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

setup_traefik_mock_env() {
  local test_dir="$1"
  export MOCK_STATE_DIR="$test_dir"
  export DEPLOY_PATH="$test_dir"
  export RUNTIME_ROOT="$test_dir"
  export RUNTIME_STATE_DIR="$test_dir/state"
  export RUNTIME_DATA_DIR="$test_dir/data"
  export DEPLOY_STATE_FILE="$test_dir/state/deploy-state.json"
  export MOCK_ACK_WRONG_SLOT=0
  export MOCK_ACK_WRONG_COMMIT=0
  export MOCK_ACK_TIMEOUT=0
  export MOCK_TRAEFIK_MOUNT_MISMATCH=0
  export ROUTE_ACK_TIMEOUT=3

  mkdir -p "$test_dir/bin" "$test_dir/dynamic" "$test_dir/secrets" "$test_dir/state" "$test_dir/data"
  export TRAEFIK_DYNAMIC_DIR="$test_dir/dynamic"
  export ACB_CONFIG="$test_dir/dynamic/acb.yml"
  export ACTIVE_SLOT_FILE="$test_dir/.active-slot"
  export PREVIOUS_SLOT_FILE="$test_dir/.previous-slot"
  export DEPLOY_LOCK_FILE="$test_dir/.deploy.lock"
  export ALLOW_TEST_LOCK_PATH=1
  export FAILOVER_STATE_DIR="$test_dir/failover"
  mkdir -p "$FAILOVER_STATE_DIR"
  export ENV_FILE="$test_dir/.env.production"
  export SECRETS_DIR="$test_dir/secrets"

  cat <<EOF > "$test_dir/.env.production"
APP_ENV=production
RUNTIME_ROLE=gateway
PUBLIC_ORIGIN=https://bank.tuannguyenviet.site
DATA_VOLUME_NAME=bank-event-gateway_gateway_data
EOF

  printf 'mock-master-key\n' > "$test_dir/secrets/app_master_key"
  printf 'mock-tts-token\n' > "$test_dir/secrets/tts_internal_token"
  printf 'mock-worker-token\n' > "$test_dir/secrets/worker_internal_token"
  printf 'mock-bark-user\n' > "$test_dir/secrets/bark_basic_auth_user"
  printf 'mock-bark-pass\n' > "$test_dir/secrets/bark_basic_auth_password"

  # Initial route points to blue
  cat <<EOF > "$ACB_CONFIG"
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
  printf 'blue' > "$ACTIVE_SLOT_FILE"

  # Mock curl CLI
  cat <<'EOF' > "$test_dir/bin/curl"
#!/usr/bin/env bash
set -eu
if [[ "${MOCK_ACK_TIMEOUT:-0}" == "1" ]]; then
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

if [[ "${MOCK_ACK_WRONG_SLOT:-0}" == "1" ]]; then
  active_slot="wrong-slot-xyz"
fi

slot_commit="${EXPECTED_COMMIT:-commit-12345}"
if [[ "${MOCK_ACK_WRONG_COMMIT:-0}" == "1" ]]; then
  slot_commit="wrong-commit-abc"
fi

if [[ -n "$hdr_file" ]]; then
  cat <<HDR > "$hdr_file"
HTTP/1.1 200 OK
Content-Type: application/json
X-Platform-Slot: ${active_slot}
X-Release-Commit: ${slot_commit}
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

  # Mock docker CLI
  cat <<'EOF' > "$test_dir/bin/docker"
#!/usr/bin/env bash
set -eu
cmd="${1:-}"
if [[ "$cmd" == "info" || "$cmd" == "network" || "$cmd" == "image" || "$cmd" == "exec" ]]; then
  exit 0
fi
if [[ "$cmd" == "inspect" ]]; then
  args="$*"
  if [[ "$args" == *"edge-traefik"* ]]; then
    if [[ "${MOCK_TRAEFIK_MOUNT_MISMATCH:-0}" == "1" ]]; then
      printf '/mismatched/nonexistent/dynamic\n'
      exit 0
    fi
    printf '%s\n' "${TRAEFIK_DYNAMIC_DIR:-$MOCK_STATE_DIR/dynamic}"
    exit 0
  fi
  if [[ "$args" == *"edge-cloudflared"* ]]; then
    printf 'true\n'
    exit 0
  fi
  printf 'running\n'
  exit 0
fi
exit 0
EOF
  chmod +x "$test_dir/bin/docker"

  # Mock edge-probe.sh CLI
  cat <<'EOF' > "$test_dir/edge-probe.sh"
#!/usr/bin/env bash
set -eu
target="production"
expected_slot=""
expected_commit=""
check_runtime=0
expected_status=200

while [[ $# -gt 0 ]]; do
  case "$1" in
    --target) target="$2"; shift 2 ;;
    --expected-slot) expected_slot="$2"; shift 2 ;;
    --expected-commit) expected_commit="$2"; shift 2 ;;
    --expected-status) expected_status="$2"; shift 2 ;;
    --check-runtime) check_runtime=1; shift ;;
    *) shift ;;
  esac
done

if [[ "$check_runtime" -eq 1 ]]; then
  if [[ "${MOCK_PROBE_RUNTIME_FAIL:-0}" == "1" ]]; then
    printf "Runtime check failed\n" >&2
    exit 1
  fi
  exit 0
fi

if [[ "${MOCK_ACK_TIMEOUT:-0}" == "1" ]]; then
  printf "Mock probe timeout\n" >&2
  exit 1
fi

active_slot="blue"
if [[ -f "${ACB_CONFIG:-}" ]]; then
  if grep -q "acb-web-green" "$ACB_CONFIG" 2>/dev/null; then
    active_slot="green"
  fi
fi

if [[ "${MOCK_ACK_WRONG_SLOT:-0}" == "1" ]]; then
  active_slot="wrong-slot-xyz"
fi

slot_commit="${EXPECTED_COMMIT:-commit-12345}"
if [[ "${MOCK_ACK_WRONG_COMMIT:-0}" == "1" ]]; then
  slot_commit="wrong-commit-abc"
fi

if [[ "${MOCK_ACK_LEGACY:-0}" == "1" ]]; then
  if [[ -n "$expected_slot" || -n "$expected_commit" ]]; then
    printf "ERROR: Route identity ACK missing or unknown X-Platform-Slot header in response\n" >&2
    exit 1
  fi
  printf "HTTP Status: 200 (expected: 200)\n"
  exit 0
fi

if [[ -n "$expected_slot" && "$expected_slot" != "$active_slot" ]]; then
  printf "ERROR: Route identity ACK slot mismatch: expected '%s', got '%s'\n" "$expected_slot" "$active_slot" >&2
  exit 1
fi

if [[ -n "$expected_commit" && "$expected_commit" != "$slot_commit" ]]; then
  printf "ERROR: Route identity ACK commit mismatch: expected '%s', got '%s'\n" "$expected_commit" "$slot_commit" >&2
  exit 1
fi

printf "HTTP Status: 200 (expected: 200)\n"
printf "Verified route identity slot: %s\n" "$active_slot"
printf "Verified route identity commit: %s\n" "$slot_commit"
exit 0
EOF
  chmod +x "$test_dir/edge-probe.sh"
  export EDGE_PROBE_SCRIPT="$test_dir/edge-probe.sh"

  export PATH="$test_dir/bin:$PATH"
}

# ==============================================================================
# TEST 1: Hostname derivation from PUBLIC_ORIGIN & PUBLIC_HOST
# ==============================================================================
printf '\n=== TEST 1: Hostname Derivation from PUBLIC_ORIGIN ===\n'
T1="$TEST_TMP/t1"
setup_traefik_mock_env "$T1"

(
  # shellcheck source=deploy/lib/common.sh
  source "$DEPLOY_DIR/lib/common.sh"
  # shellcheck source=deploy/lib/traefik.sh
  source "$DEPLOY_DIR/lib/traefik.sh"

  PUBLIC_ORIGIN="https://bank.tuannguyenviet.site"
  h1="$(get_route_host)"
  assert_eq "bank.tuannguyenviet.site" "$h1" "Derived pure hostname from https URL"

  PUBLIC_ORIGIN="http://bank.mycompany.internal:8080/prefix"
  h2="$(get_route_host)"
  assert_eq "bank.mycompany.internal" "$h2" "Derived hostname stripping port and prefix"

  # Reject invalid hostname formats
  PUBLIC_ORIGIN="https://invalid host with spaces"
  set +e
  get_route_host >/dev/null 2>&1
  code=$?
  set -e
  assert_eq "1" "$(( code != 0 ? 1 : 0 ))" "Rejected invalid hostname with spaces"
)

# ==============================================================================
# TEST 2: render-traefik-route.sh renders and validates configuration
# ==============================================================================
printf '\n=== TEST 2: render-traefik-route.sh Execution ===\n'
T2="$TEST_TMP/t2"
setup_traefik_mock_env "$T2"

rendered_file="$T2/dynamic/rendered-green.yml"
"$DEPLOY_DIR/render-traefik-route.sh" green "$rendered_file"

assert_file_exists "$rendered_file" "Rendered YAML file created"
assert_file_contains "$rendered_file" "Host(\`bank.tuannguyenviet.site\`)" "Contains Host rule matching PUBLIC_ORIGIN"
assert_file_contains "$rendered_file" "Host(\`transactions.tuannguyenviet.site\`)" "Contains Host rule matching PUBLIC_VIEWER_HOST"
assert_file_contains "$rendered_file" "acb-public-api-router" "Contains acb-public-api-router"
assert_file_contains "$rendered_file" "acb-public-sse-router" "Contains acb-public-sse-router"
assert_file_contains "$rendered_file" "public-api-rate-limit" "Contains public-api-rate-limit"
assert_file_contains "$rendered_file" "public-sse-rate-limit" "Contains public-sse-rate-limit"
assert_file_contains "$rendered_file" "priority: 350" "Contains SSE router priority 350"
assert_file_contains "$rendered_file" "acb-public-deny-private" "Contains acb-public-deny-private"
assert_file_contains "$rendered_file" "acb-public-frontend-router" "Contains acb-public-frontend-router"
assert_file_contains "$rendered_file" "http://acb-web-green:8090" "Contains backend URL pointing to green"
assert_file_contains "$rendered_file" "acb-deny-internal" "Contains deny-internal security rule"
assert_file_contains "$rendered_file" "tunnel-only" "Contains tunnel-only middleware"

# ==============================================================================
# TEST 3: switch-slot.sh switches route and acknowledges identity
# ==============================================================================
printf '\n=== TEST 3: switch-slot.sh Cutover with Route Identity ACK ===\n'
T3="$TEST_TMP/t3"
setup_traefik_mock_env "$T3"
export EXPECTED_COMMIT="release-v2-1234"

"$DEPLOY_DIR/switch-slot.sh" green

assert_eq "green" "$(cat "$T3/.active-slot")" "Active slot file updated to green"
assert_eq "blue" "$(cat "$T3/.previous-slot")" "Previous slot file recorded as blue"
assert_file_contains "$T3/dynamic/acb.yml" "acb-web-green" "Active route file updated to acb-web-green"
assert_file_exists "$T3/dynamic/acb.yml.prev" "Previous route copy saved for instant rollback"

# ==============================================================================
# TEST 4: Route ACK Failure Triggers Automatic Rollback
# ==============================================================================
printf '\n=== TEST 4: Forced Route ACK Failure Triggers Rollback ===\n'
T4="$TEST_TMP/t4"
setup_traefik_mock_env "$T4"
export MOCK_ACK_TIMEOUT=1

set +e
"$DEPLOY_DIR/switch-slot.sh" green
exit_code=$?
set -e

assert_eq "1" "$(( exit_code != 0 ? 1 : 0 ))" "switch-slot.sh exited with error code on ACK failure"
assert_file_contains "$T4/dynamic/acb.yml" "acb-web-blue" "Route reverted back to blue on ACK failure"

# ==============================================================================
# TEST 5: rollback-warm.sh restores previous slot and confirms route identity
# ==============================================================================
printf '\n=== TEST 5: rollback-warm.sh Successful Rollback ===\n'
T5="$TEST_TMP/t5"
setup_traefik_mock_env "$T5"
# Simulate active is green, previous was blue
printf 'green' > "$T5/.active-slot"
printf 'blue' > "$T5/.previous-slot"
"$DEPLOY_DIR/render-traefik-route.sh" green "$T5/dynamic/acb.yml"

"$DEPLOY_DIR/rollback-warm.sh"

assert_eq "blue" "$(cat "$T5/.active-slot")" "Active slot successfully rolled back to blue"
assert_file_contains "$T5/dynamic/acb.yml" "acb-web-blue" "Active route successfully reverted to acb-web-blue"

# ==============================================================================
# TEST 6: Traefik dynamic directory mount mismatch is rejected before cutover
# ==============================================================================
printf '\n=== TEST 6: Traefik Dynamic Directory Mount Mismatch Rejection ===\n'
T6="$TEST_TMP/t6"
setup_traefik_mock_env "$T6"
export MOCK_TRAEFIK_MOUNT_MISMATCH=1

set +e
"$DEPLOY_DIR/switch-slot.sh" green
mismatch_code=$?
set -e

assert_eq "1" "$(( mismatch_code != 0 ? 1 : 0 ))" "switch-slot.sh failed closed on dynamic mount mismatch"
assert_eq "blue" "$(cat "$T6/.active-slot")" "Active slot untouched after preflight failure"
assert_file_contains "$T6/dynamic/acb.yml" "acb-web-blue" "Route configuration untouched after preflight failure"
export MOCK_TRAEFIK_MOUNT_MISMATCH=0

# ==============================================================================
# TEST 7: Route identity ACK failure on wrong commit triggers rollback
# ==============================================================================
printf '\n=== TEST 7: Route Identity ACK Commit Mismatch Triggers Rollback ===\n'
T7="$TEST_TMP/t7"
setup_traefik_mock_env "$T7"
export MOCK_ACK_WRONG_COMMIT=1
export EXPECTED_COMMIT="expected-commit-999"

set +e
"$DEPLOY_DIR/switch-slot.sh" green
commit_mismatch_code=$?
set -e

assert_eq "1" "$(( commit_mismatch_code != 0 ? 1 : 0 ))" "switch-slot.sh failed on commit mismatch"
assert_eq "blue" "$(cat "$T7/.active-slot")" "Active slot reverted to blue after commit mismatch"
assert_file_contains "$T7/dynamic/acb.yml" "acb-web-blue" "Route reverted back to blue on commit mismatch"

# ==============================================================================
# TEST 8: Legacy rollback succeeds with availability confirmation
# ==============================================================================
printf '\n=== TEST 8: Legacy Rollback Availability Confirmation ===\n'
T8="$TEST_TMP/t8"
setup_traefik_mock_env "$T8"
printf 'green' > "$T8/.active-slot"
printf 'blue' > "$T8/.previous-slot"
"$DEPLOY_DIR/render-traefik-route.sh" green "$T8/dynamic/acb.yml"
export MOCK_ACK_LEGACY=1
export EXPECTED_COMMIT=""

"$DEPLOY_DIR/rollback-warm.sh"

assert_eq "blue" "$(cat "$T8/.active-slot")" "Legacy slot successfully rolled back to blue"
assert_file_contains "$T8/dynamic/acb.yml" "acb-web-blue" "Route reverted to blue on legacy rollback"

# ==============================================================================
# TEST 9: Explicit Route and Stop Safety Fail-Closed Contract
# ==============================================================================
printf '\n=== TEST 9: Explicit Route and Stop Safety Fail-Closed Contract ===\n'
T9="$TEST_TMP/t9"
setup_traefik_mock_env "$T9"
source "$DEPLOY_DIR/lib.sh"

# 1. Traefik dynamic directory mismatch inside if / && conditional context
export MOCK_TRAEFIK_MOUNT_MISMATCH=1
route_before="$(cat "$ACB_CONFIG")"
sw_code=0
if atomic_switch_route green && ack_route_identity green "" 3; then
  sw_code=0
else
  sw_code=1
fi
assert_eq "1" "$sw_code" "atomic_switch_route fails closed inside if/&& conditional when prerequisites fail"
route_after="$(cat "$ACB_CONFIG")"
assert_eq "$route_before" "$route_after" "Route config bytes untouched on prerequisite failure"
assert_eq "blue" "$(cat "$ACTIVE_SLOT_FILE")" "Active slot untouched on prerequisite failure"
export MOCK_TRAEFIK_MOUNT_MISMATCH=0

# 2. Stop standby fails closed when failover state dir is unwritable
mkdir -p "$T9/unwritable_failover"
chmod 500 "$T9/unwritable_failover"
export FAILOVER_STATE_DIR="$T9/unwritable_failover"

stop_code=0
if stop_standby_container green; then
  stop_code=0
else
  stop_code=1
fi
assert_eq "1" "$stop_code" "stop_standby_container returns nonzero when intentional-stop marker cannot be written"
chmod 700 "$T9/unwritable_failover"
export FAILOVER_STATE_DIR="$T9/failover"

# 3. Rollback route ACK failure preserves both slots and pending evidence
export PENDING_GATEWAY_RETIRE_FILE="$T9/pending-gateway-retire.env"
printf 'old_slot=blue\ncandidate_slot=green\n' > "$PENDING_GATEWAY_RETIRE_FILE"
export MOCK_ACK_TIMEOUT=1
export ROUTE_ACK_TIMEOUT=1

rb_code=0
if rollback_gateway_route; then
  rb_code=0
else
  rb_code=1
fi
assert_eq "1" "$rb_code" "rollback_gateway_route fails when route ACK fails"
assert_file_exists "$PENDING_GATEWAY_RETIRE_FILE" "Pending evidence preserved when rollback route ACK fails"
unset MOCK_ACK_TIMEOUT

printf '\n==================================================\n'
printf 'TRAEFIK SWITCH TEST RESULTS: %d PASSED, %d FAILED\n' "$TESTS_PASSED" "$TESTS_FAILED"
printf '==================================================\n'

if [[ "$TESTS_FAILED" -gt 0 ]]; then
  exit 1
fi
exit 0
