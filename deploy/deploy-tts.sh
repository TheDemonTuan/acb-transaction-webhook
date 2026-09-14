#!/usr/bin/env bash
# deploy/deploy-tts.sh
# Auxiliary TTS Service Deployment Transaction.
# Replaces only TTS container; worker and gateway remain completely untouched.
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deploy/lib.sh
source "$SCRIPT_DIR/lib.sh"

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
trap 'release_deploy_lock' EXIT

PREV_TTS_REF="$(get_release_env TTS_IMAGE_REF 2>/dev/null || true)"

log_info "=========================================================="
log_info "Starting TTS Auxiliary Deployment Transaction"
log_info "Candidate TTS Image: ${CANDIDATE_TTS_IMAGE}"
log_info "=========================================================="

start_tts() {
  local img="$1"
  if [[ -n "${TTS_START_CMD:-}" ]]; then
    $TTS_START_CMD "$img"
    return $?
  fi
  if command -v docker >/dev/null 2>&1; then
    docker stop -t 10 tts-gateway >/dev/null 2>&1 || true
    docker rm tts-gateway >/dev/null 2>&1 || true
    TTS_IMAGE_REF="$img" docker compose -f "$SCRIPT_DIR/compose.prod.yaml" up -d --no-deps tts-gateway
  fi
}

if ! start_tts "$CANDIDATE_TTS_IMAGE"; then
  log_error "Failed to start candidate TTS container."
  exit 1
fi

# Smoke check
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
    if command -v docker >/dev/null 2>&1 && docker ps --format '{{.Names}}' | grep -q '^tts-gateway$'; then
      if docker exec tts-gateway python3 -c "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8095/healthz')" >/dev/null 2>&1; then
        return 0
      fi
    fi
    sleep 1
    elapsed=$(( elapsed + 1 ))
  done
  return 1
}

if ! wait_for_tts_ready 20; then
  log_error "Candidate TTS failed healthcheck. Rolling back to previous digest..."
  if [[ -n "${PREV_TTS_REF:-}" ]]; then
    start_tts "$PREV_TTS_REF"
    wait_for_tts_ready 20 || true
  fi
  exit 1
fi

set_release_env "TTS_IMAGE_REF" "$CANDIDATE_TTS_IMAGE"
log_info "TTS auxiliary deployment successfully completed."
exit 0
