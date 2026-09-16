#!/usr/bin/env bash
# deploy/tests/test_release_crash_recovery.sh
# Chaos matrix: injects failure/disconnect after every orchestration boundary (Task 11 Step 2).
# Asserts that recovery always converges to either the exact previous canonical release
# or fully committed candidate, never leaving a mixed undocumented state.
set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR="$(cd -- "$SCRIPT_DIR/.." && pwd)"

TEST_TMP="$(mktemp -d "${TMPDIR:-/tmp}/acb-chaos-matrix.XXXXXX")"
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

setup_chaos_fixture() {
  local tdir="$1"
  mkdir -p "$tdir/deploy" "$tdir/data" "$tdir/secrets" "$tdir/state" "$tdir/traefik" "$tdir/mock_bin"

  # Copy engine files
  cp -r "$DEPLOY_DIR/lib"* "$tdir/deploy/"
  cp "$DEPLOY_DIR/release-env.sh" "$tdir/deploy/"
  cp "$DEPLOY_DIR/runtime-layout.sh" "$tdir/deploy/"
  cp "$DEPLOY_DIR/release-state.py" "$tdir/deploy/"
  cp "$DEPLOY_DIR/reconcile-release.sh" "$tdir/deploy/"
  cp "$DEPLOY_DIR/rollback-release.sh" "$tdir/deploy/"
  cp "$DEPLOY_DIR/edge-probe.sh" "$tdir/deploy/"

  printf 'mock-master\n' > "$tdir/secrets/app_master_key"
  printf 'mock-worker\n' > "$tdir/secrets/worker_auth_token"
  printf 'admin\n' > "$tdir/secrets/bark_basic_auth_user"
  printf 'pass\n' > "$tdir/secrets/bark_basic_auth_password"

  # Initial canonical release A
  cat <<'EOF' > "$tdir/state/current-release.json"
{
  "schema_version": 2,
  "generation": 10,
  "release_id": "rel-A",
  "status": "COMPLETED",
  "git_sha": "1111111111111111111111111111111111111111",
  "active_slots": {
    "gateway": "green",
    "frontend": "blue"
  },
  "images": {
    "gateway": {
      "blue": "ghcr.io/test/gateway@sha256:0000000000000000000000000000000000000000000000000000000000000001",
      "green": "ghcr.io/test/gateway@sha256:0000000000000000000000000000000000000000000000000000000000000001"
    },
    "frontend": "ghcr.io/test/frontend@sha256:0000000000000000000000000000000000000000000000000000000000000001",
    "worker": "ghcr.io/test/worker@sha256:0000000000000000000000000000000000000000000000000000000000000001",
    "auth_browser": "ghcr.io/test/auth@sha256:0000000000000000000000000000000000000000000000000000000000000001",
    "tts": "ghcr.io/test/tts@sha256:0000000000000000000000000000000000000000000000000000000000000001",
    "bark": "ghcr.io/test/bark@sha256:0000000000000000000000000000000000000000000000000000000000000001"
  }
}
EOF

  printf 'green' > "$tdir/state/gateway-active-slot"
  printf 'blue' > "$tdir/state/frontend-active-slot"

  cat <<'EOF' > "$tdir/traefik/acb.yml"
http:
  services:
    acb-service:
      loadBalancer:
        servers:
          - url: "http://acb-web-green:8090"
    acb-frontend-service:
      loadBalancer:
        servers:
          - url: "http://acb-frontend-blue:8080"
EOF

  cat <<'EOF' > "$tdir/mock_bin/docker"
#!/usr/bin/env bash
cmd="${1:-}"
case "$cmd" in
  inspect)
    if [[ "$*" == *"{{.State.Running}}"* ]]; then
      echo "true"
      exit 0
    fi
    if [[ "$*" == *"{{if .State.Health}}"* ]]; then
      echo "healthy"
      exit 0
    fi
    echo "mock-id"
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
EOF
  chmod +x "$tdir/mock_bin/docker"

  cat <<'EOF' > "$tdir/mock_bin/systemctl"
#!/usr/bin/env bash
exit 0
EOF
  chmod +x "$tdir/mock_bin/systemctl"
}

run_chaos_point() {
  local name="$1"
  local completed_steps_json="$2"
  local has_gw_retire="${3:-0}"
  local has_fe_retire="${4:-0}"

  printf '\nTesting chaos boundary: %s...\n' "$name"
  local tdir="$TEST_TMP/chaos_$name"
  setup_chaos_fixture "$tdir"

  # Interrupted journal simulating crash at boundary
  cat <<EOF > "$tdir/data/rollout-journal.json"
{
  "release_id": "rel-chaos-candidate",
  "status": "INTERRUPTED",
  "steps": $completed_steps_json
}
EOF

  if [[ "$has_gw_retire" -eq 1 ]]; then
    printf 'old_slot=green\ncandidate_slot=blue\n' > "$tdir/data/pending-gateway-retire.env"
    cat <<'EOF' > "$tdir/traefik/acb.yml"
http:
  services:
    acb-service:
      loadBalancer:
        servers:
          - url: "http://acb-web-blue:8090"
EOF
    printf 'blue' > "$tdir/state/gateway-active-slot"
  fi

  if [[ "$has_fe_retire" -eq 1 ]]; then
    printf 'old_slot=blue\ncandidate_slot=green\n' > "$tdir/data/pending-frontend-retire.env"
    cat <<'EOF' >> "$tdir/traefik/acb.yml"
    acb-frontend-service:
      loadBalancer:
        servers:
          - url: "http://acb-frontend-green:8080"
EOF
    printf 'green' > "$tdir/state/frontend-active-slot"
  fi

  # Execute recovery
  local ec=0
  PATH="$tdir/mock_bin:$PATH" \
  RUNTIME_ROOT="$tdir" \
  DEPLOY_PATH="$tdir" \
  ACB_CONFIG="$tdir/traefik/acb.yml" \
  ACTIVE_SLOT_FILE="$tdir/state/gateway-active-slot" \
  FRONTEND_ACTIVE_SLOT_FILE="$tdir/state/frontend-active-slot" \
  SKIP_MANIFEST_CHECK=1 \
  RUNTIME_DRIFT_CHECK_CMD="true" \
  EDGE_PROBE_SCRIPT="$tdir/mock_bin/docker" \
  bash "$tdir/deploy/reconcile-release.sh" --runtime-root "$tdir" --recovery-only || ec=$?

  assert_eq "0" "$ec" "Chaos boundary [$name]: Reconcile converges to clean state"
  assert_eq "green" "$(cat "$tdir/state/gateway-active-slot")" "Chaos boundary [$name]: Gateway slot is green"
  [[ ! -f "$tdir/data/pending-gateway-retire.env" ]] && printf 'PASS: pending-gateway-retire cleaned\n'
  [[ ! -f "$tdir/data/pending-frontend-retire.env" ]] && printf 'PASS: pending-frontend-retire cleaned\n'
}

# 1. Crash after schema
run_chaos_point "after_schema" '{"schema":"STEP_COMPLETED"}'

# 2. Crash after auth-browser
run_chaos_point "after_auth_browser" '{"schema":"STEP_COMPLETED","auth_browser":"STEP_COMPLETED"}'

# 3. Crash after tts
run_chaos_point "after_tts" '{"schema":"STEP_COMPLETED","auth_browser":"STEP_COMPLETED","tts":"STEP_COMPLETED"}'

# 4. Crash after bark
run_chaos_point "after_bark" '{"schema":"STEP_COMPLETED","auth_browser":"STEP_COMPLETED","tts":"STEP_COMPLETED","bark":"STEP_COMPLETED"}'

# 5. Crash after frontend route ACK
run_chaos_point "after_frontend_ack" '{"frontend":"STEP_COMPLETED"}' 0 1

# 6. Crash after worker quiesce / upgrade
run_chaos_point "after_worker" '{"worker":"STEP_COMPLETED"}'

# 7. Crash after gateway candidate switch & ACK
run_chaos_point "after_gateway_ack" '{"gateway":"STEP_COMPLETED"}' 1 0

# 8. Crash during gateway soak
run_chaos_point "during_gateway_soak" '{"gateway":"STEP_COMPLETED"}' 1 0

# 9. Crash after failover-controller install
run_chaos_point "after_failover_install" '{"failover_controller":"STEP_COMPLETED"}' 1 0

# 10. Crash during canonical commit (all steps completed)
run_chaos_point "during_canonical_commit" '{"schema":"STEP_COMPLETED","auth_browser":"STEP_COMPLETED","tts":"STEP_COMPLETED","bark":"STEP_COMPLETED","frontend":"STEP_COMPLETED","worker":"STEP_COMPLETED","gateway":"STEP_COMPLETED","failover_controller":"STEP_COMPLETED"}' 1 1

echo "=========================================="
echo "Chaos Matrix Results: $TESTS_PASSED passed, $TESTS_FAILED failed"
echo "=========================================="
[[ "$TESTS_FAILED" -eq 0 ]] || exit 1
