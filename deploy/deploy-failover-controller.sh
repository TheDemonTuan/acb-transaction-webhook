#!/usr/bin/env bash
# Transactionally install the signed failover controller bundle and systemd units.
set -Eeuo pipefail
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deploy/lib.sh
source "$SCRIPT_DIR/lib.sh"
require_release_orchestrator

CANDIDATE_DIR="${FAILOVER_CANDIDATE_DIR:-$SCRIPT_DIR/failover}"
INSTALL_DIR="${FAILOVER_INSTALL_DIR:-/opt/platform/failover}"
REGISTRY_DIR="${FAILOVER_REGISTRY_DIR:-/etc/vps-failover/apps.d}"
SYSTEMD_DIR="${FAILOVER_SYSTEMD_DIR:-/etc/systemd/system}"
BACKUP_ROOT="${FAILOVER_BACKUP_ROOT:-${DATA_DIR:-${DEPLOY_PATH:-$SCRIPT_DIR/..}/data}/failover-controller-backups}"
RELEASE_KEY="${EXPECTED_COMMIT:-$(date -u +%Y%m%d%H%M%S)}"
BACKUP_DIR="$BACKUP_ROOT/$RELEASE_KEY"
INSTALLED=0
COMMITTED=0

run_root() {
  if [[ "$(id -u)" -eq 0 ]]; then
    "$@"
  else
    command -v sudo >/dev/null 2>&1 || { log_error "sudo is required to deploy the failover controller."; return 1; }
    sudo -n "$@"
  fi
}

required_files=(
  vps-failover-controller.py
  vps-failover-controller.service
  vps-failover-reconcile.service
  vps-failover-reconcile.timer
  apps.d/acb.json
  apps.d/auth-browser.json
  apps.d/worker.json
)
for file in "${required_files[@]}"; do
  [[ -f "$CANDIDATE_DIR/$file" && ! -L "$CANDIDATE_DIR/$file" ]] || {
    log_error "Signed failover candidate is missing a regular file: $file"
    exit 1
  }
done

python3 - "$CANDIDATE_DIR/vps-failover-controller.py" <<'PY'
import pathlib, sys
source = pathlib.Path(sys.argv[1]).read_text(encoding="utf-8")
compile(source, sys.argv[1], "exec")
PY
python3 - "$CANDIDATE_DIR/apps.d" <<'PY'
import json, pathlib, sys
root = pathlib.Path(sys.argv[1])
for path in sorted(root.glob("*.json")):
    value = json.loads(path.read_text(encoding="utf-8"))
    if value.get("app") != path.stem:
        raise SystemExit(f"invalid failover registry identity: {path}")
    if value.get("workload_class") not in {"blue_green", "singleton"}:
        raise SystemExit(f"invalid workload class: {path}")
PY
if command -v systemd-analyze >/dev/null 2>&1; then
  systemd-analyze verify \
    "$CANDIDATE_DIR/vps-failover-controller.service" \
    "$CANDIDATE_DIR/vps-failover-reconcile.service" \
    "$CANDIDATE_DIR/vps-failover-reconcile.timer" >/dev/null
fi

candidate_hash="$(sha256sum "$CANDIDATE_DIR/vps-failover-controller.py" | awk '{print $1}')"
installed_hash="$(run_root sha256sum "$INSTALL_DIR/vps-failover-controller.py" 2>/dev/null | awk '{print $1}' || true)"
controller_unchanged=1
[[ "$candidate_hash" == "$installed_hash" ]] || controller_unchanged=0
for unit in vps-failover-controller.service vps-failover-reconcile.service vps-failover-reconcile.timer; do
  run_root cmp -s "$CANDIDATE_DIR/$unit" "$SYSTEMD_DIR/$unit" || controller_unchanged=0
done
for config in "$CANDIDATE_DIR"/apps.d/*.json; do
  run_root cmp -s "$config" "$REGISTRY_DIR/$(basename "$config")" || controller_unchanged=0
done
if [[ "$controller_unchanged" -eq 1 ]] \
  && run_root systemctl is-active --quiet vps-failover-controller.service \
  && run_root systemctl is-active --quiet vps-failover-reconcile.timer; then
  mkdir -p "$BACKUP_DIR"
  chmod 700 "$BACKUP_DIR"
  touch "$BACKUP_DIR/noop"
  log_info "Failover controller bundle is unchanged and both systemd units are healthy; skipping install."
  exit 0
fi

# Provision one shared lock directory. PrivateTmp makes /tmp an invalid coordination path.
deploy_group="$(id -gn)"
run_root install -d -m 0770 -o root -g "$deploy_group" /run/lock/vps-failover
run_root install -d -m 0755 -o root -g root /var/lib/vps-failover
run_root install -d -m 0750 -o root -g "$deploy_group" /var/lib/vps-failover/apps
run_root install -d -m 0770 -o root -g "$deploy_group" /var/lib/vps-failover/apps/acb
if [[ "${DEPLOY_LOCK_HELD:-0}" != "1" ]]; then
  export DEPLOY_LOCK_FILE=/run/lock/vps-failover/acb.lock
  acquire_deploy_lock
fi
[[ "$DEPLOY_LOCK_FILE" == "/run/lock/vps-failover/acb.lock" ]] || {
  log_error "Failover deployment did not inherit the canonical host lock."
  exit 1
}

restore_previous_controller() {
  [[ -d "$BACKUP_DIR" ]] || return 1
  [[ -f "$BACKUP_DIR/noop" ]] && return 0
  log_warn "Restoring the previous failover controller bundle..."
  run_root systemctl stop vps-failover-reconcile.timer vps-failover-controller.service >/dev/null 2>&1 || true
  if [[ -f "$BACKUP_DIR/install.tar" ]]; then
    run_root rm -rf "$INSTALL_DIR"
    run_root mkdir -p "$(dirname "$INSTALL_DIR")"
    run_root tar -xf "$BACKUP_DIR/install.tar" -C /
  else
    run_root rm -rf "$INSTALL_DIR"
  fi
  if [[ -f "$BACKUP_DIR/registry.tar" ]]; then
    run_root rm -rf "$REGISTRY_DIR"
    run_root mkdir -p "$(dirname "$REGISTRY_DIR")"
    run_root tar -xf "$BACKUP_DIR/registry.tar" -C /
  else
    run_root rm -rf "$REGISTRY_DIR"
  fi
  for unit in vps-failover-controller.service vps-failover-reconcile.service vps-failover-reconcile.timer; do
    if [[ -f "$BACKUP_DIR/$unit" ]]; then
      run_root install -m 0644 "$BACKUP_DIR/$unit" "$SYSTEMD_DIR/$unit"
    else
      run_root rm -f "$SYSTEMD_DIR/$unit"
    fi
  done
  run_root systemctl daemon-reload
  if [[ -f "$BACKUP_DIR/controller.enabled" ]]; then
    run_root systemctl enable --now vps-failover-controller.service
    run_root systemctl is-active --quiet vps-failover-controller.service
  else
    run_root systemctl disable --now vps-failover-controller.service >/dev/null 2>&1 || true
  fi
  if [[ -f "$BACKUP_DIR/timer.enabled" ]]; then
    run_root systemctl enable --now vps-failover-reconcile.timer
    run_root systemctl is-active --quiet vps-failover-reconcile.timer
  else
    run_root systemctl disable --now vps-failover-reconcile.timer >/dev/null 2>&1 || true
  fi
  return 0
}

cleanup_controller_deploy() {
  local exit_code=$?
  if [[ "$INSTALLED" -eq 1 && "$COMMITTED" -eq 0 ]]; then
    if restore_previous_controller; then
      log_warn "Previous failover controller restored and verified."
    else
      log_error "Failover controller rollback failed; operator intervention is required."
      exit_code=1
    fi
  fi
  if [[ "${RELEASE_ORCHESTRATED:-0}" != "1" ]]; then
    release_deploy_lock
  fi
  exit "$exit_code"
}
trap cleanup_controller_deploy EXIT HUP INT TERM

if [[ "${FAILOVER_ROLLBACK_ONLY:-0}" == "1" ]]; then
  restore_previous_controller
  log_info "Previous failover controller bundle restored and verified."
  exit 0
fi

mkdir -p "$BACKUP_DIR"
chmod 700 "$BACKUP_DIR"
if run_root test -d "$INSTALL_DIR"; then
  run_root tar -cf "$BACKUP_DIR/install.tar" -C / "${INSTALL_DIR#/}"
fi
if run_root test -d "$REGISTRY_DIR"; then
  run_root tar -cf "$BACKUP_DIR/registry.tar" -C / "${REGISTRY_DIR#/}"
fi
for unit in vps-failover-controller.service vps-failover-reconcile.service vps-failover-reconcile.timer; do
  run_root test -f "$SYSTEMD_DIR/$unit" && run_root cp -a "$SYSTEMD_DIR/$unit" "$BACKUP_DIR/$unit" || true
done
run_root systemctl is-enabled --quiet vps-failover-controller.service && touch "$BACKUP_DIR/controller.enabled" || true
run_root systemctl is-enabled --quiet vps-failover-reconcile.timer && touch "$BACKUP_DIR/timer.enabled" || true

run_root systemctl stop vps-failover-reconcile.timer vps-failover-controller.service >/dev/null 2>&1 || true
INSTALLED=1
run_root install -d -m 0755 -o root -g root "$INSTALL_DIR" "$REGISTRY_DIR"
run_root install -m 0755 "$CANDIDATE_DIR/vps-failover-controller.py" "$INSTALL_DIR/vps-failover-controller.py"
for config in "$CANDIDATE_DIR"/apps.d/*.json; do
  run_root install -m 0644 "$config" "$REGISTRY_DIR/$(basename "$config")"
done
for unit in vps-failover-controller.service vps-failover-reconcile.service vps-failover-reconcile.timer; do
  run_root install -m 0644 "$CANDIDATE_DIR/$unit" "$SYSTEMD_DIR/$unit"
done
run_root systemctl daemon-reload
run_root systemctl enable --now vps-failover-controller.service
run_root systemctl enable --now vps-failover-reconcile.timer
run_root systemctl is-active --quiet vps-failover-controller.service
run_root systemctl is-active --quiet vps-failover-reconcile.timer
actual_hash="$(run_root sha256sum "$INSTALL_DIR/vps-failover-controller.py" | awk '{print $1}')"
[[ "$actual_hash" == "$candidate_hash" ]] || { log_error "Installed failover controller checksum mismatch."; exit 1; }
run_root python3 "$INSTALL_DIR/vps-failover-controller.py" --status >/dev/null
COMMITTED=1
log_info "Failover controller bundle installed and verified: $candidate_hash"
