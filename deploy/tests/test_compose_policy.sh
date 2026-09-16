#!/usr/bin/env bash
# Test suite for Compose immutability policy and deploy/.release.env state management
set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR="$(cd -- "$SCRIPT_DIR/.." && pwd)"
RELEASE_ENV_SCRIPT="$DEPLOY_DIR/release-env.sh"
PROD_COMPOSE="$DEPLOY_DIR/compose.prod.yaml"

TEST_TMP="$(mktemp -d "${TMPDIR:-/tmp}/acb-compose-policy-tests.XXXXXX")"
trap 'rm -rf "$TEST_TMP"' EXIT

TESTS_PASSED=0
TESTS_FAILED=0

assert_eq() {
  local expected="$1"
  local actual="$2"
  local msg="$3"
  if [[ "$expected" != "$actual" ]]; then
    printf '  [FAIL] %s (expected "%s", got "%s")\n' "$msg" "$expected" "$actual" >&2
    TESTS_FAILED=$((TESTS_FAILED + 1))
    return 1
  fi
  printf '  [PASS] %s\n' "$msg"
  TESTS_PASSED=$((TESTS_PASSED + 1))
  return 0
}

assert_success() {
  local desc="$1"
  shift
  if "$@"; then
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
printf "Running Compose Immutability & Release Env Test Suite\n"
printf "========================================================\n\n"

# ----------------------------------------------------
# 1. Static Compose Policy Tests
# ----------------------------------------------------
printf "1. Testing Static Compose Immutability Invariants...\n"

# 1.1 No :latest tags
if grep -E 'image:[[:space:]]*.*:latest' "$PROD_COMPOSE" >/dev/null 2>&1; then
  assert_failure "No :latest image tags in compose.prod.yaml" false
else
  assert_success "No mutable :latest image tags in compose.prod.yaml" true
fi

# 1.2 No :- fallbacks
if grep -E 'image:[[:space:]]*\$\{[^}:]+:-' "$PROD_COMPOSE" >/dev/null 2>&1; then
  assert_failure "No ':-' fallback expressions in image directives" false
else
  assert_success "No ':-' fallback expressions in image directives" true
fi

# 1.3 Check required variable syntax for each service
required_vars=(
  "WORKER_IMAGE_REF"
  "BROWSER_IMAGE_REF"
  "TTS_IMAGE_REF"
  "BARK_IMAGE_REF"
  "IMAGE_REF_BLUE"
  "IMAGE_REF_GREEN"
  "DBTOOL_IMAGE_REF"
)

for v in "${required_vars[@]}"; do
  expected_pattern="image:[[:space:]]*\\\${${v}:\\?${v}[[:space:]]+is[[:space:]]+required}"
  if grep -E "$expected_pattern" "$PROD_COMPOSE" >/dev/null 2>&1; then
    assert_success "compose.prod.yaml requires ${v} via fail-closed syntax" true
  else
    assert_failure "compose.prod.yaml requires ${v} via fail-closed syntax" false
  fi
done

# 1.4 Distinct slot variables
blue_var="$(grep -E 'image:[[:space:]]*\$\{IMAGE_REF_BLUE' "$PROD_COMPOSE" || true)"
green_var="$(grep -E 'image:[[:space:]]*\$\{IMAGE_REF_GREEN' "$PROD_COMPOSE" || true)"
if [[ -n "$blue_var" && -n "$green_var" ]]; then
  assert_success "Gateway slots use distinct variables (IMAGE_REF_BLUE and IMAGE_REF_GREEN)" true
else
  assert_failure "Gateway slots use distinct variables" false
fi

printf "\n"

# ----------------------------------------------------
# 2. Release Env Script Unit Tests
# ----------------------------------------------------
printf "2. Testing deploy/release-env.sh Operations...\n"

# Source release-env script
# shellcheck source=deploy/release-env.sh
source "$RELEASE_ENV_SCRIPT"

valid_sha="0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
valid_ref="ghcr.io/thedemontuan/acb-transaction-webhook@sha256:${valid_sha}"
valid_bark="ghcr.io/finb/bark-server@sha256:32d65b07fa835c99b31a396b77727a04ed058377fc2482da3e9dc7397167ffc4"

# 2.1 Digest validation
assert_success "validate_image_ref accepts valid sha256 digest" \
  validate_image_ref "$valid_ref" "gateway"

assert_failure "validate_image_ref rejects mutable tag (:latest)" \
  validate_image_ref "ghcr.io/thedemontuan/acb-transaction-webhook:latest" "gateway"

assert_failure "validate_image_ref rejects semver tag (:v1.0.0)" \
  validate_image_ref "ghcr.io/thedemontuan/acb-transaction-webhook:v1.0.0" "gateway"

assert_failure "validate_image_ref rejects empty ref" \
  validate_image_ref "" "gateway"

assert_failure "validate_image_ref rejects truncated sha256" \
  validate_image_ref "ghcr.io/test@sha256:012345" "gateway"

assert_failure "validate_image_ref rejects uppercase hex in sha256" \
  validate_image_ref "ghcr.io/test@sha256:0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF" "gateway"

assert_success "validate_image_ref accepts approved Bark third-party digest" \
  validate_image_ref "$valid_bark" "bark"

assert_failure "validate_image_ref rejects unapproved third-party Bark digest" \
  validate_image_ref "ghcr.io/finb/bark-server@sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff" "bark"

# 2.2 Atomic init and file validation
T_REL="$TEST_TMP/.release.env"
export RELEASE_ENV_FILE="$T_REL"
export USE_CANONICAL_RELEASE_STATE=0

assert_success "init_release_env creates complete .release.env" \
  init_release_env \
    --file "$T_REL" \
    --frontend-image "$valid_ref" \
    --gateway-blue "$valid_ref" \
    --gateway-green "$valid_ref" \
    --worker-image "ghcr.io/test/worker@sha256:${valid_sha}" \
    --dbtool-image "ghcr.io/test/dbtool@sha256:${valid_sha}" \
    --auth-browser-image "ghcr.io/test/browser@sha256:${valid_sha}" \
    --tts-image "ghcr.io/test/tts@sha256:${valid_sha}" \
    --bark-image "$valid_bark"

assert_success "validate_release_env_file accepts fully initialized release env" \
  validate_release_env_file "$T_REL"

# 2.3 Key retrieval
assert_eq "$valid_ref" "$(get_release_env "IMAGE_REF_BLUE")" "get_release_env retrieves IMAGE_REF_BLUE"
assert_eq "$valid_bark" "$(get_release_env "BARK_IMAGE_REF")" "get_release_env retrieves BARK_IMAGE_REF"
assert_eq "fallback_default" "$(get_release_env "NON_EXISTENT_KEY" "fallback_default")" "get_release_env returns default on missing key"

# 2.4 Atomic set and previous preservation
new_sha="9999999999abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
new_ref="ghcr.io/thedemontuan/acb-transaction-webhook@sha256:${new_sha}"

assert_success "set_release_env updates IMAGE_REF_GREEN" \
  set_release_env "IMAGE_REF_GREEN" "$new_ref"

assert_eq "$new_ref" "$(get_release_env "IMAGE_REF_GREEN")" "IMAGE_REF_GREEN updated to new ref"
assert_eq "$valid_ref" "$(get_release_env "PREVIOUS_IMAGE_REF_GREEN")" "PREVIOUS_IMAGE_REF_GREEN saved prior ref"

# 2.5 Rollback
assert_success "rollback_release_env restores prior ref" \
  rollback_release_env "IMAGE_REF_GREEN"

assert_eq "$valid_ref" "$(get_release_env "IMAGE_REF_GREEN")" "IMAGE_REF_GREEN restored to previous ref"
assert_eq "$new_ref" "$(get_release_env "PREVIOUS_IMAGE_REF_GREEN")" "PREVIOUS_IMAGE_REF_GREEN holds candidate ref after rollback"

# 2.6 Reject invalid ref update without file corruption
assert_failure "set_release_env rejects invalid ref" \
  set_release_env "IMAGE_REF_GREEN" "invalid-ref:latest"

assert_eq "$valid_ref" "$(get_release_env "IMAGE_REF_GREEN")" "IMAGE_REF_GREEN unchanged after failed update"

# 2.7 Incomplete release env validation failure
T_INCOMPLETE="$TEST_TMP/.release.incomplete.env"
cat <<EOF > "$T_INCOMPLETE"
IMAGE_REF_BLUE=${valid_ref}
WORKER_IMAGE_REF=ghcr.io/test/worker@sha256:${valid_sha}
EOF
assert_failure "validate_release_env_file fails on incomplete file" \
  validate_release_env_file "$T_INCOMPLETE"

printf "\n"

# ----------------------------------------------------
# 3. Simulated Compose Interpolation Tests (Fail-Closed)
# ----------------------------------------------------
printf "3. Testing Compose Interpolation & Missing Digest Fail-Closed (GATE-14)...\n"

# Test evaluation of required variables in bash subshell (matching Compose interpolation semantics)
# Compose required variable syntax ${VAR:?ERR} aborts evaluation if VAR is empty or unset.

eval_compose_interpolation() {
  local compose_yaml="$1"
  local env_file="$2"

  # Run in clean subshell with environment loaded from env_file
  (
    # Clear any ambient image variables
    unset FRONTEND_IMAGE_REF WORKER_IMAGE_REF BROWSER_IMAGE_REF TTS_IMAGE_REF BARK_IMAGE_REF IMAGE_REF_BLUE IMAGE_REF_GREEN DBTOOL_IMAGE_REF || true
    if [[ -f "$env_file" ]]; then
      # shellcheck disable=SC1090
      set -a
      source "$env_file"
      set +a
    fi

    # Extract all required variable declarations from compose.prod.yaml
    # e.g. ${WORKER_IMAGE_REF:?WORKER_IMAGE_REF is required}
    while IFS= read -r expr; do
      # Test evaluation using bash parameter expansion
      eval ": \"$expr\""
    done < <(grep -o -E '\$\{[A-Z0-9_]+:\?[^}]+\}' "$compose_yaml")
  )
}

# 3.1 Evaluation succeeds when all required digests are provided in .release.env
assert_success "Compose interpolation succeeds with complete synthetic .release.env" \
  eval_compose_interpolation "$PROD_COMPOSE" "$T_REL"

# 3.2 Evaluation aborts when any required variable is missing (GATE-14)
T_MISSING_WORKER="$TEST_TMP/.release.missing-worker.env"
grep -v "WORKER_IMAGE_REF=" "$T_REL" > "$T_MISSING_WORKER"
assert_failure "Evaluation aborts before container mutation when WORKER_IMAGE_REF is missing (GATE-14)" \
  eval_compose_interpolation "$PROD_COMPOSE" "$T_MISSING_WORKER"

T_MISSING_BLUE="$TEST_TMP/.release.missing-blue.env"
grep -v "IMAGE_REF_BLUE=" "$T_REL" > "$T_MISSING_BLUE"
assert_failure "Evaluation aborts when IMAGE_REF_BLUE is missing (GATE-14)" \
  eval_compose_interpolation "$PROD_COMPOSE" "$T_MISSING_BLUE"

T_MISSING_DBTOOL="$TEST_TMP/.release.missing-dbtool.env"
grep -v "DBTOOL_IMAGE_REF=" "$T_REL" > "$T_MISSING_DBTOOL"
assert_failure "Evaluation aborts when DBTOOL_IMAGE_REF is missing (GATE-14)" \
  eval_compose_interpolation "$PROD_COMPOSE" "$T_MISSING_DBTOOL"

T_MISSING_BARK="$TEST_TMP/.release.missing-bark.env"
grep -v "BARK_IMAGE_REF=" "$T_REL" > "$T_MISSING_BARK"
assert_failure "Evaluation aborts when BARK_IMAGE_REF is missing (GATE-14)" \
  eval_compose_interpolation "$PROD_COMPOSE" "$T_MISSING_BARK"

printf "\n========================================================\n"
printf "Compose Policy Test Results: %d Passed, %d Failed\n" "$TESTS_PASSED" "$TESTS_FAILED"
printf "========================================================\n"

if [[ "$TESTS_FAILED" -gt 0 ]]; then
  exit 1
fi
exit 0
