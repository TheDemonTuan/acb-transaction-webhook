#!/usr/bin/env bash
# deploy/verify-release-baseline.sh
# Verifies signed release manifest base against canonical production state.
# Fails with STALE_RELEASE_BASELINE if production generation or git_sha changed after build.
# Fails on malformed baseline identity.
# Permits deployment only under explicit documented bootstrap mode when canonical state or baseline is absent.
set -Eeuo pipefail

manifest_file=""
state_file=""
allow_bootstrap="${ALLOW_BOOTSTRAP:-0}"

log_info() {
  printf '[INFO] %s\n' "$*"
}

log_warn() {
  printf '[WARN] %s\n' "$*" >&2
}

log_error() {
  printf '[ERROR] %s\n' "$*" >&2
}

usage() {
  cat <<'EOF'
Usage: verify-release-baseline.sh [options]

Verifies that the signed release manifest's base baseline identity matches the
target host's canonical state (git_sha and generation).

Options:
  --manifest <path>        Path to release-manifest.json
  --state <path>           Path to canonical state file (e.g. current-release.json)
  --canonical-state <path> Alias for --state
  --allow-bootstrap        Permit deployment when canonical state or manifest baseline is absent
                           (strictly for documented initial VPS bootstrapping)
  -h, --help               Show this help message
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --manifest)
      manifest_file="$2"
      shift 2
      ;;
    --state|--canonical-state)
      state_file="$2"
      shift 2
      ;;
    --allow-bootstrap)
      allow_bootstrap=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      log_error "Unknown argument: $1"
      usage >&2
      exit 1
      ;;
  esac
done

if [[ "$allow_bootstrap" =~ ^(1|true|TRUE|yes|YES)$ ]]; then
  allow_bootstrap=1
else
  allow_bootstrap=0
fi

if [[ -z "$manifest_file" ]]; then
  if [[ -n "${RELEASE_DIR:-}" && -f "$RELEASE_DIR/release-manifest.json" ]]; then
    manifest_file="$RELEASE_DIR/release-manifest.json"
  elif [[ -f "release-manifest.json" ]]; then
    manifest_file="release-manifest.json"
  elif [[ -f "deploy/release-manifest.json" ]]; then
    manifest_file="deploy/release-manifest.json"
  else
    log_error "Manifest file is required and release-manifest.json was not found in default locations."
    exit 1
  fi
fi

if [[ ! -f "$manifest_file" ]]; then
  log_error "Release manifest file not found: $manifest_file"
  exit 1
fi

if [[ -z "$state_file" ]]; then
  state_file="${CURRENT_RELEASE_FILE:-${RUNTIME_STATE_DIR:-${DEPLOY_PATH:-/opt/acb-transaction-webhook}/state}/current-release.json}"
fi

PYTHON_BIN=""
if command -v python3 >/dev/null 2>&1; then
  PYTHON_BIN="python3"
elif command -v python >/dev/null 2>&1; then
  PYTHON_BIN="python"
else
  log_error "python3 or python interpreter is required to verify release baseline."
  exit 1
fi

"$PYTHON_BIN" - "$manifest_file" "$state_file" "$allow_bootstrap" <<'PY'
import json
import os
import re
import sys

manifest_path = sys.argv[1]
state_path = sys.argv[2]
allow_bootstrap = sys.argv[3].strip() == '1'

HEX40_RE = re.compile(r'^[0-9a-f]{40}$')

def die_malformed(msg):
    sys.stderr.write(f'Error: malformed identity: {msg}\n')
    sys.stderr.write(f'STALE_RELEASE_BASELINE: malformed baseline identity: {msg}; start a new workflow run.\n')
    sys.exit(1)

def die_stale(msg):
    sys.stderr.write(f'STALE_RELEASE_BASELINE: {msg}; start a new workflow run.\n')
    sys.exit(1)

# 1. Parse manifest
try:
    with open(manifest_path, 'r', encoding='utf-8') as f:
        manifest = json.load(f)
except Exception as e:
    sys.stderr.write(f'Error: failed to parse manifest JSON ({manifest_path}): {e}\n')
    sys.exit(1)

if not isinstance(manifest, dict):
    die_malformed(f'manifest root must be a JSON object, got {type(manifest).__name__}')

manifest_has_base = 'base' in manifest and manifest['base'] is not None

# 2. Check canonical state existence
state_exists = os.path.exists(state_path) and os.path.getsize(state_path) > 0

# Bootstrap checks
if allow_bootstrap:
    if not state_exists:
        sys.stdout.write('BOOTSTRAP: canonical state is absent; bootstrap mode permitted.\n')
        sys.exit(0)
    if not manifest_has_base:
        sys.stdout.write('BOOTSTRAP: manifest baseline is absent; bootstrap mode permitted.\n')
        sys.exit(0)

# Non-bootstrap requirements
if not manifest_has_base:
    sys.stderr.write(f'Error: manifest missing base identity: base.git_sha and base.generation are required.\n')
    die_stale('manifest missing base baseline identity (only permitted with explicit --allow-bootstrap)')

if not state_exists:
    sys.stderr.write(f'Error: canonical release state missing: {state_path}\n')
    die_stale('canonical release state missing (only permitted with explicit --allow-bootstrap)')

# Validate manifest base identity
base = manifest['base']
if not isinstance(base, dict):
    die_malformed(f'manifest base must be a JSON object, got {type(base).__name__}')

if 'git_sha' not in base or 'generation' not in base:
    die_malformed('manifest base must contain both git_sha and generation')

base_sha = base['git_sha']
base_gen = base['generation']

if not isinstance(base_sha, str) or not HEX40_RE.fullmatch(base_sha):
    die_malformed(f'manifest base.git_sha must be a 40-character lowercase hex string, got {base_sha!r}')

if isinstance(base_gen, bool) or not isinstance(base_gen, int) or base_gen < 0:
    die_malformed(f'manifest base.generation must be a non-negative integer, got {base_gen!r}')

# 3. Read and validate canonical state identity
try:
    with open(state_path, 'r', encoding='utf-8') as f:
        state = json.load(f)
except Exception as e:
    sys.stderr.write(f'Error: failed to parse canonical state JSON ({state_path}): {e}\n')
    sys.exit(1)

if not isinstance(state, dict):
    die_malformed(f'canonical state root must be a JSON object, got {type(state).__name__}')

if 'git_sha' not in state or 'generation' not in state:
    die_malformed('canonical state missing git_sha or generation')

state_sha = state['git_sha']
state_gen = state['generation']

if not isinstance(state_sha, str) or not HEX40_RE.fullmatch(state_sha):
    die_malformed(f'canonical state git_sha must be a 40-character lowercase hex string, got {state_sha!r}')

if isinstance(state_gen, bool) or not isinstance(state_gen, int) or state_gen < 0:
    die_malformed(f'canonical state generation must be a non-negative integer, got {state_gen!r}')

# 4. Compare baseline identity to canonical state
if state_gen != base_gen:
    die_stale(f'production generation changed after build (canonical: {state_gen}, manifest base: {base_gen})')

if state_sha != base_sha:
    die_stale(f'production git_sha changed after build (canonical: {state_sha}, manifest base: {base_sha})')

sys.stdout.write(f'RELEASE_BASELINE_OK: verified manifest base matches canonical state (generation: {state_gen}, git_sha: {state_sha}).\n')
sys.exit(0)
PY
