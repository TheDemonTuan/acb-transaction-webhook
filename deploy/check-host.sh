#!/usr/bin/env bash
# Host Preflight Audit Script for Single-VPS Platform
set -euo pipefail

printf "=== HOST PREFLIGHT AUDIT ===\n"

# 1. Check Docker & Compose
if ! command -v docker >/dev/null 2>&1; then
  printf "FAIL: Docker is not installed or not in PATH\n" >&2
  exit 1
fi
DOCKER_VER=$(docker version --format '{{.Server.Version}}' 2>/dev/null || echo "unknown")
printf "Docker Server Version: %s\n" "$DOCKER_VER"

if ! docker compose version >/dev/null 2>&1; then
  printf "FAIL: Docker Compose plugin is not available\n" >&2
  exit 1
fi

# 2. Check Disk Space (> 10GB free)
FREE_KB=$(df --output=avail / | tail -n 1 | tr -d '[:space:]')
FREE_GB=$(( FREE_KB / 1024 / 1024 ))
printf "Available Disk on /: %d GB\n" "$FREE_GB"
if [[ $FREE_GB -lt 10 ]]; then
  printf "WARNING: Less than 10GB free disk space available\n" >&2
fi

# 3. Check Free Memory (> 2GB free/available)
AVAIL_MEM_MB=$(free -m | awk '/^Mem:/{print $7}')
printf "Available Memory: %d MB\n" "$AVAIL_MEM_MB"
if [[ $AVAIL_MEM_MB -lt 2000 ]]; then
  printf "WARNING: Less than 2000MB available memory\n" >&2
fi

# 4. Check SELinux & Firewall
if command -v getenforce >/dev/null 2>&1; then
  SELINUX=$(getenforce)
  printf "SELinux State: %s\n" "$SELINUX"
fi

# 5. Check Required External Docker Networks
for net in edge-acb acb-core acb-egress; do
  if docker network inspect "$net" >/dev/null 2>&1; then
    printf "Network [%s]: OK\n" "$net"
  else
    printf "Network [%s]: NOT FOUND (will be created automatically on deploy)\n" "$net"
  fi
done

printf "=== PREFLIGHT AUDIT COMPLETED ===\n"
