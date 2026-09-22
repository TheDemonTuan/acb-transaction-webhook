#!/usr/bin/env bash
# deploy/tests/test_staged_release_compose.sh
# Validates that compose_prod resolves relative resources from the release root.
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "$SCRIPT_DIR/../.." && pwd)"

TEST_TMP="$(mktemp -d /tmp/acb-staged-compose.XXXXXX)"
cleanup() {
  rm -rf "$TEST_TMP"
}
trap cleanup EXIT

# 1. Static Guard: compose_prod must explicitly set --project-directory
printf "1. Checking static compose_prod --project-directory guard...\n"
if ! grep -Eq -- '--project-directory[[:space:]]+"\$ctx_dir"' "$REPO_ROOT/deploy/lib/common.sh"; then
  printf "  [FAIL] compose_prod in deploy/lib/common.sh does not configure --project-directory \"\$ctx_dir\"\n" >&2
  exit 1
fi
printf "  [PASS] compose_prod sets --project-directory \"\$ctx_dir\"\n"

# 2. Setup mock docker if docker compose is not available
MOCK_BIN="$TEST_TMP/mock-bin"
mkdir -p "$MOCK_BIN"
USE_MOCK=0
if ! command -v docker >/dev/null 2>&1 || ! docker compose version >/dev/null 2>&1; then
  USE_MOCK=1
fi

cat <<'EOF' > "$MOCK_BIN/docker"
#!/usr/bin/env bash
set -euo pipefail

if [[ "${1:-}" != "compose" ]]; then
  echo "mock docker only supports compose" >&2
  exit 1
fi
shift

project_dir=""
env_files=()
compose_files=()
args=()

while [[ $# -gt 0 ]]; do
  case "$1" in
    --project-directory)
      project_dir="$2"
      shift 2
      ;;
    --env-file)
      env_files+=("$2")
      shift 2
      ;;
    -f)
      compose_files+=("$2")
      shift 2
      ;;
    config)
      shift
      break
      ;;
    *)
      args+=("$1")
      shift
      ;;
  esac
done

if [[ -z "$project_dir" ]]; then
  if [[ ${#compose_files[@]} -gt 0 ]]; then
    project_dir="$(dirname "${compose_files[0]}")"
  else
    project_dir="."
  fi
fi

# Validate relative resources expected by compose files against project_dir
missing=()
for rel_file in "secrets/app_master_key" "bark-entrypoint.sh" "seccomp-auth-browser.json"; do
  if [[ ! -e "$project_dir/$rel_file" ]]; then
    missing+=("$project_dir/$rel_file")
  fi
done

if [[ ${#missing[@]} -gt 0 ]]; then
  echo "error: open ${missing[0]}: no such file or directory" >&2
  exit 1
fi

cat <<YAML
name: acb
services:
  bark:
    volumes:
      - type: bind
        source: $project_dir/bark-entrypoint.sh
        target: /bark-entrypoint.sh
  auth-browser:
    security_opt:
      - seccomp:$project_dir/seccomp-auth-browser.json
secrets:
  app_master_key:
    file: $project_dir/secrets/app_master_key
YAML
EOF
chmod +x "$MOCK_BIN/docker"

if [[ "$USE_MOCK" -eq 1 ]]; then
  export PATH="$MOCK_BIN:$PATH"
fi

# 3. Create exact VPS layout
printf "2. Setting up staged release runtime layout...\n"
RUNTIME_DIR="$TEST_TMP/runtime"
RELEASE_DIR="$TEST_TMP/releases/rel-test"

mkdir -p "$RUNTIME_DIR/deploy/secrets"
mkdir -p "$RELEASE_DIR/compose"

# Write dummy secrets in runtime
for s in app_master_key tts_internal_token worker_internal_token auth_browser_internal_token bark_basic_auth_user bark_basic_auth_password; do
  printf 'secret_%s_val' "$s" > "$RUNTIME_DIR/deploy/secrets/$s"
done
chmod 600 "$RUNTIME_DIR/deploy/secrets/"*

# Write runtime env files
cat <<'EOF' > "$RUNTIME_DIR/deploy/.env.production"
APP_ENV=production
POSTGRES_DB=acb_gateway
POSTGRES_USER=gateway
POSTGRES_PASSWORD=secret
POSTGRES_PORT=5432
GATEWAY_SLOT=blue
FRONTEND_SLOT=blue
EOF

DIGEST="sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
cat <<EOF > "$RUNTIME_DIR/deploy/.release.env"
IMAGE_REF_BLUE=ghcr.io/org/gateway@$DIGEST
IMAGE_REF_GREEN=ghcr.io/org/gateway@$DIGEST
WORKER_IMAGE_REF=ghcr.io/org/worker@$DIGEST
FRONTEND_IMAGE_REF=ghcr.io/org/frontend@$DIGEST
BROWSER_IMAGE_REF=ghcr.io/org/browser@$DIGEST
TTS_IMAGE_REF=ghcr.io/org/tts@$DIGEST
BARK_IMAGE_REF=ghcr.io/org/bark@$DIGEST
DBTOOL_IMAGE_REF=ghcr.io/org/dbtool@$DIGEST
EOF

# Copy compose definitions and release-root resources
cp "$REPO_ROOT"/deploy/compose/*.yaml "$RELEASE_DIR/compose/"
touch "$RELEASE_DIR/bark-entrypoint.sh"
chmod +x "$RELEASE_DIR/bark-entrypoint.sh"
printf '{"defaultAction":"SCMP_ACT_ERRNO"}' > "$RELEASE_DIR/seccomp-auth-browser.json"

# Symlinks in release dir pointing to runtime
ln -s "$RUNTIME_DIR/deploy/.env.production" "$RELEASE_DIR/.env.production"
ln -s "$RUNTIME_DIR/deploy/.release.env" "$RELEASE_DIR/.release.env"
ln -s "$RUNTIME_DIR/deploy/secrets" "$RELEASE_DIR/secrets"

# 4. Execute compose_prod config
printf "3. Running compose_prod config...\n"
export SCRIPT_DIR="$RELEASE_DIR"
export DEPLOY_DIR="$RELEASE_DIR"

# Source lib.sh
# shellcheck disable=SC1091
source "$REPO_ROOT/deploy/lib.sh"

RENDERED_FILE="$TEST_TMP/rendered-compose.yaml"

RELEASE_CONTEXT_DIR="$RELEASE_DIR" \
COMPOSE_ROOT="$RELEASE_DIR/compose" \
ENV_FILE="$RUNTIME_DIR/deploy/.env.production" \
RELEASE_ENV_FILE="$RUNTIME_DIR/deploy/.release.env" \
compose_prod config > "$RENDERED_FILE"

# 5. Assertions
printf "4. Validating rendered compose paths...\n"

# Must NOT contain references to $RELEASE_DIR/compose/secrets or $RELEASE_DIR/compose/bark-entrypoint.sh
if grep -F "$RELEASE_DIR/compose/secrets" "$RENDERED_FILE" || \
   grep -F "$RELEASE_DIR/compose/bark-entrypoint.sh" "$RENDERED_FILE" || \
   grep -F "$RELEASE_DIR/compose/seccomp-auth-browser.json" "$RENDERED_FILE"; then
  printf "  [FAIL] Rendered compose references compose/ subdirectory instead of release root!\n" >&2
  cat "$RENDERED_FILE" >&2
  exit 1
fi

# Must reference release-root resources
if ! grep -q "bark-entrypoint.sh" "$RENDERED_FILE" || \
   ! grep -q "seccomp-auth-browser.json" "$RENDERED_FILE" || \
   ! grep -q "app_master_key" "$RENDERED_FILE"; then
  printf "  [FAIL] Rendered compose missing expected release-root resources!\n" >&2
  cat "$RENDERED_FILE" >&2
  exit 1
fi

printf "  [PASS] Staged release compose resolves all relative paths from release root!\n"
exit 0
