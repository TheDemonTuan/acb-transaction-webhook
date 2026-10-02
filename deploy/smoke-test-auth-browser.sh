#!/usr/bin/env bash
set -Eeuo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
image_ref="${1:-${AUTH_BROWSER_IMAGE:-ghcr.io/thedemontuan/acb-transaction-webhook-auth-browser:latest}}"
container_name="${CONTAINER_NAME:-auth-browser-smoke-$$-${RANDOM}}"

# Ports exposed on localhost for testing (configurable)
AUTH_BROWSER_PORT="${AUTH_BROWSER_PORT:-8182}"
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
  -e AUTH_BROWSER_INTERNAL_TOKEN="$internal_token" \
  -e HOME=/tmp \
  --tmpfs /tmp:rw,nosuid,nodev,size=1g,mode=1777 \
  --cpus "1.5" \
  --memory "1536m" \
  --pids-limit 300 \
  -p "${host}:${AUTH_BROWSER_PORT}:8181" \
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

# Verify private RPCs reject missing/wrong tokens without starting a browser or touching ACB.
echo "Verifying internal authentication on session, automation, and revocation RPCs..."
for invalid_token in missing wrong; do
  invalid_header=()
  if [[ "$invalid_token" == "wrong" ]]; then
    invalid_header=(-H "X-Auth-Browser-Internal-Token: $wrong_token")
  fi
  for rpc in \
    "POST /sessions" \
    "GET /sessions/idle-smoke/status" \
    "DELETE /sessions/idle-smoke" \
    "POST /sessions/idle-smoke/handoff" \
    "POST /sessions/idle-smoke/complete" \
    "GET /sessions/idle-smoke/observation" \
    "GET /sessions/idle-smoke/captcha" \
    "POST /sessions/idle-smoke/login" \
    "POST /sessions/idle-smoke/captcha" \
    "POST /sessions/idle-smoke/otp" \
    "POST /session-revocations"; do
    method="${rpc%% *}"
    path="${rpc#* }"
    code=$(curl --silent --show-error --max-time 5 -o /dev/null -w "%{http_code}" \
      -X "$method" "${invalid_header[@]}" \
      "http://${host}:${AUTH_BROWSER_PORT}${path}")
    if [[ "$code" != "401" ]]; then
      echo "Error: ${invalid_token} internal token ${rpc} returned HTTP $code (expected 401)" >&2
      exit 1
    fi
  done
done

# Authorized reads of a nonexistent attempt must stay read-only. Login/OTP/logout behavior
# is exercised against local fixtures by the real-Chromium integration suite, never ACB here.
echo "Verifying an idle controller has no session and rejects malformed mutation requests..."
status_code=$(curl --silent --show-error --max-time 5 -o /dev/null -w "%{http_code}" \
  -H "X-Auth-Browser-Internal-Token: $internal_token" \
  "http://${host}:${AUTH_BROWSER_PORT}/sessions/idle-smoke/status")
if [[ "$status_code" != "404" ]]; then
  echo "Error: Idle session status returned HTTP $status_code (expected 404)" >&2
  exit 1
fi
for path in /sessions /session-revocations; do
  code=$(curl --silent --show-error --max-time 5 -o /dev/null -w "%{http_code}" -X POST \
    -H "X-Auth-Browser-Internal-Token: $internal_token" \
    -H "Content-Type: application/json" -d '{}' \
    "http://${host}:${AUTH_BROWSER_PORT}${path}")
  if [[ "$code" != "400" ]]; then
    echo "Error: Malformed ${path} request returned HTTP $code (expected 400)" >&2
    exit 1
  fi
done
for public_probe in healthz readyz; do
  curl --silent --show-error --fail --max-time 5 \
    "http://${host}:${AUTH_BROWSER_PORT}/${public_probe}?role=auth-browser" >/dev/null
done

# Verify container stability without starting a login session.
echo "Verifying final container stability..."
final_restarts=$(docker inspect --format='{{.RestartCount}}' "$container_name" 2>/dev/null || echo "0")
if [[ "$final_restarts" -ne 0 ]]; then
  echo "Error: Container had unexpected restarts during smoke test: $final_restarts" >&2
  exit 1
fi

echo "All auth-browser smoke test assertions passed successfully!"
exit 0
