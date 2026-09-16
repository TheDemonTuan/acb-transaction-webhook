#!/usr/bin/env bash
# deploy/lib/recovery.sh
# Canonical recovery implementation: converges runtime back to the exact current canonical release.
set -Eeuo pipefail

reconcile_runtime_to_canonical() {
  local state_file="${1:-${CURRENT_RELEASE_FILE:-${RUNTIME_STATE_DIR:-$SCRIPT_DIR}/current-release.json}}"
  local rollout_journal="${2:-${ROLLOUT_JOURNAL_FILE:-${RUNTIME_DATA_DIR:-$SCRIPT_DIR}/rollout-journal.json}}"

  log_info "=========================================================="
  log_info "Reconciling runtime to canonical release"
  log_info "Canonical state: $state_file"
  log_info "Rollout journal: $rollout_journal"
  log_info "=========================================================="

  [[ -s "$state_file" ]] || {
    log_error "Canonical release state is missing or empty: $state_file"
    return 1
  }

  local py_tool=""
  if [[ -f "$SCRIPT_DIR/release-state.py" ]]; then
    py_tool="$SCRIPT_DIR/release-state.py"
  elif [[ -f "${DEPLOY_DIR:-}/release-state.py" ]]; then
    py_tool="${DEPLOY_DIR:-}/release-state.py"
  elif [[ -f "${RUNTIME_DEPLOY_DIR:-}/release-state.py" ]]; then
    py_tool="${RUNTIME_DEPLOY_DIR:-}/release-state.py"
  fi

  if [[ "${SKIP_MANIFEST_CHECK:-0}" -ne 1 && -n "$py_tool" ]]; then
    if ! python3 "$py_tool" validate "$state_file"; then
      log_error "Canonical release state failed schema validation: $state_file"
      return 1
    fi
  fi

  # Step 3: Load canonical target fields ONLY from current canonical state
  local target_release git_sha manifest_sha256
  local canonical_gw_slot canonical_fe_slot
  local canonical_gw_blue_img canonical_gw_green_img canonical_fe_img
  local canonical_worker_img canonical_browser_img canonical_tts_img canonical_bark_img canonical_dbtool_img

  target_release="$(python3 - "$state_file" <<'PY'
import json, sys
d = json.load(open(sys.argv[1], encoding='utf-8'))
print(d.get("release_dir", ""))
PY
)"
  git_sha="$(python3 - "$state_file" <<'PY'
import json, sys
d = json.load(open(sys.argv[1], encoding='utf-8'))
print(d.get("git_sha", ""))
PY
)"
  manifest_sha256="$(python3 - "$state_file" <<'PY'
import json, sys
d = json.load(open(sys.argv[1], encoding='utf-8'))
print(d.get("manifest_sha256", ""))
PY
)"
  canonical_gw_slot="$(python3 - "$state_file" <<'PY'
import json, sys
d = json.load(open(sys.argv[1], encoding='utf-8'))
print(d.get("active_slots", {}).get("gateway", "blue"))
PY
)"
  canonical_fe_slot="$(python3 - "$state_file" <<'PY'
import json, sys
d = json.load(open(sys.argv[1], encoding='utf-8'))
print(d.get("active_slots", {}).get("frontend", "blue"))
PY
)"
  canonical_gw_blue_img="$(python3 - "$state_file" <<'PY'
import json, sys
d = json.load(open(sys.argv[1], encoding='utf-8'))
print(d.get("images", {}).get("gateway", {}).get("blue", ""))
PY
)"
  canonical_gw_green_img="$(python3 - "$state_file" <<'PY'
import json, sys
d = json.load(open(sys.argv[1], encoding='utf-8'))
print(d.get("images", {}).get("gateway", {}).get("green", ""))
PY
)"
  canonical_fe_img="$(python3 - "$state_file" <<'PY'
import json, sys
d = json.load(open(sys.argv[1], encoding='utf-8'))
print(d.get("images", {}).get("frontend", ""))
PY
)"
  canonical_worker_img="$(python3 - "$state_file" <<'PY'
import json, sys
d = json.load(open(sys.argv[1], encoding='utf-8'))
print(d.get("images", {}).get("worker", ""))
PY
)"
  canonical_browser_img="$(python3 - "$state_file" <<'PY'
import json, sys
d = json.load(open(sys.argv[1], encoding='utf-8'))
print(d.get("images", {}).get("auth_browser", ""))
PY
)"
  canonical_tts_img="$(python3 - "$state_file" <<'PY'
import json, sys
d = json.load(open(sys.argv[1], encoding='utf-8'))
print(d.get("images", {}).get("tts", ""))
PY
)"
  canonical_bark_img="$(python3 - "$state_file" <<'PY'
import json, sys
d = json.load(open(sys.argv[1], encoding='utf-8'))
print(d.get("images", {}).get("bark", ""))
PY
)"
  canonical_dbtool_img="$(python3 - "$state_file" <<'PY'
import json, sys
d = json.load(open(sys.argv[1], encoding='utf-8'))
print(d.get("images", {}).get("dbtool", ""))
PY
)"

  if [[ -z "$target_release" || ! -d "$target_release" ]]; then
    if [[ "${SKIP_MANIFEST_CHECK:-0}" -eq 1 ]]; then
      target_release="${DEPLOY_DIR:-${SCRIPT_DIR:-$RUNTIME_ROOT/deploy}}"
    fi
  fi

  [[ -n "$target_release" && -d "$target_release" ]] || {
    log_error "Canonical release directory missing or not a directory: $target_release"
    return 1
  }

  log_info "Target canonical release directory: $target_release"
  log_info "Target canonical git SHA: $git_sha"
  log_info "Canonical gateway slot: $canonical_gw_slot, frontend slot: $canonical_fe_slot"

  # Recover component transaction journal first if present
  if [[ -f "${TX_JOURNAL_FILE:-}" ]]; then
    log_info "Inspecting component transaction journal: $TX_JOURNAL_FILE"
    if ! recover_tx_journal; then
      log_error "Failed to recover component transaction journal."
      return 1
    fi
  fi

  # Helper to check if a step was recorded in rollout journal
  step_was_touched() {
    local step="$1"
    [[ -f "$rollout_journal" ]] || return 1
    python3 - "$rollout_journal" "$step" <<'PY'
import json, sys
try:
    d = json.load(open(sys.argv[1], encoding='utf-8'))
    steps = d.get("steps", {})
    st = steps.get(sys.argv[2], "")
    if st in ("STEP_STARTED", "STEP_IN_PROGRESS", "STEP_COMPLETED"):
        sys.exit(0)
    sys.exit(1)
except Exception:
    sys.exit(1)
PY
  }

  # Export environment for compose/release context targeting canonical release
  export RELEASE_CONTEXT_DIR="$target_release"
  export COMPOSE_ROOT="$target_release/compose"
  export IMAGE_REF_BLUE="$canonical_gw_blue_img"
  export IMAGE_REF_GREEN="$canonical_gw_green_img"
  export FRONTEND_IMAGE_REF="$canonical_fe_img"
  export WORKER_IMAGE_REF="$canonical_worker_img"
  export BROWSER_IMAGE_REF="$canonical_browser_img"
  export TTS_IMAGE_REF="$canonical_tts_img"
  export BARK_IMAGE_REF="$canonical_bark_img"
  export DBTOOL_IMAGE_REF="${canonical_dbtool_img:-$canonical_worker_img}"

  # Step 4: Restore canonical active gateway before switching traffic
  local target_gw_img="$canonical_gw_blue_img"
  local candidate_gw_slot="green"
  if [[ "$canonical_gw_slot" == "green" ]]; then
    target_gw_img="$canonical_gw_green_img"
    candidate_gw_slot="blue"
  fi

  local cur_gw_img cur_gw_status cur_gw_running
  cur_gw_img="$(docker inspect --format '{{.Config.Image}}' "acb-gateway-${canonical_gw_slot}" 2>/dev/null || echo "")"
  cur_gw_status="$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "acb-gateway-${canonical_gw_slot}" 2>/dev/null || echo "")"
  cur_gw_running="$(docker inspect --format '{{.State.Running}}' "acb-gateway-${canonical_gw_slot}" 2>/dev/null || echo "false")"

  if [[ "$cur_gw_running" != "true" || ( "$cur_gw_status" != "healthy" && "$cur_gw_status" != "running" ) || ( -n "$target_gw_img" && "$cur_gw_img" != "$target_gw_img" ) ]]; then
    log_warn "Canonical active gateway container [acb-gateway-${canonical_gw_slot}] does not match canonical state (running=$cur_gw_running, status=$cur_gw_status, image=$cur_gw_img, expected=$target_gw_img). Recreating from canonical release context..."
    if ! compose_prod up -d --no-deps "gateway-${canonical_gw_slot}"; then
      log_error "Failed to recreate canonical active gateway container gateway-${canonical_gw_slot}"
      return 1
    fi

    # Wait for health
    local gw_healthy=0
    for _ in $(seq 1 30); do
      cur_gw_status="$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "acb-gateway-${canonical_gw_slot}" 2>/dev/null || echo "")"
      if [[ "$cur_gw_status" == "healthy" || "$cur_gw_status" == "running" ]]; then
        gw_healthy=1
        break
      fi
      sleep 1
    done
    if [[ "$gw_healthy" -ne 1 ]]; then
      log_error "Recreated canonical gateway slot acb-gateway-${canonical_gw_slot} failed health check (status: $cur_gw_status)."
      return 1
    fi
  fi

  # Check if route or active-slot file needs to be converged
  local route_needs_update=0
  if [[ -f "$ACB_CONFIG" ]] && ! grep -q "acb-web-${canonical_gw_slot}" "$ACB_CONFIG" 2>/dev/null; then
    route_needs_update=1
  fi
  if [[ -f "$ACTIVE_SLOT_FILE" ]] && [[ "$(cat "$ACTIVE_SLOT_FILE" 2>/dev/null || echo "")" != "$canonical_gw_slot" ]]; then
    route_needs_update=1
  fi
  if [[ -f "${PENDING_GATEWAY_RETIRE_FILE:-}" ]]; then
    route_needs_update=1
  fi

  if [[ "$route_needs_update" -eq 1 ]]; then
    log_info "Switching Traefik route pointer to canonical gateway slot: ${canonical_gw_slot}..."
    local tmp_route="${ACB_CONFIG}.recovery.$$"
    render_traefik_config "$canonical_gw_slot" "$tmp_route" "$canonical_fe_slot"
    if ! validate_traefik_yaml "$tmp_route"; then
      log_error "Generated recovery Traefik route is invalid."
      rm -f "$tmp_route"
      return 1
    fi
    if ! atomic_write_file "$ACB_CONFIG" 644 < "$tmp_route"; then
      log_error "Failed to write Traefik configuration file: $ACB_CONFIG"
      rm -f "$tmp_route"
      return 1
    fi
    rm -f "$tmp_route"

    if ! ack_route_identity "$canonical_gw_slot" "$git_sha" 30; then
      log_error "Route identity ACK failed for canonical gateway slot ${canonical_gw_slot}."
      return 1
    fi
    printf '%s' "$canonical_gw_slot" | atomic_write_file "$ACTIVE_SLOT_FILE" 600
    log_info "Gateway route restored to canonical slot: ${canonical_gw_slot}"

    # Only after route ACK may known candidate standby be stopped
    if docker inspect "acb-gateway-${candidate_gw_slot}" >/dev/null 2>&1; then
      local cand_running
      cand_running="$(docker inspect --format '{{.State.Running}}' "acb-gateway-${candidate_gw_slot}" 2>/dev/null || echo false)"
      if [[ "$cand_running" == "true" ]]; then
        log_info "Stopping candidate standby container acb-gateway-${candidate_gw_slot}..."
        stop_standby_container "$candidate_gw_slot" || {
          log_error "Failed to stop candidate standby container: ${candidate_gw_slot}"
          return 1
        }
      fi
    fi
    rm -f "${PENDING_GATEWAY_RETIRE_FILE:-}" 2>/dev/null || true
  fi

  # Step 6: Restore frontend topology exactly
  local fe_container="acb-frontend"
  if [[ "$canonical_fe_slot" == "blue" || "$canonical_fe_slot" == "green" ]]; then
    fe_container="acb-frontend-${canonical_fe_slot}"
  fi

  local cur_fe_img cur_fe_status cur_fe_running
  cur_fe_img="$(docker inspect --format '{{.Config.Image}}' "$fe_container" 2>/dev/null || echo "")"
  cur_fe_status="$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$fe_container" 2>/dev/null || echo "")"
  cur_fe_running="$(docker inspect --format '{{.State.Running}}' "$fe_container" 2>/dev/null || echo "false")"

  if [[ "$cur_fe_running" != "true" || ( "$cur_fe_status" != "healthy" && "$cur_fe_status" != "running" ) || ( -n "$canonical_fe_img" && "$cur_fe_img" != "$canonical_fe_img" ) ]]; then
    log_warn "Canonical frontend container [$fe_container] does not match canonical state (running=$cur_fe_running, status=$cur_fe_status, image=$cur_fe_img, expected=$canonical_fe_img). Recreating..."
    local fe_compose_service="frontend"
    if [[ "$canonical_fe_slot" == "blue" || "$canonical_fe_slot" == "green" ]]; then
      fe_compose_service="frontend-${canonical_fe_slot}"
    fi
    if ! compose_prod up -d --no-deps "$fe_compose_service"; then
      log_error "Failed to recreate canonical frontend container: $fe_compose_service"
      return 1
    fi
  fi

  local fe_route_needs_update=0
  if [[ -f "$FRONTEND_ACTIVE_SLOT_FILE" ]] && [[ "$(cat "$FRONTEND_ACTIVE_SLOT_FILE" 2>/dev/null || echo "")" != "$canonical_fe_slot" ]]; then
    fe_route_needs_update=1
  fi
  if [[ -f "${PENDING_FRONTEND_RETIRE_FILE:-}" ]]; then
    fe_route_needs_update=1
  fi

  if [[ "$fe_route_needs_update" -eq 1 ]]; then
    log_info "Switching Traefik frontend route to canonical frontend slot: ${canonical_fe_slot}..."
    local tmp_route="${ACB_CONFIG}.fe_recovery.$$"
    render_traefik_config "$canonical_gw_slot" "$tmp_route" "$canonical_fe_slot"
    if ! validate_traefik_yaml "$tmp_route"; then
      log_error "Generated recovery Traefik route for frontend is invalid."
      rm -f "$tmp_route"
      return 1
    fi
    if ! atomic_write_file "$ACB_CONFIG" 644 < "$tmp_route"; then
      log_error "Failed to write Traefik configuration file: $ACB_CONFIG"
      rm -f "$tmp_route"
      return 1
    fi
    rm -f "$tmp_route"

    if [[ "$canonical_fe_slot" == "legacy" ]]; then
      rm -f "$FRONTEND_ACTIVE_SLOT_FILE" 2>/dev/null || true
    else
      printf '%s' "$canonical_fe_slot" | atomic_write_file "$FRONTEND_ACTIVE_SLOT_FILE" 600
    fi
    log_info "Frontend route pointer set to canonical slot: ${canonical_fe_slot}"

    # Stop candidate frontend container if different
    for fslot in blue green; do
      if [[ "$fslot" != "$canonical_fe_slot" ]]; then
        if docker inspect "acb-frontend-${fslot}" >/dev/null 2>&1; then
          docker stop "acb-frontend-${fslot}" 2>/dev/null || true
        fi
      fi
    done
    rm -f "${PENDING_FRONTEND_RETIRE_FILE:-}" 2>/dev/null || true
  fi

  # Step 5: Restore only components proven mutated
  if step_was_touched worker; then
    local cur_w_img cur_w_running
    cur_w_img="$(docker inspect --format '{{.Config.Image}}' "acb-worker" 2>/dev/null || echo "")"
    cur_w_running="$(docker inspect --format '{{.State.Running}}' "acb-worker" 2>/dev/null || echo "false")"
    if [[ "$cur_w_running" != "true" || ( -n "$canonical_worker_img" && "$cur_w_img" != "$canonical_worker_img" ) ]]; then
      log_info "Restoring worker to canonical image $canonical_worker_img..."
      if [[ -f "$target_release/deploy-worker.sh" ]]; then
        RELEASE_ORCHESTRATED=1 DEFER_RELEASE_STATE=1 SCRIPT_DIR="$target_release" DEPLOY_DIR="$target_release" \
          bash "$target_release/deploy-worker.sh" "$canonical_worker_img" || {
            log_error "Worker canonical restore failed."
            return 1
          }
      else
        WORKER_IMAGE_REF="$canonical_worker_img" compose_prod up -d --no-deps worker || {
          log_error "Worker canonical compose up failed."
          return 1
        }
      fi
    fi
  fi

  if step_was_touched auth_browser; then
    local cur_b_img cur_b_running
    cur_b_img="$(docker inspect --format '{{.Config.Image}}' "acb-auth-browser" 2>/dev/null || echo "")"
    cur_b_running="$(docker inspect --format '{{.State.Running}}' "acb-auth-browser" 2>/dev/null || echo "false")"
    if [[ "$cur_b_running" != "true" || ( -n "$canonical_browser_img" && "$cur_b_img" != "$canonical_browser_img" ) ]]; then
      log_info "Restoring auth-browser to canonical image $canonical_browser_img..."
      if [[ -f "$target_release/deploy-auth-browser.sh" ]]; then
        RELEASE_ORCHESTRATED=1 DEFER_RELEASE_STATE=1 SCRIPT_DIR="$target_release" DEPLOY_DIR="$target_release" \
          bash "$target_release/deploy-auth-browser.sh" "$canonical_browser_img" || {
            log_error "Auth-browser canonical restore failed."
            return 1
          }
      else
        BROWSER_IMAGE_REF="$canonical_browser_img" compose_prod up -d --no-deps auth-browser || {
          log_error "Auth-browser canonical compose up failed."
          return 1
        }
      fi
    fi
  fi

  if step_was_touched tts; then
    local cur_t_img cur_t_running
    cur_t_img="$(docker inspect --format '{{.Config.Image}}' "acb-tts-gateway" 2>/dev/null || echo "")"
    cur_t_running="$(docker inspect --format '{{.State.Running}}' "acb-tts-gateway" 2>/dev/null || echo "false")"
    if [[ "$cur_t_running" != "true" || ( -n "$canonical_tts_img" && "$cur_t_img" != "$canonical_tts_img" ) ]]; then
      log_info "Restoring TTS gateway to canonical image $canonical_tts_img..."
      if [[ -f "$target_release/deploy-tts.sh" ]]; then
        RELEASE_ORCHESTRATED=1 DEFER_RELEASE_STATE=1 SCRIPT_DIR="$target_release" DEPLOY_DIR="$target_release" \
          bash "$target_release/deploy-tts.sh" "$canonical_tts_img" || {
            log_error "TTS canonical restore failed."
            return 1
          }
      else
        TTS_IMAGE_REF="$canonical_tts_img" compose_prod up -d --no-deps tts-gateway || {
          log_error "TTS canonical compose up failed."
          return 1
        }
      fi
    fi
  fi

  if step_was_touched bark; then
    local cur_k_img cur_k_running
    cur_k_img="$(docker inspect --format '{{.Config.Image}}' "acb-bark" 2>/dev/null || echo "")"
    cur_k_running="$(docker inspect --format '{{.State.Running}}' "acb-bark" 2>/dev/null || echo "false")"
    if [[ "$cur_k_running" != "true" || ( -n "$canonical_bark_img" && "$cur_k_img" != "$canonical_bark_img" ) ]]; then
      log_info "Restoring Bark to canonical image $canonical_bark_img..."
      if [[ -f "$target_release/deploy-bark.sh" ]]; then
        RELEASE_ORCHESTRATED=1 DEFER_RELEASE_STATE=1 SCRIPT_DIR="$target_release" DEPLOY_DIR="$target_release" \
          bash "$target_release/deploy-bark.sh" "$canonical_bark_img" || {
            log_error "Bark canonical restore failed."
            return 1
          }
      else
        BARK_IMAGE_REF="$canonical_bark_img" compose_prod up -d --no-deps bark || {
          log_error "Bark canonical compose up failed."
          return 1
        }
      fi
    fi
  fi

  if step_was_touched failover_controller; then
    if [[ -f "$target_release/deploy-failover-controller.sh" ]]; then
      log_info "Restoring failover controller..."
      EXPECTED_COMMIT="$git_sha" RELEASE_ORCHESTRATED=1 DEFER_RELEASE_STATE=1 FAILOVER_ROLLBACK_ONLY=1 \
        SCRIPT_DIR="$target_release" DEPLOY_DIR="$target_release" \
        bash "$target_release/deploy-failover-controller.sh" || {
          log_error "Failover controller restore failed."
          return 1
        }
    fi
  fi

  # Step 7: Verify full drift before deleting or archiving evidence
  log_info "Verifying full runtime drift against canonical state..."
  if [[ "${SKIP_MANIFEST_CHECK:-0}" -eq 1 && -n "${RUNTIME_DRIFT_CHECK_CMD:-}" ]]; then
    if ! eval "$RUNTIME_DRIFT_CHECK_CMD"; then
      log_error "Runtime drift check override command failed; preserving recovery evidence."
      return 1
    fi
  else
    local drift_verifier=""
    if [[ -f "$target_release/verify-runtime-drift.sh" ]]; then
      drift_verifier="$target_release/verify-runtime-drift.sh"
    elif [[ -f "$SCRIPT_DIR/verify-runtime-drift.sh" ]]; then
      drift_verifier="$SCRIPT_DIR/verify-runtime-drift.sh"
    fi

    if [[ -n "$drift_verifier" ]]; then
      if ! bash "$drift_verifier" --state "$state_file"; then
        log_error "Runtime drift verification failed against canonical state; preserving evidence."
        return 1
      fi
      log_info "Runtime drift verification PASSED: runtime matches canonical state."
    fi
  fi

  # Archive rollout journal only after drift verification passes
  if [[ -f "$rollout_journal" ]]; then
    local archive_path="${rollout_journal}.reconciled.$(date +%s)"
    mv -f "$rollout_journal" "$archive_path"
    log_info "Archived reconciled rollout journal to: $archive_path"
  fi

  log_info "Canonical recovery completed successfully."
  return 0
}
