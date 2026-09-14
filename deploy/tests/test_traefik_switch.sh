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
  export MOCK_ACK_WRONG_SLOT=0
  export MOCK_ACK_WRONG_COMMIT=0
  export MOCK_ACK_TIMEOUT=0
  export ROUTE_ACK_TIMEOUT=3

  mkdir -p "$test_dir/bin" "$test_dir/dynamic" "$test_dir/secrets"
  export TRAEFIK_DYNAMIC_DIR="$test_dir/dynamic"
  export ACB_CONFIG="$test_dir/dynamic/acb.yml"
  export ACTIVE_SLOT_FILE="$test_dir/.active-slot"
  export PREVIOUS_SLOT_FILE="$test_dir/.previous-slot"
  export DEPLOY_LOCK_FILE="$test_dir/.deploy.lock"
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
if [[ "$cmd" == "exec" ]]; then
  exit 0
fi
printf 'running\n'
exit 0
EOF
  chmod +x "$test_dir/bin/docker"

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

printf '\n==================================================\n'
printf 'TRAEFIK SWITCH TEST RESULTS: %d PASSED, %d FAILED\n' "$TESTS_PASSED" "$TESTS_FAILED"
printf '==================================================\n'

if [[ "$TESTS_FAILED" -gt 0 ]]; then
  exit 1
fi
exit 0
