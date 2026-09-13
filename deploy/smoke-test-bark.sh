#!/usr/bin/env bash
set -Eeuo pipefail

host="${BARK_HOST:-127.0.0.1}"
port="${BARK_PORT:-8080}"
base_url="http://${host}:${port}"
user="${BARK_USER:-}"
pass="${BARK_PASS:-}"

printf 'Running smoke tests on Bark server at %s...\n' "$base_url"

# 1. Ping check
printf '1. Checking /ping endpoint...\n'
ping_code="$(curl -s -o /dev/null -w '%{http_code}' "${base_url}/ping" || true)"
if [[ "$ping_code" != "200" && -n "$user" && -n "$pass" ]]; then
    ping_code="$(curl -s -o /dev/null -w '%{http_code}' -u "${user}:${pass}" "${base_url}/ping" || true)"
fi
if [[ "$ping_code" != "200" ]]; then
    printf 'FAIL: /ping returned %s (expected 200)\n' "$ping_code" >&2
    exit 1
fi
printf 'OK: /ping returned 200\n'

# 3. Unauthorized push check (if basic auth is enabled)
if [[ -n "$user" && -n "$pass" ]]; then
    printf '2. Checking unauthenticated /push rejection...\n'
    unauth_code="$(curl -s -o /dev/null -w '%{http_code}' -X POST "${base_url}/push" || true)"
    if [[ "$unauth_code" != "418" && "$unauth_code" != "401" ]]; then
        printf 'FAIL: unauthenticated /push returned %s (expected 418 or 401)\n' "$unauth_code" >&2
        exit 1
    fi
    printf 'OK: unauthenticated /push rejected with %s\n' "$unauth_code"

    # 3. Authenticated push with empty body -> should return 400 bad request (request bind failed)
    printf '3. Checking authenticated /push handling...\n'
    auth_code="$(curl -s -o /dev/null -w '%{http_code}' -u "${user}:${pass}" -H "Content-Type: application/json" -d '{}' -X POST "${base_url}/push" || true)"
    if [[ "$auth_code" != "400" && "$auth_code" != "200" ]]; then
        printf 'FAIL: authenticated /push returned unexpected status: %s\n' "$auth_code" >&2
        exit 1
    fi
    printf 'OK: authenticated /push handled correctly (status %s)\n' "$auth_code"
fi

printf 'All Bark smoke tests passed successfully!\n'
