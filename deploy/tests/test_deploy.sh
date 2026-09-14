#!/usr/bin/env bash
# Shell test suite for transactional warm cutover and data safety
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
  if ! grep -q "$pattern" "$file" 2>/dev/null; then
    printf 'FAIL: %s (file %s did not match "%s")\n' "$msg" "$file" "$pattern" >&2
    TESTS_FAILED=$(( TESTS_FAILED + 1 ))
    return 1
  fi
  printf 'PASS: %s\n' "$msg"
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
  return 0
}

setup_mock_env() {
  local test_dir="$1"
  export MOCK_VOLUME_MISSING=0
  export MOCK_VOLUME_AMBIGUOUS=0
  export MOCK_MIGRATION_FAIL=0
  export MOCK_CANDIDATE_FAIL=0
  export MOCK_ROUTE_ACK_FAIL=0
  export MOCK_ACTIVE_AUTH=0
  export DBTOOL_IMAGE_REF="ghcr.io/test/dbtool@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
  unset ROUTE_ACK_URL || true
  unset ROUTE_ACK_TIMEOUT || true
  unset READY_TIMEOUT || true
  unset SOAK_DURATION_SEC || true
  mkdir -p "$test_dir/bin" "$test_dir/secrets" "$test_dir/data/backups" "$test_dir/dynamic" "$test_dir/failover"
  touch "$test_dir/data/gateway.db"

  # Canonical production env file and release env file
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

  # Default secret files
  printf 'mock-master-key\n' > "$test_dir/secrets/app_master_key"
  printf 'mock-tts-token\n' > "$test_dir/secrets/tts_internal_token"
  printf 'mock-worker-token\n' > "$test_dir/secrets/worker_internal_token"
  printf 'mock-bark-user\n' > "$test_dir/secrets/bark_basic_auth_user"
  printf 'mock-bark-pass\n' > "$test_dir/secrets/bark_basic_auth_password"

  # Default initial active route (pointing to blue)
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
  if [[ "$sub" == "inspect" ]]; then
    vol="${3:-}"
    if [[ "${MOCK_VOLUME_MISSING:-0}" == "1" ]]; then
      exit 1
    fi
    exit 0
  elif [[ "$sub" == "ls" ]]; then
    if [[ "${MOCK_VOLUME_AMBIGUOUS:-0}" == "1" ]]; then
      printf 'bank-event-gateway_gateway_data\nbank-event-gateway_gateway_data_ambiguous\n'
      exit 0
    fi
    for a in "$@"; do
      if [[ "$a" =~ ^name= ]]; then
        printf '%s\n' "${a#name=}"
        exit 0
      fi
    done
    printf 'bank-event-gateway_gateway_data\n'
    exit 0
  fi
elif [[ "$cmd" == "inspect" ]]; then
  container="${@: -1}"
  if [[ "$container" =~ acb-gateway-(blue|green) ]]; then
    printf 'running\n'
    exit 0
  fi
  printf 'running\n'
  exit 0
elif [[ "$cmd" == "exec" ]]; then
  container="${2:-}"
  probe="${3:-}"
  if [[ "${MOCK_CANDIDATE_FAIL:-0}" == "1" && "$container" == "acb-gateway-green" ]]; then
    exit 1
  fi
  exit 0
elif [[ "$cmd" == "run" ]]; then
  if [[ "$*" =~ -active-auth-count ]]; then
    if [[ "${MOCK_ACTIVE_AUTH_FAIL:-0}" == "1" ]]; then
      exit 1
    elif [[ "${MOCK_ACTIVE_AUTH_CORRUPT:-0}" == "1" ]]; then
      printf 'invalid-non-json-output\n'
      exit 0
    elif [[ "${MOCK_ACTIVE_AUTH:-0}" == "1" ]]; then
      printf '{"activeCount":1}\n'
    else
      printf '{"activeCount":0}\n'
    fi
    exit 0
  fi
  if [[ "${MOCK_MIGRATION_FAIL:-0}" == "1" && "$*" =~ -migrate ]]; then
    exit 1
  fi
  backup_host_dir=""
  for arg in "$@"; do
    if [[ "$arg" =~ :/backup ]]; then
      backup_host_dir="$(printf '%s' "$arg" | cut -d: -f1)"
    fi
  done
  if [[ -z "$backup_host_dir" ]]; then
    backup_host_dir="${BACKUP_DIR:-$STATE_DIR/data/backups}"
  fi
  next_is_target=0
  for a in "$@"; do
    if [[ "$next_is_target" -eq 1 ]]; then
      backup_filename="$(basename "$a")"
      target_path="$backup_host_dir/$backup_filename"
      mkdir -p "$(dirname "$target_path")" 2>/dev/null || true
      printf 'SQLite format 3\n' > "$target_path"
      break
    fi
    if [[ "$a" == "-backup-to" ]]; then
      next_is_target=1
    fi
  done
  exit 0
elif [[ "$cmd" == "compose" ]]; then
  action="${@: -1}"
  if [[ "$action" == "stop" || "$action" =~ stop ]]; then
    slot="${@: -1}"
    printf 'stopped %s\n' "$slot" > "$STATE_DIR/last_stopped"
    exit 0
  fi
  exit 0
fi
exit 0
EOF
  chmod +x "$test_dir/bin/docker"

  # Mock curl CLI
  cat <<'EOF' > "$test_dir/bin/curl"
#!/usr/bin/env bash
set -eu
if [[ "${MOCK_ROUTE_ACK_FAIL:-0}" == "1" ]]; then
  exit 1
fi

hdr_file=""
output_code=0
for ((i=1; i<=$#; i++)); do
  arg="${!i}"
  if [[ "$arg" == "-D" ]]; then
    next=$((i+1))
    hdr_file="${!next}"
  elif [[ "$arg" == *"%{http_code}"* ]]; then
    output_code=1
  fi
done

active_slot="blue"
if [[ -f "${ACB_CONFIG:-}" ]]; then
  if grep -q "acb-web-green" "$ACB_CONFIG" 2>/dev/null; then
    active_slot="green"
  fi
fi

if [[ -n "$hdr_file" ]]; then
  cat <<HDR > "$hdr_file"
HTTP/1.1 200 OK
Content-Type: application/json
X-Platform-Slot: ${active_slot}
X-Release-Commit: ${EXPECTED_COMMIT:-test-commit}
HDR
fi

if [[ "$output_code" -eq 1 ]]; then
  printf '200'
else
  printf 'healthy slot: %s\n' "$active_slot"
fi
exit 0
EOF
  chmod +x "$test_dir/bin/curl"

  # Mock sqlite3 CLI
  cat <<'EOF' > "$test_dir/bin/sqlite3"
#!/usr/bin/env bash
set -eu
query="${2:-}"
if [[ "$query" =~ integrity_check ]]; then
  printf 'ok\n'
elif [[ "$query" =~ schema_migrations ]]; then
  printf '12\n'
elif [[ "$query" =~ auth_attempts ]]; then
  if [[ "${MOCK_ACTIVE_AUTH:-0}" == "1" ]]; then
    printf '1\n'
  else
    printf '0\n'
  fi
elif [[ "$query" =~ .backup ]]; then
  backup_file="$(printf '%s' "$query" | cut -d"'" -f2)"
  mkdir -p "$(dirname "$backup_file")" 2>/dev/null || true
  printf 'SQLite format 3\n' > "$backup_file"
else
  printf '0\n'
fi
exit 0
EOF
  chmod +x "$test_dir/bin/sqlite3"

  # Mock age CLI
  cat <<'EOF' > "$test_dir/bin/age"
#!/usr/bin/env bash
set -eu
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
  printf 'age-encryption.org/v1\n' > "$out"
fi
exit 0
EOF
  chmod +x "$test_dir/bin/age"

  export BACKUP_AGE_RECIPIENT="age1mockrecipienttest000000000000000000000000000000000000000000"
}

# ==============================================================================
# TEST 1: Unchanged active on failed preflight (volume missing)
# ==============================================================================
printf '\n=== TEST 1: Unchanged active on failed preflight ===\n'
T1="$TEST_TMP/t1"
setup_mock_env "$T1"
export PATH="$T1/bin:$PATH"
export MOCK_STATE_DIR="$T1"
export MOCK_VOLUME_MISSING=1

set +e
(
  export ACTIVE_SLOT_FILE="$T1/.active-slot"
  export PREVIOUS_SLOT_FILE="$T1/.previous-slot"
  export TRAEFIK_DYNAMIC_DIR="$T1/dynamic"
  export ACB_CONFIG="$T1/dynamic/acb.yml"
  export SECRETS_DIR="$T1/secrets"
  export FAILOVER_STATE_DIR="$T1/failover"
  export DEPLOY_LOCK_FILE="$T1/.deploy.lock"
  export DATA_VOLUME_NAME="bank-event-gateway_gateway_data"
  "$DEPLOY_DIR/deploy-warm.sh" "ghcr.io/test/gateway@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)
exit_code=$?
set -e

assert_eq "1" "$(( exit_code != 0 ? 1 : 0 ))" "Deploy aborted on missing volume preflight"
assert_eq "blue" "$(cat "$T1/.active-slot")" "Active slot remains blue"
assert_file_contains "$T1/dynamic/acb.yml" "acb-web-blue" "Traefik route remains pointing to blue"

# ==============================================================================
# TEST 2: Unchanged active on failed preflight (ambiguous volume)
# ==============================================================================
printf '\n=== TEST 2: Unchanged active on ambiguous volume ===\n'
T2="$TEST_TMP/t2"
setup_mock_env "$T2"
export MOCK_VOLUME_MISSING=0
export MOCK_VOLUME_AMBIGUOUS=1

set +e
(
  export ACTIVE_SLOT_FILE="$T2/.active-slot"
  export PREVIOUS_SLOT_FILE="$T2/.previous-slot"
  export TRAEFIK_DYNAMIC_DIR="$T2/dynamic"
  export ACB_CONFIG="$T2/dynamic/acb.yml"
  export SECRETS_DIR="$T2/secrets"
  export FAILOVER_STATE_DIR="$T2/failover"
  export DEPLOY_LOCK_FILE="$T2/.deploy.lock"
  "$DEPLOY_DIR/deploy-warm.sh" "ghcr.io/test/gateway@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)
exit_code=$?
set -e

assert_eq "1" "$(( exit_code != 0 ? 1 : 0 ))" "Deploy aborted on ambiguous volume"
assert_eq "blue" "$(cat "$T2/.active-slot")" "Active slot remains blue"

# ==============================================================================
# TEST 3: Unchanged active on failed migration, NO auto restore
# ==============================================================================
printf '\n=== TEST 3: Unchanged active on failed migration, NO auto restore ===\n'
T3="$TEST_TMP/t3"
setup_mock_env "$T3"
export MOCK_VOLUME_MISSING=0
export MOCK_VOLUME_AMBIGUOUS=0
export MOCK_MIGRATION_FAIL=1

set +e
(
  export ACTIVE_SLOT_FILE="$T3/.active-slot"
  export PREVIOUS_SLOT_FILE="$T3/.previous-slot"
  export TRAEFIK_DYNAMIC_DIR="$T3/dynamic"
  export ACB_CONFIG="$T3/dynamic/acb.yml"
  export SECRETS_DIR="$T3/secrets"
  export BACKUP_DIR="$T3/data/backups"
  export FAILOVER_STATE_DIR="$T3/failover"
  export DEPLOY_LOCK_FILE="$T3/.deploy.lock"
  "$DEPLOY_DIR/deploy-warm.sh" "ghcr.io/test/gateway@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)
exit_code=$?
set -e

assert_eq "1" "$(( exit_code != 0 ? 1 : 0 ))" "Deploy aborted on migration failure"
assert_eq "blue" "$(cat "$T3/.active-slot")" "Active slot remains blue after migration failure"
assert_file_contains "$T3/dynamic/acb.yml" "acb-web-blue" "Route unchanged after migration failure"

# ==============================================================================
# TEST 4: Unchanged active on failed candidate readiness
# ==============================================================================
printf '\n=== TEST 4: Unchanged active on failed candidate readiness ===\n'
T4="$TEST_TMP/t4"
setup_mock_env "$T4"
export MOCK_MIGRATION_FAIL=0
export MOCK_CANDIDATE_FAIL=1

set +e
(
  export ACTIVE_SLOT_FILE="$T4/.active-slot"
  export PREVIOUS_SLOT_FILE="$T4/.previous-slot"
  export TRAEFIK_DYNAMIC_DIR="$T4/dynamic"
  export ACB_CONFIG="$T4/dynamic/acb.yml"
  export SECRETS_DIR="$T4/secrets"
  export BACKUP_DIR="$T4/data/backups"
  export FAILOVER_STATE_DIR="$T4/failover"
  export DEPLOY_LOCK_FILE="$T4/.deploy.lock"
  export READY_TIMEOUT=2
  "$DEPLOY_DIR/deploy-warm.sh" "ghcr.io/test/gateway@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)
exit_code=$?
set -e

assert_eq "1" "$(( exit_code != 0 ? 1 : 0 ))" "Deploy aborted when candidate failed readiness"
assert_eq "blue" "$(cat "$T4/.active-slot")" "Active slot remains blue after candidate failure"
assert_file_contains "$T4/dynamic/acb.yml" "acb-web-blue" "Route pointer was NOT switched"

# ==============================================================================
# TEST 5: Automatic route rollback on failed route ACK
# ==============================================================================
printf '\n=== TEST 5: Automatic route rollback on failed route ACK ===\n'
T5="$TEST_TMP/t5"
setup_mock_env "$T5"
export MOCK_CANDIDATE_FAIL=0
export MOCK_ROUTE_ACK_FAIL=1

set +e
(
  export ACTIVE_SLOT_FILE="$T5/.active-slot"
  export PREVIOUS_SLOT_FILE="$T5/.previous-slot"
  export TRAEFIK_DYNAMIC_DIR="$T5/dynamic"
  export ACB_CONFIG="$T5/dynamic/acb.yml"
  export SECRETS_DIR="$T5/secrets"
  export BACKUP_DIR="$T5/data/backups"
  export FAILOVER_STATE_DIR="$T5/failover"
  export DEPLOY_LOCK_FILE="$T5/.deploy.lock"
  export ROUTE_ACK_URL="http://127.0.0.1:8090/healthz"
  export ROUTE_ACK_TIMEOUT=1
  "$DEPLOY_DIR/deploy-warm.sh" "ghcr.io/test/gateway@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)
exit_code=$?
set -e

assert_eq "1" "$(( exit_code != 0 ? 1 : 0 ))" "Deploy failed when route ACK failed"
assert_file_contains "$T5/dynamic/acb.yml" "acb-web-blue" "Route was rolled back to blue after ACK failure"

# ==============================================================================
# TEST 6: Successful cutover, manual stopped standby with intentional stop marker
# ==============================================================================
printf '\n=== TEST 6: Successful cutover, manual stopped standby ===\n'
T6="$TEST_TMP/t6"
setup_mock_env "$T6"
export MOCK_ROUTE_ACK_FAIL=0

set +e
(
  export ACTIVE_SLOT_FILE="$T6/.active-slot"
  export PREVIOUS_SLOT_FILE="$T6/.previous-slot"
  export TRAEFIK_DYNAMIC_DIR="$T6/dynamic"
  export ACB_CONFIG="$T6/dynamic/acb.yml"
  export SECRETS_DIR="$T6/secrets"
  export BACKUP_DIR="$T6/data/backups"
  export FAILOVER_STATE_DIR="$T6/failover"
  export DEPLOY_LOCK_FILE="$T6/.deploy.lock"
  export SOAK_DURATION_SEC=1
  "$DEPLOY_DIR/deploy-warm.sh" "ghcr.io/test/gateway@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)
exit_code=$?
set -e

assert_eq "0" "$exit_code" "Deploy succeeded"
assert_eq "green" "$(cat "$T6/.active-slot")" "Active slot successfully switched to green"
assert_file_contains "$T6/dynamic/acb.yml" "acb-web-green" "Traefik route updated to green"
# Verify intentional stop marker was created during stop
assert_file_exists "$T6/failover/acb.cooldown" "Cooldown marker created for controller handshake"

# ==============================================================================
# TEST 7: Paths with spaces
# ==============================================================================
printf '\n=== TEST 7: Scripts execute cleanly in paths with spaces ===\n'
T7="$TEST_TMP/dir with spaces in path"
setup_mock_env "$T7"
mkdir -p "$T7/deploy"
cp -p "$DEPLOY_DIR/"*.sh "$T7/deploy/"
cp -rp "$DEPLOY_DIR/lib" "$T7/deploy/"

set +e
(
  export PATH="$T7/bin:$PATH"
  export MOCK_STATE_DIR="$T7"
  export ACTIVE_SLOT_FILE="$T7/.active-slot"
  export PREVIOUS_SLOT_FILE="$T7/.previous-slot"
  export TRAEFIK_DYNAMIC_DIR="$T7/dynamic"
  export ACB_CONFIG="$T7/dynamic/acb.yml"
  export SECRETS_DIR="$T7/secrets"
  export BACKUP_DIR="$T7/data/backups"
  export FAILOVER_STATE_DIR="$T7/failover"
  export DEPLOY_LOCK_FILE="$T7/.deploy.lock"
  export SOAK_DURATION_SEC=1
  "$T7/deploy/deploy-warm.sh" "ghcr.io/test/gateway@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)
exit_code=$?
set -e

assert_eq "0" "$exit_code" "Deployment ran successfully in directory with spaces"

# ==============================================================================
# TEST 8: Core auth gate aborts upgrade when auth sessions active
# ==============================================================================
printf '\n=== TEST 8: Core auth gate aborts when session in-flight ===\n'
T8="$TEST_TMP/t8"
setup_mock_env "$T8"
export MOCK_ACTIVE_AUTH=1

set +e
(
  export ACTIVE_SLOT_FILE="$T8/.active-slot"
  export PREVIOUS_SLOT_FILE="$T8/.previous-slot"
  export TRAEFIK_DYNAMIC_DIR="$T8/dynamic"
  export ACB_CONFIG="$T8/dynamic/acb.yml"
  export SECRETS_DIR="$T8/secrets"
  export BACKUP_DIR="$T8/data/backups"
  export FAILOVER_STATE_DIR="$T8/failover"
  export DEPLOY_LOCK_FILE="$T8/.deploy.lock"
  export SOAK_DURATION_SEC=1
  "$DEPLOY_DIR/deploy-warm.sh" --upgrade-core "ghcr.io/test/gateway@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)
exit_code=$?
set -e

assert_eq "1" "$(( exit_code != 0 ? 1 : 0 ))" "Core upgrade aborted by active-auth gate"
assert_eq "blue" "$(cat "$T8/.active-slot")" "Active slot remains blue"

# TEST 8B: Fail-closed when dbtool exits 1 (previously converted to 0)
printf '\n=== TEST 8B: Fail-closed on dbtool exit code 1 ===\n'
set +e
(
  export PATH="$T8/bin:$PATH"
  export MOCK_ACTIVE_AUTH_FAIL=1
  # shellcheck source=deploy/lib.sh
  source "$DEPLOY_DIR/lib.sh"
  check_active_auth_gate >/dev/null 2>&1
)
res=$?
set -e
assert_eq "1" "$(( res != 0 ? 1 : 0 ))" "Active auth gate fails closed when dbtool exits 1"

# TEST 8C: Fail-closed when dbtool returns invalid non-JSON output
printf '\n=== TEST 8C: Fail-closed on invalid JSON ===\n'
set +e
(
  export PATH="$T8/bin:$PATH"
  export MOCK_ACTIVE_AUTH_CORRUPT=1
  # shellcheck source=deploy/lib.sh
  source "$DEPLOY_DIR/lib.sh"
  check_active_auth_gate >/dev/null 2>&1
)
res=$?
set -e
assert_eq "1" "$(( res != 0 ? 1 : 0 ))" "Active auth gate fails closed on corrupt/invalid JSON"

# TEST 8D: Read-only WAL probe verification
printf '\n=== TEST 8D: Read-only WAL probe verification ===\n'
set +e
(
  export PATH="$T8/bin:$PATH"
  # shellcheck source=deploy/lib.sh
  source "$DEPLOY_DIR/lib.sh"
  verify_wal_probe "bank-event-gateway_gateway_data" "$DBTOOL_IMAGE_REF"
)
res=$?
set -e
assert_eq "0" "$res" "Read-only WAL probe passes with valid DB"

# ==============================================================================
# TEST 9: Deployment aborts if canonical deploy/.env.production is missing
# ==============================================================================
printf '\n=== TEST 9: Missing canonical .env.production aborts deployment ===\n'
T9="$TEST_TMP/t9"
setup_mock_env "$T9"
rm -f "$T9/.env.production"

set +e
(
  export ACTIVE_SLOT_FILE="$T9/.active-slot"
  export PREVIOUS_SLOT_FILE="$T9/.previous-slot"
  export TRAEFIK_DYNAMIC_DIR="$T9/dynamic"
  export ACB_CONFIG="$T9/dynamic/acb.yml"
  export SECRETS_DIR="$T9/secrets"
  export ENV_FILE="$T9/.env.production"
  "$DEPLOY_DIR/deploy-warm.sh" "ghcr.io/test/gateway@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)
exit_code=$?
set -e

assert_eq "1" "$(( exit_code != 0 ? 1 : 0 ))" "Deployment aborted when canonical env file was missing"

# ==============================================================================
# TEST 10: Root-level .env.production is not read or rewritten
# ==============================================================================
printf '\n=== TEST 10: No root-level .env.production mutation ===\n'
T10="$TEST_TMP/t10"
setup_mock_env "$T10"
mkdir -p "$T10/deploy"
cp -p "$DEPLOY_DIR/"*.sh "$T10/deploy/"
cp -rp "$DEPLOY_DIR/lib" "$T10/deploy/"
cp -p "$T10/.env.production" "$T10/deploy/.env.production"
printf 'ROOT_ENV_SENTINEL=original\n' > "$T10/.env.production"

set +e
(
  export PATH="$T10/bin:$PATH"
  export MOCK_STATE_DIR="$T10"
  export ACTIVE_SLOT_FILE="$T10/deploy/.active-slot"
  export PREVIOUS_SLOT_FILE="$T10/deploy/.previous-slot"
  export TRAEFIK_DYNAMIC_DIR="$T10/dynamic"
  export ACB_CONFIG="$T10/dynamic/acb.yml"
  export SECRETS_DIR="$T10/secrets"
  export BACKUP_DIR="$T10/data/backups"
  export FAILOVER_STATE_DIR="$T10/failover"
  export DEPLOY_LOCK_FILE="$T10/.deploy.lock"
  export SOAK_DURATION_SEC=1
  export ENV_FILE="$T10/deploy/.env.production"
  export RELEASE_ENV_FILE="$T10/.release.env"
  "$T10/deploy/deploy-warm.sh" "ghcr.io/test/gateway@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)
exit_code=$?
set -e

assert_eq "0" "$exit_code" "Deployment succeeded using deploy/.env.production"
assert_file_contains "$T10/.env.production" "ROOT_ENV_SENTINEL=original" "Root-level .env.production was untouched"

printf '\n==================================================\n'
printf 'TEST RESULTS: %d PASSED, %d FAILED\n' "$TESTS_PASSED" "$TESTS_FAILED"
printf '==================================================\n'

if [[ "$TESTS_FAILED" -gt 0 ]]; then
  exit 1
fi
exit 0
