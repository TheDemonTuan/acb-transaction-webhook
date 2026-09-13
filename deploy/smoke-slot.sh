#!/usr/bin/env bash
# Smoke test for an inactive or active Gateway slot
set -euo pipefail

SLOT="${1:-blue}"
CONTAINER_NAME="acb-gateway-${SLOT}"

printf "Running smoke test on slot: %s (container: %s)...\n" "$SLOT" "$CONTAINER_NAME"

# 1. Verify container is running
STATUS=$(docker inspect --format '{{.State.Status}}' "$CONTAINER_NAME" 2>/dev/null || echo "not_found")
if [[ "$STATUS" != "running" ]]; then
  printf "ERROR: Container %s is not running (status: %s)\n" "$CONTAINER_NAME" "$STATUS" >&2
  exit 1
fi

# 2. Probe /readyz via container internal CLI
if docker exec "$CONTAINER_NAME" /gateway --healthcheck; then
  printf "Slot %s passed health check probe.\n" "$SLOT"
else
  printf "ERROR: Slot %s failed health check probe\n" "$SLOT" >&2
  exit 1
fi

printf "Smoke test for slot %s PASSED.\n" "$SLOT"
