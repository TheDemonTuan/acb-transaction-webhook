#!/usr/bin/env bash
set -Eeuo pipefail

image_ref="${1:-${TTS_GATEWAY_IMAGE:-ghcr.io/thedemontuan/acb-transaction-webhook-tts-gateway:latest}}"
container_name="${CONTAINER_NAME:-tts-gateway-smoke-$$-${RANDOM}}"

TTS_PORT="${TTS_PORT:-8183}"
TTS_CPUS="${TTS_CPUS:-2.0}"
TTS_MEMORY_LIMIT="${TTS_MEMORY_LIMIT:-4G}"
TTS_VIENEU_THREADS="${TTS_VIENEU_THREADS:-2}"
TTS_SMOKE_OFFLINE="${TTS_SMOKE_OFFLINE:-0}"

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
  -e "TTS_VIENEU_THREADS=$TTS_VIENEU_THREADS" \
  "$image_ref"

echo "Verifying VieNeu readiness, authentication, real decoded synthesis, and inference cancellation..."
# Both published-port and network-none containers use the same bounded loopback
# readiness/auth/inference checks inside the immutable runtime image.
docker exec -i "$container_name" python - <<'PY'
import http.client
import importlib.util
import io
import json
import math
import os
from pathlib import Path
import signal
from concurrent.futures import ThreadPoolExecutor
import sys
import time

import numpy as np
import soundfile as sf

for unused_module in ("gradio", "PIL"):
    assert importlib.util.find_spec(unused_module) is None, f"Unused UI/image runtime shipped: {unused_module}"

TOKEN = Path("/run/secrets/tts_internal_token").read_text().strip()
VOICE = "vi-VN-vieneu-v3-turbo-mai-anh-v1"
CORE = "Đã nhận năm trăm nghìn đồng."
LIMIT = 4 * 1024 * 1024
HEADERS = {"Content-Type": "application/json", "X-Internal-TTS-Token": TOKEN}


def health(timeout=2):
    connection = http.client.HTTPConnection("127.0.0.1", 8081, timeout=timeout)
    try:
        connection.request("GET", "/health")
        response = connection.getresponse()
        assert response.status == 200, f"Health HTTP {response.status}"
        data = json.loads(response.read(LIMIT + 1))
        assert set(data) == {"status", "primary", "vieneu"}, "Retired provider in health"
        assert data["primary"] == "vieneu", "Unexpected primary provider"
        assert data["vieneu"]["workerCount"] == 1, "Expected one VieNeu worker"
        return data
    finally:
        connection.close()


def wait_ready():
    deadline = time.monotonic() + 150
    last_error = "VieNeu unavailable"
    while time.monotonic() < deadline:
        try:
            data = health(timeout=min(2, deadline - time.monotonic()))
            if (data["status"] == "ok" and data["vieneu"]["available"] is True
                    and data["vieneu"]["activeWorkers"] == 0
                    and data["vieneu"]["idleWorkers"] == 1):
                return
            last_error = str(data)
        except (OSError, TimeoutError, http.client.HTTPException) as error:
            last_error = str(error)
        time.sleep(min(1, max(0, deadline - time.monotonic())))
    raise AssertionError(f"VieNeu not ready within 150s: {last_error}")


def payload(text=CORE, **changes):
    data = {
        "text": text, "voice": VOICE, "rate": "+0%", "pitch": "+0Hz",
        "provider_mode": "VIENEU_ONLY", "cacheable": False, "timeout_ms": 8000,
    }
    data.update(changes)
    return data


def read_eof(response):
    data = bytearray()
    while True:
        chunk = response.read(16384)
        if not chunk:
            return bytes(data)
        data.extend(chunk)
        assert len(data) <= LIMIT, "Audio over limit"


def decode_audio(data):
    assert 0 < len(data) <= LIMIT, "Audio empty/over limit"
    samples, sample_rate = sf.read(io.BytesIO(data), dtype="float32", always_2d=True)
    assert sample_rate == 24000, f"Unexpected sample rate {sample_rate}"
    assert samples.dtype == np.float32 and samples.ndim == 2 and samples.shape[1] == 1, "Expected float32 mono"
    duration = len(samples) / sample_rate
    assert math.isfinite(duration) and duration > 0, "Empty/invalid duration"
    assert np.isfinite(samples).all(), "Non-finite audio samples"
    energy = float(np.mean(np.square(samples, dtype=np.float64)))
    assert math.isfinite(energy) and energy > 1e-8, f"No speech energy: {energy}"


def audio_headers(response, cached="false"):
    assert response.getheader("Content-Type", "").split(";")[0] == "audio/mpeg", "Not MPEG content type"
    assert response.getheader("X-TTS-Provider") == "vieneu", "Unexpected provider"
    assert response.getheader("X-TTS-Voice") == VOICE, "Effective voice mismatch"
    assert response.getheader("X-TTS-Cached") == cached, "Unexpected cache result"
    assert response.getheader("X-TTS-Fallback") == "false", "Fallback not permitted"


def synthesize(path="/synthesize", data=None, cached="false", allow_timeout=False):
    connection = http.client.HTTPConnection("127.0.0.1", 8081, timeout=12)
    started = time.monotonic()
    try:
        connection.request("POST", path, json.dumps(data or payload(), ensure_ascii=False).encode(), HEADERS)
        response = connection.getresponse()
        body = read_eof(response)
        if allow_timeout and response.status == 504:
            assert json.loads(body)["detail"] == "TTS_TIMEOUT"
            assert time.monotonic() - started < 10, "Unbounded synthesis deadline"
            return response.status
        assert response.status == 200, f"Synthesis HTTP {response.status}: {body[:256]!r}"
        audio_headers(response, cached)
        decode_audio(body)
        return response.status
    finally:
        connection.close()


def auth_and_catalog():
    for token, expected_status in ((None, 401), ("invalid", 401), (TOKEN, 200)):
        connection = http.client.HTTPConnection("127.0.0.1", 8081, timeout=2)
        try:
            connection.request("GET", "/voices", headers={} if token is None else {"X-Internal-TTS-Token": token})
            response = connection.getresponse()
            body = response.read(LIMIT + 1)
            assert response.status == expected_status, f"Catalog auth HTTP {response.status}, expected {expected_status}"
            if expected_status == 200:
                voices = json.loads(body)
                assert len(voices) == 1 and voices[0]["id"] == VOICE and voices[0]["provider"] == "vieneu", "Unexpected voice catalog"
        finally:
            connection.close()
    for path in ("/synthesize", "/synthesize/stream"):
        for retired_mode in ("PIPER_ONLY", "EDGE_ONLY", "BROWSER_ONLY", "HYBRID"):
            connection = http.client.HTTPConnection("127.0.0.1", 8081, timeout=2)
            try:
                connection.request("POST", path, json.dumps(payload(provider_mode=retired_mode)).encode(), HEADERS)
                response = connection.getresponse()
                response.read(LIMIT + 1)
                assert response.status == 422, f"Retired mode {retired_mode} accepted by {path}: {response.status}"
            finally:
                connection.close()


def worker_pids():
    pids = set()
    for path in Path("/proc").glob("[0-9]*/cmdline"):
        try:
            if b"app.providers.vieneu_worker" in path.read_bytes():
                pids.add(int(path.parent.name))
        except (FileNotFoundError, PermissionError):
            pass
    return pids


try:
    wait_ready()
    auth_and_catalog()
    synthesize()
    synthesize("/synthesize/stream")
    # Pause the real worker before headers, prove the request owns inference,
    # then disconnect. The worker must be killed/reaped, not returned to the pool.
    # Retrying the exact cacheable payload must synthesize, never serve partial data.
    for index in range(3):
        wait_ready()
        before = worker_pids()
        assert len(before) == 1, f"Unexpected VieNeu workers: {before}"
        data = payload(CORE + " Cảm ơn quý khách. " + ("Một.", "Hai.", "Ba.")[index], cacheable=True)
        connection = http.client.HTTPConnection("127.0.0.1", 8081, timeout=12)
        try:
            for pid in before:
                os.kill(pid, signal.SIGSTOP)
            connection.request("POST", "/synthesize/stream", json.dumps(data, ensure_ascii=False).encode(), HEADERS)
            deadline = time.monotonic() + 3
            while time.monotonic() < deadline:
                if health()["vieneu"]["activeWorkers"] == 1:
                    break
                time.sleep(0.025)
            else:
                raise AssertionError("Disconnected request never entered VieNeu inference")
        finally:
            connection.close()
        deadline = time.monotonic() + 3
        while before & worker_pids() and time.monotonic() < deadline:
            time.sleep(0.025)
        assert not before & worker_pids(), "Disconnected VieNeu worker was not killed/reaped"
        assert len(worker_pids()) <= 1, "Unbounded inference workers"
        wait_ready()
        synthesize(data=data)
        synthesize(data=data, cached="true")
        synthesize()

    # Exercise the maximum text and two admitted callers under production caps.
    # A bounded 504 is truthful degraded service; OOM/provider crash is not.
    def long_request(index):
        text = ((CORE + " ") * 25)[:599] + str(index)
        return synthesize(data=payload(text), allow_timeout=True)

    with ThreadPoolExecutor(max_workers=2) as workers:
        print("Maximum-text two-slot resource smoke:", list(workers.map(long_request, range(2))))
    wait_ready()
    synthesize()
    synthesize("/synthesize/stream")
except (OSError, TimeoutError, http.client.HTTPException) as error:
    print(f"VIENEU_FAILURE: local stream interrupted: {type(error).__name__}; no fallback accepted", file=sys.stderr)
    sys.exit(2)
except AssertionError as error:
    print(f"SYNTHESIS_CONTRACT_FAILURE: {error}", file=sys.stderr)
    sys.exit(1)
print("Real fixed VieNeu buffered/stream EOF decoded to finite 24kHz mono speech; auth, retired-mode rejection, 3 inference cancellations, cache safety, and recovery passed.")
PY

echo "TTS-gateway smoke test PASSED successfully."
