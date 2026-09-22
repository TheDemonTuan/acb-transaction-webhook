#!/usr/bin/env bash
# Test suite for disaster recovery restore drill and isolated canary decrypt
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR="$(cd -- "$SCRIPT_DIR/.." && pwd)"
REPO_ROOT="$(cd -- "$DEPLOY_DIR/.." && pwd)"

TEST_TMP="$(mktemp -d "${TMPDIR:-/tmp}/acb-restore-drill-tests.XXXXXX")"
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

# Ensure age is in PATH if installed in user's go/bin
if [[ -d "${HOME}/go/bin" ]]; then
  export PATH="${HOME}/go/bin:$PATH"
fi

# ==============================================================================
# TEST 1: restore-drill.sh fails if --drill-dir is omitted
# ==============================================================================
printf '\n=== TEST 1: Refuse execution without explicit drill dir ===\n'
rc=0
"$REPO_ROOT/scripts/ops/restore-drill.sh" >/dev/null 2>&1 || rc=$?
assert_eq "1" "$rc" "restore-drill.sh aborts without --drill-dir"

# ==============================================================================
# TEST 2: restore-drill.sh refuses live production data paths
# ==============================================================================
printf '\n=== TEST 2: Refuse live production data directory ===\n'
rc=0
"$REPO_ROOT/scripts/ops/restore-drill.sh" --drill-dir "$DEPLOY_DIR/data" >/dev/null 2>&1 || rc=$?
assert_eq "1" "$rc" "restore-drill.sh refuses live production deploy/data directory"

rc=0
"$REPO_ROOT/scripts/ops/restore-drill.sh" --drill-dir "/data" >/dev/null 2>&1 || rc=$?
assert_eq "1" "$rc" "restore-drill.sh refuses root /data directory"

# ==============================================================================
# TEST 3: Full End-to-End Isolated Restore Drill
# ==============================================================================
printf '\n=== TEST 3: Full End-to-End Isolated Restore Drill ===\n'
T3="$TEST_TMP/drill_workspace"
evidence_out="$T3/evidence.json"

restore_log="$TEST_TMP/restore-drill.log"
if "$REPO_ROOT/scripts/ops/restore-drill.sh" --drill-dir "$T3" --evidence "$evidence_out" >"$restore_log" 2>&1; then
  rc=0
else
  rc=$?
  cat "$restore_log" >&2
fi
assert_eq "0" "$rc" "restore-drill.sh executes end-to-end successfully"

if [[ -f "$evidence_out" ]]; then
  printf 'PASS: Evidence JSON record exists\n'
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
else
  printf 'FAIL: Evidence JSON record was not created\n' >&2
  TESTS_FAILED=$(( TESTS_FAILED + 1 ))
fi

# Check evidence contents
if grep -q '"drill_status": *"SUCCESS"' "$evidence_out" && grep -q '"integrity_check": *"ok"' "$evidence_out"; then
  printf 'PASS: Evidence JSON indicates SUCCESS and ok integrity\n'
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
else
  printf 'FAIL: Evidence JSON missing expected success or integrity status\n' >&2
  TESTS_FAILED=$(( TESTS_FAILED + 1 ))
fi

for secret in app_master_key worker_internal_token auth_browser_internal_token tts_internal_token bark_basic_auth_user bark_basic_auth_password; do
  original="$T3/fixture/secrets/$secret"
  restored="$T3/canary_restored/secrets/$secret"
  if [[ -s "$original" && -s "$restored" ]] && cmp -s "$original" "$restored"; then
    printf 'PASS: %s restored with matching contents\n' "$secret"
    TESTS_PASSED=$(( TESTS_PASSED + 1 ))
  else
    printf 'FAIL: %s missing, empty, or changed after restore\n' "$secret" >&2
    TESTS_FAILED=$(( TESTS_FAILED + 1 ))
  fi

  if [[ ! -s "$original" || ! -s "$evidence_out" || ! -s "$restore_log" ]] || grep -Fq -f "$original" "$evidence_out" "$restore_log"; then
    printf 'FAIL: Cannot verify plaintext protection for %s, or value leaked\n' "$secret" >&2
    TESTS_FAILED=$(( TESTS_FAILED + 1 ))
  else
    printf 'PASS: %s plaintext absent from evidence and log\n' "$secret"
    TESTS_PASSED=$(( TESTS_PASSED + 1 ))
  fi
done

# ==============================================================================
# TEST 4: restore-db.sh fails when using wrong age recovery identity
# ==============================================================================
printf '\n=== TEST 4: restore-db.sh fails with wrong identity ===\n'
wrong_identity="$TEST_TMP/wrong_identity.txt"
if command -v age-keygen >/dev/null 2>&1; then
  age-keygen -o "$wrong_identity" 2>/dev/null
elif [[ -x "${HOME}/go/bin/age-keygen" ]]; then
  "${HOME}/go/bin/age-keygen" -o "$wrong_identity" 2>/dev/null
fi

backup_artifact="$(find "$T3/fixture/backups" -name "*.db.age" | head -n1)"
rc=0
"$DEPLOY_DIR/restore-db.sh" \
  --identity "$wrong_identity" \
  --backup "$backup_artifact" \
  --target-dir "$TEST_TMP/wrong_restore" >/dev/null 2>&1 || rc=$?
assert_eq "1" "$rc" "restore-db.sh aborts when using incorrect private recovery key"

# ==============================================================================
# TEST 5: restore-db.sh fails closed when verification tools are missing
# ==============================================================================
printf '\n=== TEST 5: restore-db.sh fails closed without verification tools ===\n'
no_tools_dir="$TEST_TMP/no_tools_bin"
mkdir -p "$no_tools_dir"
cat <<'EOF' > "$no_tools_dir/age"
#!/usr/bin/env bash
out=""
while [[ $# -gt 0 ]]; do
  if [[ "$1" == "-o" ]]; then
    out="$2"
    shift 2
  else
    shift
  fi
done
if [[ -n "$out" ]]; then
  echo "dummy sqlite database" > "$out"
fi
exit 0
EOF
chmod +x "$no_tools_dir/age"

rc=0
PATH="$no_tools_dir:/usr/bin:/bin" "$DEPLOY_DIR/restore-db.sh" \
  --identity "$wrong_identity" \
  --backup "$backup_artifact" \
  --target-dir "$TEST_TMP/no_tools_restore" >/dev/null 2>&1 || rc=$?
assert_eq "1" "$rc" "restore-db.sh fails closed when no integrity tools exist"

printf '\n======================================================\n'
printf 'Restore Drill Test Results: %d passed, %d failed\n' "$TESTS_PASSED" "$TESTS_FAILED"
printf '======================================================\n'

if [[ "$TESTS_FAILED" -gt 0 ]]; then
  exit 1
fi
exit 0
