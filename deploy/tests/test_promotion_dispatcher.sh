#!/usr/bin/env bash
# deploy/tests/test_promotion_dispatcher.sh
# Test suite for minimal trusted CI promotion dispatcher and rollout orchestration.
set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR="$(cd -- "$SCRIPT_DIR/.." && pwd)"

TEST_TMP="$(mktemp -d "${TMPDIR:-/tmp}/acb-dispatcher-tests.XXXXXX")"
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

setup_dispatcher_env() {
  local tdir="$1"
  mkdir -p "$tdir/deploy" "$tdir/data" "$tdir/secrets"

  # Copy library and helper files
  cp -r "$DEPLOY_DIR/lib"* "$tdir/deploy/"
  cp "$DEPLOY_DIR/release-env.sh" "$tdir/deploy/"
  cp "$DEPLOY_DIR/verify-manifest.sh" "$tdir/deploy/"
  cp "$DEPLOY_DIR/dispatch-rollout.sh" "$tdir/deploy/"
  cp "$DEPLOY_DIR/deploy-frontend.sh" "$tdir/deploy/"
  chmod 755 "$tdir/deploy/"*.sh

  # Create canonical mock env files
  cat <<'EOF' > "$tdir/deploy/.env.production"
APP_ENV=production
DATA_DIR=/tmp/data
PUBLIC_ORIGIN=https://acb.example.com
EOF

  cat <<'EOF' > "$tdir/deploy/compose.prod.yaml"
services:
  gateway-blue:
    image: ${GATEWAY_IMAGE_REF}
  gateway-green:
    image: ${GATEWAY_IMAGE_REF}
  worker:
    image: ${WORKER_IMAGE_REF}
EOF

  # Create mock component transaction scripts that log calls into trace.log
  local trace_file="$tdir/data/trace.log"
  rm -f "$trace_file"
  touch "$trace_file"

  cat <<'EOF' > "$tdir/deploy/deploy-schema.sh"
#!/usr/bin/env bash
set -euo pipefail
printf 'SCHEMA:%s\n' "$@" >> "${TRACE_FILE}"
exit 0
EOF

  cat <<'EOF' > "$tdir/deploy/deploy-worker.sh"
#!/usr/bin/env bash
set -euo pipefail
printf 'WORKER:%s\n' "$@" >> "${TRACE_FILE}"
exit 0
EOF

  cat <<'EOF' > "$tdir/deploy/deploy-gateway.sh"
#!/usr/bin/env bash
set -euo pipefail
printf 'GATEWAY:%s\n' "$@" >> "${TRACE_FILE}"
exit 0
EOF

  cat <<'EOF' > "$tdir/deploy/deploy-frontend.sh"
#!/usr/bin/env bash
set -euo pipefail
printf 'FRONTEND:%s\n' "$@" >> "${TRACE_FILE}"
exit 0
EOF

  cat <<'EOF' > "$tdir/deploy/deploy-auth-browser.sh"
#!/usr/bin/env bash
set -euo pipefail
printf 'AUTH_BROWSER:%s\n' "$@" >> "${TRACE_FILE}"
exit 0
EOF

  cat <<'EOF' > "$tdir/deploy/deploy-tts.sh"
#!/usr/bin/env bash
set -euo pipefail
printf 'TTS:%s\n' "$@" >> "${TRACE_FILE}"
exit 0
EOF

  cat <<'EOF' > "$tdir/deploy/deploy-bark.sh"
#!/usr/bin/env bash
set -euo pipefail
printf 'BARK:%s\n' "$@" >> "${TRACE_FILE}"
exit 0
EOF

  cat <<'EOF' > "$tdir/deploy/switch-slot.sh"
#!/usr/bin/env bash
set -euo pipefail
printf 'SWITCH_SLOT:%s\n' "$@" >> "${TRACE_FILE}"
exit 0
EOF

  chmod 755 "$tdir/deploy/"*.sh
}

write_mock_manifest() {
  local path="$1"
  local git_sha="$2"
  local scope_json="$3"
  local created_at="${4:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"
  local rel_id="${5:-rel-${git_sha:0:12}-$(date +%s)}"

  cat <<EOF > "$path"
{
  "schema_version": 1,
  "release_id": "${rel_id}",
  "git_sha": "${git_sha}",
  "created_at": "${created_at}",
  "compatibility": {
    "schema_version": 1,
    "worker_rpc_version": 2
  },
  "promotion": ${scope_json},
  "images": {
    "frontend": "ghcr.io/test/frontend@sha256:7777777777777777777777777777777777777777777777777777777777777777",
    "gateway": "ghcr.io/test/gateway@sha256:1111111111111111111111111111111111111111111111111111111111111111",
    "worker": "ghcr.io/test/worker@sha256:2222222222222222222222222222222222222222222222222222222222222222",
    "dbtool": "ghcr.io/test/dbtool@sha256:3333333333333333333333333333333333333333333333333333333333333333",
    "auth_browser": "ghcr.io/test/auth-browser@sha256:4444444444444444444444444444444444444444444444444444444444444444",
    "tts_gateway": "ghcr.io/test/tts-gateway@sha256:5555555555555555555555555555555555555555555555555555555555555555",
    "bark": "ghcr.io/finb/bark-server@sha256:32d65b07fa835c99b31a396b77727a04ed058377fc2482da3e9dc7397167ffc4"
  },
  "artifacts": {}
}
EOF
}

printf "========================================================\n"
printf "Running Promotion Dispatcher & Rollout Orchestrator Tests\n"
printf "========================================================\n\n"

# ----------------------------------------------------
# 0. Frontend-only Release
printf "\n0. Testing frontend-only isolated rollout...\n"
T0="$TEST_TMP/t0"
setup_dispatcher_env "$T0"
sha_t0="0000000000000000000000000000000000000000"
manifest_t0="$T0/deploy/release-manifest.json"
write_mock_manifest "$manifest_t0" "$sha_t0" '{"frontend":true,"gateway":false,"worker":false,"schema":false,"auth_browser":false,"tts":false,"bark":false,"platform":false}'
TRACE_FILE="$T0/trace.log" DATA_DIR="$T0/data" SECRETS_DIR="$T0/secrets" DEPLOY_PATH="$T0" RELEASE_ENV_FILE="$T0/deploy/.release.env" SKIP_MANIFEST_CHECK=1 \
  bash "$T0/deploy/dispatch-rollout.sh" --manifest "$manifest_t0" --deploy-dir "$T0/deploy" --data-dir "$T0/data" --skip-manifest-check --allow-redeploy
assert_file_contains "$T0/trace.log" '^FRONTEND:' "Frontend-only rollout invokes frontend deploy"
if grep -Eq '^(GATEWAY|WORKER|TTS|BARK):' "$T0/trace.log"; then
  printf 'FAIL: frontend-only rollout touched another runtime\n' >&2
  TESTS_FAILED=$(( TESTS_FAILED + 1 ))
else
  printf 'PASS: frontend-only rollout leaves gateway, worker, TTS and Bark untouched\n'
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
fi

# 1. Documentation-Only Release (Zero runtime promotion)
# ----------------------------------------------------
printf "TEST 1: Documentation-Only Promotion (Zero Runtime Mutation)...\n"
T1="$TEST_TMP/t1"
setup_dispatcher_env "$T1"
manifest_t1="$T1/deploy/release-manifest.json"
sha_t1="1111222233334444555566667777888899990000"
write_mock_manifest "$manifest_t1" "$sha_t1" '{"gateway":false,"worker":false,"schema":false,"auth_browser":false,"tts":false,"bark":false,"platform":false}'

TRACE_FILE="$T1/data/trace.log" \
DEPLOY_LOCK_FILE="$T1/data/deploy.lock" \
bash "$T1/deploy/dispatch-rollout.sh" \
  --manifest "$manifest_t1" \
  --deploy-dir "$T1/deploy" \
  --data-dir "$T1/data" \
  --skip-manifest-check

assert_eq "0" "$?" "Docs-only rollout exits with code 0"
assert_eq "" "$(cat "$T1/data/trace.log")" "Docs-only rollout invoked zero component transaction scripts"
assert_file_exists "$T1/data/last-release.json" "last-release.json created for docs-only"
assert_file_contains "$T1/data/last-release.json" '"doc_only": true' "last-release records doc_only: true"
assert_file_contains "$T1/data/last-release.json" "$sha_t1" "last-release records git_sha"

# ----------------------------------------------------
# 2. Gateway-Only Promotion
# ----------------------------------------------------
printf "\nTEST 2: Gateway-Only Promotion...\n"
T2="$TEST_TMP/t2"
setup_dispatcher_env "$T2"
manifest_t2="$T2/deploy/release-manifest.json"
sha_t2="2222333344445555666677778888999900001111"
write_mock_manifest "$manifest_t2" "$sha_t2" '{"gateway":true,"worker":false,"schema":false,"auth_browser":false,"tts":false,"bark":false,"platform":false}'

TRACE_FILE="$T2/data/trace.log" \
DEPLOY_LOCK_FILE="$T2/data/deploy.lock" \
bash "$T2/deploy/dispatch-rollout.sh" \
  --manifest "$manifest_t2" \
  --deploy-dir "$T2/deploy" \
  --data-dir "$T2/data" \
  --skip-manifest-check

assert_eq "0" "$?" "Gateway-only rollout exits with code 0"
assert_file_contains "$T2/data/trace.log" "GATEWAY:ghcr.io/test/gateway@sha256:1111111111111111111111111111111111111111111111111111111111111111" "Only deploy-gateway.sh was called"
assert_eq "false" "$(grep -q 'WORKER:' "$T2/data/trace.log" && echo "true" || echo "false")" "Worker script was NOT called"
assert_eq "false" "$(grep -q 'SCHEMA:' "$T2/data/trace.log" && echo "true" || echo "false")" "Schema script was NOT called"

# ----------------------------------------------------
# 3. Worker-Only Promotion
# ----------------------------------------------------
printf "\nTEST 3: Worker-Only Promotion...\n"
T3="$TEST_TMP/t3"
setup_dispatcher_env "$T3"
manifest_t3="$T3/deploy/release-manifest.json"
sha_t3="3333444455556666777788889999000011112222"
write_mock_manifest "$manifest_t3" "$sha_t3" '{"gateway":false,"worker":true,"schema":false,"auth_browser":false,"tts":false,"bark":false,"platform":false}'

TRACE_FILE="$T3/data/trace.log" \
DEPLOY_LOCK_FILE="$T3/data/deploy.lock" \
bash "$T3/deploy/dispatch-rollout.sh" \
  --manifest "$manifest_t3" \
  --deploy-dir "$T3/deploy" \
  --data-dir "$T3/data" \
  --skip-manifest-check

assert_eq "0" "$?" "Worker-only rollout exits with code 0"
assert_file_contains "$T3/data/trace.log" "WORKER:ghcr.io/test/worker@sha256:2222222222222222222222222222222222222222222222222222222222222222" "Only deploy-worker.sh was called"
assert_eq "false" "$(grep -q 'GATEWAY:' "$T3/data/trace.log" && echo "true" || echo "false")" "Gateway script was NOT called"

# ----------------------------------------------------
# 4. Strict Dependency Order (Schema -> Worker -> Gateway)
# ----------------------------------------------------
printf "\nTEST 4: Strict Dependency Order (Schema -> Worker -> Gateway)...\n"
T4="$TEST_TMP/t4"
setup_dispatcher_env "$T4"
manifest_t4="$T4/deploy/release-manifest.json"
sha_t4="4444555566667777888899990000111122223333"
write_mock_manifest "$manifest_t4" "$sha_t4" '{"gateway":true,"worker":true,"schema":true,"auth_browser":false,"tts":false,"bark":false,"platform":false}'

TRACE_FILE="$T4/data/trace.log" \
DEPLOY_LOCK_FILE="$T4/data/deploy.lock" \
bash "$T4/deploy/dispatch-rollout.sh" \
  --manifest "$manifest_t4" \
  --deploy-dir "$T4/deploy" \
  --data-dir "$T4/data" \
  --skip-manifest-check

assert_eq "0" "$?" "Multi-component rollout exits with code 0"
# Trace log must have schema first, then worker, then gateway
line1="$(sed -n '1p' "$T4/data/trace.log" | cut -d: -f1)"
line2="$(sed -n '2p' "$T4/data/trace.log" | cut -d: -f1)"
line3="$(sed -n '3p' "$T4/data/trace.log" | cut -d: -f1)"
assert_eq "SCHEMA" "$line1" "Step 1 in trace is SCHEMA migration"
assert_eq "WORKER" "$line2" "Step 2 in trace is WORKER upgrade"
assert_eq "GATEWAY" "$line3" "Step 3 in trace is GATEWAY promotion"

# ----------------------------------------------------
# 5. Auxiliary Services Promotion (Auth-Browser, TTS, Bark)
# ----------------------------------------------------
printf "\nTEST 5: Auxiliary Services Promotion...\n"
T5="$TEST_TMP/t5"
setup_dispatcher_env "$T5"
manifest_t5="$T5/deploy/release-manifest.json"
sha_t5="5555666677778888999900001111222233334444"
write_mock_manifest "$manifest_t5" "$sha_t5" '{"gateway":false,"worker":false,"schema":false,"auth_browser":true,"tts":true,"bark":true,"platform":false}'

TRACE_FILE="$T5/data/trace.log" \
DEPLOY_LOCK_FILE="$T5/data/deploy.lock" \
bash "$T5/deploy/dispatch-rollout.sh" \
  --manifest "$manifest_t5" \
  --deploy-dir "$T5/deploy" \
  --data-dir "$T5/data" \
  --skip-manifest-check

assert_eq "0" "$?" "Auxiliary rollout exits with code 0"
assert_file_contains "$T5/data/trace.log" "AUTH_BROWSER:" "Auth browser script was called"
assert_file_contains "$T5/data/trace.log" "TTS:" "TTS script was called"
assert_file_contains "$T5/data/trace.log" "BARK:" "Bark script was called"
assert_eq "false" "$(grep -q 'GATEWAY:' "$T5/data/trace.log" && echo "true" || echo "false")" "Gateway was NOT called"
assert_eq "false" "$(grep -q 'WORKER:' "$T5/data/trace.log" && echo "true" || echo "false")" "Worker was NOT called"

# ----------------------------------------------------
# 6. Unauthorized Component Refusal (Fail-Closed)
# ----------------------------------------------------
printf "\nTEST 6: Unauthorized Component Refusal (Fail-Closed)...\n"
T6="$TEST_TMP/t6"
setup_dispatcher_env "$T6"
manifest_t6="$T6/deploy/release-manifest.json"
sha_t6="6666777788889999000011112222333344445555"
# Manifest authorizes only gateway
write_mock_manifest "$manifest_t6" "$sha_t6" '{"gateway":true,"worker":false,"schema":false,"auth_browser":false,"tts":false,"bark":false,"platform":false}'

set +e
TRACE_FILE="$T6/data/trace.log" \
DEPLOY_LOCK_FILE="$T6/data/deploy.lock" \
bash "$T6/deploy/dispatch-rollout.sh" \
  --manifest "$manifest_t6" \
  --deploy-dir "$T6/deploy" \
  --data-dir "$T6/data" \
  --scope "worker" \
  --skip-manifest-check >/dev/null 2>&1
res_t6=$?
set -e

assert_eq "1" "$res_t6" "Requesting unauthorized worker component fails closed (exit 1)"
assert_eq "" "$(cat "$T6/data/trace.log")" "No deployment transactions executed on unauthorized request"

# ----------------------------------------------------
# 7. Anti-Replay Protection
# ----------------------------------------------------
printf "\nTEST 7: Anti-Replay Protection...\n"
T7="$TEST_TMP/t7"
setup_dispatcher_env "$T7"
manifest_t7="$T7/deploy/release-manifest.json"
sha_t7="7777888899990000111122223333444455556666"
write_mock_manifest "$manifest_t7" "$sha_t7" '{"gateway":true,"worker":false,"schema":false,"auth_browser":false,"tts":false,"bark":false,"platform":false}'

# Pre-populate last-release.json with exact same commit
cat <<EOF > "$T7/data/last-release.json"
{
  "release_id": "rel-prior",
  "git_sha": "${sha_t7}",
  "status": "SUCCESS",
  "deployed_at": "2026-09-14T00:00:00Z"
}
EOF

set +e
TRACE_FILE="$T7/data/trace.log" \
DEPLOY_LOCK_FILE="$T7/data/deploy.lock" \
bash "$T7/deploy/dispatch-rollout.sh" \
  --manifest "$manifest_t7" \
  --deploy-dir "$T7/deploy" \
  --data-dir "$T7/data" >/dev/null 2>&1
res_t7=$?
set -e

assert_eq "1" "$res_t7" "Replaying already deployed commit fails closed without --allow-redeploy"

# With --allow-redeploy, it should proceed
TRACE_FILE="$T7/data/trace.log" \
DEPLOY_LOCK_FILE="$T7/data/deploy.lock" \
bash "$T7/deploy/dispatch-rollout.sh" \
  --manifest "$manifest_t7" \
  --deploy-dir "$T7/deploy" \
  --data-dir "$T7/data" \
  --allow-redeploy \
  --skip-manifest-check

assert_eq "0" "$?" "Replaying commit succeeds when --allow-redeploy is explicitly granted"

# ----------------------------------------------------
# 8. Startup Recovery of Prior Dangling Transaction
# ----------------------------------------------------
printf "\nTEST 8: Startup Recovery of Interrupted Prior Transaction...\n"
T8="$TEST_TMP/t8"
setup_dispatcher_env "$T8"
manifest_t8="$T8/deploy/release-manifest.json"
sha_t8="8888999900001111222233334444555566667777"
write_mock_manifest "$manifest_t8" "$sha_t8" '{"gateway":true,"worker":false,"schema":false,"auth_browser":false,"tts":false,"bark":false,"platform":false}'

# Create a dangling uncommitted component transaction
cat <<EOF > "$T8/data/deploy-journal.json"
{
  "tx_id": "tx-dangling-test",
  "component": "gateway",
  "state": "TX_CANDIDATE_STARTED",
  "active_slot": "blue",
  "candidate_slot": "green",
  "candidate_digest": "ghcr.io/test/gateway@sha256:1111111111111111111111111111111111111111111111111111111111111111"
}
EOF

TRACE_FILE="$T8/data/trace.log" \
DEPLOY_LOCK_FILE="$T8/data/deploy.lock" \
TX_JOURNAL_FILE="$T8/data/deploy-journal.json" \
bash "$T8/deploy/dispatch-rollout.sh" \
  --manifest "$manifest_t8" \
  --deploy-dir "$T8/deploy" \
  --data-dir "$T8/data" \
  --skip-manifest-check

assert_eq "0" "$?" "Rollout with prior dangling transaction succeeds after startup recovery"
# The dangling journal should have been archived or recovered
assert_eq "false" "$(grep -q 'TX_CANDIDATE_STARTED' "$T8/data/deploy-journal.json" 2>/dev/null && echo "true" || echo "false")" "Dangling transaction was cleaned up"

# ----------------------------------------------------
# 9. Signal Handling & Rollout Journal Interruption
# ----------------------------------------------------
printf "\nTEST 9: Signal / Cancel Trap and Journal Recording...\n"
T9="$TEST_TMP/t9"
setup_dispatcher_env "$T9"
manifest_t9="$T9/deploy/release-manifest.json"
sha_t9="9999000011112222333344445555666677778888"
write_mock_manifest "$manifest_t9" "$sha_t9" '{"gateway":true,"worker":false,"schema":false,"auth_browser":false,"tts":false,"bark":false,"platform":false}'

# Make deploy-gateway.sh sleep so we can send SIGTERM
cat <<'EOF' > "$T9/deploy/deploy-gateway.sh"
#!/usr/bin/env bash
sleep 10
exit 0
EOF
chmod 755 "$T9/deploy/deploy-gateway.sh"

TRACE_FILE="$T9/data/trace.log" \
DEPLOY_LOCK_FILE="$T9/data/deploy.lock" \
bash "$T9/deploy/dispatch-rollout.sh" \
  --manifest "$manifest_t9" \
  --deploy-dir "$T9/deploy" \
  --data-dir "$T9/data" \
  --skip-manifest-check &
DISPATCHER_PID=$!

for _ in {1..50}; do
  [[ -f "$T9/data/rollout-journal.json" ]] && break
  sleep 0.1
done
sleep 0.5

kill -TERM "$DISPATCHER_PID" 2>/dev/null || true
wait "$DISPATCHER_PID" 2>/dev/null || true

assert_file_exists "$T9/data/rollout-journal.json" "Rollout journal preserved on SIGTERM"
assert_file_contains "$T9/data/rollout-journal.json" '"status": "INTERRUPTED"' "Rollout journal marked INTERRUPTED on cancellation"

# ----------------------------------------------------
# 10. Remote Release Lock Contention
# ----------------------------------------------------
printf "\nTEST 10: Remote Release Lock Contention...\n"
T10="$TEST_TMP/t10"
setup_dispatcher_env "$T10"
manifest_t10="$T10/deploy/release-manifest.json"
sha_t10="0000111122223333444455556666777788889999"
write_mock_manifest "$manifest_t10" "$sha_t10" '{"gateway":true,"worker":false,"schema":false,"auth_browser":false,"tts":false,"bark":false,"platform":false}'

# Lock the deploy lock file externally using flock (or open FD)
lock_file="$T10/data/deploy.lock"
touch "$lock_file"

if command -v flock >/dev/null 2>&1; then
  (
    exec 9>"$lock_file"
    flock -x 9
    sleep 3
  ) &
  HOLDER_PID=$!
  sleep 0.5

  set +e
  DEPLOY_LOCK_TIMEOUT=1 \
  DEPLOY_LOCK_FILE="$lock_file" \
  TRACE_FILE="$T10/data/trace.log" \
  bash "$T10/deploy/dispatch-rollout.sh" \
    --manifest "$manifest_t10" \
    --deploy-dir "$T10/deploy" \
    --data-dir "$T10/data" \
    --skip-manifest-check >/dev/null 2>&1
  res_t10=$?
  set -e

  wait "$HOLDER_PID" 2>/dev/null || true
  assert_eq "1" "$res_t10" "Dispatcher fails when deploy lock is held by another process"
else
  printf '  [SKIP] flock not available in this test environment\n'
fi

# ----------------------------------------------------
# 11. Evidence Retention
# ----------------------------------------------------
printf "\nTEST 11: Evidence & Receipt Retention...\n"
T11="$TEST_TMP/t11"
setup_dispatcher_env "$T11"
manifest_t11="$T11/deploy/release-manifest.json"
sha_t11="1234567890abcdef1234567890abcdef12345678"
rel_id_t11="rel-test-evidence-11"
write_mock_manifest "$manifest_t11" "$sha_t11" '{"gateway":true,"worker":true,"schema":false,"auth_browser":false,"tts":false,"bark":false,"platform":false}' "2026-09-14T00:00:00Z" "$rel_id_t11"

TRACE_FILE="$T11/data/trace.log" \
DEPLOY_LOCK_FILE="$T11/data/deploy.lock" \
bash "$T11/deploy/dispatch-rollout.sh" \
  --manifest "$manifest_t11" \
  --deploy-dir "$T11/deploy" \
  --data-dir "$T11/data" \
  --skip-manifest-check

assert_eq "0" "$?" "Rollout completes successfully"
evidence_dir="$T11/data/releases/$rel_id_t11"
assert_file_exists "$evidence_dir/receipt.json" "receipt.json retained in evidence directory"
assert_file_exists "$evidence_dir/rollout-journal.json" "rollout-journal.json retained in evidence directory"
assert_file_exists "$evidence_dir/release-manifest.json" "release-manifest.json copy retained in evidence directory"
assert_file_contains "$evidence_dir/receipt.json" '"status": "SUCCESS"' "receipt.json records SUCCESS"
assert_file_contains "$evidence_dir/receipt.json" '"gateway"' "receipt.json lists gateway in promoted components"
assert_file_contains "$evidence_dir/receipt.json" '"worker"' "receipt.json lists worker in promoted components"

printf "\n========================================================\n"
printf "Results: %d passed, %d failed\n" "$TESTS_PASSED" "$TESTS_FAILED"
printf "========================================================\n"

if [[ $TESTS_FAILED -gt 0 ]]; then
  exit 1
fi
exit 0
