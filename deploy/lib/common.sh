#!/usr/bin/env bash
# deploy/lib/common.sh
# Shared logging, environment validation, paths, and secrets primitives.
set -euo pipefail

# Determine script directory reliably
if [[ -z "${SCRIPT_DIR:-}" ]]; then
  SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
  export SCRIPT_DIR
fi

ENV_FILE="${ENV_FILE:-$SCRIPT_DIR/.env.production}"
RELEASE_ENV_FILE="${RELEASE_ENV_FILE:-$SCRIPT_DIR/.release.env}"
COMPOSE_FILE="${COMPOSE_FILE:-$SCRIPT_DIR/compose.prod.yaml}"
SECRETS_DIR="${SECRETS_DIR:-$SCRIPT_DIR/secrets}"
BACKUP_DIR="${BACKUP_DIR:-$SCRIPT_DIR/data/backups}"
ACTIVE_SLOT_FILE="${ACTIVE_SLOT_FILE:-$SCRIPT_DIR/.active-slot}"
PREVIOUS_SLOT_FILE="${PREVIOUS_SLOT_FILE:-$SCRIPT_DIR/.previous-slot}"
FRONTEND_ACTIVE_SLOT_FILE="${FRONTEND_ACTIVE_SLOT_FILE:-$SCRIPT_DIR/.active-frontend-slot}"
FRONTEND_PREVIOUS_SLOT_FILE="${FRONTEND_PREVIOUS_SLOT_FILE:-$SCRIPT_DIR/.previous-frontend-slot}"
DEPLOY_STATE_FILE="${DEPLOY_STATE_FILE:-$SCRIPT_DIR/.deploy-state}"
SOAK_STATE_FILE="${SOAK_STATE_FILE:-$SCRIPT_DIR/.soak-state}"
DEPLOY_LOCK_FILE="${DEPLOY_LOCK_FILE:-/run/lock/vps-failover/acb.lock}"
if [[ -z "${TRAEFIK_DYNAMIC_DIR:-}" ]]; then
  if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
    _detected_dynamic="$(docker inspect edge-traefik --format '{{range .Mounts}}{{if eq .Destination "/etc/traefik/dynamic"}}{{.Source}}{{end}}{{end}}' 2>/dev/null || true)"
    if [[ -n "$_detected_dynamic" ]]; then
      TRAEFIK_DYNAMIC_DIR="$_detected_dynamic"
    fi
  fi
  TRAEFIK_DYNAMIC_DIR="${TRAEFIK_DYNAMIC_DIR:-/opt/platform/edge/dynamic}"
fi
ACB_CONFIG="${ACB_CONFIG:-$TRAEFIK_DYNAMIC_DIR/acb.yml}"

if [[ -z "${EDGE_PROBE_SCRIPT:-}" ]]; then
  if [[ -f "$SCRIPT_DIR/edge-probe.sh" ]]; then
    EDGE_PROBE_SCRIPT="$SCRIPT_DIR/edge-probe.sh"
  elif [[ -f "$SCRIPT_DIR/../platform/edge/probe.sh" ]]; then
    EDGE_PROBE_SCRIPT="$SCRIPT_DIR/../platform/edge/probe.sh"
  fi
fi
export TRAEFIK_DYNAMIC_DIR ACB_CONFIG EDGE_PROBE_SCRIPT
FAILOVER_STATE_DIR="${FAILOVER_STATE_DIR:-/var/lib/vps-failover/apps/acb}"
DATA_VOLUME_NAME="${DATA_VOLUME_NAME:-bank-event-gateway_gateway_data}"
BARK_VOLUME_NAME="${BARK_VOLUME_NAME:-bank-event-gateway_bark_data}"

# Source release environment helper if available
if [[ -f "$SCRIPT_DIR/release-env.sh" ]]; then
  # shellcheck source=deploy/release-env.sh
  source "$SCRIPT_DIR/release-env.sh"
  if [[ -f "$RELEASE_ENV_FILE" ]]; then
    export_release_env "$RELEASE_ENV_FILE"
  fi
fi

log_info() {
  printf '[%s] [INFO] %s\n' "$(date -u +'%Y-%m-%dT%H:%M:%SZ')" "$*" >&2
}

log_warn() {
  printf '[%s] [WARN] %s\n' "$(date -u +'%Y-%m-%dT%H:%M:%SZ')" "$*" >&2
}

log_error() {
  printf '[%s] [ERROR] %s\n' "$(date -u +'%Y-%m-%dT%H:%M:%SZ')" "$*" >&2
}

validate_canonical_env() {
  if [[ ! -f "$ENV_FILE" ]]; then
    log_error "Canonical production environment file '$ENV_FILE' does not exist. Aborting to prevent unconfigured container execution."
    return 1
  fi
  # Refuse root-level env file if explicitly targeted
  local root_env="$SCRIPT_DIR/../.env.production"
  if [[ -f "$root_env" && "$ENV_FILE" == "$root_env" ]]; then
    log_error "Root-level .env.production is forbidden. Canonical configuration must reside at '$SCRIPT_DIR/.env.production'."
    return 1
  fi
  return 0
}

compose_prod() {
  local compose_flags=()
  if [[ -f "$ENV_FILE" ]]; then
    compose_flags+=(--env-file "$ENV_FILE")
  fi
  if [[ -f "$RELEASE_ENV_FILE" ]]; then
    compose_flags+=(--env-file "$RELEASE_ENV_FILE")
  fi
  docker compose "${compose_flags[@]}" -f "$COMPOSE_FILE" "$@"
}

ensure_secret_permissions() {
  local target_path="$1"
  local mode="${2:-600}"
  if [[ -e "$target_path" ]]; then
    chmod "$mode" "$target_path" 2>/dev/null || true
  fi
}

BARK_SECRET_GROUP="${BARK_SECRET_GROUP:-1000}"

prepare_bark_secret_permissions() {
  [[ "$BARK_SECRET_GROUP" =~ ^[0-9]+$ ]] || {
    log_error "BARK_SECRET_GROUP must be a numeric group ID."
    return 1
  }

  local secret_path
  for secret_path in \
    "$SECRETS_DIR/bark_basic_auth_user" \
    "$SECRETS_DIR/bark_basic_auth_password"; do
    if [[ ! -f "$secret_path" || -L "$secret_path" || ! -s "$secret_path" ]]; then
      log_error "Bark secret must be a non-empty regular file and must not be a symlink: $secret_path"
      return 1
    fi
    if ! chgrp "$BARK_SECRET_GROUP" "$secret_path" 2>/dev/null; then
      if ! command -v sudo >/dev/null 2>&1 || ! sudo -n chgrp "$BARK_SECRET_GROUP" "$secret_path"; then
        log_error "Cannot assign Bark secret '$secret_path' to group $BARK_SECRET_GROUP. Run the deployment as the file owner or correct the group as an operator."
        return 1
      fi
    fi
    if ! chmod 0640 "$secret_path" 2>/dev/null; then
      if ! command -v sudo >/dev/null 2>&1 || ! sudo -n chmod 0640 "$secret_path"; then
        log_error "Cannot set least-privilege mode 0640 on Bark secret '$secret_path'."
        return 1
      fi
    fi
  done
}

preflight_bark_secret_access() {
  local image_ref="$1"
  validate_digest "$image_ref" "bark"

  if [[ -n "${BARK_SECRET_PREFLIGHT_CMD:-}" ]]; then
    if ! $BARK_SECRET_PREFLIGHT_CMD "$image_ref"; then
      log_error "Bark secret access preflight failed."
      return 1
    fi
    return 0
  fi

  command -v docker >/dev/null 2>&1 || {
    log_error "Docker is required to verify Bark secret access."
    return 1
  }
  docker info >/dev/null 2>&1 || {
    log_error "Docker is unavailable; Bark secret access was not verified."
    return 1
  }

  local read_check='set -eu; for file in /run/secrets/bark_basic_auth_user /run/secrets/bark_basic_auth_password; do test -s "$file"; cat "$file" >/dev/null; done'
  if ! BARK_IMAGE_REF="$image_ref" compose_prod run --rm --no-deps --entrypoint /bin/sh bark -c "$read_check" >/dev/null 2>&1; then
    log_error "Bark cannot read its basic-auth secrets with the configured runtime identity."
    return 1
  fi
  if ! BARK_IMAGE_REF="$image_ref" compose_prod run --rm --no-deps --user 1000:1000 --entrypoint /bin/sh bark -c "$read_check" >/dev/null 2>&1; then
    log_error "Worker identity 1000:1000 cannot read the shared Bark basic-auth secrets."
    return 1
  fi
}

check_secret_permissions() {
  local target_path="$1"
  if [[ ! -e "$target_path" ]]; then
    return 0
  fi
  # On Windows/MSYS environments, POSIX permission bits are emulated as 644/755.
  # Preserve Windows development while strictly enforcing mode on Linux target hosts.
  if [[ "$(uname -s 2>/dev/null)" =~ MINGW|MSYS|CYGWIN ]]; then
    return 0
  fi
  if command -v stat >/dev/null 2>&1; then
    local mode=""
    mode="$(stat -c '%a' "$target_path" 2>/dev/null || true)"
    if [[ -z "$mode" ]]; then
      mode="$(stat -f '%Lp' "$target_path" 2>/dev/null || true)"
    fi
    if [[ -n "$mode" ]]; then
      local last_two="${mode: -2}"
      local base_name
      base_name="$(basename "$target_path")"
      if [[ "$base_name" == "bark_basic_auth_user" || "$base_name" == "bark_basic_auth_password" ]]; then
        if [[ "$mode" != "600" && "$mode" != "640" ]]; then
          log_error "Bark secret file '$target_path' has unsafe permissions (${mode}); expected 600 before preparation or 640 for runtime access."
          return 1
        fi
        if [[ "$mode" == "640" ]]; then
          local group_id=""
          group_id="$(stat -c '%g' "$target_path" 2>/dev/null || true)"
          if [[ -n "$group_id" && "$group_id" != "$BARK_SECRET_GROUP" ]]; then
            log_error "Bark secret file '$target_path' belongs to group ${group_id}; expected ${BARK_SECRET_GROUP}."
            return 1
          fi
        fi
      elif [[ "$last_two" != "00" ]]; then
        log_error "Secret file '$target_path' has unsafe permissions (${mode}). Secrets must not be group or world readable."
        return 1
      fi
    fi
  fi
  return 0
}

check_required_secrets() {
  if [[ ! -d "$SECRETS_DIR" ]]; then
    log_error "Secrets directory '$SECRETS_DIR' does not exist."
    return 1
  fi
  local required_secrets=(app_master_key tts_internal_token worker_internal_token bark_basic_auth_user bark_basic_auth_password)
  local missing=()
  for s in "${required_secrets[@]}"; do
    local s_file="$SECRETS_DIR/$s"
    if [[ ! -f "$s_file" || ! -s "$s_file" ]]; then
      missing+=("$s")
    else
      if ! check_secret_permissions "$s_file"; then
        return 1
      fi
    fi
  done
  if [[ ${#missing[@]} -gt 0 ]]; then
    log_error "Missing or empty required production secrets: [${missing[*]}]. Production deployment never generates or auto-rotates secrets. Provision them explicitly via 'deploy/init-fresh-data.sh --confirm-fresh-init' or 'deploy/provision-secrets.sh --confirm-fresh-provision'."
    return 1
  fi
  return 0
}

validate_secrets() {
  check_required_secrets
}

assert_fresh_installation() {
  if [[ -f "$SCRIPT_DIR/data/gateway.db" ]]; then
    log_error "assert_fresh_installation: Pre-existing database '$SCRIPT_DIR/data/gateway.db' detected. Aborting to prevent accidental data loss or key mismatch."
    return 1
  fi
  if command -v docker >/dev/null 2>&1; then
    if docker volume inspect "$DATA_VOLUME_NAME" >/dev/null 2>&1; then
      log_warn "assert_fresh_installation: Volume '$DATA_VOLUME_NAME' already exists."
      local vol_inspect
      vol_inspect="$(docker volume inspect "$DATA_VOLUME_NAME" 2>/dev/null || true)"
      if [[ -n "$vol_inspect" && "$vol_inspect" != "[]" ]]; then
        log_error "assert_fresh_installation: Pre-existing data volume '$DATA_VOLUME_NAME' exists. Aborting fresh installation."
        return 1
      fi
    fi
  fi
  if [[ -d "$SECRETS_DIR" ]]; then
    for s in app_master_key worker_internal_token tts_internal_token bark_basic_auth_password; do
      if [[ -s "$SECRETS_DIR/$s" ]]; then
        log_error "assert_fresh_installation: Pre-existing secret '$SECRETS_DIR/$s' detected. Aborting fresh provisioning."
        return 1
      fi
    done
  fi
  return 0
}

provision_fresh_secrets() {
  umask 077
  mkdir -p "$SECRETS_DIR"
  chmod 700 "$SECRETS_DIR" 2>/dev/null || true

  log_info "Provisioning high-entropy production secrets in $SECRETS_DIR..."

  local master_key_file="$SECRETS_DIR/app_master_key"
  if [[ ! -s "$master_key_file" ]]; then
    if command -v openssl >/dev/null 2>&1; then
      openssl rand -hex 32 > "$master_key_file"
    else
      head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' > "$master_key_file"
    fi
    chmod 600 "$master_key_file" 2>/dev/null || true
  fi

  local worker_token_file="$SECRETS_DIR/worker_internal_token"
  if [[ ! -s "$worker_token_file" ]]; then
    if command -v openssl >/dev/null 2>&1; then
      openssl rand -hex 32 > "$worker_token_file"
    else
      head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' > "$worker_token_file"
    fi
    chmod 600 "$worker_token_file" 2>/dev/null || true
  fi

  local tts_token_file="$SECRETS_DIR/tts_internal_token"
  if [[ ! -s "$tts_token_file" ]]; then
    if command -v openssl >/dev/null 2>&1; then
      openssl rand -hex 32 > "$tts_token_file"
    else
      head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' > "$tts_token_file"
    fi
    chmod 600 "$tts_token_file" 2>/dev/null || true
  fi

  local bark_user_file="$SECRETS_DIR/bark_basic_auth_user"
  if [[ ! -s "$bark_user_file" ]]; then
    printf 'bark_admin\n' > "$bark_user_file"
    chmod 600 "$bark_user_file" 2>/dev/null || true
  fi

  local bark_pass_file="$SECRETS_DIR/bark_basic_auth_password"
  if [[ ! -s "$bark_pass_file" ]]; then
    if command -v openssl >/dev/null 2>&1; then
      openssl rand -hex 32 > "$bark_pass_file"
    else
      head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' > "$bark_pass_file"
    fi
    chmod 600 "$bark_pass_file" 2>/dev/null || true
  fi

  check_required_secrets
}
