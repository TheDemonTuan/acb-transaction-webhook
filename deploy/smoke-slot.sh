#!/usr/bin/env bash
# Smoke test for an inactive or active Gateway slot
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deploy/lib.sh
source "$SCRIPT_DIR/lib.sh"

SLOT="${1:-blue}"
if [[ "$SLOT" != "blue" && "$SLOT" != "green" ]]; then
  log_error "Invalid slot: '$SLOT'. Must be 'blue' or 'green'."
  exit 1
fi

CONTAINER_NAME="acb-gateway-${SLOT}"
log_info "Running smoke test on slot: ${SLOT} (container: ${CONTAINER_NAME})..."

# 1. Verify container is running
STATUS=$(docker inspect --format '{{.State.Status}}' "$CONTAINER_NAME" 2>/dev/null || echo "not_found")
if [[ "$STATUS" != "running" ]]; then
  log_error "Container ${CONTAINER_NAME} is not running (status: ${STATUS})"
  exit 1
fi

# 2. Probe health probe
if docker exec "$CONTAINER_NAME" /gateway --healthcheck >/dev/null 2>&1; then
  log_info "Slot ${SLOT} passed health check probe."
else
  log_error "Slot ${SLOT} failed health check probe"
  exit 1
fi

log_info "Smoke test for slot ${SLOT} PASSED."
exit 0
