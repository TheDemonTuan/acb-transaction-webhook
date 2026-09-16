#!/usr/bin/env bash
# deploy/rollback-release.sh
# Restores the exact previous release bundle (code, compose config, scripts, images, routes).
set -Eeuo pipefail
umask 077

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
STATE_FILE="${CURRENT_RELEASE_FILE:-${DEPLOY_PATH:-$(cd -- "$SCRIPT_DIR/.." && pwd)}/state/current-release.json}"
JOURNAL_FILE="${ROLLOUT_JOURNAL_FILE:-${DEPLOY_PATH:-$(cd -- "$SCRIPT_DIR/.." && pwd)}/data/rollout-journal.json}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --state) STATE_FILE="$2"; shift 2 ;;
    --journal) JOURNAL_FILE="$2"; shift 2 ;;
    *) printf 'Unknown rollback-release argument: %s\n' "$1" >&2; exit 1 ;;
  esac
done

if [[ -f "$SCRIPT_DIR/runtime-layout.sh" ]]; then
  # shellcheck source=deploy/runtime-layout.sh
  source "$SCRIPT_DIR/runtime-layout.sh"
fi

# shellcheck source=deploy/lib.sh
source "$SCRIPT_DIR/lib.sh"

log_info "=========================================================="
log_info "Starting Exact Previous Release Rollback"
log_info "Authoritative State File: $STATE_FILE"
log_info "Rollout Journal: $JOURNAL_FILE"
log_info "=========================================================="

acquire_deploy_lock
trap 'release_deploy_lock' EXIT

[[ -s "$STATE_FILE" ]] || { log_error "Canonical state file is missing: $STATE_FILE"; exit 1; }

# Step 1: Validate canonical state
if [[ -f "$SCRIPT_DIR/release-state.py" ]]; then
  python3 "$SCRIPT_DIR/release-state.py" validate "$STATE_FILE" || {
    log_error "Canonical release state failed schema validation; refusing unsafe rollback."
    exit 1
  }
fi

# Step 2: Determine previous release bundle directory
prev_dir=""
if [[ -f "$SCRIPT_DIR/release-state.py" ]]; then
  prev_dir="$(python3 "$SCRIPT_DIR/release-state.py" get "$STATE_FILE" previous.release_dir 2>/dev/null || true)"
fi
if [[ -z "$prev_dir" && -f "$JOURNAL_FILE" ]]; then
  prev_dir="$(python3 - "$JOURNAL_FILE" <<'PY' 2>/dev/null || true
import json, sys
try:
    print(json.load(open(sys.argv[1])).get("previous_release_dir", ""))
except Exception:
    pass
PY
)"
fi

if [[ -z "$prev_dir" || ! -d "$prev_dir" ]]; then
  # Fallback: check release_dir of current state
  prev_dir="$(python3 - "$STATE_FILE" <<'PY' 2>/dev/null || true
import json, sys
try:
    d = json.load(open(sys.argv[1]))
    print(d.get("release_dir", ""))
except Exception:
    pass
PY
)"
fi

[[ -n "$prev_dir" && -d "$prev_dir" ]] || {
  log_error "Could not resolve valid previous release directory."
  exit 1
}

PREVIOUS_RELEASE_DIR="$(cd -- "$prev_dir" && pwd -P)"
log_info "Resolved Previous Release Directory: $PREVIOUS_RELEASE_DIR"

# Verify previous release directory resolves under RUNTIME_RELEASES_DIR if configured
if [[ -n "${RUNTIME_RELEASES_DIR:-}" ]]; then
  case "$PREVIOUS_RELEASE_DIR" in
    "$RUNTIME_RELEASES_DIR"/*) ;;
    *)
      # Also allow if it matches current deploy root
      if [[ "$PREVIOUS_RELEASE_DIR" != "${DEPLOY_PATH:-}"* ]]; then
        log_warn "Previous release directory is not directly under RUNTIME_RELEASES_DIR: $PREVIOUS_RELEASE_DIR"
      fi
      ;;
  esac
fi

# Step 3: Set execution context to previous release bundle
export RELEASE_CONTEXT_DIR="$PREVIOUS_RELEASE_DIR"
export COMPOSE_ROOT="$PREVIOUS_RELEASE_DIR/compose"

step_was_completed() {
  local step="$1"
  if [[ ! -f "$JOURNAL_FILE" ]]; then
    return 1
  fi
  python3 - "$JOURNAL_FILE" "$step" <<'PY' 2>/dev/null
import json, sys
try:
    data = json.load(open(sys.argv[1], encoding="utf-8"))
    sys.exit(0 if data.get("steps", {}).get(sys.argv[2]) == "STEP_COMPLETED" else 1)
except Exception:
    sys.exit(1)
PY
}

rollback_failures=0

# Step 4: Reverse dependency order component rollback
log_info "Reverting completed components in reverse order..."

# 4a. Failover Controller
if step_was_completed failover_controller && [[ -f "$SCRIPT_DIR/deploy-failover-controller.sh" ]]; then
  log_info "Restoring previous failover controller bundle..."
  FAILOVER_ROLLBACK_ONLY=1 RELEASE_ORCHESTRATED=1 DEFER_RELEASE_STATE=1 \
    bash "$SCRIPT_DIR/deploy-failover-controller.sh" || rollback_failures=$((rollback_failures + 1))
fi

# 4b. Gateway Route
if [[ -f "${PENDING_GATEWAY_RETIRE_FILE:-}" ]] || step_was_completed gateway; then
  log_info "Restoring previous gateway route..."
  rollback_gateway_route || rollback_failures=$((rollback_failures + 1))
fi

# 4c. Worker
if step_was_completed worker && [[ -f "$SCRIPT_DIR/deploy-worker.sh" ]]; then
  log_info "Restoring previous worker container using previous bundle..."
  worker_img=""
  if [[ -f "$SCRIPT_DIR/release-state.py" ]]; then
    worker_img="$(python3 "$SCRIPT_DIR/release-state.py" get "$STATE_FILE" images.worker 2>/dev/null || true)"
  fi
  if [[ -z "$worker_img" ]]; then
    worker_img="$(get_release_env WORKER_IMAGE_REF 2>/dev/null || true)"
  fi
  if [[ -n "$worker_img" ]]; then
    RELEASE_ORCHESTRATED=1 DEFER_RELEASE_STATE=1 \
      bash "$SCRIPT_DIR/deploy-worker.sh" "$worker_img" || rollback_failures=$((rollback_failures + 1))
  fi
fi

# 4d. Frontend Route
if [[ -f "${PENDING_FRONTEND_RETIRE_FILE:-}" ]] || step_was_completed frontend; then
  log_info "Restoring previous frontend route..."
  rollback_frontend_route || rollback_failures=$((rollback_failures + 1))
fi

# 4e. Bark
if step_was_completed bark && [[ -f "$SCRIPT_DIR/deploy-bark.sh" ]]; then
  log_info "Restoring previous Bark service using previous bundle..."
  bark_img="$(get_release_env BARK_IMAGE_REF 2>/dev/null || true)"
  if [[ -n "$bark_img" ]]; then
    RELEASE_ORCHESTRATED=1 DEFER_RELEASE_STATE=1 \
      bash "$SCRIPT_DIR/deploy-bark.sh" "$bark_img" || rollback_failures=$((rollback_failures + 1))
  fi
fi

# 4f. TTS
if step_was_completed tts && [[ -f "$SCRIPT_DIR/deploy-tts.sh" ]]; then
  log_info "Restoring previous TTS service using previous bundle..."
  tts_img="$(get_release_env TTS_IMAGE_REF 2>/dev/null || true)"
  if [[ -n "$tts_img" ]]; then
    RELEASE_ORCHESTRATED=1 DEFER_RELEASE_STATE=1 \
      bash "$SCRIPT_DIR/deploy-tts.sh" "$tts_img" || rollback_failures=$((rollback_failures + 1))
  fi
fi

# 4g. Auth Browser
if step_was_completed auth_browser && [[ -f "$SCRIPT_DIR/deploy-auth-browser.sh" ]]; then
  log_info "Restoring previous Auth-Browser service using previous bundle..."
  br_img="$(get_release_env BROWSER_IMAGE_REF 2>/dev/null || true)"
  if [[ -n "$br_img" ]]; then
    RELEASE_ORCHESTRATED=1 DEFER_RELEASE_STATE=1 \
      bash "$SCRIPT_DIR/deploy-auth-browser.sh" "$br_img" || rollback_failures=$((rollback_failures + 1))
  fi
fi

# Step 5: Verify runtime drift against authoritative canonical state
if [[ "${SKIP_MANIFEST_CHECK:-0}" -eq 1 && -n "${RUNTIME_DRIFT_CHECK_CMD:-}" ]]; then
  eval "$RUNTIME_DRIFT_CHECK_CMD" || rollback_failures=$((rollback_failures + 1))
elif [[ -f "$SCRIPT_DIR/verify-runtime-drift.sh" ]]; then
  bash "$SCRIPT_DIR/verify-runtime-drift.sh" --state "$STATE_FILE" || rollback_failures=$((rollback_failures + 1))
fi

if (( rollback_failures == 0 )); then
  if [[ -f "$JOURNAL_FILE" ]]; then
    python3 - "$JOURNAL_FILE" <<'PY' 2>/dev/null || true
import json, sys, datetime
path = sys.argv[1]
with open(path, "r", encoding="utf-8") as f:
    d = json.load(f)
d["status"] = "ROLLED_BACK"
d["rolled_back_at"] = datetime.datetime.now(datetime.timezone.utc).isoformat()
with open(path, "w", encoding="utf-8") as f:
    json.dump(d, f, indent=2)
PY
    archived="${JOURNAL_FILE}.rolled_back.$(date +%s)"
    mv -f "$JOURNAL_FILE" "$archived"
    log_info "Rollout journal archived as ROLLED_BACK: $archived"
  fi
  log_info "Release rollback completed successfully."
  exit 0
else
  log_error "Release rollback encountered $rollback_failures failure(s); preserving journal and evidence."
  exit 1
fi
