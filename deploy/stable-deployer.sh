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
DATA_DIR="$(mkdir -p "$DATA_DIR" && cd -- "$DATA_DIR" && pwd -P)"
case "$RELEASE_DIR" in
  /|/tmp|/var/tmp) printf 'Unsafe release directory: %s\n' "$RELEASE_DIR" >&2; exit 1 ;;
esac
[[ -f "$RELEASE_DIR/release-manifest.json" && -f "$RELEASE_DIR/release-manifest.bundle" ]] || {
  printf 'Signed release manifest is missing.\n' >&2
  exit 1
}
[[ -x "$SCRIPT_DIR/verify-manifest.sh" ]] || { printf 'Trusted manifest verifier is missing.\n' >&2; exit 1; }

"$SCRIPT_DIR/verify-manifest.sh" \
  --manifest "$RELEASE_DIR/release-manifest.json" \
  --bundle "$RELEASE_DIR/release-manifest.bundle" \
  --deploy-dir "$RELEASE_DIR" \
  --expected-identity "$EXPECTED_IDENTITY" \
  --expected-issuer "$EXPECTED_ISSUER" \
  --require-cosign

runtime_dir="$SCRIPT_DIR"
for required in .env.production .release.env secrets; do
  [[ -e "$runtime_dir/$required" ]] || { printf 'Missing canonical runtime state: %s\n' "$runtime_dir/$required" >&2; exit 1; }
done
for required in .env.production .release.env secrets; do
  if [[ ! -e "$RELEASE_DIR/$required" && ! -L "$RELEASE_DIR/$required" ]]; then
    ln -s "$runtime_dir/$required" "$RELEASE_DIR/$required"
  fi
done

export ENV_FILE="$runtime_dir/.env.production"
export RELEASE_ENV_FILE="$runtime_dir/.release.env"
export SECRETS_DIR="$runtime_dir/secrets"
export ACTIVE_SLOT_FILE="$runtime_dir/.active-slot"
export PREVIOUS_SLOT_FILE="$runtime_dir/.previous-slot"
export DEPLOY_STATE_FILE="$runtime_dir/.deploy-state"
export SOAK_STATE_FILE="$runtime_dir/.soak-state"
export DEPLOY_LOCK_FILE="${DEPLOY_LOCK_FILE:-/run/lock/vps-failover/acb.lock}"
deploy_group="$(id -gn)"
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
export PATH="$runtime_dir:$PATH"
if [[ -f "$RELEASE_DIR/edge-probe.sh" ]]; then
  export EDGE_PROBE_SCRIPT="$RELEASE_DIR/edge-probe.sh"
fi

bash "$RELEASE_DIR/dispatch-rollout.sh" \
  --manifest "$RELEASE_DIR/release-manifest.json" \
  --bundle "$RELEASE_DIR/release-manifest.bundle" \
  --deploy-dir "$RELEASE_DIR" \
  --data-dir "$DATA_DIR" \
  --expected-identity "$EXPECTED_IDENTITY" \
  --expected-issuer "$EXPECTED_ISSUER" \
  --require-cosign \
  --soak-seconds "$SOAK_SECONDS"

# The candidate is already covered by the signed manifest and artifact hashes.
for file in stable-deployer.sh verify-manifest.sh; do
  tmp="$runtime_dir/.${file}.next.$$"
  install -m 0755 "$RELEASE_DIR/$file" "$tmp"
  mv -f "$tmp" "$runtime_dir/$file"
done
