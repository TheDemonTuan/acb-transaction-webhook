#!/usr/bin/env bash
# deploy/deploy-auth-browser.sh
# Sandboxed Browser Container Deployment Transaction with Fail-Closed Active-Auth Gate.
# Invariants:
# 1. Active authentication attempt strictly blocks browser redeployment.
# 2. Replaces only auth-browser; core gateway and worker containers remain untouched.
# 3. Rolls back to previous digest if candidate browser fails healthcheck.
# 4. Enforces deployment transaction journal and atomic recovery.
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
recover_tx_journal

PREV_BROWSER_REF="$(get_release_env BROWSER_IMAGE_REF 2>/dev/null || true)"

stop_browser() {
  if command -v docker >/dev/null 2>&1; then
    for c in acb-auth-browser acb-browser; do
      if docker ps -a --format '{{.Names}}' | grep -q "^${c}\$"; then
        docker stop -t 10 "$c" >/dev/null 2>&1 || true
        docker rm "$c" >/dev/null 2>&1 || true
      fi
    done
  fi
}

start_browser() {
  local img="$1"
  if [[ -n "${BROWSER_START_CMD:-}" ]]; then
    $BROWSER_START_CMD "$img"
    return $?
  fi
  if command -v docker >/dev/null 2>&1; then
    stop_browser
    BROWSER_IMAGE_REF="$img" docker compose -f "$SCRIPT_DIR/compose.prod.yaml" up -d --no-deps auth-browser
  fi
}

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
    if command -v docker >/dev/null 2>&1; then
      for c in acb-auth-browser acb-browser; do
        if docker ps --format '{{.Names}}' | grep -q "^${c}\$"; then
          local st
          st="$(docker inspect --format '{{.State.Health.Status}}' "$c" 2>/dev/null || echo "")"
          if [[ "$st" == "healthy" ]]; then
            return 0
          fi
          if docker exec "$c" /auth-browser --healthcheck >/dev/null 2>&1; then
            return 0
          fi
        fi
      done
    fi
    sleep 1
    elapsed=$(( elapsed + 1 ))
  done
  return 1
}

cleanup_browser_deploy() {
  local exit_code=$?
  if [[ -f "$TX_JOURNAL_FILE" ]] && ! is_tx_committed; then
    local st
    st="$(get_tx_state)"
    if [[ "$st" != "TX_ROLLED_BACK" && "$st" != "TX_ROLLBACK_FAILED" && "$st" != "TX_COMPLETED" ]]; then
      log_warn "Interruption caught in state [$st]. Rolling back auth-browser..."
      if [[ -n "${PREV_BROWSER_REF:-}" ]]; then
        update_tx_state "TX_ROLLING_BACK" "Interruption caught, rolling back"
        stop_browser
        start_browser "$PREV_BROWSER_REF" 2>/dev/null || true
        if wait_for_browser_ready 10; then
          update_tx_state "TX_ROLLED_BACK" "Rolled back to previous image on interruption"
        else
          update_tx_state "TX_ROLLBACK_FAILED" "Rollback container also failed healthcheck"
        fi
      fi
    fi
  fi
  release_deploy_lock
  exit "$exit_code"
}
trap cleanup_browser_deploy EXIT HUP INT TERM

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

# 2. Init transaction journal
init_tx_journal "auth-browser" "singleton" "singleton" "$CANDIDATE_BROWSER_IMAGE" "$PREV_BROWSER_REF" "${EXPECTED_COMMIT:-}"
update_tx_state "CANDIDATE_STARTING" "Starting candidate auth-browser container"

# 3. Start candidate container
log_info "Starting candidate auth-browser container..."
if ! start_browser "$CANDIDATE_BROWSER_IMAGE"; then
  log_error "Failed to start candidate auth-browser container."
  update_tx_state "TX_ROLLING_BACK" "Candidate startup failed"
  if [[ -n "${PREV_BROWSER_REF:-}" ]]; then
    start_browser "$PREV_BROWSER_REF" || true
    wait_for_browser_ready 10 || true
    update_tx_state "TX_ROLLED_BACK" "Restored previous container after candidate startup failure"
  fi
  exit 1
fi

# 4. Health check candidate browser
READY_CHECK_TIMEOUT="${READY_CHECK_TIMEOUT:-20}"
update_tx_state "VERIFYING_HEALTH" "Verifying candidate auth-browser health"
if ! wait_for_browser_ready "$READY_CHECK_TIMEOUT"; then
  log_error "Candidate auth-browser failed healthcheck. Rolling back to previous digest..."
  update_tx_state "TX_ROLLING_BACK" "Candidate failed healthcheck; rolling back"
  if [[ -n "${PREV_BROWSER_REF:-}" ]]; then
    stop_browser
    start_browser "$PREV_BROWSER_REF"
    if wait_for_browser_ready "$READY_CHECK_TIMEOUT"; then
      update_tx_state "TX_ROLLED_BACK" "Rolled back successfully to $PREV_BROWSER_REF"
      log_info "Rollback to previous auth-browser digest succeeded."
    else
      update_tx_state "TX_ROLLBACK_FAILED" "Rollback container also failed healthcheck"
      log_error "Rollback failed: previous browser digest also failed healthcheck."
    fi
  else
    update_tx_state "TX_FAILED" "Candidate failed healthcheck and no previous digest available"
  fi
  exit 1
fi

# 5. Commit new browser image ref
update_tx_state "TX_COMMITTED" "Candidate auth-browser verified healthy"
set_release_env "BROWSER_IMAGE_REF" "$CANDIDATE_BROWSER_IMAGE"
log_info "Committed new BROWSER_IMAGE_REF to .release.env."

update_tx_state "TX_COMPLETED" "Auth-browser deployment transaction completed successfully"
archive_tx_journal "completed"

log_info "=========================================================="
log_info "Auth-Browser Deployment Transaction Successfully Completed"
log_info "=========================================================="
exit 0
