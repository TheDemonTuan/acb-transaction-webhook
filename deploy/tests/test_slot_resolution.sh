#!/usr/bin/env bash
# deploy/tests/test_slot_resolution.sh
# Table-driven test suite for fail-closed slot resolution (Task 6).
set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR="$(cd -- "$SCRIPT_DIR/.." && pwd)"

TEST_TMP="$(mktemp -d "${TMPDIR:-/tmp}/acb-slot-res-tests.XXXXXX")"
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

setup_env() {
  local tdir="$1"
  mkdir -p "$tdir/state" "$tdir/traefik" "$tdir/mock_bin" "$tdir/data" "$tdir/deploy"
  export RUNTIME_ROOT="$tdir"
  export DEPLOY_PATH="$tdir"
  export ACTIVE_SLOT_FILE="$tdir/state/gateway-active-slot"
  export FRONTEND_ACTIVE_SLOT_FILE="$tdir/state/frontend-active-slot"
  export ACB_CONFIG="$tdir/traefik/acb.yml"
  export TRAEFIK_DYNAMIC_DIR="$tdir/traefik"
  export PATH="$tdir/mock_bin:$PATH"
  hash -r 2>/dev/null || true

  set_mock_docker "$tdir" "false" "false"

  source "$DEPLOY_DIR/lib.sh"
}

set_mock_docker() {
  local tdir="$1"
  local blue_running="$2"
  local green_running="$3"

  cat <<'EOF' > "$tdir/mock_bin/docker"
#!/usr/bin/env bash
for arg in "$@"; do
  if [[ "$arg" == "acb-gateway-blue" ]]; then
    echo "BLUE_MOCK_VAL"
    exit 0
  elif [[ "$arg" == "acb-gateway-green" ]]; then
    echo "GREEN_MOCK_VAL"
    exit 0
  fi
done
echo "false"
exit 0
EOF
  sed -i "s/BLUE_MOCK_VAL/$blue_running/g" "$tdir/mock_bin/docker"
  sed -i "s/GREEN_MOCK_VAL/$green_running/g" "$tdir/mock_bin/docker"
  chmod +x "$tdir/mock_bin/docker"
  hash -r 2>/dev/null || true
}

set_traefik_route() {
  local slot="$1"
  if [[ -z "$slot" ]]; then
    rm -f "$ACB_CONFIG"
    return
  fi
  cat <<EOF > "$ACB_CONFIG"
http:
  services:
    acb-service:
      loadBalancer:
        servers:
          - url: "http://acb-web-${slot}:8090"
EOF
}

test_gateway_slot_table() {
  local tdir="$TEST_TMP/gw_table"
  setup_env "$tdir"

  # Case 1: state=blue, route=blue -> blue
  printf 'blue' > "$ACTIVE_SLOT_FILE"
  set_traefik_route "blue"
  set_mock_docker "$tdir" "false" "false"
  assert_eq "blue" "$(resolve_gateway_slot_strict)" "Case 1: state=blue, route=blue -> blue"

  # Case 2: state=green, route=green -> green
  printf 'green' > "$ACTIVE_SLOT_FILE"
  set_traefik_route "green"
  assert_eq "green" "$(resolve_gateway_slot_strict)" "Case 2: state=green, route=green -> green"

  # Case 3: state missing, route=blue -> blue
  rm -f "$ACTIVE_SLOT_FILE"
  set_traefik_route "blue"
  assert_eq "blue" "$(resolve_gateway_slot_strict)" "Case 3: state missing, route=blue -> blue"

  # Case 4: state missing, route missing, only blue running -> blue
  rm -f "$ACTIVE_SLOT_FILE"
  set_traefik_route ""
  set_mock_docker "$tdir" "true" "false"
  assert_eq "blue" "$(resolve_gateway_slot_strict)" "Case 4: state missing, route missing, only blue running -> blue"

  # Case 5: state missing, route missing, only green running -> green
  rm -f "$ACTIVE_SLOT_FILE"
  set_traefik_route ""
  set_mock_docker "$tdir" "false" "true"
  assert_eq "green" "$(resolve_gateway_slot_strict)" "Case 5: state missing, route missing, only green running -> green"

  # Case 6: both running + no route/state -> FAIL
  rm -f "$ACTIVE_SLOT_FILE"
  set_traefik_route ""
  set_mock_docker "$tdir" "true" "true"
  local ec=0
  resolve_gateway_slot_strict >/dev/null 2>&1 || ec=$?
  assert_eq "1" "$ec" "Case 6: both running + no route/state -> FAIL"

  # Case 7: neither running + no route/state -> FAIL
  rm -f "$ACTIVE_SLOT_FILE"
  set_traefik_route ""
  set_mock_docker "$tdir" "false" "false"
  ec=0
  resolve_gateway_slot_strict >/dev/null 2>&1 || ec=$?
  assert_eq "1" "$ec" "Case 7: neither running + no route/state -> FAIL"

  # Case 8: state=blue + route=green -> FAIL
  printf 'blue' > "$ACTIVE_SLOT_FILE"
  set_traefik_route "green"
  set_mock_docker "$tdir" "true" "true"
  ec=0
  resolve_gateway_slot_strict >/dev/null 2>&1 || ec=$?
  assert_eq "1" "$ec" "Case 8: state=blue + route=green -> FAIL"

  # Case 9: state=green + route=blue -> FAIL
  printf 'green' > "$ACTIVE_SLOT_FILE"
  set_traefik_route "blue"
  ec=0
  resolve_gateway_slot_strict >/dev/null 2>&1 || ec=$?
  assert_eq "1" "$ec" "Case 9: state=green + route=blue -> FAIL"
}

test_gateway_slot_table

echo "Passed: $TESTS_PASSED, Failed: $TESTS_FAILED"
[[ "$TESTS_FAILED" -eq 0 ]] || exit 1
