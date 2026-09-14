#!/usr/bin/env bash
set -Eeuo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
image_ref="${1:-${TTS_GATEWAY_IMAGE:-ghcr.io/thedemontuan/acb-transaction-webhook-tts-gateway:latest}}"
container_name="${CONTAINER_NAME:-tts-gateway-smoke-$$-${RANDOM}}"

TTS_PORT="${TTS_PORT:-8183}"
host="127.0.0.1"

if ! command -v docker >/dev/null 2>&1; then
  echo "Error: Docker CLI is required for the tts-gateway smoke test." >&2
  exit 1
fi

if ! docker info >/dev/null 2>&1; then
  echo "Error: Docker daemon is required for the tts-gateway smoke test." >&2
  exit 1
fi

temp_dir="$(mktemp -d)"
secret_token="$(openssl rand -hex 32)"
token_file="$temp_dir/tts_internal_token"
printf '%s\n' "$secret_token" > "$token_file"
chmod 644 "$token_file"

cleanup() {
  local exit_code=$?
  trap - EXIT INT TERM
  if [[ $exit_code -ne 0 ]]; then
    echo "=== TTS-gateway smoke test FAILED (exit code: $exit_code) ===" >&2
    if docker inspect "$container_name" >/dev/null 2>&1; then
      echo "=== Container Status ===" >&2
      docker inspect "$container_name" --format='Status: {{.State.Status}}, Restarts: {{.RestartCount}}, ExitCode: {{.State.ExitCode}}, Error: {{.State.Error}}' >&2 || true
      echo "=== Container Logs (last 100 lines) ===" >&2
      docker logs --tail 100 "$container_name" >&2 || true
    fi
  fi
  if docker inspect "$container_name" >/dev/null 2>&1; then
    echo "Cleaning up container $container_name..."
    docker rm -f "$container_name" >/dev/null 2>&1 || true
  fi
  rm -rf "$temp_dir"
  exit "$exit_code"
}
trap cleanup EXIT INT TERM

echo "Starting isolated tts-gateway container ($container_name) from $image_ref..."
docker run -d \
  --name "$container_name" \
  --init \
  --user "1000:1000" \
  --cap-drop ALL \
  --security-opt "no-new-privileges:true" \
  -p "${TTS_PORT}:8081" \
  -v "$token_file:/run/secrets/tts_internal_token:ro" \
  -e "TTS_PORT=8081" \
  -e "TTS_REQUIRE_AUTH=true" \
  -e "TTS_INTERNAL_TOKEN_FILE=/run/secrets/tts_internal_token" \
  "$image_ref"

echo "Waiting for tts-gateway container to become ready..."
ready=0
for i in {1..30}; do
  if curl --silent --fail "http://${host}:${TTS_PORT}/health" >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 1
done

if [[ $ready -ne 1 ]]; then
  echo "Error: tts-gateway failed to respond on /health within 30s" >&2
  exit 1
fi

echo "Verifying unauthorized request returns 401..."
http_code="$(curl -s -o /dev/null -w "%{http_code}" "http://${host}:${TTS_PORT}/voices")"
if [[ "$http_code" != "401" ]]; then
  echo "Error: expected HTTP 401 without auth token, got $http_code" >&2
  exit 1
fi

echo "Verifying invalid token returns 401..."
http_code="$(curl -s -o /dev/null -w "%{http_code}" -H "X-Internal-TTS-Token: invalid" "http://${host}:${TTS_PORT}/voices")"
if [[ "$http_code" != "401" ]]; then
  echo "Error: expected HTTP 401 with invalid token, got $http_code" >&2
  exit 1
fi

echo "Verifying valid token returns 200..."
http_code="$(curl -s -o /dev/null -w "%{http_code}" -H "X-Internal-TTS-Token: ${secret_token}" "http://${host}:${TTS_PORT}/voices")"
if [[ "$http_code" != "200" ]]; then
  echo "Error: expected HTTP 200 with valid token, got $http_code" >&2
  exit 1
fi

echo "TTS-gateway smoke test PASSED successfully."
