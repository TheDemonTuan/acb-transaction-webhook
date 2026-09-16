#!/usr/bin/env bash
# deploy/tests/test_release_baseline.sh
# Tests verify-release-baseline.sh contract:
# - Baseline identity match between signed manifest base and canonical state passes.
# - Changed generation fails with STALE_RELEASE_BASELINE.
# - Changed SHA fails with STALE_RELEASE_BASELINE.
# - Missing base in signed manifest fails unless explicit bootstrap is permitted.
# - Malformed identity in manifest base or canonical state fails closed.
# - Explicit documented bootstrap permits deployment when canonical state or manifest baseline is absent.
set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR="$(cd -- "$SCRIPT_DIR/.." && pwd)"

TEST_TMP="$(mktemp -d "${TMPDIR:-/tmp}/acb-baseline-test.XXXXXX")"
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

assert_contains() {
  local haystack="$1"
  local needle="$2"
  local msg="$3"
  if [[ "$haystack" != *"$needle"* ]]; then
    printf 'FAIL: %s (did not find "%s" in output)\n' "$msg" "$needle" >&2
    printf 'Output was:\n%s\n' "$haystack" >&2
    TESTS_FAILED=$(( TESTS_FAILED + 1 ))
    return 1
  fi
  printf 'PASS: %s\n' "$msg"
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
  return 0
}

BASE_VERIFY_SCRIPT="$DEPLOY_DIR/verify-release-baseline.sh"
[[ -f "$BASE_VERIFY_SCRIPT" ]] || {
  printf 'Error: verify-release-baseline.sh not found at %s\n' "$BASE_VERIFY_SCRIPT" >&2
  exit 1
}

CANONICAL_SHA="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
OTHER_SHA="bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

printf "========================================================\n"
printf "Running Release Baseline Verification Tests\n"
printf "========================================================\n\n"

# 1. Test: same (manifest base matches canonical state)
printf "1. Testing baseline match (same)...\n"
t1_dir="$TEST_TMP/t1_same"
mkdir -p "$t1_dir"
cat <<EOF > "$t1_dir/release-manifest.json"
{
  "release_id": "rel-2026-09-16-01",
  "git_sha": "$CANONICAL_SHA",
  "base": {
    "git_sha": "$CANONICAL_SHA",
    "generation": 42
  }
}
EOF
cat <<EOF > "$t1_dir/current-release.json"
{
  "generation": 42,
  "git_sha": "$CANONICAL_SHA",
  "status": "COMPLETED"
}
EOF

ec=0
output="$(bash "$BASE_VERIFY_SCRIPT" \
  --manifest "$t1_dir/release-manifest.json" \
  --state "$t1_dir/current-release.json" 2>&1)" || ec=$?

assert_eq "0" "$ec" "Same baseline generation and git_sha succeeds"
assert_contains "$output" "RELEASE_BASELINE_OK" "Output confirms RELEASE_BASELINE_OK"

# 2. Test: changed generation
printf "\n2. Testing changed generation...\n"
t2_dir="$TEST_TMP/t2_changed_gen"
mkdir -p "$t2_dir"
cat <<EOF > "$t2_dir/release-manifest.json"
{
  "release_id": "rel-2026-09-16-02",
  "git_sha": "$CANONICAL_SHA",
  "base": {
    "git_sha": "$CANONICAL_SHA",
    "generation": 42
  }
}
EOF
cat <<EOF > "$t2_dir/current-release.json"
{
  "generation": 43,
  "git_sha": "$CANONICAL_SHA",
  "status": "COMPLETED"
}
EOF

ec=0
output="$(bash "$BASE_VERIFY_SCRIPT" \
  --manifest "$t2_dir/release-manifest.json" \
  --state "$t2_dir/current-release.json" 2>&1)" || ec=$?

assert_eq "1" "$ec" "Changed generation fails closed"
assert_contains "$output" "STALE_RELEASE_BASELINE" "Output contains STALE_RELEASE_BASELINE error marker"
assert_contains "$output" "generation changed after build" "Error explains generation mismatch"

# 3. Test: changed SHA
printf "\n3. Testing changed SHA...\n"
t3_dir="$TEST_TMP/t3_changed_sha"
mkdir -p "$t3_dir"
cat <<EOF > "$t3_dir/release-manifest.json"
{
  "release_id": "rel-2026-09-16-03",
  "git_sha": "$CANONICAL_SHA",
  "base": {
    "git_sha": "$CANONICAL_SHA",
    "generation": 42
  }
}
EOF
cat <<EOF > "$t3_dir/current-release.json"
{
  "generation": 42,
  "git_sha": "$OTHER_SHA",
  "status": "COMPLETED"
}
EOF

ec=0
output="$(bash "$BASE_VERIFY_SCRIPT" \
  --manifest "$t3_dir/release-manifest.json" \
  --state "$t3_dir/current-release.json" 2>&1)" || ec=$?

assert_eq "1" "$ec" "Changed git_sha fails closed"
assert_contains "$output" "STALE_RELEASE_BASELINE" "Output contains STALE_RELEASE_BASELINE error marker"
assert_contains "$output" "git_sha changed after build" "Error explains git_sha mismatch"

# 4. Test: missing base without bootstrap
printf "\n4. Testing missing base without bootstrap...\n"
t4_dir="$TEST_TMP/t4_missing_base"
mkdir -p "$t4_dir"
cat <<EOF > "$t4_dir/release-manifest.json"
{
  "release_id": "rel-2026-09-16-04",
  "git_sha": "$CANONICAL_SHA"
}
EOF
cat <<EOF > "$t4_dir/current-release.json"
{
  "generation": 42,
  "git_sha": "$CANONICAL_SHA",
  "status": "COMPLETED"
}
EOF

ec=0
output="$(bash "$BASE_VERIFY_SCRIPT" \
  --manifest "$t4_dir/release-manifest.json" \
  --state "$t4_dir/current-release.json" 2>&1)" || ec=$?

assert_eq "1" "$ec" "Missing base fails when bootstrap not permitted"
assert_contains "$output" "STALE_RELEASE_BASELINE" "Missing base outputs STALE_RELEASE_BASELINE"

# 5. Test: missing base with explicit documented bootstrap (--allow-bootstrap)
printf "\n5. Testing missing base with --allow-bootstrap...\n"
ec=0
output="$(bash "$BASE_VERIFY_SCRIPT" \
  --manifest "$t4_dir/release-manifest.json" \
  --state "$t4_dir/current-release.json" \
  --allow-bootstrap 2>&1)" || ec=$?

assert_eq "0" "$ec" "Missing base succeeds with explicit --allow-bootstrap flag"
assert_contains "$output" "BOOTSTRAP" "Output confirms bootstrap mode permitted"

# 6. Test: missing base with ALLOW_BOOTSTRAP=1 environment variable
printf "\n6. Testing missing base with ALLOW_BOOTSTRAP=1 env var...\n"
ec=0
output="$(ALLOW_BOOTSTRAP=1 bash "$BASE_VERIFY_SCRIPT" \
  --manifest "$t4_dir/release-manifest.json" \
  --state "$t4_dir/current-release.json" 2>&1)" || ec=$?

assert_eq "0" "$ec" "Missing base succeeds with ALLOW_BOOTSTRAP=1"
assert_contains "$output" "BOOTSTRAP" "Output confirms bootstrap mode permitted"

# 7. Test: missing canonical state without bootstrap
printf "\n7. Testing missing canonical state without bootstrap...\n"
t7_dir="$TEST_TMP/t7_missing_state"
mkdir -p "$t7_dir"
cat <<EOF > "$t7_dir/release-manifest.json"
{
  "release_id": "rel-2026-09-16-07",
  "git_sha": "$CANONICAL_SHA",
  "base": {
    "git_sha": "$CANONICAL_SHA",
    "generation": 42
  }
}
EOF

ec=0
output="$(bash "$BASE_VERIFY_SCRIPT" \
  --manifest "$t7_dir/release-manifest.json" \
  --state "$t7_dir/non-existent-state.json" 2>&1)" || ec=$?

assert_eq "1" "$ec" "Missing canonical state fails without --allow-bootstrap"
assert_contains "$output" "canonical release state missing" "Error indicates canonical state missing"

# 8. Test: missing canonical state with explicit documented bootstrap
printf "\n8. Testing missing canonical state with --allow-bootstrap...\n"
ec=0
output="$(bash "$BASE_VERIFY_SCRIPT" \
  --manifest "$t7_dir/release-manifest.json" \
  --state "$t7_dir/non-existent-state.json" \
  --allow-bootstrap 2>&1)" || ec=$?

assert_eq "0" "$ec" "Missing canonical state succeeds with --allow-bootstrap"
assert_contains "$output" "BOOTSTRAP" "Output confirms bootstrap mode permitted"

# 9. Test: malformed identity - non-hex git_sha in manifest base
printf "\n9. Testing malformed manifest base.git_sha...\n"
t9_dir="$TEST_TMP/t9_malformed_sha"
mkdir -p "$t9_dir"
cat <<EOF > "$t9_dir/release-manifest.json"
{
  "release_id": "rel-2026-09-16-09",
  "base": {
    "git_sha": "not-a-40-hex-sha",
    "generation": 42
  }
}
EOF
cat <<EOF > "$t9_dir/current-release.json"
{
  "generation": 42,
  "git_sha": "$CANONICAL_SHA"
}
EOF

ec=0
output="$(bash "$BASE_VERIFY_SCRIPT" \
  --manifest "$t9_dir/release-manifest.json" \
  --state "$t9_dir/current-release.json" 2>&1)" || ec=$?

assert_eq "1" "$ec" "Malformed manifest git_sha fails closed"
assert_contains "$output" "malformed identity" "Output reports malformed identity"

# 10. Test: malformed identity - negative generation in manifest base
printf "\n10. Testing malformed manifest base.generation (negative)...\n"
t10_dir="$TEST_TMP/t10_malformed_gen"
mkdir -p "$t10_dir"
cat <<EOF > "$t10_dir/release-manifest.json"
{
  "release_id": "rel-2026-09-16-10",
  "base": {
    "git_sha": "$CANONICAL_SHA",
    "generation": -1
  }
}
EOF

ec=0
output="$(bash "$BASE_VERIFY_SCRIPT" \
  --manifest "$t10_dir/release-manifest.json" \
  --state "$t1_dir/current-release.json" 2>&1)" || ec=$?

assert_eq "1" "$ec" "Negative generation in manifest base fails closed"
assert_contains "$output" "malformed identity" "Output reports malformed identity"

# 11. Test: malformed identity - string generation in manifest base
printf "\n11. Testing malformed manifest base.generation (string)...\n"
t11_dir="$TEST_TMP/t11_string_gen"
mkdir -p "$t11_dir"
cat <<EOF > "$t11_dir/release-manifest.json"
{
  "release_id": "rel-2026-09-16-11",
  "base": {
    "git_sha": "$CANONICAL_SHA",
    "generation": "forty-two"
  }
}
EOF

ec=0
output="$(bash "$BASE_VERIFY_SCRIPT" \
  --manifest "$t11_dir/release-manifest.json" \
  --state "$t1_dir/current-release.json" 2>&1)" || ec=$?

assert_eq "1" "$ec" "String generation in manifest base fails closed"
assert_contains "$output" "malformed identity" "Output reports malformed identity"

# 12. Test: malformed identity - base is not a dictionary/object
printf "\n12. Testing malformed manifest base (not an object)...\n"
t12_dir="$TEST_TMP/t12_base_not_obj"
mkdir -p "$t12_dir"
cat <<EOF > "$t12_dir/release-manifest.json"
{
  "release_id": "rel-2026-09-16-12",
  "base": "invalid-base-string"
}
EOF

ec=0
output="$(bash "$BASE_VERIFY_SCRIPT" \
  --manifest "$t12_dir/release-manifest.json" \
  --state "$t1_dir/current-release.json" 2>&1)" || ec=$?

assert_eq "1" "$ec" "Non-object manifest base fails closed"
assert_contains "$output" "malformed identity" "Output reports malformed identity"

# 13. Test: malformed identity - canonical state invalid git_sha
printf "\n13. Testing malformed canonical state git_sha...\n"
t13_dir="$TEST_TMP/t13_canonical_malformed_sha"
mkdir -p "$t13_dir"
cat <<EOF > "$t13_dir/current-release.json"
{
  "generation": 42,
  "git_sha": "short-sha"
}
EOF

ec=0
output="$(bash "$BASE_VERIFY_SCRIPT" \
  --manifest "$t1_dir/release-manifest.json" \
  --state "$t13_dir/current-release.json" 2>&1)" || ec=$?

assert_eq "1" "$ec" "Malformed canonical state git_sha fails closed"
assert_contains "$output" "malformed identity" "Output reports malformed identity"

# 14. Test: malformed identity - canonical state missing generation
printf "\n14. Testing malformed canonical state missing generation...\n"
t14_dir="$TEST_TMP/t14_canonical_missing_gen"
mkdir -p "$t14_dir"
cat <<EOF > "$t14_dir/current-release.json"
{
  "git_sha": "$CANONICAL_SHA"
}
EOF

ec=0
output="$(bash "$BASE_VERIFY_SCRIPT" \
  --manifest "$t1_dir/release-manifest.json" \
  --state "$t14_dir/current-release.json" 2>&1)" || ec=$?

assert_eq "1" "$ec" "Missing generation in canonical state fails closed"
assert_contains "$output" "malformed identity" "Output reports malformed identity"

# 15. Test: conflicting baseline fails even with --allow-bootstrap
printf "\n15. Testing conflicting baseline with --allow-bootstrap...\n"
ec=0
output="$(bash "$BASE_VERIFY_SCRIPT" \
  --manifest "$t2_dir/release-manifest.json" \
  --state "$t2_dir/current-release.json" \
  --allow-bootstrap 2>&1)" || ec=$?

assert_eq "1" "$ec" "Mismatched baseline still fails when --allow-bootstrap is set"
assert_contains "$output" "STALE_RELEASE_BASELINE" "Output confirms STALE_RELEASE_BASELINE on stale baseline"

printf "\n========================================================\n"
printf "Results: %d Passed, %d Failed\n" "$TESTS_PASSED" "$TESTS_FAILED"
printf "========================================================\n"

if (( TESTS_FAILED > 0 )); then
  exit 1
fi
