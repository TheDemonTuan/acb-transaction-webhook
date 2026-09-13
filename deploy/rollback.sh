#!/usr/bin/env bash
# Rollback entrypoint - delegates to transactional warm rollback
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deploy/lib.sh
source "$SCRIPT_DIR/lib.sh"

log_info "Invoking transactional warm rollback..."
exec "$SCRIPT_DIR/rollback-warm.sh" "$@"
