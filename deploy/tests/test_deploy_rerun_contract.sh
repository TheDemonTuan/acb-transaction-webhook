#!/usr/bin/env bash
# deploy/tests/test_deploy_rerun_contract.sh
# Tests collision-safe immutable release staging contract (Task 6):
# 1. remote release absent                 -> install
# 2. remote release exists and identical   -> reuse
# 3. remote release exists and differs     -> RELEASE_ID_COLLISION
# 4. partial .upload directory exists      -> clean only that attempt's staging dir
set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR="$(cd -- "$SCRIPT_DIR/.." && pwd)"
STAGE_SCRIPT="$DEPLOY_DIR/stage-immutable-release.sh"

TEST_TMP="$(mktemp -d "${TMPDIR:-/tmp}/acb-rerun-contract.XXXXXX")"
# shellcheck disable=SC2329
cleanup() {
  rm -rf "$TEST_TMP"
}
trap cleanup EXIT

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
    printf 'FAIL: %s (expected to find "%s")\n' "$msg" "$needle" >&2
    TESTS_FAILED=$(( TESTS_FAILED + 1 ))
    return 1
  fi
  printf 'PASS: %s\n' "$msg"
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
  return 0
}

assert_exists() {
  local path="$1"
  local msg="$2"
  if [[ ! -e "$path" && ! -L "$path" ]]; then
    printf 'FAIL: %s (path does not exist: %s)\n' "$msg" "$path" >&2
    TESTS_FAILED=$(( TESTS_FAILED + 1 ))
    return 1
  fi
  printf 'PASS: %s\n' "$msg"
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
  return 0
}

assert_not_exists() {
  local path="$1"
  local msg="$2"
  if [[ -e "$path" || -L "$path" ]]; then
    printf 'FAIL: %s (path unexpectedly exists: %s)\n' "$msg" "$path" >&2
    TESTS_FAILED=$(( TESTS_FAILED + 1 ))
    return 1
  fi
  printf 'PASS: %s\n' "$msg"
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
  return 0
}

# Helper to build a valid signed release bundle
create_test_bundle() {
  local dir="$1"
  local rel_id="$2"
  local extra="${3:-}"

  mkdir -p "$dir"
  printf 'version: "3"\nservices:\n  gateway: {}\n%s' "$extra" > "$dir/compose.prod.yaml"
  printf '#!/usr/bin/env bash\necho deploy\n%s' "$extra" > "$dir/deploy.sh"
  printf '#!/usr/bin/env bash\necho lib\n%s' "$extra" > "$dir/lib.sh"

  local h_compose h_deploy h_lib
  h_compose="$(sha256sum "$dir/compose.prod.yaml" | awk '{print $1}')"
  h_deploy="$(sha256sum "$dir/deploy.sh" | awk '{print $1}')"
  h_lib="$(sha256sum "$dir/lib.sh" | awk '{print $1}')"

  cat <<EOF > "$dir/release-manifest.json"
{
  "release_id": "$rel_id",
  "git_sha": "0123456789abcdef0123456789abcdef01234567",
  "created_at": "2026-09-16T12:00:00Z",
  "artifacts": {
    "compose.prod.yaml": "$h_compose",
    "deploy.sh": "$h_deploy",
    "lib.sh": "$h_lib"
  }
}
EOF
  printf 'bundle-signature-content-%s\n' "$extra" > "$dir/release-manifest.bundle"
}

printf "========================================================\n"
printf "Running Task 6 Staging Rerun Contract Tests\n"
printf "========================================================\n\n"

# -----------------------------------------------------------------------------
# Scenario 1: remote release absent -> install atomically
# -----------------------------------------------------------------------------
printf '%s\n' "--- Scenario 1: Remote release absent -> install atomically ---"
rel_id_1="rel-test-1"
stage_dir_1="$TEST_TMP/releases/.${rel_id_1}.upload-101-1"
dest_dir_1="$TEST_TMP/releases/${rel_id_1}"

create_test_bundle "$stage_dir_1" "$rel_id_1"
out_1=""
exit_1=0
out_1="$(bash "$STAGE_SCRIPT" --staging-dir "$stage_dir_1" --release-dir "$dest_dir_1" --skip-cosign 2>&1)" || exit_1=$?

assert_eq "0" "$exit_1" "Scenario 1: staging succeeds when remote release absent"
assert_exists "$dest_dir_1" "Scenario 1: release directory exists after atomic install"
assert_exists "$dest_dir_1/compose.prod.yaml" "Scenario 1: artifact exists in release directory"
assert_not_exists "$stage_dir_1" "Scenario 1: staging upload directory moved"
assert_contains "$out_1" "Successfully installed release" "Scenario 1: confirmation output present"

# Verify permissions
if [[ "$(uname -s)" =~ Linux|Darwin ]]; then
  perms="$(stat -c "%a" "$dest_dir_1" 2>/dev/null || stat -f "%OLp" "$dest_dir_1" 2>/dev/null || true)"
  if [[ -n "$perms" ]]; then
    # Verify group/others do not have write permission
    last_two="${perms: -2}"
    if [[ "$last_two" == *2* || "$last_two" == *3* || "$last_two" == *6* || "$last_two" == *7* ]]; then
      printf 'FAIL: release directory has group/other write permissions: %s\n' "$perms" >&2
      TESTS_FAILED=$(( TESTS_FAILED + 1 ))
    else
      printf 'PASS: release directory is not group/other writable (%s)\n' "$perms"
      TESTS_PASSED=$(( TESTS_PASSED + 1 ))
    fi
  fi
fi

# -----------------------------------------------------------------------------
# Scenario 2: remote release exists and identical -> reuse
# -----------------------------------------------------------------------------
printf '\n%s\n' "--- Scenario 2: Remote release exists and identical -> reuse ---"
rel_id_2="rel-test-2"
stage_dir_2="$TEST_TMP/releases/.${rel_id_2}.upload-102-1"
dest_dir_2="$TEST_TMP/releases/${rel_id_2}"

create_test_bundle "$dest_dir_2" "$rel_id_2"
# Place a canary marker in existing release to verify it is NOT deleted or overwritten
echo "canary-marker-content" > "$dest_dir_2/.canary"

# Create identical staging bundle
create_test_bundle "$stage_dir_2" "$rel_id_2"

out_2=""
exit_2=0
out_2="$(bash "$STAGE_SCRIPT" --staging-dir "$stage_dir_2" --release-dir "$dest_dir_2" --skip-cosign 2>&1)" || exit_2=$?

assert_eq "0" "$exit_2" "Scenario 2: staging succeeds when identical release exists"
assert_contains "$out_2" "Reusing existing release" "Scenario 2: output notes reuse of existing release"
assert_exists "$dest_dir_2/.canary" "Scenario 2: existing release was NOT overwritten or wiped"
assert_not_exists "$stage_dir_2" "Scenario 2: staging upload directory cleaned up"

# -----------------------------------------------------------------------------
# Scenario 3: remote release exists and differs -> RELEASE_ID_COLLISION
# -----------------------------------------------------------------------------
printf '\n%s\n' "--- Scenario 3: Remote release exists and differs -> RELEASE_ID_COLLISION ---"

# 3a: Manifest content differs
printf '3a. Manifest differs\n'
rel_id_3a="rel-test-3a"
stage_dir_3a="$TEST_TMP/releases/.${rel_id_3a}.upload-103-1"
dest_dir_3a="$TEST_TMP/releases/${rel_id_3a}"

create_test_bundle "$dest_dir_3a" "$rel_id_3a" "existing-content"
echo "canary-3a" > "$dest_dir_3a/.canary"
create_test_bundle "$stage_dir_3a" "$rel_id_3a" "different-candidate-content"

out_3a=""
exit_3a=0
out_3a="$(bash "$STAGE_SCRIPT" --staging-dir "$stage_dir_3a" --release-dir "$dest_dir_3a" --skip-cosign 2>&1)" || exit_3a=$?

if [[ "$exit_3a" -eq 0 ]]; then
  printf 'FAIL: Scenario 3a: expected non-zero exit on manifest collision, got 0\n' >&2
  TESTS_FAILED=$(( TESTS_FAILED + 1 ))
else
  printf 'PASS: Scenario 3a: non-zero exit on manifest collision\n'
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
fi
assert_contains "$out_3a" "RELEASE_ID_COLLISION" "Scenario 3a: output contains RELEASE_ID_COLLISION"
assert_exists "$dest_dir_3a/.canary" "Scenario 3a: existing release directory untouched"
assert_not_exists "$stage_dir_3a" "Scenario 3a: staging directory cleaned up"

# 3b: Artifact hash mismatch in existing release (corrupted on host)
printf '3b. Artifact hash mismatch in existing release\n'
rel_id_3b="rel-test-3b"
stage_dir_3b="$TEST_TMP/releases/.${rel_id_3b}.upload-103-2"
dest_dir_3b="$TEST_TMP/releases/${rel_id_3b}"

create_test_bundle "$dest_dir_3b" "$rel_id_3b"
create_test_bundle "$stage_dir_3b" "$rel_id_3b"
# Tamper with existing release artifact on disk without updating manifest
echo "tampered-content" > "$dest_dir_3b/deploy.sh"

out_3b=""
exit_3b=0
out_3b="$(bash "$STAGE_SCRIPT" --staging-dir "$stage_dir_3b" --release-dir "$dest_dir_3b" --skip-cosign 2>&1)" || exit_3b=$?

if [[ "$exit_3b" -eq 0 ]]; then
  printf 'FAIL: Scenario 3b: expected non-zero exit on tampered artifact, got 0\n' >&2
  TESTS_FAILED=$(( TESTS_FAILED + 1 ))
else
  printf 'PASS: Scenario 3b: non-zero exit on existing artifact hash mismatch\n'
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
fi
assert_contains "$out_3b" "RELEASE_ID_COLLISION" "Scenario 3b: output contains RELEASE_ID_COLLISION"
assert_not_exists "$stage_dir_3b" "Scenario 3b: staging directory cleaned up"

# 3c: Artifact missing from existing release
printf '3c. Artifact missing in existing release\n'
rel_id_3c="rel-test-3c"
stage_dir_3c="$TEST_TMP/releases/.${rel_id_3c}.upload-103-3"
dest_dir_3c="$TEST_TMP/releases/${rel_id_3c}"

create_test_bundle "$dest_dir_3c" "$rel_id_3c"
create_test_bundle "$stage_dir_3c" "$rel_id_3c"
rm "$dest_dir_3c/lib.sh"

out_3c=""
exit_3c=0
out_3c="$(bash "$STAGE_SCRIPT" --staging-dir "$stage_dir_3c" --release-dir "$dest_dir_3c" --skip-cosign 2>&1)" || exit_3c=$?

if [[ "$exit_3c" -eq 0 ]]; then
  printf 'FAIL: Scenario 3c: expected non-zero exit on missing artifact, got 0\n' >&2
  TESTS_FAILED=$(( TESTS_FAILED + 1 ))
else
  printf 'PASS: Scenario 3c: non-zero exit on existing release missing artifact\n'
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
fi
assert_contains "$out_3c" "RELEASE_ID_COLLISION" "Scenario 3c: output contains RELEASE_ID_COLLISION"
assert_not_exists "$stage_dir_3c" "Scenario 3c: staging directory cleaned up"

# 3d: Existing path is a regular file
printf '3d. Existing path is not a directory\n'
rel_id_3d="rel-test-3d"
stage_dir_3d="$TEST_TMP/releases/.${rel_id_3d}.upload-103-4"
dest_dir_3d="$TEST_TMP/releases/${rel_id_3d}"

create_test_bundle "$stage_dir_3d" "$rel_id_3d"
mkdir -p "$TEST_TMP/releases"
echo "not-a-dir" > "$dest_dir_3d"

out_3d=""
exit_3d=0
out_3d="$(bash "$STAGE_SCRIPT" --staging-dir "$stage_dir_3d" --release-dir "$dest_dir_3d" --skip-cosign 2>&1)" || exit_3d=$?

if [[ "$exit_3d" -eq 0 ]]; then
  printf 'FAIL: Scenario 3d: expected non-zero exit on regular file collision, got 0\n' >&2
  TESTS_FAILED=$(( TESTS_FAILED + 1 ))
else
  printf 'PASS: Scenario 3d: non-zero exit when destination is a regular file\n'
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
fi
assert_contains "$out_3d" "RELEASE_ID_COLLISION" "Scenario 3d: output contains RELEASE_ID_COLLISION"
assert_exists "$dest_dir_3d" "Scenario 3d: existing path preserved"
assert_not_exists "$stage_dir_3d" "Scenario 3d: staging directory cleaned up"

# -----------------------------------------------------------------------------
# Scenario 4: partial .upload directory exists -> clean only that attempt's dir
# -----------------------------------------------------------------------------
printf '\n%s\n' "--- Scenario 4: Partial .upload exists -> clean only own attempt's staging dir ---"

# 4a: Staging success cleans ONLY own staging dir; leaves previous attempt & generic .upload
printf "4a. Successful install cleans only own attempt staging dir\n"
rel_id_4a="rel-test-4a"
prev_stage_4a="$TEST_TMP/releases/.${rel_id_4a}.upload-104-1"
curr_stage_4a="$TEST_TMP/releases/.${rel_id_4a}.upload-104-2"
generic_upload_4a="$TEST_TMP/releases/.upload"
other_stage_4a="$TEST_TMP/releases/.other-rel.upload-999-1"
dest_dir_4a="$TEST_TMP/releases/${rel_id_4a}"

mkdir -p "$prev_stage_4a" "$generic_upload_4a" "$other_stage_4a"
echo "prev-attempt-evidence" > "$prev_stage_4a/partial.txt"
echo "generic-upload-data" > "$generic_upload_4a/data.txt"
echo "other-upload-data" > "$other_stage_4a/data.txt"

create_test_bundle "$curr_stage_4a" "$rel_id_4a"

exit_4a=0
bash "$STAGE_SCRIPT" --staging-dir "$curr_stage_4a" --release-dir "$dest_dir_4a" --skip-cosign >/dev/null 2>&1 || exit_4a=$?

assert_eq "0" "$exit_4a" "Scenario 4a: install succeeds"
assert_exists "$dest_dir_4a" "Scenario 4a: release installed"
assert_not_exists "$curr_stage_4a" "Scenario 4a: current attempt staging dir moved"
assert_exists "$prev_stage_4a/partial.txt" "Scenario 4a: previous attempt staging dir is preserved"
assert_exists "$generic_upload_4a/data.txt" "Scenario 4a: generic .upload directory is preserved"
assert_exists "$other_stage_4a/data.txt" "Scenario 4a: other release staging dir is preserved"

# 4b: Staging failure cleans ONLY own staging dir; leaves previous attempt & generic .upload
printf "4b. Staging failure cleans only own attempt staging dir\n"
rel_id_4b="rel-test-4b"
prev_stage_4b="$TEST_TMP/releases/.${rel_id_4b}.upload-105-1"
curr_stage_4b="$TEST_TMP/releases/.${rel_id_4b}.upload-105-2"
generic_upload_4b="$TEST_TMP/releases/.upload"
dest_dir_4b="$TEST_TMP/releases/${rel_id_4b}"

mkdir -p "$prev_stage_4b" "$generic_upload_4b"
echo "prev-attempt-evidence" > "$prev_stage_4b/partial.txt"
echo "generic-upload-data" > "$generic_upload_4b/data.txt"

# Candidate staging directory has a corrupted artifact (fails candidate verification)
create_test_bundle "$curr_stage_4b" "$rel_id_4b"
echo "corrupted-file" > "$curr_stage_4b/deploy.sh"

exit_4b=0
bash "$STAGE_SCRIPT" --staging-dir "$curr_stage_4b" --release-dir "$dest_dir_4b" --skip-cosign >/dev/null 2>&1 || exit_4b=$?

if [[ "$exit_4b" -eq 0 ]]; then
  printf 'FAIL: Scenario 4b: expected non-zero exit on corrupted candidate, got 0\n' >&2
  TESTS_FAILED=$(( TESTS_FAILED + 1 ))
else
  printf 'PASS: Scenario 4b: non-zero exit on candidate verification failure\n'
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
fi

assert_not_exists "$curr_stage_4b" "Scenario 4b: failing attempt staging dir cleaned up"
assert_exists "$prev_stage_4b/partial.txt" "Scenario 4b: previous attempt staging dir is preserved"
assert_exists "$generic_upload_4b/data.txt" "Scenario 4b: generic .upload directory is preserved"

# -----------------------------------------------------------------------------
# Scenario 5: Safety against generic .upload deletion in cleanup-only mode
# -----------------------------------------------------------------------------
printf "\n--- Scenario 5: Cleanup protection for generic .upload and release dir ---\n"
generic_dir="$TEST_TMP/releases/.upload"
mkdir -p "$generic_dir"
echo "keep-me" > "$generic_dir/keep.txt"

exit_5a=0
bash "$STAGE_SCRIPT" --staging-dir "$generic_dir" --cleanup-only >/dev/null 2>&1 || exit_5a=$?
if [[ "$exit_5a" -eq 0 ]]; then
  printf 'FAIL: Scenario 5: expected refusal to clean generic .upload\n' >&2
  TESTS_FAILED=$(( TESTS_FAILED + 1 ))
else
  printf 'PASS: Scenario 5: refused cleanup of generic .upload\n'
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
fi
assert_exists "$generic_dir/keep.txt" "Scenario 5: generic .upload content was not deleted"

# Cleanup-only on an attempt-specific dir works
attempt_cleanup_dir="$TEST_TMP/releases/.rel-clean.upload-888-1"
mkdir -p "$attempt_cleanup_dir"
echo "temp" > "$attempt_cleanup_dir/temp.txt"

exit_5b=0
bash "$STAGE_SCRIPT" --staging-dir "$attempt_cleanup_dir" --cleanup-only >/dev/null 2>&1 || exit_5b=$?
assert_eq "0" "$exit_5b" "Scenario 5: cleanup-only succeeds on attempt-specific dir"
assert_not_exists "$attempt_cleanup_dir" "Scenario 5: attempt-specific dir removed by cleanup-only"

# -----------------------------------------------------------------------------
# Scenario 6: Cosign verification integration with mock cosign
# -----------------------------------------------------------------------------
printf "\n--- Scenario 6: Cosign verification contract ---\n"
mock_bin="$TEST_TMP/mock_bin"
mkdir -p "$mock_bin"

cat <<'EOF' > "$mock_bin/cosign"
#!/usr/bin/env bash
# Mock cosign verifier
signer=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --certificate-identity) signer="$2"; shift 2 ;;
    *) shift ;;
  esac
done

if [[ "$signer" == "valid-identity" ]]; then
  exit 0
else
  echo "Cosign mock verification error: invalid signer $signer" >&2
  exit 1
fi
EOF
chmod +x "$mock_bin/cosign"

rel_id_6="rel-test-6"
stage_dir_6="$TEST_TMP/releases/.${rel_id_6}.upload-106-1"
dest_dir_6="$TEST_TMP/releases/${rel_id_6}"
create_test_bundle "$stage_dir_6" "$rel_id_6"

exit_6a=0
PATH="$mock_bin:$PATH" bash "$STAGE_SCRIPT" \
  --staging-dir "$stage_dir_6" \
  --release-dir "$dest_dir_6" \
  --expected-identity "valid-identity" >/dev/null 2>&1 || exit_6a=$?

assert_eq "0" "$exit_6a" "Scenario 6a: cosign verification succeeds with valid identity"
assert_exists "$dest_dir_6" "Scenario 6a: release installed on valid signature"

# Cosign verification failure on invalid identity
rel_id_6b="rel-test-6b"
stage_dir_6b="$TEST_TMP/releases/.${rel_id_6b}.upload-106-2"
dest_dir_6b="$TEST_TMP/releases/${rel_id_6b}"
create_test_bundle "$stage_dir_6b" "$rel_id_6b"

exit_6b=0
PATH="$mock_bin:$PATH" bash "$STAGE_SCRIPT" \
  --staging-dir "$stage_dir_6b" \
  --release-dir "$dest_dir_6b" \
  --expected-identity "wrong-identity" >/dev/null 2>&1 || exit_6b=$?

if [[ "$exit_6b" -eq 0 ]]; then
  printf 'FAIL: Scenario 6b: expected non-zero exit on invalid cosign identity\n' >&2
  TESTS_FAILED=$(( TESTS_FAILED + 1 ))
else
  printf 'PASS: Scenario 6b: fails closed on invalid cosign identity\n'
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
fi
assert_not_exists "$dest_dir_6b" "Scenario 6b: destination was NOT created on signature failure"
assert_not_exists "$stage_dir_6b" "Scenario 6b: staging directory cleaned up on signature failure"

# -----------------------------------------------------------------------------
# Scenario 7: Self-reexec when script is invoked from inside staging directory
# -----------------------------------------------------------------------------
printf "\n--- Scenario 7: Self-reexec when invoked from inside staging directory ---\n"
rel_id_7="rel-test-7"
stage_dir_7="$TEST_TMP/releases/.${rel_id_7}.upload-107-1"
dest_dir_7="$TEST_TMP/releases/${rel_id_7}"

create_test_bundle "$stage_dir_7" "$rel_id_7"
cp "$STAGE_SCRIPT" "$stage_dir_7/stage-immutable-release.sh"
chmod +x "$stage_dir_7/stage-immutable-release.sh"

exit_7=0
bash "$stage_dir_7/stage-immutable-release.sh" \
  --staging-dir "$stage_dir_7" \
  --release-dir "$dest_dir_7" \
  --skip-cosign >/dev/null 2>&1 || exit_7=$?

assert_eq "0" "$exit_7" "Scenario 7: executes cleanly from inside staging directory"
assert_exists "$dest_dir_7" "Scenario 7: release installed when executed from inside staging directory"
assert_not_exists "$stage_dir_7" "Scenario 7: staging directory moved cleanly without file lock error"

# -----------------------------------------------------------------------------
# Summary
# -----------------------------------------------------------------------------
printf "\n========================================================\n"
printf "Task 6 Staging Rerun Contract Results: %d passed, %d failed\n" "$TESTS_PASSED" "$TESTS_FAILED"
printf "========================================================\n"

if [[ "$TESTS_FAILED" -gt 0 ]]; then
  exit 1
fi
exit 0
