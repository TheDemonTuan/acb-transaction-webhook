#!/usr/bin/env bash
set -Eeuo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
env_file="${ENV_FILE:-$script_dir/.env.production}"
compose_file="${COMPOSE_FILE:-$script_dir/compose.prod.yaml}"
image_ref="${1:-${IMAGE_REF:-}}"
browser_image_ref="${2:-${AUTH_BROWSER_IMAGE_REF:-}}"
image_pattern='^[^[:space:]]+@sha256:[a-f0-9]{64}$'

# Determine arguments: $1=gateway, $2=auth-browser, $3=tts-gateway, $4=staged_compose
if [[ "${3:-}" =~ $image_pattern ]]; then
  tts_image_ref="$3"
  staged_compose="${4:-}"
elif [[ -n "${TTS_GATEWAY_IMAGE_REF:-}" && "${TTS_GATEWAY_IMAGE_REF}" =~ $image_pattern ]]; then
  tts_image_ref="${TTS_GATEWAY_IMAGE_REF}"
  staged_compose="${3:-}"
else
  tts_image_ref="${3:-}"
  staged_compose="${4:-}"
fi

[[ -f "$env_file" ]] || { printf 'Missing production env file: %s\n' "$env_file" >&2; exit 1; }
[[ "$image_ref" =~ $image_pattern ]] || { printf 'Pass an immutable gateway image digest as first argument.\n' >&2; exit 1; }
[[ "$browser_image_ref" =~ $image_pattern ]] || { printf 'Pass an immutable auth-browser image digest as second argument.\n' >&2; exit 1; }
[[ "$tts_image_ref" =~ $image_pattern ]] || { printf 'Pass an immutable tts-gateway image digest as third argument (or via TTS_GATEWAY_IMAGE_REF).\n' >&2; exit 1; }
bark_image_ref="${BARK_IMAGE_REF:-}"
[[ "$bark_image_ref" =~ $image_pattern ]] || { printf 'BARK_IMAGE_REF must be an immutable image digest.\n' >&2; exit 1; }
[[ -z "$staged_compose" || -f "$staged_compose" ]] || { printf 'Staged compose file not found: %s\n' "$staged_compose" >&2; exit 1; }

export IMAGE_REF="$image_ref"
export AUTH_BROWSER_IMAGE_REF="$browser_image_ref"
export TTS_GATEWAY_IMAGE_REF="$tts_image_ref"

lock="$script_dir/.deploy.lock"
# A cancelled SSH job can leave a Compose one-off container attached to an old deploy.
stale_oneoffs="$(docker ps -q --filter label=com.docker.compose.project=acb-transaction-webhook --filter label=com.docker.compose.oneoff=True)"
if [[ -n "$stale_oneoffs" ]]; then
  printf 'Stopping stale deployment one-off containers: %s\n' "$stale_oneoffs"
  docker stop --time 10 $stale_oneoffs >/dev/null
fi
exec 9>"$lock"
flock -w "${DEPLOY_LOCK_TIMEOUT:-30}" 9 || { printf 'Another deployment is active after waiting %ss.\n' "${DEPLOY_LOCK_TIMEOUT:-30}" >&2; exit 1; }

# Ensure data and secrets directories exist with proper permissions for container user (UID 1000)
mkdir -p "$script_dir/data" "$script_dir/secrets"
chmod 775 "$script_dir/data" || true

# Validate the shared edge network before changing compose or containers.
docker network inspect edge-acb >/dev/null 2>&1 || { printf 'Required network edge-acb is missing.\n' >&2; exit 1; }
[[ "$(docker network inspect edge-acb --format '{{.Internal}}')" == "true" ]] || { printf 'edge-acb must be internal.\n' >&2; exit 1; }
[[ "$(docker network inspect edge-acb --format '{{index .Labels "io.tuan.edge.managed"}}')" == "true" ]] || { printf 'edge-acb is not managed by the shared edge stack.\n' >&2; exit 1; }

if [[ -n "$staged_compose" ]]; then
  docker compose --env-file "$env_file" -f "$staged_compose" config --quiet
  [[ -f "$compose_file" ]] && cp "$compose_file" "$script_dir/.previous-compose.yaml"
  mv "$staged_compose" "$compose_file"
fi

# Stop writers before checkpointing and backing up SQLite.
docker stop acb-transaction-gateway acb-auth-browser 2>/dev/null || true

ensure_secret_permissions() {
  local target_path="$1"
  local name="$2"
  local cur_uid
  cur_uid="$(stat -c '%u' "$target_path" 2>/dev/null || stat -f '%u' "$target_path" 2>/dev/null || echo "")"
  if [[ "$cur_uid" != "1000" ]]; then
    if ! chown 1000:1000 "$target_path" 2>/dev/null; then
      if command -v docker >/dev/null 2>&1 && [[ -n "${tts_image_ref:-}" ]]; then
        docker run --rm --user 0:0 -v "$(dirname "$target_path"):/sec" --entrypoint /bin/sh "$tts_image_ref" -c "chown 1000:1000 /sec/$(basename "$target_path")" 2>/dev/null || true
      fi
    fi
  fi
  chmod 600 "$target_path"
}

# Clean up any stale directory created by Docker bind-mount on previous deploys
if [[ -d "$script_dir/secrets/app_master_key" ]]; then
  rmdir "$script_dir/secrets/app_master_key" 2>/dev/null || rm -rf "$script_dir/secrets/app_master_key" 2>/dev/null || true
  if [[ -d "$script_dir/secrets/app_master_key" ]] && command -v docker >/dev/null 2>&1 && [[ -n "${tts_image_ref:-}" ]]; then
    docker run --rm --user 0:0 -v "$script_dir/secrets:/sec" --entrypoint /bin/sh "$tts_image_ref" -c "rm -rf /sec/app_master_key" 2>/dev/null || true
  fi
fi

if [[ -d "$script_dir/secrets/tts_internal_token" ]]; then
  rmdir "$script_dir/secrets/tts_internal_token" 2>/dev/null || rm -rf "$script_dir/secrets/tts_internal_token" 2>/dev/null || true
  if [[ -d "$script_dir/secrets/tts_internal_token" ]] && command -v docker >/dev/null 2>&1 && [[ -n "${tts_image_ref:-}" ]]; then
    docker run --rm --user 0:0 -v "$script_dir/secrets:/sec" --entrypoint /bin/sh "$tts_image_ref" -c "rm -rf /sec/tts_internal_token" 2>/dev/null || true
  fi
fi

# Ensure secrets/app_master_key file exists before Docker mounts it
if [[ ! -f "$script_dir/secrets/app_master_key" ]]; then
  if [[ -n "${APP_MASTER_KEY:-}" ]]; then
    printf '%s\n' "$APP_MASTER_KEY" > "$script_dir/secrets/app_master_key"
  elif grep -q '^APP_MASTER_KEY=' "$env_file" 2>/dev/null; then
    val="$(grep '^APP_MASTER_KEY=' "$env_file" | head -n1 | cut -d= -f2- | tr -d ' "[:space:]' | tr -d "'")"
    if [[ -n "$val" ]]; then
      printf '%s\n' "$val" > "$script_dir/secrets/app_master_key"
    else
      openssl rand -hex 32 > "$script_dir/secrets/app_master_key"
    fi
  else
    openssl rand -hex 32 > "$script_dir/secrets/app_master_key"
  fi
fi
ensure_secret_permissions "$script_dir/secrets/app_master_key" "app_master_key"

# Ensure secrets/tts_internal_token file exists and is provisioned for UID 1000 containers
tts_token_file="$script_dir/secrets/tts_internal_token"
if [[ ! -f "$tts_token_file" ]]; then
  token_val=""
  if [[ -n "${TTS_INTERNAL_TOKEN:-}" ]]; then
    token_val="$TTS_INTERNAL_TOKEN"
  elif grep -q '^TTS_INTERNAL_TOKEN=' "$env_file" 2>/dev/null; then
    val="$(grep '^TTS_INTERNAL_TOKEN=' "$env_file" | head -n1 | cut -d= -f2- | tr -d ' "[:space:]' | tr -d "'")"
    if [[ -n "$val" ]]; then
      token_val="$val"
    fi
  fi
  if [[ -z "$token_val" ]]; then
    token_val="$(openssl rand -hex 32)"
  fi
  tmp_token="$(mktemp "$script_dir/secrets/token_tmp.XXXXXX")"
  printf '%s\n' "$token_val" > "$tmp_token"
  mv -f "$tmp_token" "$tts_token_file"
fi
ensure_secret_permissions "$tts_token_file" "tts_internal_token"
[[ -s "$tts_token_file" ]] || { printf 'TTS internal token file is empty: %s\n' "$tts_token_file" >&2; exit 1; }

# Ensure secrets for Bark Basic Auth
bark_user_file="$script_dir/secrets/bark_basic_auth_user"
if [[ ! -f "$bark_user_file" ]]; then
  user_val=""
  if [[ -n "${BARK_BASIC_AUTH_USER:-}" ]]; then
    user_val="$BARK_BASIC_AUTH_USER"
  elif grep -q '^BARK_BASIC_AUTH_USER=' "$env_file" 2>/dev/null; then
    val="$(grep '^BARK_BASIC_AUTH_USER=' "$env_file" | head -n1 | cut -d= -f2- | tr -d ' "[:space:]' | tr -d "'")"
    [[ -n "$val" ]] && user_val="$val"
  fi
  [[ -z "$user_val" ]] && user_val="bark_admin"
  printf '%s\n' "$user_val" > "$bark_user_file"
fi
ensure_secret_permissions "$bark_user_file" "bark_basic_auth_user"

bark_pass_file="$script_dir/secrets/bark_basic_auth_password"
if [[ ! -f "$bark_pass_file" ]]; then
  pass_val=""
  if [[ -n "${BARK_BASIC_AUTH_PASSWORD:-}" ]]; then
    pass_val="$BARK_BASIC_AUTH_PASSWORD"
  elif grep -q '^BARK_BASIC_AUTH_PASSWORD=' "$env_file" 2>/dev/null; then
    val="$(grep '^BARK_BASIC_AUTH_PASSWORD=' "$env_file" | head -n1 | cut -d= -f2- | tr -d ' "[:space:]' | tr -d "'")"
    [[ -n "$val" ]] && pass_val="$val"
  fi
  [[ -z "$pass_val" ]] && pass_val="$(openssl rand -hex 24 2>/dev/null || head -c 32 /dev/urandom | xxd -p | head -n1)"
  printf '%s\n' "$pass_val" > "$bark_pass_file"
fi
ensure_secret_permissions "$bark_pass_file" "bark_basic_auth_password"

[[ -s "$bark_user_file" ]] || { printf 'Bark basic auth user file is empty.\n' >&2; exit 1; }
[[ -s "$bark_pass_file" ]] || { printf 'Bark basic auth password file is empty.\n' >&2; exit 1; }
[[ -f "$script_dir/bark-entrypoint.sh" ]] && chmod 755 "$script_dir/bark-entrypoint.sh" || true
[[ -f "$script_dir/smoke-test-bark.sh" ]] && chmod 755 "$script_dir/smoke-test-bark.sh" || true

# 1. Back up the active named-volume database through the gateway image.
backup_dir="$script_dir/data/backups"
mkdir -p "$backup_dir"
if docker volume inspect bank-event-gateway_gateway_data >/dev/null 2>&1; then
  backup_name="gateway-$(date -u +%Y%m%d%H%M%S).db"
  printf 'Backing up the active gateway named volume...\n'
  docker run --rm --user 1000:1000 \
    -e APP_ENV=development -e DATA_DIR=/data -e DATABASE_PATH=/data/gateway.db \
    -e APP_MASTER_KEY_FILE=/run/secrets/app_master_key \
    -v bank-event-gateway_gateway_data:/data:rw \
    -v "$backup_dir:/backup:rw" \
    -v "$script_dir/secrets/app_master_key:/run/secrets/app_master_key:ro" \
    "$image_ref" --backup-to "/backup/$backup_name"
fi

[[ -f "$compose_file" ]] || { printf 'Missing compose file: %s\n' "$compose_file" >&2; exit 1; }
gateway_current_file="$script_dir/.deployed-image"
browser_current_file="$script_dir/.deployed-browser-image"
tts_current_file="$script_dir/.deployed-tts-image"
bark_current_file="$script_dir/.deployed-bark-image"
gateway_current="$(cat "$gateway_current_file" 2>/dev/null || true)"
browser_current="$(cat "$browser_current_file" 2>/dev/null || true)"
tts_current="$(cat "$tts_current_file" 2>/dev/null || true)"
bark_current="$(cat "$bark_current_file" 2>/dev/null || true)"

export BARK_IMAGE_REF="$bark_image_ref"

# 2. Pull images and execute migration-only gate
pull_targets=(gateway auth-browser tts-gateway)
if docker compose --env-file "$env_file" -f "$compose_file" config --services 2>/dev/null | grep -q "^bark$"; then
  pull_targets+=(bark)
fi
if ! docker compose --env-file "$env_file" -f "$compose_file" pull "${pull_targets[@]}"; then
  echo "Failed to pull deployment images." >&2
  exit 1
fi

printf 'Executing database migration gate (--migrate-only)...\n'
if ! timeout "${MIGRATION_TIMEOUT:-90}" docker compose --env-file "$env_file" -f "$compose_file" run --rm --no-deps --entrypoint /gateway gateway --migrate-only; then
  echo "Database migration gate failed. Aborting deployment." >&2
  exit 1
fi

printf 'Verifying database integrity before startup (--check)...\n'
if ! timeout "${CHECK_TIMEOUT:-90}" docker compose --env-file "$env_file" -f "$compose_file" run --rm --no-deps --entrypoint /gateway gateway --check; then
  echo "Database integrity check failed. Aborting deployment." >&2
  exit 1
fi

# 3. Start services with health wait
if ! timeout "${READY_TIMEOUT:-120}" docker compose --env-file "$env_file" -f "$compose_file" up -d --remove-orphans --wait --wait-timeout "${READY_TIMEOUT:-120}"; then
  echo "Docker compose deployment failed. Dumping container status and logs:" >&2
  docker compose --env-file "$env_file" -f "$compose_file" ps -a || true
  docker compose --env-file "$env_file" -f "$compose_file" logs --tail 50 gateway auth-browser tts-gateway bark || true
  exit 1
fi

PORT="${PORT:-8080}" "$script_dir/verify-deployment.sh"
printf 'Running Bark authentication smoke test in the private Docker network...\n'
bark_user="$(tr -d '\r\n' < "$script_dir/secrets/bark_basic_auth_user")"
bark_pass="$(tr -d '\r\n' < "$script_dir/secrets/bark_basic_auth_password")"
docker run --rm --network acb-transaction-webhook_default \
  -v "$script_dir/smoke-test-bark.sh:/smoke-test-bark.sh:ro" \
  -e BARK_HOST=bark -e BARK_PORT=8080 -e BARK_USER="$bark_user" -e BARK_PASS="$bark_pass" \
  bash:5.3 /bin/bash /smoke-test-bark.sh
if [[ -n "$gateway_current" && "$gateway_current" != "$image_ref" ]]; then
  printf '%s\n' "$gateway_current" > "$script_dir/.previous-image"
fi
if [[ -n "$browser_current" && "$browser_current" != "$browser_image_ref" ]]; then
  printf '%s\n' "$browser_current" > "$script_dir/.previous-browser-image"
fi
if [[ -n "$tts_current" && "$tts_current" != "$tts_image_ref" ]]; then
  printf '%s\n' "$tts_current" > "$script_dir/.previous-tts-image"
fi
if [[ -n "$bark_current" && "$bark_current" != "$bark_image_ref" ]]; then
  printf '%s\n' "$bark_current" > "$script_dir/.previous-bark-image"
fi
printf '%s\n' "$image_ref" > "$gateway_current_file"
printf '%s\n' "$browser_image_ref" > "$browser_current_file"
printf '%s\n' "$tts_image_ref" > "$tts_current_file"
printf '%s\n' "$bark_image_ref" > "$bark_current_file"
printf 'Deployment successful: gateway=%s auth-browser=%s tts=%s bark=%s\n' "$image_ref" "$browser_image_ref" "${tts_image_ref:-none}" "$bark_image_ref"
