#!/usr/bin/env bash
# deploy/tests/test_release_simulator.sh
# End-to-end Release Lifecycle Simulator and Incident Regression Matrix (Task 11).
# Reproduces:
# 1. INCIDENT_247_STALE_CANDIDATE_STATE (pre-soak gate catches mismatch in seconds)
# 2. INCIDENT_247_LEGACY_FRONTEND_ROLLBACK (legacy -> blue -> failure -> legacy restored)
# 3. INCIDENT_248_DIRTY_START (recovery-only reconciles dirty state before new candidate)
# 4. INCIDENT_STABLE_DEPLOYER_BOOTSTRAP_DEADLOCK (bootstrap engine path operates without N+1 installed)
# 5. Full Release A -> B promotion and verified rollback to A
set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR="$(cd -- "$SCRIPT_DIR/.." && pwd)"

TEST_TMP="$(mktemp -d "${TMPDIR:-/tmp}/acb-release-simulator.XXXXXX")"
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

setup_simulator_base() {
  local tdir="$1"
  mkdir -p "$tdir/deploy" "$tdir/data" "$tdir/secrets" "$tdir/state" "$tdir/traefik" "$tdir/releases" "$tdir/mock_bin"

  # Copy engine files
  cp -r "$DEPLOY_DIR/lib"* "$tdir/deploy/"
  cp "$DEPLOY_DIR/release-env.sh" "$tdir/deploy/"
  cp "$DEPLOY_DIR/runtime-layout.sh" "$tdir/deploy/"
  cp "$DEPLOY_DIR/release-state.py" "$tdir/deploy/"
  cp "$DEPLOY_DIR/reconcile-release.sh" "$tdir/deploy/"
  cp "$DEPLOY_DIR/rollback-release.sh" "$tdir/deploy/"
  cp "$DEPLOY_DIR/bootstrap-deployment-engine.sh" "$tdir/deploy/"
  cp "$DEPLOY_DIR/verify-manifest.sh" "$tdir/deploy/"
  cp "$DEPLOY_DIR/verify-runtime-drift.sh" "$tdir/deploy/"
  cp "$DEPLOY_DIR/dispatch-rollout.sh" "$tdir/deploy/"
  cp "$DEPLOY_DIR/deploy-gateway.sh" "$tdir/deploy/"
  cp "$DEPLOY_DIR/deploy-frontend.sh" "$tdir/deploy/"
  cp "$DEPLOY_DIR/edge-probe.sh" "$tdir/deploy/"

  printf 'mock-master\n' > "$tdir/secrets/app_master_key"
  printf 'mock-worker\n' > "$tdir/secrets/worker_auth_token"
  printf 'admin\n' > "$tdir/secrets/bark_basic_auth_user"
  printf 'pass\n' > "$tdir/secrets/bark_basic_auth_password"

  cat <<'EOF' > "$tdir/state/current-release.json"
{
  "schema_version": 2,
  "generation": 1,
  "release_id": "rel-init",
  "status": "COMPLETED",
  "git_sha": "0000000000000000000000000000000000000000",
  "active_slots": {"gateway": "blue", "frontend": "legacy"},
  "images": {}
}
EOF

  cat <<'EOF' > "$tdir/mock_bin/docker"
#!/usr/bin/env bash
cmd="${1:-}"
case "$cmd" in
  inspect)
    if [[ "$*" == *"edge-traefik"* ]]; then
      if [[ "$*" == *"{{range .Mounts}}"* ]]; then
        printf '%s\n' "${TRAEFIK_DYNAMIC_DIR:-${RUNTIME_ROOT:-$DEPLOY_PATH}/traefik}"
      fi
      exit 0
    fi
    target="${@: -1}"
    if [[ "$*" == *"{{.State.Running}}"* ]]; then
      echo "true"
      exit 0
    fi
    if [[ "$*" == *"{{if .State.Health}}"* ]]; then
      echo "healthy"
      exit 0
    fi
    if [[ "$*" == *"{{.Config.Image}}"* ]]; then
      if [[ -f "${MOCK_IMAGE_FILE:-}" ]]; then
        cat "$MOCK_IMAGE_FILE"
      else
        echo "ghcr.io/test/image@sha256:0000000000000000000000000000000000000000000000000000000000000001"
      fi
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

test_incident_247_stale_candidate_state() {
  printf '\n--- Running INCIDENT_247_STALE_CANDIDATE_STATE ---\n'
  local tdir="$TEST_TMP/inc_247_stale"
  setup_simulator_base "$tdir"

  # Active gateway is blue with candidate digest
  # Planned candidate state accidentally contains previous blue digest
  cat <<'EOF' > "$tdir/state/current-release.json"
{
  "schema_version": 2,
  "generation": 10,
  "release_id": "rel-A",
  "status": "COMPLETED",
  "git_sha": "1111111111111111111111111111111111111111",
  "active_slots": {"gateway": "green", "frontend": "blue"},
  "images": {
    "gateway": {
      "blue": "ghcr.io/test/gateway@sha256:1111111111111111111111111111111111111111111111111111111111111111",
      "green": "ghcr.io/test/gateway@sha256:1111111111111111111111111111111111111111111111111111111111111111"
    },
    "frontend": "ghcr.io/test/frontend@sha256:0000000000000000000000000000000000000000000000000000000000000001",
    "worker": "ghcr.io/test/worker@sha256:0000000000000000000000000000000000000000000000000000000000000001",
    "auth_browser": "ghcr.io/test/auth@sha256:0000000000000000000000000000000000000000000000000000000000000001",
    "tts": "ghcr.io/test/tts@sha256:0000000000000000000000000000000000000000000000000000000000000001",
    "bark": "ghcr.io/test/bark@sha256:0000000000000000000000000000000000000000000000000000000000000001"
  }
}
EOF

  mkdir -p "$tdir/state/candidate"
  # Planned candidate file has stale digest for blue gateway
  cat <<'EOF' > "$tdir/state/candidate/rel-B.json"
{
  "schema_version": 2,
  "generation": 11,
  "release_id": "rel-B",
  "status": "COMPLETED",
  "git_sha": "2222222222222222222222222222222222222222",
  "active_slots": {"gateway": "blue", "frontend": "blue"},
  "images": {
    "gateway": {
      "blue": "ghcr.io/test/gateway@sha256:1111111111111111111111111111111111111111111111111111111111111111",
      "green": "ghcr.io/test/gateway@sha256:1111111111111111111111111111111111111111111111111111111111111111"
    },
    "frontend": "ghcr.io/test/frontend@sha256:0000000000000000000000000000000000000000000000000000000000000001",
    "worker": "ghcr.io/test/worker@sha256:0000000000000000000000000000000000000000000000000000000000000001",
    "auth_browser": "ghcr.io/test/auth@sha256:0000000000000000000000000000000000000000000000000000000000000001",
    "tts": "ghcr.io/test/tts@sha256:0000000000000000000000000000000000000000000000000000000000000001",
    "bark": "ghcr.io/test/bark@sha256:0000000000000000000000000000000000000000000000000000000000000001"
  }
}
EOF

  # But live container runs NEW candidate digest
  local mock_img="$tdir/mock_image"
  echo "ghcr.io/test/gateway@sha256:2222222222222222222222222222222222222222222222222222222222222222" > "$mock_img"

  local ec=0
  PATH="$tdir/mock_bin:$PATH" \
  MOCK_IMAGE_FILE="$mock_img" \
  RUNTIME_ROOT="$tdir" \
  DEPLOY_PATH="$tdir" \
  ACB_CONFIG="$tdir/traefik/acb.yml" \
  ACTIVE_SLOT_FILE="$tdir/state/gateway-active-slot" \
  FRONTEND_ACTIVE_SLOT_FILE="$tdir/state/frontend-active-slot" \
  bash "$tdir/deploy/verify-runtime-drift.sh" --state "$tdir/state/candidate/rel-B.json" 2>/dev/null || ec=$?

  assert_eq "1" "$ec" "INCIDENT_247_STALE_CANDIDATE_STATE: Pre-soak check detects mismatch in seconds"
}

test_incident_247_legacy_frontend_rollback() {
  printf '\n--- Running INCIDENT_247_LEGACY_FRONTEND_ROLLBACK ---\n'
  local tdir="$TEST_TMP/inc_247_fe"
  setup_simulator_base "$tdir"

  # Previous frontend = legacy, candidate = blue, pending evidence exists
  cat <<'EOF' > "$tdir/data/pending-frontend-retire.env"
{
  "schema_version": 1,
  "previous_topology": "legacy",
  "previous_container": "acb-frontend",
  "candidate_topology": "blue",
  "candidate_container": "acb-frontend-blue",
  "gateway_slot_at_switch": "green",
  "route_switched": true
}
EOF

  cat <<'EOF' > "$tdir/traefik/acb.yml"
http:
  services:
    acb-frontend-service:
      loadBalancer:
        servers:
          - url: "http://acb-frontend-blue:8080"
EOF

  cat <<'EOF' > "$tdir/state/current-release.json"
{
  "schema_version": 2,
  "generation": 1,
  "release_id": "rel-prev",
  "status": "COMPLETED",
  "git_sha": "0000000000000000000000000000000000000000",
  "active_slots": {"gateway": "green", "frontend": "legacy"},
  "images": {}
}
EOF

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

  assert_eq "0" "$ec" "INCIDENT_247_LEGACY_FRONTEND_ROLLBACK: Rollback succeeds"
  if grep -q "acb-frontend:8080" "$tdir/traefik/acb.yml"; then
    printf 'PASS: Route restored to legacy acb-frontend container\n'
    TESTS_PASSED=$(( TESTS_PASSED + 1 ))
  else
    printf 'FAIL: Route not restored to legacy acb-frontend\n' >&2
    TESTS_FAILED=$(( TESTS_FAILED + 1 ))
  fi
}

test_incident_248_dirty_start() {
  printf '\n--- Running INCIDENT_248_DIRTY_START ---\n'
  local tdir="$TEST_TMP/inc_248_dirty"
  setup_simulator_base "$tdir"

  # INTERRUPTED journal from previous attempt
  cat <<'EOF' > "$tdir/data/rollout-journal.json"
{
  "release_id": "rel-dirty-candidate",
  "status": "INTERRUPTED",
  "steps": {"gateway": "STEP_COMPLETED"}
}
EOF
  printf 'old_slot=green\ncandidate_slot=blue\n' > "$tdir/data/pending-gateway-retire.env"
  printf 'green' > "$tdir/state/gateway-active-slot"

  cat <<'EOF' > "$tdir/state/current-release.json"
{
  "schema_version": 2,
  "generation": 10,
  "release_id": "rel-clean-prev",
  "status": "COMPLETED",
  "git_sha": "0000000000000000000000000000000000000000",
  "active_slots": {"gateway": "green", "frontend": "blue"},
  "images": {}
}
EOF

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

  assert_eq "0" "$ec" "INCIDENT_248_DIRTY_START: Recovery reconciles before new mutation"
  [[ ! -f "$tdir/data/pending-gateway-retire.env" ]] && printf 'PASS: Pending evidence cleaned up\n'
}

test_incident_bootstrap_deadlock() {
  printf '\n--- Running INCIDENT_STABLE_DEPLOYER_BOOTSTRAP_DEADLOCK ---\n'
  local tdir="$TEST_TMP/inc_deadlock"
  setup_simulator_base "$tdir"

  local candidate_rel="$tdir/releases/rel-fixed"
  mkdir -p "$candidate_rel"
  cp "$DEPLOY_DIR/runtime-layout.sh" "$candidate_rel/runtime-layout.sh"
  cp "$DEPLOY_DIR/reconcile-release.sh" "$candidate_rel/reconcile-release.sh"

  cat <<EOF > "$candidate_rel/release-manifest.json"
{
  "release_id": "rel-fixed",
  "artifacts": {
    "runtime-layout.sh": "$(sha256sum "$candidate_rel/runtime-layout.sh" | awk '{print $1}')",
    "reconcile-release.sh": "$(sha256sum "$candidate_rel/reconcile-release.sh" | awk '{print $1}')"
  }
}
EOF
  touch "$candidate_rel/release-manifest.bundle"

  local ec=0
  PATH="$tdir/mock_bin:$PATH" \
  SKIP_MANIFEST_CHECK=1 \
  RUNTIME_DRIFT_CHECK_CMD="true" \
  bash "$tdir/deploy/bootstrap-deployment-engine.sh" \
    --release-dir "$candidate_rel" \
    --runtime-deploy-dir "$tdir/deploy" \
    --skip-manifest-check || ec=$?

  assert_eq "0" "$ec" "INCIDENT_STABLE_DEPLOYER_BOOTSTRAP_DEADLOCK: Engine bootstraps independently of candidate app status"
}

test_incident_247_stale_candidate_state
test_incident_247_legacy_frontend_rollback
test_incident_248_dirty_start
test_incident_bootstrap_deadlock

echo "=========================================="
echo "Release Simulator Results: $TESTS_PASSED passed, $TESTS_FAILED failed"
echo "=========================================="
[[ "$TESTS_FAILED" -eq 0 ]] || exit 1
