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

  # Canonical state pointing to Release A with validated previous release identity
  cat <<EOF > "$tdir/state/current-release.json"
{
  "schema_version": 2,
  "generation": 41,
  "release_id": "rel-A",
  "release_dir": "$rel_a",
  "git_sha": "1111111111111111111111111111111111111111",
  "manifest_sha256": "$m_sha_a",
  "status": "COMPLETED",
  "previous": {
    "generation": 40,
    "release_id": "rel-A",
    "release_dir": "$rel_a",
    "git_sha": "1111111111111111111111111111111111111111",
    "manifest_sha256": "$m_sha_a"
  },
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

  # Generate rollout journal for Candidate B using canonical rollout-journal API
  source "$tdir/deploy/lib.sh"
  ROLLOUT_JOURNAL_FILE="$tdir/data/rollout-journal.json" \
    init_rollout_journal "rel-B" "2222222222222222222222222222222222222222" "schema,auth_browser,worker" "$rel_b" "$rel_a"
  ROLLOUT_JOURNAL_FILE="$tdir/data/rollout-journal.json" update_rollout_step "schema" "STEP_COMPLETED"
  ROLLOUT_JOURNAL_FILE="$tdir/data/rollout-journal.json" update_rollout_step "auth_browser" "STEP_COMPLETED"
  ROLLOUT_JOURNAL_FILE="$tdir/data/rollout-journal.json" update_rollout_step "worker" "STEP_COMPLETED"

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

  # Mock deploy-auth-browser.sh and deploy-bark.sh
  cat <<'EOF' > "$tdir/deploy/deploy-auth-browser.sh"
#!/usr/bin/env bash
printf 'AUTH_BROWSER_DEPLOYED: image=%s\n' "$1" >> "$RUNTIME_ROOT/aux_deploy.log"
exit 0
EOF
  chmod +x "$tdir/deploy/deploy-auth-browser.sh"

  cat <<'EOF' > "$tdir/deploy/deploy-bark.sh"
#!/usr/bin/env bash
printf 'BARK_DEPLOYED: image=%s\n' "$1" >> "$RUNTIME_ROOT/aux_deploy.log"
exit 0
EOF
  chmod +x "$tdir/deploy/deploy-bark.sh"

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

  # Assert that completed auth_browser was rolled back
  if grep -q "AUTH_BROWSER_DEPLOYED" "$tdir/aux_deploy.log" 2>/dev/null; then
    printf 'PASS: Completed auxiliary component auth_browser was restored\n'
    TESTS_PASSED=$(( TESTS_PASSED + 1 ))
  else
    printf 'FAIL: Completed auxiliary component auth_browser was not restored\n' >&2
    TESTS_FAILED=$(( TESTS_FAILED + 1 ))
  fi

  # Assert that uncompleted bark was NOT rolled back
  if grep -q "BARK_DEPLOYED" "$tdir/aux_deploy.log" 2>/dev/null; then
    printf 'FAIL: Uncompleted component bark was incorrectly touched\n' >&2
    TESTS_FAILED=$(( TESTS_FAILED + 1 ))
  else
    printf 'PASS: Uncompleted component bark was not touched\n'
    TESTS_PASSED=$(( TESTS_PASSED + 1 ))
  fi

  # Assert that rollout journal was archived
  if [[ ! -f "$tdir/data/rollout-journal.json" ]] && compgen -G "$tdir/data/rollout-journal.json.rolled_back."* >/dev/null; then
    printf 'PASS: Rollout journal was archived after successful rollback\n'
    TESTS_PASSED=$(( TESTS_PASSED + 1 ))
  else
    printf 'FAIL: Rollout journal was not archived properly\n' >&2
    TESTS_FAILED=$(( TESTS_FAILED + 1 ))
  fi
}

test_rollback_failure_accumulation() {
  local tdir="$TEST_TMP/failure_accum"
  local rel_a="$tdir/releases/rel-A"
  local rel_b="$tdir/releases/rel-B"
  mkdir -p "$rel_a/compose" "$rel_b/compose" "$tdir/state" "$tdir/data" "$tdir/secrets" "$tdir/mock_bin" "$tdir/deploy"

  cp -r "$DEPLOY_DIR/lib"* "$tdir/deploy/"
  cp "$DEPLOY_DIR/release-env.sh" "$tdir/deploy/"
  cp "$DEPLOY_DIR/verify-manifest.sh" "$tdir/deploy/"
  cp "$DEPLOY_DIR/runtime-layout.sh" "$tdir/deploy/"
  cp "$DEPLOY_DIR/rollback-release.sh" "$tdir/deploy/"
  cp "$DEPLOY_DIR/release-state.py" "$tdir/deploy/"
  cp "$DEPLOY_DIR/edge-probe.sh" "$tdir/deploy/"

  cat <<'EOF' > "$rel_a/compose/worker.yaml"
services:
  worker:
    image: ${WORKER_IMAGE_REF}
EOF
  cat <<'EOF' > "$rel_a/release-manifest.json"
{
  "release_id": "rel-A",
  "git_sha": "1111111111111111111111111111111111111111",
  "created_at": "2026-09-16T00:00:00Z",
  "compatibility": { "schema_version": 2, "worker_rpc_version": "2.0" },
  "images": {
    "frontend": "ghcr.io/test/frontend@sha256:0000000000000000000000000000000000000000000000000000000000000001",
    "gateway": "ghcr.io/test/gateway@sha256:0000000000000000000000000000000000000000000000000000000000000001",
    "worker": "ghcr.io/test/worker@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "dbtool": "ghcr.io/test/dbtool@sha256:0000000000000000000000000000000000000000000000000000000000000001",
    "auth_browser": "ghcr.io/test/auth@sha256:0000000000000000000000000000000000000000000000000000000000000001",
    "tts": "ghcr.io/test/tts@sha256:0000000000000000000000000000000000000000000000000000000000000001",
    "bark": "ghcr.io/test/bark@sha256:0000000000000000000000000000000000000000000000000000000000000001"
  }
}
EOF

  local m_sha_a
  m_sha_a="$(sha256sum "$rel_a/release-manifest.json" | awk '{print $1}')"

  cat <<EOF > "$tdir/state/current-release.json"
{
  "schema_version": 2,
  "generation": 41,
  "release_id": "rel-A",
  "release_dir": "$rel_a",
  "git_sha": "1111111111111111111111111111111111111111",
  "manifest_sha256": "$m_sha_a",
  "status": "COMPLETED",
  "previous": {
    "generation": 40,
    "release_id": "rel-A",
    "release_dir": "$rel_a",
    "git_sha": "1111111111111111111111111111111111111111",
    "manifest_sha256": "$m_sha_a"
  },
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
    "worker": "ghcr.io/test/worker@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "dbtool": "ghcr.io/test/dbtool@sha256:0000000000000000000000000000000000000000000000000000000000000001",
    "auth_browser": "ghcr.io/test/auth@sha256:0000000000000000000000000000000000000000000000000000000000000001",
    "tts": "ghcr.io/test/tts@sha256:0000000000000000000000000000000000000000000000000000000000000001",
    "bark": "ghcr.io/test/bark@sha256:0000000000000000000000000000000000000000000000000000000000000001"
  }
}
EOF

  source "$tdir/deploy/lib.sh"
  ROLLOUT_JOURNAL_FILE="$tdir/data/rollout-journal.json" \
    init_rollout_journal "rel-B" "2222222222222222222222222222222222222222" "schema,auth_browser,worker" "$rel_b" "$rel_a"
  ROLLOUT_JOURNAL_FILE="$tdir/data/rollout-journal.json" update_rollout_step "schema" "STEP_COMPLETED"
  ROLLOUT_JOURNAL_FILE="$tdir/data/rollout-journal.json" update_rollout_step "auth_browser" "STEP_COMPLETED"
  ROLLOUT_JOURNAL_FILE="$tdir/data/rollout-journal.json" update_rollout_step "worker" "STEP_COMPLETED"

  # Worker fails during rollback
  cat <<'EOF' > "$tdir/deploy/deploy-worker.sh"
#!/usr/bin/env bash
echo "WORKER_FAILED" >> "$RUNTIME_ROOT/rollback.log"
exit 1
EOF
  chmod +x "$tdir/deploy/deploy-worker.sh"

  # Auth browser succeeds
  cat <<'EOF' > "$tdir/deploy/deploy-auth-browser.sh"
#!/usr/bin/env bash
echo "AUTH_BROWSER_RESTORED" >> "$RUNTIME_ROOT/rollback.log"
exit 0
EOF
  chmod +x "$tdir/deploy/deploy-auth-browser.sh"

  local r_code=0
  RUNTIME_ROOT="$tdir" \
  DEPLOY_PATH="$tdir" \
  RUNTIME_RELEASES_DIR="$tdir/releases" \
  SKIP_MANIFEST_CHECK=1 \
  RUNTIME_DRIFT_CHECK_CMD="true" \
  bash "$tdir/deploy/rollback-release.sh" \
    --state "$tdir/state/current-release.json" \
    --journal "$tdir/data/rollout-journal.json" || r_code=$?

  assert_eq "1" "$r_code" "Rollback returns failure when component rollback fails"

  # Verify auth-browser was still executed (failure accumulated, not short-circuited)
  if grep -q "AUTH_BROWSER_RESTORED" "$tdir/rollback.log" 2>/dev/null; then
    printf 'PASS: Failure was accumulated and subsequent components were still rolled back\n'
    TESTS_PASSED=$(( TESTS_PASSED + 1 ))
  else
    printf 'FAIL: Subsequent component was short-circuited\n' >&2
    TESTS_FAILED=$(( TESTS_FAILED + 1 ))
  fi

  # Verify journal was preserved because of rollback failure
  if [[ -f "$tdir/data/rollout-journal.json" ]]; then
    printf 'PASS: Rollout journal was preserved after failed rollback\n'
    TESTS_PASSED=$(( TESTS_PASSED + 1 ))
  else
    printf 'FAIL: Rollout journal was removed despite failure\n' >&2
    TESTS_FAILED=$(( TESTS_FAILED + 1 ))
  fi
}

test_task8_bundle_integrity_verification() {
  local tdir="$TEST_TMP/task8_integrity"
  local rel_a="$tdir/releases/rel-A"
  local rel_b="$tdir/releases/rel-B"
  mkdir -p "$rel_a/compose" "$rel_b/compose" "$tdir/state" "$tdir/data" "$tdir/secrets" "$tdir/mock_bin" "$tdir/deploy"

  cp -r "$DEPLOY_DIR/lib"* "$tdir/deploy/"
  cp "$DEPLOY_DIR/release-env.sh" "$tdir/deploy/"
  cp "$DEPLOY_DIR/verify-manifest.sh" "$tdir/deploy/"
  cp "$DEPLOY_DIR/runtime-layout.sh" "$tdir/deploy/"
  cp "$DEPLOY_DIR/rollback-release.sh" "$tdir/deploy/"
  cp "$DEPLOY_DIR/release-state.py" "$tdir/deploy/"
  cp "$DEPLOY_DIR/edge-probe.sh" "$tdir/deploy/"

  echo "services: {worker: {}}" > "$rel_a/compose/worker.yaml"
  local comp_sha
  comp_sha="$(sha256sum "$rel_a/compose/worker.yaml" | awk '{print $1}')"

  cat <<EOF > "$rel_a/release-manifest.json"
{
  "release_id": "rel-A",
  "git_sha": "1111111111111111111111111111111111111111",
  "created_at": "2026-09-16T00:00:00Z",
  "compatibility": { "schema_version": 2, "worker_rpc_version": "2.0" },
  "images": {
    "frontend": "ghcr.io/test/frontend@sha256:0000000000000000000000000000000000000000000000000000000000000001",
    "gateway": "ghcr.io/test/gateway@sha256:0000000000000000000000000000000000000000000000000000000000000001",
    "worker": "ghcr.io/test/worker@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "dbtool": "ghcr.io/test/dbtool@sha256:0000000000000000000000000000000000000000000000000000000000000001",
    "auth_browser": "ghcr.io/test/auth@sha256:0000000000000000000000000000000000000000000000000000000000000001",
    "tts": "ghcr.io/test/tts@sha256:0000000000000000000000000000000000000000000000000000000000000001",
    "bark": "ghcr.io/test/bark@sha256:0000000000000000000000000000000000000000000000000000000000000001"
  },
  "artifacts": {
    "compose/worker.yaml": "$comp_sha"
  }
}
EOF

  local m_sha
  m_sha="$(sha256sum "$rel_a/release-manifest.json" | awk '{print $1}')"

  cat <<EOF > "$tdir/state/current-release.json"
{
  "schema_version": 2,
  "generation": 41,
  "release_id": "rel-A",
  "release_dir": "$rel_a",
  "git_sha": "1111111111111111111111111111111111111111",
  "manifest_sha256": "$m_sha",
  "status": "COMPLETED",
  "previous": {
    "generation": 40,
    "release_id": "rel-A",
    "release_dir": "$rel_a",
    "git_sha": "1111111111111111111111111111111111111111",
    "manifest_sha256": "$m_sha"
  },
  "active_slots": { "gateway": "green", "frontend": "blue" },
  "images": {
    "gateway": { "blue": "ghcr.io/test/gateway@sha256:0000000000000000000000000000000000000000000000000000000000000001", "green": "ghcr.io/test/gateway@sha256:0000000000000000000000000000000000000000000000000000000000000001" },
    "frontend": "ghcr.io/test/frontend@sha256:0000000000000000000000000000000000000000000000000000000000000001",
    "worker": "ghcr.io/test/worker@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "dbtool": "ghcr.io/test/dbtool@sha256:0000000000000000000000000000000000000000000000000000000000000001",
    "auth_browser": "ghcr.io/test/auth@sha256:0000000000000000000000000000000000000000000000000000000000000001",
    "tts": "ghcr.io/test/tts@sha256:0000000000000000000000000000000000000000000000000000000000000001",
    "bark": "ghcr.io/test/bark@sha256:0000000000000000000000000000000000000000000000000000000000000001"
  }
}
EOF

  cat <<'EOF' > "$tdir/deploy/deploy-worker.sh"
#!/usr/bin/env bash
echo "DOCKER_MUTATION_EXECUTED" >> "$RUNTIME_ROOT/docker_mutations.log"
exit 0
EOF
  chmod +x "$tdir/deploy/deploy-worker.sh"

  source "$tdir/deploy/lib.sh"
  ROLLOUT_JOURNAL_FILE="$tdir/data/rollout-journal.json" \
    init_rollout_journal "rel-B" "2222222222222222222222222222222222222222" "worker" "$rel_b" "$rel_a"
  ROLLOUT_JOURNAL_FILE="$tdir/data/rollout-journal.json" update_rollout_step "worker" "STEP_COMPLETED"

  # 1. Tampered previous compose file fails before docker mutation
  echo "# malicious edit" >> "$rel_a/compose/worker.yaml"
  rm -f "$tdir/docker_mutations.log"
  local code1=0
  RUNTIME_ROOT="$tdir" DEPLOY_PATH="$tdir" RUNTIME_RELEASES_DIR="$tdir/releases" \
    SKIP_MANIFEST_CHECK=1 RUNTIME_DRIFT_CHECK_CMD="true" \
    bash "$tdir/deploy/rollback-release.sh" --state "$tdir/state/current-release.json" --journal "$tdir/data/rollout-journal.json" >/dev/null 2>&1 || code1=$?
  assert_eq "1" "$code1" "Rollback fails when previous compose file is tampered"
  [[ ! -f "$tdir/docker_mutations.log" ]] && printf 'PASS: No docker mutations invoked when compose is tampered\n' || {
    printf 'FAIL: Docker mutation occurred despite tampered compose\n' >&2
    TESTS_FAILED=$(( TESTS_FAILED + 1 ))
  }

  # Restore compose
  echo "services: {worker: {}}" > "$rel_a/compose/worker.yaml"

  # 2. Tampered manifest fails
  echo "tampered" >> "$rel_a/release-manifest.json"
  local code2=0
  RUNTIME_ROOT="$tdir" DEPLOY_PATH="$tdir" RUNTIME_RELEASES_DIR="$tdir/releases" \
    SKIP_MANIFEST_CHECK=1 RUNTIME_DRIFT_CHECK_CMD="true" \
    bash "$tdir/deploy/rollback-release.sh" --state "$tdir/state/current-release.json" --journal "$tdir/data/rollout-journal.json" >/dev/null 2>&1 || code2=$?
  assert_eq "1" "$code2" "Rollback fails when previous release manifest SHA is tampered"

  # 3. Symlink release directory is rejected
  mkdir -p "$tdir/other_releases/rel-symlink"
  ln -s "$tdir/other_releases/rel-symlink" "$tdir/releases/symlinked-rel"
  cat <<EOF > "$tdir/state/current-release.json"
{
  "schema_version": 2,
  "generation": 41,
  "release_id": "rel-A",
  "release_dir": "$rel_a",
  "git_sha": "1111111111111111111111111111111111111111",
  "manifest_sha256": "$m_sha",
  "status": "COMPLETED",
  "previous": {
    "generation": 40,
    "release_id": "rel-symlink",
    "release_dir": "$tdir/releases/symlinked-rel",
    "git_sha": "1111111111111111111111111111111111111111",
    "manifest_sha256": "$m_sha"
  },
  "active_slots": { "gateway": "green", "frontend": "blue" },
  "images": { "gateway": { "blue": "$m_sha", "green": "$m_sha" } }
}
EOF
  local code3=0
  RUNTIME_ROOT="$tdir" DEPLOY_PATH="$tdir" RUNTIME_RELEASES_DIR="$tdir/releases" \
    SKIP_MANIFEST_CHECK=1 RUNTIME_DRIFT_CHECK_CMD="true" \
    bash "$tdir/deploy/rollback-release.sh" --state "$tdir/state/current-release.json" --journal "$tdir/data/rollout-journal.json" >/dev/null 2>&1 || code3=$?
  assert_eq "1" "$code3" "Rollback fails when previous release_dir is a symlink"
}

test_intentional_rollback_requires_validated_previous_identity() {
  printf '\n--- Running test_intentional_rollback_requires_validated_previous_identity ---\n'
  local tdir="$TEST_TMP/req_prev"
  local rel_a="$tdir/releases/rel-A"
  mkdir -p "$rel_a/compose" "$tdir/releases/rel-B/compose" "$tdir/state" "$tdir/data" "$tdir/secrets" "$tdir/mock_bin" "$tdir/deploy"
  cp -r "$DEPLOY_DIR/lib"* "$tdir/deploy/"
  cp "$DEPLOY_DIR/release-env.sh" "$tdir/deploy/"
  cp "$DEPLOY_DIR/verify-manifest.sh" "$tdir/deploy/"
  cp "$DEPLOY_DIR/runtime-layout.sh" "$tdir/deploy/"
  cp "$DEPLOY_DIR/rollback-release.sh" "$tdir/deploy/"
  cp "$DEPLOY_DIR/release-state.py" "$tdir/deploy/"
  cp "$DEPLOY_DIR/edge-probe.sh" "$tdir/deploy/"

  # 1. State missing previous block completely fails closed
  cat <<EOF > "$tdir/state/current-release.json"
{
  "schema_version": 2,
  "generation": 41,
  "release_id": "rel-A",
  "release_dir": "$rel_a",
  "git_sha": "1111111111111111111111111111111111111111",
  "manifest_sha256": "0000000000000000000000000000000000000000000000000000000000000000",
  "status": "COMPLETED",
  "active_slots": { "gateway": "green", "frontend": "blue" },
  "images": {}
}
EOF

  # Candidate journal has previous_release_dir, but rollback MUST NOT fallback to it
  source "$tdir/deploy/lib.sh"
  ROLLOUT_JOURNAL_FILE="$tdir/data/rollout-journal.json" \
    init_rollout_journal "rel-B" "2222222222222222222222222222222222222222" "worker" "$tdir/releases/rel-B" "$rel_a"

  local code_no_prev=0
  RUNTIME_ROOT="$tdir" DEPLOY_PATH="$tdir" RUNTIME_RELEASES_DIR="$tdir/releases" \
    SKIP_MANIFEST_CHECK=1 RUNTIME_DRIFT_CHECK_CMD="true" \
    bash "$tdir/deploy/rollback-release.sh" --state "$tdir/state/current-release.json" --journal "$tdir/data/rollout-journal.json" >/dev/null 2>&1 || code_no_prev=$?
  assert_eq "1" "$code_no_prev" "Rollback fails when previous block is missing from state"

  # 2. Previous block missing required fields (e.g. manifest_sha256) fails closed
  cat <<EOF > "$tdir/state/current-release.json"
{
  "schema_version": 2,
  "generation": 41,
  "release_id": "rel-A",
  "release_dir": "$rel_a",
  "git_sha": "1111111111111111111111111111111111111111",
  "manifest_sha256": "0000000000000000000000000000000000000000000000000000000000000000",
  "status": "COMPLETED",
  "previous": {
    "generation": 40,
    "release_id": "rel-A0",
    "release_dir": "$rel_a"
  },
  "active_slots": { "gateway": "green", "frontend": "blue" },
  "images": {}
}
EOF

  local code_incomplete_prev=0
  RUNTIME_ROOT="$tdir" DEPLOY_PATH="$tdir" RUNTIME_RELEASES_DIR="$tdir/releases" \
    SKIP_MANIFEST_CHECK=1 RUNTIME_DRIFT_CHECK_CMD="true" \
    bash "$tdir/deploy/rollback-release.sh" --state "$tdir/state/current-release.json" --journal "$tdir/data/rollout-journal.json" >/dev/null 2>&1 || code_incomplete_prev=$?
  assert_eq "1" "$code_incomplete_prev" "Rollback fails when previous block lacks required fields"
}

test_rollback_restores_previous_config
test_rollback_failure_accumulation
test_task8_bundle_integrity_verification
test_intentional_rollback_requires_validated_previous_identity

echo "Passed: $TESTS_PASSED, Failed: $TESTS_FAILED"
[[ "$TESTS_FAILED" -eq 0 ]] || exit 1
