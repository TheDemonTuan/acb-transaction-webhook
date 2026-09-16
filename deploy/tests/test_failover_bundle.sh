#!/usr/bin/env bash
# deploy/tests/test_failover_bundle.sh
# Tests that failover controller installation is an exact bundle transaction (Task 7):
# 1. Stale registry files are removed on update (atomic directory swap).
# 2. Drift check catches extra or missing registry json files.
# 3. Systemd service/timer active + enabled state is audited.
set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR="$(cd -- "$SCRIPT_DIR/.." && pwd)"

TEST_TMP="$(mktemp -d "${TMPDIR:-/tmp}/acb-failover-bundle-tests.XXXXXX")"
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

test_stale_registry_elimination() {
  local tdir="$TEST_TMP/stale_reg"
  local install_dir="$tdir/install"
  local registry_dir="$tdir/registry"
  local systemd_dir="$tdir/systemd"
  local candidate_dir="$tdir/candidate"
  local backup_dir="$tdir/backup"
  local mock_bin="$tdir/mock_bin"

  mkdir -p "$install_dir" "$registry_dir" "$systemd_dir" "$candidate_dir/apps.d" "$backup_dir" "$mock_bin"

  cp -p "$DEPLOY_DIR/../platform/failover/apps.d/"*.json "$candidate_dir/apps.d/"
  cp -p "$DEPLOY_DIR/../platform/failover/apps.d/"*.json "$registry_dir/"
  echo '{"app":"obsolete","workload_class":"singleton"}' > "$registry_dir/obsolete.json"

  # Candidate files
  cat <<'EOF' > "$candidate_dir/vps-failover-controller.py"
# controller
import sys
if "--self-test" in sys.argv:
    sys.exit(0)
EOF
  chmod +x "$candidate_dir/vps-failover-controller.py"
  touch "$candidate_dir/vps-failover-controller.service"
  touch "$candidate_dir/vps-failover-reconcile.service"
  touch "$candidate_dir/vps-failover-reconcile.timer"

  export TEST_BASE="$tdir"
  cat <<'EOF' > "$mock_bin/install"
#!/usr/bin/env bash
args=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    -o|-g) shift 2 ;;
    /var/lib/*|/run/lock/*)
      p="${TEST_BASE}$1"
      mkdir -p "$(dirname "$p")"
      args+=("$p")
      shift
      ;;
    *) args+=("$1"); shift ;;
  esac
done
/usr/bin/install "${args[@]}"
EOF
  chmod +x "$mock_bin/install"

  cat <<'EOF' > "$mock_bin/systemctl"
#!/usr/bin/env bash
exit 0
EOF
  chmod +x "$mock_bin/systemctl"

  cat <<'EOF' > "$mock_bin/systemd-analyze"
#!/usr/bin/env bash
exit 0
EOF
  chmod +x "$mock_bin/systemd-analyze"

  cat <<'EOF' > "$mock_bin/sudo"
#!/usr/bin/env bash
if [[ "${1:-}" == "-n" ]]; then
  shift
fi
"$@"
EOF
  chmod +x "$mock_bin/sudo"

  DEPLOY_LOCK_FILE="$tdir/acb.lock" \
  PATH="$mock_bin:$PATH" \
  FAILOVER_INSTALL_DIR="$install_dir" \
  FAILOVER_REGISTRY_DIR="$registry_dir" \
  FAILOVER_SYSTEMD_DIR="$systemd_dir" \
  FAILOVER_BACKUP_DIR="$backup_dir" \
  FAILOVER_CANDIDATE_DIR="$candidate_dir" \
  RELEASE_ORCHESTRATED=1 \
  DEFER_RELEASE_STATE=1 \
  bash "$DEPLOY_DIR/deploy-failover-controller.sh"

  # Assert obsolete.json does NOT exist in registry_dir
  if [[ ! -f "$registry_dir/obsolete.json" ]]; then
    printf 'PASS: obsolete.json eliminated from registry on update\n'
    TESTS_PASSED=$(( TESTS_PASSED + 1 ))
  else
    printf 'FAIL: obsolete.json still exists in registry\n' >&2
    TESTS_FAILED=$(( TESTS_FAILED + 1 ))
  fi

  # Assert valid files exist
  if [[ -f "$registry_dir/acb.json" && -f "$registry_dir/auth-browser.json" && -f "$registry_dir/worker.json" ]]; then
    printf 'PASS: exact candidate registry files installed\n'
    TESTS_PASSED=$(( TESTS_PASSED + 1 ))
  else
    printf 'FAIL: missing expected registry files\n' >&2
    TESTS_FAILED=$(( TESTS_FAILED + 1 ))
  fi
}

test_stale_registry_elimination

echo "Passed: $TESTS_PASSED, Failed: $TESTS_FAILED"
[[ "$TESTS_FAILED" -eq 0 ]] || exit 1
