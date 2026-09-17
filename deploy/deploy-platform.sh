#!/usr/bin/env bash
# deploy/deploy-platform.sh
# Transactionally validates, backs up, installs edge dynamic configs (middlewares.yml, portfolio.yml, bark.yml),
# renders acb.yml to the active slot, and verifies edge routes.
set -Eeuo pipefail
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deploy/lib.sh
source "$SCRIPT_DIR/lib.sh"
require_release_orchestrator

CANDIDATE_DYNAMIC_DIR="${PLATFORM_CANDIDATE_DYNAMIC_DIR:-${RELEASE_CONTEXT_DIR:-$SCRIPT_DIR}/edge/dynamic}"
TRAEFIK_DYNAMIC_DIR="${TRAEFIK_DYNAMIC_DIR:-/opt/platform/edge/dynamic}"
BACKUP_ROOT="${PLATFORM_BACKUP_ROOT:-${DATA_DIR:-${DEPLOY_PATH:-$SCRIPT_DIR/..}/data}/platform-edge-backups}"
RELEASE_KEY="${EXPECTED_COMMIT:-$(date -u +%Y%m%d%H%M%S)}"
BACKUP_DIR="$BACKUP_ROOT/$RELEASE_KEY"
PENDING_PLATFORM_ROLLBACK_FILE="${PENDING_PLATFORM_ROLLBACK_FILE:-}"

MUTATION_STARTED=0
COMMITTED=0

rollback_platform() {
  local exit_code=$?
  if [[ "$COMMITTED" -eq 1 || "$MUTATION_STARTED" -eq 0 ]]; then
    exit "$exit_code"
  fi
  log_warn "deploy-platform failed: rolling back dynamic edge configs from $BACKUP_DIR..."
  if [[ -d "$BACKUP_DIR" ]]; then
    if [[ -f "$BACKUP_DIR/.manifest" ]]; then
      while IFS=: read -r fname fstatus; do
        [[ -n "$fname" ]] || continue
        if [[ "$fstatus" == "existed" && -f "$BACKUP_DIR/$fname" ]]; then
          cp -p "$BACKUP_DIR/$fname" "$TRAEFIK_DYNAMIC_DIR/$fname" 2>/dev/null || true
        elif [[ "$fstatus" == "absent" ]]; then
          rm -f "$TRAEFIK_DYNAMIC_DIR/$fname" 2>/dev/null || true
        fi
      done < "$BACKUP_DIR/.manifest"
    else
      for f in middlewares.yml portfolio.yml bark.yml acb.yml; do
        if [[ -f "$BACKUP_DIR/$f" ]]; then
          cp -p "$BACKUP_DIR/$f" "$TRAEFIK_DYNAMIC_DIR/$f" 2>/dev/null || true
        fi
      done
    fi
  fi
  local active_slot
  active_slot="$(get_active_slot 2>/dev/null || echo blue)"
  atomic_switch_route "$active_slot" 2>/dev/null || true
  exit "$exit_code"
}
trap rollback_platform EXIT INT TERM

log_info "Deploying platform dynamic edge configurations to $TRAEFIK_DYNAMIC_DIR..."

if ! mkdir -p "$TRAEFIK_DYNAMIC_DIR" 2>/dev/null; then
  log_error "Traefik dynamic directory '$TRAEFIK_DYNAMIC_DIR' is not writable or cannot be created."
  exit 1
fi

# 1. Validate candidate YAMLs
dynamic_files=(middlewares.yml portfolio.yml bark.yml)
for df in "${dynamic_files[@]}"; do
  candidate_file="$CANDIDATE_DYNAMIC_DIR/$df"
  if [[ -f "$candidate_file" ]]; then
    if ! validate_traefik_yaml "$candidate_file" "dynamic"; then
      log_error "Candidate dynamic configuration '$candidate_file' failed YAML validation."
      exit 1
    fi
  fi
done

# 2. Backup current configs and record existence manifest
mkdir -p "$BACKUP_DIR"
rm -f "$BACKUP_DIR/.manifest"
for df in "${dynamic_files[@]}" acb.yml; do
  if [[ -f "$TRAEFIK_DYNAMIC_DIR/$df" ]]; then
    cp -p "$TRAEFIK_DYNAMIC_DIR/$df" "$BACKUP_DIR/$df"
    printf '%s:existed\n' "$df" >> "$BACKUP_DIR/.manifest"
  else
    printf '%s:absent\n' "$df" >> "$BACKUP_DIR/.manifest"
  fi
done

# 3. Atomically install dynamic files
MUTATION_STARTED=1
for df in "${dynamic_files[@]}"; do
  candidate_file="$CANDIDATE_DYNAMIC_DIR/$df"
  if [[ -f "$candidate_file" ]]; then
    atomic_write_file "$TRAEFIK_DYNAMIC_DIR/$df" 644 < "$candidate_file"
    log_info "Installed $df -> $TRAEFIK_DYNAMIC_DIR/$df"
  fi
done

# 4. Render acb.yml for the current active slot
active_slot="$(get_active_slot)"
log_info "Re-rendering acb route for active slot: $active_slot..."
atomic_switch_route "$active_slot"

# 5. Acknowledge route identity
if ! ack_route_identity "$active_slot" "${EXPECTED_COMMIT:-}" 15; then
  log_error "Route identity acknowledgment failed for $active_slot after platform deploy!"
  exit 1
fi

# 6. Record pending platform rollback evidence if orchestrated
if [[ -n "$PENDING_PLATFORM_ROLLBACK_FILE" ]]; then
  mkdir -p "$(dirname "$PENDING_PLATFORM_ROLLBACK_FILE")"
  printf 'backup_dir=%s\nrelease_key=%s\n' "$BACKUP_DIR" "$RELEASE_KEY" | atomic_write_file "$PENDING_PLATFORM_ROLLBACK_FILE" 600
fi

COMMITTED=1
log_info "Platform edge configuration deployed and verified successfully."
exit 0
