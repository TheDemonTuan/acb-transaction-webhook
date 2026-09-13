#!/usr/bin/env bash
set -Eeuo pipefail

manifest_file=""
bundle_file=""
deploy_dir=""
expected_gateway=""
expected_worker=""
expected_dbtool=""
expected_browser=""
expected_tts=""
expected_bark=""
expected_identity=""
expected_issuer="https://token.actions.githubusercontent.com"
require_cosign=0

usage() {
  cat <<'EOF'
Usage: verify-manifest.sh [options]

Options:
  --manifest <path>          Path to release-manifest.json
  --bundle <path>            Path to release-manifest.bundle (optional)
  --deploy-dir <dir>         Directory containing deployed files (default: manifest directory)
  --gateway-image <ref>      Expected gateway image digest (optional)
  --worker-image <ref>       Expected worker image digest (optional)
  --dbtool-image <ref>       Expected dbtool image digest (optional)
  --browser-image <ref>      Expected auth-browser image digest (optional)
  --tts-image <ref>          Expected tts-gateway image digest (optional)
  --bark-image <ref>         Expected bark image digest (optional)
  --expected-identity <id>   Expected Cosign certificate identity (optional)
  --expected-issuer <issuer> Expected Cosign OIDC issuer (default: github actions)
  --require-cosign           Require Cosign binary to be installed if bundle is provided
  --help, -h                 Show help
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --manifest)
      manifest_file="$2"
      shift 2
      ;;
    --bundle)
      bundle_file="$2"
      shift 2
      ;;
    --deploy-dir)
      deploy_dir="$2"
      shift 2
      ;;
    --gateway-image)
      expected_gateway="$2"
      shift 2
      ;;
    --worker-image)
      expected_worker="$2"
      shift 2
      ;;
    --dbtool-image)
      expected_dbtool="$2"
      shift 2
      ;;
    --browser-image)
      expected_browser="$2"
      shift 2
      ;;
    --tts-image)
      expected_tts="$2"
      shift 2
      ;;
    --bark-image)
      expected_bark="$2"
      shift 2
      ;;
    --expected-identity)
      expected_identity="$2"
      shift 2
      ;;
    --expected-issuer)
      expected_issuer="$2"
      shift 2
      ;;
    --require-cosign)
      require_cosign=1
      shift
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

if [[ -z "$manifest_file" ]]; then
  if [[ -f "release-manifest.json" ]]; then
    manifest_file="release-manifest.json"
  elif [[ -f "deploy/release-manifest.json" ]]; then
    manifest_file="deploy/release-manifest.json"
  else
    printf 'Error: --manifest file path is required.\n' >&2
    exit 1
  fi
fi

if [[ ! -f "$manifest_file" ]]; then
  printf 'Error: release manifest file not found: %s\n' "$manifest_file" >&2
  exit 1
fi

if [[ -z "$deploy_dir" ]]; then
  deploy_dir="$(cd -- "$(dirname -- "$manifest_file")" && pwd)"
fi

if [[ -z "$bundle_file" ]]; then
  candidate_bundle="${manifest_file%.json}.bundle"
  if [[ -f "$candidate_bundle" ]]; then
    bundle_file="$candidate_bundle"
  fi
fi

compute_hash() {
  local target="$1"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$target" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$target" | awk '{print $1}'
  else
    printf 'Error: neither sha256sum nor shasum found\n' >&2
    exit 1
  fi
}

# Validate JSON content and extract fields
validate_and_extract() {
  if command -v node >/dev/null 2>&1; then
    node - "$manifest_file" "$expected_gateway" "$expected_worker" "$expected_dbtool" "$expected_browser" "$expected_tts" "$expected_bark" <<'JSEOF'
const fs = require('fs');

const manifestPath = process.argv[2];
const expected = {
  gateway: process.argv[3],
  worker: process.argv[4],
  dbtool: process.argv[5],
  auth_browser: process.argv[6],
  tts_gateway: process.argv[7],
  bark: process.argv[8]
};

let manifest;
try {
  manifest = JSON.parse(fs.readFileSync(manifestPath, 'utf8'));
} catch (e) {
  console.error(`Error: invalid JSON in ${manifestPath}: ${e.message}`);
  process.exit(1);
}

if (!manifest.git_sha || !/^[0-9a-fA-F]{40}$/.test(manifest.git_sha)) {
  console.error(`Error: invalid or missing git_sha in manifest: ${manifest.git_sha}`);
  process.exit(1);
}

if (!manifest.compatibility || typeof manifest.compatibility !== 'object') {
  console.error('Error: missing compatibility section in manifest');
  process.exit(1);
}

if (!manifest.images || typeof manifest.images !== 'object') {
  console.error('Error: missing images section in manifest');
  process.exit(1);
}

const requiredImages = ['gateway', 'worker', 'dbtool', 'auth_browser', 'tts_gateway', 'bark'];
const digestRe = /^[^\s]+@sha256:[a-f0-9]{64}$/;

for (const key of requiredImages) {
  const img = manifest.images[key];
  if (!img || !digestRe.test(img)) {
    console.error(`Error: image '${key}' is missing or not an immutable sha256 digest: ${img}`);
    process.exit(1);
  }
  if (expected[key] && expected[key] !== img) {
    console.error(`Error: runtime image mismatch for '${key}': expected=${expected[key]}, manifest=${img}`);
    process.exit(1);
  }
}

if (!manifest.artifacts || typeof manifest.artifacts !== 'object') {
  console.error('Error: missing artifacts section in manifest');
  process.exit(1);
}

// Print parsed values in line-delimited key=value format for bash caller
console.log(`GIT_SHA=${manifest.git_sha}`);
console.log(`COMPAT_SCHEMA=${manifest.compatibility.schema_version}`);
console.log(`COMPAT_RPC=${manifest.compatibility.worker_rpc_version}`);
for (const key of requiredImages) {
  console.log(`IMAGE_${key.toUpperCase()}=${manifest.images[key]}`);
}
for (const [artFile, artHash] of Object.entries(manifest.artifacts)) {
  console.log(`ARTIFACT:${artFile}=${artHash}`);
}
JSEOF
  elif command -v python3 >/dev/null 2>&1; then
    python3 - "$manifest_file" "$expected_gateway" "$expected_worker" "$expected_dbtool" "$expected_browser" "$expected_tts" "$expected_bark" <<'PYEOF'
import sys, json, re

manifest_path = sys.argv[1]
expected = {
    'gateway': sys.argv[2],
    'worker': sys.argv[3],
    'dbtool': sys.argv[4],
    'auth_browser': sys.argv[5],
    'tts_gateway': sys.argv[6],
    'bark': sys.argv[7],
}

try:
    with open(manifest_path, 'r', encoding='utf-8') as f:
        manifest = json.load(f)
except Exception as e:
    print(f"Error: invalid JSON in {manifest_path}: {e}", file=sys.stderr)
    sys.exit(1)

git_sha = manifest.get('git_sha', '')
if not re.match(r'^[0-9a-fA-F]{40}$', git_sha):
    print(f"Error: invalid or missing git_sha in manifest: {git_sha}", file=sys.stderr)
    sys.exit(1)

compat = manifest.get('compatibility')
if not isinstance(compat, dict):
    print("Error: missing compatibility section in manifest", file=sys.stderr)
    sys.exit(1)

images = manifest.get('images')
if not isinstance(images, dict):
    print("Error: missing images section in manifest", file=sys.stderr)
    sys.exit(1)

required = ['gateway', 'worker', 'dbtool', 'auth_browser', 'tts_gateway', 'bark']
digest_re = re.compile(r'^[^ \t\n\r]+@sha256:[a-f0-9]{64}$')

for key in required:
    img = images.get(key, '')
    if not digest_re.match(img):
        print(f"Error: image '{key}' is missing or not an immutable sha256 digest: {img}", file=sys.stderr)
        sys.exit(1)
    exp = expected.get(key)
    if exp and exp != img:
        print(f"Error: runtime image mismatch for '{key}': expected={exp}, manifest={img}", file=sys.stderr)
        sys.exit(1)

artifacts = manifest.get('artifacts')
if not isinstance(artifacts, dict):
    print("Error: missing artifacts section in manifest", file=sys.stderr)
    sys.exit(1)

print(f"GIT_SHA={git_sha}")
print(f"COMPAT_SCHEMA={compat.get('schema_version')}")
print(f"COMPAT_RPC={compat.get('worker_rpc_version')}")
for k in required:
    print(f"IMAGE_{k.upper()}={images[k]}")
for art_file, art_hash in artifacts.items():
    print(f"ARTIFACT:{art_file}={art_hash}")
PYEOF
  elif command -v jq >/dev/null 2>&1; then
    local sha
    sha="$(jq -r '.git_sha // empty' "$manifest_file")"
    if [[ ! "$sha" =~ ^[0-9a-fA-F]{40}$ ]]; then
      printf 'Error: invalid or missing git_sha in manifest: %s\n' "$sha" >&2
      exit 1
    fi
    printf 'GIT_SHA=%s\n' "$sha"
    for k in gateway worker dbtool auth_browser tts_gateway bark; do
      local val
      val="$(jq -r --arg k "$k" '.images[$k] // empty' "$manifest_file")"
      if [[ ! "$val" =~ ^[^[:space:]]+@sha256:[a-f0-9]{64}$ ]]; then
        printf 'Error: image %s missing or not an immutable digest\n' "$k" >&2
        exit 1
      fi
      local ukey
      ukey="$(printf '%s' "$k" | tr '[:lower:]' '[:upper:]')"
      printf 'IMAGE_%s=%s\n' "$ukey" "$val"
    done
    jq -r '.artifacts | to_entries[] | "ARTIFACT:\(.key)=\(.value)"' "$manifest_file"
  else
    printf 'Error: node, python3, or jq required to parse release manifest\n' >&2
    exit 1
  fi
}

parsed_output="$(validate_and_extract)"

# Verify artifacts checksums
artifact_count=0
while IFS= read -r line; do
  if [[ "$line" =~ ^ARTIFACT:(.*)=(.*)$ ]]; then
    art_file="${BASH_REMATCH[1]}"
    expected_hash="${BASH_REMATCH[2]}"

    # Locate artifact file on disk
    target_path=""
    if [[ -f "$deploy_dir/$art_file" ]]; then
      target_path="$deploy_dir/$art_file"
    elif [[ -f "$art_file" ]]; then
      target_path="$art_file"
    elif [[ -f "$deploy_dir/$(basename "$art_file")" ]]; then
      target_path="$deploy_dir/$(basename "$art_file")"
    else
      printf 'Error: artifact file listed in manifest not found on disk: %s (checked in %s)\n' "$art_file" "$deploy_dir" >&2
      exit 1
    fi

    actual_hash="$(compute_hash "$target_path")"
    if [[ "$actual_hash" != "$expected_hash" ]]; then
      printf 'Error: artifact checksum mismatch for %s: expected=%s actual=%s\n' "$art_file" "$expected_hash" "$actual_hash" >&2
      exit 1
    fi
    artifact_count=$((artifact_count + 1))
  fi
done <<< "$parsed_output"

# Verify Cosign signature bundle if provided
if [[ -n "$bundle_file" && -f "$bundle_file" ]]; then
  if command -v cosign >/dev/null 2>&1; then
    printf 'Verifying Cosign signature bundle: %s\n' "$bundle_file"
    cosign_args=(verify-blob --bundle "$bundle_file" --certificate-oidc-issuer "$expected_issuer")
    if [[ -n "$expected_identity" ]]; then
      cosign_args+=(--certificate-identity "$expected_identity")
    else
      cosign_args+=(--certificate-identity-regexp '.*')
    fi
    cosign_args+=("$manifest_file")
    if ! cosign "${cosign_args[@]}"; then
      printf 'Error: Cosign manifest signature verification failed for %s\n' "$manifest_file" >&2
      exit 1
    fi
    printf 'Cosign manifest blob signature verified successfully.\n'
  else
    if [[ $require_cosign -eq 1 ]]; then
      printf 'Error: Cosign verification required but cosign binary not found in PATH\n' >&2
      exit 1
    else
      printf 'Warning: Cosign not installed on host; skipping cryptographic blob signature check.\n'
    fi
  fi
fi

manifest_git_sha="$(printf '%s\n' "$parsed_output" | grep '^GIT_SHA=' | cut -d'=' -f2)"
printf 'Release manifest preflight OK: git_sha=%s, 6 images verified, %d artifact checksums verified.\n' "$manifest_git_sha" "$artifact_count"
exit 0
