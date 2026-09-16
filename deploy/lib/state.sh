#!/usr/bin/env bash
# deploy/lib/state.sh
# Deployment lock, slot state, transaction journal, intentional stop markers, and soak management.
set -euo pipefail

TX_JOURNAL_FILE="${TX_JOURNAL_FILE:-${RUNTIME_DATA_DIR:-$SCRIPT_DIR/data}/deploy-journal.json}"
atomic_write_file() {
  local target="$1"
  local mode="${2:-600}"
  local parent tmp
  parent="$(dirname "$target")"
  mkdir -p "$parent"
  tmp="$(mktemp "${parent}/.$(basename "$target").tmp.XXXXXX")"
  cat > "$tmp"
  chmod "$mode" "$tmp"
  python3 - "$tmp" "$parent" <<'PY'
import os, sys
file_path, parent = sys.argv[1:]
with open(file_path, 'rb') as handle:
    os.fsync(handle.fileno())
dir_fd = os.open(parent, os.O_RDONLY | getattr(os, 'O_DIRECTORY', 0))
try:
    os.fsync(dir_fd)
finally:
    os.close(dir_fd)
PY
  mv -f "$tmp" "$target"
  python3 - "$parent" <<'PY'
import os, sys
fd = os.open(sys.argv[1], os.O_RDONLY | getattr(os, 'O_DIRECTORY', 0))
try:
    os.fsync(fd)
finally:
    os.close(fd)
PY
}

terminate_background_soak() {
  if [[ -f "${SOAK_STATE_FILE:-}" ]]; then
    local soak_pid=""
    if grep -q '^pid=' "$SOAK_STATE_FILE" 2>/dev/null; then
      soak_pid="$(grep '^pid=' "$SOAK_STATE_FILE" | head -n 1 | cut -d'=' -f2 | tr -d '\r\n')"
    fi
    if [[ -n "$soak_pid" && "$soak_pid" =~ ^[0-9]+$ ]]; then
      if kill -0 "$soak_pid" 2>/dev/null; then
        log_info "Terminating previous background soak process (PID: $soak_pid)..."
        kill -TERM "$soak_pid" 2>/dev/null || true
        sleep 1
        if kill -0 "$soak_pid" 2>/dev/null; then
          kill -9 "$soak_pid" 2>/dev/null || true
        fi
      fi
    fi
    if command -v pkill >/dev/null 2>&1; then
      pkill -f "run_resumable_soak" 2>/dev/null || true
    fi
    rm -f "$SOAK_STATE_FILE" 2>/dev/null || true
  fi
}

acquire_deploy_lock() {
  local timeout="${DEPLOY_LOCK_TIMEOUT:-30}"
  if [[ "${SKIP_LOCK:-0}" == "1" ]]; then
    return 0
  fi
  if [[ "${DEPLOY_LOCK_HELD:-0}" == "1" ]]; then
    local owner="${DEPLOY_LOCK_OWNER_PID:-}"
    local owner_lock=""
    [[ "$owner" =~ ^[0-9]+$ ]] && owner_lock="$(readlink -f "/proc/$owner/fd/9" 2>/dev/null || true)"
    if [[ -z "$owner_lock" || "$owner_lock" != "$(readlink -f "$DEPLOY_LOCK_FILE" 2>/dev/null || true)" ]]; then
      log_error "Inherited deployment lock ownership could not be verified."
      return 1
    fi
    return 0
  fi
  if ! command -v flock >/dev/null 2>&1; then
    log_error "flock is required for coordinated deployment and failover locking."
    return 1
  fi

  local lock_dir
  lock_dir="$(dirname "$DEPLOY_LOCK_FILE")"
  if [[ "${ALLOW_TEST_LOCK_PATH:-0}" != "1" && "$DEPLOY_LOCK_FILE" != /run/lock/vps-failover/*.lock ]]; then
    log_error "Unsafe deployment lock path: $DEPLOY_LOCK_FILE"
    return 1
  fi
  if ! mkdir -p "$lock_dir" 2>/dev/null || [[ ! -w "$lock_dir" ]]; then
    log_error "Canonical deployment lock directory is unavailable: $lock_dir"
    return 1
  fi
  exec 9>"$DEPLOY_LOCK_FILE"
  if ! flock -w "$timeout" 9; then
    log_error "Another deployment or cutover is active (lock timeout ${timeout}s on ${DEPLOY_LOCK_FILE})."
    return 1
  fi
  export DEPLOY_LOCK_HELD=1
  export DEPLOY_LOCK_OWNER_PID="$BASHPID"
  terminate_background_soak
  return 0
}
release_deploy_lock() {
  if [[ "${DEPLOY_LOCK_HELD:-0}" == "1" && "${DEPLOY_LOCK_OWNER_PID:-}" == "$BASHPID" ]]; then
    unset DEPLOY_LOCK_HELD DEPLOY_LOCK_OWNER_PID
    if command -v flock >/dev/null 2>&1; then
      flock -u 9 2>/dev/null || true
    fi
    exec 9>&- 2>/dev/null || true
  fi
}

get_active_slot() {
  local slot=""
  if [[ -f "$ACTIVE_SLOT_FILE" ]]; then
    slot="$(tr -d ' \r\n[:space:]' < "$ACTIVE_SLOT_FILE")"
  fi
  if [[ "$slot" != "blue" && "$slot" != "green" ]]; then
    if [[ -f "$ACB_CONFIG" ]]; then
      if grep -q "acb-web-green" "$ACB_CONFIG" 2>/dev/null; then
        slot="green"
      elif grep -q "acb-web-blue" "$ACB_CONFIG" 2>/dev/null; then
        slot="blue"
      fi
    fi
  fi
  if [[ "$slot" != "blue" && "$slot" != "green" ]]; then
    local blue_running
    blue_running="$(docker inspect --format '{{.State.Running}}' acb-gateway-blue 2>/dev/null || echo "false")"
    local green_running
    green_running="$(docker inspect --format '{{.State.Running}}' acb-gateway-green 2>/dev/null || echo "false")"
    if [[ "$green_running" == "true" && "$blue_running" != "true" ]]; then
      slot="green"
    else
      slot="blue"
    fi
  fi
  printf '%s' "$slot"
}

get_candidate_slot() {
  local active="$1"
  if [[ "$active" == "blue" ]]; then
    printf 'green'
  else
    printf 'blue'
  fi
}

set_deploy_state() {
  local state="$1"
  local details="${2:-}"
  printf '{"state":"%s","timestamp":"%s","details":"%s"}\n' "$state" "$(date -u +'%Y-%m-%dT%H:%M:%SZ')" "$details" > "$DEPLOY_STATE_FILE"
}

get_deploy_state() {
  if [[ -f "$DEPLOY_STATE_FILE" ]]; then
    cat "$DEPLOY_STATE_FILE"
  else
    printf '{"state":"IDLE"}\n'
  fi
}

clear_deploy_state() {
  rm -f "$DEPLOY_STATE_FILE" 2>/dev/null || true
}

# Transaction Journal Primitives
init_tx_journal() {
  local component="$1"
  local candidate_slot="$2"
  local active_slot="$3"
  local candidate_digest="$4"
  local previous_digest="${5:-}"
  local expected_commit="${6:-}"

  mkdir -p "$(dirname "$TX_JOURNAL_FILE")"
  local tx_id
  tx_id="tx-$(date +%s)-$RANDOM"
  local now
  now="$(date -u +'%Y-%m-%dT%H:%M:%SZ')"

  atomic_write_file "$TX_JOURNAL_FILE" 600 <<EOF
{
  "tx_id": "${tx_id}",
  "component": "${component}",
  "state": "TX_INITIALIZED",
  "active_slot": "${active_slot}",
  "candidate_slot": "${candidate_slot}",
  "candidate_digest": "${candidate_digest}",
  "previous_digest": "${previous_digest}",
  "expected_commit": "${expected_commit}",
  "created_at": "${now}",
  "updated_at": "${now}",
  "details": "Transaction initialized"
}
EOF
  log_info "Deployment transaction journal initialized: [${tx_id}]"
}

update_tx_state() {
  local state="$1"
  local details="${2:-}"
  if [[ ! -f "$TX_JOURNAL_FILE" ]]; then
    return 0
  fi
  local now
  now="$(date -u +'%Y-%m-%dT%H:%M:%SZ')"
  local tmp="${TX_JOURNAL_FILE}.tmp.$$"

  if command -v jq >/dev/null 2>&1; then
    jq --arg s "$state" --arg d "$details" --arg t "$now" \
      '.state = $s | .details = $d | .updated_at = $t' "$TX_JOURNAL_FILE" > "$tmp" 2>/dev/null || true
  fi

  if [[ ! -s "$tmp" ]]; then
    sed -e "s/\"state\": \"[^\"]*\"/\"state\": \"$state\"/" \
        -e "s/\"updated_at\": \"[^\"]*\"/\"updated_at\": \"$now\"/" "$TX_JOURNAL_FILE" > "$tmp" 2>/dev/null || true
  fi

  if [[ -s "$tmp" ]]; then
    atomic_write_file "$TX_JOURNAL_FILE" 600 < "$tmp"
  fi
  rm -f "$tmp" 2>/dev/null || true
  log_info "Transaction state -> [${state}]"
}

get_tx_state() {
  if [[ ! -f "$TX_JOURNAL_FILE" ]]; then
    printf 'IDLE'
    return
  fi
  local st
  st="$(grep -o '"state":[[:space:]]*"[^"]*"' "$TX_JOURNAL_FILE" | head -n1 | cut -d'"' -f4 || echo "CORRUPT")"
  printf '%s' "${st:-CORRUPT}"
}

is_tx_committed() {
  local st
  st="$(get_tx_state)"
  if [[ "$st" == "TX_COMMITTED" || "$st" == "TX_SOAKING" || "$st" == "TX_COMPLETED" ]]; then
    return 0
  fi
  return 1
}

archive_tx_journal() {
  local suffix="${1:-archived}"
  if [[ -f "$TX_JOURNAL_FILE" ]]; then
    local ts
    ts="$(date +%s)"
    mv -f "$TX_JOURNAL_FILE" "${TX_JOURNAL_FILE}.${suffix}.${ts}" 2>/dev/null || true
  fi
}

recover_tx_journal() {
  if [[ ! -f "$TX_JOURNAL_FILE" ]]; then
    return 0
  fi

  local cur_state
  cur_state="$(get_tx_state)"
  if [[ "$cur_state" == "TX_COMPLETED" || "$cur_state" == "TX_ROLLED_BACK" || "$cur_state" == "IDLE" ]]; then
    archive_tx_journal "previous"
    return 0
  fi

  log_warn "RECOVERY: Found uncommitted or interrupted transaction journal in state [${cur_state}]!"

  local component
  component="$(grep -o '"component":[[:space:]]*"[^"]*"' "$TX_JOURNAL_FILE" 2>/dev/null | head -n1 | cut -d'"' -f4 || echo "gateway")"
  local cand_slot
  cand_slot="$(grep -o '"candidate_slot":[[:space:]]*"[^"]*"' "$TX_JOURNAL_FILE" 2>/dev/null | head -n1 | cut -d'"' -f4 || echo "")"
  local act_slot
  act_slot="$(grep -o '"active_slot":[[:space:]]*"[^"]*"' "$TX_JOURNAL_FILE" 2>/dev/null | head -n1 | cut -d'"' -f4 || echo "")"
  local prev_digest
  prev_digest="$(grep -o '"previous_digest":[[:space:]]*"[^"]*"' "$TX_JOURNAL_FILE" 2>/dev/null | head -n1 | cut -d'"' -f4 || echo "")"

  case "$component" in
    frontend)
      if [[ "$cur_state" != "TX_COMPLETED" ]]; then
        log_warn "RECOVERY: Reverting interrupted frontend candidate and route."
        if [[ "$act_slot" == "blue" || "$act_slot" == "green" || "$act_slot" == "legacy" ]]; then
          gateway_slot="$(get_active_slot 2>/dev/null || true)"
          if [[ "$gateway_slot" == "blue" || "$gateway_slot" == "green" ]]; then
            recovery_route="${ACB_CONFIG}.recovery.$$"
            render_traefik_config "$gateway_slot" "$recovery_route" "$act_slot" && mv -f "$recovery_route" "$ACB_CONFIG" || true
          fi
          if [[ "$act_slot" == "legacy" ]]; then
            rm -f "$FRONTEND_ACTIVE_SLOT_FILE"
          else
            printf '%s' "$act_slot" | atomic_write_file "$FRONTEND_ACTIVE_SLOT_FILE" 600
          fi
        fi
        [[ -z "$cand_slot" ]] || docker stop "acb-frontend-${cand_slot}" 2>/dev/null || true
      fi
      ;;
    gateway)
      if [[ -n "$cand_slot" && "$cur_state" != "TX_COMMITTED" && "$cur_state" != "TX_SOAKING" ]]; then
        log_info "RECOVERY: Stopping uncommitted candidate container [acb-gateway-${cand_slot}]..."
        docker compose -f "$COMPOSE_FILE" stop "gateway-${cand_slot}" 2>/dev/null || docker stop "acb-gateway-${cand_slot}" 2>/dev/null || true
        if [[ -n "$act_slot" && -f "$ACB_CONFIG" ]]; then
          if ! grep -q "acb-web-${act_slot}" "$ACB_CONFIG" 2>/dev/null; then
            log_warn "RECOVERY: Restoring Traefik route pointer back to known active slot [${act_slot}]..."
            if [[ -f "${ACB_CONFIG}.prev" ]]; then
              cp -f "${ACB_CONFIG}.prev" "$ACB_CONFIG" 2>/dev/null || true
            fi
            printf '%s' "$act_slot" > "$ACTIVE_SLOT_FILE" 2>/dev/null || true
          fi
        fi
      fi
      ;;
    worker)
      if [[ "$cur_state" != "TX_COMMITTED" && "$cur_state" != "TX_COMPLETED" ]]; then
        log_warn "RECOVERY: Interrupted worker transaction. Restoring singleton container..."
        docker stop -t 10 acb-worker 2>/dev/null || true
        if [[ -n "$prev_digest" ]]; then
          WORKER_IMAGE_REF="$prev_digest" docker compose -f "$COMPOSE_FILE" up -d --no-deps worker 2>/dev/null || true
        fi
      fi
      ;;
    auth-browser)
      if [[ "$cur_state" != "TX_COMMITTED" && "$cur_state" != "TX_COMPLETED" ]]; then
        log_warn "RECOVERY: Interrupted auth-browser transaction. Restoring previous container..."
        docker stop -t 5 acb-auth-browser 2>/dev/null || docker stop -t 5 acb-browser 2>/dev/null || true
        if [[ -n "$prev_digest" ]]; then
          BROWSER_IMAGE_REF="$prev_digest" docker compose -f "$COMPOSE_FILE" up -d --no-deps auth-browser 2>/dev/null || true
        fi
      fi
      ;;
    tts|tts-gateway)
      if [[ "$cur_state" != "TX_COMMITTED" && "$cur_state" != "TX_COMPLETED" ]]; then
        log_warn "RECOVERY: Interrupted TTS transaction. Restoring previous container..."
        docker stop -t 5 acb-tts-gateway 2>/dev/null || docker stop -t 5 tts-gateway 2>/dev/null || true
        if [[ -n "$prev_digest" ]]; then
          TTS_IMAGE_REF="$prev_digest" docker compose -f "$COMPOSE_FILE" up -d --no-deps tts-gateway 2>/dev/null || true
        fi
      fi
      ;;
    bark)
      if [[ "$cur_state" != "TX_COMMITTED" && "$cur_state" != "TX_COMPLETED" ]]; then
        log_warn "RECOVERY: Interrupted Bark transaction. Restoring previous container..."
        docker stop -t 5 acb-bark 2>/dev/null || docker stop -t 5 bark 2>/dev/null || true
        if [[ -n "$prev_digest" ]]; then
          BARK_IMAGE_REF="$prev_digest" docker compose -f "$COMPOSE_FILE" up -d --no-deps bark 2>/dev/null || true
        fi
      fi
      ;;
    *)
      log_warn "RECOVERY: Unrecognized component [$component] in transaction journal."
      ;;
  esac

  archive_tx_journal "recovered"
  clear_deploy_state
  log_info "RECOVERY: Transaction journal reconciled and recovered."
  return 0
}

mark_intentional_stop() {
  local slot="$1"
  if ! mkdir -p "$FAILOVER_STATE_DIR" 2>/dev/null || [[ ! -w "$FAILOVER_STATE_DIR" ]]; then
    log_error "Canonical failover state directory is unavailable: $FAILOVER_STATE_DIR"
    return 1
  fi
  local marker="$FAILOVER_STATE_DIR/intentional-stop-${slot}"
  local tmp="${marker}.tmp.$$"
  printf '{"slot":"%s","desired":"stopped","recordedAt":"%s"}\n' \
    "$slot" "$(date -u +'%Y-%m-%dT%H:%M:%SZ')" > "$tmp"
  chmod 0640 "$tmp"
  mv -f "$tmp" "$marker"
  touch "$FAILOVER_STATE_DIR/acb.cooldown"
  touch "$SCRIPT_DIR/.intentional-stop-${slot}" 2>/dev/null || true
  log_info "Recorded intentional stop for slot [${slot}] before stopping container."
}

clear_intentional_stop() {
  local slot="${1:-}"
  if [[ -z "$slot" ]]; then
    return 0
  fi
  rm -f "$FAILOVER_STATE_DIR/intentional-stop-${slot}" \
        "$FAILOVER_STATE_DIR/.intentional-stop-${slot}" \
        "$FAILOVER_STATE_DIR/acb/intentional-stop-${slot}" \
        "$FAILOVER_STATE_DIR/acb/.intentional-stop-${slot}" \
        "$SCRIPT_DIR/.intentional-stop-${slot}" 2>/dev/null || true
  log_info "Cleared intentional stop marker for slot [${slot}]."
}

stop_standby_container() {
  local slot="$1"
  log_info "Stopping container for slot [${slot}]..."
  mark_intentional_stop "$slot"
  compose_prod stop "gateway-${slot}" 2>/dev/null || docker compose -f "$COMPOSE_FILE" stop "gateway-${slot}" 2>/dev/null || docker stop "acb-gateway-${slot}" 2>/dev/null || true
  log_info "Slot [${slot}] stopped into warm standby state."
}

run_resumable_soak() {
  local candidate="$1"
  local old_slot="$2"
  local duration="${3:-900}"

  cat <<EOF > "$SOAK_STATE_FILE"
candidate=${candidate}
old_slot=${old_slot}
start_time=$(date +%s)
duration=${duration}
pid=$$
EOF

  log_info "Starting soak observation (${duration}s). Candidate [${candidate}] active, old slot [${old_slot}] running..."
  local elapsed=0
  local interval=10
  if [[ "$duration" -le 10 ]]; then
    interval=1
  fi

  while [[ "$elapsed" -lt "$duration" ]]; do
    if ! docker exec "acb-gateway-${candidate}" /gateway --healthcheck >/dev/null 2>&1; then
      log_error "Candidate [${candidate}] health probe failed during soak! Executing automatic route rollback to [${old_slot}]..."
      atomic_switch_route "$old_slot"
      ack_route_identity "$old_slot" "" 15 || true
      stop_standby_container "$candidate"
      rm -f "$SOAK_STATE_FILE"
      return 1
    fi
    sleep "$interval"
    elapsed=$(( elapsed + interval ))
  done

  if [[ "${DEFER_OLD_SLOT_RETIREMENT:-0}" == "1" ]]; then
    [[ -n "${PENDING_GATEWAY_RETIRE_FILE:-}" ]] || {
      log_error "PENDING_GATEWAY_RETIRE_FILE is required when old-slot retirement is deferred."
      return 1
    }
    printf 'old_slot=%s\ncandidate_slot=%s\n' "$old_slot" "$candidate" | atomic_write_file "$PENDING_GATEWAY_RETIRE_FILE" 600
    log_info "Soak completed; retaining old slot [${old_slot}] until the release commits."
  else
    log_info "Soak observation completed successfully. Transitioning [${old_slot}] to stopped warm standby..."
    stop_standby_container "$old_slot"
  fi
  rm -f "$SOAK_STATE_FILE"
  return 0
}

resume_soak() {
  if [[ ! -f "$SOAK_STATE_FILE" ]]; then
    log_info "No soak state file found ($SOAK_STATE_FILE). Nothing to resume."
    return 0
  fi
  # shellcheck disable=SC1090
  source "$SOAK_STATE_FILE"
  local now
  now="$(date +%s)"
  local elapsed=$(( now - start_time ))
  local remaining=$(( duration - elapsed ))
  if [[ "$remaining" -le 0 ]]; then
    log_info "Soak period elapsed during disconnection. Stopping [${old_slot}]..."
    stop_standby_container "$old_slot"
    rm -f "$SOAK_STATE_FILE"
    return 0
  fi
  log_info "Resuming soak observation with ${remaining}s remaining..."
  run_resumable_soak "$candidate" "$old_slot" "$remaining"
}
