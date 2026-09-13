#!/usr/bin/env bash
# Online SQLite Preflight Backup and Manifest Generation
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deploy/lib.sh
source "$SCRIPT_DIR/lib.sh"

out="${1:-}"

log_info "Starting preflight backup process..."
validate_data_volume "$DATA_VOLUME_NAME"
validate_secrets

active_slot="$(get_active_slot)"
dbtool_img="${DBTOOL_IMAGE_REF:-$(get_release_env DBTOOL_IMAGE_REF 2>/dev/null || true)}"
if [[ -z "$dbtool_img" ]]; then
  log_error "DBTOOL_IMAGE_REF immutable digest is required for preflight backup."
  exit 1
fi
validate_digest "$dbtool_img" "dbtool"

backup_file="$(create_preflight_backup "$DATA_VOLUME_NAME" "$dbtool_img" "$active_slot")"

if [[ -n "$out" && "$out" != "$backup_file" ]]; then
  mkdir -p "$(dirname "$out")"
  cp -p "$backup_file" "$out"
  log_info "Copied backup to requested destination: ${out}"
fi

log_info "Backup operation completed successfully: ${backup_file}"
printf '%s\n' "$backup_file"
exit 0
