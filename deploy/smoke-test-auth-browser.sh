#!/usr/bin/env bash
set -Eeuo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
image_ref="${1:-${AUTH_BROWSER_IMAGE:-ghcr.io/thedemontuan/acb-transaction-webhook-auth-browser:latest}}"
container_name="${CONTAINER_NAME:-auth-browser-smoke-$$-${RANDOM}}"

# Ports exposed on localhost for testing (configurable)
AUTH_BROWSER_PORT="${AUTH_BROWSER_PORT:-8182}"
AUTH_BROWSER_VNC_PORT="${AUTH_BROWSER_VNC_PORT:-6082}"
host="127.0.0.1"
internal_token='auth-browser-smoke-test-token'
wrong_token='auth-browser-smoke-wrong-token'
negative_container_name="${container_name}-missing-token"

if ! command -v docker >/dev/null 2>&1; then
  echo "Error: Docker CLI is required for the auth-browser smoke test." >&2
  exit 1
fi

if ! docker info >/dev/null 2>&1; then
  echo "Error: Docker daemon is required for the auth-browser smoke test." >&2
  exit 1
fi

seccomp_profile="$script_dir/seccomp-auth-browser.json"
[[ -f "$seccomp_profile" ]] || { echo "Error: Missing Chromium seccomp profile: $seccomp_profile" >&2; exit 1; }

# shellcheck disable=SC2329 # invoked through EXIT/INT/TERM traps
cleanup() {
  local exit_code=$?
  trap - EXIT INT TERM
  if [[ $exit_code -ne 0 ]]; then
    echo "=== Auth-browser smoke test FAILED (exit code: $exit_code) ===" >&2
    if docker inspect "$container_name" >/dev/null 2>&1; then
      echo "=== Container Status ===" >&2
      docker inspect "$container_name" --format='Status: {{.State.Status}}, Restarts: {{.RestartCount}}, ExitCode: {{.State.ExitCode}}, Error: {{.State.Error}}' >&2 || true
      echo "=== Container Logs (last 100 lines) ===" >&2
      docker logs --tail 100 "$container_name" >&2 || true
    fi
  fi
  for temporary_container in "$container_name" "$negative_container_name"; do
    if docker inspect "$temporary_container" >/dev/null 2>&1; then
      echo "Cleaning up container $temporary_container..."
      docker rm -f "$temporary_container" >/dev/null 2>&1 || true
    fi
  done
  exit "$exit_code"
}
trap cleanup EXIT INT TERM

echo "Verifying production startup rejects missing internal token..."
negative_status=0
negative_output=$(timeout 30s docker run --rm \
  --name "$negative_container_name" \
  --entrypoint /auth-browser \
  -e APP_ENV=production \
  -e ACB_LOGIN_URL=about:blank \
  -e AUTH_BROWSER_INTERNAL_TOKEN= \
  -e AUTH_BROWSER_INTERNAL_TOKEN_FILE= \
  "$image_ref" 2>&1) || negative_status=$?
if [[ "$negative_status" -eq 124 ]]; then
  echo "Error: Missing-token binary startup timed out" >&2
  exit 1
fi
if [[ "$negative_status" -ne 1 ]]; then
  echo "Error: Missing-token binary startup returned unexpected exit code $negative_status" >&2
  exit 1
fi
if ! echo "$negative_output" | grep -Fq "AUTH_BROWSER_INTERNAL_TOKEN or AUTH_BROWSER_INTERNAL_TOKEN_FILE is required in production"; then
  echo "Error: Missing-token binary startup did not report the expected configuration error" >&2
  exit 1
fi
echo "Production missing-token binary startup rejected with the expected configuration error."

# Production-parity sandbox, security, and resource limits matching deploy/compose.prod.yaml.
echo "Starting isolated auth-browser container ($container_name) from $image_ref..."
docker run -d \
  --name "$container_name" \
  --init \
  --user "1000:1000" \
  --cap-drop ALL \
  --security-opt "no-new-privileges:true" \
  --security-opt "seccomp=$seccomp_profile" \
  -e APP_ENV=production \
  -e ACB_LOGIN_URL=about:blank \
  -e AUTH_BROWSER_INTERNAL_TOKEN="$internal_token" \
  -e HOME=/tmp \
  --tmpfs /tmp:rw,nosuid,nodev,size=1g,mode=1777 \
  --cpus "1.5" \
  --memory "1536m" \
  --pids-limit 300 \
  -p "${host}:${AUTH_BROWSER_PORT}:8181" \
  -p "${host}:${AUTH_BROWSER_VNC_PORT}:6080" \
  "$image_ref"

echo "Waiting for auth-browser to report healthy..."
ready=0
for _ in $(seq 1 30); do
  if curl --silent --fail "http://${host}:${AUTH_BROWSER_PORT}/healthz" >/dev/null 2>&1; then
    ready=1
    break
  fi
  if ! docker inspect --format='{{.State.Running}}' "$container_name" 2>/dev/null | grep -q "true"; then
    echo "Error: Container exited prematurely during startup" >&2
    exit 1
  fi
  sleep 1
done

if [[ $ready -ne 1 ]]; then
  echo "Error: Timed out waiting for http://${host}:${AUTH_BROWSER_PORT}/healthz" >&2
  exit 1
fi
for public_probe in healthz readyz; do
  probe_code=$(curl -s -o /dev/null -w "%{http_code}" "http://${host}:${AUTH_BROWSER_PORT}/${public_probe}" || true)
  if [[ "$probe_code" != "200" ]]; then
    echo "Error: public /${public_probe} returned HTTP $probe_code after desktop readiness" >&2
    exit 1
  fi
done
echo "Auth-browser HTTP controller and desktop are ready; public healthz/readyz returned 200."

echo "Verifying internal /auth-browser --healthcheck via docker exec..."
docker exec "$container_name" /auth-browser --healthcheck
echo "Internal healthcheck passed."

# Step 1: Verify noVNC HTTP and WebSocket transport
echo "Verifying noVNC HTTP static server on port ${AUTH_BROWSER_VNC_PORT}..."
http_vnc_code=$(curl -s -o /dev/null -w "%{http_code}" "http://${host}:${AUTH_BROWSER_VNC_PORT}/vnc.html" || true)
if [[ "$http_vnc_code" != "200" && "$http_vnc_code" != "304" ]]; then
  http_vnc_code=$(curl -s -o /dev/null -w "%{http_code}" "http://${host}:${AUTH_BROWSER_VNC_PORT}/" || true)
fi
if [[ "$http_vnc_code" != "200" && "$http_vnc_code" != "304" ]]; then
  echo "Error: noVNC HTTP server returned status $http_vnc_code on port ${AUTH_BROWSER_VNC_PORT}" >&2
  exit 1
fi
echo "noVNC HTTP server OK (status $http_vnc_code)."

echo "Verifying noVNC WebSocket upgrade handshake on /websockify..."
ws_handshake=$(curl -s -i -N \
  --max-time 5 \
  -H "Connection: Upgrade" \
  -H "Upgrade: websocket" \
  -H "Sec-WebSocket-Version: 13" \
  -H "Sec-WebSocket-Key: SGVsbG8sIHdvcmxkIQ==" \
  "http://${host}:${AUTH_BROWSER_VNC_PORT}/websockify" 2>&1 || true)

if echo "$ws_handshake" | grep -qi "101 Switching Protocols\|101 Web Socket"; then
  echo "noVNC WebSocket upgrade handshake succeeded (HTTP 101 Switching Protocols)."
else
  echo "Error: noVNC WebSocket upgrade handshake failed on /websockify (expected HTTP 101 Switching Protocols, got: $ws_handshake)" >&2
  exit 1
fi

# Step 2: Test internal authentication and POST session creation
attempt_id="smoke-test-$(date +%s)-$RANDOM"
echo "Verifying missing and wrong internal tokens cannot create a session..."
for invalid_token in missing wrong; do
  invalid_header=()
  if [[ "$invalid_token" == "wrong" ]]; then
    invalid_header=(-H "X-Auth-Browser-Internal-Token: $wrong_token")
  fi
  invalid_code=$(curl -s -o /dev/null -w "%{http_code}" -X POST \
    "${invalid_header[@]}" \
    -H "Content-Type: application/json" \
    -d '{"attemptId":"unauthorized-smoke"}' \
    "http://${host}:${AUTH_BROWSER_PORT}/sessions")
  if [[ "$invalid_code" != "401" ]]; then
    echo "Error: ${invalid_token} internal token POST /sessions returned HTTP $invalid_code (expected 401)" >&2
    exit 1
  fi
done
echo "Missing and wrong internal tokens rejected with HTTP 401."

echo "Creating login session via POST /sessions (attemptId: $attempt_id)..."

create_resp=$(curl -s -w "\n%{http_code}" -X POST \
  -H "X-Auth-Browser-Internal-Token: $internal_token" \
  -H "Content-Type: application/json" \
  -d "{\"attemptId\":\"$attempt_id\"}" \
  "http://${host}:${AUTH_BROWSER_PORT}/sessions")

http_code=$(echo "$create_resp" | tail -n1)
resp_body=$(echo "$create_resp" | sed '$d')

if [[ "$http_code" != "201" ]]; then
  echo "Error: POST /sessions failed with HTTP $http_code (body: $resp_body)" >&2
  exit 1
fi

if ! echo "$resp_body" | grep -q "\"attemptId\":\"$attempt_id\""; then
  echo "Error: Response body missing attemptId: $resp_body" >&2
  exit 1
fi

if ! echo "$resp_body" | grep -q '"status":"AWAITING_USER_LOGIN"'; then
  echo "Error: Session status is not AWAITING_USER_LOGIN: $resp_body" >&2
  exit 1
fi
echo "POST /sessions created session: $attempt_id (status: AWAITING_USER_LOGIN)."

# Step 3: Verify POST session survives observer cycles >= ~10s
echo "Verifying session survives observer cycles for at least 60s (sampling every 2s)..."
duration=60
start_time="$(date +%s)"
cycle=0

while (( $(date +%s) - start_time < duration )); do
  sleep 2
  cycle=$((cycle + 1))
  elapsed=$(( $(date +%s) - start_time ))

  status_resp=$(curl -s -w "\n%{http_code}" \
    -H "X-Auth-Browser-Internal-Token: $internal_token" \
    "http://${host}:${AUTH_BROWSER_PORT}/sessions/${attempt_id}/status")
  s_code=$(echo "$status_resp" | tail -n1)
  s_body=$(echo "$status_resp" | sed '$d')

  if [[ "$s_code" != "200" ]]; then
    echo "Error: Session status returned HTTP $s_code at cycle $cycle (${elapsed}s elapsed): $s_body" >&2
    exit 1
  fi

  if ! echo "$s_body" | grep -q '"status":"AWAITING_USER_LOGIN"'; then
    echo "Error: Session transitioned out of AWAITING_USER_LOGIN at cycle $cycle (${elapsed}s elapsed): $s_body" >&2
    exit 1
  fi

  # Confirm container has not crashed or restarted
  restarts=$(docker inspect --format='{{.RestartCount}}' "$container_name" 2>/dev/null || echo "0")
  if [[ "$restarts" -ne 0 ]]; then
    echo "Error: Container restarted during observation loop! Restarts: $restarts" >&2
    exit 1
  fi
  echo "  Cycle $cycle (${elapsed}s elapsed): session active and AWAITING_USER_LOGIN (restarts: 0)"
done
echo "Session successfully survived $cycle observer cycles over ${duration}s."

# Step 4: Verify status endpoint payload structure
echo "Verifying status payload structure..."
status_resp=$(curl -s -w "\n%{http_code}" \
  -H "X-Auth-Browser-Internal-Token: $internal_token" \
  "http://${host}:${AUTH_BROWSER_PORT}/sessions/${attempt_id}/status")
s_code=$(echo "$status_resp" | tail -n1)
s_body=$(echo "$status_resp" | sed '$d')

if [[ "$s_code" != "200" ]] || ! echo "$s_body" | grep -q '"screenUrl"' || ! echo "$s_body" | grep -q '"expiresAt"'; then
  echo "Error: Malformed status response: HTTP $s_code, body: $s_body" >&2
  exit 1
fi
echo "Status endpoint payload verified."

# Step 5: Test session cancel and start lifecycle
echo "Cancelling session $attempt_id via DELETE /sessions/${attempt_id}..."
del_code=$(curl -s -o /dev/null -w "%{http_code}" -X DELETE \
  -H "X-Auth-Browser-Internal-Token: $internal_token" \
  "http://${host}:${AUTH_BROWSER_PORT}/sessions/${attempt_id}")
if [[ "$del_code" != "204" ]]; then
  echo "Error: DELETE /sessions returned HTTP $del_code (expected 204)" >&2
  exit 1
fi

echo "Verifying cancelled session remains queryable with terminal status..."
after_del_resp=$(curl -s -w "\n%{http_code}" \
  -H "X-Auth-Browser-Internal-Token: $internal_token" \
  "http://${host}:${AUTH_BROWSER_PORT}/sessions/${attempt_id}/status")
after_del_code=$(echo "$after_del_resp" | tail -n1)
after_del_body=$(echo "$after_del_resp" | sed '$d')
if [[ "$after_del_code" != "200" ]] || ! echo "$after_del_body" | grep -q '"status":"CANCELLED"'; then
  echo "Error: Expected HTTP 200 CANCELLED after cancellation, got HTTP $after_del_code (body: $after_del_body)" >&2
  exit 1
fi
echo "Session cancellation confirmed with terminal status CANCELLED."

echo "Starting a new session after cancellation to verify clean reset..."
attempt_id_2="smoke-test-2-$(date +%s)-$RANDOM"
create_resp_2=$(curl -s -w "\n%{http_code}" -X POST \
  -H "X-Auth-Browser-Internal-Token: $internal_token" \
  -H "Content-Type: application/json" \
  -d "{\"attemptId\":\"$attempt_id_2\"}" \
  "http://${host}:${AUTH_BROWSER_PORT}/sessions")

http_code_2=$(echo "$create_resp_2" | tail -n1)
resp_body_2=$(echo "$create_resp_2" | sed '$d')

if [[ "$http_code_2" != "201" ]] || ! echo "$resp_body_2" | grep -q '"status":"AWAITING_USER_LOGIN"'; then
  echo "Error: Failed to start new session after cancel: HTTP $http_code_2 (body: $resp_body_2)" >&2
  exit 1
fi
echo "New session $attempt_id_2 started successfully."

# Clean up second session
curl -s -o /dev/null -X DELETE \
  -H "X-Auth-Browser-Internal-Token: $internal_token" \
  "http://${host}:${AUTH_BROWSER_PORT}/sessions/${attempt_id_2}" || true

# Step 6: Verify container stability
echo "Verifying final container stability..."
final_restarts=$(docker inspect --format='{{.RestartCount}}' "$container_name" 2>/dev/null || echo "0")
if [[ "$final_restarts" -ne 0 ]]; then
  echo "Error: Container had unexpected restarts during smoke test: $final_restarts" >&2
  exit 1
fi

echo "All auth-browser smoke test assertions passed successfully!"
exit 0
