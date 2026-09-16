#!/usr/bin/env bash
# deploy/tests/test_bootstrap_recovery.sh
# Tests for signed deployment-engine bootstrap path (Task 0 Step 6).
set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR="$(cd -- "$SCRIPT_DIR/.." && pwd)"

TEST_TMP="$(mktemp -d "${TMPDIR:-/tmp}/acb-bootstrap-tests.XXXXXX")"
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

test_bootstrap_engine_success() {
  local tdir="$TEST_TMP/bootstrap_success"
  mkdir -p "$tdir/releases/rel-1" "$tdir/deploy" "$tdir/data" "$tdir/state"

  # Older version in deploy dir
  cat <<'EOF' > "$tdir/deploy/runtime-layout.sh"
# old version
OLD_MARKER=1
EOF

  # Candidate version in releases dir
  cp "$DEPLOY_DIR/runtime-layout.sh" "$tdir/releases/rel-1/runtime-layout.sh"
  cp "$DEPLOY_DIR/reconcile-release.sh" "$tdir/releases/rel-1/reconcile-release.sh"
  cp "$DEPLOY_DIR/bootstrap-deployment-engine.sh" "$tdir/deploy/bootstrap-deployment-engine.sh"
  cp "$DEPLOY_DIR/release-env.sh" "$tdir/deploy/release-env.sh"
  cp -r "$DEPLOY_DIR/lib"* "$tdir/deploy/"

  # Create manifest with artifact hashes
  local hash_layout hash_reconcile
  hash_layout="$(sha256sum "$tdir/releases/rel-1/runtime-layout.sh" | awk '{print $1}')"
  hash_reconcile="$(sha256sum "$tdir/releases/rel-1/reconcile-release.sh" | awk '{print $1}')"

  cat <<EOF > "$tdir/releases/rel-1/release-manifest.json"
{
  "release_id": "rel-1",
  "artifacts": {
    "runtime-layout.sh": "$hash_layout",
    "reconcile-release.sh": "$hash_reconcile"
  }
}
EOF
  touch "$tdir/releases/rel-1/release-manifest.bundle"

  # Current canonical state
  cat <<EOF > "$tdir/state/current-release.json"
{
  "schema_version": 1,
  "generation": 1,
  "status": "COMPLETED",
  "git_sha": "0000000000000000000000000000000000000000"
}
EOF

  # Run bootstrap
  bash "$tdir/deploy/bootstrap-deployment-engine.sh" \
    --release-dir "$tdir/releases/rel-1" \
    --runtime-deploy-dir "$tdir/deploy" \
    --skip-manifest-check

  # Verify updated file
  if grep -q "RUNTIME_ROOT=" "$tdir/deploy/runtime-layout.sh"; then
    printf 'PASS: runtime-layout.sh upgraded successfully\n'
    TESTS_PASSED=$(( TESTS_PASSED + 1 ))
  else
    printf 'FAIL: runtime-layout.sh was not upgraded\n' >&2
    TESTS_FAILED=$(( TESTS_FAILED + 1 ))
  fi

  # Verify previous file retained
  if [[ -f "$tdir/deploy/.runtime-layout.sh.previous" ]]; then
    printf 'PASS: .runtime-layout.sh.previous retained\n'
    TESTS_PASSED=$(( TESTS_PASSED + 1 ))
  else
    printf 'FAIL: .runtime-layout.sh.previous missing\n' >&2
    TESTS_FAILED=$(( TESTS_FAILED + 1 ))
  fi
}

test_bootstrap_hash_mismatch_fails() {
  local tdir="$TEST_TMP/bootstrap_mismatch"
  mkdir -p "$tdir/releases/rel-1" "$tdir/deploy" "$tdir/data" "$tdir/state"

  cat <<'EOF' > "$tdir/deploy/runtime-layout.sh"
# old version
OLD_MARKER=1
EOF

  echo "# modified candidate" > "$tdir/releases/rel-1/runtime-layout.sh"

  cat <<EOF > "$tdir/releases/rel-1/release-manifest.json"
{
  "release_id": "rel-1",
  "artifacts": {
    "runtime-layout.sh": "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
  }
}
EOF
  touch "$tdir/releases/rel-1/release-manifest.bundle"

  local exit_code=0
  bash "$DEPLOY_DIR/bootstrap-deployment-engine.sh" \
    --release-dir "$tdir/releases/rel-1" \
    --runtime-deploy-dir "$tdir/deploy" || exit_code=$?

  assert_eq "1" "$exit_code" "Hash mismatch aborts bootstrap before install"
  if grep -q "OLD_MARKER=1" "$tdir/deploy/runtime-layout.sh"; then
    printf 'PASS: deploy directory untouched after hash mismatch\n'
    TESTS_PASSED=$(( TESTS_PASSED + 1 ))
  else
    printf 'FAIL: deploy directory modified despite hash mismatch\n' >&2
    TESTS_FAILED=$(( TESTS_FAILED + 1 ))
  fi
}

test_bootstrap_engine_success
test_bootstrap_hash_mismatch_fails

echo "Passed: $TESTS_PASSED, Failed: $TESTS_FAILED"
[[ "$TESTS_FAILED" -eq 0 ]] || exit 1
