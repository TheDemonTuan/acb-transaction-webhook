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
dbtool_img="${DBTOOL_IMAGE_REF:-ghcr.io/thedemontuan/acb-transaction-webhook-dbtool:latest}"

backup_file="$(create_preflight_backup "$DATA_VOLUME_NAME" "$dbtool_img" "$active_slot")"

if [[ -n "$out" && "$out" != "$backup_file" ]]; then
  mkdir -p "$(dirname "$out")"
  cp -p "$backup_file" "$out"
  log_info "Copied backup to requested destination: ${out}"
fi

log_info "Backup operation completed successfully."
exit 0
