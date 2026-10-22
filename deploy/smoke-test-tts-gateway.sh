#!/usr/bin/env bash
set -Eeuo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
image_ref="${1:-${TTS_GATEWAY_IMAGE:-ghcr.io/thedemontuan/acb-transaction-webhook-tts-gateway:latest}}"
container_name="${CONTAINER_NAME:-tts-gateway-smoke-$$-${RANDOM}}"

TTS_PORT="${TTS_PORT:-8183}"
TTS_CPUS="${TTS_CPUS:-2.0}"
TTS_MEMORY_LIMIT="${TTS_MEMORY_LIMIT:-4G}"
TTS_VIENEU_THREADS="${TTS_VIENEU_THREADS:-2}"
TTS_SMOKE_OFFLINE="${TTS_SMOKE_OFFLINE:-0}"
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

echo "Starting isolated tts-gateway container ($container_name) from $image_ref (cpus: $TTS_CPUS, mem: $TTS_MEMORY_LIMIT, offline: $TTS_SMOKE_OFFLINE)..."
network_args=()
if [[ "$TTS_SMOKE_OFFLINE" == "1" ]]; then
  network_args=(--network none)
else
  network_args=(-p "127.0.0.1:${TTS_PORT}:8081")
fi
docker run -d \
  --name "$container_name" \
  --init \
  --user "1000:1000" \
  --cap-drop ALL \
  --security-opt "no-new-privileges:true" \
  --read-only \
  --cpus "$TTS_CPUS" \
  --memory "$TTS_MEMORY_LIMIT" \
  --pids-limit 100 \
  --tmpfs /tmp:rw,noexec,nosuid,size=64m \
  "${network_args[@]}" \
  -v "$token_file:/run/secrets/tts_internal_token:ro" \
  -e "TTS_PORT=8081" \
  -e "TTS_REQUIRE_AUTH=true" \
  -e "TTS_INTERNAL_TOKEN_FILE=/run/secrets/tts_internal_token" \
  -e "PIPER_WORKERS=${PIPER_WORKERS:-1}" \
  -e "TTS_VIENEU_THREADS=$TTS_VIENEU_THREADS" \
  "$image_ref"

if [[ "$TTS_SMOKE_OFFLINE" != "1" ]]; then

echo "Waiting for tts-gateway container to become ready..."
ready=0
for i in {1..60}; do
  if curl --silent --fail "http://${host}:${TTS_PORT}/health" >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 1
done

if [[ $ready -ne 1 ]]; then
  echo "Error: tts-gateway failed to respond on /health within 60s" >&2
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
fi

echo "Verifying real fixed Piper and VieNeu synthesis, streaming, and inference cancellation..."
docker exec -i "$container_name" python - <<'PY'
import http.client
import importlib.util
import json
from pathlib import Path
import os
import signal
from concurrent.futures import ThreadPoolExecutor
import sys
import time

for unused_module in ("gradio", "PIL"):
    assert importlib.util.find_spec(unused_module) is None, f"Unused UI/image runtime shipped: {unused_module}"

TOKEN = Path("/run/secrets/tts_internal_token").read_text().strip()
PIPER_VOICE = "vi-VN-vais1000-medium"
VIENEU_VOICE = "vi-VN-vieneu-v3-turbo-mai-anh-v1"
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
def vieneu_test():
    connection = http.client.HTTPConnection("127.0.0.1", 8081, timeout=12)
    try:
        connection.request("POST", "/synthesize", json.dumps({
            "text": "Đã nhận một trăm linh năm nghìn đồng.",
            "voice": VIENEU_VOICE, "provider_mode": "VIENEU_ONLY",
            "rate": "+0%", "pitch": "+0Hz", "cacheable": False, "timeout_ms": 8000,
        }).encode(), {
            "Content-Type": "application/json", "X-Internal-TTS-Token": TOKEN,
        })
        response = connection.getresponse()
        data = response.read(LIMIT + 1)
        assert response.status == 200, f"VieNeu status {response.status}"
        assert response.getheader("X-TTS-Provider") == "vieneu"
        assert response.getheader("X-TTS-Voice") == VIENEU_VOICE
        assert mpeg_frame(data) and len(data) <= LIMIT
    finally:
        connection.close()

vieneu_test()

def request(path, text):
    connection = http.client.HTTPConnection("127.0.0.1", 8081, timeout=12)
    try:
        connection.request("POST", path, json.dumps({
            "text": text, "voice": PIPER_VOICE, "rate": "+25%", "pitch": "+0Hz",
            "provider_mode": "PIPER_ONLY", "cacheable": False, "timeout_ms": 8000,
        }, ensure_ascii=False).encode(), {
            "Content-Type": "application/json", "X-Internal-TTS-Token": TOKEN,
        })
        response = connection.getresponse()
        if response.status == 502:
            raise RuntimeError("PIPER_UNAVAILABLE: fixed local synthesis failed; no fallback accepted")
        if response.status == 504:
            raise RuntimeError("TTS_TIMEOUT: total synthesis budget exhausted (admission/upstream); no fallback accepted")
        if response.status == 503:
            raise AssertionError("TTS_OVERLOADED: isolated smoke admission unavailable after disconnect")
        if response.status != 200:
            raise AssertionError(f"Synthesis HTTP {response.status}")
        assert response.getheader("Content-Type", "").split(";")[0] == "audio/mpeg", "Not MPEG content type"
        assert response.getheader("X-TTS-Provider") == "piper", "Unexpected provider/fallback"
        assert response.getheader("X-TTS-Voice") == PIPER_VOICE, "Effective voice mismatch"
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

def worker_pids():
    pids = set()
    for path in Path("/proc").glob("[0-9]*/cmdline"):
        try:
            if b"app.providers.piper_worker" in path.read_bytes():
                pids.add(path.parent.name)
        except (FileNotFoundError, PermissionError):
            pass
    return pids

try:
    health_connection = http.client.HTTPConnection("127.0.0.1", 8081, timeout=3)
    health_connection.request("GET", "/health")
    health_data = json.loads(health_connection.getresponse().read())
    health_connection.close()
    expected_workers = int(os.environ.get("PIPER_WORKERS", "1"))
    assert health_data["primary"] == "vieneu" and health_data["vieneu"]["available"], "VieNeu not ready"
    assert health_data["piper"]["available"], "Piper not ready"
    assert health_data["piper"]["workerCount"] == expected_workers, f"Expected {expected_workers} workers, got {health_data['piper'].get('workerCount')}"
    buffered()
    connection, response = request("/synthesize/stream", CORE)
    assert mpeg_frame(response.read(LIMIT + 1)), "Stream is not real MPEG"
    response.close()
    connection.close()
    # Disconnect BEFORE headers while real inference is running. Reaped worker
    # PIDs, followed by real successful synthesis, prove cancellation ownership.
    for index in range(3):
        before = worker_pids()
        for pid in before:
            os.kill(int(pid), signal.SIGSTOP)
        connection = http.client.HTTPConnection("127.0.0.1", 8081, timeout=12)
        connection.request("POST", "/synthesize/stream", json.dumps({
            "text": (CORE + " Cảm ơn bạn đã ghé quầy. ") * 8 + str(index),
            "voice": PIPER_VOICE, "provider_mode": "PIPER_ONLY", "cacheable": False,
            "rate": "+25%", "pitch": "+0Hz", "timeout_ms": 8000,
        }, ensure_ascii=False).encode(), {
            "Content-Type": "application/json", "X-Internal-TTS-Token": TOKEN,
        })
        time.sleep(0.05)
        connection.close()
        deadline = time.monotonic() + 3
        while before <= worker_pids() and time.monotonic() < deadline:
            time.sleep(0.025)
        assert before - worker_pids(), "Disconnected inference worker was not killed/reaped"
        for pid in worker_pids():
            os.kill(int(pid), signal.SIGCONT)
        assert len(worker_pids()) <= expected_workers, "Unbounded inference workers"
        buffered()
        buffered()
    # Exercise maximum accepted text on both slots under production caps.
    # A bounded 504 is truthful degraded service; OOM/provider crash is not.
    def long_request(index):
        connection = http.client.HTTPConnection("127.0.0.1", 8081, timeout=12)
        try:
            text = ((CORE + " ") * 25)[:599] + str(index)
            connection.request("POST", "/synthesize", json.dumps({
                "text": text, "voice": PIPER_VOICE, "provider_mode": "PIPER_ONLY",
                "rate": "+25%", "cacheable": False, "timeout_ms": 8000,
            }, ensure_ascii=False).encode(), {
                "Content-Type": "application/json", "X-Internal-TTS-Token": TOKEN,
            })
            response = connection.getresponse()
            data = response.read(LIMIT + 1)
            assert response.status in (200, 504), f"Maximum-text request failed HTTP {response.status}"
            if response.status == 200:
                assert mpeg_frame(data) and len(data) <= LIMIT
            else:
                assert json.loads(data)["detail"] == "TTS_TIMEOUT"
            return response.status
        finally:
            connection.close()
    with ThreadPoolExecutor(max_workers=2) as workers:
        print("Maximum-text two-slot resource smoke:", list(workers.map(long_request, range(2))))
    buffered()
    buffered()
except (OSError, TimeoutError, http.client.HTTPException) as error:
    print(f"PIPER_FAILURE: local stream interrupted: {type(error).__name__}; no fallback accepted", file=sys.stderr)
    sys.exit(2)
except RuntimeError as error:
    print(str(error), file=sys.stderr)
    sys.exit(2)
except AssertionError as error:
    print(f"SYNTHESIS_CONTRACT_FAILURE: {error}", file=sys.stderr)
    sys.exit(1)
print("Real fixed VieNeu synthesis and Piper 500,000 VND buffered/stream MPEG headers passed; 3 in-flight disconnects reaped Piper workers and subsequent synthesis succeeded.")
PY

echo "TTS-gateway smoke test PASSED successfully."
