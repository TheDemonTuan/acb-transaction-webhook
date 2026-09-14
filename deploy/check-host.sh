#!/usr/bin/env bash
# Host Preflight Audit Script for Single-VPS Platform
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deploy/lib.sh
source "$SCRIPT_DIR/lib.sh"

log_info "=== HOST PREFLIGHT AUDIT ==="

# 1. Check Docker & Compose
if ! command -v docker >/dev/null 2>&1; then
  log_error "Docker is not installed or not in PATH"
  exit 1
fi
DOCKER_VER=$(docker version --format '{{.Server.Version}}' 2>/dev/null || echo "unknown")
log_info "Docker Server Version: ${DOCKER_VER}"

if ! docker compose version >/dev/null 2>&1; then
  log_error "Docker Compose plugin is not available"
  exit 1
fi

# 2. Check Disk Space (> 10GB free)
if command -v df >/dev/null 2>&1; then
  FREE_KB=$(df --output=avail / 2>/dev/null | tail -n 1 | tr -d '[:space:]' || echo "0")
  if [[ "$FREE_KB" =~ ^[0-9]+$ ]] && [[ "$FREE_KB" -gt 0 ]]; then
    FREE_GB=$(( FREE_KB / 1024 / 1024 ))
    log_info "Available Disk on /: ${FREE_GB} GB"
    if [[ $FREE_GB -lt 10 ]]; then
      log_warn "Less than 10GB free disk space available"
    fi
  fi
fi

# 3. Check Free Memory (> 2GB free/available)
if command -v free >/dev/null 2>&1; then
  AVAIL_MEM_MB=$(free -m 2>/dev/null | awk '/^Mem:/{print $7}' || echo "0")
  if [[ "$AVAIL_MEM_MB" =~ ^[0-9]+$ ]] && [[ "$AVAIL_MEM_MB" -gt 0 ]]; then
    log_info "Available Memory: ${AVAIL_MEM_MB} MB"
    if [[ $AVAIL_MEM_MB -lt 2000 ]]; then
      log_warn "Less than 2000MB available memory"
    fi
  fi
fi

# 4. Check SELinux / Security if present
if command -v getenforce >/dev/null 2>&1; then
  SELINUX=$(getenforce)
  log_info "SELinux State: ${SELINUX}"
fi

# 5. Check Required External Docker Networks
for net in edge-acb acb-core; do
  if docker network inspect "$net" >/dev/null 2>&1; then
    is_int="$(docker network inspect "$net" --format '{{.Internal}}' 2>/dev/null || echo "unknown")"
    log_info "Network [${net}]: OK (Internal=${is_int})"
  else
    log_warn "Network [${net}]: NOT FOUND (must be created before first container run)"
  fi
done
for net in acb-egress; do
  if docker network inspect "$net" >/dev/null 2>&1; then
    is_int="$(docker network inspect "$net" --format '{{.Internal}}' 2>/dev/null || echo "unknown")"
    log_info "Network [${net}]: OK (Egress, Internal=${is_int})"
  else
    log_warn "Network [${net}]: NOT FOUND (must be created before first container run)"
  fi
done

# 6. Check Production Data Volume
if docker volume inspect "$DATA_VOLUME_NAME" >/dev/null 2>&1; then
  log_info "Production Data Volume [${DATA_VOLUME_NAME}]: OK"
else
  log_warn "Production Data Volume [${DATA_VOLUME_NAME}]: NOT FOUND (run 'deploy/init-fresh-data.sh --confirm-fresh-init' for fresh setup)"
fi

# 7. Check Traefik Dynamic Directory and Route Derivation
if [[ -d "$TRAEFIK_DYNAMIC_DIR" ]]; then
  log_info "Traefik dynamic directory [${TRAEFIK_DYNAMIC_DIR}]: OK"
else
  log_warn "Traefik dynamic directory [${TRAEFIK_DYNAMIC_DIR}] does not exist yet (will be created on switch)"
fi
if route_host="$(get_route_host 2>/dev/null)"; then
  log_info "Traefik Route Host [${route_host}] (derived from PUBLIC_ORIGIN): OK"
else
  log_warn "Could not derive valid Traefik route host from PUBLIC_ORIGIN (${PUBLIC_ORIGIN:-unset})"
fi

# 8. Check Cosign Availability (required for signed manifest verification)
require_cosign="${REQUIRE_COSIGN:-0}"
for arg in "$@"; do
  case "$arg" in
    --require-cosign)
      require_cosign=1
      ;;
  esac
done

if command -v cosign >/dev/null 2>&1; then
  COSIGN_VER=$(cosign version 2>&1 | head -n 1 || echo "unknown")
  log_info "Cosign binary: OK (${COSIGN_VER})"
else
  log_warn "Cosign binary: NOT FOUND in PATH (required for production release manifest verification)"
  if [[ "$require_cosign" -eq 1 ]]; then
    log_error "Cosign binary is required but not found in PATH"
    exit 1
  fi
fi

log_info "=== PREFLIGHT AUDIT COMPLETED ==="
exit 0
