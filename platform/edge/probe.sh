#!/usr/bin/env bash
# Pinned helper probe script for Traefik edge platform
# Reaches slot-probe inside edge-traefik namespace (127.0.0.1:18080)
# Reaches production acknowledgement via edge-cloudflared namespace (172.31.250.2 -> 172.31.250.4:8080)
# No host ports or allowlist bypass required.

set -euo pipefail

PINNED_CURL_IMAGE="curlimages/curl:8.12.1"

TARGET="slot-probe"
SLOT="blue"
SERVICE="acb"
CUSTOM_HOST=""
PATH_URL=""
EXPECTED_STATUS="200"
EXPECTED_SLOT=""
EXPECTED_COMMIT=""
EXPECTED_DIGEST=""
EXPECTED_HEADER=""
EXPECTED_BODY=""
TIMEOUT_SECONDS="5"
CHECK_RUNTIME="0"
DIRECT_MODE="0"
DRY_RUN="0"

usage() {
  cat <<EOF
Usage: $0 [options]

Target selection:
  --target <slot-probe|production>  Routing path (default: slot-probe)
  --slot <blue|green>               Candidate slot for slot-probe (default: blue)
  --service <acb|bark>              Service to probe in production (default: acb)
  --host <hostname>                 Explicit Host header override
  --path <path>                     HTTP request path (default: /readyz or /ping)

Verification:
  --expected-status <code>          Expected HTTP status code (default: 200)
  --expected-slot <blue|green>      Expected route slot (X-Platform-Slot header on HTTP 200)
  --expected-commit <sha/string>    Expected release commit (X-Release-Commit header on HTTP 200)
  --expected-digest <digest>        Expected release digest/SHA in headers or body
  --expected-header <Name:Value>    Expected response header
  --expected-body <string>          Expected substring in response body
  --timeout <seconds>               Request timeout in seconds (default: 5)

Preflight & execution mode:
  --check-runtime                   Validate namespace container and helper image upfront
  --direct                          Execute curl directly (inside container/namespace)
  --dry-run                         Simulate probe without network execution
  --help                            Display this help message
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --target)
      TARGET="${2:-}"
      shift 2
      ;;
    --slot)
      SLOT="${2:-}"
      shift 2
      ;;
    --service)
      SERVICE="${2:-}"
      shift 2
      ;;
    --host)
      CUSTOM_HOST="${2:-}"
      shift 2
      ;;
    --path)
      PATH_URL="${2:-}"
      shift 2
      ;;
    --expected-status)
      EXPECTED_STATUS="${2:-}"
      shift 2
      ;;
    --expected-slot)
      EXPECTED_SLOT="${2:-}"
      if [[ -z "$EXPECTED_SLOT" || "$EXPECTED_SLOT" == "unknown" ]]; then
        printf "ERROR: --expected-slot must not be empty or 'unknown'\n" >&2
        exit 1
      fi
      shift 2
      ;;
    --expected-commit)
      EXPECTED_COMMIT="${2:-}"
      if [[ -z "$EXPECTED_COMMIT" || "$EXPECTED_COMMIT" == "unknown" ]]; then
        printf "ERROR: --expected-commit must not be empty or 'unknown'\n" >&2
        exit 1
      fi
      shift 2
      ;;
    --expected-digest)
      EXPECTED_DIGEST="${2:-}"
      shift 2
      ;;
    --expected-header)
      EXPECTED_HEADER="${2:-}"
      shift 2
      ;;
    --expected-body)
      EXPECTED_BODY="${2:-}"
      shift 2
      ;;
    --timeout)
      TIMEOUT_SECONDS="${2:-}"
      shift 2
      ;;
    --check-runtime)
      CHECK_RUNTIME="1"
      shift
      ;;
    --direct)
      DIRECT_MODE="1"
      shift
      ;;
    --dry-run)
      DRY_RUN="1"
      shift
      ;;
    --help|-h)
      usage
      exit 0
      ;;
    *)
      printf "Unknown option: %s\n" "$1" >&2
      usage >&2
      exit 1
      ;;
  esac
done

# Resolve URL, host header, and network namespace
case "$TARGET" in
  slot-probe)
    CONTAINER_NS="edge-traefik"
    DEST_URL="http://127.0.0.1:18080"
    DEFAULT_HOST="acb-${SLOT}.internal.invalid"
    DEFAULT_PATH="/readyz"
    ;;
  production)
    CONTAINER_NS="edge-cloudflared"
    DEST_URL="http://172.31.250.4:8080"
    if [[ "$SERVICE" == "bark" ]]; then
      DEFAULT_HOST="bark.tuannguyenviet.site"
      DEFAULT_PATH="/ping"
    else
      DEFAULT_HOST="bank.tuannguyenviet.site"
      DEFAULT_PATH="/readyz"
    fi
    ;;
  *)
    printf "Invalid target: %s (must be slot-probe or production)\n" "$TARGET" >&2
    exit 1
    ;;
esac

PROBE_HOST="${CUSTOM_HOST:-$DEFAULT_HOST}"
PROBE_PATH="${PATH_URL:-$DEFAULT_PATH}"
FULL_URL="${DEST_URL}${PROBE_PATH}"

# Preflight runtime check
if [[ "$CHECK_RUNTIME" == "1" ]]; then
  if [[ "$DRY_RUN" == "1" ]]; then
    printf "DRY_RUN: Preflight runtime check validated for container_ns=%s image=%s\n" "$CONTAINER_NS" "$PINNED_CURL_IMAGE"
    exit 0
  fi

  if [[ "$DIRECT_MODE" == "1" ]]; then
    if ! command -v curl >/dev/null 2>&1; then
      printf "ERROR: curl binary not found in PATH for direct mode\n" >&2
      exit 1
    fi
    printf "Runtime check passed: curl binary available for direct mode\n"
    exit 0
  fi

  if ! command -v docker >/dev/null 2>&1; then
    printf "ERROR: docker command not found in PATH\n" >&2
    exit 1
  fi

  if ! docker info >/dev/null 2>&1; then
    printf "ERROR: Docker daemon is not accessible\n" >&2
    exit 1
  fi

  ns_running="$(docker inspect -f '{{.State.Running}}' "$CONTAINER_NS" 2>/dev/null || echo "false")"
  if [[ "$ns_running" != "true" ]]; then
    printf "ERROR: Target namespace container '%s' is not running\n" "$CONTAINER_NS" >&2
    exit 1
  fi

  if ! docker image inspect "$PINNED_CURL_IMAGE" >/dev/null 2>&1; then
    printf "ERROR: Helper image '%s' not found locally. Pull image before probing: docker pull %s\n" "$PINNED_CURL_IMAGE" "$PINNED_CURL_IMAGE" >&2
    exit 1
  fi

  printf "Runtime check passed: container '%s' is running, image '%s' is available\n" "$CONTAINER_NS" "$PINNED_CURL_IMAGE"
  exit 0
fi

printf "Probing target=%s container_ns=%s host=%s url=%s\n" \
  "$TARGET" "$CONTAINER_NS" "$PROBE_HOST" "$FULL_URL"

if [[ "$DRY_RUN" == "1" ]]; then
  printf "DRY_RUN: Probe validated parameters successfully.\n"
  if [[ -n "$EXPECTED_SLOT" ]]; then
    printf "DRY_RUN: Expecting slot: %s\n" "$EXPECTED_SLOT"
  fi
  if [[ -n "$EXPECTED_COMMIT" ]]; then
    printf "DRY_RUN: Expecting commit: %s\n" "$EXPECTED_COMMIT"
  fi
  if [[ -n "$EXPECTED_DIGEST" ]]; then
    printf "DRY_RUN: Expecting digest: %s\n" "$EXPECTED_DIGEST"
  fi
  exit 0
fi

# Prepare curl arguments
# We capture headers, body, and an unambiguous status sentinel from stdout.
# This eliminates host temp bind mounts, avoiding container SELinux and permission failures.
CURL_ARGS=(
  -sS
  -m "$TIMEOUT_SECONDS"
  -i
  -w $'\n__STATUS_SENTINEL__:%{http_code}\n'
  -H "Host: ${PROBE_HOST}"
  "$FULL_URL"
)

HTTP_RAW=""
HTTP_RC=0

if [[ "$DIRECT_MODE" == "1" ]]; then
  HTTP_RAW="$(curl "${CURL_ARGS[@]}" 2>&1)" || HTTP_RC=$?
else
  # Pinned curl container sharing the target network namespace
  # curlimages/curl has ENTRYPOINT ["curl"], so do not supply duplicate 'curl' argument
  HTTP_RAW="$(docker run --rm \
    --read-only \
    --cap-drop ALL \
    --security-opt no-new-privileges \
    --network "container:${CONTAINER_NS}" \
    "$PINNED_CURL_IMAGE" \
    "${CURL_ARGS[@]}" 2>&1)" || HTTP_RC=$?
fi

HTTP_CODE=""
if [[ "$HTTP_RAW" =~ __STATUS_SENTINEL__:([0-9]{3}) ]]; then
  HTTP_CODE="${BASH_REMATCH[1]}"
fi

printf "HTTP Status: %s (expected: %s)\n" "${HTTP_CODE:-none}" "$EXPECTED_STATUS"

if [[ -z "$HTTP_CODE" || "$HTTP_CODE" != "$EXPECTED_STATUS" ]]; then
  printf "ERROR: Status mismatch: got %s, expected %s (rc=%d)\n" "${HTTP_CODE:-none}" "$EXPECTED_STATUS" "$HTTP_RC" >&2
  if [[ -n "$HTTP_RAW" ]]; then
    printf "Response / output:\n%s\n" "$HTTP_RAW" >&2
  fi
  exit 1
fi

CLEAN_OUTPUT="${HTTP_RAW%%__STATUS_SENTINEL__:*}"

HEADERS=""
BODY=""
is_header=1
while IFS= read -r line || [[ -n "$line" ]]; do
  clean_line="$(printf '%s' "$line" | tr -d '\r')"
  if [[ "$is_header" -eq 1 ]]; then
    if [[ -z "$clean_line" ]]; then
      is_header=0
      continue
    fi
    HEADERS+="${clean_line}"$'\n'
  else
    if [[ "$clean_line" =~ ^HTTP/[12] ]]; then
      HEADERS="${clean_line}"$'\n'
      BODY=""
      is_header=1
      continue
    fi
    BODY+="${line}"$'\n'
  fi
done <<< "$CLEAN_OUTPUT"

# Host-only temporary files for regex/grep compatibility without container bind mounts
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

HEADERS_FILE="${TMP_DIR}/headers.txt"
BODY_FILE="${TMP_DIR}/body.txt"

printf '%s' "$HEADERS" > "$HEADERS_FILE"
printf '%s' "$BODY" > "$BODY_FILE"

# Strict route identity verification on HTTP 200 response
if [[ -n "$EXPECTED_SLOT" || -n "$EXPECTED_COMMIT" ]]; then
  if [[ "$HTTP_CODE" != "200" ]]; then
    printf "ERROR: Route identity ACK requires HTTP status 200, got %s\n" "$HTTP_CODE" >&2
    exit 1
  fi
fi

if [[ -n "$EXPECTED_SLOT" ]]; then
  slot_header="$(printf '%s\n' "$HEADERS" | grep -i '^x-platform-slot:' | head -n1 | tr -d '\r\n' | sed -e 's/^[^:]*:[[:space:]]*//' -e 's/[[:space:]]*$//' || echo "")"
  if [[ -z "$slot_header" || "$slot_header" == "unknown" ]]; then
    printf "ERROR: Route identity ACK missing or unknown X-Platform-Slot header in response\n" >&2
    exit 1
  fi
  if [[ "$slot_header" != "$EXPECTED_SLOT" ]]; then
    printf "ERROR: Route identity ACK slot mismatch: expected '%s', got '%s'\n" "$EXPECTED_SLOT" "$slot_header" >&2
    exit 1
  fi
  printf "Verified route identity slot: %s\n" "$slot_header"
fi

if [[ -n "$EXPECTED_COMMIT" ]]; then
  commit_header="$(printf '%s\n' "$HEADERS" | grep -i '^x-release-commit:' | head -n1 | tr -d '\r\n' | sed -e 's/^[^:]*:[[:space:]]*//' -e 's/[[:space:]]*$//' || echo "")"
  if [[ -z "$commit_header" || "$commit_header" == "unknown" ]]; then
    printf "ERROR: Route identity ACK missing or unknown X-Release-Commit header in response\n" >&2
    exit 1
  fi
  if [[ "$commit_header" != "$EXPECTED_COMMIT" ]]; then
    printf "ERROR: Route identity ACK commit mismatch: expected '%s', got '%s'\n" "$EXPECTED_COMMIT" "$commit_header" >&2
    exit 1
  fi
  printf "Verified route identity commit: %s\n" "$commit_header"
fi

# Verify expected header if requested
if [[ -n "$EXPECTED_HEADER" ]]; then
  HEADER_KEY="${EXPECTED_HEADER%%:*}"
  HEADER_VAL="${EXPECTED_HEADER#*:}"
  HEADER_VAL="$(printf '%s' "$HEADER_VAL" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')"
  if ! grep -i -q "^${HEADER_KEY}:.*${HEADER_VAL}" "$HEADERS_FILE"; then
    printf "ERROR: Missing or mismatched header %s in response\n" "$EXPECTED_HEADER" >&2
    printf "Received headers:\n" >&2
    cat "$HEADERS_FILE" >&2
    exit 1
  fi
  printf "Verified expected header: %s\n" "$EXPECTED_HEADER"
fi

# Verify expected body substring if requested
if [[ -n "$EXPECTED_BODY" ]]; then
  if ! grep -q "$EXPECTED_BODY" "$BODY_FILE"; then
    printf "ERROR: Response body does not contain expected pattern: %s\n" "$EXPECTED_BODY" >&2
    printf "Received body:\n" >&2
    cat "$BODY_FILE" >&2
    printf "\n" >&2
    exit 1
  fi
  printf "Verified expected body pattern: %s\n" "$EXPECTED_BODY"
fi

# Verify expected digest (commit SHA or release digest)
if [[ -n "$EXPECTED_DIGEST" ]]; then
  DIGEST_FOUND=0

  # Check identity headers
  if grep -i -E "^(x-release-digest|x-release-id|x-commit-sha|x-app-digest|x-release-commit):.*${EXPECTED_DIGEST}" "$HEADERS_FILE" >/dev/null 2>&1; then
    DIGEST_FOUND=1
  fi

  # Check JSON body fields or content
  if grep -i -E "(\"release\"|\"digest\"|\"commit\"|\"sha\")[[:space:]]*:[[:space:]]*\"[^\"]*${EXPECTED_DIGEST}[^\"]*\"" "$BODY_FILE" >/dev/null 2>&1; then
    DIGEST_FOUND=1
  elif grep -q "$EXPECTED_DIGEST" "$BODY_FILE"; then
    DIGEST_FOUND=1
  fi

  if [[ "$DIGEST_FOUND" != "1" ]]; then
    printf "ERROR: Expected digest %s not found in headers or response body\n" "$EXPECTED_DIGEST" >&2
    printf "Headers:\n" >&2
    cat "$HEADERS_FILE" >&2
    printf "Body:\n" >&2
    cat "$BODY_FILE" >&2
    printf "\n" >&2
    exit 1
  fi
  printf "Verified endpoint identity digest: %s\n" "$EXPECTED_DIGEST"
fi

printf "Probe SUCCESS for target=%s host=%s path=%s\n" "$TARGET" "$PROBE_HOST" "$PROBE_PATH"
exit 0
