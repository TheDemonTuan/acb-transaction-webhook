#!/usr/bin/env bash
# deploy/tests/test_legacy_frontend_recovery.sh
# Regression test reproducing run #247:
# Interrupted rollout with frontend previous topology = legacy (acb-frontend)
# and candidate = blue (acb-frontend-blue).
set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR="$(cd -- "$SCRIPT_DIR/.." && pwd)"

TEST_TMP="$(mktemp -d "${TMPDIR:-/tmp}/acb-legacy-frontend-tests.XXXXXX")"
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

setup_fixture_247() {
  local tdir="$1"
  mkdir -p "$tdir/deploy" "$tdir/data" "$tdir/secrets" "$tdir/state" "$tdir/traefik" "$tdir/mock_bin"

  # Copy deploy scripts
  cp -r "$DEPLOY_DIR/lib"* "$tdir/deploy/"
  cp "$DEPLOY_DIR/release-env.sh" "$tdir/deploy/"
  cp "$DEPLOY_DIR/verify-manifest.sh" "$tdir/deploy/"
  cp "$DEPLOY_DIR/dispatch-rollout.sh" "$tdir/deploy/"
  cp "$DEPLOY_DIR/verify-runtime-drift.sh" "$tdir/deploy/"
  cp "$DEPLOY_DIR/deploy-frontend.sh" "$tdir/deploy/"
  cp "$DEPLOY_DIR/edge-probe.sh" "$tdir/deploy/"
  if [[ -f "$DEPLOY_DIR/runtime-layout.sh" ]]; then
    cp "$DEPLOY_DIR/runtime-layout.sh" "$tdir/deploy/"
  fi
  if [[ -f "$DEPLOY_DIR/reconcile-release.sh" ]]; then
    cp "$DEPLOY_DIR/reconcile-release.sh" "$tdir/deploy/"
  fi

  printf 'mock-master\n' > "$tdir/secrets/app_master_key"
  printf 'mock-worker\n' > "$tdir/secrets/worker_auth_token"
  printf 'admin\n' > "$tdir/secrets/bark_basic_auth_user"
  printf 'pass\n' > "$tdir/secrets/bark_basic_auth_password"

  # Mock docker and systemctl
  cat <<'EOF' > "$tdir/mock_bin/docker"
#!/usr/bin/env bash
cmd="${1:-}"
case "$cmd" in
  inspect)
    # Return healthy for legacy container acb-frontend, green gateway, etc.
    target="${@: -1}"
    if [[ "$target" == "acb-frontend" || "$target" == "acb-gateway-green" || "$target" == "acb-worker" ]]; then
      if [[ "$*" == *"{{.State.Health.Status}}"* || "$*" == *"{{if .State.Health}}"* ]]; then
        printf 'healthy\n'
        exit 0
      fi
      if [[ "$*" == *"{{.Config.Image}}"* ]]; then
        printf 'test-image@sha256:%s\n' "0000000000000000000000000000000000000000000000000000000000000001"
        exit 0
      fi
      printf 'container-id-%s\n' "$target"
      exit 0
    elif [[ "$target" == "acb-frontend-blue" || "$target" == "acb-gateway-blue" ]]; then
      if [[ "$*" == *"{{.State.Health.Status}}"* || "$*" == *"{{if .State.Health}}"* ]]; then
        printf 'healthy\n'
        exit 0
      fi
      printf 'container-id-%s\n' "$target"
      exit 0
    fi
    printf 'mock-id\n'
    exit 0
    ;;
  stop|rm)
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

  cat <<'EOF' > "$tdir/mock_bin/curl"
#!/usr/bin/env bash
exit 0
EOF
  chmod +x "$tdir/mock_bin/curl"

  # Active routes / slots
  # Last successful gateway was green
  printf 'green' > "$tdir/state/gateway-active-slot"
  # Candidate frontend was blue, so frontend-active-slot currently blue
  printf 'blue' > "$tdir/state/frontend-active-slot"

  # Canonical state v1 pointing to previous release
  cat <<EOF > "$tdir/state/current-release.json"
{
  "schema_version": 1,
  "generation": 41,
  "release_id": "rel-prev-246",
  "git_sha": "1111111111111111111111111111111111111111",
  "manifest_sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "status": "COMPLETED",
  "active_slots": {
    "gateway": "green",
    "frontend": null
  },
  "images": {
    "gateway": {
      "blue": "ghcr.io/test/gateway@sha256:0000000000000000000000000000000000000000000000000000000000000001",
      "green": "ghcr.io/test/gateway@sha256:0000000000000000000000000000000000000000000000000000000000000001"
    },
    "frontend": "ghcr.io/test/frontend@sha256:0000000000000000000000000000000000000000000000000000000000000001",
    "worker": "ghcr.io/test/worker@sha256:0000000000000000000000000000000000000000000000000000000000000001",
    "dbtool": "ghcr.io/test/dbtool@sha256:0000000000000000000000000000000000000000000000000000000000000001",
    "auth_browser": "ghcr.io/test/auth@sha256:0000000000000000000000000000000000000000000000000000000000000001",
    "tts": "ghcr.io/test/tts@sha256:0000000000000000000000000000000000000000000000000000000000000001",
    "bark": "ghcr.io/test/bark@sha256:0000000000000000000000000000000000000000000000000000000000000001"
  }
}
EOF

  # Pending retire evidence matching run #247
  printf 'old_slot=green\ncandidate_slot=blue\n' > "$tdir/data/pending-gateway-retire.env"
  printf 'old_slot=legacy\ncandidate_slot=blue\n' > "$tdir/data/pending-frontend-retire.env"

  # Rollout journal INTERRUPTED with completed steps
  cat <<EOF > "$tdir/data/rollout-journal.json"
{
  "release_id": "rel-candidate-247",
  "git_sha": "2222222222222222222222222222222222222222",
  "status": "INTERRUPTED",
  "started_at": "2026-09-16T00:00:00Z",
  "steps": {
    "schema": "STEP_COMPLETED",
    "auth_browser": "STEP_COMPLETED",
    "tts": "STEP_COMPLETED",
    "bark": "STEP_COMPLETED",
    "frontend": "STEP_COMPLETED",
    "worker": "STEP_COMPLETED",
    "gateway": "STEP_COMPLETED",
    "failover_controller": "STEP_COMPLETED"
  }
}
EOF

  # Traefik routing pointing to candidate blue frontend & blue gateway
  cat <<EOF > "$tdir/traefik/acb.yml"
http:
  services:
    acb-frontend-service:
      loadBalancer:
        servers:
          - url: "http://acb-frontend-blue:8080"
    acb-service:
      loadBalancer:
        servers:
          - url: "http://acb-web-blue:8090"
EOF
}

test_reconcile_run_247() {
  local tdir="$TEST_TMP/run_247"
  setup_fixture_247 "$tdir"

  local log="$tdir/reconcile.log"
  local exit_code=0
  PATH="$tdir/mock_bin:$PATH" \
  RUNTIME_ROOT="$tdir" \
  DEPLOY_PATH="$tdir" \
  TRAEFIK_DYNAMIC_DIR="$tdir/traefik" \
  ACB_CONFIG="$tdir/traefik/acb.yml" \
  SKIP_MANIFEST_CHECK=1 \
  RUNTIME_DRIFT_CHECK_CMD="true" \
  EDGE_PROBE_SCRIPT="$tdir/mock_bin/curl" \
  bash "$tdir/deploy/reconcile-release.sh" --runtime-root "$tdir" --recovery-only >"$log" 2>&1 || exit_code=$?

  echo "Reconcile exit code: $exit_code"
  cat "$log"
  assert_eq "0" "$exit_code" "Reconcile interrupted rollout succeeds"
  assert_eq "green" "$(cat "$tdir/state/gateway-active-slot")" "Gateway active slot restored to green"
  [[ ! -f "$tdir/state/frontend-active-slot" ]] && printf 'PASS: frontend-active-slot removed (legacy)\n' || {
    printf 'FAIL: frontend-active-slot should be removed for legacy topology\n' >&2
    TESTS_FAILED=$(( TESTS_FAILED + 1 ))
  }
  [[ ! -f "$tdir/data/pending-gateway-retire.env" ]] && printf 'PASS: pending-gateway-retire removed\n' || {
    printf 'FAIL: pending-gateway-retire still exists\n' >&2
    TESTS_FAILED=$(( TESTS_FAILED + 1 ))
  }
  [[ ! -f "$tdir/data/pending-frontend-retire.env" ]] && printf 'PASS: pending-frontend-retire removed\n' || {
    printf 'FAIL: pending-frontend-retire still exists\n' >&2
    TESTS_FAILED=$(( TESTS_FAILED + 1 ))
  }
  grep -q "http://acb-frontend:8080" "$tdir/traefik/acb.yml" && printf 'PASS: Traefik route points to acb-frontend\n' || {
    printf 'FAIL: Traefik route does not point to acb-frontend\n' >&2
    TESTS_FAILED=$(( TESTS_FAILED + 1 ))
  }
}

test_reconcile_run_247

echo "Passed: $TESTS_PASSED, Failed: $TESTS_FAILED"
[[ "$TESTS_FAILED" -eq 0 ]] || exit 1
