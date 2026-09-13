#!/usr/bin/env bash
set -Eeuo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
host="${HOST:-127.0.0.1}"
port="${PORT:-8090}"
timeout="${READY_TIMEOUT:-120}"
compose_file="${COMPOSE_FILE:-$script_dir/compose.prod.yaml}"
expected_gateway_image="${IMAGE_REF:-}"
expected_browser_image="${AUTH_BROWSER_IMAGE_REF:-}"
expected_tts_image="${TTS_GATEWAY_IMAGE_REF:-}"
expected_bark_image="${BARK_IMAGE_REF:-}"
start="$(date +%s)"

verify_image() {
  local container_name="$1"
  local expected="$2"
  local service_name="$3"
  [[ -n "$expected" ]] || { printf 'Expected image is missing for %s.\n' "$service_name" >&2; return 1; }
  local actual
  actual="$(docker inspect --format '{{.Config.Image}}' "$container_name" 2>/dev/null || true)"
  if [[ "$actual" != "$expected" ]]; then
    printf 'Image mismatch for %s: expected=%s actual=%s\n' "$service_name" "$expected" "${actual:-missing}" >&2
    return 1
  fi
  printf '%s is running expected image %s\n' "$service_name" "$expected"
}

# Step 1: Verify gateway readiness
gateway_container="${GATEWAY_CONTAINER:-acb-transaction-gateway}"
printf 'Waiting for Gateway readiness (%s, timeout: %ss)...\n' "$gateway_container" "$timeout"
while true; do
  if [[ "$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{end}}' "$gateway_container" 2>/dev/null || true)" == "healthy" ]]; then
    printf 'Gateway container is healthy: %s\n' "$gateway_container"
    break
  fi
  if curl --fail --silent --show-error "http://${host}:${port}/readyz" >/dev/null 2>&1 || \
     curl --fail --silent --show-error "http://${host}:${port}/ready" >/dev/null 2>&1 || \
     curl --fail --silent --show-error "http://${host}:8080/readyz" >/dev/null 2>&1 || \
     curl --fail --silent --show-error "http://${host}:8090/readyz" >/dev/null 2>&1; then
    printf 'Gateway is ready at http://%s:%s\n' "$host" "$port"
    break
  fi
  if (( $(date +%s) - start >= timeout )); then
    printf 'Gateway did not become ready within %ss\n' "$timeout" >&2
    if command -v docker >/dev/null 2>&1 && [[ -f "$compose_file" ]]; then
      echo "=== Docker Compose Container Status ===" >&2
      docker compose -f "$compose_file" ps -a >&2 || true
      echo "=== Gateway Logs ===" >&2
      docker compose -f "$compose_file" logs --tail 50 gateway >&2 || true
      echo "=== Auth-browser Logs ===" >&2
      docker compose -f "$compose_file" logs --tail 50 auth-browser >&2 || true
    fi
    exit 1
  fi
  sleep 2
done

# Step 2: Verify immutable images and auth-browser health.
# Explicit constraint: NEVER create or cancel production login sessions.
if command -v docker >/dev/null 2>&1 && [[ -f "$compose_file" ]]; then
  verify_image "$gateway_container" "$expected_gateway_image" "gateway"
  if docker compose --env-file "${ENV_FILE:-$script_dir/.env.production}" -f "$compose_file" config --services 2>/dev/null | grep -q "^auth-browser$"; then
    echo "Verifying auth-browser container health..."
    container_name="${AUTH_BROWSER_CONTAINER:-acb-auth-browser}"
    verify_image "$container_name" "$expected_browser_image" "auth-browser"
    ab_timeout="${AUTH_BROWSER_READY_TIMEOUT:-60}"
    ab_start="$(date +%s)"
    ab_healthy=0

    while true; do
      running=$(docker inspect --format='{{.State.Running}}' "$container_name" 2>/dev/null || echo "false")
      restarts=$(docker inspect --format='{{.RestartCount}}' "$container_name" 2>/dev/null || echo "0")

      if [[ "$running" == "true" ]]; then
        # Run in-container healthcheck directly without touching /sessions
        if docker compose -f "$compose_file" exec -T auth-browser /auth-browser --healthcheck >/dev/null 2>&1; then
          ab_healthy=1
          if [[ "$restarts" -gt 0 ]]; then
            echo "[DIAGNOSTIC] auth-browser container has restarted ${restarts} time(s) since container creation" >&2
            echo "=== Recent auth-browser logs (restart diagnostic) ===" >&2
            docker compose -f "$compose_file" logs --tail 30 auth-browser >&2 || true
          fi
          printf 'Auth-browser is healthy (container: %s, restarts: %s)\n' "$container_name" "$restarts"
          break
        fi
      fi

      if (( $(date +%s) - ab_start >= ab_timeout )); then
        echo "Error: auth-browser did not become healthy within ${ab_timeout}s" >&2
        echo "=== Docker Compose Container Status ===" >&2
        docker compose -f "$compose_file" ps -a >&2 || true
        echo "=== Auth-browser Logs ===" >&2
        docker compose -f "$compose_file" logs --tail 50 auth-browser >&2 || true
        exit 1
      fi
      sleep 2
    done
  fi
  if docker compose --env-file "${ENV_FILE:-$script_dir/.env.production}" -f "$compose_file" config --services 2>/dev/null | grep -q "^tts-gateway$"; then
    echo "Verifying tts-gateway container health..."
    tts_container="${TTS_GATEWAY_CONTAINER:-acb-tts-gateway}"
    if [[ -n "$expected_tts_image" ]]; then
      verify_image "$tts_container" "$expected_tts_image" "tts-gateway"
    fi
    tts_timeout="${TTS_READY_TIMEOUT:-60}"
    tts_start="$(date +%s)"
    while true; do
      tts_running=$(docker inspect --format='{{.State.Running}}' "$tts_container" 2>/dev/null || echo "false")
      if [[ "$tts_running" == "true" ]]; then
        if docker compose -f "$compose_file" exec -T tts-gateway curl -f http://127.0.0.1:8081/health >/dev/null 2>&1; then
          printf 'TTS-gateway is healthy (container: %s)\n' "$tts_container"
          break
        fi
      fi
      if (( $(date +%s) - tts_start >= tts_timeout )); then
        echo "Error: tts-gateway did not become healthy within ${tts_timeout}s" >&2
        docker compose -f "$compose_file" logs --tail 30 tts-gateway >&2 || true
        exit 1
      fi
      sleep 2
    done
  fi
  if docker compose --env-file "${ENV_FILE:-$script_dir/.env.production}" -f "$compose_file" config --services 2>/dev/null | grep -q "^bark$"; then
    bark_container="${BARK_CONTAINER:-acb-bark}"
    verify_image "$bark_container" "$expected_bark_image" "bark"
    if [[ "$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{end}}' "$bark_container" 2>/dev/null || true)" != "healthy" ]]; then
      printf 'Bark container is not healthy.\n' >&2
      docker compose -f "$compose_file" logs --tail 30 bark >&2 || true
      exit 1
    fi
  fi
elif [[ -n "${AUTH_BROWSER_URL:-}" ]]; then
  # Non-Docker fallback: verify healthz endpoint only (never /sessions)
  if ! curl --fail --silent --show-error "${AUTH_BROWSER_URL}/healthz" >/dev/null 2>&1; then
    echo "Error: auth-browser health check failed at ${AUTH_BROWSER_URL}/healthz" >&2
    exit 1
  fi
  printf 'Auth-browser is healthy at %s/healthz\n' "$AUTH_BROWSER_URL"
fi

printf 'Deployment verification completed successfully.\n'
exit 0
