#!/usr/bin/env bash
set -Eeuo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
deploy_dir="$script_dir"
output_file="$deploy_dir/release-manifest.json"

git_sha="${GITHUB_SHA:-}"
release_id=""
base_sha=""
promotion_scope_json=""
promotion_scope_file=""

frontend_image=""
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
  --git-sha <sha>               Git commit SHA (default: HEAD or GITHUB_SHA)
  --release-id <id>             Release ID (default: rel-<sha:12>-<timestamp>)
  --base-sha <sha>              Base commit SHA to compute promotion scope against
  --promotion-scope <json>      Explicit promotion scope JSON string
  --promotion-scope-file <path> Path to computed promotion scope JSON file
  --frontend-image <ref>        Exact frontend image digest (required)
  --gateway-image <ref>         Exact gateway image digest (required)
  --worker-image <ref>          Exact worker image digest (required)
  --dbtool-image <ref>          Exact dbtool image digest (required)
  --auth-browser-image <ref>    Exact auth-browser image digest (required)
  --tts-image <ref>             Exact tts-gateway image digest (required)
  --bark-image <ref>            Exact bark image digest (required)
  --deploy-dir <dir>            Deploy bundle directory (default: deploy/)
  --output <path>               Output manifest JSON path (default: deploy/release-manifest.json)
  --help, -h                    Show help
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --git-sha)
      git_sha="$2"
      shift 2
      ;;
    --release-id)
      release_id="$2"
      shift 2
      ;;
    --base-sha)
      base_sha="$2"
      shift 2
      ;;
    --promotion-scope)
      promotion_scope_json="$2"
      shift 2
      ;;
    --promotion-scope-file)
      promotion_scope_file="$2"
      shift 2
      ;;
    --frontend-image)
      frontend_image="$2"
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

created_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
created_epoch="$(date -u +%s)"

if [[ -z "$release_id" ]]; then
  release_id="rel-${git_sha:0:12}-${created_epoch}"
fi

# Resolve promotion scope
tmp_scope_file="$(mktemp)"
trap 'rm -f "$tmp_scope_file" "$tmp_artifacts"' EXIT

if [[ -n "$promotion_scope_file" && -f "$promotion_scope_file" ]]; then
  cp "$promotion_scope_file" "$tmp_scope_file"
elif [[ -n "$promotion_scope_json" ]]; then
  printf '%s\n' "$promotion_scope_json" > "$tmp_scope_file"
elif [[ -n "$base_sha" ]] && [[ -f "$script_dir/../scripts/compute-promotion-scope.sh" ]]; then
  bash "$script_dir/../scripts/compute-promotion-scope.sh" \
    --base "$base_sha" \
    --head "$git_sha" \
    --format json \
    --output "$tmp_scope_file"
else
  # Default full promotion scope when not specified
  cat <<'EOF' > "$tmp_scope_file"
{
  "promotion": {
    "frontend": true,
    "gateway": true,
    "worker": true,
    "schema": true,
    "auth_browser": true,
    "tts": true,
    "bark": true,
    "failover_controller": true,
    "platform": true
  },
  "promotion_scope": [
    "frontend",
    "gateway",
    "worker",
    "schema",
    "auth_browser",
    "tts",
    "bark",
    "failover_controller",
    "platform"
  ]
}
EOF
fi

declare -A images=(
  ["frontend"]="$frontend_image"
  ["gateway"]="$gateway_image"
  ["worker"]="$worker_image"
  ["dbtool"]="$dbtool_image"
  ["auth_browser"]="$auth_browser_image"
  ["tts"]="$tts_image"
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
  "compose/base.yaml"
  "compose/gateway.yaml"
  "compose/frontend.yaml"
  "compose/worker.yaml"
  "compose/auth-browser.yaml"
  "compose/tts.yaml"
  "compose/bark.yaml"
  "compose/dbtool.yaml"
  "runtime-layout.sh"
  "reconcile-release.sh"
  "release-state.py"
  "component-map.json"
  "lib.sh"
  "deploy-warm.sh"
  "deploy-frontend.sh"
  "frontend-nginx.conf"
  "rollback-warm.sh"
  "switch-slot.sh"
  "smoke-slot.sh"
  "check-host.sh"
  "seccomp-auth-browser.json"
  "deploy.sh"
  "rollback.sh"
  "verify-deployment.sh"
  "backup.sh"
  "backup-db.sh"
  "backup-secrets.sh"
  "restore-db.sh"
  "provision-secrets.sh"
  "init-fresh-data.sh"
  "release-env.sh"
  "stable-deployer.sh"
  "stage-platform-assets.sh"
  "edge-probe.sh"
  "lib/common.sh"
  "lib/database.sh"
  "lib/images.sh"
  "lib/state.sh"
  "lib/traefik.sh"
  "lib/rollout-journal.sh"
  "verify-compose-runtime.sh"
  "verify-runtime-drift.sh"
  "cve-allowlist.json"
  "validate-cve-allowlist.sh"
  "third-party-allowlist.json"
  "verify-third-party-policy.sh"
  "bark-entrypoint.sh"
  "smoke-test-bark.sh"
  "smoke-test-tts-gateway.sh"
  "smoke-test-auth-browser.sh"
  "verify-manifest.sh"
  "dispatch-rollout.sh"
  "deploy-gateway.sh"
  "deploy-schema.sh"
  "deploy-worker.sh"
  "deploy-auth-browser.sh"
  "deploy-tts.sh"
  "deploy-bark.sh"
  "deploy-failover-controller.sh"
  "failover/vps-failover-controller.py"
  "failover/vps-failover-controller.service"
  "failover/vps-failover-reconcile.service"
  "failover/vps-failover-reconcile.timer"
  "failover/apps.d/acb.json"
  "failover/apps.d/auth-browser.json"
  "failover/apps.d/worker.json"
  "render-traefik-route.sh"
  "README.md"
)

tmp_artifacts="$(mktemp)"

for filename in "${bundle_files[@]}"; do
  filepath="$deploy_dir/$filename"
  if [[ ! -f "$filepath" || -L "$filepath" ]]; then
    printf 'Error: required regular release artifact is missing or a symlink: %s\n' "$filename" >&2
    exit 1
  fi
  hash="$(compute_hash "$filepath")"
  printf '%s=%s\n' "$filename" "$hash" >> "$tmp_artifacts"
done

# Deterministic split-compose bundle hash
compose_bundle_hash="$(python3 - "$deploy_dir" <<'PY'
import hashlib, pathlib, sys
root = pathlib.Path(sys.argv[1])
h = hashlib.sha256()
for p in sorted((root / "compose").glob("*.yaml")):
    rel = f"compose/{p.name}"
    h.update(rel.encode() + b"\0" + p.read_bytes() + b"\0")
print(h.hexdigest())
PY
)"
printf 'compose_bundle=%s\n' "$compose_bundle_hash" >> "$tmp_artifacts"
printf 'compose_bundle_sha256=%s\n' "$compose_bundle_hash" >> "$tmp_artifacts"

# Deterministic failover controller bundle hash
failover_bundle_hash="$(python3 - "$deploy_dir" <<'PY'
import hashlib, pathlib, sys
root = pathlib.Path(sys.argv[1]) / "failover"
paths = [
    pathlib.Path("vps-failover-controller.py"),
    pathlib.Path("vps-failover-controller.service"),
    pathlib.Path("vps-failover-reconcile.service"),
    pathlib.Path("vps-failover-reconcile.timer"),
    pathlib.Path("apps.d/acb.json"),
    pathlib.Path("apps.d/auth-browser.json"),
    pathlib.Path("apps.d/worker.json"),
]
h = hashlib.sha256()
for rel in paths:
    p = root / rel
    if p.is_file():
        h.update(str(rel).encode() + b"\0" + p.read_bytes() + b"\0")
print(h.hexdigest())
PY
)"
printf 'failover-bundle=%s\n' "$failover_bundle_hash" >> "$tmp_artifacts"
printf 'failover_bundle_sha256=%s\n' "$failover_bundle_hash" >> "$tmp_artifacts"

if command -v node >/dev/null 2>&1; then
  node - "$output_file" "$git_sha" "$release_id" "$created_at" "$tmp_artifacts" "$tmp_scope_file" \
    "$frontend_image" "$gateway_image" "$worker_image" "$dbtool_image" "$auth_browser_image" "$tts_image" "$bark_image" <<'JSEOF'
const fs = require('fs');

const [,, outFile, gitSha, releaseId, createdAt, artifactsFile, scopeFile, frontend, gateway, worker, dbtool, authBrowser, tts, bark] = process.argv;

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

let scopeData = {};
try {
  scopeData = JSON.parse(fs.readFileSync(scopeFile, 'utf8'));
} catch (e) {
  console.error(`Error parsing scope file: ${e.message}`);
  process.exit(1);
}

const defaultPromotion = {
  frontend: true,
  gateway: true,
  worker: true,
  schema: true,
  auth_browser: true,
  tts: true,
  bark: true,
  platform: true
};

const promotion = scopeData.promotion || defaultPromotion;
const promotionScope = scopeData.promotion_scope || Object.keys(promotion).filter(k => promotion[k]);

const manifest = {
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  schema_version: 1,
  release_id: releaseId,
  git_sha: gitSha,
  created_at: createdAt,
  compatibility: {
    schema_version: 9,
    min_supported_schema_version: 9,
    worker_rpc_version: 2,
    worker_rpc_endpoints: [
      "/rpc/request-sync",
      "/rpc/history-jobs",
      "/rpc/notify-settings-changed",
      "/rpc/wake-dispatcher",
      "/rpc/verify-session"
    ]
  },
  promotion,
  promotion_scope: promotionScope,
  images: {
    frontend,
    gateway,
    worker,
    dbtool,
    auth_browser: authBrowser,
    tts,
    bark
  },
  artifacts
};

fs.writeFileSync(outFile, JSON.stringify(manifest, null, 2) + '\n', 'utf8');
JSEOF
elif command -v python3 >/dev/null 2>&1; then
  python3 - "$output_file" "$git_sha" "$release_id" "$created_at" "$tmp_artifacts" "$tmp_scope_file" \
    "$frontend_image" "$gateway_image" "$worker_image" "$dbtool_image" "$auth_browser_image" "$tts_image" "$bark_image" <<'PYEOF'
import sys, json

out_file = sys.argv[1]
git_sha = sys.argv[2]
release_id = sys.argv[3]
created_at = sys.argv[4]
artifacts_file = sys.argv[5]
scope_file = sys.argv[6]
frontend, gateway, worker, dbtool, auth_browser, tts, bark = sys.argv[7:14]

artifacts = {}
with open(artifacts_file, 'r', encoding='utf-8') as f:
    for line in f:
        line = line.strip()
        if not line:
            continue
        if '=' in line:
            k, v = line.split('=', 1)
            artifacts[k] = v

try:
    with open(scope_file, 'r', encoding='utf-8') as f:
        scope_data = json.load(f)
except Exception as e:
    print(f"Error parsing scope file: {e}", file=sys.stderr)
    sys.exit(1)

default_promotion = {
    "frontend": True,
    "gateway": True,
    "worker": True,
    "schema": True,
    "auth_browser": True,
    "tts": True,
    "bark": True,
    "platform": True
}

promotion = scope_data.get("promotion", default_promotion)
promotion_scope = scope_data.get("promotion_scope", [k for k, v in promotion.items() if v])

manifest = {
    "$schema": "https://json-schema.org/draft/2020-12/schema",
    "schema_version": 1,
    "release_id": release_id,
    "git_sha": git_sha,
    "created_at": created_at,
    "compatibility": {
        "schema_version": 9,
        "min_supported_schema_version": 9,
        "worker_rpc_version": 2,
        "worker_rpc_endpoints": [
            "/rpc/request-sync",
            "/rpc/history-jobs",
            "/rpc/notify-settings-changed",
            "/rpc/wake-dispatcher",
            "/rpc/verify-session"
        ]
    },
    "promotion": promotion,
    "promotion_scope": promotion_scope,
    "images": {
        "frontend": frontend,
        "gateway": gateway,
        "worker": worker,
        "dbtool": dbtool,
        "auth_browser": auth_browser,
        "tts": tts,
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

printf 'Release manifest generated successfully: %s (git_sha: %s, release_id: %s)\n' "$output_file" "$git_sha" "$release_id"
exit 0
