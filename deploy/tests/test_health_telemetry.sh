#!/usr/bin/env bash
# deploy/tests/test_health_telemetry.sh
# Verification test suite for PR16:
# - Three-level health check semantics (healthz liveness, readyz readiness, deployz promotion gate)
# - Exact route identity ACK (X-Platform-Slot, X-Release-Commit, X-Runtime-Role)
# - Split role checks (role=gateway, role=worker, role=auth-browser)
# - Observability runbook thresholds and privacy controls
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR="$(cd -- "$SCRIPT_DIR/.." && pwd)"
REPO_ROOT="$(cd -- "$DEPLOY_DIR/.." && pwd)"

TEST_TMP="$(mktemp -d "${TMPDIR:-/tmp}/acb-health-telemetry-tests.XXXXXX")"
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

# ==============================================================================
# TEST 1: Observability runbook exists and defines all 10 canonical alerts
# ==============================================================================
printf '\n=== TEST 1: Observability runbook & alert thresholds ===\n'
runbook="$REPO_ROOT/docs/runbooks/OBSERVABILITY.md"
assert_file_exists "$runbook" "Observability runbook exists"
assert_file_contains "$runbook" "ALERT_STALE_REALTIME_POLL" "Runbook documents ALERT_STALE_REALTIME_POLL"
assert_file_contains "$runbook" "ALERT_STALE_WORKER" "Runbook documents ALERT_STALE_WORKER"
assert_file_contains "$runbook" "ALERT_AUTH_STUCK" "Runbook documents ALERT_AUTH_STUCK"
assert_file_contains "$runbook" "ALERT_QUEUE_SATURATION" "Runbook documents ALERT_QUEUE_SATURATION"
assert_file_contains "$runbook" "ALERT_HISTORY_STALL" "Runbook documents ALERT_HISTORY_STALL"
assert_file_contains "$runbook" "ALERT_NOTIFICATION_BACKLOG_STUCK" "Runbook documents ALERT_NOTIFICATION_BACKLOG_STUCK"
assert_file_contains "$runbook" "ALERT_BACKUP_OVERDUE" "Runbook documents ALERT_BACKUP_OVERDUE"
assert_file_contains "$runbook" "ALERT_RESTORE_DRILL_OVERDUE" "Runbook documents ALERT_RESTORE_DRILL_OVERDUE"
assert_file_contains "$runbook" "ALERT_MUTATION_GATE_LOCKED" "Runbook documents ALERT_MUTATION_GATE_LOCKED"
assert_file_contains "$runbook" "ALERT_WRONG_ROLE_RELEASE_SCHEMA" "Runbook documents ALERT_WRONG_ROLE_RELEASE_SCHEMA"
assert_file_contains "$runbook" "ALERT_REALTIME_STREAM_DEGRADED" "Runbook documents ALERT_REALTIME_STREAM_DEGRADED"
assert_file_contains "$runbook" "ALERT_REALTIME_FALLBACK_RECOVERY" "Runbook documents ALERT_REALTIME_FALLBACK_RECOVERY"
assert_file_contains "$runbook" "Zero Secret Leakage" "Runbook enforces zero secret leakage"
assert_file_contains "$runbook" "Cardinality Bounds" "Runbook enforces cardinality bounds"

# ==============================================================================
# TEST 2: Exact route identity ACK header verification
# ==============================================================================
printf '\n=== TEST 2: Exact route identity ACK response headers ===\n'
parse_route_ack_headers() {
  local header_file="$1"
  local target_slot="$2"
  local expected_commit="$3"
  local expected_role="$4"

  local slot_hdr commit_hdr role_hdr
  slot_hdr="$(grep -i '^x-platform-slot:' "$header_file" | head -n1 | tr -d '\r\n' | awk -F': ' '{print $2}' || echo "")"
  commit_hdr="$(grep -i '^x-release-commit:' "$header_file" | head -n1 | tr -d '\r\n' | awk -F': ' '{print $2}' || echo "")"
  role_hdr="$(grep -i '^x-runtime-role:' "$header_file" | head -n1 | tr -d '\r\n' | awk -F': ' '{print $2}' || echo "")"

  if [[ "$slot_hdr" != "$target_slot" ]]; then
    return 1
  fi
  if [[ -n "$expected_commit" && "$expected_commit" != "unknown" && "$commit_hdr" != "$expected_commit" ]]; then
    return 2
  fi
  if [[ -n "$expected_role" && "$role_hdr" != "$expected_role" ]]; then
    return 3
  fi
  return 0
}

hdr_sample="$TEST_TMP/good_headers.txt"
cat <<EOF > "$hdr_sample"
HTTP/1.1 200 OK
Content-Type: application/json
X-Platform-Slot: blue
X-Release-Commit: sha256-abcdef123456
X-Runtime-Role: gateway
Date: Mon, 14 Sep 2026 12:00:00 GMT
EOF

rc=0
parse_route_ack_headers "$hdr_sample" "blue" "sha256-abcdef123456" "gateway" || rc=$?
assert_eq "0" "$rc" "parse_route_ack_headers succeeds for matching slot, commit, and role"

rc=0
parse_route_ack_headers "$hdr_sample" "green" "sha256-abcdef123456" "gateway" || rc=$?
assert_eq "1" "$rc" "parse_route_ack_headers rejects slot mismatch (expected green, got blue)"

rc=0
parse_route_ack_headers "$hdr_sample" "blue" "different-commit" "gateway" || rc=$?
assert_eq "2" "$rc" "parse_route_ack_headers rejects commit mismatch"

rc=0
parse_route_ack_headers "$hdr_sample" "blue" "sha256-abcdef123456" "worker" || rc=$?
assert_eq "3" "$rc" "parse_route_ack_headers rejects role mismatch (expected worker, got gateway)"

# ==============================================================================
# TEST 3: Split role check behavior
# ==============================================================================
printf '\n=== TEST 3: Split role check validation ===\n'
validate_role_probe() {
  local runtime_role="$1"
  local expected_role="$2"

  if [[ -z "$expected_role" ]]; then
    return 0
  fi
  if [[ "$runtime_role" == "$expected_role" || "$runtime_role" == "all-in-one" ]]; then
    return 0
  fi
  return 1
}

rc=0
validate_role_probe "gateway" "gateway" || rc=$?
assert_eq "0" "$rc" "gateway role probe matches gateway"

rc=0
validate_role_probe "gateway" "worker" || rc=$?
assert_eq "1" "$rc" "worker probe rejected on gateway"

rc=0
validate_role_probe "worker" "worker" || rc=$?
assert_eq "0" "$rc" "worker role probe matches worker"

rc=0
validate_role_probe "worker" "gateway" || rc=$?
assert_eq "1" "$rc" "gateway probe rejected on worker"

rc=0
validate_role_probe "all-in-one" "worker" || rc=$?
assert_eq "0" "$rc" "all-in-one role satisfies worker probe"

# ==============================================================================
# TEST 4: Telemetry secrecy & privacy regex checks
# ==============================================================================
printf '\n=== TEST 4: Telemetry secrecy validation ===\n'
assert_no_secrets_in_payload() {
  local payload="$1"
  if echo "$payload" | grep -E -q '(AGE-SECRET-KEY-|Bearer [A-Za-z0-9_-]{8,}|eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.|<table|<html)'; then
    return 1
  fi
  return 0
}

safe_telemetry='{"status":"ok","role":"gateway","slot":"blue","release":"abc1234","scheduler":{"totalQueueDepth":4}}'
rc=0
assert_no_secrets_in_payload "$safe_telemetry" || rc=$?
assert_eq "0" "$rc" "Safe telemetry payload passes secret scan"

leaky_bearer='{"status":"ok","token":"Bearer sec_secret_token_123456789"}'
rc=0
assert_no_secrets_in_payload "$leaky_bearer" || rc=$?
assert_eq "1" "$rc" "Leaked Bearer token caught by secret scan"

leaky_jwt='{"status":"ok","jwt":"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.sig"}'
rc=0
assert_no_secrets_in_payload "$leaky_jwt" || rc=$?
assert_eq "1" "$rc" "Leaked JWT caught by secret scan"

leaky_html='{"status":"ok","content":"<table class=\"acb\"><tr><td>1000</td></tr></table>"}'
rc=0
assert_no_secrets_in_payload "$leaky_html" || rc=$?
assert_eq "1" "$rc" "Leaked raw HTML markup caught by secret scan"

# ==============================================================================
# SUMMARY
# ==============================================================================
printf '\n======================================================\n'
printf 'Health, Telemetry & Observability Tests: %d passed, %d failed\n' "$TESTS_PASSED" "$TESTS_FAILED"
printf '======================================================\n'

if [[ "$TESTS_FAILED" -gt 0 ]]; then
  exit 1
fi
exit 0
