#!/usr/bin/env bash
# deploy/lib.sh
# Backward-compatibility shim sourcing modular deploy libraries.
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
export SCRIPT_DIR

LIB_DIR="$SCRIPT_DIR/lib"
if [[ ! -d "$LIB_DIR" && -d "$SCRIPT_DIR/../deploy/lib" ]]; then
  LIB_DIR="$SCRIPT_DIR/../deploy/lib"
fi

# shellcheck source=deploy/lib/common.sh
source "$LIB_DIR/common.sh"
# shellcheck source=deploy/lib/images.sh
source "$LIB_DIR/images.sh"
# shellcheck source=deploy/lib/state.sh
source "$LIB_DIR/state.sh"
# shellcheck source=deploy/lib/traefik.sh
source "$LIB_DIR/traefik.sh"
# shellcheck source=deploy/lib/database.sh
source "$LIB_DIR/database.sh"
