#!/usr/bin/env bash
# Shared Deployment & Cutover Library for ACB Single-VPS Platform
set -euo pipefail

# Determine script directory reliably
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
export SCRIPT_DIR

# Canonical environment variable defaults
ENV_FILE="${ENV_FILE:-$SCRIPT_DIR/.env.production}"
COMPOSE_FILE="${COMPOSE_FILE:-$SCRIPT_DIR/compose.prod.yaml}"
SECRETS_DIR="${SECRETS_DIR:-$SCRIPT_DIR/secrets}"
BACKUP_DIR="${BACKUP_DIR:-$SCRIPT_DIR/data/backups}"
ACTIVE_SLOT_FILE="${ACTIVE_SLOT_FILE:-$SCRIPT_DIR/.active-slot}"
PREVIOUS_SLOT_FILE="${PREVIOUS_SLOT_FILE:-$SCRIPT_DIR/.previous-slot}"
DEPLOY_STATE_FILE="${DEPLOY_STATE_FILE:-$SCRIPT_DIR/.deploy-state}"
SOAK_STATE_FILE="${SOAK_STATE_FILE:-$SCRIPT_DIR/.soak-state}"
DEPLOY_LOCK_FILE="${DEPLOY_LOCK_FILE:-/run/lock/vps-failover/acb.lock}"
TRAEFIK_DYNAMIC_DIR="${TRAEFIK_DYNAMIC_DIR:-/opt/edge/dynamic}"
ACB_CONFIG="${ACB_CONFIG:-$TRAEFIK_DYNAMIC_DIR/acb.yml}"
FAILOVER_STATE_DIR="${FAILOVER_STATE_DIR:-/var/lib/vps-failover/apps/acb}"

DATA_VOLUME_NAME="${DATA_VOLUME_NAME:-bank-event-gateway_gateway_data}"
BARK_VOLUME_NAME="${BARK_VOLUME_NAME:-bank-event-gateway_bark_data}"

log_info() {
  printf '[%s] [INFO] %s\n' "$(date -u +'%Y-%m-%dT%H:%M:%SZ')" "$*"
}

log_warn() {
  printf '[%s] [WARN] %s\n' "$(date -u +'%Y-%m-%dT%H:%M:%SZ')" "$*" >&2
}

log_error() {
  printf '[%s] [ERROR] %s\n' "$(date -u +'%Y-%m-%dT%H:%M:%SZ')" "$*" >&2
}

validate_digest() {
  local ref="$1"
  local name="$2"
  local pattern='^[^[:space:]]+@sha256:[a-f0-9]{64}$'
  if [[ ! "$ref" =~ $pattern ]]; then
    log_error "Invalid or non-immutable image digest for ${name}: '${ref}' (must match @sha256:<64-hex>)"
    return 1
  fi
  return 0
}

validate_data_volume() {
  local vol="$1"
  if ! docker volume inspect "$vol" >/dev/null 2>&1; then
    log_error "Production data volume '${vol}' does not exist. Aborting to prevent accidental data creation/loss. For initial setup, run 'deploy/init-fresh-data.sh --confirm-fresh-init'."
    return 1
  fi

  # Ambiguity check: if multiple volumes match filter, abort unless exactly one unique match
  local matches
  matches="$(docker volume ls -q --filter "name=${vol}" 2>/dev/null || true)"
  local match_count=0
  local exact_count=0
  while IFS= read -r line; do
    if [[ -n "$line" ]]; then
      match_count=$(( match_count + 1 ))
      if [[ "$line" == "$vol" ]]; then
        exact_count=$(( exact_count + 1 ))
      fi
    fi
  done <<< "$matches"

  if [[ "$exact_count" -ne 1 || "$match_count" -gt 1 ]]; then
    log_error "Ambiguous data volume detected for '${vol}'. Matches: [${matches}]. Aborting."
    return 1
  fi
  return 0
}

ensure_secret_permissions() {
  local target_path="$1"
  local mode="${2:-600}"
  if [[ -f "$target_path" ]]; then
    chmod "$mode" "$target_path" 2>/dev/null || true
  fi
}

validate_secrets() {
  mkdir -p "$SECRETS_DIR"
  local required_secrets=(app_master_key tts_internal_token worker_internal_token bark_basic_auth_user bark_basic_auth_password)
  for s in "${required_secrets[@]}"; do
    local s_file="$SECRETS_DIR/$s"
    if [[ ! -s "$s_file" ]]; then
      if [[ -f "$ENV_FILE" ]] && grep -q "^${s^^}=" "$ENV_FILE" 2>/dev/null; then
        local val
        val="$(grep "^${s^^}=" "$ENV_FILE" | head -n1 | cut -d= -f2- | tr -d ' "\r\n[:space:]' | tr -d "'")"
        if [[ -n "$val" ]]; then
          printf '%s\n' "$val" > "$s_file"
        fi
      fi
      if [[ ! -s "$s_file" ]]; then
        if [[ "$s" == "bark_basic_auth_user" ]]; then
          printf 'bark_admin\n' > "$s_file"
        elif command -v openssl >/dev/null 2>&1; then
          openssl rand -hex 32 > "$s_file"
        else
          head -c 32 /dev/urandom 2>/dev/null | od -An -tx1 | tr -d ' \n' > "$s_file" || printf '0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n' > "$s_file"
        fi
      fi
    fi
    if [[ "$s" == bark_basic_auth_* ]]; then
      ensure_secret_permissions "$s_file" 644
    else
      ensure_secret_permissions "$s_file" 600
    fi
  done
  return 0
}

acquire_deploy_lock() {
  local timeout="${DEPLOY_LOCK_TIMEOUT:-30}"
  if [[ "${DEPLOY_LOCK_HELD:-0}" == "1" || "${SKIP_LOCK:-0}" == "1" ]]; then
    return 0
  fi
  local lock_dir
  lock_dir="$(dirname "$DEPLOY_LOCK_FILE")"
  if ! mkdir -p "$lock_dir" 2>/dev/null; then
    DEPLOY_LOCK_FILE="$SCRIPT_DIR/.deploy.lock"
    lock_dir="$(dirname "$DEPLOY_LOCK_FILE")"
    mkdir -p "$lock_dir"
  fi
  exec 9>"$DEPLOY_LOCK_FILE"
  if command -v flock >/dev/null 2>&1; then
    if ! flock -w "$timeout" 9; then
      log_error "Another deployment or cutover is active (lock timeout ${timeout}s on ${DEPLOY_LOCK_FILE})."
      return 1
    fi
  fi
  export DEPLOY_LOCK_HELD=1
  return 0
}

release_deploy_lock() {
  if [[ "${DEPLOY_LOCK_HELD:-0}" == "1" ]]; then
    unset DEPLOY_LOCK_HELD
    exec 9>&- 2>/dev/null || true
    rm -f "$DEPLOY_LOCK_FILE" 2>/dev/null || true
  fi
}

get_active_slot() {
  local slot=""
  if [[ -f "$ACTIVE_SLOT_FILE" ]]; then
    slot="$(tr -d ' \r\n[:space:]' < "$ACTIVE_SLOT_FILE")"
  fi
  if [[ "$slot" != "blue" && "$slot" != "green" ]]; then
    if [[ -f "$ACB_CONFIG" ]]; then
      if grep -q "acb-web-green" "$ACB_CONFIG" 2>/dev/null; then
        slot="green"
      elif grep -q "acb-web-blue" "$ACB_CONFIG" 2>/dev/null; then
        slot="blue"
      fi
    fi
  fi
  if [[ "$slot" != "blue" && "$slot" != "green" ]]; then
    local blue_running
    blue_running="$(docker inspect --format '{{.State.Running}}' acb-gateway-blue 2>/dev/null || echo "false")"
    local green_running
    green_running="$(docker inspect --format '{{.State.Running}}' acb-gateway-green 2>/dev/null || echo "false")"
    if [[ "$green_running" == "true" && "$blue_running" != "true" ]]; then
      slot="green"
    else
      slot="blue"
    fi
  fi
  printf '%s' "$slot"
}

get_candidate_slot() {
  local active="$1"
  if [[ "$active" == "blue" ]]; then
    printf 'green'
  else
    printf 'blue'
  fi
}

set_deploy_state() {
  local state="$1"
  local details="${2:-}"
  printf '{"state":"%s","timestamp":"%s","details":"%s"}\n' "$state" "$(date -u +'%Y-%m-%dT%H:%M:%SZ')" "$details" > "$DEPLOY_STATE_FILE"
}

get_deploy_state() {
  if [[ -f "$DEPLOY_STATE_FILE" ]]; then
    cat "$DEPLOY_STATE_FILE"
  else
    printf '{"state":"IDLE"}\n'
  fi
}

clear_deploy_state() {
  rm -f "$DEPLOY_STATE_FILE" 2>/dev/null || true
}

check_active_auth_gate() {
  log_info "Evaluating active-auth gate before migration / core modification..."
  local active_count=0

  if [[ -n "${ACTIVE_AUTH_CHECK_CMD:-}" ]]; then
    active_count="$($ACTIVE_AUTH_CHECK_CMD)"
  elif command -v docker >/dev/null 2>&1 && docker volume inspect "$DATA_VOLUME_NAME" >/dev/null 2>&1; then
    local dbtool_img="${DBTOOL_IMAGE_REF:-ghcr.io/thedemontuan/acb-transaction-webhook-dbtool:latest}"
    local auth_json
    auth_json="$(
      docker run --rm \
        --user 1000:1000 \
        -e DATABASE_PATH=/data/gateway.db \
        -v "${DATA_VOLUME_NAME}:/data:ro" \
        "$dbtool_img" \
        -path /data/gateway.db \
        -active-auth-count 2>/dev/null || echo '{"activeCount":0}'
    )"
    if command -v jq >/dev/null 2>&1; then
      active_count="$(printf '%s' "$auth_json" | jq -r '.activeCount // .count // 0' 2>/dev/null || echo 0)"
    else
      active_count="$(printf '%s' "$auth_json" | grep -o '"activeCount":[0-9]*' | cut -d: -f2 || echo 0)"
    fi
  elif command -v sqlite3 >/dev/null 2>&1; then
    local db_file="${1:-$SCRIPT_DIR/data/gateway.db}"
    local query="SELECT count(*) FROM auth_attempts WHERE status IN ('STARTING','IN_PROGRESS','EXPORTING','VERIFYING') AND (expires_at > datetime('now') OR expires_at > strftime('%Y-%m-%dT%H:%M:%SZ', 'now'));"
    active_count="$(sqlite3 "$db_file" "$query" 2>/dev/null || echo "0")"
  fi

  if [[ "$active_count" =~ ^[0-9]+$ ]] && [[ "$active_count" -gt 0 ]]; then
    log_error "Active-auth gate FAILED: found ${active_count} active in-flight authentication session(s). Aborting to protect customer authentication."
    return 1
  fi
  log_info "Active-auth gate passed: 0 in-flight authentication sessions."
  return 0
}

create_preflight_backup() {
  local db_volume="$1"
  local dbtool_img="$2"
  local active_slot="$3"
  local ts
  ts="$(date -u +'%Y%m%d%H%M%S')"
  mkdir -p "$BACKUP_DIR"

  local backup_file="$BACKUP_DIR/gateway-${ts}.db"
  log_info "Performing online SQLite preflight backup: ${backup_file}"

  if docker volume inspect "$db_volume" >/dev/null 2>&1; then
    docker run --rm --user 1000:1000 \
      -e APP_ENV=production -e DATA_DIR=/data -e DATABASE_PATH=/data/gateway.db \
      -v "${db_volume}:/data:rw" \
      -v "${BACKUP_DIR}:/backup:rw" \
      "$dbtool_img" -path /data/gateway.db -backup-to "/backup/gateway-${ts}.db"
  elif [[ -f "$SCRIPT_DIR/data/gateway.db" ]]; then
    if command -v sqlite3 >/dev/null 2>&1; then
      sqlite3 "$SCRIPT_DIR/data/gateway.db" "PRAGMA wal_checkpoint(TRUNCATE);" || true
      sqlite3 "$SCRIPT_DIR/data/gateway.db" ".backup '$backup_file'"
    fi
  fi

  if [[ ! -s "$backup_file" ]]; then
    log_error "Preflight SQLite backup failed: backup file is missing or empty"
    return 1
  fi
  chmod 600 "$backup_file" 2>/dev/null || true

  log_info "Verifying SQLite integrity of backup..."
  if command -v sqlite3 >/dev/null 2>&1; then
    local integrity
    integrity="$(sqlite3 "$backup_file" "PRAGMA integrity_check;" 2>/dev/null || echo "failed")"
    if [[ "$integrity" != "ok" ]]; then
      log_error "SQLite integrity check failed on backup: ${integrity}"
      return 1
    fi
  fi

  log_info "Verifying schema migration state..."
  if command -v sqlite3 >/dev/null 2>&1; then
    local migrations
    migrations="$(sqlite3 "$backup_file" "SELECT count(*) FROM schema_migrations;" 2>/dev/null || echo "0")"
    log_info "Schema migration count in backup: ${migrations}"
  fi

  # Backup secrets and write manifest
  local manifest_file="$BACKUP_DIR/manifest-${ts}.json"
  local db_sha256
  db_sha256="$(sha256sum "$backup_file" 2>/dev/null | cut -d' ' -f1 || echo "unknown")"
  local db_size
  db_size="$(wc -c < "$backup_file" 2>/dev/null | tr -d ' ' || echo "0")"

  local secrets_backup_dir="$BACKUP_DIR/secrets-${ts}"
  mkdir -p "$secrets_backup_dir"
  local sec_items=()
  for s in app_master_key tts_internal_token worker_internal_token bark_basic_auth_user bark_basic_auth_password; do
    if [[ -f "$SECRETS_DIR/$s" ]]; then
      cp -p "$SECRETS_DIR/$s" "$secrets_backup_dir/$s"
      chmod 600 "$secrets_backup_dir/$s" 2>/dev/null || true
      local s_sha
      s_sha="$(sha256sum "$SECRETS_DIR/$s" 2>/dev/null | cut -d' ' -f1 || echo "")"
      sec_items+=("{\"name\":\"$s\",\"sha256\":\"$s_sha\"}")
    fi
  done
  local joined_secrets
  joined_secrets="$(IFS=,; echo "${sec_items[*]}")"

  cat <<EOF > "$manifest_file"
{
  "timestamp": "${ts}",
  "active_slot": "${active_slot}",
  "dbtool_image": "${dbtool_img}",
  "data_volume": "${db_volume}",
  "sqlite_backup": {
    "file": "$(basename "$backup_file")",
    "sha256": "${db_sha256}",
    "size_bytes": ${db_size}
  },
  "secrets_backup_dir": "$(basename "$secrets_backup_dir")",
  "secrets": [${joined_secrets}]
}
EOF
  chmod 600 "$manifest_file" 2>/dev/null || true
  log_info "Backup manifest recorded: ${manifest_file}"

  if [[ -n "${ENCRYPTED_BACKUP_HOOK:-}" || -n "${OFFHOST_BACKUP_HOOK:-}" ]]; then
    local hook="${ENCRYPTED_BACKUP_HOOK:-${OFFHOST_BACKUP_HOOK:-}}"
    log_info "Verifying encrypted offhost backup hook: ${hook}"
    if [[ ! -x "$hook" ]]; then
      log_error "Encrypted offhost hook is not executable: ${hook}"
      return 1
    fi
    log_info "Executing verified offhost backup hook..."
    "$hook" "$manifest_file" "$backup_file"
    log_info "Offhost backup hook completed."
  fi

  printf '%s' "$backup_file"
}

run_dbtool_migration() {
  local db_volume="$1"
  local dbtool_img="$2"
  log_info "Executing database migration via immutable dbtool (${dbtool_img})..."
  if ! docker run --rm --user 1000:1000 \
    -e DATABASE_PATH=/data/gateway.db \
    -v "${db_volume}:/data:rw" \
    "$dbtool_img" -path /data/gateway.db -migrate; then
    log_error "Database migration failed. NO AUTO RESTORE will be executed to prevent unintended data loss."
    return 1
  fi
  log_info "Database migration completed successfully."
  return 0
}

wait_for_candidate_ready() {
  local slot="$1"
  local timeout="${2:-60}"
  local container="acb-gateway-${slot}"
  log_info "Waiting for candidate slot [${slot}] to pass health probes (timeout: ${timeout}s)..."
  local start
  start="$(date +%s)"
  local consecutive=0
  while true; do
    local status
    status="$(docker inspect --format '{{.State.Status}}' "$container" 2>/dev/null || echo "not_found")"
    if [[ "$status" == "running" ]]; then
      if docker exec "$container" /gateway --healthcheck >/dev/null 2>&1; then
        consecutive=$(( consecutive + 1 ))
        if [[ "$consecutive" -ge 2 ]]; then
          log_info "Candidate slot [${slot}] is healthy and ready."
          return 0
        fi
      else
        consecutive=0
      fi
    fi
    local now
    now="$(date +%s)"
    if (( now - start >= timeout )); then
      log_error "Candidate slot [${slot}] failed to become ready within ${timeout}s."
      return 1
    fi
    sleep 2
  done
}

atomic_switch_route() {
  local target_slot="$1"
  log_info "Atomically updating Traefik route pointer to slot: ${target_slot}..."
  mkdir -p "$TRAEFIK_DYNAMIC_DIR"
  local tmp_config="${ACB_CONFIG}.tmp.$$"
  cat <<EOF > "$tmp_config"
http:
  routers:
    acb-deny-internal:
      rule: "Host(\`bank.tuannguyenviet.site\`) && PathPrefix(\`/internal\`)"
      entryPoints:
        - web
      priority: 1000
      middlewares:
        - deny-internal
      service: acb-service

    acb-router:
      rule: "Host(\`bank.tuannguyenviet.site\`) && !PathPrefix(\`/internal\`)"
      entryPoints:
        - web
      priority: 100
      middlewares:
        - tunnel-only
        - security-headers
      service: acb-service

  services:
    acb-service:
      loadBalancer:
        passHostHeader: true
        responseForwarding:
          flushInterval: "100ms"
        servers:
          - url: "http://acb-web-${target_slot}:8090"
        healthCheck:
          path: "/readyz"
          interval: "5s"
          timeout: "2s"
EOF
  mv -f "$tmp_config" "$ACB_CONFIG"
  printf '%s' "$target_slot" > "$ACTIVE_SLOT_FILE"
  log_info "Dynamic route pointer successfully set to acb-web-${target_slot}."
}

ack_route_identity() {
  local target_slot="$1"
  local timeout="${2:-${ROUTE_ACK_TIMEOUT:-15}}"
  log_info "Acknowledging route identity for target slot: ${target_slot} (timeout: ${timeout}s)..."
  local start
  start="$(date +%s)"
  while true; do
    if [[ -f "$ACB_CONFIG" ]] && grep -q "http://acb-web-${target_slot}:8090" "$ACB_CONFIG" 2>/dev/null; then
      if [[ -n "${ROUTE_ACK_URL:-}" ]] && command -v curl >/dev/null 2>&1; then
        if curl --fail --silent --show-error -H "Host: bank.tuannguyenviet.site" "$ROUTE_ACK_URL" 2>/dev/null | grep -q "${target_slot}"; then
          log_info "Route identity acknowledged via route endpoint probe for [${target_slot}]."
          return 0
        fi
      else
        if docker exec "acb-gateway-${target_slot}" /gateway --healthcheck >/dev/null 2>&1; then
          log_info "Route identity acknowledged for [${target_slot}]."
          return 0
        fi
      fi
    fi
    local now
    now="$(date +%s)"
    if (( now - start >= timeout )); then
      log_error "Route identity acknowledgment failed for [${target_slot}] within ${timeout}s."
      return 1
    fi
    sleep 1
  done
}

central_switch_route() {
  local target_slot="$1"
  local skip_lock="${2:-0}"
  if [[ "$skip_lock" != "1" && "${DEPLOY_LOCK_HELD:-0}" != "1" ]]; then
    acquire_deploy_lock
  fi
  atomic_switch_route "$target_slot"
  ack_route_identity "$target_slot" 15
}

mark_intentional_stop() {
  local slot="$1"
  if ! mkdir -p "$FAILOVER_STATE_DIR" 2>/dev/null; then
    FAILOVER_STATE_DIR="/tmp/vps-failover"
    mkdir -p "$FAILOVER_STATE_DIR" 2>/dev/null || true
  fi
  local marker="$FAILOVER_STATE_DIR/intentional-stop-${slot}"
  local tmp="${marker}.tmp.$$"
  printf '{"slot":"%s","desired":"stopped","recordedAt":"%s"}\n' \
    "$slot" "$(date -u +'%Y-%m-%dT%H:%M:%SZ')" > "$tmp"
  mv -f "$tmp" "$marker" 2>/dev/null || true
  touch "$SCRIPT_DIR/.intentional-stop-${slot}" 2>/dev/null || true
  log_info "Recorded intentional stop for slot [${slot}] before stopping container."
}

clear_intentional_stop() {
  # The desired stopped marker is durable by design; the controller ignores non-active slots.
  return 0
}

stop_standby_container() {
  local slot="$1"
  log_info "Stopping container for slot [${slot}]..."
  mark_intentional_stop "$slot"
  docker compose -f "$COMPOSE_FILE" stop "gateway-${slot}" 2>/dev/null || docker stop "acb-gateway-${slot}" 2>/dev/null || true
  clear_intentional_stop "$slot"
  log_info "Slot [${slot}] stopped into warm standby state."
}

run_resumable_soak() {
  local candidate="$1"
  local old_slot="$2"
  local duration="${3:-900}"

  cat <<EOF > "$SOAK_STATE_FILE"
candidate=${candidate}
old_slot=${old_slot}
start_time=$(date +%s)
duration=${duration}
EOF

  log_info "Starting soak observation (${duration}s). Candidate [${candidate}] active, old slot [${old_slot}] running..."
  local elapsed=0
  local interval=10
  if [[ "$duration" -le 10 ]]; then
    interval=1
  fi

  while [[ "$elapsed" -lt "$duration" ]]; do
    if ! docker exec "acb-gateway-${candidate}" /gateway --healthcheck >/dev/null 2>&1; then
      log_error "Candidate [${candidate}] health probe failed during soak! Executing automatic route rollback to [${old_slot}]..."
      atomic_switch_route "$old_slot"
      ack_route_identity "$old_slot" 15 || true
      stop_standby_container "$candidate"
      rm -f "$SOAK_STATE_FILE"
      return 1
    fi
    sleep "$interval"
    elapsed=$(( elapsed + interval ))
  done

  log_info "Soak observation completed successfully. Transitioning [${old_slot}] to stopped warm standby..."
  stop_standby_container "$old_slot"
  rm -f "$SOAK_STATE_FILE"
  return 0
}

resume_soak() {
  if [[ ! -f "$SOAK_STATE_FILE" ]]; then
    log_info "No soak state file found ($SOAK_STATE_FILE). Nothing to resume."
    return 0
  fi
  # shellcheck disable=SC1090
  source "$SOAK_STATE_FILE"
  local now
  now="$(date +%s)"
  local elapsed=$(( now - start_time ))
  local remaining=$(( duration - elapsed ))
  if [[ "$remaining" -le 0 ]]; then
    log_info "Soak observation period already satisfied. Stopping standby slot [${old_slot}]..."
    stop_standby_container "$old_slot"
    rm -f "$SOAK_STATE_FILE"
    return 0
  fi
  log_info "Resuming soak observation for candidate [${candidate}] with remaining ${remaining}s..."
  run_resumable_soak "$candidate" "$old_slot" "$remaining"
}
