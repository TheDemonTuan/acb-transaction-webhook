#!/usr/bin/env bash
# Host Watchdog for Warm Standby Automatic Failover (<= 90s target)
set -euo pipefail

DEPLOY_DIR="${DEPLOY_DIR:-/opt/bank-event-gateway}"
cd "$DEPLOY_DIR"

ACTIVE_SLOT_FILE="${DEPLOY_DIR}/.active-slot"
FAIL_COUNT_FILE="/tmp/.acb-watchdog-fails"
COOLDOWN_FILE="/tmp/.acb-watchdog-cooldown"

# Check cooldown (10 minutes after a failover)
if [[ -f "$COOLDOWN_FILE" ]]; then
  COOLDOWN_AGE=$(( $(date +%s) - $(stat -c %Y "$COOLDOWN_FILE" 2>/dev/null || echo 0) ))
  if [[ $COOLDOWN_AGE -lt 600 ]]; then
    exit 0
  fi
  rm -f "$COOLDOWN_FILE"
fi

ACTIVE_SLOT="blue"
if [[ -f "$ACTIVE_SLOT_FILE" ]]; then
  ACTIVE_SLOT=$(cat "$ACTIVE_SLOT_FILE" | tr -d '[:space:]')
fi

PRIMARY_CONTAINER="acb-gateway-${ACTIVE_SLOT}"

# 1. Probe primary health
if docker exec "$PRIMARY_CONTAINER" /gateway --healthcheck >/dev/null 2>&1; then
  # Healthy -> reset fail counter
  rm -f "$FAIL_COUNT_FILE"
  exit 0
fi

# 2. Increment failure counter
FAILS=1
if [[ -f "$FAIL_COUNT_FILE" ]]; then
  FAILS=$(( $(cat "$FAIL_COUNT_FILE") + 1 ))
fi
printf "%d" "$FAILS" > "$FAIL_COUNT_FILE"

printf "WARNING: Primary slot %s failed health probe (failure %d/3)\n" "$ACTIVE_SLOT" "$FAILS" >&2

if [[ $FAILS -lt 3 ]]; then
  exit 0
fi

# 3. Failover Triggered!
printf "CRITICAL: Primary slot %s failed 3 consecutive health checks. Initiating recovery...\n" "$ACTIVE_SLOT" >&2

# First: attempt bounded docker restart of primary (20s budget)
printf "Attempting emergency restart of primary %s...\n" "$PRIMARY_CONTAINER"
docker restart -t 5 "$PRIMARY_CONTAINER" >/dev/null 2>&1 || true
sleep 5

if docker exec "$PRIMARY_CONTAINER" /gateway --healthcheck >/dev/null 2>&1; then
  printf "Primary %s recovered after restart.\n" "$PRIMARY_CONTAINER"
  rm -f "$FAIL_COUNT_FILE"
  exit 0
fi

# Primary still failing -> activate Warm Standby!
STANDBY_SLOT="green"
if [[ "$ACTIVE_SLOT" == "green" ]]; then
  STANDBY_SLOT="blue"
fi

STANDBY_CONTAINER="acb-gateway-${STANDBY_SLOT}"
printf "Starting warm standby container %s...\n" "$STANDBY_CONTAINER"
docker compose -p "acb-${STANDBY_SLOT}" -f deploy/compose.slot.yaml start || \
  docker compose -p "acb-${STANDBY_SLOT}" -f deploy/compose.slot.yaml up -d

# Wait for standby readiness (up to 30s)
READY=0
for i in {1..15}; do
  if docker exec "$STANDBY_CONTAINER" /gateway --healthcheck >/dev/null 2>&1; then
    READY=1
    break
  fi
  sleep 2
done

if [[ $READY -eq 1 ]]; then
  printf "Warm standby %s is READY. Switching Traefik traffic pointer...\n" "$STANDBY_SLOT"
  bash deploy/switch-slot.sh "$STANDBY_SLOT"
  touch "$COOLDOWN_FILE"
  rm -f "$FAIL_COUNT_FILE"
  printf "SUCCESS: Automated failover to %s completed.\n" "$STANDBY_SLOT"
else
  printf "ERROR: Warm standby %s failed to become healthy. Manual operator intervention required!\n" "$STANDBY_SLOT" >&2
  touch "$COOLDOWN_FILE"
  exit 1
fi
