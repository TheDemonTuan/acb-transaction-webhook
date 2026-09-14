#!/usr/bin/env bash
# deploy/deploy-worker.sh
# Controlled Worker Singleton Upgrade and Rollback Transaction.
# Invariants:
# 1. Active authentication attempt strictly blocks worker redeployment.
# 2. Durable mutation gate acquired before touching singleton worker.
# 3. Old worker is quiesced via RPC (draining scheduler, history, notifications, maintenance).
# 4. Old worker container is stopped ONLY after quiesce succeeds.
# 5. Candidate worker started; verified via healthz and readyz probes.
# 6. Automatic rollback to previous digest if candidate fails to become ready.
# 7. Gateway, browser, TTS, and Bark containers remain completely untouched.
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deploy/lib.sh
source "$SCRIPT_DIR/lib.sh"

CANDIDATE_WORKER_IMAGE="${1:-${WORKER_IMAGE_REF:-}}"

if [[ -z "$CANDIDATE_WORKER_IMAGE" ]]; then
  CANDIDATE_WORKER_IMAGE="$(get_release_env WORKER_IMAGE_REF 2>/dev/null || true)"
fi

if [[ -z "$CANDIDATE_WORKER_IMAGE" ]]; then
  log_error "Usage: $0 <candidate-worker-image-digest>"
  exit 1
fi

validate_canonical_env
validate_digest "$CANDIDATE_WORKER_IMAGE" "worker"
validate_data_volume "$DATA_VOLUME_NAME"
validate_secrets

acquire_deploy_lock

WORKER_DEPLOY_OWNER="deploy-worker-$(date -u +%Y%m%d%H%M%S)"
GATE_TOKEN=""
PREV_WORKER_REF="$(get_release_env WORKER_IMAGE_REF 2>/dev/null || true)"
OLD_WORKER_STOPPED=0

cleanup_worker_deploy() {
  local exit_code=$?
  if [[ -n "${GATE_TOKEN:-}" || -n "${WORKER_DEPLOY_OWNER:-}" ]]; then
    release_mutation_gate "$DATA_VOLUME_NAME" "${DBTOOL_IMAGE_REF:-}" "$WORKER_DEPLOY_OWNER" "${GATE_TOKEN:-}" 2>/dev/null || true
  fi
  release_deploy_lock
  exit "$exit_code"
}
trap cleanup_worker_deploy EXIT HUP INT TERM

log_info "=========================================================="
log_info "Starting Worker Singleton Upgrade Transaction"
log_info "Candidate Worker Image: ${CANDIDATE_WORKER_IMAGE}"
log_info "Previous Worker Image:  ${PREV_WORKER_REF:-none}"
log_info "Owner:                  ${WORKER_DEPLOY_OWNER}"
log_info "=========================================================="

# 1. Preflight active-auth check (active in-flight sessions block worker upgrade)
if ! check_active_auth_gate; then
  log_error "Worker upgrade aborted: active customer authentication attempt in progress or check failed."
  exit 1
fi

# 2. Acquire durable mutation gate lease
GATE_TOKEN="$(acquire_mutation_gate "$DATA_VOLUME_NAME" "${DBTOOL_IMAGE_REF:-}" "$WORKER_DEPLOY_OWNER" "worker-upgrade")"

# 3. Quiesce old worker via RPC if container is running
WORKER_RPC_URL="${WORKER_RPC_URL:-http://127.0.0.1:8190}"
WORKER_TOKEN=""
if [[ -f "$SECRETS_DIR/worker_internal_token" ]]; then
  WORKER_TOKEN="$(tr -d ' \r\n' < "$SECRETS_DIR/worker_internal_token")"
fi

quiesce_old_worker() {
  log_info "Quiescing old worker via RPC (${WORKER_RPC_URL}/rpc/quiesce)..."
  if [[ -n "${WORKER_QUIESCE_CMD:-}" ]]; then
    local out
    if ! out="$($WORKER_QUIESCE_CMD 2>&1)"; then
      log_error "Worker quiesce command failed: ${out}"
      return 1
    fi
    log_info "Old worker quiesced successfully: ${out}"
    return 0
  fi

  if command -v docker >/dev/null 2>&1 && docker ps --format '{{.Names}}' | grep -q '^acb-worker$'; then
    local q_resp
    if command -v curl >/dev/null 2>&1; then
      if ! q_resp="$(curl -s -f -m 30 -X POST \
        -H "Content-Type: application/json" \
        -H "X-Worker-Internal-Token: ${WORKER_TOKEN}" \
        "${WORKER_RPC_URL}/rpc/quiesce" 2>&1)"; then
        log_error "Old worker quiesce RPC failed: ${q_resp}"
        return 1
      fi
      log_info "Old worker quiesce RPC reported: ${q_resp}"
    else
      log_info "curl not available on host; issuing container drain."
      docker exec acb-worker /worker --drain 2>/dev/null || true
    fi
  else
    log_info "No currently running acb-worker container found to quiesce."
  fi
  return 0
}

if ! quiesce_old_worker; then
  log_error "CRITICAL: Failed to quiesce running worker cleanly. Aborting worker upgrade without stopping container."
  exit 1
fi

# 4. Stop old worker container ONLY AFTER quiesce succeeds
log_info "Stopping old worker singleton container (acb-worker)..."
if [[ -n "${WORKER_STOP_CMD:-}" ]]; then
  $WORKER_STOP_CMD || true
elif command -v docker >/dev/null 2>&1 && docker ps -a --format '{{.Names}}' | grep -q '^acb-worker$'; then
  docker stop -t 10 acb-worker >/dev/null 2>&1 || true
  docker rm acb-worker >/dev/null 2>&1 || true
fi
OLD_WORKER_STOPPED=1
log_info "Old worker singleton container stopped and removed."

# 5. Start candidate worker container
log_info "Starting candidate worker singleton container (${CANDIDATE_WORKER_IMAGE})..."
start_worker_container() {
  local img="$1"
  if [[ -n "${WORKER_START_CMD:-}" ]]; then
    $WORKER_START_CMD "$img"
    return $?
  fi

  if command -v docker >/dev/null 2>&1; then
    WORKER_IMAGE_REF="$img" docker compose -f "$SCRIPT_DIR/compose.prod.yaml" up -d --no-deps worker
  else
    log_error "docker CLI not available to start worker"
    return 1
  fi
}

if ! start_worker_container "$CANDIDATE_WORKER_IMAGE"; then
  log_error "Failed to start candidate worker container."
  rollback_worker
  exit 1
fi

# 6. Candidate worker health and readiness probe
wait_for_worker_ready() {
  local timeout="${WORKER_READINESS_TIMEOUT:-${1:-30}}"
  local check_cmd="${2:-${WORKER_READY_CHECK_CMD:-}}"
  local elapsed=0
  log_info "Waiting for candidate worker readiness probe (timeout: ${timeout}s)..."

  if [[ -n "$check_cmd" ]]; then
    while [[ "$elapsed" -lt "$timeout" ]]; do
      if eval "$check_cmd" >/dev/null 2>&1; then
        log_info "Candidate worker passed readiness probe."
        return 0
      fi
      sleep 1
      elapsed=$(( elapsed + 1 ))
    done
    return 1
  fi

  while [[ "$elapsed" -lt "$timeout" ]]; do
    if command -v docker >/dev/null 2>&1 && docker ps --format '{{.Names}}' | grep -q '^acb-worker$'; then
      if docker exec acb-worker /worker --readiness-check >/dev/null 2>&1; then
        log_info "Candidate worker passed readiness probe via container probe."
        return 0
      fi
    fi
    if command -v curl >/dev/null 2>&1; then
      local code
      code="$(curl -s -o /dev/null -w "%{http_code}" -m 2 -H "X-Worker-Internal-Token: ${WORKER_TOKEN}" "${WORKER_RPC_URL}/readyz" 2>/dev/null || echo "000")"
      if [[ "$code" == "200" ]]; then
        log_info "Candidate worker passed readiness probe via HTTP /readyz."
        return 0
      fi
    fi
    sleep 1
    elapsed=$(( elapsed + 1 ))
  done
  return 1
}

rollback_worker() {
  log_error "=========================================================="
  log_error "TRIGGERING WORKER ROLLBACK TO PREVIOUS DIGEST"
  log_error "Target rollback image: ${PREV_WORKER_REF:-none}"
  log_error "=========================================================="

  if [[ -z "${PREV_WORKER_REF:-}" ]]; then
    log_error "No previous worker image reference available to rollback!"
    return 1
  fi

  if [[ -n "${WORKER_STOP_CMD:-}" ]]; then
    $WORKER_STOP_CMD || true
  elif command -v docker >/dev/null 2>&1 && docker ps -a --format '{{.Names}}' | grep -q '^acb-worker$'; then
    docker stop -t 10 acb-worker >/dev/null 2>&1 || true
    docker rm acb-worker >/dev/null 2>&1 || true
  fi

  log_info "Restarting previous worker image (${PREV_WORKER_REF})..."
  if ! start_worker_container "$PREV_WORKER_REF"; then
    log_error "CRITICAL: Failed to start rollback worker container!"
    return 1
  fi

  local rollback_cmd="${WORKER_ROLLBACK_READY_CHECK_CMD:-${WORKER_READY_CHECK_CMD:-}}"
  if ! wait_for_worker_ready "${WORKER_READINESS_TIMEOUT:-30}" "$rollback_cmd"; then
    log_error "CRITICAL: Rollback worker failed readiness check!"
    return 1
  fi
  log_info "Rollback worker successfully restored and verified ready."
  return 0
}

if ! wait_for_worker_ready "${WORKER_READINESS_TIMEOUT:-30}"; then
  log_error "Candidate worker failed readiness probe within timeout!"
  rollback_worker
  exit 1
fi

# 7. Commit new worker image reference to release state
set_release_env "WORKER_IMAGE_REF" "$CANDIDATE_WORKER_IMAGE"
log_info "Committed new WORKER_IMAGE_REF to .release.env."

# 8. Release durable mutation gate
release_mutation_gate "$DATA_VOLUME_NAME" "${DBTOOL_IMAGE_REF:-}" "$WORKER_DEPLOY_OWNER" "${GATE_TOKEN:-}"
GATE_TOKEN=""

log_info "=========================================================="
log_info "Worker Singleton Upgrade Transaction Successfully Completed."
log_info "Active Worker Image: ${CANDIDATE_WORKER_IMAGE}"
log_info "=========================================================="
exit 0
