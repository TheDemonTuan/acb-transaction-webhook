#!/usr/bin/env bash
# Deploy Gateway using Warm Standby Blue/Green with unified compose.prod.yaml
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deploy/lib.sh
source "$SCRIPT_DIR/lib.sh"

UPGRADE_CORE="${UPGRADE_CORE:-0}"
RESUME_SOAK="${RESUME_SOAK:-0}"
SOAK_DURATION_SEC="${SOAK_DURATION_SEC:-900}"
DETACH_SOAK="${DETACH_SOAK:-0}"
IMAGE_REF="${IMAGE_REF:-${GATEWAY_IMAGE_REF:-}}"

# Parse options and arguments
while [[ $# -gt 0 ]]; do
  case "$1" in
    --upgrade-core)
      UPGRADE_CORE=1
      shift
      ;;
    --resume-soak)
      RESUME_SOAK=1
      shift
      ;;
    --soak-seconds)
      SOAK_DURATION_SEC="$2"
      shift 2
      ;;
    --detach-soak)
      DETACH_SOAK=1
      shift
      ;;
    -*)
      log_warn "Unknown option: $1"
      shift
      ;;
    *)
      if [[ -z "$IMAGE_REF" ]]; then
        IMAGE_REF="$1"
      fi
      shift
      ;;
  esac
done

# If resume-soak was requested, handle it directly
if [[ "$RESUME_SOAK" -eq 1 ]]; then
  log_info "Resuming soak observation..."
  resume_soak
  exit 0
fi

if [[ -z "$IMAGE_REF" ]]; then
  log_error "Usage: $0 [options] <gateway-image-digest>"
  exit 1
fi

validate_digest "$IMAGE_REF" "gateway"
validate_data_volume "$DATA_VOLUME_NAME"
validate_secrets
acquire_deploy_lock

ACTIVE_SLOT="$(get_active_slot)"
CANDIDATE_SLOT="$(get_candidate_slot "$ACTIVE_SLOT")"
set_deploy_state "PREFLIGHT" "Active: ${ACTIVE_SLOT}, Candidate: ${CANDIDATE_SLOT}"

log_info "========================================"
log_info "Deploying Gateway: Active is [${ACTIVE_SLOT}] -> Candidate is [${CANDIDATE_SLOT}]"
log_info "Target Gateway Image: ${IMAGE_REF}"
if [[ "$UPGRADE_CORE" -eq 1 ]]; then
  log_info "Deployment Mode: CORE UPGRADE (Worker/Browser/TTS/Bark included)"
else
  log_info "Deployment Mode: WEB-ONLY (Worker and core singleton services untouched)"
fi
log_info "========================================"

# Core upgrade gate: check active authentication attempts before proceeding
if [[ "$UPGRADE_CORE" -eq 1 ]]; then
  check_active_auth_gate
  worker_img="${WORKER_IMAGE_REF:-}"
  browser_img="${AUTH_BROWSER_IMAGE_REF:-${BROWSER_IMAGE_REF:-}}"
  tts_img="${TTS_GATEWAY_IMAGE_REF:-${TTS_IMAGE_REF:-}}"
  bark_img="${BARK_IMAGE_REF:-}"
  [[ -n "$worker_img" ]] && validate_digest "$worker_img" "worker"
  [[ -n "$browser_img" ]] && validate_digest "$browser_img" "auth-browser"
  [[ -n "$tts_img" ]] && validate_digest "$tts_img" "tts-gateway"
  [[ -n "$bark_img" ]] && validate_digest "$bark_img" "bark"
fi

# Preflight online SQLite backup and manifest
set_deploy_state "BACKUP"
dbtool_img="${DBTOOL_IMAGE_REF:-ghcr.io/thedemontuan/acb-transaction-webhook-dbtool:latest}"
create_preflight_backup "$DATA_VOLUME_NAME" "$dbtool_img" "$ACTIVE_SLOT" >/dev/null

# Execute dbtool schema migration if enabled
if [[ "${SKIP_MIGRATION:-0}" != "1" && -n "$dbtool_img" ]]; then
  set_deploy_state "MIGRATION"
  run_dbtool_migration "$DATA_VOLUME_NAME" "$dbtool_img"
fi

# Pull images
set_deploy_state "PULL"
if [[ "$UPGRADE_CORE" -eq 1 ]]; then
  log_info "Pulling core services and candidate gateway..."
  docker compose -f "$COMPOSE_FILE" pull worker auth-browser tts-gateway bark "gateway-${CANDIDATE_SLOT}"
  log_info "Starting core singleton services..."
  docker compose -f "$COMPOSE_FILE" up -d worker auth-browser tts-gateway bark
else
  log_info "Web-only deploy: Pulling ONLY candidate [gateway-${CANDIDATE_SLOT}]..."
  docker compose -f "$COMPOSE_FILE" pull "gateway-${CANDIDATE_SLOT}" 2>/dev/null || docker pull "$IMAGE_REF"
fi

# Start candidate slot with new image
set_deploy_state "START_CANDIDATE"
log_info "Starting candidate slot [gateway-${CANDIDATE_SLOT}]..."
if [[ "$CANDIDATE_SLOT" == "green" ]]; then
  IMAGE_REF_GREEN="$IMAGE_REF" docker compose -f "$COMPOSE_FILE" up -d gateway-green
else
  IMAGE_REF_BLUE="$IMAGE_REF" docker compose -f "$COMPOSE_FILE" up -d gateway-blue
fi

# Wait for candidate readiness probe
set_deploy_state "WAIT_READY"
if ! wait_for_candidate_ready "$CANDIDATE_SLOT" "${READY_TIMEOUT:-60}"; then
  log_error "Candidate slot [${CANDIDATE_SLOT}] failed readiness probe! Aborting cutover."
  log_warn "Stopping candidate container [gateway-${CANDIDATE_SLOT}]. Active slot [${ACTIVE_SLOT}] remains untouched."
  docker compose -f "$COMPOSE_FILE" stop "gateway-${CANDIDATE_SLOT}" 2>/dev/null || true
  set_deploy_state "FAILED_CANDIDATE"
  exit 1
fi

# Atomic switch on Traefik
set_deploy_state "SWITCH_ROUTE"
log_info "Switching Traefik traffic pointer to candidate [${CANDIDATE_SLOT}]..."
atomic_switch_route "$CANDIDATE_SLOT"
printf "%s" "$ACTIVE_SLOT" > "$PREVIOUS_SLOT_FILE"

# Production route identity acknowledgement
set_deploy_state "ACK_ROUTE"
if ! ack_route_identity "$CANDIDATE_SLOT" "${ROUTE_ACK_TIMEOUT:-15}"; then
  log_error "Route identity acknowledgment failed for candidate [${CANDIDATE_SLOT}]! Executing automatic rollback to [${ACTIVE_SLOT}]..."
  atomic_switch_route "$ACTIVE_SLOT"
  ack_route_identity "$ACTIVE_SLOT" "${ROUTE_ACK_TIMEOUT:-15}" || true
  stop_standby_container "$CANDIDATE_SLOT"
  set_deploy_state "ROLLED_BACK" "Route identity ACK failure on candidate ${CANDIDATE_SLOT}"
  exit 1
fi

log_info "Cutover successful! Traefik is now routing live traffic to slot [${CANDIDATE_SLOT}]."
log_info "Old slot [${ACTIVE_SLOT}] remains active during soak period (${SOAK_DURATION_SEC}s) for instant rollback."

# 15-minute resumable soak observation
set_deploy_state "SOAK"
if [[ "$DETACH_SOAK" -eq 1 || "${SOAK_BACKGROUND:-0}" == "1" ]]; then
  log_info "Detaching soak observation to background process..."
  mkdir -p "$SCRIPT_DIR/data"
  nohup bash -c "source '${SCRIPT_DIR}/lib.sh' && run_resumable_soak '${CANDIDATE_SLOT}' '${ACTIVE_SLOT}' '${SOAK_DURATION_SEC}'" > "$SCRIPT_DIR/data/soak.log" 2>&1 &
  log_info "Soak detached (PID: $!). State recorded in $SOAK_STATE_FILE."
else
  if ! run_resumable_soak "$CANDIDATE_SLOT" "$ACTIVE_SLOT" "$SOAK_DURATION_SEC"; then
    log_error "Soak observation failed! Active route automatically reverted to [${ACTIVE_SLOT}]."
    set_deploy_state "FAILED_SOAK"
    exit 1
  fi
  set_deploy_state "COMPLETED" "Slot ${CANDIDATE_SLOT} is live and soak completed."
fi

release_deploy_lock
log_info "Deployment transaction completed successfully."
exit 0
