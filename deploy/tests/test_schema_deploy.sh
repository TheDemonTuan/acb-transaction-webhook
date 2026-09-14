#!/usr/bin/env bash
# deploy/tests/test_schema_deploy.sh
# Test suite for isolated schema deployment transaction, fail-closed active-auth check,
# preflight backup verification, migration isolation, and no route changes.
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR="$(cd -- "$SCRIPT_DIR/.." && pwd)"

TEST_TMP="$(mktemp -d "${TMPDIR:-/tmp}/acb-schema-deploy-tests.XXXXXX")"
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
  if ! grep -q "$pattern" "$file" 2>/dev/null; then
    printf 'FAIL: %s (file %s did not match "%s")\n' "$msg" "$file" "$pattern" >&2
    TESTS_FAILED=$(( TESTS_FAILED + 1 ))
    return 1
  fi
  printf 'PASS: %s\n' "$msg"
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
  return 0
}

setup_schema_mock_env() {
  local test_dir="$1"
  export MOCK_STATE_DIR="$test_dir"
  export MOCK_ACTIVE_AUTH=0
  export MOCK_ACTIVE_AUTH_FAIL=0
  export MOCK_BACKUP_FAIL=0
  export MOCK_MIGRATION_FAIL=0

  mkdir -p "$test_dir/bin" "$test_dir/secrets" "$test_dir/data/backups" "$test_dir/dynamic"
  touch "$test_dir/data/gateway.db"

  cat <<EOF > "$test_dir/.env.production"
APP_ENV=production
RUNTIME_ROLE=gateway
DATA_VOLUME_NAME=bank-event-gateway_gateway_data
EOF

  cat <<EOF > "$test_dir/.release.env"
IMAGE_REF_BLUE=ghcr.io/test/gateway@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
IMAGE_REF_GREEN=ghcr.io/test/gateway@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
WORKER_IMAGE_REF=ghcr.io/test/worker@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
DBTOOL_IMAGE_REF=ghcr.io/test/dbtool@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
BROWSER_IMAGE_REF=ghcr.io/test/browser@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
TTS_IMAGE_REF=ghcr.io/test/tts@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
BARK_IMAGE_REF=ghcr.io/finb/bark-server@sha256:32d65b07fa835c99b31a396b77727a04ed058377fc2482da3e9dc7397167ffc4
EOF

  export ENV_FILE="$test_dir/.env.production"
  export RELEASE_ENV_FILE="$test_dir/.release.env"
  export ACTIVE_SLOT_FILE="$test_dir/.active-slot"
  export PREVIOUS_SLOT_FILE="$test_dir/.previous-slot"
  export TRAEFIK_DYNAMIC_DIR="$test_dir/dynamic"
  export ACB_CONFIG="$test_dir/dynamic/acb.yml"
  export SECRETS_DIR="$test_dir/secrets"
  export BACKUP_DIR="$test_dir/data/backups"
  export MIGRATION_RECORD="$test_dir/data/migration-record.json"
  export DEPLOY_LOCK_FILE="$test_dir/.deploy.lock"

  printf 'mock-master-key\n' > "$test_dir/secrets/app_master_key"
  printf 'mock-tts-token\n' > "$test_dir/secrets/tts_internal_token"
  printf 'mock-worker-token\n' > "$test_dir/secrets/worker_internal_token"
  printf 'mock-bark-user\n' > "$test_dir/secrets/bark_basic_auth_user"
  printf 'mock-bark-pass\n' > "$test_dir/secrets/bark_basic_auth_password"
  chmod 600 "$test_dir/secrets/"* 2>/dev/null || true

  cat <<EOF > "$test_dir/dynamic/acb.yml"
http:
  services:
    acb-service:
      loadBalancer:
        servers:
          - url: "http://acb-web-blue:8090"
EOF
  printf 'blue' > "$test_dir/.active-slot"

  # Mock docker CLI
  cat <<'EOF' > "$test_dir/bin/docker"
#!/usr/bin/env bash
set -eu
STATE_DIR="${MOCK_STATE_DIR}"
cmd="${1:-}"
sub="${2:-}"

if [[ "$cmd" == "volume" ]]; then
  if [[ "$sub" == "ls" ]]; then
    printf 'bank-event-gateway_gateway_data\n'
    exit 0
  fi
  exit 0
elif [[ "$cmd" == "run" ]]; then
  if [[ "$*" =~ -active-auth-count ]]; then
    if [[ "${MOCK_ACTIVE_AUTH_FAIL:-0}" == "1" ]]; then
      exit 1
    elif [[ "${MOCK_ACTIVE_AUTH:-0}" == "1" ]]; then
      printf '{"activeCount":1}\n'
      exit 0
    else
      printf '{"activeCount":0}\n'
      exit 0
    fi
  fi
  if [[ "$*" =~ -backup-to ]]; then
    if [[ "${MOCK_BACKUP_FAIL:-0}" == "1" ]]; then
      exit 1
    fi
    backup_file=""
    backup_host_dir=""
    for arg in "$@"; do
      if [[ "$arg" =~ :/backup ]]; then
        backup_host_dir="${arg%:/backup*}"
      fi
    done
    if [[ -z "$backup_host_dir" ]]; then
      backup_host_dir="${BACKUP_DIR:-$STATE_DIR/data/backups}"
    fi

    next_arg=0
    for a in "$@"; do
      if [[ "$next_arg" -eq 1 ]]; then
        backup_file="$a"
        break
      fi
      if [[ "$a" == "-backup-to" ]]; then
        next_arg=1
      fi
    done
    if [[ -n "$backup_file" ]]; then
      fname="$(basename "$backup_file")"
      target="$backup_host_dir/$fname"
      mkdir -p "$(dirname "$target")" 2>/dev/null || true
      printf 'SQLite format 3\n' > "$target"
    fi
    exit 0
  fi
  if [[ "$*" =~ -gate-acquire ]]; then
    if [[ "${MOCK_ACTIVE_AUTH:-0}" == "1" ]]; then
      exit 1
    fi
    printf '{"status":"acquired","gateState":"LOCKED","leaseToken":"mocktoken123","fenceGeneration":2}\n'
    exit 0
  fi
  if [[ "$*" =~ -gate-release ]]; then
    printf '{"status":"released","gateState":"OPEN"}\n'
    exit 0
  fi
  if [[ "$*" =~ -schema-compat ]]; then
    printf '{"compatible":true,"schemaVersion":9,"requiredVersion":9}\n'
    exit 0
  fi
  if [[ "$*" =~ -gate-status ]]; then
    printf '{"gateState":"OPEN","activeAuthCount":0}\n'
    exit 0
  fi
  if [[ "$*" =~ -migrate ]]; then
    if [[ "${MOCK_MIGRATION_FAIL:-0}" == "1" ]]; then
      exit 1
    fi
    printf 'MIGRATED\n' >> "$STATE_DIR/migration_completed.log"
    exit 0
  fi
  if [[ "$*" =~ -check ]]; then
    printf '{"integrityOK":true}\n'
    exit 0
  fi
fi
exit 0
EOF
  chmod +x "$test_dir/bin/docker"

  # Mock sqlite3 CLI
  cat <<'EOF' > "$test_dir/bin/sqlite3"
#!/usr/bin/env bash
set -eu
query="${2:-}"
if [[ "$query" =~ integrity_check ]]; then
  printf 'ok\n'
elif [[ "$query" =~ schema_migrations ]]; then
  printf '15\n'
else
  printf '0\n'
fi
exit 0
EOF
  chmod +x "$test_dir/bin/sqlite3"

  export PATH="$test_dir/bin:$PATH"
}

# ==============================================================================
# TEST 1: Active Auth Attempt Blocks Schema Promotion
# ==============================================================================
printf '\n=== TEST 1: Active Auth Blocks Schema Promotion ===\n'
T1="$TEST_TMP/t1"
setup_schema_mock_env "$T1"
export MOCK_ACTIVE_AUTH=1

set +e
"$DEPLOY_DIR/deploy-schema.sh" "ghcr.io/test/dbtool@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
exit_code=$?
set -e

assert_eq "1" "$(( exit_code != 0 ? 1 : 0 ))" "Schema promotion aborted when active auth attempt is in progress"

# ==============================================================================
# TEST 2: Inability to Determine Active-Auth State Blocks Schema Promotion
# ==============================================================================
printf '\n=== TEST 2: Active-Auth Tool Failure Blocks Schema Promotion ===\n'
T2="$TEST_TMP/t2"
setup_schema_mock_env "$T2"
export MOCK_ACTIVE_AUTH_FAIL=1

set +e
"$DEPLOY_DIR/deploy-schema.sh" "ghcr.io/test/dbtool@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
exit_code=$?
set -e

assert_eq "1" "$(( exit_code != 0 ? 1 : 0 ))" "Schema promotion aborted when dbtool fails active-auth check (exit code 1)"

# ==============================================================================
# TEST 3: Preflight Backup Failure Prevents Migration
# ==============================================================================
printf '\n=== TEST 3: Backup Failure Prevents Migration ===\n'
T3="$TEST_TMP/t3"
setup_schema_mock_env "$T3"
export MOCK_BACKUP_FAIL=1

set +e
"$DEPLOY_DIR/deploy-schema.sh" "ghcr.io/test/dbtool@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
exit_code=$?
set -e

assert_eq "1" "$(( exit_code != 0 ? 1 : 0 ))" "Schema promotion aborted when preflight backup failed"
if [[ -f "$T3/migration_completed.log" ]]; then
  printf 'FAIL: Migration was run despite backup failure!\n' >&2
  TESTS_FAILED=$(( TESTS_FAILED + 1 ))
else
  printf 'PASS: Migration was never executed on backup failure\n'
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
fi

# ==============================================================================
# TEST 4: Migration Failure Does NOT Auto-Restore Live DB
# ==============================================================================
printf '\n=== TEST 4: Migration Failure Does NOT Auto-Restore Live DB ===\n'
T4="$TEST_TMP/t4"
setup_schema_mock_env "$T4"
export MOCK_MIGRATION_FAIL=1
printf 'ORIGINAL_LIVE_DB\n' > "$T4/data/gateway.db"

set +e
"$DEPLOY_DIR/deploy-schema.sh" "ghcr.io/test/dbtool@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
exit_code=$?
set -e

assert_eq "1" "$(( exit_code != 0 ? 1 : 0 ))" "Schema promotion aborted on migration failure"
assert_file_contains "$T4/data/gateway.db" "ORIGINAL_LIVE_DB" "Live DB was not overwritten automatically"

# ==============================================================================
# TEST 5: Successful Schema Promotion
# ==============================================================================
printf '\n=== TEST 5: Successful Schema Deployment Transaction ===\n'
T5="$TEST_TMP/t5"
setup_schema_mock_env "$T5"

"$DEPLOY_DIR/deploy-schema.sh" "ghcr.io/test/dbtool@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

assert_file_exists "$T5/data/migration-record.json" "Migration metadata record was saved"
assert_file_contains "$T5/data/migration-record.json" "COMPLETED" "Migration record status is COMPLETED"
# Assert Traefik dynamic route remains untouched
assert_file_contains "$T5/dynamic/acb.yml" "acb-web-blue" "Traefik route was never touched by schema promotion"
assert_eq "blue" "$(cat "$T5/.active-slot")" "Active slot remains blue"

printf '\n==================================================\n'
printf 'SCHEMA DEPLOY TEST RESULTS: %d PASSED, %d FAILED\n' "$TESTS_PASSED" "$TESTS_FAILED"
printf '==================================================\n'

if [[ "$TESTS_FAILED" -gt 0 ]]; then
  exit 1
fi
exit 0
