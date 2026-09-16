#!/usr/bin/env bash
# deploy/tests/test_release_rollback.sh
# Regression test verifying exact previous-release rollback restores previous image + config,
# not merely previous image with candidate config (Task 5).
set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR="$(cd -- "$SCRIPT_DIR/.." && pwd)"

TEST_TMP="$(mktemp -d "${TMPDIR:-/tmp}/acb-release-rollback-tests.XXXXXX")"
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

test_rollback_restores_previous_config() {
  local tdir="$TEST_TMP/config_regression"
  local rel_a="$tdir/releases/rel-A"
  local rel_b="$tdir/releases/rel-B"
  mkdir -p "$rel_a/compose" "$rel_b/compose" "$tdir/state" "$tdir/data" "$tdir/secrets" "$tdir/mock_bin" "$tdir/deploy"

  # Copy deploy scripts
  cp -r "$DEPLOY_DIR/lib"* "$tdir/deploy/"
  cp "$DEPLOY_DIR/release-env.sh" "$tdir/deploy/"
  cp "$DEPLOY_DIR/verify-manifest.sh" "$tdir/deploy/"
  cp "$DEPLOY_DIR/runtime-layout.sh" "$tdir/deploy/"
  cp "$DEPLOY_DIR/rollback-release.sh" "$tdir/deploy/"
  cp "$DEPLOY_DIR/release-state.py" "$tdir/deploy/"
  cp "$DEPLOY_DIR/edge-probe.sh" "$tdir/deploy/"

  # Release A has GOOD_A config in worker compose
  cat <<'EOF' > "$rel_a/compose/worker.yaml"
services:
  worker:
    image: ${WORKER_IMAGE_REF}
    environment:
      CONFIG_VERSION: GOOD_A
EOF
  # Release B has BAD_B config in worker compose
  cat <<'EOF' > "$rel_b/compose/worker.yaml"
services:
  worker:
    image: ${WORKER_IMAGE_REF}
    environment:
      CONFIG_VERSION: BAD_B
EOF

  # Manifests
  cat <<'EOF' > "$rel_a/release-manifest.json"
{
  "release_id": "rel-A",
  "git_sha": "1111111111111111111111111111111111111111",
  "images": {
    "worker": "ghcr.io/test/worker@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  }
}
EOF

  local img_a="ghcr.io/test/worker@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  local m_sha_a
  m_sha_a="$(sha256sum "$rel_a/release-manifest.json" | awk '{print $1}')"

  # Canonical state pointing to Release A
  cat <<EOF > "$tdir/state/current-release.json"
{
  "schema_version": 2,
  "generation": 41,
  "release_id": "rel-A",
  "release_dir": "$rel_a",
  "git_sha": "1111111111111111111111111111111111111111",
  "manifest_sha256": "$m_sha_a",
  "status": "COMPLETED",
  "active_slots": {
    "gateway": "green",
    "frontend": "blue"
  },
  "images": {
    "gateway": {
      "blue": "ghcr.io/test/gateway@sha256:0000000000000000000000000000000000000000000000000000000000000001",
      "green": "ghcr.io/test/gateway@sha256:0000000000000000000000000000000000000000000000000000000000000001"
    },
    "frontend": "ghcr.io/test/frontend@sha256:0000000000000000000000000000000000000000000000000000000000000001",
    "worker": "$img_a",
    "dbtool": "ghcr.io/test/dbtool@sha256:0000000000000000000000000000000000000000000000000000000000000001",
    "auth_browser": "ghcr.io/test/auth@sha256:0000000000000000000000000000000000000000000000000000000000000001",
    "tts": "ghcr.io/test/tts@sha256:0000000000000000000000000000000000000000000000000000000000000001",
    "bark": "ghcr.io/test/bark@sha256:0000000000000000000000000000000000000000000000000000000000000001"
  }
}
EOF

  # Rollout journal for Candidate B which failed after promoting worker
  cat <<EOF > "$tdir/data/rollout-journal.json"
{
  "release_id": "rel-B",
  "candidate_release_dir": "$rel_b",
  "previous_release_dir": "$rel_a",
  "status": "RUNNING",
  "steps": {
    "schema": "STEP_COMPLETED",
    "auth_browser": "STEP_COMPLETED",
    "worker": "STEP_COMPLETED"
  }
}
EOF

  # Mock deploy-worker.sh to record which compose directory was loaded
  cat <<'EOF' > "$tdir/deploy/deploy-worker.sh"
#!/usr/bin/env bash
target_img="$1"
# Check compose_prod resolution
source "$(dirname "$0")/lib.sh"
recorded_cfg="$(grep 'CONFIG_VERSION:' "${COMPOSE_ROOT:-$SCRIPT_DIR/compose}/worker.yaml" | awk '{print $2}')"
printf 'WORKER_DEPLOYED: image=%s config=%s\n' "$target_img" "$recorded_cfg" >> "$RUNTIME_ROOT/worker_deploy.log"
exit 0
EOF
  chmod +x "$tdir/deploy/deploy-worker.sh"

  # Execute rollback-release.sh
  RUNTIME_ROOT="$tdir" \
  DEPLOY_PATH="$tdir" \
  RUNTIME_RELEASES_DIR="$tdir/releases" \
  SKIP_MANIFEST_CHECK=1 \
  RUNTIME_DRIFT_CHECK_CMD="true" \
  bash "$tdir/deploy/rollback-release.sh" \
    --state "$tdir/state/current-release.json" \
    --journal "$tdir/data/rollout-journal.json"

  # Assert that worker was restored with image A and config GOOD_A
  local last_worker
  last_worker="$(tail -n 1 "$tdir/worker_deploy.log")"
  echo "Last worker deploy: $last_worker"

  if [[ "$last_worker" == *"image=$img_a"* && "$last_worker" == *"config=GOOD_A"* ]]; then
    printf 'PASS: Rollback used exact previous image AND previous config GOOD_A\n'
    TESTS_PASSED=$(( TESTS_PASSED + 1 ))
  else
    printf 'FAIL: Rollback did not use previous config GOOD_A: %s\n' "$last_worker" >&2
    TESTS_FAILED=$(( TESTS_FAILED + 1 ))
  fi
}

test_rollback_restores_previous_config

echo "Passed: $TESTS_PASSED, Failed: $TESTS_FAILED"
[[ "$TESTS_FAILED" -eq 0 ]] || exit 1
