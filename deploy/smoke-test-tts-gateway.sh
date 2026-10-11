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
  --read-only \
  --tmpfs /tmp:rw,noexec,nosuid,size=16m \
  -p "127.0.0.1:${TTS_PORT}:8081" \
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

echo "Verifying real fixed-voice Edge synthesis and downstream cancellation (external network required)..."
docker exec -i "$container_name" python - <<'PY'
import http.client
import json
from pathlib import Path
import socket
import sys
import time

TOKEN = Path("/run/secrets/tts_internal_token").read_text().strip()
VOICE = "vi-VN-NamMinhNeural"
CORE = "Đã nhận năm trăm nghìn đồng."
LIMIT = 4 * 1024 * 1024

def mpeg_frame(data):
    # Skip optional ID3v2 metadata and require a plausible MPEG audio frame.
    start = 0
    if data.startswith(b"ID3") and len(data) >= 10:
        if any(value & 128 for value in data[6:10]):
            return False
        size = sum(value << shift for value, shift in zip(data[6:10], (21, 14, 7, 0)))
        start = 10 + size + (10 if data[5] & 16 else 0)
    for index in range(start, min(len(data) - 3, start + 4096)):
        a, b, c = data[index:index + 3]
        if a == 255 and b & 224 == 224 and (b >> 3) & 3 != 1 and (b >> 1) & 3 != 0 and (c >> 4) not in (0, 15) and (c >> 2) & 3 != 3:
            return True
    return False

def request(path, text):
    connection = http.client.HTTPConnection("127.0.0.1", 8081, timeout=12)
    try:
        connection.request("POST", path, json.dumps({
            "text": text, "voice": VOICE, "rate": "+25%", "pitch": "+0Hz",
            "provider_mode": "EDGE_ONLY", "cacheable": False, "timeout_ms": 8000,
        }, ensure_ascii=False).encode(), {
            "Content-Type": "application/json", "X-Internal-TTS-Token": TOKEN,
        })
        response = connection.getresponse()
        if response.status == 502:
            raise RuntimeError("EXTERNAL_FAILURE: fixed Edge synthesis unavailable (HTTP 502); no fallback accepted")
        if response.status == 504:
            raise RuntimeError("TTS_TIMEOUT: total synthesis budget exhausted (admission/upstream); no fallback accepted")
        if response.status == 503:
            raise AssertionError("TTS_OVERLOADED: isolated smoke admission unavailable after disconnect")
        if response.status != 200:
            raise AssertionError(f"Synthesis HTTP {response.status}")
        assert response.getheader("Content-Type", "").split(";")[0] == "audio/mpeg", "Not MPEG content type"
        assert response.getheader("X-TTS-Provider") == "edge", "Unexpected provider/fallback"
        assert response.getheader("X-TTS-Voice") == VOICE, "Effective voice mismatch"
        assert response.getheader("X-TTS-Cached") == "false", "Smoke must exercise real synthesis"
        assert response.getheader("X-TTS-Fallback") == "false", "Fallback not permitted"
        return connection, response
    except BaseException:
        connection.close()
        raise

def buffered():
    connection, response = request("/synthesize", CORE)
    try:
        data = response.read(LIMIT + 1)
        assert 0 < len(data) <= LIMIT, "Audio empty/over limit"
        assert mpeg_frame(data), "Response is not MPEG audio"
    finally:
        response.close()
        connection.close()

try:
    buffered()
    # Three uncached streams exceed the active-slot cap if disconnects leak slots.
    # Close immediately after actual audio, never persist or publish synthesized bytes.
    for index in range(3):
        connection, response = request("/synthesize/stream", (CORE + " Cảm ơn bạn đã ghé quầy.") * (8 + index))
        try:
            data = response.read(512)
            assert mpeg_frame(data), "Cancelled stream did not start with MPEG audio"
            if connection.sock is not None:
                connection.sock.shutdown(socket.SHUT_RDWR)
        finally:
            response.close()
            connection.close()
        time.sleep(0.1)
    buffered()
except (OSError, TimeoutError, http.client.HTTPException) as error:
    print(f"EXTERNAL_FAILURE: Edge/network stream interrupted: {type(error).__name__}; no fallback accepted", file=sys.stderr)
    sys.exit(2)
except RuntimeError as error:
    print(str(error), file=sys.stderr)
    sys.exit(2)
except AssertionError as error:
    print(f"SYNTHESIS_CONTRACT_FAILURE: {error}", file=sys.stderr)
    sys.exit(1)
print("Real Nam Minh 500,000 VND MPEG/header checks passed; 3 downstream disconnects followed by successful synthesis.")
PY

echo "TTS-gateway smoke test PASSED successfully."
