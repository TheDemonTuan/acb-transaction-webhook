#!/usr/bin/env bash
# Blue/green frontend deployment. All API and worker containers remain untouched.
set -Eeuo pipefail
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deploy/lib.sh
source "$SCRIPT_DIR/lib.sh"

CANDIDATE_FRONTEND_IMAGE="${1:-${FRONTEND_IMAGE_REF:-}}"
[[ -n "$CANDIDATE_FRONTEND_IMAGE" ]] || { log_error "Usage: $0 <candidate-frontend-image-digest>"; exit 1; }
validate_canonical_env
validate_digest "$CANDIDATE_FRONTEND_IMAGE" "frontend"
acquire_deploy_lock
recover_tx_journal

ACTIVE_FRONTEND_SLOT="legacy"
if [[ -f "$FRONTEND_ACTIVE_SLOT_FILE" ]]; then
  ACTIVE_FRONTEND_SLOT="$(tr -d '[:space:]' < "$FRONTEND_ACTIVE_SLOT_FILE")"
fi
case "$ACTIVE_FRONTEND_SLOT" in
  blue) CANDIDATE_FRONTEND_SLOT="green" ;;
  green) CANDIDATE_FRONTEND_SLOT="blue" ;;
  *) ACTIVE_FRONTEND_SLOT="legacy"; CANDIDATE_FRONTEND_SLOT="blue" ;;
esac
CANDIDATE_SERVICE="frontend-${CANDIDATE_FRONTEND_SLOT}"
CANDIDATE_CONTAINER="acb-frontend-${CANDIDATE_FRONTEND_SLOT}"
if [[ "$ACTIVE_FRONTEND_SLOT" == "legacy" ]]; then
  ACTIVE_CONTAINER="acb-frontend"
else
  ACTIVE_CONTAINER="acb-frontend-${ACTIVE_FRONTEND_SLOT}"
fi

PREV_FRONTEND_REF="$(docker inspect --format '{{.Config.Image}}' "$ACTIVE_CONTAINER" 2>/dev/null || true)"
if [[ -z "$PREV_FRONTEND_REF" ]]; then
  PREV_FRONTEND_REF="$(get_release_env FRONTEND_IMAGE_REF 2>/dev/null || true)"
fi
validate_digest "$PREV_FRONTEND_REF" "previous frontend"

snapshot_file="$(mktemp)"
for container in acb-worker acb-gateway-blue acb-gateway-green acb-auth-browser acb-tts-gateway acb-bark; do
  printf '%s=%s\n' "$container" "$(docker inspect --format '{{.Id}}' "$container" 2>/dev/null || printf missing)" >> "$snapshot_file"
done

FRONTEND_SWITCHED=0
FRONTEND_COMMITTED=0
init_tx_journal "frontend" "$CANDIDATE_FRONTEND_SLOT" "$ACTIVE_FRONTEND_SLOT" "$CANDIDATE_FRONTEND_IMAGE" "$PREV_FRONTEND_REF" "${RELEASE_COMMIT:-unknown}"
cleanup_frontend_deploy() {
  local code=$?
  if [[ "$code" -ne 0 && "$FRONTEND_COMMITTED" -eq 0 ]]; then
    if [[ "$FRONTEND_SWITCHED" -eq 1 ]]; then
      old_frontend="$ACTIVE_FRONTEND_SLOT"
      gateway_slot="$(get_active_slot 2>/dev/null || true)"
      if [[ "$gateway_slot" == "blue" || "$gateway_slot" == "green" ]]; then
        tmp_route="${ACB_CONFIG}.rollback.$$"
        render_traefik_config "$gateway_slot" "$tmp_route" "$old_frontend" && mv -f "$tmp_route" "$ACB_CONFIG" || true
      fi
      if [[ "$ACTIVE_FRONTEND_SLOT" == "legacy" ]]; then
        rm -f "$FRONTEND_ACTIVE_SLOT_FILE"
      else
        printf '%s' "$ACTIVE_FRONTEND_SLOT" | atomic_write_file "$FRONTEND_ACTIVE_SLOT_FILE" 600
      fi
    fi
    docker rm -f "$CANDIDATE_CONTAINER" >/dev/null 2>&1 || true
  fi
  rm -f "$snapshot_file"
  release_deploy_lock
  exit "$code"
}
trap cleanup_frontend_deploy EXIT HUP INT TERM

wait_for_frontend_ready() {
  local timeout="${FRONTEND_READY_TIMEOUT:-30}"
  local elapsed=0
  while [[ "$elapsed" -lt "$timeout" ]]; do
    if docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$CANDIDATE_CONTAINER" 2>/dev/null | grep -qx healthy; then
      return 0
    fi
    sleep 1
    elapsed=$((elapsed + 1))
  done
  return 1
}

FRONTEND_IMAGE_REF="$CANDIDATE_FRONTEND_IMAGE" compose_prod pull "$CANDIDATE_SERVICE"
FRONTEND_IMAGE_REF="$CANDIDATE_FRONTEND_IMAGE" compose_prod up -d --no-deps "$CANDIDATE_SERVICE"
update_tx_state "TX_CANDIDATE_STARTED"
wait_for_frontend_ready || { log_error "Frontend candidate failed readiness."; exit 1; }
update_tx_state "TX_CANDIDATE_READY"

printf '%s' "$ACTIVE_FRONTEND_SLOT" | atomic_write_file "$FRONTEND_PREVIOUS_SLOT_FILE" 600
atomic_switch_frontend_route "$CANDIDATE_FRONTEND_SLOT"
FRONTEND_SWITCHED=1
ack_frontend_route "${FRONTEND_ROUTE_ACK_TIMEOUT:-20}" || { log_error "Frontend route acknowledgement failed."; exit 1; }
update_tx_state "TX_ROUTE_ACKED"

soak="${FRONTEND_SOAK_SECONDS:-30}"
[[ "$soak" =~ ^[0-9]+$ ]] || { log_error "FRONTEND_SOAK_SECONDS must be numeric."; exit 1; }
while [[ "$soak" -gt 0 ]]; do
  wait_for_frontend_ready || { log_error "Frontend failed during soak."; exit 1; }
  ack_frontend_route "${FRONTEND_ROUTE_ACK_TIMEOUT:-20}" || { log_error "Frontend route failed during soak."; exit 1; }
  sleep_for=$(( soak > 5 ? 5 : soak ))
  sleep "$sleep_for"
  soak=$((soak - sleep_for))
done

while IFS='=' read -r container expected; do
  actual="$(docker inspect --format '{{.Id}}' "$container" 2>/dev/null || printf missing)"
  [[ "$actual" == "$expected" ]] || { log_error "Out-of-scope container changed: ${container}"; exit 1; }
done < "$snapshot_file"

if [[ "$ACTIVE_CONTAINER" != "$CANDIDATE_CONTAINER" ]]; then
  docker stop --time "${FRONTEND_STOP_TIMEOUT:-10}" "$ACTIVE_CONTAINER" >/dev/null 2>&1 || true
fi
commit_component_release_env FRONTEND_IMAGE_REF "$CANDIDATE_FRONTEND_IMAGE"
update_tx_state "TX_COMPLETED"
archive_tx_journal "completed"
FRONTEND_COMMITTED=1
log_info "Frontend slot ${CANDIDATE_FRONTEND_SLOT} promoted; unrelated containers were unchanged."
