#!/usr/bin/env bash
# deploy/tests/test_release_state.sh
# Exhaustive test suite for canonical release state schema v2 validator, builder, and reader.
set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR="$(cd -- "$SCRIPT_DIR/.." && pwd)"

TEST_TMP="$(mktemp -d "${TMPDIR:-/tmp}/acb-release-state-tests.XXXXXX")"
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

setup_fixture() {
  local tdir="$1"
  mkdir -p "$tdir/releases/rel-current" "$tdir/releases/rel-prev" "$tdir/state"
  
  # Create valid release-manifest.json in release_dir
  cat <<'EOF' > "$tdir/releases/rel-current/release-manifest.json"
{
  "release_id": "rel-current",
  "git_sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "images": {
    "gateway": "ghcr.io/test/gateway@sha256:0000000000000000000000000000000000000000000000000000000000000005",
    "frontend": "ghcr.io/test/frontend@sha256:0000000000000000000000000000000000000000000000000000000000000004",
    "worker": "ghcr.io/test/worker@sha256:0000000000000000000000000000000000000000000000000000000000000008",
    "dbtool": "ghcr.io/test/dbtool@sha256:0000000000000000000000000000000000000000000000000000000000000003",
    "auth_browser": "ghcr.io/test/auth-browser@sha256:0000000000000000000000000000000000000000000000000000000000000001",
    "tts": "ghcr.io/test/tts@sha256:0000000000000000000000000000000000000000000000000000000000000007",
    "bark": "ghcr.io/test/bark@sha256:0000000000000000000000000000000000000000000000000000000000000002"
  },
  "artifacts": {
    "compose.prod.yaml": "1111111111111111111111111111111111111111111111111111111111111111"
  }
}
EOF
  touch "$tdir/releases/rel-prev/release-manifest.json"

  local m_sha
  m_sha="$(sha256sum "$tdir/releases/rel-current/release-manifest.json" | awk '{print $1}')"

  cat <<EOF > "$tdir/state/valid-v2.json"
{
  "schema_version": 2,
  "generation": 42,
  "release_id": "rel-current",
  "release_dir": "$tdir/releases/rel-current",
  "git_sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "manifest_sha256": "$m_sha",
  "status": "COMPLETED",
  "previous": {
    "generation": 41,
    "release_id": "rel-prev",
    "release_dir": "$tdir/releases/rel-prev"
  },
  "active_slots": {
    "gateway": "green",
    "frontend": "blue"
  },
  "images": {
    "gateway": {
      "blue": "ghcr.io/test/gateway@sha256:0000000000000000000000000000000000000000000000000000000000000005",
      "green": "ghcr.io/test/gateway@sha256:0000000000000000000000000000000000000000000000000000000000000006"
    },
    "frontend": "ghcr.io/test/frontend@sha256:0000000000000000000000000000000000000000000000000000000000000004",
    "worker": "ghcr.io/test/worker@sha256:0000000000000000000000000000000000000000000000000000000000000008",
    "dbtool": "ghcr.io/test/dbtool@sha256:0000000000000000000000000000000000000000000000000000000000000003",
    "auth_browser": "ghcr.io/test/auth-browser@sha256:0000000000000000000000000000000000000000000000000000000000000001",
    "tts": "ghcr.io/test/tts@sha256:0000000000000000000000000000000000000000000000000000000000000007",
    "bark": "ghcr.io/test/bark@sha256:0000000000000000000000000000000000000000000000000000000000000002"
  },
  "config": {
    "compose_bundle_sha256": "1111111111111111111111111111111111111111111111111111111111111111",
    "traefik_template_sha256": "2222222222222222222222222222222222222222222222222222222222222222",
    "platform_bundle_sha256": "3333333333333333333333333333333333333333333333333333333333333333"
  }
}
EOF
}

test_valid_schema_v2() {
  local tdir="$TEST_TMP/valid"
  setup_fixture "$tdir"
  export RUNTIME_RELEASES_DIR="$tdir/releases"

  local exit_code=0
  python3 "$DEPLOY_DIR/release-state.py" validate "$tdir/state/valid-v2.json" || exit_code=$?
  assert_eq "0" "$exit_code" "Valid schema v2 passes validation"

  local git_sha
  git_sha="$(python3 "$DEPLOY_DIR/release-state.py" get "$tdir/state/valid-v2.json" git_sha)"
  assert_eq "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" "$git_sha" "get git_sha returns exact value"

  local gw_slot
  gw_slot="$(python3 "$DEPLOY_DIR/release-state.py" get "$tdir/state/valid-v2.json" active_slots.gateway)"
  assert_eq "green" "$gw_slot" "get active_slots.gateway returns green"

  local worker_img
  worker_img="$(python3 "$DEPLOY_DIR/release-state.py" get "$tdir/state/valid-v2.json" images.worker)"
  assert_eq "ghcr.io/test/worker@sha256:0000000000000000000000000000000000000000000000000000000000000008" "$worker_img" "get images.worker returns exact digest"
}

test_reject_invalid_states() {
  local tdir="$TEST_TMP/invalids"
  setup_fixture "$tdir"
  export RUNTIME_RELEASES_DIR="$tdir/releases"

  # 1. Reject previous generation >= current
  local bad_gen="$tdir/state/bad-gen.json"
  python3 -c "
import json
d = json.load(open('$tdir/state/valid-v2.json'))
d['previous']['generation'] = 45
json.dump(d, open('$bad_gen', 'w'))
"
  local ec=0
  python3 "$DEPLOY_DIR/release-state.py" validate "$bad_gen" 2>/dev/null || ec=$?
  assert_eq "1" "$ec" "Rejects previous generation >= current"

  # 2. Reject non-immutable image
  local bad_img="$tdir/state/bad-img.json"
  python3 -c "
import json
d = json.load(open('$tdir/state/valid-v2.json'))
d['images']['worker'] = 'ghcr.io/test/worker:latest'
json.dump(d, open('$bad_img', 'w'))
"
  ec=0
  python3 "$DEPLOY_DIR/release-state.py" validate "$bad_img" 2>/dev/null || ec=$?
  assert_eq "1" "$ec" "Rejects mutable image tag"

  # 3. Reject release_dir outside RUNTIME_RELEASES_DIR
  local bad_dir="$tdir/state/bad-dir.json"
  python3 -c "
import json
d = json.load(open('$tdir/state/valid-v2.json'))
d['release_dir'] = '/tmp/outside-releases'
json.dump(d, open('$bad_dir', 'w'))
"
  ec=0
  python3 "$DEPLOY_DIR/release-state.py" validate "$bad_dir" 2>/dev/null || ec=$?
  assert_eq "1" "$ec" "Rejects release_dir outside RUNTIME_RELEASES_DIR"

  # 4. Reject release_dir symlink
  local symlink_dir="$tdir/releases/rel-symlink"
  ln -s "$tdir/releases/rel-current" "$symlink_dir"
  local bad_symlink="$tdir/state/bad-symlink.json"
  python3 -c "
import json
d = json.load(open('$tdir/state/valid-v2.json'))
d['release_dir'] = '$symlink_dir'
json.dump(d, open('$bad_symlink', 'w'))
"
  ec=0
  python3 "$DEPLOY_DIR/release-state.py" validate "$bad_symlink" 2>/dev/null || ec=$?
  assert_eq "1" "$ec" "Rejects symlinked release_dir"

  # 5. Reject manifest SHA mismatch
  local bad_msha="$tdir/state/bad-msha.json"
  python3 -c "
import json
d = json.load(open('$tdir/state/valid-v2.json'))
d['manifest_sha256'] = '0000000000000000000000000000000000000000000000000000000000000000'
json.dump(d, open('$bad_msha', 'w'))
"
  ec=0
  python3 "$DEPLOY_DIR/release-state.py" validate "$bad_msha" 2>/dev/null || ec=$?
  assert_eq "1" "$ec" "Rejects manifest_sha256 mismatch"
}

test_build_candidate_state() {
  local tdir="$TEST_TMP/build"
  setup_fixture "$tdir"
  export RUNTIME_RELEASES_DIR="$tdir/releases"

  local candidate_state="$tdir/state/built-candidate.json"
  python3 "$DEPLOY_DIR/release-state.py" build \
    --previous "$tdir/state/valid-v2.json" \
    --release-dir "$tdir/releases/rel-current" \
    --manifest "$tdir/releases/rel-current/release-manifest.json" \
    --gateway-slot "blue" \
    --frontend-slot "green" \
    --output "$candidate_state"

  local ec=0
  python3 "$DEPLOY_DIR/release-state.py" validate "$candidate_state" || ec=$?
  assert_eq "0" "$ec" "Built candidate state passes schema v2 validation"

  local gen
  gen="$(python3 "$DEPLOY_DIR/release-state.py" get "$candidate_state" generation)"
  assert_eq "43" "$gen" "Built candidate advances generation from 42 to 43"

  local prev_id
  prev_id="$(python3 "$DEPLOY_DIR/release-state.py" get "$candidate_state" previous.release_id)"
  assert_eq "rel-current" "$prev_id" "Built candidate records exact previous release_id"

  # Test export-env
  local env_out="$tdir/state/.release.env"
  python3 "$DEPLOY_DIR/release-state.py" export-env "$candidate_state" --output "$env_out"
  assert_eq "ACTIVE_GATEWAY_SLOT=blue" "$(grep '^ACTIVE_GATEWAY_SLOT=' "$env_out")" "export-env contains ACTIVE_GATEWAY_SLOT=blue"
  assert_eq "CANONICAL_GENERATION=43" "$(grep '^CANONICAL_GENERATION=' "$env_out")" "export-env contains CANONICAL_GENERATION=43"
}

test_task2_canonical_naming_and_advance_doc_only() {
  local tdir="$TEST_TMP/task2_test"
  mkdir -p "$tdir/releases/rel-current/compose" "$tdir/state" "$tdir/data" "$tdir/secrets"
  export RUNTIME_RELEASES_DIR="$tdir/releases"

  local tts_cand="ghcr.io/test/tts@sha256:9999999999999999999999999999999999999999999999999999999999999999"
  local tts_prev="ghcr.io/test/tts@sha256:0000000000000000000000000000000000000000000000000000000000000007"
  local fe_img="ghcr.io/test/frontend@sha256:0000000000000000000000000000000000000000000000000000000000000004"
  local gw_img="ghcr.io/test/gateway@sha256:0000000000000000000000000000000000000000000000000000000000000005"
  local wk_img="ghcr.io/test/worker@sha256:0000000000000000000000000000000000000000000000000000000000000008"
  local db_img="ghcr.io/test/dbtool@sha256:0000000000000000000000000000000000000000000000000000000000000003"
  local br_img="ghcr.io/test/auth-browser@sha256:0000000000000000000000000000000000000000000000000000000000000001"
  local bk_img="ghcr.io/test/bark@sha256:0000000000000000000000000000000000000000000000000000000000000002"

  local manifest_file="$tdir/releases/rel-current/release-manifest.json"
  bash "$DEPLOY_DIR/generate-release-manifest.sh" \
    --git-sha "2222222222222222222222222222222222222222" \
    --release-id "rel-task2" \
    --promotion-scope '{"tts":true}' \
    --frontend-image "$fe_img" \
    --gateway-image "$gw_img" \
    --worker-image "$wk_img" \
    --dbtool-image "$db_img" \
    --auth-browser-image "$br_img" \
    --tts-image "$tts_cand" \
    --bark-image "$bk_img" \
    --deploy-dir "$DEPLOY_DIR" \
    --output "$manifest_file"

  # 1. Assert new manifest contains "tts" and NOT "tts_gateway"
  local has_tts_key
  has_tts_key="$(python3 -c 'import json, sys; d = json.load(open(sys.argv[1])); print("tts" in d.get("images", {}))' "$manifest_file")"
  assert_eq "True" "$has_tts_key" "Real manifest generator writes images.tts"

  local has_tts_gw_key
  has_tts_gw_key="$(python3 -c 'import json, sys; d = json.load(open(sys.argv[1])); print("tts_gateway" in d.get("images", {}))' "$manifest_file")"
  assert_eq "False" "$has_tts_gw_key" "Real manifest generator does not emit tts_gateway"

  # 2. Assert verify-manifest.sh passes on real manifest
  local ec=0
  local v_out
  v_out="$(bash "$DEPLOY_DIR/verify-manifest.sh" --manifest "$manifest_file" --deploy-dir "$DEPLOY_DIR" 2>&1)" || ec=$?
  if [[ "$ec" -ne 0 ]]; then
    printf "verify-manifest error: %s\n" "$v_out" >&2
  fi
  assert_eq "0" "$ec" "verify-manifest.sh succeeds on generated manifest"

  # 3. Assert old tts_gateway manifest is supported for backwards compatibility
  local old_manifest="$tdir/releases/rel-current/old-manifest.json"
  sed 's/"tts":/"tts_gateway":/' "$manifest_file" > "$old_manifest"
  ec=0
  v_out="$(bash "$DEPLOY_DIR/verify-manifest.sh" --manifest "$old_manifest" --deploy-dir "$DEPLOY_DIR" 2>&1)" || ec=$?
  if [[ "$ec" -ne 0 ]]; then
    printf "verify-manifest old error: %s\n" "$v_out" >&2
  fi
  assert_eq "0" "$ec" "verify-manifest.sh accepts legacy tts_gateway key for compatibility"

  # 4. Previous canonical state with explicit legacy frontend
  local prev_state="$tdir/state/current-release.json"
  cat <<EOF > "$prev_state"
{
  "schema_version": 2,
  "generation": 50,
  "release_id": "rel-prev50",
  "release_dir": "$tdir/releases/rel-current",
  "git_sha": "1111111111111111111111111111111111111111",
  "manifest_sha256": "$(sha256sum "$manifest_file" | awk '{print $1}')",
  "status": "COMPLETED",
  "active_slots": {
    "gateway": "blue",
    "frontend": "legacy"
  },
  "images": {
    "gateway": { "blue": "$gw_img", "green": "$gw_img" },
    "frontend": "$fe_img",
    "worker": "$wk_img",
    "dbtool": "$db_img",
    "auth_browser": "$br_img",
    "tts": "$tts_prev",
    "bark": "$bk_img"
  }
}
EOF

  # 5. Build candidate state with TTS promotion
  local cand_state="$tdir/state/candidate-tts.json"
  python3 "$DEPLOY_DIR/release-state.py" build \
    --previous "$prev_state" \
    --release-dir "$tdir/releases/rel-current" \
    --manifest "$manifest_file" \
    --gateway-slot "blue" \
    --frontend-slot "legacy" \
    --scope "tts" \
    --output "$cand_state"

  ec=0
  python3 "$DEPLOY_DIR/release-state.py" validate "$cand_state" || ec=$?
  assert_eq "0" "$ec" "TTS candidate state passes schema v2 validation"

  local built_tts
  built_tts="$(python3 "$DEPLOY_DIR/release-state.py" get "$cand_state" images.tts)"
  assert_eq "$tts_cand" "$built_tts" "TTS candidate state contains candidate digest for images.tts"

  local fe_topo
  fe_topo="$(python3 "$DEPLOY_DIR/release-state.py" get "$cand_state" active_slots.frontend)"
  assert_eq "legacy" "$fe_topo" "Frontend topology remains explicit 'legacy'"

  # 6. Test advance-doc-only
  local doc_state="$tdir/state/doc-state.json"
  python3 "$DEPLOY_DIR/release-state.py" advance-doc-only \
    --previous "$prev_state" \
    --release-dir "$tdir/releases/rel-current" \
    --manifest "$manifest_file" \
    --output "$doc_state"

  ec=0
  python3 "$DEPLOY_DIR/release-state.py" validate "$doc_state" || ec=$?
  assert_eq "0" "$ec" "advance-doc-only state passes schema v2 validation"

  local doc_gen
  doc_gen="$(python3 "$DEPLOY_DIR/release-state.py" get "$doc_state" generation)"
  assert_eq "51" "$doc_gen" "advance-doc-only advances generation from 50 to 51"

  local doc_prev_id
  doc_prev_id="$(python3 "$DEPLOY_DIR/release-state.py" get "$doc_state" previous.release_id)"
  assert_eq "rel-prev50" "$doc_prev_id" "advance-doc-only records exact previous release_id"

  local doc_fe_slot
  doc_fe_slot="$(python3 "$DEPLOY_DIR/release-state.py" get "$doc_state" active_slots.frontend)"
  assert_eq "legacy" "$doc_fe_slot" "advance-doc-only preserves explicit legacy frontend slot"

  local doc_tts
  doc_tts="$(python3 "$DEPLOY_DIR/release-state.py" get "$doc_state" images.tts)"
  assert_eq "$tts_prev" "$doc_tts" "advance-doc-only preserves previous runtime images (no mutations)"
}

test_valid_schema_v2
test_reject_invalid_states
test_build_candidate_state
test_task2_canonical_naming_and_advance_doc_only

echo "Passed: $TESTS_PASSED, Failed: $TESTS_FAILED"
[[ "$TESTS_FAILED" -eq 0 ]] || exit 1
