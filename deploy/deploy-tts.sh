#!/usr/bin/env bash
# deploy/deploy-tts.sh
# Auxiliary TTS Service Deployment Transaction with Fail-Safe Rollback and Journaling.
# Invariants:
# 1. Replaces only TTS container; worker, gateway, browser, Bark containers remain completely untouched.
# 2. Rolls back to previous digest if candidate TTS fails healthcheck.
# 3. Enforces deployment transaction journal and atomic recovery.
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deploy/lib.sh
source "$SCRIPT_DIR/lib.sh"
require_release_orchestrator

CANDIDATE_TTS_IMAGE="${1:-${TTS_IMAGE_REF:-}}"

if [[ -z "$CANDIDATE_TTS_IMAGE" ]]; then
  CANDIDATE_TTS_IMAGE="$(get_release_env TTS_IMAGE_REF 2>/dev/null || true)"
fi

if [[ -z "$CANDIDATE_TTS_IMAGE" ]]; then
  log_error "Usage: $0 <candidate-tts-image-digest>"
  exit 1
fi

validate_canonical_env
validate_digest "$CANDIDATE_TTS_IMAGE" "tts-gateway"
validate_secrets

acquire_deploy_lock
recover_tx_journal

PREV_TTS_REF="$(get_release_env TTS_IMAGE_REF 2>/dev/null || true)"

stop_tts() {
  if command -v docker >/dev/null 2>&1; then
    for c in acb-tts-gateway tts-gateway; do
      if docker ps -a --format '{{.Names}}' | grep -q "^${c}\$"; then
        docker stop -t 10 "$c" >/dev/null 2>&1 || true
        docker rm "$c" >/dev/null 2>&1 || true
      fi
    done
  fi
}

start_tts() {
  local img="$1"
  if [[ -n "${TTS_START_CMD:-}" ]]; then
    $TTS_START_CMD "$img"
    return $?
  fi
  if command -v docker >/dev/null 2>&1; then
    stop_tts
    TTS_IMAGE_REF="$img" compose_prod up -d --no-deps tts-gateway
  fi
}

wait_for_tts_ready() {
  local timeout="${1:-20}"
  local elapsed=0
  if [[ -n "${TTS_READY_CHECK_CMD:-}" ]]; then
    while [[ "$elapsed" -lt "$timeout" ]]; do
      if $TTS_READY_CHECK_CMD >/dev/null 2>&1; then
        return 0
      fi
      sleep 1
      elapsed=$(( elapsed + 1 ))
    done
    return 1
  fi

  while [[ "$elapsed" -lt "$timeout" ]]; do
    if command -v docker >/dev/null 2>&1; then
      for c in acb-tts-gateway tts-gateway; do
        if docker ps --format '{{.Names}}' | grep -q "^${c}\$"; then
          local st
          st="$(docker inspect --format '{{.State.Health.Status}}' "$c" 2>/dev/null || echo "")"
          if [[ "$st" == "healthy" ]]; then
            return 0
          fi
          if docker exec "$c" python3 -c "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8081/health')" >/dev/null 2>&1 || \
             docker exec "$c" python3 -c "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8095/healthz')" >/dev/null 2>&1; then
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

cleanup_tts_deploy() {
  local exit_code=$?
  if [[ -f "$TX_JOURNAL_FILE" ]] && ! is_tx_committed; then
    local st
    st="$(get_tx_state)"
    if [[ "$st" != "TX_ROLLED_BACK" && "$st" != "TX_ROLLBACK_FAILED" && "$st" != "TX_COMPLETED" ]]; then
      log_warn "Interruption caught in state [$st]. Rolling back TTS..."
      if [[ -n "${PREV_TTS_REF:-}" ]]; then
        update_tx_state "TX_ROLLING_BACK" "Interruption caught, rolling back"
        stop_tts
        start_tts "$PREV_TTS_REF" 2>/dev/null || true
        if wait_for_tts_ready 10; then
          update_tx_state "TX_ROLLED_BACK" "Rolled back to previous TTS image on interruption"
        else
          update_tx_state "TX_ROLLBACK_FAILED" "Rollback TTS container also failed on interruption"
        fi
      fi
    fi
  fi
  release_deploy_lock
  exit "$exit_code"
}
trap cleanup_tts_deploy EXIT HUP INT TERM

log_info "=========================================================="
log_info "Starting TTS Auxiliary Deployment Transaction"
log_info "Candidate TTS Image: ${CANDIDATE_TTS_IMAGE}"
log_info "Previous TTS Image:  ${PREV_TTS_REF:-none}"
log_info "=========================================================="

# 1. Init transaction journal
init_tx_journal "tts" "singleton" "singleton" "$CANDIDATE_TTS_IMAGE" "$PREV_TTS_REF" "${EXPECTED_COMMIT:-}"
update_tx_state "CANDIDATE_STARTING" "Starting candidate TTS container"

# 2. Start candidate container
log_info "Starting candidate TTS container..."
if ! start_tts "$CANDIDATE_TTS_IMAGE"; then
  log_error "Failed to start candidate TTS container."
  update_tx_state "TX_ROLLING_BACK" "Candidate startup failed"
  if [[ -n "${PREV_TTS_REF:-}" ]]; then
    start_tts "$PREV_TTS_REF" || true
    wait_for_tts_ready 10 || true
    update_tx_state "TX_ROLLED_BACK" "Restored previous TTS container after candidate startup failure"
  fi
  exit 1
fi

# 3. Health check candidate TTS
READY_CHECK_TIMEOUT="${READY_CHECK_TIMEOUT:-20}"
update_tx_state "VERIFYING_HEALTH" "Verifying candidate TTS health"
if ! wait_for_tts_ready "$READY_CHECK_TIMEOUT"; then
  log_error "Candidate TTS failed healthcheck. Rolling back to previous digest..."
  update_tx_state "TX_ROLLING_BACK" "Candidate failed healthcheck; rolling back"
  if [[ -n "${PREV_TTS_REF:-}" ]]; then
    stop_tts
    start_tts "$PREV_TTS_REF"
    if wait_for_tts_ready "$READY_CHECK_TIMEOUT"; then
      update_tx_state "TX_ROLLED_BACK" "Rolled back successfully to $PREV_TTS_REF"
      log_info "Rollback to previous TTS digest succeeded."
    else
      update_tx_state "TX_ROLLBACK_FAILED" "Rollback container also failed healthcheck"
      log_error "Rollback failed: previous TTS digest also failed healthcheck."
    fi
  else
    update_tx_state "TX_FAILED" "Candidate failed healthcheck and no previous digest available"
  fi
  exit 1
fi

# 4. Record component success; the dispatcher owns release-state commit.
update_tx_state "TX_COMMITTED" "Candidate TTS verified healthy"

update_tx_state "TX_COMPLETED" "TTS auxiliary deployment transaction completed successfully"
archive_tx_journal "completed"

log_info "=========================================================="
log_info "TTS Auxiliary Deployment Transaction Successfully Completed"
log_info "=========================================================="
exit 0
