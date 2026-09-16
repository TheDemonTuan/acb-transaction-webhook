#!/usr/bin/env bash
# Trusted host-side release verifier. Install separately and update only after a verified rollout.
set -Eeuo pipefail
umask 077

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
RELEASE_DIR=""
DATA_DIR=""
EXPECTED_IDENTITY=""
EXPECTED_ISSUER="https://token.actions.githubusercontent.com"
SOAK_SECONDS="900"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --release-dir) RELEASE_DIR="$2"; shift 2 ;;
    --data-dir) DATA_DIR="$2"; shift 2 ;;
    --expected-identity) EXPECTED_IDENTITY="$2"; shift 2 ;;
    --expected-issuer) EXPECTED_ISSUER="$2"; shift 2 ;;
    --soak-seconds) SOAK_SECONDS="$2"; shift 2 ;;
    *) printf 'Unknown stable-deployer argument: %s\n' "$1" >&2; exit 1 ;;
  esac
done

[[ -n "$RELEASE_DIR" && -d "$RELEASE_DIR" ]] || { printf 'Release directory is missing.\n' >&2; exit 1; }
[[ -n "$DATA_DIR" ]] || { printf 'Data directory is required.\n' >&2; exit 1; }
[[ -n "$EXPECTED_IDENTITY" && "$EXPECTED_IDENTITY" != '*' && "$EXPECTED_IDENTITY" != '.*' ]] || {
  printf 'Exact signing identity is required.\n' >&2
  exit 1
}
[[ "$SOAK_SECONDS" =~ ^[0-9]+$ ]] || { printf 'Invalid soak duration.\n' >&2; exit 1; }

RELEASE_DIR="$(cd -- "$RELEASE_DIR" && pwd -P)"
export RELEASE_DIR
export RELEASE_CONTEXT_DIR="$RELEASE_DIR"
DATA_DIR="$(mkdir -p "$DATA_DIR" && cd -- "$DATA_DIR" && pwd -P)"
case "$RELEASE_DIR" in
  /|/tmp|/var/tmp) printf 'Unsafe release directory: %s\n' "$RELEASE_DIR" >&2; exit 1 ;;
esac
[[ -f "$RELEASE_DIR/release-manifest.json" && -f "$RELEASE_DIR/release-manifest.bundle" ]] || {
  printf 'Signed release manifest is missing.\n' >&2
  exit 1
}
[[ -x "$SCRIPT_DIR/verify-manifest.sh" ]] || { printf 'Trusted manifest verifier is missing.\n' >&2; exit 1; }

# Step 1: Verify manifest with trusted verifier.
# If trusted verifier fails (e.g. old manifest schema deadlock), attempt schema-light bootstrap.
if ! "$SCRIPT_DIR/verify-manifest.sh" \
  --manifest "$RELEASE_DIR/release-manifest.json" \
  --bundle "$RELEASE_DIR/release-manifest.bundle" \
  --deploy-dir "$RELEASE_DIR" \
  --expected-identity "$EXPECTED_IDENTITY" \
  --expected-issuer "$EXPECTED_ISSUER" \
  --require-cosign; then
  if [[ -f "$SCRIPT_DIR/bootstrap-deployment-engine.sh" ]]; then
    printf 'Trusted manifest verifier failed. Attempting signed engine bootstrap...\n'
    bash "$SCRIPT_DIR/bootstrap-deployment-engine.sh" \
      --release-dir "$RELEASE_DIR" \
      --runtime-deploy-dir "$SCRIPT_DIR" \
      --expected-identity "$EXPECTED_IDENTITY" \
      --expected-issuer "$EXPECTED_ISSUER" \
      --require-cosign \
      --skip-semantic-check
    printf 'Signed engine bootstrap succeeded. Retrying manifest verification with updated verifier...\n'
    bash "$SCRIPT_DIR/verify-manifest.sh" \
      --manifest "$RELEASE_DIR/release-manifest.json" \
      --bundle "$RELEASE_DIR/release-manifest.bundle" \
      --deploy-dir "$RELEASE_DIR" \
      --expected-identity "$EXPECTED_IDENTITY" \
      --expected-issuer "$EXPECTED_ISSUER" \
      --require-cosign
  else
    printf 'Manifest verification failed and bootstrap-deployment-engine.sh not available.\n' >&2
    exit 1
  fi
fi

# Determine runtime root and source runtime layout
RUNTIME_ROOT="${RUNTIME_ROOT:-${DEPLOY_PATH:-$(cd -- "$SCRIPT_DIR/.." && pwd -P)}}"
export RUNTIME_ROOT

if [[ -f "$SCRIPT_DIR/runtime-layout.sh" ]]; then
  # shellcheck source=deploy/runtime-layout.sh
  source "$SCRIPT_DIR/runtime-layout.sh"
elif [[ -f "$RELEASE_DIR/runtime-layout.sh" ]]; then
  # shellcheck source=deploy/runtime-layout.sh
  source "$RELEASE_DIR/runtime-layout.sh"
fi

# One-time atomic path migration from legacy locations to canonical runtime-state locations
migrate_runtime_state_path() {
  local old_path="$1"
  local new_path="$2"
  if [[ -e "$old_path" ]]; then
    if [[ -e "$new_path" ]]; then
      local old_sum new_sum
      old_sum="$(sha256sum "$old_path" | awk '{print $1}')"
      new_sum="$(sha256sum "$new_path" | awk '{print $1}')"
      if [[ "$old_sum" != "$new_sum" ]]; then
        printf 'Conflicting state between legacy (%s) and canonical (%s); failing closed.\n' "$old_path" "$new_path" >&2
        exit 1
      fi
      rm -f "$old_path"
    else
      local parent tmp
      parent="$(dirname "$new_path")"
      mkdir -p "$parent"
      tmp="$(mktemp "${parent}/.$(basename "$new_path").tmp.XXXXXX")"
      cp -p "$old_path" "$tmp"
      python3 - "$tmp" "$parent" <<'PY'
import os, sys
p, parent = sys.argv[1:3]
try:
    with open(p, 'r+b') as f:
        os.fsync(f.fileno())
except OSError:
    pass
try:
    pfd = os.open(parent, os.O_RDONLY | getattr(os, 'O_DIRECTORY', 0))
    try:
        os.fsync(pfd)
    finally:
        os.close(pfd)
except OSError:
    pass
PY
      mv -f "$tmp" "$new_path"
      python3 - "$parent" <<'PY'
import os, sys
try:
    pfd = os.open(sys.argv[1], os.O_RDONLY | getattr(os, 'O_DIRECTORY', 0))
    try:
        os.fsync(pfd)
    finally:
        os.close(pfd)
except OSError:
    pass
PY
      rm -f "$old_path"
    fi
  fi
}

runtime_dir="$SCRIPT_DIR"
mkdir -p "$RUNTIME_ROOT/state" "$RUNTIME_ROOT/data"

migrate_runtime_state_path "$runtime_dir/.active-slot" "${ACTIVE_SLOT_FILE:-$RUNTIME_ROOT/state/gateway-active-slot}"
migrate_runtime_state_path "$runtime_dir/.previous-slot" "${PREVIOUS_SLOT_FILE:-$RUNTIME_ROOT/state/gateway-previous-slot}"
migrate_runtime_state_path "$runtime_dir/.active-frontend-slot" "${FRONTEND_ACTIVE_SLOT_FILE:-$RUNTIME_ROOT/state/frontend-active-slot}"
migrate_runtime_state_path "$runtime_dir/.previous-frontend-slot" "${FRONTEND_PREVIOUS_SLOT_FILE:-$RUNTIME_ROOT/state/frontend-previous-slot}"
migrate_runtime_state_path "$runtime_dir/.deploy-state" "${DEPLOY_STATE_FILE:-$RUNTIME_ROOT/state/deploy-state.json}"
migrate_runtime_state_path "$runtime_dir/.soak-state" "${SOAK_STATE_FILE:-$RUNTIME_ROOT/state/soak-state.env}"

for required in .env.production .release.env secrets; do
  [[ -e "$runtime_dir/$required" ]] || { printf 'Missing canonical runtime state: %s\n' "$runtime_dir/$required" >&2; exit 1; }
done
for required in .env.production .release.env secrets; do
  if [[ ! -e "$RELEASE_DIR/$required" && ! -L "$RELEASE_DIR/$required" ]]; then
    ln -s "$runtime_dir/$required" "$RELEASE_DIR/$required"
  fi
done

export DEPLOY_LOCK_FILE="${DEPLOY_LOCK_FILE:-/run/lock/vps-failover/acb.lock}"
if [[ -z "${ALLOW_TEST_LOCK_PATH:-}" ]]; then
  deploy_group="$(id -gn 2>/dev/null || id -g)"
  if [[ "$(id -u)" -eq 0 ]]; then
    install -d -m 0770 -o root -g "$deploy_group" /run/lock/vps-failover
    install -d -m 0755 -o root -g root /var/lib/vps-failover
    install -d -m 0750 -o root -g "$deploy_group" /var/lib/vps-failover/apps
    install -d -m 0770 -o root -g "$deploy_group" /var/lib/vps-failover/apps/acb
  else
    command -v sudo >/dev/null 2>&1 || { printf 'sudo is required to provision canonical deployment locks.\n' >&2; exit 1; }
    sudo -n install -d -m 0770 -o root -g "$deploy_group" /run/lock/vps-failover
    sudo -n install -d -m 0755 -o root -g root /var/lib/vps-failover
    sudo -n install -d -m 0750 -o root -g "$deploy_group" /var/lib/vps-failover/apps
    sudo -n install -d -m 0770 -o root -g "$deploy_group" /var/lib/vps-failover/apps/acb
  fi
  [[ -w /run/lock/vps-failover && -w /var/lib/vps-failover/apps/acb ]] || {
    printf 'Canonical deployment lock/state directories are not writable.\n' >&2
    exit 1
  }
fi
export PATH="$runtime_dir:$PATH"
if [[ -f "$RELEASE_DIR/edge-probe.sh" ]]; then
  export EDGE_PROBE_SCRIPT="$RELEASE_DIR/edge-probe.sh"
fi

# Preflight VPS runtime immediately before candidate rollout
preflight_script=""
if [[ -f "$SCRIPT_DIR/preflight-vps.sh" ]]; then
  preflight_script="$SCRIPT_DIR/preflight-vps.sh"
elif [[ -f "$RELEASE_DIR/preflight-vps.sh" ]]; then
  preflight_script="$RELEASE_DIR/preflight-vps.sh"
fi
if [[ -n "$preflight_script" ]]; then
  bash "$preflight_script" \
    --state "${CURRENT_RELEASE_FILE:-$RUNTIME_ROOT/state/current-release.json}" \
    --data-dir "$DATA_DIR" \
    --config "${ACB_CONFIG:-/opt/platform/edge/dynamic/acb.yml}"
fi

bash "$RELEASE_DIR/dispatch-rollout.sh" \
  --manifest "$RELEASE_DIR/release-manifest.json" \
  --bundle "$RELEASE_DIR/release-manifest.bundle" \
  --deploy-dir "$RELEASE_DIR" \
  --runtime-root "$RUNTIME_ROOT" \
  --data-dir "$DATA_DIR" \
  --expected-identity "$EXPECTED_IDENTITY" \
  --expected-issuer "$EXPECTED_ISSUER" \
  --require-cosign \
  --soak-seconds "$SOAK_SECONDS"

# Atomically promote verifier/launcher and engine components after a successful release
if [[ -x "$SCRIPT_DIR/bootstrap-deployment-engine.sh" ]]; then
  "$SCRIPT_DIR/bootstrap-deployment-engine.sh" \
    --release-dir "$RELEASE_DIR" \
    --runtime-deploy-dir "$runtime_dir" \
    --expected-identity "$EXPECTED_IDENTITY" \
    --expected-issuer "$EXPECTED_ISSUER" \
    --require-cosign \
    --skip-semantic-check
else
  ENGINE_ALLOWLIST=(
    "stable-deployer.sh"
    "verify-manifest.sh"
    "runtime-layout.sh"
    "reconcile-release.sh"
    "release-state.py"
    "verify-runtime-drift.sh"
    "release-env.sh"
    "edge-probe.sh"
    "lib.sh"
    "lib/common.sh"
    "lib/database.sh"
    "lib/images.sh"
    "lib/recovery.sh"
    "lib/rollout-journal.sh"
    "lib/state.sh"
    "lib/traefik.sh"
    "preflight-vps.sh"
    "preflight-runtime.sh"
    "bootstrap-deployment-engine.sh"
  )
  for file in "${ENGINE_ALLOWLIST[@]}"; do
    if [[ -f "$RELEASE_DIR/$file" ]]; then
      flat_name="${file//\//_}"
      tmp="$runtime_dir/.${flat_name}.next.$$"
      mode="0755"
      [[ "$file" != *.py ]] || mode="0644"
      mkdir -p "$(dirname "$runtime_dir/$file")"
      install -m "$mode" "$RELEASE_DIR/$file" "$tmp"
      mv -f "$tmp" "$runtime_dir/$file"
    fi
  done
fi
