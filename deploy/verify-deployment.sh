#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deploy/lib.sh
source "$SCRIPT_DIR/lib.sh"

host="${HOST:-127.0.0.1}"
port="${PORT:-8090}"
timeout="${READY_TIMEOUT:-120}"
compose_file="${COMPOSE_FILE:-$SCRIPT_DIR/compose.prod.yaml}"
expected_gateway_image="${IMAGE_REF:-${GATEWAY_IMAGE_REF:-}}"
expected_browser_image="${AUTH_BROWSER_IMAGE_REF:-${BROWSER_IMAGE_REF:-}}"
expected_tts_image="${TTS_GATEWAY_IMAGE_REF:-${TTS_IMAGE_REF:-}}"
expected_bark_image="${BARK_IMAGE_REF:-}"
expected_worker_image="${WORKER_IMAGE_REF:-}"
start="$(date +%s)"

verify_image() {
  local container_name="$1"
  local expected="$2"
  local service_name="$3"
  [[ -n "$expected" ]] || return 0
  local actual
  actual="$(docker inspect --format '{{.Config.Image}}' "$container_name" 2>/dev/null || true)"
  if [[ "$actual" != "$expected" ]]; then
    log_error "Image mismatch for ${service_name}: expected=${expected} actual=${actual:-missing}"
    return 1
  fi
  log_info "${service_name} is running expected image ${expected}"
}

# Step 1: Determine active slot and container
active_slot="$(get_active_slot)"
gateway_container="acb-gateway-${active_slot}"

log_info "Waiting for Gateway readiness (${gateway_container}, timeout: ${timeout}s)..."
while true; do
  if [[ "$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{end}}' "$gateway_container" 2>/dev/null || true)" == "healthy" ]]; then
    log_info "Gateway container is healthy: ${gateway_container}"
    break
  fi
  if docker exec "$gateway_container" /gateway --healthcheck >/dev/null 2>&1; then
    log_info "Gateway passed internal health probe: ${gateway_container}"
    break
  fi
  if curl --fail --silent --show-error "http://${host}:${port}/readyz" >/dev/null 2>&1 || \
     curl --fail --silent --show-error "http://${host}:${port}/ready" >/dev/null 2>&1; then
    log_info "Gateway is ready at http://${host}:${port}"
    break
  fi
  if (( $(date +%s) - start >= timeout )); then
    log_error "Gateway did not become ready within ${timeout}s"
    if command -v docker >/dev/null 2>&1 && [[ -f "$compose_file" ]]; then
      docker compose -f "$compose_file" ps -a >&2 || true
      docker compose -f "$compose_file" logs --tail 50 "gateway-${active_slot}" >&2 || true
    fi
    exit 1
  fi
  sleep 2
done

# Step 2: Verify immutable images if requested
if command -v docker >/dev/null 2>&1; then
  verify_image "$gateway_container" "$expected_gateway_image" "gateway"
  if [[ -n "$expected_worker_image" ]]; then
    verify_image "acb-worker" "$expected_worker_image" "worker"
  fi

  if docker compose -f "$compose_file" config --services 2>/dev/null | grep -q "^auth-browser$"; then
    log_info "Verifying auth-browser container health..."
    ab_container="acb-auth-browser"
    verify_image "$ab_container" "$expected_browser_image" "auth-browser"
    ab_timeout="${AUTH_BROWSER_READY_TIMEOUT:-60}"
    ab_start="$(date +%s)"
    while true; do
      running=$(docker inspect --format='{{.State.Running}}' "$ab_container" 2>/dev/null || echo "false")
      if [[ "$running" == "true" ]]; then
        if docker compose -f "$compose_file" exec -T auth-browser /auth-browser --healthcheck >/dev/null 2>&1; then
          log_info "Auth-browser is healthy"
          break
        fi
      fi
      if (( $(date +%s) - ab_start >= ab_timeout )); then
        log_error "auth-browser did not become healthy within ${ab_timeout}s"
        docker compose -f "$compose_file" logs --tail 50 auth-browser >&2 || true
        exit 1
      fi
      sleep 2
    done
  fi

  if docker compose -f "$compose_file" config --services 2>/dev/null | grep -q "^tts-gateway$"; then
    log_info "Verifying tts-gateway container health..."
    tts_container="acb-tts-gateway"
    verify_image "$tts_container" "$expected_tts_image" "tts-gateway"
    tts_timeout="${TTS_READY_TIMEOUT:-60}"
    tts_start="$(date +%s)"
    while true; do
      tts_running=$(docker inspect --format='{{.State.Running}}' "$tts_container" 2>/dev/null || echo "false")
      if [[ "$tts_running" == "true" ]]; then
        if docker compose -f "$compose_file" exec -T tts-gateway curl -f http://127.0.0.1:8081/health >/dev/null 2>&1; then
          log_info "TTS-gateway is healthy"
          break
        fi
      fi
      if (( $(date +%s) - tts_start >= tts_timeout )); then
        log_error "tts-gateway did not become healthy within ${tts_timeout}s"
        docker compose -f "$compose_file" logs --tail 30 tts-gateway >&2 || true
        exit 1
      fi
      sleep 2
    done
  fi

  if docker compose -f "$compose_file" config --services 2>/dev/null | grep -q "^bark$"; then
    bark_container="acb-bark"
    verify_image "$bark_container" "$expected_bark_image" "bark"
    if [[ "$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{end}}' "$bark_container" 2>/dev/null || true)" != "healthy" ]]; then
      if ! docker compose -f "$compose_file" exec -T bark /bin/sh -c "nc -z 127.0.0.1 8080" 2>/dev/null; then
        log_warn "Bark container health check probe did not report healthy"
      fi
    fi
  fi
fi

log_info "Deployment verification completed successfully."
exit 0
