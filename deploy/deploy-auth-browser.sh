#!/usr/bin/env bash
# deploy/deploy-auth-browser.sh
# Sandboxed Browser Container Deployment Transaction with Fail-Closed Active-Auth Gate.
# Invariants:
# 1. Active authentication attempt strictly blocks browser redeployment.
# 2. Replaces only auth-browser; core gateway and worker containers remain untouched.
# 3. Rolls back to previous digest if candidate browser fails healthcheck.
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deploy/lib.sh
source "$SCRIPT_DIR/lib.sh"

CANDIDATE_BROWSER_IMAGE="${1:-${BROWSER_IMAGE_REF:-}}"

if [[ -z "$CANDIDATE_BROWSER_IMAGE" ]]; then
  CANDIDATE_BROWSER_IMAGE="$(get_release_env BROWSER_IMAGE_REF 2>/dev/null || true)"
fi

if [[ -z "$CANDIDATE_BROWSER_IMAGE" ]]; then
  log_error "Usage: $0 <candidate-browser-image-digest>"
  exit 1
fi

validate_canonical_env
validate_digest "$CANDIDATE_BROWSER_IMAGE" "auth-browser"
validate_secrets

acquire_deploy_lock
trap 'release_deploy_lock' EXIT

PREV_BROWSER_REF="$(get_release_env BROWSER_IMAGE_REF 2>/dev/null || true)"

log_info "=========================================================="
log_info "Starting Auth-Browser Deployment Transaction"
log_info "Candidate Browser Image: ${CANDIDATE_BROWSER_IMAGE}"
log_info "Previous Browser Image:  ${PREV_BROWSER_REF:-none}"
log_info "=========================================================="

# 1. Fail-closed active-auth check
if ! check_active_auth_gate; then
  log_error "Auth-browser deployment aborted: active customer authentication attempt in progress or check failed."
  exit 1
fi

# 2. Stop and recreate auth-browser container
log_info "Stopping current auth-browser container..."
if command -v docker >/dev/null 2>&1 && docker ps -a --format '{{.Names}}' | grep -q '^acb-browser$'; then
  docker stop -t 10 acb-browser >/dev/null 2>&1 || true
  docker rm acb-browser >/dev/null 2>&1 || true
fi

start_browser() {
  local img="$1"
  if [[ -n "${BROWSER_START_CMD:-}" ]]; then
    $BROWSER_START_CMD "$img"
    return $?
  fi
  if command -v docker >/dev/null 2>&1; then
    BROWSER_IMAGE_REF="$img" docker compose -f "$SCRIPT_DIR/compose.prod.yaml" up -d --no-deps auth-browser
  fi
}

if ! start_browser "$CANDIDATE_BROWSER_IMAGE"; then
  log_error "Failed to start candidate auth-browser container."
  exit 1
fi

# 3. Health check candidate browser
wait_for_browser_ready() {
  local timeout="${1:-20}"
  local elapsed=0
  if [[ -n "${BROWSER_READY_CHECK_CMD:-}" ]]; then
    while [[ "$elapsed" -lt "$timeout" ]]; do
      if $BROWSER_READY_CHECK_CMD >/dev/null 2>&1; then
        return 0
      fi
      sleep 1
      elapsed=$(( elapsed + 1 ))
    done
    return 1
  fi

  while [[ "$elapsed" -lt "$timeout" ]]; do
    if command -v docker >/dev/null 2>&1 && docker ps --format '{{.Names}}' | grep -q '^acb-browser$'; then
      if docker exec acb-browser /auth-browser --healthcheck >/dev/null 2>&1; then
        return 0
      fi
    fi
    sleep 1
    elapsed=$(( elapsed + 1 ))
  done
  return 1
}

if ! wait_for_browser_ready 20; then
  log_error "Candidate auth-browser failed healthcheck. Rolling back to previous digest..."
  if [[ -n "${PREV_BROWSER_REF:-}" ]]; then
    docker stop -t 5 acb-browser >/dev/null 2>&1 || true
    docker rm acb-browser >/dev/null 2>&1 || true
    start_browser "$PREV_BROWSER_REF"
    wait_for_browser_ready 20 || true
  fi
  exit 1
fi

# 4. Commit new browser image ref
set_release_env "BROWSER_IMAGE_REF" "$CANDIDATE_BROWSER_IMAGE"
log_info "Committed new BROWSER_IMAGE_REF to .release.env."

log_info "=========================================================="
log_info "Auth-Browser Deployment Transaction Successfully Completed."
log_info "=========================================================="
exit 0
