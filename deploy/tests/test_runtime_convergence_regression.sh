#!/usr/bin/env bash
# deploy/tests/test_runtime_convergence_regression.sh
# Regression tests for:
# TEST 1: Ambient TTS = OLD vs Manifest TTS = NEW -> deploy-tts MUST receive NEW (P0-1)
# TEST 2: Scope = gateway + failover_controller -> gateway ACK does not fail on drift;
#         failover installs -> orchestrator pre-soak drift PASS (P0-2)
set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR="$(cd -- "$SCRIPT_DIR/.." && pwd)"
REPO_ROOT="$(cd -- "$DEPLOY_DIR/.." && pwd)"

TEST_TMP="$(mktemp -d "${TMPDIR:-/tmp}/acb-convergence-tests.XXXXXX")"
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

assert_not_contains() {
  local haystack="$1"
  local needle="$2"
  local msg="$3"
  if grep -F -q -- "$needle" <<< "$haystack"; then
    printf 'FAIL: %s (text contained forbidden "%s")\n' "$msg" "$needle" >&2
    TESTS_FAILED=$(( TESTS_FAILED + 1 ))
    return 1
  fi
  printf 'PASS: %s\n' "$msg"
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
  return 0
}

setup_env() {
  local tdir="$1"
  mkdir -p "$tdir/deploy/compose" "$tdir/data" "$tdir/secrets" "$tdir/state" "$tdir/releases" "$tdir/bin"
  export RUNTIME_ROOT="$tdir"
  export RUNTIME_RELEASES_DIR="$tdir"
  export DEPLOY_PATH="$tdir"
  export SOAK_SECONDS=0

  cp -r "$DEPLOY_DIR/lib"* "$tdir/deploy/"
  cp "$DEPLOY_DIR/runtime-layout.sh" "$tdir/deploy/"
  cp "$DEPLOY_DIR/release-env.sh" "$tdir/deploy/"
  cp "$DEPLOY_DIR/release-state.py" "$tdir/deploy/"
  cp "$DEPLOY_DIR/verify-manifest.sh" "$tdir/deploy/"
  cp "$DEPLOY_DIR/dispatch-rollout.sh" "$tdir/deploy/"
  cp "$DEPLOY_DIR/deploy-gateway.sh" "$tdir/deploy/"
  cp -r "$DEPLOY_DIR/compose/"* "$tdir/deploy/compose/"
  cp "$DEPLOY_DIR/compose.prod.yaml" "$tdir/deploy/"

  cat <<'EOF' > "$tdir/deploy/verify-runtime-drift.sh"
#!/usr/bin/env bash
if [[ -n "${RUNTIME_DRIFT_CHECK_CMD:-}" ]]; then
  eval "$RUNTIME_DRIFT_CHECK_CMD"
  exit $?
fi
exit 0
EOF
  chmod +x "$tdir/deploy/verify-runtime-drift.sh"

  printf 'mock-master\n' > "$tdir/secrets/app_master_key"
  printf 'mock-worker\n' > "$tdir/secrets/worker_internal_token"
  printf 'mock-tts\n' > "$tdir/secrets/tts_internal_token"
  printf 'mock-bark-user\n' > "$tdir/secrets/bark_basic_auth_user"
  printf 'mock-bark-pass\n' > "$tdir/secrets/bark_basic_auth_password"
  chmod 600 "$tdir/secrets/"* 2>/dev/null || true
  chmod 755 "$tdir/deploy/"*.sh

  cat <<'EOF' > "$tdir/deploy/.env.production"
APP_ENV=production
DATA_DIR=/tmp/data
PUBLIC_ORIGIN=https://acb.example.com
EOF

  cat <<'EOF' > "$tdir/deploy/.release.env"
IMAGE_REF_BLUE=ghcr.io/test/gateway@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
IMAGE_REF_GREEN=ghcr.io/test/gateway@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
FRONTEND_IMAGE_REF=ghcr.io/test/frontend@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
WORKER_IMAGE_REF=ghcr.io/test/worker@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
DBTOOL_IMAGE_REF=ghcr.io/test/dbtool@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
BROWSER_IMAGE_REF=ghcr.io/test/auth-browser@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
TTS_IMAGE_REF=ghcr.io/test/tts-gateway@sha256:0000000000000000000000000000000000000000000000000000000000000000
BARK_IMAGE_REF=ghcr.io/finb/bark-server@sha256:32d65b07fa835c99b31a396b77727a04ed058377fc2482da3e9dc7397167ffc4
EOF

  printf 'blue' > "$tdir/state/gateway-active-slot"
  printf 'blue' > "$tdir/state/frontend-active-slot"
  cat <<'EOF' > "$tdir/state/current-release.json"
{
  "schema_version": 1,
  "generation": 1,
  "release_id": "baseline",
  "git_sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "manifest_sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "status": "COMPLETED",
  "committed_at": "2026-01-01T00:00:00Z",
  "active_slots": {"gateway": "blue", "frontend": "blue"},
  "images": {
    "gateway": {"blue": "ghcr.io/test/gateway@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "green": "ghcr.io/test/gateway@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
    "frontend": "ghcr.io/test/frontend@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "worker": "ghcr.io/test/worker@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "dbtool": "ghcr.io/test/dbtool@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "auth_browser": "ghcr.io/test/browser@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "tts": "ghcr.io/test/tts-gateway@sha256:0000000000000000000000000000000000000000000000000000000000000000",
    "bark": "ghcr.io/finb/bark-server@sha256:32d65b07fa835c99b31a396b77727a04ed058377fc2482da3e9dc7397167ffc4"
  }
}
EOF

  cat <<'EOF' > "$tdir/bin/docker"
#!/usr/bin/env bash
if [[ "$1" == "inspect" ]]; then
  if [[ "$*" == *".State.Running"* ]]; then echo "true"; exit 0; fi
  if [[ "$*" == *".State.Health"* ]]; then echo "healthy"; exit 0; fi
  if [[ "$*" == *".Config.Image"* ]]; then echo "ghcr.io/test/gateway@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"; exit 0; fi
fi
exit 0
EOF
  chmod 755 "$tdir/bin/docker"
  export PATH="$tdir/bin:$PATH"

  export BARK_SECRET_PREFLIGHT_CMD=true
  export RUNTIME_DRIFT_CHECK_CMD=true
  export ALLOW_TEST_LOCK_PATH=1
}

write_manifest() {
  local path="$1"
  local git_sha="$2"
  local scope_json="$3"
  local tts_img="$4"

  cat <<EOF > "$path"
{
  "schema_version": 1,
  "release_id": "rel-${git_sha:0:12}",
  "git_sha": "${git_sha}",
  "created_at": "2026-09-17T00:00:00Z",
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
    "tts": "${tts_img}",
    "bark": "ghcr.io/finb/bark-server@sha256:32d65b07fa835c99b31a396b77727a04ed058377fc2482da3e9dc7397167ffc4"
  },
  "artifacts": {}
}
EOF
}

printf "========================================================\n"
printf "Running Runtime Convergence Regression Tests (P0-1, P0-2)\n"
printf "========================================================\n\n"

# ---------------------------------------------------------------
# TEST 1: Ambient TTS = OLD vs Manifest TTS = NEW (P0-1)
# ---------------------------------------------------------------
printf "TEST 1: Ambient TTS = OLD, Manifest TTS = NEW -> deploy-tts MUST receive NEW\n"
T1="$TEST_TMP/t1"
setup_env "$T1"
sha_t1="1111111111111111111111111111111111111111"
manifest_t1="$T1/deploy/release-manifest.json"

old_tts="ghcr.io/test/tts-gateway@sha256:0000000000000000000000000000000000000000000000000000000000000000"
new_tts="ghcr.io/test/tts-gateway@sha256:5555555555555555555555555555555555555555555555555555555555555555"

# Ambient .release.env has OLD
sed -i "s|^TTS_IMAGE_REF=.*|TTS_IMAGE_REF=$old_tts|" "$T1/deploy/.release.env"

# Manifest has NEW
write_manifest "$manifest_t1" "$sha_t1" '{"gateway":false,"worker":false,"schema":false,"auth_browser":false,"tts":true,"bark":false,"failover_controller":false,"platform":false}' "$new_tts"

cat <<'EOF' > "$T1/deploy/deploy-tts.sh"
#!/usr/bin/env bash
set -euo pipefail
printf 'TTS:%s\n' "$@" >> "${TRACE_FILE}"
exit 0
EOF
chmod +x "$T1/deploy/deploy-tts.sh"

cat <<EOF > "$T1/deploy/verify-manifest.sh"
#!/usr/bin/env bash
out=""
while [[ \$# -gt 0 ]]; do
  case "\$1" in
    --output-env) out="\$2"; shift 2 ;;
    *) shift ;;
  esac
done
if [[ -n "\$out" ]]; then
  cat <<ENVEOF > "\$out"
PROMOTION_FRONTEND=false
PROMOTION_GATEWAY=false
PROMOTION_WORKER=false
PROMOTION_SCHEMA=false
PROMOTION_AUTH_BROWSER=false
PROMOTION_TTS=true
PROMOTION_BARK=false
PROMOTION_FAILOVER_CONTROLLER=false
PROMOTION_PLATFORM=false
PROMOTION_DOC_ONLY=false
IMAGE_FRONTEND=ghcr.io/test/frontend@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
IMAGE_GATEWAY=ghcr.io/test/gateway@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
IMAGE_WORKER=ghcr.io/test/worker@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
IMAGE_DBTOOL=ghcr.io/test/dbtool@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
IMAGE_AUTH_BROWSER=ghcr.io/test/auth-browser@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
IMAGE_TTS=$new_tts
IMAGE_BARK=ghcr.io/test/bark@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
GIT_SHA=$sha_t1
RELEASE_ID=rel-t1
ENVEOF
fi
exit 0
EOF
chmod +x "$T1/deploy/verify-manifest.sh"

cat <<'EOF' > "$T1/bin/cosign"
#!/usr/bin/env bash
exit 0
EOF
chmod +x "$T1/bin/cosign"
echo "mock-bundle" > "$T1/deploy/release-manifest.bundle"

cat <<'EOF' > "$T1/deploy/verify-release-baseline.sh"
#!/usr/bin/env bash
exit 0
EOF
chmod +x "$T1/deploy/verify-release-baseline.sh"

TRACE_FILE="$T1/data/trace.log" DEPLOY_LOCK_FILE="$T1/data/deploy.lock" \
TTS_IMAGE_REF="$old_tts" \
  bash "$T1/deploy/dispatch-rollout.sh" \
    --manifest "$manifest_t1" \
    --bundle "$T1/deploy/release-manifest.bundle" \
    --expected-identity "https://github.com/test/repo" \
    --require-cosign \
    --deploy-dir "$T1/deploy" \
    --data-dir "$T1/data"

assert_file_contains "$T1/data/trace.log" "TTS:$new_tts" "deploy-tts.sh received NEW digest from manifest"
assert_not_contains "$(cat "$T1/data/trace.log")" "$old_tts" "deploy-tts.sh did NOT receive ambient OLD digest"

# ---------------------------------------------------------------
# TEST 2: Scope = gateway + failover_controller ordering (P0-2)
# ---------------------------------------------------------------
printf "\nTEST 2: Gateway + Failover Controller promotion ordering\n"
T2="$TEST_TMP/t2"
setup_env "$T2"
sha_t2="2222222222222222222222222222222222222222"
manifest_t2="$T2/deploy/release-manifest.json"

write_manifest "$manifest_t2" "$sha_t2" '{"gateway":true,"worker":false,"schema":false,"auth_browser":false,"tts":false,"bark":false,"failover_controller":true,"platform":false}' "$old_tts"

cat <<'EOF' > "$T2/deploy/deploy-gateway.sh"
#!/usr/bin/env bash
set -euo pipefail
printf 'GATEWAY_ACK\n' >> "${TRACE_FILE}"
exit 0
EOF
chmod +x "$T2/deploy/deploy-gateway.sh"

cat <<'EOF' > "$T2/deploy/deploy-failover-controller.sh"
#!/usr/bin/env bash
set -euo pipefail
if ! grep -q "GATEWAY_ACK" "${TRACE_FILE}"; then
  echo "FAIL: failover controller invoked before gateway ACK!" >&2
  exit 1
fi
printf 'FAILOVER_CONTROLLER_INSTALLED\n' >> "${TRACE_FILE}"
exit 0
EOF
chmod +x "$T2/deploy/deploy-failover-controller.sh"

# Pre-soak drift check runs in orchestrator: failover controller MUST already be installed
cat <<EOF > "$T2/data/check_presoak_drift.sh"
#!/usr/bin/env bash
if ! grep -q "GATEWAY_ACK" "$T2/data/trace.log"; then
  echo "DRIFT_ERROR: gateway ACK has not occurred!" >&2
  exit 1
fi
if ! grep -q "FAILOVER_CONTROLLER_INSTALLED" "$T2/data/trace.log"; then
  echo "DRIFT_ERROR: failover controller has not been installed yet!" >&2
  exit 1
fi
exit 0
EOF
chmod +x "$T2/data/check_presoak_drift.sh"

TRACE_FILE="$T2/data/trace.log" DEPLOY_LOCK_FILE="$T2/data/deploy.lock" \
RUNTIME_DRIFT_CHECK_CMD="bash $T2/data/check_presoak_drift.sh" \
  bash "$T2/deploy/dispatch-rollout.sh" \
    --manifest "$manifest_t2" \
    --deploy-dir "$T2/deploy" \
    --data-dir "$T2/data" \
    --skip-manifest-check

assert_file_contains "$T2/data/trace.log" "GATEWAY_ACK" "Gateway route ACK succeeded before failover controller"
assert_file_contains "$T2/data/trace.log" "FAILOVER_CONTROLLER_INSTALLED" "Failover controller installed after gateway return"
canonical_t2="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["git_sha"])' "$T2/state/current-release.json")"
assert_eq "$sha_t2" "$canonical_t2" "Pre-soak drift check passed and canonical state committed"
assert_not_contains "$(cat "$DEPLOY_DIR/deploy-gateway.sh")" "verify-runtime-drift.sh" "deploy-gateway.sh has no premature full-release drift audit"

printf "\n========================================================\n"
printf "Results: %d passed, %d failed\n" "$TESTS_PASSED" "$TESTS_FAILED"
printf "========================================================\n"

if [[ $TESTS_FAILED -gt 0 ]]; then
  exit 1
fi
exit 0
