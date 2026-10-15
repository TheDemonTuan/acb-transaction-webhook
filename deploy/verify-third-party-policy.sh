#!/usr/bin/env bash
set -Eeuo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
image_ref=""
allowlist_file="$script_dir/third-party-allowlist.json"
expected_identity="${BARK_EXPECTED_IDENTITY:-}"
oidc_issuer="${BARK_OIDC_ISSUER:-https://token.actions.githubusercontent.com}"
public_key="${BARK_COSIGN_KEY:-}"
reference_date="${REFERENCE_DATE:-$(date -u +%F)}"

usage() {
  cat <<'EOF'
Usage: verify-third-party-policy.sh [options]

Options:
  --image <ref>              Image reference with immutable digest (required)
  --allowlist <path>         Path to third-party allowlist JSON (default: deploy/third-party-allowlist.json)
  --expected-identity <id>   Expected Cosign certificate identity (optional)
  --oidc-issuer <issuer>     Expected Cosign OIDC issuer (optional)
  --public-key <path>        Cosign public key for non-keyless verification (optional)
  --reference-date <date>    Reference date YYYY-MM-DD (default: current UTC date)
  --help, -h                 Show help
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --image)
      image_ref="$2"
      shift 2
      ;;
    --allowlist)
      allowlist_file="$2"
      shift 2
      ;;
    --expected-identity)
      expected_identity="$2"
      shift 2
      ;;
    --oidc-issuer)
      oidc_issuer="$2"
      shift 2
      ;;
    --public-key)
      public_key="$2"
      shift 2
      ;;
    --reference-date)
      reference_date="$2"
      shift 2
      ;;
    --help|-h)
      usage
      exit 0
      ;;
    *)
      printf 'Unknown argument: %s\n' "$1" >&2
      usage >&2
      exit 1
      ;;
  esac
done

if [[ -z "$image_ref" ]]; then
  printf 'Error: --image is required.\n' >&2
  exit 1
fi

# 1. Require immutable digest
if [[ ! "$image_ref" =~ ^[^[:space:]]+@sha256:[a-f0-9]{64}$ ]]; then
  printf 'Error: image ref must be an immutable digest with sha256 prefix: %s\n' "$image_ref" >&2
  exit 1
fi

image_digest="${image_ref#*@}"

# 2. Signature verification when configured
if [[ -n "$expected_identity" || -n "$public_key" ]]; then
  if ! command -v cosign >/dev/null 2>&1; then
    printf 'Error: cosign is required for signature verification but not found in PATH\n' >&2
    exit 1
  fi
  printf 'Verifying signature for %s using configured identity...\n' "$image_ref"
  if [[ -n "$public_key" ]]; then
    cosign verify --key "$public_key" "$image_ref"
  else
    cosign verify --certificate-identity "$expected_identity" --certificate-oidc-issuer "$oidc_issuer" "$image_ref"
  fi
  printf 'Third-party signature verification succeeded for %s.\n' "$image_ref"
  exit 0
fi

# 3. Fallback policy: verify against approved digest allowlist
if [[ ! -f "$allowlist_file" ]]; then
  printf 'Error: Third-party allowlist file not found: %s\n' "$allowlist_file" >&2
  exit 1
fi

if command -v python3 >/dev/null 2>&1; then
  python3 - "$allowlist_file" "$image_ref" "$image_digest" "$reference_date" <<'PYEOF'
import sys, json, re, datetime

filepath = sys.argv[1]
image_ref = sys.argv[2]
target_digest = sys.argv[3]
ref_date_str = sys.argv[4]

try:
    with open(filepath, "r", encoding="utf-8") as f:
        data = json.load(f)
except Exception as e:
    print(f"Error: failed to parse JSON in {filepath}: {e}", file=sys.stderr)
    sys.exit(1)

try:
    ref_date = datetime.date.fromisoformat(ref_date_str)
except Exception as e:
    print(f"Error: invalid reference date '{ref_date_str}': {e}", file=sys.stderr)
    sys.exit(1)

images = []
if isinstance(data, list):
    images = data
elif isinstance(data, dict) and "images" in data:
    if not isinstance(data["images"], list):
        print(f"Error: 'images' must be a JSON array in {filepath}", file=sys.stderr)
        sys.exit(1)
    images = data["images"]
else:
    print(f"Error: allowlist must contain an array or an object with 'images' key in {filepath}", file=sys.stderr)
    sys.exit(1)

match = None
for i, item in enumerate(images):
    if not isinstance(item, dict):
        print(f"Error: entry #{i} must be an object", file=sys.stderr)
        sys.exit(1)
    for req in ("digest", "owner", "reason", "expiry"):
        if req not in item or not isinstance(item[req], str) or not item[req].strip():
            print(f"Error: entry #{i} missing required field '{req}'", file=sys.stderr)
            sys.exit(1)
    d = item["digest"].strip()
    img = item.get("image", "").strip()
    if d == target_digest or (img and f"{img}@{d}" == image_ref):
        match = item
        break

if not match:
    print(f"Error: image digest '{target_digest}' is not in approved third-party allowlist: {filepath}", file=sys.stderr)
    sys.exit(1)

expiry_str = match["expiry"].strip()
try:
    exp_date = datetime.date.fromisoformat(expiry_str)
except ValueError:
    print(f"Error: invalid expiry date format '{expiry_str}' for digest {target_digest}", file=sys.stderr)
    sys.exit(1)

if exp_date < ref_date:
    print(f"Error: third-party allowlist entry for {target_digest} expired on {expiry_str} (owner: {match['owner']}, reason: {match['reason']})", file=sys.stderr)
    sys.exit(1)

print(f"Third-party policy OK: image {image_ref} approved by allowlist (owner: {match['owner']}, expiry: {match['expiry']}).")
PYEOF
else
  printf 'Error: python3 required to validate third-party allowlist\n' >&2
  exit 1
fi
