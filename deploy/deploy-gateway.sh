#!/usr/bin/env bash
# deploy/deploy-gateway.sh
# Isolated Gateway Blue/Green Deployment with Transaction Journal and Edge Identity ACK.
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deploy/lib.sh
source "$SCRIPT_DIR/lib.sh"

RESUME_SOAK="${RESUME_SOAK:-0}"
SOAK_DURATION_SEC="${SOAK_DURATION_SEC:-900}"
DETACH_SOAK="${DETACH_SOAK:-0}"
IMAGE_REF="${IMAGE_REF:-${GATEWAY_IMAGE_REF:-}}"
EXPECTED_COMMIT="${EXPECTED_COMMIT:-}"
SKIP_MANIFEST="${SKIP_MANIFEST:-0}"

# Parse command-line options
while [[ $# -gt 0 ]]; do
  case "$1" in
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
    --expected-commit)
      EXPECTED_COMMIT="$2"
      shift 2
      ;;
    --skip-manifest-check)
      SKIP_MANIFEST=1
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

if [[ "$RESUME_SOAK" -eq 1 ]]; then
  log_info "Resuming soak observation..."
  resume_soak
  exit 0
fi

if [[ -z "$IMAGE_REF" ]]; then
  log_error "Usage: $0 [options] <candidate-gateway-image-digest>"
  exit 1
fi

validate_canonical_env
validate_digest "$IMAGE_REF" "gateway"
validate_data_volume "$DATA_VOLUME_NAME"
validate_secrets
verify_traefik_prerequisites

# Host lock acquisition
acquire_deploy_lock

# Recover from any previous interrupted or corrupt transaction
recover_tx_journal

# Verify promotion scope if signed manifest is present and not skipped
if [[ "$SKIP_MANIFEST" -ne 1 && -f "$SCRIPT_DIR/manifest.json" && -f "$SCRIPT_DIR/verify-manifest.sh" ]]; then
  log_info "Verifying signed release manifest scope for gateway promotion..."
  if ! bash "$SCRIPT_DIR/verify-manifest.sh" --manifest "$SCRIPT_DIR/manifest.json"; then
    log_error "Signed manifest verification failed. Aborting gateway promotion."
    release_deploy_lock
    exit 1
  fi
fi

ACTIVE_SLOT="$(get_active_slot)"
CANDIDATE_SLOT="$(get_candidate_slot "$ACTIVE_SLOT")"
CANDIDATE_VAR="IMAGE_REF_${CANDIDATE_SLOT^^}"
PREV_REF="$(get_release_env "$CANDIDATE_VAR" 2>/dev/null || echo "")"

SNAPSHOT_FILE="${SCRIPT_DIR}/data/core-containers.snapshot.$$"

# Clean-up traps
cleanup() {
  local exit_code="$?"
  rm -f "$SNAPSHOT_FILE" 2>/dev/null || true

  if [[ "$exit_code" -ne 0 ]]; then
    log_warn "Deployment script exiting with error code ${exit_code}."
    local cur_tx
    cur_tx="$(get_tx_state 2>/dev/null || echo "")"
    if ! is_tx_committed && [[ "$cur_tx" != "TX_ROLLED_BACK" && "$cur_tx" != "TX_ROLLBACK_FAILED" ]]; then
      log_warn "Transaction is NOT committed. Executing automatic recovery and cleanup..."
      # If route was switched, revert it
      if [[ -f "$ACB_CONFIG" ]] && grep -q "acb-web-${CANDIDATE_SLOT}" "$ACB_CONFIG" 2>/dev/null; then
        log_warn "Reverting Traefik route pointer back to [${ACTIVE_SLOT}]..."
        rollback_route "$ACTIVE_SLOT" "" 15 || true
      fi
      # Stop candidate container
      log_warn "Stopping candidate container [acb-gateway-${CANDIDATE_SLOT}]..."
      compose_prod stop "gateway-${CANDIDATE_SLOT}" 2>/dev/null || docker compose -f "$COMPOSE_FILE" stop "gateway-${CANDIDATE_SLOT}" 2>/dev/null || true
      update_tx_state "TX_FAILED" "Aborted on error (exit code: ${exit_code})"
      set_deploy_state "FAILED" "Failed before commit"
    fi
  fi
  release_deploy_lock
}
trap cleanup EXIT
trap 'log_warn "Caught SIGINT / SIGTERM"; exit 130' INT TERM

# Snapshot core container IDs
snapshot_core_containers "$SNAPSHOT_FILE"

# Initialize Transaction Journal
init_tx_journal "gateway" "$CANDIDATE_SLOT" "$ACTIVE_SLOT" "$IMAGE_REF" "$PREV_REF" "$EXPECTED_COMMIT"
set_deploy_state "START_CANDIDATE" "Active: ${ACTIVE_SLOT}, Candidate: ${CANDIDATE_SLOT}"

log_info "=========================================================="
log_info "Gateway Promotion: Active [${ACTIVE_SLOT}] -> Candidate [${CANDIDATE_SLOT}]"
log_info "Target Image Ref: ${IMAGE_REF}"
log_info "Mode: GATEWAY-ONLY (Worker, Browser, TTS, Bark strictly preserved)"
log_info "=========================================================="

# 1. Pull candidate image
pull_candidate_image "gateway-${CANDIDATE_SLOT}" "$IMAGE_REF"

# 2. Start candidate container
log_info "Starting candidate slot [gateway-${CANDIDATE_SLOT}]..."
clear_intentional_stop "$CANDIDATE_SLOT"
export "${CANDIDATE_VAR}=${IMAGE_REF}"
if [[ -n "$EXPECTED_COMMIT" && "$EXPECTED_COMMIT" != "unknown" ]]; then
  export RELEASE_COMMIT="$EXPECTED_COMMIT"
fi
compose_prod up -d "gateway-${CANDIDATE_SLOT}" 2>/dev/null || docker compose -f "$COMPOSE_FILE" up -d "gateway-${CANDIDATE_SLOT}"
update_tx_state "TX_CANDIDATE_STARTED"

# 3. Candidate readiness probe (twice consecutively)
set_deploy_state "WAIT_READY"
if ! wait_for_candidate_ready "$CANDIDATE_SLOT" "${READY_TIMEOUT:-60}" "$EXPECTED_COMMIT"; then
  log_error "Candidate slot [${CANDIDATE_SLOT}] failed readiness probe! Aborting cutover."
  log_warn "Stopping candidate container [gateway-${CANDIDATE_SLOT}]. Active slot [${ACTIVE_SLOT}] remains live and untouched."
  compose_prod stop "gateway-${CANDIDATE_SLOT}" 2>/dev/null || docker compose -f "$COMPOSE_FILE" stop "gateway-${CANDIDATE_SLOT}" 2>/dev/null || true
  set_deploy_state "FAILED_CANDIDATE" "Candidate failed readiness check"
  update_tx_state "TX_FAILED" "Readiness probe failed"
  exit 1
fi
update_tx_state "TX_CANDIDATE_READY"

# 4. Atomic Traefik route switch
set_deploy_state "SWITCH_ROUTE"
log_info "Switching Traefik dynamic route to candidate slot [${CANDIDATE_SLOT}]..."
if ! atomic_switch_route "$CANDIDATE_SLOT"; then
  log_error "Failed to switch Traefik dynamic route to [${CANDIDATE_SLOT}]! Aborting cutover."
  compose_prod stop "gateway-${CANDIDATE_SLOT}" 2>/dev/null || true
  exit 1
fi
update_tx_state "TX_ROUTE_SWITCHED"

# 5. Real Edge Route Identity Acknowledgement
set_deploy_state "ACK_ROUTE"
if ! ack_route_identity "$CANDIDATE_SLOT" "$EXPECTED_COMMIT" "${ROUTE_ACK_TIMEOUT:-15}"; then
  log_error "Route identity acknowledgment failed for candidate [${CANDIDATE_SLOT}]!"
  log_warn "Executing automatic route rollback to [${ACTIVE_SLOT}]..."
  if rollback_route "$ACTIVE_SLOT" "" 15; then
    stop_standby_container "$CANDIDATE_SLOT"
    set_deploy_state "ROLLED_BACK" "Route identity ACK failed on candidate ${CANDIDATE_SLOT}; reverted to ${ACTIVE_SLOT}"
    update_tx_state "TX_ROLLED_BACK" "Route ACK failed"
  else
    log_error "CRITICAL: Route rollback to [${ACTIVE_SLOT}] also failed route identity acknowledgment!"
    stop_standby_container "$CANDIDATE_SLOT"
    set_deploy_state "ROLLBACK_FAILED" "Route ACK failed and rollback to ${ACTIVE_SLOT} also failed ACK"
    update_tx_state "TX_ROLLBACK_FAILED" "Rollback to ${ACTIVE_SLOT} failed ACK"
  fi
  exit 1
fi
update_tx_state "TX_ACK_VERIFIED"

# 6. Commit Transaction State
update_tx_state "TX_COMMITTED"
printf '%s' "$CANDIDATE_SLOT" > "$ACTIVE_SLOT_FILE"
printf '%s' "$ACTIVE_SLOT" > "$PREVIOUS_SLOT_FILE"
set_release_env "$CANDIDATE_VAR" "$IMAGE_REF" 2>/dev/null || true
if [[ -n "$EXPECTED_COMMIT" && "$EXPECTED_COMMIT" != "unknown" ]]; then
  set_release_env "RELEASE_COMMIT" "$EXPECTED_COMMIT" 2>/dev/null || true
fi
log_info "Transaction COMMITTED: Live traffic routed to [${CANDIDATE_SLOT}]."

# 7. Audit: Core containers must have exact same IDs
assert_core_containers_unchanged "$SNAPSHOT_FILE"

# 8. 15-minute resumable soak observation
set_deploy_state "SOAK"
update_tx_state "TX_SOAKING"
log_info "Old slot [${ACTIVE_SLOT}] remains running during soak (${SOAK_DURATION_SEC}s) for instant failover."

if [[ "$DETACH_SOAK" -eq 1 || "${SOAK_BACKGROUND:-0}" == "1" ]]; then
  log_info "Detaching soak observation to background process..."
  mkdir -p "$SCRIPT_DIR/data"
  nohup bash -c "source '${SCRIPT_DIR}/lib.sh' && run_resumable_soak '${CANDIDATE_SLOT}' '${ACTIVE_SLOT}' '${SOAK_DURATION_SEC}'" > "$SCRIPT_DIR/data/soak.log" 2>&1 &
  log_info "Soak detached (PID: $!). State recorded in $SOAK_STATE_FILE."
else
  if ! run_resumable_soak "$CANDIDATE_SLOT" "$ACTIVE_SLOT" "$SOAK_DURATION_SEC"; then
    log_error "Soak observation failed! Active route automatically reverted to [${ACTIVE_SLOT}]..."
    set_deploy_state "FAILED_SOAK"
    update_tx_state "TX_FAILED" "Soak observation failure"
    exit 1
  fi
  update_tx_state "TX_COMPLETED" "Promotion and soak completed successfully"
  set_deploy_state "COMPLETED" "Slot ${CANDIDATE_SLOT} is live and soak completed."
fi

log_info "Gateway deployment transaction completed successfully."
exit 0
