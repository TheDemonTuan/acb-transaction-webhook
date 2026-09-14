#!/usr/bin/env bash
# deploy/tests/test_deploy.sh
# Test suite for deployment entrypoints, legacy deprecation hard-fails, and component existence.
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR="$(cd -- "$SCRIPT_DIR/.." && pwd)"

TEST_TMP="$(mktemp -d "${TMPDIR:-/tmp}/acb-deploy-tests.XXXXXX")"
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
  if ! grep -q -- "$pattern" "$file" 2>/dev/null; then
    printf 'FAIL: %s (file %s did not match "%s")\n' "$msg" "$file" "$pattern" >&2
    TESTS_FAILED=$(( TESTS_FAILED + 1 ))
    return 1
  fi
  printf 'PASS: %s\n' "$msg"
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
  return 0
}

# ==============================================================================
# TEST 1: Obsolete deploy-warm.sh hard-fails with zero arguments
# ==============================================================================
printf '\n=== TEST 1: Obsolete deploy-warm.sh hard-fails with zero arguments ===\n'
err_output="$TEST_TMP/t1.err"
set +e
"$DEPLOY_DIR/deploy-warm.sh" 2>"$err_output"
code=$?
set -e
assert_eq "1" "$(( code != 0 ? 1 : 0 ))" "deploy-warm.sh exits with failure code"
assert_file_contains "$err_output" "obsolete and has been retired" "deploy-warm.sh reports retired status"
assert_file_contains "$err_output" "deploy/deploy-gateway.sh" "deploy-warm.sh recommends deploy-gateway.sh"

# ==============================================================================
# TEST 2: Obsolete deploy-warm.sh hard-fails with candidate image argument
# ==============================================================================
printf '\n=== TEST 2: Obsolete deploy-warm.sh hard-fails with image argument ===\n'
err_output="$TEST_TMP/t2.err"
set +e
"$DEPLOY_DIR/deploy-warm.sh" "ghcr.io/test/gateway@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" 2>"$err_output"
code=$?
set -e
assert_eq "1" "$(( code != 0 ? 1 : 0 ))" "deploy-warm.sh exits with failure code on image arg"
assert_file_contains "$err_output" "obsolete and has been retired" "deploy-warm.sh reports retirement on image arg"

# ==============================================================================
# TEST 3: Obsolete deploy-warm.sh hard-fails with --upgrade-core
# ==============================================================================
printf '\n=== TEST 3: Obsolete deploy-warm.sh hard-fails with --upgrade-core ===\n'
err_output="$TEST_TMP/t3.err"
set +e
"$DEPLOY_DIR/deploy-warm.sh" --upgrade-core "ghcr.io/test/gateway@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" 2>"$err_output"
code=$?
set -e
assert_eq "1" "$(( code != 0 ? 1 : 0 ))" "deploy-warm.sh exits with failure code on --upgrade-core"
assert_file_contains "$err_output" "Monolithic core upgrades" "deploy-warm.sh forbids monolithic upgrade"

# ==============================================================================
# TEST 4: Obsolete deploy-warm.sh hard-fails with --resume-soak
# ==============================================================================
printf '\n=== TEST 4: Obsolete deploy-warm.sh hard-fails with --resume-soak ===\n'
err_output="$TEST_TMP/t4.err"
set +e
"$DEPLOY_DIR/deploy-warm.sh" --resume-soak 2>"$err_output"
code=$?
set -e
assert_eq "1" "$(( code != 0 ? 1 : 0 ))" "deploy-warm.sh exits with failure code on --resume-soak"
assert_file_contains "$err_output" "obsolete and has been retired" "deploy-warm.sh reports retirement on soak flag"

# ==============================================================================
# TEST 5: deploy.sh hard-fails when --upgrade-core flag is supplied
# ==============================================================================
printf '\n=== TEST 5: deploy.sh hard-fails on --upgrade-core ===\n'
err_output="$TEST_TMP/t5.err"
set +e
"$DEPLOY_DIR/deploy.sh" --upgrade-core "ghcr.io/test/gateway@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" 2>"$err_output"
code=$?
set -e
assert_eq "1" "$(( code != 0 ? 1 : 0 ))" "deploy.sh exits with failure code on --upgrade-core"
assert_file_contains "$err_output" "--upgrade-core flag is retired" "deploy.sh explains --upgrade-core retirement"

# ==============================================================================
# TEST 6: deploy.sh exits 1 and prints usage when no arguments provided
# ==============================================================================
printf '\n=== TEST 6: deploy.sh usage check without arguments ===\n'
err_output="$TEST_TMP/t6.err"
touch "$TEST_TMP/test.env"
set +e
ENV_FILE="$TEST_TMP/test.env" "$DEPLOY_DIR/deploy.sh" 2>"$err_output"
code=$?
set -e
assert_eq "1" "$(( code != 0 ? 1 : 0 ))" "deploy.sh fails when missing required image arg"
assert_file_contains "$err_output" "Usage:" "deploy.sh prints usage message"

# ==============================================================================
# TEST 7: Component transaction scripts existence & permissions
# ==============================================================================
printf '\n=== TEST 7: Component transaction scripts audit ===\n'
COMPONENT_SCRIPTS=(
  "deploy/deploy-gateway.sh"
  "deploy/deploy-worker.sh"
  "deploy/deploy-schema.sh"
  "deploy/deploy-auth-browser.sh"
  "deploy/deploy-tts.sh"
  "deploy/deploy-bark.sh"
  "deploy/dispatch-rollout.sh"
  "deploy/switch-slot.sh"
  "deploy/rollback-warm.sh"
  "deploy/rollback.sh"
  "deploy/init-fresh-data.sh"
  "deploy/provision-secrets.sh"
  "deploy/backup.sh"
  "deploy/restore-db.sh"
)

for script in "${COMPONENT_SCRIPTS[@]}"; do
  full_path="$SCRIPT_DIR/../../$script"
  assert_file_exists "$full_path" "Script $script exists"
done

# ==============================================================================
# TEST 8: deploy/rollback.sh --help provides component rollback guidance
# ==============================================================================
printf '\n=== TEST 8: deploy/rollback.sh --help guidance ===\n'
help_output="$TEST_TMP/t8.txt"
"$DEPLOY_DIR/rollback.sh" --help >"$help_output"
assert_file_contains "$help_output" "Gateway Blue/Green" "rollback.sh explains gateway slot scope"
assert_file_contains "$help_output" "deploy-worker.sh" "rollback.sh points to worker rollback"
assert_file_contains "$help_output" "restore-db.sh" "rollback.sh points to database restore"

printf '\nDEPLOY ENTRYPOINTS & RETIREMENT TEST RESULTS: %d PASSED, %d FAILED\n' "$TESTS_PASSED" "$TESTS_FAILED"
if [[ "$TESTS_FAILED" -gt 0 ]]; then
  exit 1
fi
exit 0
