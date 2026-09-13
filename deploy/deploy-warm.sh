#!/usr/bin/env bash
# Deploy Gateway using Warm Standby Blue/Green with Traefik 3.x
set -euo pipefail

IMAGE_REF="${1:-${GATEWAY_IMAGE_REF:-}}"
if [[ -z "$IMAGE_REF" ]]; then
  printf "Usage: %s <gateway-image-digest>\n" "$0" >&2
  exit 1
fi

ACTIVE_SLOT_FILE="${ACTIVE_SLOT_FILE:-.active-slot}"
PREVIOUS_SLOT_FILE="${PREVIOUS_SLOT_FILE:-.previous-slot}"
ACTIVE_SLOT="blue"
if [[ -f "$ACTIVE_SLOT_FILE" ]]; then
  ACTIVE_SLOT=$(cat "$ACTIVE_SLOT_FILE" | tr -d '[:space:]')
fi

if [[ "$ACTIVE_SLOT" == "blue" ]]; then
  CANDIDATE_SLOT="green"
else
  CANDIDATE_SLOT="blue"
fi

printf "========================================\n"
printf "Deploying Gateway: Active is [%s] -> Candidate is [%s]\n" "$ACTIVE_SLOT" "$CANDIDATE_SLOT"
printf "Target Image: %s\n" "$IMAGE_REF"
printf "========================================\n"

# 1. Ensure required networks exist
for net in edge-acb acb-core acb-egress; do
  if ! docker network inspect "$net" >/dev/null 2>&1; then
    docker network create "$net"
  fi
done

# 2. Start core services if not already running
printf "Ensuring core singleton services (worker, auth-browser, tts, bark) are running...\n"
docker compose -f deploy/compose.core.yaml up -d

# 3. Start candidate slot
printf "Starting candidate slot [%s] with new image...\n" "$CANDIDATE_SLOT"
export IMAGE_REF
export SLOT="$CANDIDATE_SLOT"
docker compose -p "acb-${CANDIDATE_SLOT}" -f deploy/compose.slot.yaml up -d

# 4. Wait for candidate readiness probe
printf "Waiting for candidate slot [%s] to become healthy...\n" "$CANDIDATE_SLOT"
sleep 3
bash deploy/smoke-slot.sh "$CANDIDATE_SLOT"

# 5. Atomic switch on Traefik
printf "Switching Traefik traffic pointer to candidate [%s]...\n" "$CANDIDATE_SLOT"
bash deploy/switch-slot.sh "$CANDIDATE_SLOT"
printf "%s" "$ACTIVE_SLOT" > "$PREVIOUS_SLOT_FILE"

printf "Cutover successful! Traefik is now routing live traffic to slot [%s].\n" "$CANDIDATE_SLOT"
printf "Old slot [%s] remains active during soak period (15 minutes) for instant rollback.\n" "$ACTIVE_SLOT"
printf "To stop old slot manually after soak: docker compose -p acb-%s -f deploy/compose.slot.yaml stop\n" "$ACTIVE_SLOT"
