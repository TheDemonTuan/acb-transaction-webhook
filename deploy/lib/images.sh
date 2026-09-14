#!/usr/bin/env bash
# deploy/lib/images.sh
# Image digest validation, volume checks, immutable image pulling, and container identity preservation.
set -euo pipefail

validate_digest() {
  local ref="$1"
  local name="$2"
  local pattern='^[^[:space:]]+@sha256:[a-f0-9]{64}$'
  if [[ ! "$ref" =~ $pattern ]]; then
    log_error "Invalid or non-immutable image digest for ${name}: '${ref}' (must match @sha256:<64-hex>)"
    return 1
  fi
  return 0
}

validate_data_volume() {
  local vol="$1"
  if ! docker volume inspect "$vol" >/dev/null 2>&1; then
    log_error "Production data volume '${vol}' does not exist. Aborting to prevent accidental data creation/loss. For initial setup, run 'deploy/init-fresh-data.sh --confirm-fresh-init'."
    return 1
  fi

  # Ambiguity check: if multiple volumes match filter, abort unless exactly one unique match
  local matches
  matches="$(docker volume ls -q --filter "name=${vol}" 2>/dev/null || true)"
  local match_count=0
  local exact_count=0
  while IFS= read -r line; do
    if [[ -n "$line" ]]; then
      match_count=$(( match_count + 1 ))
      if [[ "$line" == "$vol" ]]; then
        exact_count=$(( exact_count + 1 ))
      fi
    fi
  done <<< "$matches"

  if [[ "$exact_count" -ne 1 || "$match_count" -gt 1 ]]; then
    log_error "Ambiguous data volume detected for '${vol}'. Matches: [${matches}]. Aborting."
    return 1
  fi
  return 0
}

pull_candidate_image() {
  local target_service="$1"
  local image_ref="$2"
  log_info "Pulling candidate image [${target_service}] (${image_ref})..."
  if ! compose_prod pull "$target_service" 2>/dev/null; then
    if ! docker compose -f "$COMPOSE_FILE" pull "$target_service" 2>/dev/null; then
      if ! docker pull "$image_ref"; then
        log_error "Failed to pull candidate image '${image_ref}' for service '${target_service}'"
        return 1
      fi
    fi
  fi
  return 0
}

snapshot_core_containers() {
  local snapshot_file="$1"
  mkdir -p "$(dirname "$snapshot_file")"
  local services=("acb-worker" "acb-auth-browser" "acb-tts-gateway" "acb-bark")
  : > "$snapshot_file"
  for s in "${services[@]}"; do
    local cid
    cid="$(docker inspect --format '{{.Id}}' "$s" 2>/dev/null || echo "missing")"
    printf '%s=%s\n' "$s" "$cid" >> "$snapshot_file"
  done
}

assert_core_containers_unchanged() {
  local snapshot_file="$1"
  if [[ ! -f "$snapshot_file" ]]; then
    log_warn "assert_core_containers_unchanged: snapshot file '$snapshot_file' missing. Skipping check."
    return 0
  fi
  local mismatch=0
  while IFS='=' read -r service expected_id; do
    [[ -n "$service" ]] || continue
    local actual_id
    actual_id="$(docker inspect --format '{{.Id}}' "$service" 2>/dev/null || echo "missing")"
    if [[ "$expected_id" != "$actual_id" ]]; then
      log_error "INVARIANT VIOLATION: Core container '${service}' identity changed! Expected '${expected_id}', got '${actual_id}'."
      mismatch=1
    fi
  done < "$snapshot_file"
  if [[ "$mismatch" -ne 0 ]]; then
    return 1
  fi
  log_info "Core container identity audit PASSED: worker, browser, TTS, and Bark containers remained completely untouched."
  return 0
}
