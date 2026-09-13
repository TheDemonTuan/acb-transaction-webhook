#!/usr/bin/env bash
set -Eeuo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
deploy_dir="$script_dir"
output_file="$deploy_dir/release-manifest.json"

git_sha="${GITHUB_SHA:-}"
gateway_image=""
worker_image=""
dbtool_image=""
auth_browser_image=""
tts_image=""
bark_image=""

usage() {
  cat <<'EOF'
Usage: generate-release-manifest.sh [options]

Options:
  --git-sha <sha>            Git commit SHA (default: HEAD or GITHUB_SHA)
  --gateway-image <ref>      Exact gateway image digest (required)
  --worker-image <ref>       Exact worker image digest (required)
  --dbtool-image <ref>       Exact dbtool image digest (required)
  --auth-browser-image <ref> Exact auth-browser image digest (required)
  --tts-image <ref>          Exact tts-gateway image digest (required)
  --bark-image <ref>         Exact bark image digest (required)
  --deploy-dir <dir>         Deploy bundle directory (default: deploy/)
  --output <path>            Output manifest JSON path (default: deploy/release-manifest.json)
  --help, -h                 Show help
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --git-sha)
      git_sha="$2"
      shift 2
      ;;
    --gateway-image)
      gateway_image="$2"
      shift 2
      ;;
    --worker-image)
      worker_image="$2"
      shift 2
      ;;
    --dbtool-image)
      dbtool_image="$2"
      shift 2
      ;;
    --auth-browser-image)
      auth_browser_image="$2"
      shift 2
      ;;
    --tts-image)
      tts_image="$2"
      shift 2
      ;;
    --bark-image)
      bark_image="$2"
      shift 2
      ;;
    --deploy-dir)
      deploy_dir="$2"
      shift 2
      ;;
    --output)
      output_file="$2"
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

if [[ -z "$git_sha" ]]; then
  if command -v git >/dev/null 2>&1 && git rev-parse HEAD >/dev/null 2>&1; then
    git_sha="$(git rev-parse HEAD)"
  else
    printf 'Error: --git-sha is required and cannot be inferred\n' >&2
    exit 1
  fi
fi

if [[ ! "$git_sha" =~ ^[0-9a-fA-F]{40}$ ]]; then
  printf 'Error: invalid git_sha format: %s (must be 40-character hex)\n' "$git_sha" >&2
  exit 1
fi

declare -A images=(
  ["gateway"]="$gateway_image"
  ["worker"]="$worker_image"
  ["dbtool"]="$dbtool_image"
  ["auth_browser"]="$auth_browser_image"
  ["tts_gateway"]="$tts_image"
  ["bark"]="$bark_image"
)

for name in "${!images[@]}"; do
  val="${images[$name]}"
  if [[ -z "$val" ]]; then
    printf 'Error: missing required image digest for %s\n' "$name" >&2
    exit 1
  fi
  if [[ ! "$val" =~ ^[^[:space:]]+@sha256:[a-f0-9]{64}$ ]]; then
    printf 'Error: %s image is not an immutable digest with sha256 prefix: %s\n' "$name" "$val" >&2
    exit 1
  fi
done

if [[ ! -d "$deploy_dir" ]]; then
  printf 'Error: deploy directory not found: %s\n' "$deploy_dir" >&2
  exit 1
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

# Collect deploy bundle files
bundle_files=(
  "compose.prod.yaml"
  "deploy-warm.sh"
  "rollback-warm.sh"
  "switch-slot.sh"
  "smoke-slot.sh"
  "check-host.sh"
  "seccomp-auth-browser.json"
  "deploy.sh"
  "rollback.sh"
  "verify-deployment.sh"
  "backup.sh"
  "bark-entrypoint.sh"
  "smoke-test-bark.sh"
  "smoke-test-tts-gateway.sh"
  "smoke-test-auth-browser.sh"
  "verify-manifest.sh"
  "README.md"
)

tmp_artifacts="$(mktemp)"
trap 'rm -f "$tmp_artifacts"' EXIT

for filename in "${bundle_files[@]}"; do
  filepath="$deploy_dir/$filename"
  if [[ -f "$filepath" ]]; then
    hash="$(compute_hash "$filepath")"
    printf '%s=%s\n' "$filename" "$hash" >> "$tmp_artifacts"
  fi
done

created_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

if command -v node >/dev/null 2>&1; then
  node - "$output_file" "$git_sha" "$created_at" "$tmp_artifacts" \
    "$gateway_image" "$worker_image" "$dbtool_image" "$auth_browser_image" "$tts_image" "$bark_image" <<'JSEOF'
const fs = require('fs');

const [,, outFile, gitSha, createdAt, artifactsFile, gateway, worker, dbtool, authBrowser, tts, bark] = process.argv;

const artifacts = {};
const lines = fs.readFileSync(artifactsFile, 'utf8').split('\n');
for (const line of lines) {
  const trimmed = line.trim();
  if (!trimmed) continue;
  const eqIdx = trimmed.indexOf('=');
  if (eqIdx !== -1) {
    artifacts[trimmed.slice(0, eqIdx)] = trimmed.slice(eqIdx + 1);
  }
}

const manifest = {
  schema_version: 1,
  git_sha: gitSha,
  created_at: createdAt,
  compatibility: {
    schema_version: 1,
    min_supported_schema_version: 1,
    worker_rpc_version: 1,
    worker_rpc_endpoints: [
      "/rpc/request-sync",
      "/rpc/ensure-history",
      "/rpc/notify-settings-changed",
      "/rpc/wake-dispatcher",
      "/rpc/verify-session"
    ]
  },
  images: {
    gateway,
    worker,
    dbtool,
    auth_browser: authBrowser,
    tts_gateway: tts,
    bark
  },
  artifacts
};

fs.writeFileSync(outFile, JSON.stringify(manifest, null, 2) + '\n', 'utf8');
JSEOF
elif command -v python3 >/dev/null 2>&1; then
  python3 - "$output_file" "$git_sha" "$created_at" "$tmp_artifacts" \
    "$gateway_image" "$worker_image" "$dbtool_image" "$auth_browser_image" "$tts_image" "$bark_image" <<'PYEOF'
import sys, json

out_file = sys.argv[1]
git_sha = sys.argv[2]
created_at = sys.argv[3]
artifacts_file = sys.argv[4]
gateway, worker, dbtool, auth_browser, tts, bark = sys.argv[5:11]

artifacts = {}
with open(artifacts_file, 'r', encoding='utf-8') as f:
    for line in f:
        line = line.strip()
        if not line:
            continue
        if '=' in line:
            k, v = line.split('=', 1)
            artifacts[k] = v

manifest = {
    "schema_version": 1,
    "git_sha": git_sha,
    "created_at": created_at,
    "compatibility": {
        "schema_version": 1,
        "min_supported_schema_version": 1,
        "worker_rpc_version": 1,
        "worker_rpc_endpoints": [
            "/rpc/request-sync",
            "/rpc/ensure-history",
            "/rpc/notify-settings-changed",
            "/rpc/wake-dispatcher",
            "/rpc/verify-session"
        ]
    },
    "images": {
        "gateway": gateway,
        "worker": worker,
        "dbtool": dbtool,
        "auth_browser": auth_browser,
        "tts_gateway": tts,
        "bark": bark
    },
    "artifacts": artifacts
}

with open(out_file, 'w', encoding='utf-8') as f:
    json.dump(manifest, f, indent=2)
    f.write('\n')
PYEOF
else
  printf 'Error: neither node nor python3 available to write manifest JSON\n' >&2
  exit 1
fi

printf 'Release manifest generated successfully: %s (git_sha: %s)\n' "$output_file" "$git_sha"
