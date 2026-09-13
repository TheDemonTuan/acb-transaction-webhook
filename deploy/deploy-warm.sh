#!/usr/bin/env bash
# Deploy Gateway using Warm Standby Blue/Green with unified compose.prod.yaml
set -euo pipefail

IMAGE_REF="${1:-${GATEWAY_IMAGE_REF:-}}"
if [[ -z "$IMAGE_REF" ]]; then
  printf "Usage: %s <gateway-image-digest>\n" "$0" >&2
  exit 1
fi

COMPOSE_FILE="${COMPOSE_FILE:-compose.prod.yaml}"
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

# 1. Ensure core singleton services are running
docker compose -f "$COMPOSE_FILE" up -d worker auth-browser tts-gateway bark

# 2. Start candidate slot with new image
printf "Starting candidate slot [gateway-%s]...\n" "$CANDIDATE_SLOT"
if [[ "$CANDIDATE_SLOT" == "green" ]]; then
  IMAGE_REF_GREEN="$IMAGE_REF" docker compose -f "$COMPOSE_FILE" up -d gateway-green
else
  IMAGE_REF_BLUE="$IMAGE_REF" docker compose -f "$COMPOSE_FILE" up -d gateway-blue
fi

# 3. Wait for candidate readiness probe
printf "Waiting for candidate slot [%s] to become healthy...\n" "$CANDIDATE_SLOT"
sleep 3
bash deploy/smoke-slot.sh "$CANDIDATE_SLOT"

# 4. Atomic switch on Traefik
printf "Switching Traefik traffic pointer to candidate [%s]...\n" "$CANDIDATE_SLOT"
bash deploy/switch-slot.sh "$CANDIDATE_SLOT"
printf "%s" "$ACTIVE_SLOT" > "$PREVIOUS_SLOT_FILE"

printf "Cutover successful! Traefik is now routing live traffic to slot [%s].\n" "$CANDIDATE_SLOT"
printf "Old slot [%s] remains active during soak period (15 minutes) for instant rollback.\n" "$ACTIVE_SLOT"
printf "To stop old slot after soak: docker compose -f %s stop gateway-%s\n" "$COMPOSE_FILE" "$ACTIVE_SLOT"
