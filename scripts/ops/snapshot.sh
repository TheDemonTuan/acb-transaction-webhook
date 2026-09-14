#!/usr/bin/env bash
# Host Baseline Snapshot Script (Read-Only)
# Captures OS, Docker, networking, and volume metadata before platform changes.
set -euo pipefail

SNAPSHOT_DIR="${1:-./host-snapshot-$(date -u +%Y%m%d%H%M%S)}"
mkdir -p "$SNAPSHOT_DIR"

printf "Collecting host baseline snapshot to %s...\n" "$SNAPSHOT_DIR"

# 1. System and Kernel
uname -a > "$SNAPSHOT_DIR/uname.txt" 2>&1 || true
cat /etc/os-release > "$SNAPSHOT_DIR/os-release.txt" 2>&1 || true
uptime > "$SNAPSHOT_DIR/uptime.txt" 2>&1 || true
free -m > "$SNAPSHOT_DIR/memory.txt" 2>&1 || true
df -hT > "$SNAPSHOT_DIR/disk.txt" 2>&1 || true

# 2. Security and Firewall
getenforce > "$SNAPSHOT_DIR/selinux.txt" 2>&1 || true
if command -v firewall-cmd >/dev/null 2>&1; then
  sudo firewall-cmd --list-all-zones > "$SNAPSHOT_DIR/firewalld.txt" 2>&1 || true
fi
ss -lnt > "$SNAPSHOT_DIR/listening-ports.txt" 2>&1 || true

# 3. Docker Runtime & Containers
if command -v docker >/dev/null 2>&1; then
  docker version --format '{{json .}}' > "$SNAPSHOT_DIR/docker-version.json" 2>&1 || true
  docker info --format 'live_restore={{.LiveRestoreEnabled}} logging={{.LoggingDriver}} cgroups={{.CgroupVersion}}' > "$SNAPSHOT_DIR/docker-info.txt" 2>&1 || true
  docker ps -a --format 'table {{.Names}}\t{{.Image}}\t{{.Status}}\t{{.Ports}}' > "$SNAPSHOT_DIR/docker-ps.txt" 2>&1 || true
  docker network ls --format 'table {{.Name}}\t{{.Driver}}\t{{.Scope}}' > "$SNAPSHOT_DIR/docker-networks.txt" 2>&1 || true
  
  # Inspect container mounts and networks without secrets
  for c in $(docker ps -a --format '{{.Names}}'); do
    docker inspect "$c" --format '{{.Name}} mounts={{json .Mounts}} networks={{json .NetworkSettings.Networks}}' >> "$SNAPSHOT_DIR/container-mounts.txt" 2>&1 || true
  done
  
  # Volume inventory
  docker volume ls > "$SNAPSHOT_DIR/docker-volumes.txt" 2>&1 || true
fi

printf "Host snapshot completed: %s\n" "$SNAPSHOT_DIR"
