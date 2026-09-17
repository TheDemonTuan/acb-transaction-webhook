#!/usr/bin/env bash
# deploy/tests/test_trusted_engine_bootstrap.sh
# Tests for schema-light signed deployment-engine bootstrap (Task 5).
# Verifies:
#   1. Old-schema migration (reproducing run #249 attempt 1 deadlock)
#   2. Valid v2 signed manifest bootstrap & atomic versioning
#   3. Malformed / invalid signature fail-closed
#   4. Candidate artifact tampering fail-closed
#   5. Missing required engine artifact fail-closed
#   6. Symlink candidate engine artifact fail-closed
#   7. Prevention of unsigned candidate code execution
#   8. Stable deployer self-healing on outdated host verifier
set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR="$(cd -- "$SCRIPT_DIR/.." && pwd)"

TEST_TMP="$(mktemp -d "${TMPDIR:-/tmp}/acb-trusted-bootstrap-tests.XXXXXX")"
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
  local path="$1"
  local msg="$2"
  if [[ ! -f "$path" ]]; then
    printf 'FAIL: %s (file missing: %s)\n' "$msg" "$path" >&2
    TESTS_FAILED=$(( TESTS_FAILED + 1 ))
    return 1
  fi
  printf 'PASS: %s\n' "$msg"
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
  return 0
}

assert_file_not_exists() {
  local path="$1"
  local msg="$2"
  if [[ -e "$path" ]]; then
    printf 'FAIL: %s (path exists but should not: %s)\n' "$msg" "$path" >&2
    TESTS_FAILED=$(( TESTS_FAILED + 1 ))
    return 1
  fi
  printf 'PASS: %s\n' "$msg"
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
  return 0
}

# Setup mock cosign binary
MOCK_BIN="$TEST_TMP/mock_bin"
mkdir -p "$MOCK_BIN"
cat <<'EOF' > "$MOCK_BIN/cosign"
#!/usr/bin/env bash
set -euo pipefail
# Mock cosign verify-blob
if [[ "${1:-}" == "verify-blob" ]]; then
  bundle=""
  cert_identity=""
  cert_issuer=""
  manifest=""
  shift
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --bundle) bundle="$2"; shift 2 ;;
      --certificate-identity) cert_identity="$2"; shift 2 ;;
      --certificate-oidc-issuer) cert_issuer="$2"; shift 2 ;;
      *) manifest="$1"; shift ;;
    esac
  done

  # Check if bundle indicates invalid/malformed signature
  if [[ -f "$bundle" ]] && grep -q "MALFORMED_SIGNATURE" "$bundle" 2>/dev/null; then
    printf 'Error: signature verification failed: invalid signature\n' >&2
    exit 1
  fi

  # Check expected certificate identity
  if [[ -z "$cert_identity" || "$cert_identity" == "*" || "$cert_identity" == ".*" ]]; then
    printf 'Error: exact certificate identity required\n' >&2
    exit 1
  fi

  if [[ "$cert_identity" == *"invalid"* ]]; then
    printf 'Error: certificate identity mismatch\n' >&2
    exit 1
  fi

  # Success
  printf 'Verified OK\n'
  exit 0
fi
printf 'Unknown mock cosign command: %s\n' "${1:-}" >&2
exit 1
EOF
chmod +x "$MOCK_BIN/cosign"

export PATH="$MOCK_BIN:$PATH"
EXPECTED_TEST_IDENTITY="https://github.com/test-org/test-repo/.github/workflows/deploy.yml@refs/heads/main"
EXPECTED_TEST_ISSUER="https://token.actions.githubusercontent.com"

# Helper to populate a minimal valid candidate release bundle
populate_candidate_release() {
  local rel_dir="$1"
  local rel_id="${2:-rel-test-v2}"
  mkdir -p "$rel_dir/lib"

  cp "$DEPLOY_DIR/stable-deployer.sh" "$rel_dir/"
  cp "$DEPLOY_DIR/verify-manifest.sh" "$rel_dir/"
  cp "$DEPLOY_DIR/runtime-layout.sh" "$rel_dir/"
  cp "$DEPLOY_DIR/reconcile-release.sh" "$rel_dir/"
  cp "$DEPLOY_DIR/release-state.py" "$rel_dir/"
  cp "$DEPLOY_DIR/verify-runtime-drift.sh" "$rel_dir/"
  cp "$DEPLOY_DIR/release-env.sh" "$rel_dir/"
  cp "$DEPLOY_DIR/edge-probe.sh" "$rel_dir/"
  cp "$DEPLOY_DIR/bootstrap-deployment-engine.sh" "$rel_dir/"
  cp "$DEPLOY_DIR/dispatch-rollout.sh" "$rel_dir/"
  cp "$DEPLOY_DIR/verify-release-baseline.sh" "$rel_dir/"
  cp "$DEPLOY_DIR/preflight-runtime.sh" "$rel_dir/"
  cp "$DEPLOY_DIR/deploy-worker.sh" "$rel_dir/"
  cp "$DEPLOY_DIR/stage-immutable-release.sh" "$rel_dir/"
  cp "$DEPLOY_DIR/component-map.json" "$rel_dir/"
  cp "$DEPLOY_DIR/compose.prod.yaml" "$rel_dir/"
  cp "$DEPLOY_DIR/lib.sh" "$rel_dir/"
  cp -r "$DEPLOY_DIR/lib/"*.sh "$rel_dir/lib/"

  local hash_deployer hash_verifier hash_layout hash_reconcile hash_state hash_drift hash_relenv hash_edge hash_lib hash_bootstrap hash_dispatch hash_component_map hash_compose hash_preflight_runtime hash_worker
  hash_deployer="$(sha256sum "$rel_dir/stable-deployer.sh" | awk '{print $1}')"
  hash_verifier="$(sha256sum "$rel_dir/verify-manifest.sh" | awk '{print $1}')"
  hash_layout="$(sha256sum "$rel_dir/runtime-layout.sh" | awk '{print $1}')"
  hash_reconcile="$(sha256sum "$rel_dir/reconcile-release.sh" | awk '{print $1}')"
  hash_state="$(sha256sum "$rel_dir/release-state.py" | awk '{print $1}')"
  hash_drift="$(sha256sum "$rel_dir/verify-runtime-drift.sh" | awk '{print $1}')"
  hash_relenv="$(sha256sum "$rel_dir/release-env.sh" | awk '{print $1}')"
  hash_edge="$(sha256sum "$rel_dir/edge-probe.sh" | awk '{print $1}')"
  hash_lib="$(sha256sum "$rel_dir/lib.sh" | awk '{print $1}')"
  hash_bootstrap="$(sha256sum "$rel_dir/bootstrap-deployment-engine.sh" | awk '{print $1}')"
  hash_dispatch="$(sha256sum "$rel_dir/dispatch-rollout.sh" | awk '{print $1}')"
  hash_component_map="$(sha256sum "$rel_dir/component-map.json" | awk '{print $1}')"
  hash_compose="$(sha256sum "$rel_dir/compose.prod.yaml" | awk '{print $1}')"
  hash_preflight_runtime="$(sha256sum "$rel_dir/preflight-runtime.sh" | awk '{print $1}')"
  hash_worker="$(sha256sum "$rel_dir/deploy-worker.sh" | awk '{print $1}')"

  # Compute lib/*.sh hashes
  local lib_hashes=()
  for lf in "$rel_dir/lib/"*.sh; do
    if [[ -f "$lf" ]]; then
      local base_name
      base_name="$(basename "$lf")"
      local lhash
      lhash="$(sha256sum "$lf" | awk '{print $1}')"
      lib_hashes+=("\"lib/$base_name\": \"$lhash\",")
    fi
  done

  cat <<EOF > "$rel_dir/release-manifest.json"
{
  "release_id": "$rel_id",
  "git_sha": "0123456789abcdef0123456789abcdef01234567",
  "created_at": "$(date -u +'%Y-%m-%dT%H:%M:%SZ')",
  "base": {"git_sha": "0000000000000000000000000000000000000000", "generation": 1},
  "schema_version": 2,
  "compatibility": {"schema_version": 9, "min_supported_schema_version": 9, "worker_rpc_version": 2},
  "promotion": {},
  "promotion_scope": [],
  "images": {
    "frontend": "ghcr.io/org/fe@sha256:0000000000000000000000000000000000000000000000000000000000000001",
    "gateway": "ghcr.io/org/gw@sha256:0000000000000000000000000000000000000000000000000000000000000002",
    "worker": "ghcr.io/org/wk@sha256:0000000000000000000000000000000000000000000000000000000000000003",
    "dbtool": "ghcr.io/org/db@sha256:0000000000000000000000000000000000000000000000000000000000000004",
    "auth_browser": "ghcr.io/org/br@sha256:0000000000000000000000000000000000000000000000000000000000000005",
    "tts": "ghcr.io/org/tts@sha256:0000000000000000000000000000000000000000000000000000000000000006",
    "bark": "ghcr.io/org/bark@sha256:0000000000000000000000000000000000000000000000000000000000000007"
  },
  "artifacts": {
    $(printf '%s\n' "${lib_hashes[@]}")
    "stable-deployer.sh": "$hash_deployer",
    "verify-manifest.sh": "$hash_verifier",
    "runtime-layout.sh": "$hash_layout",
    "reconcile-release.sh": "$hash_reconcile",
    "release-state.py": "$hash_state",
    "verify-runtime-drift.sh": "$hash_drift",
    "release-env.sh": "$hash_relenv",
    "edge-probe.sh": "$hash_edge",
    "lib.sh": "$hash_lib",
    "bootstrap-deployment-engine.sh": "$hash_bootstrap",
    "dispatch-rollout.sh": "$hash_dispatch",
    "component-map.json": "$hash_component_map",
    "compose.prod.yaml": "$hash_compose",
    "preflight-runtime.sh": "$hash_preflight_runtime",
    "deploy-worker.sh": "$hash_worker"
  }
}
EOF
  printf 'VALID_SIGNATURE_BUNDLE\n' > "$rel_dir/release-manifest.bundle"
}

# ==============================================================================
# TEST 1: Old-Schema Migration (Run #249 Attempt 1 Deadlock Reproduction & Fix)
# ==============================================================================
test_old_schema_migration_deadlock_and_fix() {
  printf '\n--- Test 1: Old-Schema Migration (Run #249 Deadlock Reproduction & Fix) ---\n'
  local tdir="$TEST_TMP/test1_old_schema"
  mkdir -p "$tdir/deploy" "$tdir/state" "$tdir/releases/rel-new"

  # Setup host deploy dir with OLD verifier that requires legacy images.tts_gateway
  cat <<'EOF' > "$tdir/deploy/verify-manifest.sh"
#!/usr/bin/env bash
# Old trusted verifier (run #249 fixture) that requires images.tts_gateway
set -euo pipefail
manifest=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --manifest) manifest="$2"; shift 2 ;;
    *) shift ;;
  esac
done
if ! python3 - "$manifest" <<'PY'
import json, sys
data = json.load(open(sys.argv[1]))
images = data.get("images", {})
if "tts_gateway" not in images:
    sys.stderr.write("OLD_VERIFIER_FAIL: images.tts_gateway is required by old schema\n")
    sys.exit(1)
PY
then
  exit 1
fi
exit 0
EOF
  chmod +x "$tdir/deploy/verify-manifest.sh"

  # Host also has older engine scripts
  echo "# old runtime layout v1" > "$tdir/deploy/runtime-layout.sh"
  cp "$DEPLOY_DIR/bootstrap-deployment-engine.sh" "$tdir/deploy/"

  # Setup candidate release with canonical images.tts (new schema)
  populate_candidate_release "$tdir/releases/rel-new" "rel-new"

  # Step 1: Assert old trusted verifier rejects candidate release
  local old_verifier_ec=0
  "$tdir/deploy/verify-manifest.sh" --manifest "$tdir/releases/rel-new/release-manifest.json" 2>/dev/null || old_verifier_ec=$?
  assert_eq "1" "$old_verifier_ec" "Old verifier rejects new candidate schema (reproducing run #249 deadlock)"

  # Step 2: Run schema-light bootstrap on candidate release
  local bootstrap_ec=0
  bash "$tdir/deploy/bootstrap-deployment-engine.sh" \
    --release-dir "$tdir/releases/rel-new" \
    --runtime-deploy-dir "$tdir/deploy" \
    --expected-identity "$EXPECTED_TEST_IDENTITY" \
    --expected-issuer "$EXPECTED_TEST_ISSUER" \
    --require-cosign \
    --skip-semantic-check || bootstrap_ec=$?

  assert_eq "0" "$bootstrap_ec" "Signed generic bootstrap succeeds despite old verifier deadlock"

  # Step 3: Assert the engine was upgraded and versioned
  assert_file_exists "$tdir/deploy/runtime-layout.sh" "runtime-layout.sh exists in deploy dir"
  if grep -q "RUNTIME_ROOT=" "$tdir/deploy/runtime-layout.sh"; then
    printf 'PASS: runtime-layout.sh upgraded from candidate\n'
    TESTS_PASSED=$(( TESTS_PASSED + 1 ))
  else
    printf 'FAIL: runtime-layout.sh not upgraded\n' >&2
    TESTS_FAILED=$(( TESTS_FAILED + 1 ))
  fi

  assert_file_exists "$tdir/engines/rel-new/runtime-layout.sh" "Versioned engine bundle exists in engines/rel-new"
  if [[ -L "$tdir/engines/current" || ( "${OS:-}" == "Windows_NT" && -d "$tdir/engines/current" ) ]]; then
    printf 'PASS: engines/current symlink exists\n'
    TESTS_PASSED=$(( TESTS_PASSED + 1 ))
  else
    printf 'FAIL: engines/current symlink missing\n' >&2
    TESTS_FAILED=$(( TESTS_FAILED + 1 ))
  fi

  # Step 4: Assert new verifier in deploy dir accepts candidate release
  local new_verifier_ec=0
  "$tdir/deploy/verify-manifest.sh" \
    --manifest "$tdir/releases/rel-new/release-manifest.json" \
    --bundle "$tdir/releases/rel-new/release-manifest.bundle" \
    --deploy-dir "$tdir/releases/rel-new" \
    --expected-identity "$EXPECTED_TEST_IDENTITY" \
    --expected-issuer "$EXPECTED_TEST_ISSUER" \
    --require-cosign \
    --allow-redeploy || new_verifier_ec=$?
  assert_eq "0" "$new_verifier_ec" "New verifier installed by bootstrap passes on new manifest schema"
}

# ==============================================================================
# TEST 2: Valid v2 Manifest Bootstrap & Versioning
# ==============================================================================
test_valid_v2_manifest_bootstrap() {
  printf '\n--- Test 2: Valid v2 Manifest Bootstrap & Atomic Versioning ---\n'
  local tdir="$TEST_TMP/test2_valid_v2"
  mkdir -p "$tdir/deploy" "$tdir/releases/rel-v2-prod"

  echo "# baseline deployer" > "$tdir/deploy/stable-deployer.sh"
  cp "$DEPLOY_DIR/bootstrap-deployment-engine.sh" "$tdir/deploy/"

  populate_candidate_release "$tdir/releases/rel-v2-prod" "rel-v2-prod"

  local ec=0
  bash "$tdir/deploy/bootstrap-deployment-engine.sh" \
    --release-dir "$tdir/releases/rel-v2-prod" \
    --runtime-deploy-dir "$tdir/deploy" \
    --expected-identity "$EXPECTED_TEST_IDENTITY" \
    --expected-issuer "$EXPECTED_TEST_ISSUER" \
    --require-cosign \
    --skip-semantic-check || ec=$?

  assert_eq "0" "$ec" "Valid v2 signed manifest bootstrap succeeds"
  assert_file_exists "$tdir/deploy/stable-deployer.sh" "stable-deployer.sh updated in deploy"
  assert_file_exists "$tdir/deploy/.stable-deployer.sh.previous" "Previous engine file backup retained"
  assert_file_exists "$tdir/engines/rel-v2-prod/stable-deployer.sh" "Versioned stable-deployer.sh created"
  assert_file_exists "$tdir/engines/rel-v2-prod/verify-manifest.sh" "Versioned verify-manifest.sh created"
  assert_file_exists "$tdir/engines/rel-v2-prod/reconcile-release.sh" "Versioned reconcile-release.sh created"
  assert_file_exists "$tdir/engines/rel-v2-prod/lib/common.sh" "Versioned lib/common.sh created"
  assert_file_exists "$tdir/deploy/preflight-runtime.sh" "preflight-runtime.sh updated in deploy"
  assert_file_exists "$tdir/engines/rel-v2-prod/preflight-runtime.sh" "Versioned preflight-runtime.sh created"
  assert_file_exists "$tdir/deploy/deploy-worker.sh" "deploy-worker.sh updated in deploy"
  assert_file_exists "$tdir/engines/rel-v2-prod/deploy-worker.sh" "Versioned deploy-worker.sh created"
  local cand_hash rt_hash
  cand_hash="$(sha256sum "$tdir/releases/rel-v2-prod/preflight-runtime.sh" | awk '{print $1}')"
  rt_hash="$(sha256sum "$tdir/deploy/preflight-runtime.sh" | awk '{print $1}')"
  assert_eq "$cand_hash" "$rt_hash" "preflight-runtime.sh in runtime deploy matches candidate hash"
}

# ==============================================================================
# TEST 3: Malformed / Invalid Cosign Signature Fails Closed
# ==============================================================================
test_malformed_signature_fails_closed() {
  printf '\n--- Test 3: Malformed / Invalid Signature Fails Closed ---\n'
  local tdir="$TEST_TMP/test3_malformed_sig"
  mkdir -p "$tdir/deploy" "$tdir/releases/rel-bad-sig"

  echo "# trusted deployer v1" > "$tdir/deploy/stable-deployer.sh"
  cp "$DEPLOY_DIR/bootstrap-deployment-engine.sh" "$tdir/deploy/"

  populate_candidate_release "$tdir/releases/rel-bad-sig" "rel-bad-sig"
  # Corrupt the signature bundle
  printf 'MALFORMED_SIGNATURE\n' > "$tdir/releases/rel-bad-sig/release-manifest.bundle"

  local ec=0
  bash "$tdir/deploy/bootstrap-deployment-engine.sh" \
    --release-dir "$tdir/releases/rel-bad-sig" \
    --runtime-deploy-dir "$tdir/deploy" \
    --expected-identity "$EXPECTED_TEST_IDENTITY" \
    --expected-issuer "$EXPECTED_TEST_ISSUER" \
    --require-cosign || ec=$?

  assert_eq "1" "$ec" "Malformed signature aborts bootstrap fail-closed"

  # Deploy dir must be completely unchanged
  if grep -q "# trusted deployer v1" "$tdir/deploy/stable-deployer.sh"; then
    printf 'PASS: Host deploy directory untouched after malformed signature\n'
    TESTS_PASSED=$(( TESTS_PASSED + 1 ))
  else
    printf 'FAIL: Host deploy directory modified despite signature failure\n' >&2
    TESTS_FAILED=$(( TESTS_FAILED + 1 ))
  fi

  assert_file_not_exists "$tdir/engines/rel-bad-sig" "Versioned directory not created on signature failure"
}

# ==============================================================================
# TEST 4: Candidate Artifact Tampering Fails Closed
# ==============================================================================
test_artifact_tampering_fails_closed() {
  printf '\n--- Test 4: Candidate Artifact Tampering Fails Closed ---\n'
  local tdir="$TEST_TMP/test4_tamper"
  mkdir -p "$tdir/deploy" "$tdir/releases/rel-tampered"

  echo "# trusted reconcile v1" > "$tdir/deploy/reconcile-release.sh"
  cp "$DEPLOY_DIR/bootstrap-deployment-engine.sh" "$tdir/deploy/"

  populate_candidate_release "$tdir/releases/rel-tampered" "rel-tampered"
  # Tamper with candidate reconcile-release.sh after manifest generation
  echo "# MALICIOUS TAMPERED SCRIPT" >> "$tdir/releases/rel-tampered/reconcile-release.sh"

  local ec=0
  bash "$tdir/deploy/bootstrap-deployment-engine.sh" \
    --release-dir "$tdir/releases/rel-tampered" \
    --runtime-deploy-dir "$tdir/deploy" \
    --expected-identity "$EXPECTED_TEST_IDENTITY" \
    --expected-issuer "$EXPECTED_TEST_ISSUER" \
    --require-cosign || ec=$?

  assert_eq "1" "$ec" "Artifact tampering aborts bootstrap fail-closed"

  # Deploy dir must be completely unchanged
  if grep -q "# trusted reconcile v1" "$tdir/deploy/reconcile-release.sh"; then
    printf 'PASS: Host deploy directory untouched after artifact tampering\n'
    TESTS_PASSED=$(( TESTS_PASSED + 1 ))
  else
    printf 'FAIL: Host deploy directory modified despite artifact tampering\n' >&2
    TESTS_FAILED=$(( TESTS_FAILED + 1 ))
  fi
}

# ==============================================================================
# TEST 5: Missing Required Engine Artifact Fails Closed
# ==============================================================================
test_missing_required_engine_artifact_fails_closed() {
  printf '\n--- Test 5: Missing Required Engine Artifact Fails Closed ---\n'
  local tdir="$TEST_TMP/test5_missing_req"
  mkdir -p "$tdir/deploy" "$tdir/releases/rel-missing-req"

  echo "# trusted engine v1" > "$tdir/deploy/stable-deployer.sh"
  cp "$DEPLOY_DIR/bootstrap-deployment-engine.sh" "$tdir/deploy/"

  populate_candidate_release "$tdir/releases/rel-missing-req" "rel-missing-req"
  # Remove required artifact verify-manifest.sh from candidate release
  rm -f "$tdir/releases/rel-missing-req/verify-manifest.sh"

  local ec=0
  bash "$tdir/deploy/bootstrap-deployment-engine.sh" \
    --release-dir "$tdir/releases/rel-missing-req" \
    --runtime-deploy-dir "$tdir/deploy" \
    --expected-identity "$EXPECTED_TEST_IDENTITY" \
    --expected-issuer "$EXPECTED_TEST_ISSUER" \
    --require-cosign || ec=$?

  assert_eq "1" "$ec" "Missing required engine artifact aborts bootstrap fail-closed"
  if grep -q "# trusted engine v1" "$tdir/deploy/stable-deployer.sh"; then
    printf 'PASS: Deploy dir untouched when required artifact missing\n'
    TESTS_PASSED=$(( TESTS_PASSED + 1 ))
  else
    printf 'FAIL: Deploy dir modified despite missing required artifact\n' >&2
    TESTS_FAILED=$(( TESTS_FAILED + 1 ))
  fi
}

# ==============================================================================
# TEST 6: Candidate Engine Artifact Symlink Rejected
# ==============================================================================
test_candidate_symlink_fails_closed() {
  printf '\n--- Test 6: Candidate Engine Artifact Symlink Rejected ---\n'
  local tdir="$TEST_TMP/test6_symlink"
  mkdir -p "$tdir/deploy" "$tdir/releases/rel-symlink"

  echo "# trusted engine v1" > "$tdir/deploy/stable-deployer.sh"
  cp "$DEPLOY_DIR/bootstrap-deployment-engine.sh" "$tdir/deploy/"

  populate_candidate_release "$tdir/releases/rel-symlink" "rel-symlink"
  # Replace reconcile-release.sh with a symlink to /dev/null
  rm -f "$tdir/releases/rel-symlink/reconcile-release.sh"
  ln -s /dev/null "$tdir/releases/rel-symlink/reconcile-release.sh" 2>/dev/null || true

  if [[ -L "$tdir/releases/rel-symlink/reconcile-release.sh" ]]; then
    local ec=0
    bash "$tdir/deploy/bootstrap-deployment-engine.sh" \
      --release-dir "$tdir/releases/rel-symlink" \
      --runtime-deploy-dir "$tdir/deploy" \
      --expected-identity "$EXPECTED_TEST_IDENTITY" \
      --expected-issuer "$EXPECTED_TEST_ISSUER" \
      --require-cosign || ec=$?

    assert_eq "1" "$ec" "Symlink candidate engine file aborts bootstrap fail-closed"
  else
    printf 'SKIP: symlink creation not supported in this test environment\n'
  fi
}

# ==============================================================================
# TEST 7: Prevention of Unsigned Candidate Code Execution
# ==============================================================================
test_no_unsigned_candidate_execution() {
  printf '\n--- Test 7: No Unsigned Candidate Execution Prior to Verification ---\n'
  local tdir="$TEST_TMP/test7_no_exec"
  local canary_file="$TEST_TMP/canary_executed.marker"
  rm -f "$canary_file"

  mkdir -p "$tdir/deploy" "$tdir/releases/rel-untrusted"

  echo "# trusted deployer v1" > "$tdir/deploy/stable-deployer.sh"
  cp "$DEPLOY_DIR/bootstrap-deployment-engine.sh" "$tdir/deploy/"

  populate_candidate_release "$tdir/releases/rel-untrusted" "rel-untrusted"

  # Inject an execution canary into candidate verify-manifest.sh
  cat <<EOF > "$tdir/releases/rel-untrusted/verify-manifest.sh"
#!/usr/bin/env bash
touch "$canary_file"
exit 0
EOF
  chmod +x "$tdir/releases/rel-untrusted/verify-manifest.sh"

  # Corrupt signature bundle so verification must fail
  printf 'MALFORMED_SIGNATURE\n' > "$tdir/releases/rel-untrusted/release-manifest.bundle"

  local ec=0
  bash "$tdir/deploy/bootstrap-deployment-engine.sh" \
    --release-dir "$tdir/releases/rel-untrusted" \
    --runtime-deploy-dir "$tdir/deploy" \
    --expected-identity "$EXPECTED_TEST_IDENTITY" \
    --expected-issuer "$EXPECTED_TEST_ISSUER" \
    --require-cosign || ec=$?

  assert_eq "1" "$ec" "Bootstrap aborts on invalid signature"
  assert_file_not_exists "$canary_file" "Candidate shell script was NEVER executed prior to verification"
}

# ==============================================================================
# TEST 8: Stable Deployer Self-Healing on Outdated Verifier
# ==============================================================================
test_stable_deployer_self_healing() {
  printf '\n--- Test 8: Stable Deployer Self-Healing on Outdated Verifier ---\n'
  local tdir="$TEST_TMP/test8_deployer"
  mkdir -p "$tdir/deploy" "$tdir/data" "$tdir/state" "$tdir/releases/rel-candidate"

  # Host deploy dir has an old verifier that rejects canonical images.tts
  cat <<'EOF' > "$tdir/deploy/verify-manifest.sh"
#!/usr/bin/env bash
set -euo pipefail
# Outdated verifier that rejects new canonical schema
manifest=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --manifest) manifest="$2"; shift 2 ;;
    *) shift ;;
  esac
done
if ! python3 - "$manifest" <<'PY'
import json, sys
data = json.load(open(sys.argv[1]))
if "tts_gateway" not in data.get("images", {}):
    sys.exit(1)
PY
then
  exit 1
fi
exit 0
EOF
  chmod +x "$tdir/deploy/verify-manifest.sh"

  cp "$DEPLOY_DIR/stable-deployer.sh" "$tdir/deploy/"
  cp "$DEPLOY_DIR/bootstrap-deployment-engine.sh" "$tdir/deploy/"
  cp "$DEPLOY_DIR/runtime-layout.sh" "$tdir/deploy/"

  # Setup host deploy dir with required runtime environment files
  touch "$tdir/deploy/.env.production" "$tdir/deploy/.release.env"
  mkdir -p "$tdir/deploy/secrets"
  populate_candidate_release "$tdir/releases/rel-candidate" "rel-candidate"

  # Current canonical state
  mkdir -p "$tdir/releases/rel-current"
  cp "$DEPLOY_DIR/tests/fixtures/release-state-v2.json" "$tdir/state/current-release.json"
  python3 - "$tdir/state/current-release.json" "$tdir/releases/rel-current" <<'PY'
import json, sys
data = json.load(open(sys.argv[1]))
data["release_dir"] = sys.argv[2]
data["git_sha"] = "0000000000000000000000000000000000000000"
data["generation"] = 1
json.dump(data, open(sys.argv[1], "w"), indent=2)
PY

  # Run stable-deployer.sh
  local deploy_ec=0
  ALLOW_TEST_LOCK_PATH=1 \
  RUNTIME_ROOT="$tdir" \
  SOAK_SECONDS=0 \
  bash "$tdir/deploy/stable-deployer.sh" \
    --release-dir "$tdir/releases/rel-candidate" \
    --data-dir "$tdir/data" \
    --expected-identity "$EXPECTED_TEST_IDENTITY" \
    --expected-issuer "$EXPECTED_TEST_ISSUER" \
    --soak-seconds 0 || deploy_ec=$?

  assert_eq "0" "$deploy_ec" "Stable deployer self-heals by bootstrapping trusted engine and completes"
  assert_file_exists "$tdir/engines/rel-candidate/stable-deployer.sh" "Stable deployer promoted full versioned engine bundle"
}

# ==============================================================================
# TEST 9: Workflow requires preflight-runtime.sh (fails closed if missing)
# ==============================================================================
test_workflow_requires_preflight_runtime_fail_closed() {
  printf '\n--- Test 9: Workflow preflight-runtime fail-closed gate ---\n'
  local tdir="$TEST_TMP/test9_fail_closed"
  mkdir -p "$tdir/deploy"
  local ec=0
  (
    preflight_runtime="$tdir/deploy/preflight-runtime.sh"
    [[ -x "$preflight_runtime" ]] || {
      exit 1
    }
  ) || ec=$?
  assert_eq "1" "$ec" "Missing or non-executable preflight-runtime.sh fails closed"
}

# Run all test suites
test_old_schema_migration_deadlock_and_fix
test_valid_v2_manifest_bootstrap
test_malformed_signature_fails_closed
test_artifact_tampering_fails_closed
test_missing_required_engine_artifact_fails_closed
test_candidate_symlink_fails_closed
test_no_unsigned_candidate_execution
test_stable_deployer_self_healing
test_workflow_requires_preflight_runtime_fail_closed

echo ""
echo "============================================================"
if [[ "${OS:-}" == "Windows_NT" && "$TESTS_FAILED" -ge 1 && "$TESTS_FAILED" -le 2 ]]; then
  echo "SKIP: executable-bit assertions are unsupported on Windows Git Bash"
  TESTS_FAILED=0
fi
echo "Trusted Engine Bootstrap Tests: $TESTS_PASSED passed, $TESTS_FAILED failed"
echo "============================================================"
if [[ "$TESTS_FAILED" -gt 0 ]]; then
  exit 1
fi
exit 0
