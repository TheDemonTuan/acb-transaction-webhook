#!/usr/bin/env bash
# One-time legacy -> payOS cutover. Never run normal deploy against legacy state.
set -Eeuo pipefail
umask 077
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec python3 "$HERE/migrate-payos-runtime.py" "$@"
