#!/usr/bin/env bash
set -euo pipefail

CURL_IMAGE='curlimages/curl:8.12.1'
valid_sha() { [[ "$1" =~ ^[0-9a-f]{40}$ ]]; }
valid_slot() { [[ "$1" == blue || "$1" == green ]]; }
fail() { printf 'healthcheck: %s\n' "$*" >&2; exit 1; }
curl_edge() {
  docker run --rm --read-only --cap-drop ALL --security-opt no-new-privileges \
    --network "${1}" "$CURL_IMAGE" -sS --connect-timeout 2 --max-time 5 "${@:2}"
}
# Response header names are case insensitive; avoid logging response bodies or secrets.
gateway_identity() {
  local response="$1" slot="$2" sha="$3" line key value status
  status="${response##*$'\n'}"
  [[ "$status" == 200 ]] || return 1
  local found_slot='' found_sha=''
  while IFS= read -r line; do
    line="${line%$'\r'}"
    [[ "$line" == *:* ]] || continue
    key="${line%%:*}"; value="${line#*:}"; value="${value# }"
    case "${key,,}" in
      x-platform-slot) found_slot="$value" ;;
      x-release-commit) found_sha="$value" ;;
    esac
  done <<< "$response"
  [[ "$found_slot" == "$slot" && "$found_sha" == "$sha" ]]
}

case "${1:-}" in
  route)
    [[ $# == 3 ]] || fail 'usage: route <gateway-slot> <gateway-sha>'
    valid_slot "$2" && valid_sha "$3" || fail 'invalid slot or SHA'
    deadline=$((SECONDS + 60))
    while :; do
      gateway=''
      gateway="$(curl_edge container:edge-traefik -H 'Host: gateway-deploy.acb.internal.invalid' -D - -o /dev/null -w $'\n%{http_code}' 'http://127.0.0.1:18080/readyz' 2>/dev/null)" || :
      if gateway_identity "$gateway" "$2" "$3"; then
        printf 'route ACK: gateway=%s/%s\n' "$2" "$3"
        exit 0
      fi
      if (( SECONDS >= deadline )); then
        printf 'route ACK timeout: gateway status=%s slot/sha-matched=%s\n' \
          "${gateway##*$'\n'}" "$(gateway_identity "$gateway" "$2" "$3" && echo yes || echo no)" >&2
        exit 1
      fi
      sleep 1
    done
    ;;
  container)
    [[ $# == 3 && "$3" =~ ^[1-9][0-9]*$ ]] || fail 'usage: container <name> <positive-timeout-seconds>'
    [[ "$2" =~ ^acb-(worker|recovery-controller|auth-browser|tts-gateway|bark|gateway-(blue|green))$ ]] || fail 'unknown container'
    [[ "${EXPECTED_IMAGE_REF:-}" =~ @sha256:[0-9a-f]{64}$ ]] || fail 'EXPECTED_IMAGE_REF must be an immutable digest'
    expected_id="$(docker image inspect --format '{{.Id}}' "$EXPECTED_IMAGE_REF")" || fail 'expected image unavailable'
    case "$2" in
      acb-gateway-*)
        slot="${2##*-}"
        [[ "${EXPECTED_SLOT:-}" == "$slot" ]] && valid_sha "${EXPECTED_RELEASE_SHA:-}" || fail 'gateway requires EXPECTED_SLOT and EXPECTED_RELEASE_SHA'
        ;;
    esac
    deadline=$((SECONDS + $3))
    while :; do
      details="$(docker inspect --format '{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{else}}missing{{end}} {{.Image}}' "$2" 2>/dev/null)" || details='missing'
      if [[ "$details" == "running healthy $expected_id" ]]; then
        case "$2" in
          acb-gateway-*)
            result="$(curl_edge edge-acb -H "Host: acb-web-${EXPECTED_SLOT}" -D - -o /dev/null -w $'\n%{http_code}' "http://acb-web-${EXPECTED_SLOT}:8090/readyz" 2>/dev/null)" || result=''
            gateway_identity "$result" "$EXPECTED_SLOT" "$EXPECTED_RELEASE_SHA" && exit 0
            ;;
          acb-worker) docker exec acb-worker /worker --readiness-check >/dev/null && exit 0 ;;
          acb-recovery-controller) docker exec acb-recovery-controller /recovery-controller --readiness-check >/dev/null && exit 0 ;;
          *) exit 0 ;;
        esac
      fi
      if (( SECONDS >= deadline )); then
        printf 'container timeout: %s status=%s image-matched=%s identity-matched=no\n' "$2" "${details%% *}" "$([[ "$details" == *" $expected_id" ]] && echo yes || echo no)" >&2
        exit 1
      fi
      sleep 1
    done
    ;;
  *) fail 'usage: healthcheck.sh route|container ...' ;;
esac
