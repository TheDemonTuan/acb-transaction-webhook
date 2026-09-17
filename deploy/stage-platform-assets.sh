#!/usr/bin/env bash
set -Eeuo pipefail
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "$SCRIPT_DIR/.." && pwd)"
rm -rf "$SCRIPT_DIR/failover"
mkdir -p "$SCRIPT_DIR/failover/apps.d"
rm -rf "$SCRIPT_DIR/edge"
mkdir -p "$SCRIPT_DIR/edge/dynamic"
install -m 0755 "$REPO_ROOT/platform/edge/probe.sh" "$SCRIPT_DIR/edge-probe.sh"
install -m 0644 "$REPO_ROOT/platform/edge/dynamic/middlewares.yml" "$SCRIPT_DIR/edge/dynamic/middlewares.yml"
install -m 0644 "$REPO_ROOT/platform/edge/dynamic/portfolio.yml" "$SCRIPT_DIR/edge/dynamic/portfolio.yml"
install -m 0644 "$REPO_ROOT/platform/edge/dynamic/bark.yml" "$SCRIPT_DIR/edge/dynamic/bark.yml"
install -m 0755 "$REPO_ROOT/platform/failover/vps-failover-controller.py" "$SCRIPT_DIR/failover/vps-failover-controller.py"
install -m 0644 "$REPO_ROOT/platform/failover/vps-failover-controller.service" "$SCRIPT_DIR/failover/vps-failover-controller.service"
install -m 0644 "$REPO_ROOT/platform/failover/vps-failover-reconcile.service" "$SCRIPT_DIR/failover/vps-failover-reconcile.service"
install -m 0644 "$REPO_ROOT/platform/failover/vps-failover-reconcile.timer" "$SCRIPT_DIR/failover/vps-failover-reconcile.timer"
install -m 0644 "$REPO_ROOT/platform/failover/apps.d/"*.json "$SCRIPT_DIR/failover/apps.d/"
chmod 0755 "$SCRIPT_DIR/deploy-failover-controller.sh" "$SCRIPT_DIR/verify-runtime-drift.sh"
if [[ -f "$REPO_ROOT/deploy/runtime-layout.sh" ]]; then
  chmod 0755 "$REPO_ROOT/deploy/runtime-layout.sh"
fi
