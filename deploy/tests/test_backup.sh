#!/usr/bin/env bash
# Test suite for age encrypted database backups, manifests, and secret bundles
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR="$(cd -- "$SCRIPT_DIR/.." && pwd)"

TEST_TMP="$(mktemp -d "${TMPDIR:-/tmp}/acb-backup-tests.XXXXXX")"
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

setup_backup_env() {
  local dir="$1"
  mkdir -p "$dir/data/backups" "$dir/secrets" "$dir/bin"
  export BACKUP_DIR="$dir/data/backups"
  export SECRETS_DIR="$dir/secrets"
  export SCRIPT_DIR="$dir"
  export DATA_DIR="$dir/data"
  export DATABASE_PATH="$dir/data/gateway.db"
  export PATH="$dir/bin:$PATH"
  export MOCK_AGE_FAIL=0
  export MOCK_INTEGRITY_FAIL=0
  export REQUIRE_OFFHOST_BACKUP=0
  unset OFFHOST_BACKUP_HOOK || true

  # Secrets
  printf '0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n' > "$SECRETS_DIR/app_master_key"
  printf 'worker-tok-123\n' > "$SECRETS_DIR/worker_internal_token"
  printf 'worker-tok-123\n' > "$SECRETS_DIR/auth_browser_internal_token"
  printf 'tts-tok-123\n' > "$SECRETS_DIR/tts_internal_token"
  printf 'bark-user\n' > "$SECRETS_DIR/bark_basic_auth_user"
  printf 'bark-pass\n' > "$SECRETS_DIR/bark_basic_auth_password"
  chmod 600 "$SECRETS_DIR"/* 2>/dev/null || true

  # Mock db
  printf 'SQLite format 3\n' > "$dir/data/gateway.db"

  # Mock sqlite3
  cat <<'EOF' > "$dir/bin/sqlite3"
#!/usr/bin/env bash
set -eu
query="${2:-}"
if [[ "$query" =~ integrity_check ]]; then
  if [[ "${MOCK_INTEGRITY_FAIL:-0}" == "1" ]]; then
    printf 'corrupt\n'
    exit 1
  fi
  printf 'ok\n'
elif [[ "$query" =~ schema_migrations ]]; then
  printf '12\n'
elif [[ "$query" =~ .backup ]]; then
  out="$(printf '%s' "$query" | cut -d"'" -f2)"
  mkdir -p "$(dirname "$out")"
  printf 'SQLite format 3\n' > "$out"
else
  printf 'ok\n'
fi
exit 0
EOF
  chmod +x "$dir/bin/sqlite3"

  # Mock age
  cat <<'EOF' > "$dir/bin/age"
#!/usr/bin/env bash
set -eu
if [[ "${MOCK_AGE_FAIL:-0}" == "1" ]]; then
  exit 1
fi
if [ ! -t 0 ]; then
  cat >/dev/null 2>&1 || true
fi
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
  mkdir -p "$(dirname "$out")"
  printf 'age-encrypted-payload\n' > "$out"
fi
exit 0
EOF
  chmod +x "$dir/bin/age"
}

# ==============================================================================
# TEST 1: Durable backup produces .db.age and manifest, NO plaintext .db snapshot
# ==============================================================================
printf '\n=== TEST 1: Durable backup produces ciphertext only ===\n'
T1="$TEST_TMP/t1"
setup_backup_env "$T1"
export BACKUP_AGE_RECIPIENT="age1testrecipient000000000000000000000000000000000000000000000000"

rc=0
"$DEPLOY_DIR/backup-db.sh" >/dev/null 2>&1 || rc=$?
assert_eq "0" "$rc" "backup-db.sh succeeds with configured recipient and age"

# Check durable directory contains .db.age
age_files=("$BACKUP_DIR"/*.db.age)
assert_eq "1" "${#age_files[@]}" "Exactly one .db.age file exists in backup directory"

# Check manifest exists
manifest_files=("$BACKUP_DIR"/manifest-*.json)
assert_eq "1" "${#manifest_files[@]}" "Exactly one manifest file exists in backup directory"

# Assert NO plaintext .db files in backup directory
plain_db_files=()
while IFS= read -r f; do
  [[ -n "$f" ]] && plain_db_files+=("$f")
done < <(find "$BACKUP_DIR" -maxdepth 1 -name "*.db" 2>/dev/null || true)
assert_eq "0" "${#plain_db_files[@]}" "Zero plaintext .db files exist in durable backup directory"

# Assert app_master_key is NOT copied to backup directory
sec_found=0
for d in "$BACKUP_DIR"/secrets-*; do
  if [[ -d "$d" ]]; then
    sec_found=1
    break
  fi
done
if [[ "$sec_found" -eq 1 || -f "$BACKUP_DIR/app_master_key" ]]; then
  printf 'FAIL: app_master_key or secrets directory was copied into routine backup directory\n' >&2
  TESTS_FAILED=$(( TESTS_FAILED + 1 ))
else
  printf 'PASS: app_master_key was not copied into routine backup directory\n'
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
fi

# ==============================================================================
# TEST 2: Off-host hook receives .db.age and manifest path, never plaintext
# ==============================================================================
printf '\n=== TEST 2: Off-host hook receives ciphertext only ===\n'
T2="$TEST_TMP/t2"
setup_backup_env "$T2"
export BACKUP_AGE_RECIPIENT="age1testrecipient000000000000000000000000000000000000000000000000"

hook_log="$T2/hook_args.log"
cat <<EOF > "$T2/bin/offhost-hook"
#!/usr/bin/env bash
printf '%s\n' "\$@" >> "$hook_log"
exit 0
EOF
chmod +x "$T2/bin/offhost-hook"
export OFFHOST_BACKUP_HOOK="$T2/bin/offhost-hook"

"$DEPLOY_DIR/backup-db.sh" >/dev/null 2>&1
assert_eq "2" "$(wc -l < "$hook_log" | tr -d ' ')" "Offhost hook received exactly 2 arguments"

arg1="$(head -n 1 "$hook_log")"
arg2="$(sed -n '2p' "$hook_log")"
if [[ "$arg1" == *.db.age && "$arg2" == *.json ]]; then
  printf 'PASS: Offhost hook received .db.age and manifest paths\n'
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
else
  printf 'FAIL: Offhost hook received unexpected args: [%s] [%s]\n' "$arg1" "$arg2" >&2
  TESTS_FAILED=$(( TESTS_FAILED + 1 ))
fi

# ==============================================================================
# TEST 3: Fail-closed on missing age recipient
# ==============================================================================
printf '\n=== TEST 3: Fail closed on missing age recipient ===\n'
T3="$TEST_TMP/t3"
setup_backup_env "$T3"
export BACKUP_AGE_RECIPIENT=""

rc=0
"$DEPLOY_DIR/backup-db.sh" >/dev/null 2>&1 || rc=$?
assert_eq "1" "$rc" "backup-db.sh aborts when BACKUP_AGE_RECIPIENT is empty"

# ==============================================================================
# TEST 4: Fail-closed on failed SQLite integrity check
# ==============================================================================
printf '\n=== TEST 4: Fail closed on failed SQLite integrity check ===\n'
T4="$TEST_TMP/t4"
setup_backup_env "$T4"
export BACKUP_AGE_RECIPIENT="age1testrecipient000000000000000000000000000000000000000000000000"
export MOCK_INTEGRITY_FAIL=1

rc=0
"$DEPLOY_DIR/backup-db.sh" >/dev/null 2>&1 || rc=$?
assert_eq "1" "$rc" "backup-db.sh aborts when SQLite integrity check fails"

# ==============================================================================
# TEST 5: Fail-closed on failed age encryption
# ==============================================================================
printf '\n=== TEST 5: Fail closed on failed age encryption ===\n'
T5="$TEST_TMP/t5"
setup_backup_env "$T5"
export BACKUP_AGE_RECIPIENT="age1testrecipient000000000000000000000000000000000000000000000000"
export MOCK_AGE_FAIL=1

rc=0
"$DEPLOY_DIR/backup-db.sh" >/dev/null 2>&1 || rc=$?
assert_eq "1" "$rc" "backup-db.sh aborts when age encryption fails"

# ==============================================================================
# TEST 6: Fail-closed on failed required off-host hook
# ==============================================================================
printf '\n=== TEST 6: Fail closed on failed required off-host hook ===\n'
T6="$TEST_TMP/t6"
setup_backup_env "$T6"
export BACKUP_AGE_RECIPIENT="age1testrecipient000000000000000000000000000000000000000000000000"
export REQUIRE_OFFHOST_BACKUP=1

cat <<'EOF' > "$T6/bin/failing-hook"
#!/usr/bin/env bash
exit 1
EOF
chmod +x "$T6/bin/failing-hook"
export OFFHOST_BACKUP_HOOK="$T6/bin/failing-hook"

rc=0
"$DEPLOY_DIR/backup-db.sh" >/dev/null 2>&1 || rc=$?
assert_eq "1" "$rc" "backup-db.sh aborts when required offhost hook fails"

# ==============================================================================
# TEST 7: Separate encrypted secret backup streams directly into age
# ==============================================================================
printf '\n=== TEST 7: Separate encrypted secret backup ===\n'
T7="$TEST_TMP/t7"
setup_backup_env "$T7"
export BACKUP_AGE_RECIPIENT="age1testrecipient000000000000000000000000000000000000000000000000"

rc=0
"$DEPLOY_DIR/backup-secrets.sh" >/dev/null 2>&1 || rc=$?
assert_eq "0" "$rc" "backup-secrets.sh succeeds with recipient and age"

secret_ages=("$BACKUP_DIR"/secrets-*.tar.age)
assert_eq "1" "${#secret_ages[@]}" "Exactly one secrets-*.tar.age file exists"

# Ensure NO plaintext secrets folder was created in BACKUP_DIR
plain_sec_dirs=()
while IFS= read -r d; do
  [[ -n "$d" ]] && plain_sec_dirs+=("$d")
done < <(find "$BACKUP_DIR" -maxdepth 1 -type d -name "secrets-*" 2>/dev/null || true)
assert_eq "0" "${#plain_sec_dirs[@]}" "Zero plaintext secrets directories created in backup directory"

# Manifest exists and does NOT contain secret values
manifest_sec=("$BACKUP_DIR"/manifest-secrets-*.json)
assert_eq "1" "${#manifest_sec[@]}" "Exactly one manifest-secrets-*.json exists"
if grep -q "0123456789abcdef" "${manifest_sec[0]}"; then
  printf 'FAIL: manifest-secrets contains plaintext master key value\n' >&2
  TESTS_FAILED=$(( TESTS_FAILED + 1 ))
else
  printf 'PASS: manifest-secrets does not leak secret values\n'
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
fi

# ==============================================================================
# TEST 8: Failure / interrupt cleans up staging directory and orphans
# ==============================================================================
printf '\n=== TEST 8: Failure cleans up staging directory and orphans ===\n'
T8="$TEST_TMP/t8"
setup_backup_env "$T8"
export BACKUP_AGE_RECIPIENT="age1testrecipient000000000000000000000000000000000000000000000000"
export MOCK_AGE_FAIL=1

"$DEPLOY_DIR/backup-db.sh" >/dev/null 2>&1 || true
# Verify no .staging.* directories remain
staging_dirs=()
while IFS= read -r d; do
  [[ -n "$d" ]] && staging_dirs+=("$d")
done < <(find "$BACKUP_DIR" -maxdepth 1 -name ".staging.*" 2>/dev/null || true)
assert_eq "0" "${#staging_dirs[@]}" "All staging directories and orphans cleaned up after failure"

printf '\n======================================================\n'
printf 'Backup Test Results: %d passed, %d failed\n' "$TESTS_PASSED" "$TESTS_FAILED"
printf '======================================================\n'

if [[ "$TESTS_FAILED" -gt 0 ]]; then
  exit 1
fi
exit 0
