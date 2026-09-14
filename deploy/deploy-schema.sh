#!/usr/bin/env bash
# deploy/deploy-schema.sh
# Schema Migration Deployment Transaction with Fail-Closed Active-Auth Gate and Verified Backup.
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deploy/lib.sh
source "$SCRIPT_DIR/lib.sh"

DBTOOL_IMAGE="${1:-${DBTOOL_IMAGE_REF:-}}"

if [[ -z "$DBTOOL_IMAGE" ]]; then
  DBTOOL_IMAGE="$(get_release_env DBTOOL_IMAGE_REF 2>/dev/null || true)"
fi

if [[ -z "$DBTOOL_IMAGE" ]]; then
  log_error "Usage: $0 <dbtool-image-digest>"
  exit 1
fi

validate_canonical_env
validate_digest "$DBTOOL_IMAGE" "dbtool"
validate_data_volume "$DATA_VOLUME_NAME"
validate_secrets

acquire_deploy_lock
trap 'release_deploy_lock' EXIT

log_info "=========================================================="
log_info "Starting Schema Deployment Transaction"
log_info "Target DBTool Image: ${DBTOOL_IMAGE}"
log_info "=========================================================="

# 1. Fail-closed active-auth check (active in-flight sessions block migration)
if ! check_active_auth_gate; then
  log_error "Schema deployment aborted: active customer authentication attempt in progress or check failed."
  exit 1
fi

# 2. Read-only WAL probe
if ! verify_wal_probe "$DATA_VOLUME_NAME" "$DBTOOL_IMAGE"; then
  log_error "Schema deployment aborted: read-only WAL probe failed."
  exit 1
fi

# 3. Encrypted SQLite preflight backup
ACTIVE_SLOT="$(get_active_slot)"
BACKUP_FILE="$(perform_sqlite_backup "$DATA_VOLUME_NAME" "$DBTOOL_IMAGE" "$ACTIVE_SLOT")"
if [[ ! -s "$BACKUP_FILE" ]]; then
  log_error "Schema deployment aborted: Pre-migration backup failed."
  exit 1
fi
log_info "Pre-migration backup verified at: ${BACKUP_FILE}"

# 4. Run migration via immutable dbtool
if ! run_dbtool_migration "$DATA_VOLUME_NAME" "$DBTOOL_IMAGE"; then
  log_error "CRITICAL: Database migration failed. Live database preserved without automatic overwrite."
  log_error "To restore from verified backup, run: deploy/restore-db.sh --source '${BACKUP_FILE}'"
  exit 1
fi

# 5. Verify schema compatibility post-migration
log_info "Verifying schema compatibility post-migration..."
if command -v docker >/dev/null 2>&1 && docker volume inspect "$DATA_VOLUME_NAME" >/dev/null 2>&1; then
  if ! docker run --rm --network none --read-only --user 1000:1000 \
    -v "${DATA_VOLUME_NAME}:/data:ro" \
    "$DBTOOL_IMAGE" -path /data/gateway.db -check >/dev/null 2>&1; then
    log_error "Post-migration database integrity check failed!"
    exit 1
  fi
fi

# 6. Record migration metadata
MIGRATION_RECORD="${MIGRATION_RECORD:-${DATA_DIR:-$SCRIPT_DIR/data}/migration-record.json}"
mkdir -p "$(dirname "$MIGRATION_RECORD")"
cat <<EOF > "$MIGRATION_RECORD"
{
  "timestamp": "$(date -u +'%Y-%m-%dT%H:%M:%SZ')",
  "dbtool_image": "${DBTOOL_IMAGE}",
  "backup_file": "$(basename "$BACKUP_FILE")",
  "status": "COMPLETED"
}
EOF
chmod 600 "$MIGRATION_RECORD" 2>/dev/null || true

log_info "Schema deployment transaction completed successfully. Route remains untouched."
exit 0
