#!/usr/bin/env bash
# deploy/rollback.sh
# Primary rollback entrypoint for Gateway Blue/Green active slot.
#
# Scope: Gateway dynamic edge route pointer reversion and standby container reactivation.
# Component-specific alternatives:
#   - Worker rollback:      deploy/deploy-worker.sh <previous-worker-digest>
#   - Schema / DB restore:  deploy/restore-db.sh <encrypted-backup-file>
#   - Release env rollback: deploy/release-env.sh rollback <KEY>
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deploy/lib.sh
source "$SCRIPT_DIR/lib.sh"

if [[ "${1:-}" == "--help" || "${1:-}" == "-h" ]]; then
  cat <<'EOF'
Usage: deploy/rollback.sh [options]

Rolls back the active Gateway Blue/Green slot via Traefik route pointer reversion.

For other components:
  - Worker rollback:      deploy/deploy-worker.sh <previous-worker-digest>
  - Database restore:     deploy/restore-db.sh <encrypted-backup-file>
  - Release env rollback: deploy/release-env.sh rollback <KEY>
EOF
  exit 0
fi

log_info "Invoking transactional gateway slot rollback..."
exec "$SCRIPT_DIR/rollback-warm.sh" "$@"
