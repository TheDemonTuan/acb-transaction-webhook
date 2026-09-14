#!/usr/bin/env bash
# Isolated frontend deployment. Gateway, worker, TTS and Bark are never recreated.
set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deploy/lib.sh
source "$SCRIPT_DIR/lib.sh"

CANDIDATE_FRONTEND_IMAGE="${1:-${FRONTEND_IMAGE_REF:-}}"
if [[ -z "$CANDIDATE_FRONTEND_IMAGE" ]]; then
  log_error "Usage: $0 <candidate-frontend-image-digest>"
  exit 1
fi

validate_canonical_env
validate_digest "$CANDIDATE_FRONTEND_IMAGE" "frontend"
acquire_deploy_lock
recover_tx_journal

PREV_FRONTEND_REF="$(get_release_env FRONTEND_IMAGE_REF 2>/dev/null || true)"
if [[ -z "$PREV_FRONTEND_REF" ]] && command -v docker >/dev/null 2>&1; then
  PREV_FRONTEND_REF="$(docker inspect --format '{{.Config.Image}}' acb-frontend 2>/dev/null || true)"
fi
FRONTEND_COMMITTED=0

cleanup_frontend_deploy() {
  local code=$?
  if [[ "$code" -ne 0 && "$FRONTEND_COMMITTED" -eq 0 && -n "$PREV_FRONTEND_REF" ]]; then
    log_warn "Frontend candidate failed; restoring previous digest."
    FRONTEND_IMAGE_REF="$PREV_FRONTEND_REF" compose_prod up -d --no-deps frontend >/dev/null 2>&1 || true
  fi
  release_deploy_lock
  exit "$code"
}
trap cleanup_frontend_deploy EXIT HUP INT TERM

wait_for_frontend_ready() {
  local timeout="${FRONTEND_READY_TIMEOUT:-30}"
  local elapsed=0
  while [[ "$elapsed" -lt "$timeout" ]]; do
    if docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' acb-frontend 2>/dev/null | grep -qx 'healthy'; then
      return 0
    fi
    sleep 1
    elapsed=$((elapsed + 1))
  done
  return 1
}

log_info "Deploying frontend only: $CANDIDATE_FRONTEND_IMAGE"
FRONTEND_IMAGE_REF="$CANDIDATE_FRONTEND_IMAGE" compose_prod pull frontend
FRONTEND_IMAGE_REF="$CANDIDATE_FRONTEND_IMAGE" compose_prod up -d --no-deps frontend

if ! wait_for_frontend_ready; then
  log_error "Frontend candidate failed readiness."
  exit 1
fi

set_release_env FRONTEND_IMAGE_REF "$CANDIDATE_FRONTEND_IMAGE"
FRONTEND_COMMITTED=1
log_info "Frontend deployment committed. Gateway, worker, TTS and Bark were untouched."
