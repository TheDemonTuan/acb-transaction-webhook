#!/usr/bin/env bash
# Fresh production data initialization command with explicit confirmation
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deploy/lib.sh
source "$SCRIPT_DIR/lib.sh"

CONFIRMED=0
for arg in "$@"; do
  if [[ "$arg" == "--confirm-fresh-init" || "$arg" == "--confirm" ]]; then
    CONFIRMED=1
  fi
done

if [[ "$CONFIRMED" -ne 1 ]]; then
  if [[ -t 0 ]]; then
    printf 'DANGER: You are about to initialize fresh production data volumes:\n'
    printf '  - Data Volume: %s\n' "$DATA_VOLUME_NAME"
    printf '  - Bark Volume: %s\n' "$BARK_VOLUME_NAME"
    printf 'Type "CONFIRM-INIT-PRODUCTION-DATA" to proceed: '
    read -r user_input
    if [[ "$user_input" == "CONFIRM-INIT-PRODUCTION-DATA" ]]; then
      CONFIRMED=1
    else
      log_error "Confirmation mismatch. Aborting fresh data initialization."
      exit 1
    fi
  else
    log_error "Fresh data initialization aborted: Missing explicit confirmation flag: --confirm-fresh-init"
    exit 1
  fi
fi

log_info "Initializing production data volumes..."
if ! docker volume inspect "$DATA_VOLUME_NAME" >/dev/null 2>&1; then
  docker volume create "$DATA_VOLUME_NAME"
  log_info "Created volume: $DATA_VOLUME_NAME"
else
  log_info "Volume already exists: $DATA_VOLUME_NAME"
fi

if ! docker volume inspect "$BARK_VOLUME_NAME" >/dev/null 2>&1; then
  docker volume create "$BARK_VOLUME_NAME"
  log_info "Created volume: $BARK_VOLUME_NAME"
else
  log_info "Volume already exists: $BARK_VOLUME_NAME"
fi

# Ensure secrets directory and default secrets are provisioned
validate_secrets

dbtool_img="${DBTOOL_IMAGE_REF:-}"
if [[ -n "$dbtool_img" ]]; then
  log_info "Initializing database schema via dbtool ($dbtool_img)..."
  run_dbtool_migration "$DATA_VOLUME_NAME" "$dbtool_img"
fi

log_info "Fresh production data initialized successfully."
exit 0
