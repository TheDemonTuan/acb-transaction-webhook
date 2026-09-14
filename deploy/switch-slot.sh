#!/usr/bin/env bash
# Atomically switch the active Gateway slot in Traefik File Provider
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deploy/lib.sh
source "$SCRIPT_DIR/lib.sh"

TARGET_SLOT="${1:-blue}"

if [[ "$TARGET_SLOT" != "blue" && "$TARGET_SLOT" != "green" ]]; then
  log_error "Invalid target slot '${TARGET_SLOT}'. Must be 'blue' or 'green'."
  exit 1
fi

log_info "Switching Traefik active route pointer to slot: ${TARGET_SLOT} (acb-web-${TARGET_SLOT})..."

# Verify candidate readiness before switching route
"$SCRIPT_DIR/smoke-slot.sh" "$TARGET_SLOT"

# Atomic route switch via shared primitive without nested locking
if [[ "${DEPLOY_LOCK_HELD:-0}" != "1" && "${SKIP_LOCK:-0}" != "1" ]]; then
  acquire_deploy_lock
  trap release_deploy_lock EXIT HUP INT TERM
fi

CURRENT_ACTIVE="$(get_active_slot)"
if [[ "$CURRENT_ACTIVE" != "$TARGET_SLOT" ]]; then
  printf "%s" "$CURRENT_ACTIVE" > "$PREVIOUS_SLOT_FILE"
fi

atomic_switch_route "$TARGET_SLOT"

if ! ack_route_identity "$TARGET_SLOT" 15; then
  log_error "Route identity acknowledgment failed for ${TARGET_SLOT}! Reverting to previous slot ${CURRENT_ACTIVE}..."
  rollback_route "$CURRENT_ACTIVE"
  ack_route_identity "$CURRENT_ACTIVE" 15 || true
  exit 1
fi

log_info "Traefik route pointer successfully updated and acknowledged for ${TARGET_SLOT} (acb-web-${TARGET_SLOT})."
exit 0
