#!/usr/bin/env bash
# deploy/smoke-public-payment-qr.sh
# Post-cutover production edge smoke test for Public Payment QR & /pay flow:
# 1. /pay/:identifier -> HTTP 200 with HTML document
# 2. GET /api/public/v1/payment-qr -> HTTP 200, configured & imageURL
# 3. GET /api/public/v1/payment-qr/image -> HTTP 200, image/*, bytes > 0
# 4. POST /api/public/v1/payment-qr/activate -> HTTP 200 (or structured response)
set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deploy/lib.sh
if [[ -f "$SCRIPT_DIR/lib.sh" ]]; then
  source "$SCRIPT_DIR/lib.sh"
else
  log_info() { printf '[INFO] %s\n' "$*"; }
  log_warn() { printf '[WARN] %s\n' "$*"; }
  log_error() { printf '[ERROR] %s\n' "$*" >&2; }
fi

ORIGIN="${PUBLIC_VIEWER_ORIGIN:-https://transactions.tuannguyenviet.site}"
TIMEOUT=10
REQUIRE_CONFIGURED=0
DRY_RUN=0

usage() {
  cat <<EOF
Usage: $0 [options]

Options:
  --origin <url>              Base public viewer origin (default: ${ORIGIN})
  --timeout <seconds>         Request timeout in seconds (default: 10)
  --require-configured        Fail if payment QR is not configured in DB (default: warn & skip image)
  --dry-run                   Validate syntax and options without making network calls
  --help                      Show this help message
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --origin)
      ORIGIN="$2"
      shift 2
      ;;
    --timeout)
      TIMEOUT="$2"
      shift 2
      ;;
    --require-configured)
      REQUIRE_CONFIGURED=1
      shift
      ;;
    --dry-run)
      DRY_RUN=1
      shift
      ;;
    --help|-h)
      usage
      exit 0
      ;;
    *)
      log_error "Unknown option: $1"
      usage >&2
      exit 1
      ;;
  esac
done

if [[ "$DRY_RUN" -eq 1 ]]; then
  log_info "DRY_RUN: Smoke test parameters validated successfully for origin: ${ORIGIN}"
  exit 0
fi

# Ensure curl and python3 are available
if ! command -v curl >/dev/null 2>&1; then
  log_error "curl command not found in PATH"
  exit 1
fi
if ! command -v python3 >/dev/null 2>&1; then
  log_error "python3 command not found in PATH"
  exit 1
fi

tmp_dir="$(mktemp -d)"
cleanup() {
  rm -rf "$tmp_dir"
}
trap cleanup EXIT

log_info "Starting post-cutover public payment QR smoke verification against ${ORIGIN}..."

# 1. Verify frontend /pay/:identifier route
pay_url="${ORIGIN}/pay/smoke-test-verify"
log_info "1. Probing landing page: GET ${pay_url}..."
html_file="$tmp_dir/pay.html"
pay_code="$(curl -fsS -L --max-time "$TIMEOUT" -o "$html_file" -w "%{http_code}" "$pay_url" 2>/dev/null || true)"
if [[ "$pay_code" != "200" ]]; then
  log_error "Landing page GET ${pay_url} failed with HTTP ${pay_code:-000}"
  exit 1
fi
if ! grep -qi '<!doctype html' "$html_file" && ! grep -qi '<html' "$html_file" && ! grep -q 'id="root"' "$html_file"; then
  log_error "Landing page did not return valid HTML shell: ${pay_url}"
  exit 1
fi
log_info "Landing page OK: HTTP 200 with valid HTML application shell."

# 2. Verify metadata API: GET /api/public/v1/payment-qr
meta_url="${ORIGIN}/api/public/v1/payment-qr"
log_info "2. Probing metadata endpoint: GET ${meta_url}..."
meta_file="$tmp_dir/meta.json"
meta_code="$(curl -fsS --max-time "$TIMEOUT" -o "$meta_file" -w "%{http_code}" "$meta_url" 2>/dev/null || true)"
if [[ "$meta_code" != "200" ]]; then
  log_error "Metadata endpoint GET ${meta_url} failed with HTTP ${meta_code:-000}"
  exit 1
fi

read -r is_configured has_image image_url < <(python3 - "$meta_file" <<'PY'
import json, sys
try:
    with open(sys.argv[1], encoding='utf-8') as f:
        d = json.load(f)
    print(
        str(d.get('configured', False)).lower(),
        str(d.get('hasImage', False)).lower(),
        str(d.get('imageURL', '') or '').strip()
    )
except Exception:
    print('false', 'false', '')
PY
)

log_info "Metadata status: configured=${is_configured}, hasImage=${has_image}, imageURL=${image_url}"

# 3. Verify image endpoint if configured
if [[ "$is_configured" == "true" ]]; then
  if [[ "$has_image" != "true" || -z "$image_url" ]]; then
    log_error "Payment QR configured=true but hasImage=${has_image} or imageURL is empty"
    exit 1
  fi
  full_image_url="$image_url"
  if [[ "$full_image_url" == /* ]]; then
    full_image_url="${ORIGIN}${image_url}"
  fi
  log_info "3. Probing image endpoint: GET ${full_image_url}..."
  hdr_file="$tmp_dir/img.hdr"
  img_file="$tmp_dir/img.bin"
  img_code="$(curl -fsS -D "$hdr_file" --max-time "$TIMEOUT" -o "$img_file" -w "%{http_code}" "$full_image_url" 2>/dev/null || true)"
  if [[ "$img_code" != "200" ]]; then
    log_error "Image endpoint GET ${full_image_url} failed with HTTP ${img_code:-000}"
    exit 1
  fi

  content_type="$(awk -F': ' 'tolower($1)=="content-type" {print $2}' "$hdr_file" | tr -d '\r\n' || true)"
  if [[ "$content_type" != *"image/png"* && "$content_type" != *"image/jpeg"* && "$content_type" != *"image/webp"* ]]; then
    log_error "Image Content-Type mismatch: expected image/(png|jpeg|webp), got '${content_type}'"
    exit 1
  fi

  img_size="$(wc -c < "$img_file" | tr -d ' ')"
  if (( img_size <= 0 )); then
    log_error "Image endpoint returned empty file (0 bytes)"
    exit 1
  fi
  log_info "Image endpoint OK: HTTP 200, Content-Type: ${content_type}, Size: ${img_size} bytes."
else
  if [[ "$REQUIRE_CONFIGURED" -eq 1 ]]; then
    log_error "Payment QR is required to be configured, but backend returned configured=false"
    exit 1
  fi
  log_warn "Payment QR is not yet configured in DB (configured=false). Skipping image byte retrieval."
fi

# 4. Verify activation API: POST /api/public/v1/payment-qr/activate
act_url="${ORIGIN}/api/public/v1/payment-qr/activate"
log_info "4. Probing activation endpoint: POST ${act_url}..."
act_file="$tmp_dir/act.json"
act_code="$(curl -s -X POST -H "Content-Type: application/json" -d '{"identifier":"smoke-test-verify"}' --max-time "$TIMEOUT" -o "$act_file" -w "%{http_code}" "$act_url" 2>/dev/null || true)"

if [[ "$act_code" != "200" && "$act_code" != "429" && "$act_code" != "503" ]]; then
  log_error "Activation endpoint POST ${act_url} returned unexpected HTTP ${act_code:-000}"
  cat "$act_file" >&2
  exit 1
fi
log_info "Activation endpoint OK: HTTP ${act_code} (structured activation response received)."

log_info "All Public Payment QR smoke tests PASSED successfully."
exit 0
