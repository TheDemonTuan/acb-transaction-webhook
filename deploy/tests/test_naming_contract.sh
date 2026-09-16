#!/usr/bin/env bash
# deploy/tests/test_naming_contract.sh
# Validates canonical naming consistency and single rollout journal helper across codebase (Task 9).
set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "$SCRIPT_DIR/../.." && pwd)"

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

printf "========================================================\n"
printf "Running Canonical Naming & Journal Contract Tests\n"
printf "========================================================\n\n"

# 1. Verify Manifest Generator Image Keys
printf "1. Testing Manifest Generator Canonical Image Keys...\n"
gen_keys="$(python3 - <<'PY'
import re
path = "deploy/generate-release-manifest.sh"
with open(path, "r", encoding="utf-8") as f:
    text = f.read()
# Extract bash array images
m = re.search(r'declare -A images=\((.*?)\)', text, re.DOTALL)
if m:
    keys = sorted(re.findall(r'\["([a-zA-Z0-9_-]+)"\]', m.group(1)))
    print(",".join(keys))
PY
)"
expected_images="auth_browser,bark,dbtool,frontend,gateway,tts,worker"
assert_eq "$expected_images" "$gen_keys" "generate-release-manifest.sh declares exact 7 canonical image keys"

# Ensure tts_gateway is NOT emitted in generate-release-manifest.sh
if grep -q '"tts_gateway"' "$REPO_ROOT/deploy/generate-release-manifest.sh"; then
  printf 'FAIL: generate-release-manifest.sh still emits forbidden tts_gateway\n' >&2
  TESTS_FAILED=$(( TESTS_FAILED + 1 ))
else
  printf 'PASS: generate-release-manifest.sh emits canonical tts instead of tts_gateway\n'
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
fi

# 2. Verify Manifest Verifier Image Keys
printf "\n2. Testing Manifest Verifier Canonical Image Keys...\n"
ver_keys="$(python3 - <<'PY'
import re
path = "deploy/verify-manifest.sh"
with open(path, "r", encoding="utf-8") as f:
    text = f.read()
m = re.search(r'const requiredImages = \[(.*?)\];', text)
if m:
    keys = sorted([k.strip().strip("'\"") for k in m.group(1).split(",")])
    print(",".join(keys))
PY
)"
assert_eq "$expected_images" "$ver_keys" "verify-manifest.sh validates exact 7 canonical image keys"

# 3. Verify release-state.py Canonical Components
printf "\n3. Testing release-state.py Canonical Components...\n"
state_keys="$(python3 - <<'PY'
import importlib.util
spec = importlib.util.spec_from_file_location("release_state", "deploy/release-state.py")
rs = importlib.util.module_from_spec(spec)
spec.loader.exec_module(rs)
print(",".join(sorted(rs.REQUIRED_COMPONENTS)))
PY
)"
assert_eq "$expected_images" "$state_keys" "release-state.py REQUIRED_COMPONENTS matches canonical 7 image keys"

# 4. Verify .release.env Projection Keys
printf "\n4. Testing .release.env Projection Image Keys...\n"
env_keys="$(python3 - <<'PY'
path = "deploy/release-state.py"
with open(path, "r", encoding="utf-8") as f:
    text = f.read()
import re
m = re.findall(r'([A-Z_]+_IMAGE_REF)=', text)
print(",".join(sorted(set(m))))
PY
)"
expected_env="BARK_IMAGE_REF,BROWSER_IMAGE_REF,DBTOOL_IMAGE_REF,FRONTEND_IMAGE_REF,TTS_IMAGE_REF,WORKER_IMAGE_REF"
assert_eq "$expected_env" "$env_keys" "release-state.py exports exact projection image keys"

# 5. Verify verify-runtime-drift.sh Component Coverage
printf "\n5. Testing verify-runtime-drift.sh Image Keys...\n"
drift_keys="$(python3 - <<'PY'
path = "deploy/verify-runtime-drift.sh"
with open(path, "r", encoding="utf-8") as f:
    text = f.read()
import re
m = re.findall(r'images\.get\(["\']([a-zA-Z0-9_-]+)["\']', text)
print(",".join(sorted(set(m))))
PY
)"
assert_eq "$expected_images" "$drift_keys" "verify-runtime-drift.sh checks all 7 canonical images"

# 6. Verify Single Journal Implementation Across Consumers
printf "\n6. Testing Rollout Journal Implementation Consistency...\n"
if [[ -f "$REPO_ROOT/deploy/lib/rollout-journal.sh" ]]; then
  printf 'PASS: deploy/lib/rollout-journal.sh exists\n'
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
else
  printf 'FAIL: deploy/lib/rollout-journal.sh missing\n' >&2
  TESTS_FAILED=$(( TESTS_FAILED + 1 ))
fi

# Assert consumers use deploy/lib/rollout-journal.sh and do not maintain divergent step_was_completed definitions
consumers=("deploy/dispatch-rollout.sh" "deploy/reconcile-release.sh" "deploy/rollback-release.sh")
for c in "${consumers[@]}"; do
  file="$REPO_ROOT/$c"
  if grep -q 'sys\.argv\[2\] in state\.get("completed_steps"' "$file"; then
    printf 'FAIL: %s maintains a private legacy journal check\n' "$c" >&2
    TESTS_FAILED=$(( TESTS_FAILED + 1 ))
  else
    printf 'PASS: %s delegates journal operations to shared library\n' "$c"
    TESTS_PASSED=$(( TESTS_PASSED + 1 ))
  fi
done

printf "\n========================================================\n"
printf "Results: %d passed, %d failed\n" "$TESTS_PASSED" "$TESTS_FAILED"
printf "========================================================\n"

if [[ "$TESTS_FAILED" -gt 0 ]]; then
  exit 1
fi
exit 0
