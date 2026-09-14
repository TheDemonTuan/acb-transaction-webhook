#!/usr/bin/env bash
# deploy/deploy-bark.sh
# Auxiliary Bark Service Deployment Transaction.
# Replaces only Bark container; worker and gateway remain completely untouched.
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deploy/lib.sh
source "$SCRIPT_DIR/lib.sh"

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

acquire_deploy_lock
trap 'release_deploy_lock' EXIT

PREV_BARK_REF="$(get_release_env BARK_IMAGE_REF 2>/dev/null || true)"

log_info "=========================================================="
log_info "Starting Bark Auxiliary Deployment Transaction"
log_info "Candidate Bark Image: ${CANDIDATE_BARK_IMAGE}"
log_info "=========================================================="

start_bark() {
  local img="$1"
  if [[ -n "${BARK_START_CMD:-}" ]]; then
    $BARK_START_CMD "$img"
    return $?
  fi
  if command -v docker >/dev/null 2>&1; then
    docker stop -t 10 bark >/dev/null 2>&1 || true
    docker rm bark >/dev/null 2>&1 || true
    BARK_IMAGE_REF="$img" docker compose -f "$SCRIPT_DIR/compose.prod.yaml" up -d --no-deps bark
  fi
}

if ! start_bark "$CANDIDATE_BARK_IMAGE"; then
  log_error "Failed to start candidate Bark container."
  exit 1
fi

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
    if command -v docker >/dev/null 2>&1 && docker ps --format '{{.Names}}' | grep -q '^bark$'; then
      if docker exec bark /bin/sh -c "nc -z 127.0.0.1 8080" >/dev/null 2>&1; then
        return 0
      fi
    fi
    sleep 1
    elapsed=$(( elapsed + 1 ))
  done
  return 1
}

if ! wait_for_bark_ready 20; then
  log_error "Candidate Bark failed healthcheck. Rolling back to previous digest..."
  if [[ -n "${PREV_BARK_REF:-}" ]]; then
    start_bark "$PREV_BARK_REF"
    wait_for_bark_ready 20 || true
  fi
  exit 1
fi

set_release_env "BARK_IMAGE_REF" "$CANDIDATE_BARK_IMAGE"
log_info "Bark auxiliary deployment successfully completed."
exit 0
