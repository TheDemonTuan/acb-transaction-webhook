#!/usr/bin/env bash
# deploy/deploy-bark.sh
# Auxiliary Bark Service Deployment Transaction with Fail-Safe Rollback, Journaling, and Data Volume Preservation.
# Invariants:
# 1. Replaces only Bark container; worker, gateway, browser, TTS containers remain completely untouched.
# 2. Preserves persistent data volume (bark_data) and basic-auth secrets across updates and rollbacks.
# 3. Rolls back to previous digest if candidate Bark fails healthcheck.
# 4. Enforces deployment transaction journal and atomic recovery.
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deploy/lib.sh
source "$SCRIPT_DIR/lib.sh"
require_release_orchestrator

CANDIDATE_BARK_IMAGE="${1:-${BARK_IMAGE_REF:-}}"

if [[ -z "$CANDIDATE_BARK_IMAGE" ]]; then
  CANDIDATE_BARK_IMAGE="$(get_release_env BARK_IMAGE_REF 2>/dev/null || true)"
fi

if [[ -z "$CANDIDATE_BARK_IMAGE" ]]; then
  log_error "Usage: $0 <candidate-bark-image-digest>"
  exit 1
fi

validate_canonical_env
validate_digest "$CANDIDATE_BARK_IMAGE" "bark-server"
validate_secrets

PREV_BARK_REF="$(get_release_env BARK_IMAGE_REF 2>/dev/null || true)"

stop_bark() {
  if command -v docker >/dev/null 2>&1; then
    for c in acb-bark bark; do
      if docker ps -a --format '{{.Names}}' | grep -q "^${c}\$"; then
        docker stop -t 10 "$c" >/dev/null 2>&1 || true
        docker rm "$c" >/dev/null 2>&1 || true
      fi
    done
  fi
}

start_bark() {
  local img="$1"
  if [[ -n "${BARK_START_CMD:-}" ]]; then
    $BARK_START_CMD "$img"
    return $?
  fi
  if command -v docker >/dev/null 2>&1; then
    stop_bark
    # The upstream image runs as UID 0 and writes only to the dedicated Bark volume;
    # pre-create its database path without granting host-wide capabilities.
    docker run --rm --network none --entrypoint /bin/sh -v "${BARK_VOLUME_NAME}:/data:rw" "$img" -c 'touch /data/bark.db && chmod 0600 /data/bark.db'
    BARK_IMAGE_REF="$img" compose_prod up -d --no-deps bark
  fi
}

wait_for_bark_ready() {
  local timeout="${1:-20}"
  local elapsed=0
  if [[ -n "${BARK_READY_CHECK_CMD:-}" ]]; then
    while [[ "$elapsed" -lt "$timeout" ]]; do
      if $BARK_READY_CHECK_CMD >/dev/null 2>&1; then
        return 0
      fi
      sleep 1
      elapsed=$(( elapsed + 1 ))
    done
    return 1
  fi

  while [[ "$elapsed" -lt "$timeout" ]]; do
    if command -v docker >/dev/null 2>&1; then
      for c in acb-bark bark; do
        if docker ps --format '{{.Names}}' | grep -q "^${c}\$"; then
          local st
          st="$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{end}}' "$c" 2>/dev/null || echo "")"
          if [[ "$st" == "healthy" ]]; then
            return 0
          fi
          if [[ "$st" == "unhealthy" ]] || [[ "$(docker inspect --format '{{.State.Status}}' "$c" 2>/dev/null || true)" == "exited" ]]; then
            docker inspect --format 'Bark state={{json .State}}' "$c" >&2 || true
            docker logs --tail 100 "$c" >&2 || true
            return 1
          fi
        fi
      done
    fi
    sleep 1
    elapsed=$(( elapsed + 1 ))
  done
  return 1
}

bark_is_unchanged_and_healthy() {
  if [[ -n "${BARK_UNCHANGED_CHECK_CMD:-}" ]]; then
    "$BARK_UNCHANGED_CHECK_CMD" "$CANDIDATE_BARK_IMAGE"
    return $?
  fi
  command -v docker >/dev/null 2>&1 || return 1

  local container="" candidate_hash="" actual_hash="" actual_image="" health="" started_at="" started_epoch="" secret_mtime=""
  for candidate in acb-bark bark; do
    if docker ps --format '{{.Names}}' 2>/dev/null | grep -q "^${candidate}$"; then
      container="$candidate"
      break
    fi
  done
  [[ -n "$container" ]] || return 1

  actual_image="$(docker inspect --format '{{.Config.Image}}' "$container" 2>/dev/null || true)"
  health="$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{end}}' "$container" 2>/dev/null || true)"
  actual_hash="$(docker inspect --format '{{index .Config.Labels "com.docker.compose.config-hash"}}' "$container" 2>/dev/null || true)"
  candidate_hash="$(BARK_IMAGE_REF="$CANDIDATE_BARK_IMAGE" compose_prod config --hash bark 2>/dev/null | awk 'NF == 1 {print $1; exit} $1 == "bark" {print $2; exit}')"
  started_at="$(docker inspect --format '{{.State.StartedAt}}' "$container" 2>/dev/null || true)"
  started_epoch="$(python3 - "$started_at" <<'PY' 2>/dev/null || true
from datetime import datetime
import sys
value = sys.argv[1].replace('Z', '+00:00')
print(int(datetime.fromisoformat(value).timestamp()))
PY
)"
  [[ "$started_epoch" =~ ^[0-9]+$ ]] || return 1
  for secret in "$SECRETS_DIR/bark_basic_auth_user" "$SECRETS_DIR/bark_basic_auth_password"; do
    secret_mtime="$(stat -c '%Y' "$secret" 2>/dev/null || true)"
    [[ "$secret_mtime" =~ ^[0-9]+$ && "$secret_mtime" -le "$started_epoch" ]] || return 1
  done

  [[ "$actual_image" == "$CANDIDATE_BARK_IMAGE" && "$health" == "healthy" && -n "$actual_hash" && "$actual_hash" == "$candidate_hash" ]]
}

rollback_bark() {
  local reason="$1"
  if [[ -z "${PREV_BARK_REF:-}" ]]; then
    update_tx_state "TX_FAILED" "${reason}; no previous Bark digest available"
    return 1
  fi

  update_tx_state "TX_ROLLING_BACK" "$reason"
  stop_bark
  if start_bark "$PREV_BARK_REF" && wait_for_bark_ready "${READY_CHECK_TIMEOUT:-20}"; then
    update_tx_state "TX_ROLLED_BACK" "Restored previous Bark image $PREV_BARK_REF"
    log_info "Rollback to previous Bark digest succeeded."
    return 0
  fi

  update_tx_state "TX_ROLLBACK_FAILED" "Previous Bark image failed to start or become healthy"
  log_error "Rollback failed: previous Bark digest did not become healthy."
  return 1
}

cleanup_bark_deploy() {
  local exit_code=$?
  if [[ -f "$TX_JOURNAL_FILE" ]] && ! is_tx_committed; then
    local st
    st="$(get_tx_state)"
    if [[ "$st" != "TX_ROLLED_BACK" && "$st" != "TX_ROLLBACK_FAILED" && "$st" != "TX_COMPLETED" ]]; then
      log_warn "Interruption caught in state [$st]. Rolling back Bark..."
      rollback_bark "Interruption caught in state [$st]" || true
    fi
  fi
  release_deploy_lock
  exit "$exit_code"
}
acquire_deploy_lock
prepare_bark_secret_permissions
validate_secrets
preflight_bark_secret_access "$CANDIDATE_BARK_IMAGE"

trap cleanup_bark_deploy EXIT HUP INT TERM
recover_tx_journal

if bark_is_unchanged_and_healthy; then
  log_info "Bark image and effective Compose configuration are unchanged and the container is healthy; skipping restart."
  exit 0
fi

log_info "=========================================================="
log_info "Starting Bark Auxiliary Deployment Transaction"
log_info "Candidate Bark Image: ${CANDIDATE_BARK_IMAGE}"
log_info "Previous Bark Image:  ${PREV_BARK_REF:-none}"
log_info "=========================================================="

# 1. Init transaction journal
init_tx_journal "bark" "singleton" "singleton" "$CANDIDATE_BARK_IMAGE" "$PREV_BARK_REF" "${EXPECTED_COMMIT:-}"
update_tx_state "CANDIDATE_STARTING" "Starting candidate Bark container"

# 2. Start candidate container
log_info "Starting candidate Bark container..."
if ! start_bark "$CANDIDATE_BARK_IMAGE"; then
  log_error "Failed to start candidate Bark container."
  rollback_bark "Candidate startup failed" || true
  exit 1
fi

# 3. Health check candidate Bark
READY_CHECK_TIMEOUT="${READY_CHECK_TIMEOUT:-20}"
update_tx_state "VERIFYING_HEALTH" "Verifying candidate Bark health"
if ! wait_for_bark_ready "$READY_CHECK_TIMEOUT"; then
  log_error "Candidate Bark failed healthcheck. Rolling back to previous digest..."
  rollback_bark "Candidate failed healthcheck" || true
  exit 1
fi

# 4. Record component success; the dispatcher owns release-state commit.
update_tx_state "TX_COMMITTED" "Candidate Bark verified healthy"

update_tx_state "TX_COMPLETED" "Bark auxiliary deployment transaction completed successfully"
archive_tx_journal "completed"

log_info "=========================================================="
log_info "Bark Auxiliary Deployment Transaction Successfully Completed"
log_info "=========================================================="
exit 0
