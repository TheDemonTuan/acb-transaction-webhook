#!/usr/bin/env bash
# Rollback to the previous Gateway slot using unified compose.prod.yaml
set -euo pipefail

COMPOSE_FILE="${COMPOSE_FILE:-compose.prod.yaml}"
ACTIVE_SLOT_FILE="${ACTIVE_SLOT_FILE:-.active-slot}"
PREVIOUS_SLOT_FILE="${PREVIOUS_SLOT_FILE:-.previous-slot}"

if [[ ! -f "$PREVIOUS_SLOT_FILE" ]]; then
  printf "ERROR: No previous slot recorded in %s\n" "$PREVIOUS_SLOT_FILE" >&2
  exit 1
fi

PREVIOUS_SLOT=$(cat "$PREVIOUS_SLOT_FILE" | tr -d '[:space:]')
CURRENT_SLOT="blue"
if [[ -f "$ACTIVE_SLOT_FILE" ]]; then
  CURRENT_SLOT=$(cat "$ACTIVE_SLOT_FILE" | tr -d '[:space:]')
fi

printf "========================================\n"
printf "ROLLBACK: Reverting from [%s] back to [%s]\n" "$CURRENT_SLOT" "$PREVIOUS_SLOT"
printf "========================================\n"

# 1. Start previous slot if stopped
STATUS=$(docker inspect --format '{{.State.Status}}' "acb-gateway-${PREVIOUS_SLOT}" 2>/dev/null || echo "not_found")
if [[ "$STATUS" != "running" ]]; then
  printf "Starting stopped previous container acb-gateway-%s...\n" "$PREVIOUS_SLOT"
  docker compose -f "$COMPOSE_FILE" start "gateway-${PREVIOUS_SLOT}"
  sleep 3
fi

# 2. Probe health
bash deploy/smoke-slot.sh "$PREVIOUS_SLOT"

# 3. Switch Traefik pointer back
bash deploy/switch-slot.sh "$PREVIOUS_SLOT"

printf "%s" "$PREVIOUS_SLOT" > "$ACTIVE_SLOT_FILE"
printf "%s" "$CURRENT_SLOT" > "$PREVIOUS_SLOT_FILE"

printf "Rollback completed! Traefik is now routing traffic back to slot [%s].\n" "$PREVIOUS_SLOT"
