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
  mkdir -p "$tdir/state" "$tdir/traefik" "$tdir/mock_bin" "$tdir/failover/apps.d" "$tdir/systemd" "$tdir/data" "$tdir/compose" "$tdir/lib"

  for comp in base gateway frontend worker auth-browser tts bark dbtool; do
    echo "services: {$comp: {}}" > "$tdir/compose/${comp}.yaml"
  done
  echo "# traefik template" > "$tdir/lib/traefik.sh"
  echo "# platform template" > "$tdir/runtime-layout.sh"

  local comp_bundle_hash
  comp_bundle_hash="$(python3 - "$tdir" <<'PY'
import hashlib, pathlib, sys
root = pathlib.Path(sys.argv[1])
h = hashlib.sha256()
for p in sorted((root / "compose").glob("*.yaml")):
    rel = f"compose/{p.name}"
    h.update(rel.encode() + b"\0" + p.read_bytes() + b"\0")
print(h.hexdigest())
PY
)"
  local traefik_hash
  traefik_hash="$(sha256sum "$tdir/lib/traefik.sh" | awk '{print $1}')"
  local platform_hash
  platform_hash="$(sha256sum "$tdir/runtime-layout.sh" | awk '{print $1}')"

  echo '{"name":"acb"}' > "$tdir/failover/apps.d/acb.json"
  echo '{"name":"auth-browser"}' > "$tdir/failover/apps.d/auth-browser.json"
  echo '{"name":"worker"}' > "$tdir/failover/apps.d/worker.json"
  echo "# controller" > "$tdir/failover/vps-failover-controller.py"
  echo "[Unit]" > "$tdir/systemd/vps-failover-controller.service"
  echo "[Unit]" > "$tdir/systemd/vps-failover-reconcile.service"
  echo "[Timer]" > "$tdir/systemd/vps-failover-reconcile.timer"

  local fc_hash
  fc_hash="$(sha256sum "$tdir/failover/vps-failover-controller.py" | awk '{print $1}')"
  local fc_bundle_hash
  fc_bundle_hash="$(python3 - "$tdir/failover" "$tdir/failover/apps.d" "$tdir/systemd" <<'PY'
import hashlib, pathlib, sys
controller, registry, systemd = map(pathlib.Path, sys.argv[1:])
paths = [
    ("vps-failover-controller.py", controller / "vps-failover-controller.py"),
    ("vps-failover-controller.service", systemd / "vps-failover-controller.service"),
    ("vps-failover-reconcile.service", systemd / "vps-failover-reconcile.service"),
    ("vps-failover-reconcile.timer", systemd / "vps-failover-reconcile.timer"),
    ("apps.d/acb.json", registry / "acb.json"),
    ("apps.d/auth-browser.json", registry / "auth-browser.json"),
    ("apps.d/worker.json", registry / "worker.json"),
]
h = hashlib.sha256()
for name, path in paths:
    h.update(name.encode() + b"\0" + path.read_bytes() + b"\0")
print(h.hexdigest())
PY
)"

  # Canonical state v2
  cat <<EOF > "$tdir/state/current-release.json"
{
  "schema_version": 2,
  "generation": 10,
  "release_id": "rel-drift-test",
  "release_dir": "$tdir",
  "status": "COMPLETED",
  "git_sha": "1111111111111111111111111111111111111111",
  "active_slots": {
    "gateway": "blue",
    "frontend": "blue"
  },
  "config": {
    "compose_bundle_sha256": "$comp_bundle_hash",
    "traefik_template_sha256": "$traefik_hash",
    "platform_bundle_sha256": "$platform_hash"
  },
  "failover_controller": {
    "sha256": "$fc_hash",
    "bundle_sha256": "$fc_bundle_hash"
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

run_drift_check() {
  local tdir="$1"
  PATH="$tdir/mock_bin:$PATH" \
  RUNTIME_ROOT="$tdir" \
  DEPLOY_PATH="$tdir" \
  RUNTIME_DATA_DIR="$tdir/data" \
  TX_JOURNAL_FILE="${TX_JOURNAL_FILE:-$tdir/data/deploy-journal.json}" \
  FAILOVER_INSTALL_DIR="$tdir/failover" \
  FAILOVER_REGISTRY_DIR="$tdir/failover/apps.d" \
  FAILOVER_SYSTEMD_DIR="$tdir/systemd" \
  ACB_CONFIG="$tdir/traefik/acb.yml" \
  ACTIVE_SLOT_FILE="$tdir/state/gateway-active-slot" \
  FRONTEND_ACTIVE_SLOT_FILE="$tdir/state/frontend-active-slot" \
  bash "$DEPLOY_DIR/verify-runtime-drift.sh" --state "$tdir/state/current-release.json"
}

test_clean_state_passes() {
  local tdir="$TEST_TMP/clean"
  setup_drift_env "$tdir"

  local ec=0
  run_drift_check "$tdir" || ec=$?
  assert_eq "0" "$ec" "Matching runtime passes drift verification"
}

test_frontend_slot_drift() {
  local tdir="$TEST_TMP/fe_slot_drift"
  setup_drift_env "$tdir"
  printf 'green' > "$tdir/state/frontend-active-slot"

  local ec=0
  run_drift_check "$tdir" 2>/dev/null || ec=$?
  assert_eq "1" "$ec" "Frontend slot mismatch detected as drift"
}

test_frontend_route_drift() {
  local tdir="$TEST_TMP/fe_route_drift"
  setup_drift_env "$tdir"
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
  run_drift_check "$tdir" 2>/dev/null || ec=$?
  assert_eq "1" "$ec" "Frontend route mismatch detected as drift"
}

test_gateway_ambiguous_route_drift() {
  local tdir="$TEST_TMP/gw_ambig_drift"
  setup_drift_env "$tdir"
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
  run_drift_check "$tdir" 2>/dev/null || ec=$?
  assert_eq "1" "$ec" "Ambiguous gateway route (both blue and green) detected as drift"
}

test_tx_journal_path_drift() {
  local tdir="$TEST_TMP/journal_drift"
  setup_drift_env "$tdir"

  local ec=0
  TX_JOURNAL_FILE="$tdir/wrong_dir/deploy-journal.json" run_drift_check "$tdir" 2>/dev/null || ec=$?
  assert_eq "1" "$ec" "Wrong TX journal path detected as drift"
}

test_compose_bundle_drift() {
  local tdir="$TEST_TMP/compose_drift"
  setup_drift_env "$tdir"

  echo "# mutated" >> "$tdir/compose/gateway.yaml"
  local ec=0
  run_drift_check "$tdir" 2>/dev/null || ec=$?
  assert_eq "1" "$ec" "Mutated split compose bundle detected as drift"
}

test_traefik_template_drift() {
  local tdir="$TEST_TMP/traefik_drift"
  setup_drift_env "$tdir"

  echo "# mutated" >> "$tdir/lib/traefik.sh"
  local ec=0
  run_drift_check "$tdir" 2>/dev/null || ec=$?
  assert_eq "1" "$ec" "Mutated Traefik template detected as drift"
}

test_extra_failover_registry_drift() {
  local tdir="$TEST_TMP/registry_drift"
  setup_drift_env "$tdir"

  echo '{"name":"rogue"}' > "$tdir/failover/apps.d/rogue.json"
  local ec=0
  run_drift_check "$tdir" 2>/dev/null || ec=$?
  assert_eq "1" "$ec" "Extra failover registry file detected as drift"
}

test_systemd_status_drift() {
  local tdir="$TEST_TMP/systemd_drift"
  setup_drift_env "$tdir"

  cat <<'EOF' > "$tdir/mock_bin/systemctl"
#!/usr/bin/env bash
if [[ "$1" == "is-active" && "$*" == *"vps-failover-controller.service"* ]]; then
  exit 1
fi
exit 0
EOF
  chmod +x "$tdir/mock_bin/systemctl"

  local ec=0
  run_drift_check "$tdir" 2>/dev/null || ec=$?
  assert_eq "1" "$ec" "Inactive systemd unit detected as drift"
}

test_clean_state_passes
test_frontend_slot_drift
test_frontend_route_drift
test_gateway_ambiguous_route_drift
test_tx_journal_path_drift
test_compose_bundle_drift
test_traefik_template_drift
test_extra_failover_registry_drift
test_systemd_status_drift

echo "Passed: $TESTS_PASSED, Failed: $TESTS_FAILED"
[[ "$TESTS_FAILED" -eq 0 ]] || exit 1
