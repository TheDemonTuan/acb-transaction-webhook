#!/usr/bin/env bash
# deploy/render-traefik-route.sh
# Renders and validates Traefik dynamic routing configuration for ACB Blue/Green slots.
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deploy/lib/common.sh
source "$SCRIPT_DIR/lib/common.sh"
# shellcheck source=deploy/lib/traefik.sh
source "$SCRIPT_DIR/lib/traefik.sh"

TARGET_SLOT="${1:-}"
OUTPUT_FILE="${2:-}"

if [[ -z "$TARGET_SLOT" || ( "$TARGET_SLOT" != "blue" && "$TARGET_SLOT" != "green" ) ]]; then
  log_error "Usage: $0 <blue|green> [output_file]"
  exit 1
fi

tmp_file="$(mktemp "${TMPDIR:-/tmp}/acb-route.XXXXXX")"
render_traefik_config "$TARGET_SLOT" "$tmp_file"

if ! validate_traefik_yaml "$tmp_file"; then
  log_error "Rendered Traefik configuration failed YAML validation."
  rm -f "$tmp_file" 2>/dev/null || true
  exit 1
fi

if [[ -n "$OUTPUT_FILE" ]]; then
  mkdir -p "$(dirname "$OUTPUT_FILE")"
  mv -f "$tmp_file" "$OUTPUT_FILE"
  log_info "Traefik route configuration for slot [${TARGET_SLOT}] successfully rendered to ${OUTPUT_FILE}."
else
  cat "$tmp_file"
  rm -f "$tmp_file" 2>/dev/null || true
fi
