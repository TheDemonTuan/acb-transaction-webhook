#!/usr/bin/env bash
# deploy/preflight-runtime.sh
# Inspects actual VPS runtime drift against canonical release state before deploy mutations.
# Supports --check-only and --reconcile.
set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
if [[ -f "$SCRIPT_DIR/runtime-layout.sh" ]]; then
  # shellcheck source=deploy/runtime-layout.sh
  source "$SCRIPT_DIR/runtime-layout.sh"
fi
# shellcheck source=deploy/lib.sh
source "$SCRIPT_DIR/lib.sh"

STATE_FILE="${CURRENT_RELEASE_FILE:-${RUNTIME_STATE_DIR:-${DEPLOY_PATH:-/opt/acb-transaction-webhook}/state}/current-release.json}"
DATA_DIR="${RUNTIME_DATA_DIR:-${DEPLOY_PATH:-/opt/acb-transaction-webhook}/data}"
ACB_CONFIG="${ACB_CONFIG:-/opt/platform/edge/dynamic/acb.yml}"
ALLOW_BOOTSTRAP="${ALLOW_BOOTSTRAP:-0}"
MODE="check-only"
EXPECTED_BASE_SHA=""
EXPECTED_BASE_GENERATION=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --state) STATE_FILE="$2"; shift 2 ;;
    --data-dir) DATA_DIR="$2"; shift 2 ;;
    --config) ACB_CONFIG="$2"; shift 2 ;;
    --allow-bootstrap) ALLOW_BOOTSTRAP=1; shift ;;
    --check-only) MODE="check-only"; shift ;;
    --reconcile) MODE="reconcile"; shift ;;
    --expected-base-sha) EXPECTED_BASE_SHA="$2"; shift 2 ;;
    --expected-base-generation) EXPECTED_BASE_GENERATION="$2"; shift 2 ;;
    *) log_error "Unknown preflight argument: $1"; exit 1 ;;
  esac
done

log_info "=========================================================="
log_info "Starting Runtime Preflight Drift Verification (Mode: $MODE)"
log_info "State file: $STATE_FILE"
log_info "=========================================================="

# Check if bootstrap mode
if [[ ! -s "$STATE_FILE" ]]; then
  if [[ "$ALLOW_BOOTSTRAP" -eq 1 && -z "$EXPECTED_BASE_SHA" && -z "$EXPECTED_BASE_GENERATION" ]]; then
    log_info "PREFLIGHT: current-release.json is absent; bootstrap mode permitted."
    exit 0
  fi
  log_error "PREFLIGHT_FAIL: Canonical release state missing: $STATE_FILE"
  exit 1
fi

check_runtime_drift() {
  local drift_found=0

  # 1. Check markers / journals
  local rollout_journal="$DATA_DIR/rollout-journal.json"
  if [[ -f "$rollout_journal" ]]; then
    local r_status
    r_status="$(python3 -c 'import json, sys; print(json.load(open(sys.argv[1])).get("status", "UNKNOWN"))' "$rollout_journal" 2>/dev/null || echo "UNKNOWN")"
    if [[ "$r_status" != "COMPLETED" && "$r_status" != "ROLLED_BACK" ]]; then
      log_warn "PREFLIGHT_DIRT: Found uncompleted rollout journal with status: $r_status"
      drift_found=1
    fi
  fi

  if [[ -f "$DATA_DIR/pending-gateway-retire.env" ]]; then
    log_warn "PREFLIGHT_DIRT: Found pending gateway retire evidence: $DATA_DIR/pending-gateway-retire.env"
    drift_found=1
  fi

  if [[ -f "$DATA_DIR/pending-frontend-retire.env" ]]; then
    log_warn "PREFLIGHT_DIRT: Found pending frontend retire evidence: $DATA_DIR/pending-frontend-retire.env"
    drift_found=1
  fi

  if [[ -f "${TX_JOURNAL_FILE:-$DATA_DIR/deploy-journal.json}" ]]; then
    local tx_file="${TX_JOURNAL_FILE:-$DATA_DIR/deploy-journal.json}"
    local tx_state
    tx_state="$(python3 -c 'import json, sys; print(json.load(open(sys.argv[1])).get("state", "UNKNOWN"))' "$tx_file" 2>/dev/null || echo "UNKNOWN")"
    if [[ "$tx_state" != "TX_COMPLETED" && "$tx_state" != "TX_ROLLED_BACK" && "$tx_state" != "IDLE" ]]; then
      log_warn "PREFLIGHT_DIRT: Found uncommitted component transaction: $tx_state"
      drift_found=1
    fi
  fi

  # 2. Check actual Docker containers and Traefik route against canonical state
  local verifier=""
  if [[ -f "$SCRIPT_DIR/verify-runtime-drift.sh" ]]; then
    verifier="$SCRIPT_DIR/verify-runtime-drift.sh"
  elif [[ -f "${DEPLOY_DIR:-}/verify-runtime-drift.sh" ]]; then
    verifier="${DEPLOY_DIR:-}/verify-runtime-drift.sh"
  fi

  if [[ "${SKIP_MANIFEST_CHECK:-0}" -eq 1 && -n "${RUNTIME_DRIFT_CHECK_CMD:-}" ]]; then
    if ! eval "$RUNTIME_DRIFT_CHECK_CMD"; then
      log_warn "PREFLIGHT_DIRT: Runtime drift detected via check command override."
      drift_found=1
    fi
  elif [[ -n "$verifier" ]]; then
    if ! bash "$verifier" --state "$STATE_FILE"; then
      log_warn "PREFLIGHT_DIRT: Actual runtime drift detected via verify-runtime-drift.sh!"
      drift_found=1
    fi
  else
    log_error "PREFLIGHT_FAIL: Trusted runtime drift verifier is missing."
    drift_found=1
  fi

  return "$drift_found"
}

check_baseline() {
  if [[ -n "$EXPECTED_BASE_SHA" || -n "$EXPECTED_BASE_GENERATION" ]]; then
    python3 - "$STATE_FILE" "$EXPECTED_BASE_SHA" "$EXPECTED_BASE_GENERATION" <<'PY'
import json, re, sys
try:
    with open(sys.argv[1], encoding='utf-8') as file:
        state = json.load(file)
    sha, generation = sys.argv[2:4]
    if not re.fullmatch(r'[0-9a-f]{40}', sha) or not re.fullmatch(r'(0|[1-9][0-9]*)', generation):
        raise ValueError('invalid signed baseline')
    if state.get('status') != 'COMPLETED' or state.get('git_sha') != sha or type(state.get('generation')) is not int or state['generation'] != int(generation):
        raise ValueError('production changed after build')
except (OSError, ValueError, KeyError, TypeError) as error:
    raise SystemExit(f'STALE_RELEASE_BASELINE: {error}; start a new workflow run')
PY
  fi
}

if [[ "$MODE" == "reconcile" ]]; then
  acquire_deploy_lock
  trap 'release_deploy_lock' EXIT
fi
check_baseline
if ! check_runtime_drift; then
  if [[ "$MODE" == "check-only" ]]; then
    log_error "PREFLIGHT_FAIL: VPS runtime has drifted from canonical release state or has dirty journals."
    exit 1
  fi
  log_warn "Reconciling dirty runtime back to canonical release..."
  ARCHIVE_ROLLOUT_JOURNAL=1 \
    reconcile_runtime_to_canonical "$STATE_FILE" "${ROLLOUT_JOURNAL_FILE:-$DATA_DIR/rollout-journal.json}" || {
    log_error "Reconciliation failed to restore canonical runtime."
    exit 1
  }
  check_runtime_drift || { log_error "Runtime still drifted after reconciliation."; exit 1; }
fi
check_baseline
log_info "PREFLIGHT_PASS: VPS runtime matches canonical release state."
