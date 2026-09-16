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

log_info "=========================================================="
log_info "Starting Release Reconciliation / Recovery"
log_info "Runtime Root: $RUNTIME_ROOT"
log_info "=========================================================="

acquire_deploy_lock
trap 'release_deploy_lock' EXIT

if [[ -f "${TX_JOURNAL_FILE:-}" ]]; then
  log_info "Inspecting component transaction journal: $TX_JOURNAL_FILE"
  recover_tx_journal
fi

rollback_failures=0

rollback_gateway_route_reconcile() {
  if [[ -f "$PENDING_GATEWAY_RETIRE_FILE" ]]; then
    local old_slot candidate_slot
    old_slot="$(sed -n 's/^old_slot=//p' "$PENDING_GATEWAY_RETIRE_FILE")"
    candidate_slot="$(sed -n 's/^candidate_slot=//p' "$PENDING_GATEWAY_RETIRE_FILE")"
    if [[ "$old_slot" =~ ^(blue|green)$ && "$candidate_slot" =~ ^(blue|green)$ ]]; then
      log_warn "Reverting gateway route to [$old_slot] from pending evidence..."
      if atomic_switch_route "$old_slot" && ack_route_identity "$old_slot" "" 30; then
        printf '%s' "$old_slot" | atomic_write_file "$ACTIVE_SLOT_FILE" 600
        stop_standby_container "$candidate_slot" || true
        rm -f "$PENDING_GATEWAY_RETIRE_FILE"
        log_info "Gateway route restored to [$old_slot]."
      else
        log_error "Gateway route rollback failed; preserving pending evidence."
        return 1
      fi
    else
      log_error "Gateway rollback evidence is malformed; refusing to discard it."
      return 1
    fi
  fi
  return 0
}

rollback_frontend_route_reconcile() {
  if [[ -f "$PENDING_FRONTEND_RETIRE_FILE" ]]; then
    local prev_top="" cand_top="" prev_cnt="" cand_cnt="" gw_slot=""
    if grep -q '^{' "$PENDING_FRONTEND_RETIRE_FILE" 2>/dev/null; then
      read -r prev_top cand_top prev_cnt cand_cnt gw_slot < <(python3 - "$PENDING_FRONTEND_RETIRE_FILE" <<'PY'
import json, sys
try:
    d = json.load(open(sys.argv[1], encoding="utf-8"))
    print(d.get("previous_topology", ""), d.get("candidate_topology", ""), d.get("previous_container", ""), d.get("candidate_container", ""), d.get("gateway_slot_at_switch", ""))
except Exception:
    sys.exit(1)
PY
)
    else
      prev_top="$(sed -n 's/^old_slot=//p' "$PENDING_FRONTEND_RETIRE_FILE")"
      cand_top="$(sed -n 's/^candidate_slot=//p' "$PENDING_FRONTEND_RETIRE_FILE")"
      if [[ "$prev_top" == "legacy" ]]; then
        prev_cnt="acb-frontend"
      else
        prev_cnt="acb-frontend-${prev_top}"
      fi
      cand_cnt="acb-frontend-${cand_top}"
    fi

    if [[ "$prev_top" =~ ^(blue|green|legacy)$ && "$cand_top" =~ ^(blue|green)$ ]]; then
      log_warn "Reverting frontend route to topology [$prev_top]..."
      if [[ "$prev_top" == "legacy" ]]; then
        # Exact legacy frontend rollback:
        # 1. verify acb-frontend exists + running/healthy
        if ! docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "acb-frontend" 2>/dev/null | grep -Eq '^(healthy|running)$'; then
          log_error "Legacy frontend container acb-frontend is missing or unhealthy; failing closed."
          return 1
        fi
        local current_gw_slot
        current_gw_slot="$(get_active_slot 2>/dev/null || cat "$ACTIVE_SLOT_FILE" 2>/dev/null || printf 'blue')"
        local tmp_route="${ACB_CONFIG}.rollback.$$"
        render_traefik_config "$current_gw_slot" "$tmp_route" "legacy"
        if atomic_write_file "$ACB_CONFIG" 644 < "$tmp_route" && rm -f "$tmp_route" && ack_frontend_route 30; then
          rm -f "$FRONTEND_ACTIVE_SLOT_FILE"
          docker stop --time "${FRONTEND_STOP_TIMEOUT:-10}" "$cand_cnt" >/dev/null 2>&1 || true
          rm -f "$PENDING_FRONTEND_RETIRE_FILE"
          log_info "Frontend reverted to legacy container acb-frontend."
        else
          log_error "Frontend legacy route rollback failed; preserving evidence."
          return 1
        fi
      else
        # blue or green
        if atomic_switch_frontend_route "$prev_top" && ack_frontend_route 30; then
          printf '%s' "$prev_top" | atomic_write_file "$FRONTEND_ACTIVE_SLOT_FILE" 600
          docker stop --time "${FRONTEND_STOP_TIMEOUT:-10}" "$cand_cnt" >/dev/null 2>&1 || true
          rm -f "$PENDING_FRONTEND_RETIRE_FILE"
          log_info "Frontend reverted to slot [$prev_top]."
        else
          log_error "Frontend route rollback failed; preserving pending rollback evidence."
          return 1
        fi
      fi
    else
      log_error "Frontend rollback evidence is malformed; refusing to discard it."
      return 1
    fi
  fi
  return 0
}

rollback_completed_components_reconcile() {
  if [[ ! -f "$ROLLOUT_JOURNAL_FILE" ]]; then
    return 0
  fi
  local image rollback_sha
  rollback_sha="$(python3 - "$ROLLOUT_JOURNAL_FILE" <<'PY' 2>/dev/null || true
import json, sys
try:
    print(json.load(open(sys.argv[1], encoding="utf-8")).get("git_sha", ""))
except Exception:
    pass
PY
)"

  step_completed() {
    local step="$1"
    python3 - "$ROLLOUT_JOURNAL_FILE" "$step" <<'PY' 2>/dev/null
import json, sys
try:
    data = json.load(open(sys.argv[1], encoding="utf-8"))
    sys.exit(0 if data.get("steps", {}).get(sys.argv[2]) == "STEP_COMPLETED" else 1)
except Exception:
    sys.exit(1)
PY
  }

  local comp_failures=0
  if step_completed failover_controller; then
    if [[ -f "$SCRIPT_DIR/deploy-failover-controller.sh" ]]; then
      EXPECTED_COMMIT="$rollback_sha" RELEASE_ORCHESTRATED=1 DEFER_RELEASE_STATE=1 FAILOVER_ROLLBACK_ONLY=1 \
        bash "$SCRIPT_DIR/deploy-failover-controller.sh" || comp_failures=$((comp_failures + 1))
    fi
  fi
  if step_completed worker; then
    if [[ -f "$SCRIPT_DIR/deploy-worker.sh" ]]; then
      image="$(get_release_env WORKER_IMAGE_REF 2>/dev/null || true)"
      if [[ -n "$image" ]]; then
        RELEASE_ORCHESTRATED=1 DEFER_RELEASE_STATE=1 bash "$SCRIPT_DIR/deploy-worker.sh" "$image" || comp_failures=$((comp_failures + 1))
      fi
    fi
  fi
  if step_completed bark; then
    if [[ -f "$SCRIPT_DIR/deploy-bark.sh" ]]; then
      image="$(get_release_env BARK_IMAGE_REF 2>/dev/null || true)"
      if [[ -n "$image" ]]; then
        RELEASE_ORCHESTRATED=1 DEFER_RELEASE_STATE=1 bash "$SCRIPT_DIR/deploy-bark.sh" "$image" || comp_failures=$((comp_failures + 1))
      fi
    fi
  fi
  if step_completed tts; then
    if [[ -f "$SCRIPT_DIR/deploy-tts.sh" ]]; then
      image="$(get_release_env TTS_IMAGE_REF 2>/dev/null || true)"
      if [[ -n "$image" ]]; then
        RELEASE_ORCHESTRATED=1 DEFER_RELEASE_STATE=1 bash "$SCRIPT_DIR/deploy-tts.sh" "$image" || comp_failures=$((comp_failures + 1))
      fi
    fi
  fi
  if step_completed auth_browser; then
    if [[ -f "$SCRIPT_DIR/deploy-auth-browser.sh" ]]; then
      image="$(get_release_env BROWSER_IMAGE_REF 2>/dev/null || true)"
      if [[ -n "$image" ]]; then
        RELEASE_ORCHESTRATED=1 DEFER_RELEASE_STATE=1 bash "$SCRIPT_DIR/deploy-auth-browser.sh" "$image" || comp_failures=$((comp_failures + 1))
      fi
    fi
  fi
  return "$comp_failures"
}

verify_previous_runtime_reconcile() {
  local current_state="$CURRENT_RELEASE_FILE"
  if [[ ! -s "$current_state" ]]; then
    log_error "Canonical release state is missing: $current_state"
    return 1
  fi
  if [[ "${SKIP_MANIFEST_CHECK:-0}" -eq 1 && -n "${RUNTIME_DRIFT_CHECK_CMD:-}" ]]; then
    eval "$RUNTIME_DRIFT_CHECK_CMD"
    return $?
  fi
  if [[ -f "$SCRIPT_DIR/verify-runtime-drift.sh" ]]; then
    bash "$SCRIPT_DIR/verify-runtime-drift.sh" --state "$current_state"
    return $?
  fi
  return 0
}

# Run non-short-circuiting rollback
rollback_gateway_route_reconcile || rollback_failures=$((rollback_failures + 1))
rollback_frontend_route_reconcile || rollback_failures=$((rollback_failures + 1))
rollback_completed_components_reconcile || rollback_failures=$((rollback_failures + 1))
verify_previous_runtime_reconcile || rollback_failures=$((rollback_failures + 1))

if (( rollback_failures == 0 )); then
  if [[ -f "$ROLLOUT_JOURNAL_FILE" ]]; then
    previous_rollout_archive="${ROLLOUT_JOURNAL_FILE}.reconciled.$(date +%s)"
    mv -f "$ROLLOUT_JOURNAL_FILE" "$previous_rollout_archive"
    log_info "Runtime matches canonical state; archived reconciled interrupted rollout journal: $previous_rollout_archive"
  fi
  log_info "Release reconciliation completed successfully."
  exit 0
else
  log_error "Release reconciliation encountered $rollback_failures failure(s); preserving evidence."
  exit 1
fi
