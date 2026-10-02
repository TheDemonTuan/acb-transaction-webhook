#!/usr/bin/env bash
# Resolve the release companion even through the stable deployment symlink.
set +x
set -Eeuo pipefail
HERE="$(dirname "$(realpath -- "${BASH_SOURCE[0]}")")"
exec python3 "$HERE/setup-recovery.py" "$@"
