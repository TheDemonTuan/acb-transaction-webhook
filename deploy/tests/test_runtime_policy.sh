#!/usr/bin/env bash
# Unit tests for Compose runtime isolation and security policy verifier
set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR="$(cd -- "$SCRIPT_DIR/.." && pwd)"
VERIFY_SCRIPT="$DEPLOY_DIR/verify-compose-runtime.sh"
PROD_COMPOSE="$DEPLOY_DIR/compose.prod.yaml"

TEST_TMP="$(mktemp -d "${TMPDIR:-/tmp}/acb-runtime-policy-tests.XXXXXX")"
trap 'rm -rf "$TEST_TMP"' EXIT

TESTS_PASSED=0
TESTS_FAILED=0

assert_success() {
  local desc="$1"
  shift
  if "$@" >/dev/null 2>&1; then
    printf '  [PASS] %s\n' "$desc"
    TESTS_PASSED=$((TESTS_PASSED + 1))
  else
    printf '  [FAIL] %s (expected success, got exit code %d)\n' "$desc" "$?" >&2
    TESTS_FAILED=$((TESTS_FAILED + 1))
  fi
}

assert_failure() {
  local desc="$1"
  shift
  if "$@" >/dev/null 2>&1; then
    printf '  [FAIL] %s (expected failure, but succeeded)\n' "$desc" >&2
    TESTS_FAILED=$((TESTS_FAILED + 1))
  else
    printf '  [PASS] %s\n' "$desc"
    TESTS_PASSED=$((TESTS_PASSED + 1))
  fi
}

printf "========================================================\n"
printf "Running Runtime Policy Verifier Test Suite\n"
printf "========================================================\n\n"

# 1. Canonical Production Compose Must Pass All Policy Audits
printf "1. Auditing Canonical Production Compose...\n"
assert_success "Canonical deploy/compose.prod.yaml passes full runtime policy audit" \
  bash "$VERIFY_SCRIPT" --compose-file "$PROD_COMPOSE"
if grep -A 15 'container_name: acb-bark' "$PROD_COMPOSE" | grep -A 2 'group_add:' | grep -q '"1000"'; then
  printf '  [PASS] Bark receives only shared runtime group 1000 for secret reads\n'
  TESTS_PASSED=$((TESTS_PASSED + 1))
else
  printf '  [FAIL] Bark does not receive runtime group 1000\n' >&2
  TESTS_FAILED=$((TESTS_FAILED + 1))
fi

printf "\n2. Testing Negative Invariant Violations...\n"

# 2.1 Rejection of mutable image tag
T_LATEST="$TEST_TMP/compose-latest.yaml"
sed 's/WORKER_IMAGE_REF:?WORKER_IMAGE_REF is required/WORKER_IMAGE_REF:-worker:latest/' "$PROD_COMPOSE" > "$T_LATEST"
assert_failure "Rejects mutable ':latest' image tag" \
  bash "$VERIFY_SCRIPT" --compose-file "$T_LATEST"

# 2.2 Rejection of image fallback pattern
T_FALLBACK="$TEST_TMP/compose-fallback.yaml"
sed 's/WORKER_IMAGE_REF:?WORKER_IMAGE_REF is required/WORKER_IMAGE_REF:-worker@sha256:1111111111111111111111111111111111111111111111111111111111111111/' "$PROD_COMPOSE" > "$T_FALLBACK"
assert_failure "Rejects image default fallback ':-' expression" \
  bash "$VERIFY_SCRIPT" --compose-file "$T_FALLBACK"

# 2.3 Rejection of published host ports
T_PORTS="$TEST_TMP/compose-ports.yaml"
sed 's/expose:/ports:\n      - "8190:8190"\n    expose:/' "$PROD_COMPOSE" > "$T_PORTS"
assert_failure "Rejects host port publication (ports: directive)" \
  bash "$VERIFY_SCRIPT" --compose-file "$T_PORTS"

# 2.4 Rejection of source build directive
T_BUILD="$TEST_TMP/compose-build.yaml"
sed 's/restart: unless-stopped/build: .\n    restart: unless-stopped/' "$PROD_COMPOSE" > "$T_BUILD"
assert_failure "Rejects source build block in production compose" \
  bash "$VERIFY_SCRIPT" --compose-file "$T_BUILD"

# 2.5 Rejection of docker.sock volume mount
T_SOCK="$TEST_TMP/compose-sock.yaml"
sed 's|gateway_data:/data|/var/run/docker.sock:/var/run/docker.sock|' "$PROD_COMPOSE" > "$T_SOCK"
assert_failure "Rejects Docker socket bind mount (/var/run/docker.sock)" \
  bash "$VERIFY_SCRIPT" --compose-file "$T_SOCK"

# 2.6 Rejection of host sensitive directory mount
T_ROOT="$TEST_TMP/compose-root.yaml"
sed 's|gateway_data:/data|/etc:/etc:ro|' "$PROD_COMPOSE" > "$T_ROOT"
assert_failure "Rejects host system directory mount (/etc)" \
  bash "$VERIFY_SCRIPT" --compose-file "$T_ROOT"

# 2.7 Rejection of source tree bind mount
T_SRC="$TEST_TMP/compose-src.yaml"
sed 's|gateway_data:/data|./cmd:/app/cmd|' "$PROD_COMPOSE" > "$T_SRC"
assert_failure "Rejects repository source tree bind mount (./cmd)" \
  bash "$VERIFY_SCRIPT" --compose-file "$T_SRC"

# 2.8 Rejection of read_only: false on core worker
T_NOWORKER_RO="$TEST_TMP/compose-worker-rw.yaml"
sed '0,/read_only: true/s/read_only: true/read_only: false/' "$PROD_COMPOSE" > "$T_NOWORKER_RO"
assert_failure "Rejects writable rootfs on core worker" \
  bash "$VERIFY_SCRIPT" --compose-file "$T_NOWORKER_RO"

# 2.9 Rejection of root user execution on gateway
T_ROOT_USER="$TEST_TMP/compose-root-user.yaml"
sed 's/user: "1000:1000"/user: "0:0"/' "$PROD_COMPOSE" > "$T_ROOT_USER"
assert_failure "Rejects root user (0:0) on first-party gateway" \
  bash "$VERIFY_SCRIPT" --compose-file "$T_ROOT_USER"

# 2.10 Rejection of omitted capability drop
T_NOCAP="$TEST_TMP/compose-nocap.yaml"
sed 's/cap_drop: \[ALL\]/cap_drop: []/' "$PROD_COMPOSE" > "$T_NOCAP"
assert_failure "Rejects service omitting cap_drop: [ALL]" \
  bash "$VERIFY_SCRIPT" --compose-file "$T_NOCAP"

# 2.11 Rejection of disabled no-new-privileges
T_NOPRIV="$TEST_TMP/compose-nopriv.yaml"
sed 's/no-new-privileges:true/no-new-privileges:false/' "$PROD_COMPOSE" > "$T_NOPRIV"
assert_failure "Rejects service with disabled no-new-privileges" \
  bash "$VERIFY_SCRIPT" --compose-file "$T_NOPRIV"

# 2.12 Rejection of worker joining edge-acb network
T_WORKER_EDGE="$TEST_TMP/compose-worker-edge.yaml"
sed '0,/acb-core:/s/acb-core:/edge-acb:\n      acb-core:/' "$PROD_COMPOSE" > "$T_WORKER_EDGE"
assert_failure "Rejects worker connecting to edge-acb network" \
  bash "$VERIFY_SCRIPT" --compose-file "$T_WORKER_EDGE"

# 2.13 Rejection of dbtool joining core network
T_DBTOOL_CORE="$TEST_TMP/compose-dbtool-core.yaml"
sed 's/- none/- acb-core/' "$PROD_COMPOSE" > "$T_DBTOOL_CORE"
assert_failure "Rejects dbtool connecting to non-isolated core network" \
  bash "$VERIFY_SCRIPT" --compose-file "$T_DBTOOL_CORE"

# 2.14 Rejection of omitted resource limits
T_NOLIMITS="$TEST_TMP/compose-nolimits.yaml"
sed 's/cpus: "0.75"//' "$PROD_COMPOSE" > "$T_NOLIMITS"
assert_failure "Rejects service omitting service-level resource limits" \
  bash "$VERIFY_SCRIPT" --compose-file "$T_NOLIMITS"

printf "\n========================================================\n"
printf "Runtime Policy Test Results: %d Passed, %d Failed\n" "$TESTS_PASSED" "$TESTS_FAILED"
printf "========================================================\n"

if [[ "$TESTS_FAILED" -gt 0 ]]; then
  exit 1
fi
exit 0
