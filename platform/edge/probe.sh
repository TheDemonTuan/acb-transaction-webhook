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
EXPECTED_DIGEST=""
EXPECTED_HEADER=""
EXPECTED_BODY=""
TIMEOUT_SECONDS="5"
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
  --expected-digest <digest>        Expected release digest/SHA in headers or body
  --expected-header <Name:Value>    Expected response header
  --expected-body <string>          Expected substring in response body
  --timeout <seconds>               Request timeout in seconds (default: 5)

Execution mode:
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

printf "Probing target=%s container_ns=%s host=%s url=%s\n" \
  "$TARGET" "$CONTAINER_NS" "$PROBE_HOST" "$FULL_URL"

if [[ "$DRY_RUN" == "1" ]]; then
  printf "DRY_RUN: Probe validated parameters successfully.\n"
  if [[ -n "$EXPECTED_DIGEST" ]]; then
    printf "DRY_RUN: Expecting digest: %s\n" "$EXPECTED_DIGEST"
  fi
  exit 0
fi

# Prepare temporary files for capturing response headers and body
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

HEADERS_FILE="${TMP_DIR}/headers.txt"
BODY_FILE="${TMP_DIR}/body.txt"

CURL_CMD=(
  curl -sS -m "$TIMEOUT_SECONDS"
  -D "$HEADERS_FILE"
  -o "$BODY_FILE"
  -w "%{http_code}"
  -H "Host: ${PROBE_HOST}"
  "$FULL_URL"
)

HTTP_CODE=""
if [[ "$DIRECT_MODE" == "1" ]]; then
  HTTP_CODE="$("${CURL_CMD[@]}")"
else
  # Run via pinned curl container sharing the target network namespace
  HTTP_CODE="$(docker run --rm \
    --network "container:${CONTAINER_NS}" \
    -v "${TMP_DIR}:${TMP_DIR}" \
    "$PINNED_CURL_IMAGE" \
    "${CURL_CMD[@]}")"
fi

printf "HTTP Status: %s (expected: %s)\n" "$HTTP_CODE" "$EXPECTED_STATUS"

if [[ "$HTTP_CODE" != "$EXPECTED_STATUS" ]]; then
  printf "ERROR: Status mismatch: got %s, expected %s\n" "$HTTP_CODE" "$EXPECTED_STATUS" >&2
  if [[ -s "$BODY_FILE" ]]; then
    printf "Response body:\n" >&2
    cat "$BODY_FILE" >&2
    printf "\n" >&2
  fi
  exit 1
fi

# Verify expected header if requested
if [[ -n "$EXPECTED_HEADER" ]]; then
  HEADER_KEY="${EXPECTED_HEADER%%:*}"
  HEADER_VAL="${EXPECTED_HEADER#*:}"
  HEADER_VAL="$(echo "$HEADER_VAL" | xargs)" # trim whitespace
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
  if grep -i -E "^(x-release-digest|x-release-id|x-commit-sha|x-app-digest):.*${EXPECTED_DIGEST}" "$HEADERS_FILE" >/dev/null 2>&1; then
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
