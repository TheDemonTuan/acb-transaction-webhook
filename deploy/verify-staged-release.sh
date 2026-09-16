#!/usr/bin/env bash
# Validate the exact staged deploy bundle before any VPS connection.
set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR="$SCRIPT_DIR"
MANIFEST=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --deploy-dir) DEPLOY_DIR="$2"; shift 2 ;;
    --manifest) MANIFEST="$2"; shift 2 ;;
    *) printf 'Unknown staged release argument: %s\n' "$1" >&2; exit 1 ;;
  esac
done

DEPLOY_DIR="$(cd -- "$DEPLOY_DIR" && pwd -P)"
MANIFEST="${MANIFEST:-$DEPLOY_DIR/release-manifest.json}"
[[ -s "$MANIFEST" ]] || { printf 'Staged release manifest is missing: %s\n' "$MANIFEST" >&2; exit 1; }

python3 - "$MANIFEST" "$DEPLOY_DIR" <<'PY'
import hashlib, json, pathlib, re, sys
manifest_path = pathlib.Path(sys.argv[1]).resolve(strict=True)
root = pathlib.Path(sys.argv[2]).resolve(strict=True)
manifest = json.loads(manifest_path.read_text(encoding='utf-8'))
artifacts = manifest.get('artifacts')
if not isinstance(artifacts, dict) or not artifacts:
    raise SystemExit('Staged manifest has no artifact set')
for name, digest in artifacts.items():
    if name in {'compose_bundle', 'compose_bundle_sha256', 'failover-bundle', 'failover_bundle_sha256'}:
        continue
    rel = pathlib.PurePosixPath(name)
    if rel.is_absolute() or '..' in rel.parts or not re.fullmatch(r'[0-9a-f]{64}', digest):
        raise SystemExit(f'Invalid staged artifact entry: {name}')
    target = root.joinpath(*rel.parts)
    if target.is_symlink() or not target.is_file() or not target.resolve().is_relative_to(root):
        raise SystemExit(f'Missing or unsafe staged artifact: {name}')
    if hashlib.sha256(target.read_bytes()).hexdigest() != digest:
        raise SystemExit(f'Staged artifact hash mismatch: {name}')
base = manifest.get('base')
if not isinstance(base, dict) or not re.fullmatch(r'[0-9a-f]{40}', base.get('git_sha', '')) or type(base.get('generation')) is not int or base['generation'] < 0:
    raise SystemExit('Signed staged manifest has an invalid production baseline')
PY

required=(
  compose.prod.yaml compose/base.yaml compose/gateway.yaml compose/frontend.yaml
  compose/worker.yaml compose/auth-browser.yaml compose/tts.yaml compose/bark.yaml
  compose/dbtool.yaml lib/common.sh runtime-layout.sh dispatch-rollout.sh
  stable-deployer.sh bootstrap-deployment-engine.sh preflight-runtime.sh
  verify-release-baseline.sh stage-immutable-release.sh verify-manifest.sh
)
for file in "${required[@]}"; do
  [[ -f "$DEPLOY_DIR/$file" && ! -L "$DEPLOY_DIR/$file" ]] || {
    printf 'Required staged artifact missing or unsafe: %s\n' "$file" >&2
    exit 1
  }
done

# Source only the local trusted checkout helper; config renders the exact staged files.
export RELEASE_DIR="$DEPLOY_DIR"
export RELEASE_CONTEXT_DIR="$DEPLOY_DIR"
export COMPOSE_ROOT="$DEPLOY_DIR/compose"
export DEPLOY_DIR

# Extract candidate image refs from manifest for compose interpolation
eval "$(python3 - "$MANIFEST" <<'PY'
import json, sys
try:
    data = json.load(open(sys.argv[1], encoding='utf-8'))
    imgs = data.get("images", {})
    gw = imgs.get("gateway")
    if isinstance(gw, dict):
        gw_blue = gw.get("blue", "")
        gw_green = gw.get("green", "")
    else:
        gw_blue = gw or ""
        gw_green = gw or ""
    fe = imgs.get("frontend", "")
    wk = imgs.get("worker", "")
    db = imgs.get("dbtool", "")
    br = imgs.get("auth_browser", "")
    tts = imgs.get("tts", "")
    bark = imgs.get("bark", "")
    dummy = "ghcr.io/test/app@sha256:0000000000000000000000000000000000000000000000000000000000000000"
    print(f"export IMAGE_REF_BLUE='{gw_blue or dummy}'")
    print(f"export IMAGE_REF_GREEN='{gw_green or dummy}'")
    print(f"export FRONTEND_IMAGE_REF='{fe or dummy}'")
    print(f"export WORKER_IMAGE_REF='{wk or dummy}'")
    print(f"export DBTOOL_IMAGE_REF='{db or dummy}'")
    print(f"export BROWSER_IMAGE_REF='{br or dummy}'")
    print(f"export TTS_IMAGE_REF='{tts or dummy}'")
    print(f"export BARK_IMAGE_REF='{bark or dummy}'")
except Exception:
    pass
PY
)"

# If .env.production or secrets are absent (e.g. In CI runner prior to remote deployment), provide temporary placeholder
created_dummy_env=0
if [[ ! -f "$DEPLOY_DIR/.env.production" ]]; then
  touch "$DEPLOY_DIR/.env.production"
  created_dummy_env=1
fi

created_dummy_secrets=()
mkdir -p "$DEPLOY_DIR/secrets"
for s in app_master_key tts_internal_token worker_internal_token bark_basic_auth_user bark_basic_auth_password; do
  if [[ ! -f "$DEPLOY_DIR/secrets/$s" ]]; then
    touch "$DEPLOY_DIR/secrets/$s"
    created_dummy_secrets+=("$DEPLOY_DIR/secrets/$s")
  fi
done

# shellcheck source=deploy/lib/common.sh
source "$DEPLOY_DIR/lib/common.sh"
compose_prod config --quiet >/dev/null

if [[ "$created_dummy_env" -eq 1 ]]; then
  rm -f "$DEPLOY_DIR/.env.production"
fi
for sf in "${created_dummy_secrets[@]}"; do
  rm -f "$sf"
done

bash -n "$DEPLOY_DIR"/*.sh "$DEPLOY_DIR"/lib/*.sh
python3 -m py_compile "$DEPLOY_DIR/release-state.py"
printf 'STAGED_RELEASE_ACCEPTED: %s\n' "$DEPLOY_DIR"
