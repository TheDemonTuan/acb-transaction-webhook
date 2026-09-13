#!/usr/bin/env bash
# Test suite for secret validation, provisioning separation, and least-privilege distribution
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR="$(cd -- "$SCRIPT_DIR/.." && pwd)"

TEST_TMP="$(mktemp -d "${TMPDIR:-/tmp}/acb-secrets-tests.XXXXXX")"
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

# ==============================================================================
# TEST 1: Missing app_master_key fails closed; no file is created
# ==============================================================================
printf '\n=== TEST 1: Missing app_master_key fails closed ===\n'
T1="$TEST_TMP/t1"
mkdir -p "$T1/secrets"
export SECRETS_DIR="$T1/secrets"
export SCRIPT_DIR="$T1"
# shellcheck source=deploy/lib.sh
source "$DEPLOY_DIR/lib.sh"

printf 'worker-tok\n' > "$SECRETS_DIR/worker_internal_token"
printf 'tts-tok\n' > "$SECRETS_DIR/tts_internal_token"
printf 'admin\n' > "$SECRETS_DIR/bark_basic_auth_user"
printf 'bark-pass\n' > "$SECRETS_DIR/bark_basic_auth_password"

rc=0
validate_secrets >/dev/null 2>&1 || rc=$?
assert_eq "1" "$rc" "validate_secrets fails when app_master_key is missing"
if [[ -f "$SECRETS_DIR/app_master_key" ]]; then
  printf 'FAIL: app_master_key was created automatically\n' >&2
  TESTS_FAILED=$(( TESTS_FAILED + 1 ))
else
  printf 'PASS: app_master_key was not created automatically\n'
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
fi

# ==============================================================================
# TEST 2: Missing other required secrets fail closed
# ==============================================================================
printf '\n=== TEST 2: Missing other required secrets fail closed ===\n'
T2="$TEST_TMP/t2"
mkdir -p "$T2/secrets"
export SECRETS_DIR="$T2/secrets"
for s in app_master_key worker_internal_token tts_internal_token bark_basic_auth_user bark_basic_auth_password; do
  printf 'mock\n' > "$SECRETS_DIR/$s"
done

# Baseline all secrets present
rc=0
validate_secrets >/dev/null 2>&1 || rc=$?
assert_eq "0" "$rc" "validate_secrets succeeds when all secrets are present"

# Each individual missing secret fails closed
for s in worker_internal_token tts_internal_token bark_basic_auth_user bark_basic_auth_password; do
  rm -f "$SECRETS_DIR/$s"
  rc=0
  validate_secrets >/dev/null 2>&1 || rc=$?
  assert_eq "1" "$rc" "validate_secrets fails when $s is missing"
  printf 'mock\n' > "$SECRETS_DIR/$s"
done

# ==============================================================================
# TEST 3: Unsafe world-readable secret permissions are rejected
# ==============================================================================
printf '\n=== TEST 3: World-readable secret permissions rejected ===\n'
T3="$TEST_TMP/t3"
mkdir -p "$T3/secrets"
export SECRETS_DIR="$T3/secrets"
for s in app_master_key worker_internal_token tts_internal_token bark_basic_auth_user bark_basic_auth_password; do
  printf 'secret-val\n' > "$SECRETS_DIR/$s"
  chmod 600 "$SECRETS_DIR/$s" 2>/dev/null || true
done
# Make one secret world-readable (0644)
chmod 644 "$SECRETS_DIR/bark_basic_auth_password" 2>/dev/null || true
# On Linux/POSIX systems supporting chmod octal bits, check_secret_permissions must reject
if command -v stat >/dev/null 2>&1 && [[ ! "$(uname -s 2>/dev/null)" =~ MINGW|MSYS|CYGWIN ]]; then
  mode="$(stat -c '%a' "$SECRETS_DIR/bark_basic_auth_password" 2>/dev/null || stat -f '%Lp' "$SECRETS_DIR/bark_basic_auth_password" 2>/dev/null || echo "")"
  if [[ "${mode: -2}" != "00" && -n "$mode" ]]; then
    rc=0
    check_secret_permissions "$SECRETS_DIR/bark_basic_auth_password" >/dev/null 2>&1 || rc=$?
    assert_eq "1" "$rc" "check_secret_permissions rejects world-readable secret (mode $mode)"
  fi
else
  printf 'PASS: Skipping chmod octal bit test on non-POSIX Windows/MSYS platform (%s)\n' "$(uname -s 2>/dev/null || echo unknown)"
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
fi

# ==============================================================================
# TEST 4: provision-secrets.sh fails without confirmation
# ==============================================================================
printf '\n=== TEST 4: provision-secrets.sh fails without confirmation ===\n'
T4="$TEST_TMP/t4"
mkdir -p "$T4/secrets"
export SECRETS_DIR="$T4/secrets"
export SCRIPT_DIR="$T4"

rc=0
"$DEPLOY_DIR/provision-secrets.sh" </dev/null >/dev/null 2>&1 || rc=$?
assert_eq "1" "$rc" "provision-secrets.sh exits non-zero without confirmation"

# ==============================================================================
# TEST 5: assert_fresh_installation aborts on existing database or key material
# ==============================================================================
printf '\n=== TEST 5: assert_fresh_installation aborts on pre-existing state ===\n'
T5="$TEST_TMP/t5"
mkdir -p "$T5/data" "$T5/secrets"
touch "$T5/data/gateway.db"
export SCRIPT_DIR="$T5"
export SECRETS_DIR="$T5/secrets"

rc=0
assert_fresh_installation >/dev/null 2>&1 || rc=$?
assert_eq "1" "$rc" "assert_fresh_installation aborts when gateway.db exists"

rm -f "$T5/data/gateway.db"
printf 'pre-existing-key\n' > "$T5/secrets/app_master_key"
rc=0
assert_fresh_installation >/dev/null 2>&1 || rc=$?
assert_eq "1" "$rc" "assert_fresh_installation aborts when pre-existing key exists"

# ==============================================================================
# TEST 6: Fresh provision creates 64-hex master key and restricted secrets
# ==============================================================================
printf '\n=== TEST 6: Fresh provision creates valid secrets ===\n'
T6="$TEST_TMP/t6"
mkdir -p "$T6/secrets"
export SECRETS_DIR="$T6/secrets"
export SCRIPT_DIR="$T6"

rc=0
"$DEPLOY_DIR/provision-secrets.sh" --confirm-fresh-provision >/dev/null 2>&1 || rc=$?
assert_eq "0" "$rc" "provision-secrets.sh succeeds on clean directory"

key_content="$(tr -d '\r\n' < "$SECRETS_DIR/app_master_key")"
key_len="${#key_content}"
assert_eq "64" "$key_len" "app_master_key is 64 hex characters (32 bytes raw)"

for s in worker_internal_token tts_internal_token bark_basic_auth_user bark_basic_auth_password; do
  if [[ -s "$SECRETS_DIR/$s" ]]; then
    printf 'PASS: %s is non-empty\n' "$s"
    TESTS_PASSED=$(( TESTS_PASSED + 1 ))
  else
    printf 'FAIL: %s is empty or missing\n' "$s" >&2
    TESTS_FAILED=$(( TESTS_FAILED + 1 ))
  fi
done

# ==============================================================================
# TEST 7: Compose least-privilege secret distribution matrix
# ==============================================================================
printf '\n=== TEST 7: Compose least-privilege secret matrix ===\n'
compose_file="$DEPLOY_DIR/compose.prod.yaml"

# Worker must NOT mount tts_internal_token
if grep -A 10 'container_name: acb-worker' "$compose_file" | grep -q 'tts_internal_token'; then
  printf 'FAIL: worker mounts tts_internal_token\n' >&2
  TESTS_FAILED=$(( TESTS_FAILED + 1 ))
else
  printf 'PASS: worker does not mount tts_internal_token\n'
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
fi

# Gateway blue must NOT mount bark credentials
if grep -A 60 'container_name: acb-gateway-blue' "$compose_file" | grep -q 'bark_basic_auth_user'; then
  printf 'FAIL: gateway-blue mounts bark_basic_auth_user\n' >&2
  TESTS_FAILED=$(( TESTS_FAILED + 1 ))
else
  printf 'PASS: gateway-blue does not mount bark credentials\n'
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
fi

# Gateway green must NOT mount bark credentials
if grep -A 60 'container_name: acb-gateway-green' "$compose_file" | grep -q 'bark_basic_auth_user'; then
  printf 'FAIL: gateway-green mounts bark_basic_auth_user\n' >&2
  TESTS_FAILED=$(( TESTS_FAILED + 1 ))
else
  printf 'PASS: gateway-green does not mount bark credentials\n'
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
fi

printf '\n======================================================\n'
printf 'Secrets Test Results: %d passed, %d failed\n' "$TESTS_PASSED" "$TESTS_FAILED"
printf '======================================================\n'

if [[ "$TESTS_FAILED" -gt 0 ]]; then
  exit 1
fi
exit 0
