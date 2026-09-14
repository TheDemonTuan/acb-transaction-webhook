#!/usr/bin/env bash
# Rollback to the previous Gateway slot using unified compose.prod.yaml
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deploy/lib.sh
source "$SCRIPT_DIR/lib.sh"

acquire_deploy_lock
trap release_deploy_lock EXIT HUP INT TERM

CURRENT_SLOT="$(get_active_slot)"
PREVIOUS_SLOT=""
if [[ -f "$PREVIOUS_SLOT_FILE" ]]; then
  PREVIOUS_SLOT="$(tr -d ' \r\n[:space:]' < "$PREVIOUS_SLOT_FILE")"
fi

if [[ -z "$PREVIOUS_SLOT" || "$PREVIOUS_SLOT" == "$CURRENT_SLOT" ]]; then
  PREVIOUS_SLOT="$(get_candidate_slot "$CURRENT_SLOT")"
fi

log_info "========================================"
log_info "ROLLBACK: Reverting from [${CURRENT_SLOT}] back to [${PREVIOUS_SLOT}]"
log_info "========================================"

# 1. Start previous slot container if not running
clear_intentional_stop "$PREVIOUS_SLOT"
STATUS=$(docker inspect --format '{{.State.Status}}' "acb-gateway-${PREVIOUS_SLOT}" 2>/dev/null || echo "not_found")
if [[ "$STATUS" != "running" ]]; then
  log_info "Starting stopped previous container acb-gateway-${PREVIOUS_SLOT}..."
  docker compose -f "$COMPOSE_FILE" start "gateway-${PREVIOUS_SLOT}" 2>/dev/null || docker start "acb-gateway-${PREVIOUS_SLOT}"
fi

# 2. Probe health
wait_for_candidate_ready "$PREVIOUS_SLOT" 30

# 3. Switch Traefik pointer back
rollback_route "$PREVIOUS_SLOT"

# 4. Acknowledge route identity
if ! ack_route_identity "$PREVIOUS_SLOT" 15; then
  log_error "CRITICAL: Route identity acknowledgment failed during rollback to [${PREVIOUS_SLOT}]!"
  exit 1
fi

# 5. Stop faulty current slot into warm standby
stop_standby_container "$CURRENT_SLOT"

printf "%s" "$PREVIOUS_SLOT" > "$ACTIVE_SLOT_FILE"
printf "%s" "$CURRENT_SLOT" > "$PREVIOUS_SLOT_FILE"
set_deploy_state "ROLLED_BACK" "Reverted from ${CURRENT_SLOT} to ${PREVIOUS_SLOT}"

log_info "Rollback completed! Traefik is now routing traffic back to slot [${PREVIOUS_SLOT}]."
exit 0
