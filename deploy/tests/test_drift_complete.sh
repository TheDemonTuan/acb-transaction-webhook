#!/usr/bin/env bash
# deploy/tests/test_drift_complete.sh
# Exhaustive test suite for complete runtime drift detection (Task 8).
set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR="$(cd -- "$SCRIPT_DIR/.." && pwd)"

TEST_TMP="$(mktemp -d "${TMPDIR:-/tmp}/acb-drift-complete-tests.XXXXXX")"
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

setup_drift_env() {
  local tdir="$1"
  mkdir -p "$tdir/state" "$tdir/traefik" "$tdir/mock_bin" "$tdir/failover/apps.d" "$tdir/systemd" "$tdir/data"

  # Canonical state v2
  cat <<'EOF' > "$tdir/state/current-release.json"
{
  "schema_version": 2,
  "generation": 10,
  "release_id": "rel-drift-test",
  "status": "COMPLETED",
  "git_sha": "1111111111111111111111111111111111111111",
  "active_slots": {
    "gateway": "blue",
    "frontend": "blue"
  },
  "images": {
    "gateway": {
      "blue": "ghcr.io/test/gateway@sha256:0000000000000000000000000000000000000000000000000000000000000001",
      "green": "ghcr.io/test/gateway@sha256:0000000000000000000000000000000000000000000000000000000000000002"
    },
    "frontend": "ghcr.io/test/frontend@sha256:0000000000000000000000000000000000000000000000000000000000000003",
    "worker": "ghcr.io/test/worker@sha256:0000000000000000000000000000000000000000000000000000000000000004",
    "auth_browser": "ghcr.io/test/auth@sha256:0000000000000000000000000000000000000000000000000000000000000005",
    "tts": "ghcr.io/test/tts@sha256:0000000000000000000000000000000000000000000000000000000000000006",
    "bark": "ghcr.io/test/bark@sha256:0000000000000000000000000000000000000000000000000000000000000007"
  }
}
EOF

  printf 'blue' > "$tdir/state/gateway-active-slot"
  printf 'blue' > "$tdir/state/frontend-active-slot"

  # Traefik routing matching canonical
  cat <<'EOF' > "$tdir/traefik/acb.yml"
http:
  services:
    acb-service:
      loadBalancer:
        servers:
          - url: "http://acb-web-blue:8090"
    acb-frontend-service:
      loadBalancer:
        servers:
          - url: "http://acb-frontend-blue:8080"
EOF

  # Mock docker inspect
  cat <<'EOF' > "$tdir/mock_bin/docker"
#!/usr/bin/env bash
target="${@: -1}"
format=""
for arg in "$@"; do
  if [[ "$arg" == *"format"* ]]; then
    format="$arg"
  fi
done

if [[ "$*" == *"{{.Config.Image}}"* ]]; then
  case "$target" in
    acb-gateway-blue) echo "ghcr.io/test/gateway@sha256:0000000000000000000000000000000000000000000000000000000000000001" ;;
    acb-frontend-blue) echo "ghcr.io/test/frontend@sha256:0000000000000000000000000000000000000000000000000000000000000003" ;;
    acb-worker) echo "ghcr.io/test/worker@sha256:0000000000000000000000000000000000000000000000000000000000000004" ;;
    acb-auth-browser) echo "ghcr.io/test/auth@sha256:0000000000000000000000000000000000000000000000000000000000000005" ;;
    acb-tts-gateway) echo "ghcr.io/test/tts@sha256:0000000000000000000000000000000000000000000000000000000000000006" ;;
    acb-bark) echo "ghcr.io/test/bark@sha256:0000000000000000000000000000000000000000000000000000000000000007" ;;
    *) echo "unknown" ;;
  esac
  exit 0
fi

if [[ "$*" == *"{{.State.Running}}"* ]]; then
  echo "true"
  exit 0
fi

if [[ "$*" == *"{{if .State.Health}}"* ]]; then
  echo "healthy"
  exit 0
fi

echo "mock"
exit 0
EOF
  chmod +x "$tdir/mock_bin/docker"

  cat <<'EOF' > "$tdir/mock_bin/systemctl"
#!/usr/bin/env bash
exit 0
EOF
  chmod +x "$tdir/mock_bin/systemctl"
}

test_clean_state_passes() {
  local tdir="$TEST_TMP/clean"
  setup_drift_env "$tdir"

  local ec=0
  PATH="$tdir/mock_bin:$PATH" \
  RUNTIME_ROOT="$tdir" \
  DEPLOY_PATH="$tdir" \
  ACB_CONFIG="$tdir/traefik/acb.yml" \
  ACTIVE_SLOT_FILE="$tdir/state/gateway-active-slot" \
  FRONTEND_ACTIVE_SLOT_FILE="$tdir/state/frontend-active-slot" \
  bash "$DEPLOY_DIR/verify-runtime-drift.sh" --state "$tdir/state/current-release.json" || ec=$?

  assert_eq "0" "$ec" "Matching runtime passes drift verification"
}

test_frontend_slot_drift() {
  local tdir="$TEST_TMP/fe_slot_drift"
  setup_drift_env "$tdir"
  # Frontend slot says green while state says blue
  printf 'green' > "$tdir/state/frontend-active-slot"

  local ec=0
  PATH="$tdir/mock_bin:$PATH" \
  RUNTIME_ROOT="$tdir" \
  DEPLOY_PATH="$tdir" \
  ACB_CONFIG="$tdir/traefik/acb.yml" \
  ACTIVE_SLOT_FILE="$tdir/state/gateway-active-slot" \
  FRONTEND_ACTIVE_SLOT_FILE="$tdir/state/frontend-active-slot" \
  bash "$DEPLOY_DIR/verify-runtime-drift.sh" --state "$tdir/state/current-release.json" 2>/dev/null || ec=$?

  assert_eq "1" "$ec" "Frontend slot mismatch detected as drift"
}

test_frontend_route_drift() {
  local tdir="$TEST_TMP/fe_route_drift"
  setup_drift_env "$tdir"
  # Frontend route points to green while state says blue
  cat <<'EOF' > "$tdir/traefik/acb.yml"
http:
  services:
    acb-service:
      loadBalancer:
        servers:
          - url: "http://acb-web-blue:8090"
    acb-frontend-service:
      loadBalancer:
        servers:
          - url: "http://acb-frontend-green:8080"
EOF

  local ec=0
  PATH="$tdir/mock_bin:$PATH" \
  RUNTIME_ROOT="$tdir" \
  DEPLOY_PATH="$tdir" \
  ACB_CONFIG="$tdir/traefik/acb.yml" \
  ACTIVE_SLOT_FILE="$tdir/state/gateway-active-slot" \
  FRONTEND_ACTIVE_SLOT_FILE="$tdir/state/frontend-active-slot" \
  bash "$DEPLOY_DIR/verify-runtime-drift.sh" --state "$tdir/state/current-release.json" 2>/dev/null || ec=$?

  assert_eq "1" "$ec" "Frontend route mismatch detected as drift"
}

test_gateway_ambiguous_route_drift() {
  local tdir="$TEST_TMP/gw_ambig_drift"
  setup_drift_env "$tdir"
  # Gateway route points to BOTH blue and green
  cat <<'EOF' > "$tdir/traefik/acb.yml"
http:
  services:
    acb-service:
      loadBalancer:
        servers:
          - url: "http://acb-web-blue:8090"
          - url: "http://acb-web-green:8090"
    acb-frontend-service:
      loadBalancer:
        servers:
          - url: "http://acb-frontend-blue:8080"
EOF

  local ec=0
  PATH="$tdir/mock_bin:$PATH" \
  RUNTIME_ROOT="$tdir" \
  DEPLOY_PATH="$tdir" \
  ACB_CONFIG="$tdir/traefik/acb.yml" \
  ACTIVE_SLOT_FILE="$tdir/state/gateway-active-slot" \
  FRONTEND_ACTIVE_SLOT_FILE="$tdir/state/frontend-active-slot" \
  bash "$DEPLOY_DIR/verify-runtime-drift.sh" --state "$tdir/state/current-release.json" 2>/dev/null || ec=$?

  assert_eq "1" "$ec" "Ambiguous gateway route (both blue and green) detected as drift"
}

test_clean_state_passes
test_frontend_slot_drift
test_frontend_route_drift
test_gateway_ambiguous_route_drift

echo "Passed: $TESTS_PASSED, Failed: $TESTS_FAILED"
[[ "$TESTS_FAILED" -eq 0 ]] || exit 1
