#!/usr/bin/env bash
# deploy/deploy-schema.sh
# Schema Migration Deployment Transaction with Durable Mutation Gate, Verified Preflight Backup, and Remote Receipt.
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deploy/lib.sh
source "$SCRIPT_DIR/lib.sh"
require_release_orchestrator

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

SCHEMA_DEPLOY_OWNER="deploy-schema-$(date -u +%Y%m%d%H%M%S)"
GATE_TOKEN=""

cleanup_schema_deploy() {
  local exit_code=$?
  if command -v docker >/dev/null 2>&1; then
    docker rm -f acb-schema-check >/dev/null 2>&1 || true
  fi
  if [[ -n "${GATE_TOKEN:-}" || -n "${SCHEMA_DEPLOY_OWNER:-}" ]]; then
    release_mutation_gate "$DATA_VOLUME_NAME" "$DBTOOL_IMAGE" "$SCHEMA_DEPLOY_OWNER" "${GATE_TOKEN:-}" 2>/dev/null || true
  fi
  release_deploy_lock
  exit "$exit_code"
}
trap cleanup_schema_deploy EXIT HUP INT TERM

log_info "=========================================================="
log_info "Starting Schema Deployment Transaction"
log_info "Target DBTool Image: ${DBTOOL_IMAGE}"
log_info "Owner: ${SCHEMA_DEPLOY_OWNER}"
log_info "=========================================================="

# 1. Preflight active-auth gate (active in-flight sessions block migration)
if ! check_active_auth_gate; then
  log_error "Schema deployment aborted: active customer authentication attempt in progress or check failed."
  exit 1
fi

# 2. Acquire durable mutation gate lease (atomic auth-start race prevention)
GATE_TOKEN="$(acquire_mutation_gate "$DATA_VOLUME_NAME" "$DBTOOL_IMAGE" "$SCHEMA_DEPLOY_OWNER" "schema-migration")"

# 3. Read-only WAL probe
if ! verify_wal_probe "$DATA_VOLUME_NAME" "$DBTOOL_IMAGE"; then
  log_error "Schema deployment aborted: read-only WAL probe failed."
  exit 1
fi

# 4. Encrypted SQLite preflight backup with verified remote receipt before migration
ACTIVE_SLOT="$(get_active_slot)"
BACKUP_FILE="$(perform_sqlite_backup "$DATA_VOLUME_NAME" "$DBTOOL_IMAGE" "$ACTIVE_SLOT")"
if [[ ! -s "$BACKUP_FILE" ]]; then
  log_error "Schema deployment aborted: Pre-migration backup failed."
  exit 1
fi
log_info "Pre-migration backup verified at: ${BACKUP_FILE}"

if [[ -f "${ROLLOUT_JOURNAL_FILE:-}" ]]; then
  python3 - "$ROLLOUT_JOURNAL_FILE" "$BACKUP_FILE" "$SCHEMA_DEPLOY_OWNER" <<'PY' 2>/dev/null || true
import json, sys
j_path, b_path, owner = sys.argv[1:4]
try:
    with open(j_path, "r", encoding="utf-8") as f:
        d = json.load(f)
    d["migration_backup"] = {
        "backup_file": b_path,
        "owner": owner,
    }
    with open(j_path, "w", encoding="utf-8") as f:
        json.dump(d, f, indent=2)
except Exception:
    pass
PY
fi

# 5. Run migration via immutable dbtool
if ! run_dbtool_migration "$DATA_VOLUME_NAME" "$DBTOOL_IMAGE"; then
  log_error "CRITICAL: Database migration failed. Live database preserved without automatic overwrite."
  log_error "To restore from verified backup, run: deploy/restore-db.sh --source '${BACKUP_FILE}'"
  exit 1
fi

# 6. Verify schema compatibility post-migration
log_info "Verifying schema compatibility post-migration..."
if ! verify_schema_compat "$DATA_VOLUME_NAME" "$DBTOOL_IMAGE" 10; then
  log_error "Post-migration schema compatibility check failed!"
  exit 1
fi

if command -v docker >/dev/null 2>&1 && docker volume inspect "$DATA_VOLUME_NAME" >/dev/null 2>&1; then
  docker rm -f acb-schema-check >/dev/null 2>&1 || true
  if ! docker run --name acb-schema-check --rm --network none --read-only --user 1000:1000 \
    -v "${DATA_VOLUME_NAME}:/data:ro" \
    "$DBTOOL_IMAGE" -path /data/gateway.db -readonly -check >/dev/null 2>&1; then
    log_error "Post-migration database integrity check failed!"
    exit 1
  fi
fi

# 7. Release durable mutation gate
release_mutation_gate "$DATA_VOLUME_NAME" "$DBTOOL_IMAGE" "$SCHEMA_DEPLOY_OWNER" "${GATE_TOKEN:-}"
GATE_TOKEN=""

# 8. Record migration metadata
MIGRATION_RECORD="${MIGRATION_RECORD:-${RUNTIME_DATA_DIR:-${DATA_DIR:-$SCRIPT_DIR/data}}/migration-record.json}"
mkdir -p "$(dirname "$MIGRATION_RECORD")"
cat <<EOF > "$MIGRATION_RECORD"
{
  "timestamp": "$(date -u +'%Y-%m-%dT%H:%M:%SZ')",
  "target_image": "${DBTOOL_IMAGE}",
  "backup_file": "$(basename "$BACKUP_FILE")",
  "status": "COMPLETED",
  "schema_version": 11,
  "route_touched": false
}
EOF
chmod 600 "$MIGRATION_RECORD" 2>/dev/null || true

log_info "=========================================================="
log_info "Schema Deployment Transaction Successfully Completed."
log_info "Recorded: ${MIGRATION_RECORD}"
log_info "No Traefik routes were modified (route_touched=false)."
log_info "=========================================================="
exit 0
