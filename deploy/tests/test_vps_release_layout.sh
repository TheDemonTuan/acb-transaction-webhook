#!/usr/bin/env bash
# deploy/tests/test_vps_release_layout.sh
# Integration test simulating exact VPS release layout:
#   /releases/<id>/
#     compose/
#     secrets/
#     .env.production
#     bark-entrypoint.sh
#     seccomp-auth-browser.json
# Verifies that docker compose with --project-directory resolves relative paths
# from release root, catching the Bark entrypoint path failure before VPS rollout.
set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR="$(cd -- "$SCRIPT_DIR/.." && pwd)"

TEST_TMP="$(mktemp -d "${TMPDIR:-/tmp}/acb-vps-layout-tests.XXXXXX")"
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
  if ! grep -F -q -- "$needle" <<< "$haystack"; then
    printf 'FAIL: %s (text did not contain "%s")\n' "$msg" "$needle" >&2
    TESTS_FAILED=$(( TESTS_FAILED + 1 ))
    return 1
  fi
  printf 'PASS: %s\n' "$msg"
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
  return 0
}

printf "========================================================\n"
printf "Running VPS Release Layout Compose Integration Tests\n"
printf "========================================================\n\n"

# 1. Setup exact VPS layout
runtime_root="$TEST_TMP/runtime"
release_id="rel-20260916-test"
release_dir="$runtime_root/releases/$release_id"

mkdir -p "$runtime_root/deploy/secrets"
mkdir -p "$release_dir/compose"
mkdir -p "$release_dir/lib"

# Copy compose definitions
cp -r "$DEPLOY_DIR/compose/"*.yaml "$release_dir/compose/"
cp -r "$DEPLOY_DIR/lib/"*.sh "$release_dir/lib/"
cp "$DEPLOY_DIR/lib.sh" "$release_dir/lib.sh"
cp "$DEPLOY_DIR/bark-entrypoint.sh" "$release_dir/bark-entrypoint.sh"
cp "$DEPLOY_DIR/seccomp-auth-browser.json" "$release_dir/seccomp-auth-browser.json"

# Provision secrets in runtime and symlink to release
for s in app_master_key tts_internal_token worker_internal_token auth_browser_internal_token bark_basic_auth_user bark_basic_auth_password; do
  printf '%s-secret-val\n' "$s" > "$runtime_root/deploy/secrets/$s"
  chmod 600 "$runtime_root/deploy/secrets/$s"
done
ln -s "$runtime_root/deploy/secrets" "$release_dir/secrets"

# Canonical env files
cat <<'EOF' > "$runtime_root/deploy/.env.production"
APP_ENV=production
DASHBOARD_OWNER_EMAIL=admin@example.com
EOF
ln -s "$runtime_root/deploy/.env.production" "$release_dir/.env.production"

# Populate required immutable image digests
cat <<'EOF' > "$runtime_root/deploy/.release.env"
IMAGE_REF_BLUE=ghcr.io/acb/gateway@sha256:1111111111111111111111111111111111111111111111111111111111111111
IMAGE_REF_GREEN=ghcr.io/acb/gateway@sha256:2222222222222222222222222222222222222222222222222222222222222222
FRONTEND_IMAGE_REF=ghcr.io/acb/frontend@sha256:7777777777777777777777777777777777777777777777777777777777777777
WORKER_IMAGE_REF=ghcr.io/acb/worker@sha256:3333333333333333333333333333333333333333333333333333333333333333
BROWSER_IMAGE_REF=ghcr.io/acb/auth-browser@sha256:4444444444444444444444444444444444444444444444444444444444444444
AUTH_BROWSER_IMAGE_REF=ghcr.io/acb/auth-browser@sha256:4444444444444444444444444444444444444444444444444444444444444444
TTS_IMAGE_REF=ghcr.io/acb/tts-gateway@sha256:5555555555555555555555555555555555555555555555555555555555555555
BARK_IMAGE_REF=ghcr.io/finb/bark-server@sha256:32d65b07fa835c99b31a396b77727a04ed058377fc2482da3e9dc7397167ffc4
DBTOOL_IMAGE_REF=ghcr.io/acb/dbtool@sha256:8888888888888888888888888888888888888888888888888888888888888888
EOF
ln -s "$runtime_root/deploy/.release.env" "$release_dir/.release.env"

# 2. Test compose_prod flag construction
mkdir -p "$TEST_TMP/mock_bin"
cmd_log="$TEST_TMP/docker_compose.log"
cat <<EOF > "$TEST_TMP/mock_bin/docker"
#!/usr/bin/env bash
printf '%s\n' "\$*" >> "$cmd_log"
exit 0
EOF
chmod +x "$TEST_TMP/mock_bin/docker"

(
  PATH="$TEST_TMP/mock_bin:$PATH"
  export RELEASE_DIR="$release_dir"
  export RELEASE_CONTEXT_DIR="$release_dir"
  export ENV_FILE="$release_dir/.env.production"
  export RELEASE_ENV_FILE="$release_dir/.release.env"
  # shellcheck source=/dev/null
  source "$release_dir/lib.sh"

  compose_prod config >/dev/null 2>&1
)

executed_cmd="$(cat "$cmd_log" 2>/dev/null || true)"
assert_contains "$executed_cmd" "--project-directory $release_dir" "compose_prod passes --project-directory with release root"
assert_contains "$executed_cmd" "-f $release_dir/compose/base.yaml" "compose_prod loads base.yaml from compose dir"
assert_contains "$executed_cmd" "-f $release_dir/compose/bark.yaml" "compose_prod loads bark.yaml from compose dir"

# 3. If real docker compose is present, verify full schema resolution end-to-end
if command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
  printf "\nRunning live docker compose config against VPS layout...\n"

  # Live test with --project-directory: MUST succeed
  live_ec=0
  (
    export IMAGE_REF_BLUE="ghcr.io/acb/gateway@sha256:1111111111111111111111111111111111111111111111111111111111111111"
    export IMAGE_REF_GREEN="ghcr.io/acb/gateway@sha256:2222222222222222222222222222222222222222222222222222222222222222"
    export FRONTEND_IMAGE_REF="ghcr.io/acb/frontend@sha256:7777777777777777777777777777777777777777777777777777777777777777"
    export WORKER_IMAGE_REF="ghcr.io/acb/worker@sha256:3333333333333333333333333333333333333333333333333333333333333333"
    export BROWSER_IMAGE_REF="ghcr.io/acb/auth-browser@sha256:4444444444444444444444444444444444444444444444444444444444444444"
    export AUTH_BROWSER_IMAGE_REF="ghcr.io/acb/auth-browser@sha256:4444444444444444444444444444444444444444444444444444444444444444"
    export TTS_IMAGE_REF="ghcr.io/acb/tts-gateway@sha256:5555555555555555555555555555555555555555555555555555555555555555"
    export BARK_IMAGE_REF="ghcr.io/finb/bark-server@sha256:32d65b07fa835c99b31a396b77727a04ed058377fc2482da3e9dc7397167ffc4"
    export DBTOOL_IMAGE_REF="ghcr.io/acb/dbtool@sha256:8888888888888888888888888888888888888888888888888888888888888888"

    export RELEASE_DIR="$release_dir"
    export RELEASE_CONTEXT_DIR="$release_dir"
    export ENV_FILE="$release_dir/.env.production"
    export RELEASE_ENV_FILE="$release_dir/.release.env"
    # shellcheck source=/dev/null
    source "$release_dir/lib.sh"
    compose_prod config --quiet
  ) || live_ec=$?
  assert_eq "0" "$live_ec" "Real docker compose config succeeds with explicit --project-directory on VPS layout"

  # Live negative test without --project-directory: MUST fail because bark-entrypoint.sh is not in compose/
  live_fail_ec=0
  docker compose \
    --env-file "$release_dir/.env.production" \
    --env-file "$release_dir/.release.env" \
    -f "$release_dir/compose/base.yaml" \
    -f "$release_dir/compose/bark.yaml" \
    config --quiet 2>/dev/null || live_fail_ec=$?
  assert_eq "1" "$live_fail_ec" "Real docker compose config without --project-directory fails on missing compose/bark-entrypoint.sh"
fi

# 4. Verify that relative paths in rendered model point to release root files
assert_eq "true" "$([[ -f "$release_dir/bark-entrypoint.sh" ]] && echo true)" "bark-entrypoint.sh exists at release root"
assert_eq "true" "$([[ -f "$release_dir/seccomp-auth-browser.json" ]] && echo true)" "seccomp-auth-browser.json exists at release root"
assert_eq "true" "$([[ -f "$release_dir/secrets/bark_basic_auth_user" ]] && echo true)" "bark_basic_auth_user secret exists under release root secrets"

printf "\n========================================================\n"
printf "Results: %d passed, %d failed\n" "$TESTS_PASSED" "$TESTS_FAILED"
printf "========================================================\n"

if (( TESTS_FAILED > 0 )); then
  exit 1
fi
exit 0
