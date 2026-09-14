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
OLD_WORKER_QUIESCED=0
OLD_WORKER_STOPPED=0

# Resolve previous running worker image digest
resolve_previous_worker_image() {
  local prev=""
  prev="$(get_release_env WORKER_IMAGE_REF 2>/dev/null || true)"
  if [[ -n "$prev" ]] && validate_digest "$prev" "worker" 2>/dev/null; then
    printf '%s\n' "$prev"
    return 0
  fi

  # Fallback to inspecting running worker container if release.env missing or unpinned
  if command -v docker >/dev/null 2>&1 && docker ps --format '{{.Names}}' 2>/dev/null | grep -q '^acb-worker$'; then
    local cfg_img
    cfg_img="$(docker inspect --format '{{.Config.Image}}' acb-worker 2>/dev/null || true)"
    if [[ -n "$cfg_img" ]] && validate_digest "$cfg_img" "worker" 2>/dev/null; then
      printf '%s\n' "$cfg_img"
      return 0
    fi

    local img_id
    img_id="$(docker inspect --format '{{.Image}}' acb-worker 2>/dev/null || true)"
    if [[ -n "$img_id" ]]; then
      local repo_digests
      repo_digests="$(docker inspect --format '{{range .RepoDigests}}{{.}}{{"\n"}}{{end}}' "$img_id" 2>/dev/null || true)"
      while IFS= read -r line; do
        if [[ -n "$line" ]] && validate_digest "$line" "worker" 2>/dev/null; then
          printf '%s\n' "$line"
          return 0
        fi
      done <<< "$repo_digests"
    fi
  fi

  return 1
}

PREV_WORKER_REF="$(resolve_previous_worker_image 2>/dev/null || true)"

OLD_WORKER_RUNNING=0
if command -v docker >/dev/null 2>&1 && docker ps --format '{{.Names}}' 2>/dev/null | grep -q '^acb-worker$'; then
  OLD_WORKER_RUNNING=1
fi

resume_old_worker() {
  log_info "Resuming old worker via RPC..."
  if [[ -n "${WORKER_RESUME_CMD:-}" ]]; then
    local out
    if out="$(eval "$WORKER_RESUME_CMD" 2>&1)"; then
      log_info "Old worker resumed via command: ${out}"
      return 0
    fi
    log_error "Worker resume command failed: ${out}"
    return 1
  fi

  if command -v docker >/dev/null 2>&1 && docker ps --format '{{.Names}}' 2>/dev/null | grep -q '^acb-worker$'; then
    local r_resp=""
    if r_resp="$(docker exec -e WORKER_INTERNAL_TOKEN_FILE=/run/secrets/worker_internal_token acb-worker /worker -resume 2>&1)"; then
      log_info "Old worker resumed via container exec: ${r_resp}"
      return 0
    fi

    local rpc_port="8190"
    if [[ -n "${WORKER_PORT:-}" ]]; then
      rpc_port="$WORKER_PORT"
    elif [[ "${WORKER_RPC_URL:-}" =~ :([0-9]+) ]]; then
      rpc_port="${BASH_REMATCH[1]}"
    fi

    if r_resp="$(docker run --rm --network container:acb-worker --entrypoint /worker -v "$SECRETS_DIR/worker_internal_token:/run/secrets/worker_internal_token:ro" -e WORKER_INTERNAL_TOKEN_FILE=/run/secrets/worker_internal_token -e WORKER_PORT="$rpc_port" "$CANDIDATE_WORKER_IMAGE" -resume 2>&1)"; then
      log_info "Old worker resumed via candidate container RPC: ${r_resp}"
      return 0
    fi
    log_error "Failed to resume running worker container: ${r_resp}"
    return 1
  fi
  return 0
}

cleanup_worker_deploy() {
  local exit_code=$?
  if [[ "${OLD_WORKER_QUIESCED:-0}" -eq 1 && "${OLD_WORKER_STOPPED:-0}" -eq 0 ]]; then
    log_warn "Upgrade aborted after worker was quiesced but before container was stopped. Resuming old worker..."
    resume_old_worker 2>/dev/null || log_error "Failed to resume quiesced old worker"
  fi
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

# Safety check: fail closed before mutation gate or stop if running worker lacks rollback digest
if [[ "$OLD_WORKER_RUNNING" -eq 1 ]]; then
  if [[ -z "${PREV_WORKER_REF:-}" ]] || ! validate_digest "$PREV_WORKER_REF" "previous worker" 2>/dev/null; then
    log_error "CRITICAL: Running worker container detected, but failed to resolve valid immutable rollback digest. Aborting before quiesce/stop."
    exit 1
  fi
fi

# 1. Preflight active-auth check (active in-flight sessions block worker upgrade)
if ! check_active_auth_gate; then
  log_error "Worker upgrade aborted: active customer authentication attempt in progress or check failed."
  exit 1
fi

# 2. Acquire durable mutation gate lease
GATE_TOKEN="$(acquire_mutation_gate "$DATA_VOLUME_NAME" "${DBTOOL_IMAGE_REF:-}" "$WORKER_DEPLOY_OWNER" "worker-upgrade")"

# 3. Quiesce old worker via RPC if container is running
WORKER_RPC_URL="${WORKER_RPC_URL:-http://127.0.0.1:8190}"

verify_quiesce_response() {
  local resp="$1"
  if [[ -z "$resp" ]]; then
    return 1
  fi
  local is_quiesced="false"
  if command -v jq >/dev/null 2>&1; then
    is_quiesced="$(printf '%s' "$resp" | jq -e -r '.quiesced' 2>/dev/null || echo "false")"
  fi
  if [[ "$is_quiesced" != "true" ]] && command -v python3 >/dev/null 2>&1; then
    is_quiesced="$(python3 -c "import sys, json, re
text = sys.stdin.read()
m = re.search(r'\{.*\}', text, re.DOTALL)
if m:
    try:
        data = json.loads(m.group(0))
        if data.get('quiesced') is True:
            print('true')
            sys.exit(0)
    except Exception:
        pass
print('false')
" <<< "$resp" 2>/dev/null || echo "false")"
  fi
  if [[ "$is_quiesced" != "true" ]]; then
    if [[ "$resp" =~ \"quiesced\"[[:space:]]*:[[:space:]]*true ]]; then
      is_quiesced="true"
    fi
  fi

  if [[ "$is_quiesced" == "true" ]]; then
    return 0
  fi
  return 1
}

verify_quiesced_json() {
  verify_quiesce_response "$@"
}

quiesce_old_worker() {
  log_info "Quiescing old worker via RPC..."
  if [[ -n "${WORKER_QUIESCE_CMD:-}" ]]; then
    local out
    if ! out="$(eval "$WORKER_QUIESCE_CMD" 2>&1)"; then
      log_error "Worker quiesce command failed: ${out}"
      return 1
    fi
    if ! verify_quiesced_json "$out"; then
      log_error "Worker quiesce command returned unverified response: ${out}"
      return 1
    fi
    OLD_WORKER_QUIESCED=1
    log_info "Old worker quiesced successfully: ${out}"
    return 0
  fi

  if command -v docker >/dev/null 2>&1 && docker ps --format '{{.Names}}' 2>/dev/null | grep -q '^acb-worker$'; then
    local q_resp=""
    local exec_ok=0
    if q_resp="$(docker exec -e WORKER_INTERNAL_TOKEN_FILE=/run/secrets/worker_internal_token acb-worker /worker -quiesce 2>&1)"; then
      if verify_quiesced_json "$q_resp"; then
        exec_ok=1
      fi
    fi

    if [[ "$exec_ok" -eq 1 ]]; then
      OLD_WORKER_QUIESCED=1
      log_info "Old worker quiesced via container exec: ${q_resp}"
      return 0
    fi

    log_info "Container exec quiesce did not succeed (${q_resp}); trying candidate image RPC client..."

    local rpc_port="8190"
    if [[ -n "${WORKER_PORT:-}" ]]; then
      rpc_port="$WORKER_PORT"
    elif [[ "${WORKER_RPC_URL:-}" =~ :([0-9]+) ]]; then
      rpc_port="${BASH_REMATCH[1]}"
    fi

    if q_resp="$(docker run --rm --network container:acb-worker --entrypoint /worker -v "$SECRETS_DIR/worker_internal_token:/run/secrets/worker_internal_token:ro" -e WORKER_INTERNAL_TOKEN_FILE=/run/secrets/worker_internal_token -e WORKER_PORT="$rpc_port" "$CANDIDATE_WORKER_IMAGE" -quiesce 2>&1)"; then
      if verify_quiesce_response "$q_resp"; then
        OLD_WORKER_QUIESCED=1
        log_info "Old worker quiesced via candidate container RPC: ${q_resp}"
        return 0
      else
        log_error "Candidate container quiesce returned unverified response: ${q_resp}"
      fi
    else
      log_error "Candidate container quiesce RPC failed: ${q_resp}"
    fi

    log_error "Failed to quiesce running worker container: ${q_resp}"
    return 1
  else
    log_info "No currently running acb-worker container found to quiesce."
  fi
  return 0
}

if ! quiesce_old_worker; then
  log_error "CRITICAL: Failed to quiesce running worker cleanly. Aborting worker upgrade without stopping container."
  exit 1
fi

# Pre-stop guard: verify rollback digest exists if old worker was running
if [[ "$OLD_WORKER_RUNNING" -eq 1 ]]; then
  if [[ -z "${PREV_WORKER_REF:-}" ]] || ! validate_digest "$PREV_WORKER_REF" "previous worker" 2>/dev/null; then
    log_error "CRITICAL: Cannot stop running worker: rollback digest is missing or invalid. Aborting."
    exit 1
  fi
fi

# 4. Stop old worker container ONLY AFTER quiesce succeeds
log_info "Stopping old worker singleton container (acb-worker)..."
if [[ -n "${WORKER_STOP_CMD:-}" ]]; then
  if ! eval "$WORKER_STOP_CMD"; then
    log_error "Failed to stop old worker container via command."
    exit 1
  fi
elif command -v docker >/dev/null 2>&1 && docker ps -a --format '{{.Names}}' 2>/dev/null | grep -q '^acb-worker$'; then
  if ! docker stop -t 10 acb-worker >/dev/null 2>&1; then
    log_error "Failed to stop acb-worker container."
    exit 1
  fi
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
    WORKER_IMAGE_REF="$img" compose_prod up -d --no-deps worker
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
    if command -v docker >/dev/null 2>&1 && docker ps --format '{{.Names}}' 2>/dev/null | grep -q '^acb-worker$'; then
      if docker exec acb-worker /worker --readiness-check >/dev/null 2>&1; then
        log_info "Candidate worker passed readiness probe via container probe."
        return 0
      fi
    fi
    if command -v curl >/dev/null 2>&1 && [[ -n "${WORKER_RPC_URL:-}" ]]; then
      local code
      code="$(curl -s -o /dev/null -w "%{http_code}" -m 2 "${WORKER_RPC_URL}/readyz" 2>/dev/null || echo "000")"
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
  elif command -v docker >/dev/null 2>&1 && docker ps -a --format '{{.Names}}' 2>/dev/null | grep -q '^acb-worker$'; then
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
