#!/usr/bin/env bash
# Deterministic recovery-only entrypoint for interrupted/dirty releases.
# Consumes existing rollout journal/pending evidence and converges runtime
# back to the last successful canonical release.
set -Eeuo pipefail
umask 077

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
RUNTIME_ROOT="${RUNTIME_ROOT:-${DEPLOY_PATH:-/opt/acb-transaction-webhook}}"
RECOVERY_ONLY=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --runtime-root) RUNTIME_ROOT="$2"; shift 2 ;;
    --recovery-only) RECOVERY_ONLY=1; shift ;;
    *) printf 'Unknown reconcile-release argument: %s\n' "$1" >&2; exit 1 ;;
  esac
done

export RUNTIME_ROOT
if [[ -f "$SCRIPT_DIR/runtime-layout.sh" ]]; then
  # shellcheck source=deploy/runtime-layout.sh
  source "$SCRIPT_DIR/runtime-layout.sh"
fi

# shellcheck source=deploy/lib.sh
source "$SCRIPT_DIR/lib.sh"

acquire_deploy_lock
trap 'release_deploy_lock' EXIT

ARCHIVE_ROLLOUT_JOURNAL=1 reconcile_runtime_to_canonical "${CURRENT_RELEASE_FILE:-}" "${ROLLOUT_JOURNAL_FILE:-}"
