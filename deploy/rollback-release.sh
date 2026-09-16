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

if [[ -L "$prev_dir" ]]; then
  log_error "Previous release directory must not be a symlink: $prev_dir"
  exit 1
fi

PREVIOUS_RELEASE_DIR="$(cd -- "$prev_dir" && pwd -P)"
log_info "Resolved Previous Release Directory: $PREVIOUS_RELEASE_DIR"

# Verify previous release directory resolves under RUNTIME_RELEASES_DIR if configured
if [[ -n "${RUNTIME_RELEASES_DIR:-}" ]]; then
  real_prev="$(cd -- "$PREVIOUS_RELEASE_DIR" 2>/dev/null && pwd -P || readlink -f "$PREVIOUS_RELEASE_DIR" 2>/dev/null || echo "$PREVIOUS_RELEASE_DIR")"
  real_root="$(cd -- "$RUNTIME_RELEASES_DIR" 2>/dev/null && pwd -P || readlink -f "$RUNTIME_RELEASES_DIR" 2>/dev/null || echo "$RUNTIME_RELEASES_DIR")"
  case "$real_prev" in
    "$real_root"/*) ;;
    *)
      if [[ -z "${DEPLOY_PATH:-}" || "$real_prev" != "${DEPLOY_PATH}"* ]]; then
        log_error "Previous release directory escapes RUNTIME_RELEASES_DIR ($RUNTIME_RELEASES_DIR): $PREVIOUS_RELEASE_DIR"
        exit 1
      fi
      ;;
  esac
fi

# Step 2b: Verify previous release bundle integrity before Docker mutation
prev_manifest=""
for m in "$PREVIOUS_RELEASE_DIR/release-manifest.json" "$PREVIOUS_RELEASE_DIR/manifest.json"; do
  if [[ -f "$m" ]]; then
    prev_manifest="$m"
    break
  fi
done

[[ -n "$prev_manifest" && -f "$prev_manifest" ]] || {
  log_error "Previous release manifest is missing in $PREVIOUS_RELEASE_DIR"
  exit 1
}

expected_prev_manifest_sha="$(python3 - "$STATE_FILE" "$PREVIOUS_RELEASE_DIR" <<'PY' 2>/dev/null || true
import json, sys
try:
    d = json.load(open(sys.argv[1]))
    prev = d.get("previous")
    pdir = sys.argv[2]
    if prev and prev.get("manifest_sha256") and (prev.get("release_dir") == pdir or not prev.get("release_dir")):
        print(prev["manifest_sha256"])
    elif d.get("manifest_sha256") and (d.get("release_dir") == pdir or not prev):
        print(d["manifest_sha256"])
except Exception:
    pass
PY
)"

actual_prev_manifest_sha="$(sha256sum "$prev_manifest" | awk '{print $1}')"
if [[ -n "$expected_prev_manifest_sha" && "$actual_prev_manifest_sha" != "$expected_prev_manifest_sha" ]]; then
  log_error "Previous release manifest SHA mismatch! Expected: $expected_prev_manifest_sha, actual: $actual_prev_manifest_sha"
  exit 1
fi

if ! python3 - "$prev_manifest" "$PREVIOUS_RELEASE_DIR" <<'PY'; then
import json, hashlib, pathlib, sys
manifest_path, release_dir = sys.argv[1:3]
with open(manifest_path, "r", encoding="utf-8") as f:
    m = json.load(f)
artifacts = m.get("artifacts", {})
root = pathlib.Path(release_dir)
for rel_path, exp_hash in artifacts.items():
    if not exp_hash or "=" in exp_hash or rel_path.startswith("failover-") or rel_path.startswith("compose_bundle") or rel_path.startswith("failover_bundle"):
        continue
    target = root / rel_path
    if target.is_file():
        actual = hashlib.sha256(target.read_bytes()).hexdigest()
        if actual != exp_hash:
            print(f"Artifact {rel_path} checksum mismatch: exp={exp_hash} act={actual}", file=sys.stderr)
            sys.exit(1)
PY
  log_error "Previous release bundle artifact checksum verification failed; refusing rollback."
  exit 1
fi

if [[ "${SKIP_MANIFEST_CHECK:-0}" -ne 1 && -f "$SCRIPT_DIR/verify-manifest.sh" ]]; then
  cosign_args=()
  [[ "${REQUIRE_COSIGN:-0}" == "1" ]] && cosign_args+=(--require-cosign)
  [[ -n "${EXPECTED_IDENTITY:-}" ]] && cosign_args+=(--expected-identity "$EXPECTED_IDENTITY")
  [[ -n "${EXPECTED_ISSUER:-}" ]] && cosign_args+=(--expected-issuer "$EXPECTED_ISSUER")
  if ! bash "$SCRIPT_DIR/verify-manifest.sh" --manifest "$prev_manifest" --deploy-dir "$PREVIOUS_RELEASE_DIR" "${cosign_args[@]}"; then
    log_error "Previous release manifest validation failed; refusing unsafe rollback."
    exit 1
  fi
fi

[[ -d "$PREVIOUS_RELEASE_DIR/compose" ]] || {
  log_error "Previous release directory lacks compose/ directory: $PREVIOUS_RELEASE_DIR/compose"
  exit 1
}

# Step 3: Set execution context to previous release bundle
export RELEASE_CONTEXT_DIR="$PREVIOUS_RELEASE_DIR"
export COMPOSE_ROOT="$PREVIOUS_RELEASE_DIR/compose"
export ROLLOUT_JOURNAL_FILE="$JOURNAL_FILE"

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
    worker_deployer="$PREVIOUS_RELEASE_DIR/deploy-worker.sh"
    [[ -f "$worker_deployer" ]] || worker_deployer="$SCRIPT_DIR/deploy-worker.sh"
    RELEASE_ORCHESTRATED=1 DEFER_RELEASE_STATE=1 \
      RELEASE_DIR="$PREVIOUS_RELEASE_DIR" RELEASE_CONTEXT_DIR="$PREVIOUS_RELEASE_DIR" COMPOSE_ROOT="$PREVIOUS_RELEASE_DIR/compose" \
      bash "$worker_deployer" "$worker_img" || rollback_failures=$((rollback_failures + 1))
  fi
fi

# 4d. Frontend Route
if [[ -f "${PENDING_FRONTEND_RETIRE_FILE:-}" ]] || step_was_completed frontend; then
  log_info "Restoring previous frontend route..."
  rollback_frontend_route || rollback_failures=$((rollback_failures + 1))
fi

# 4e. Bark
if step_was_completed bark && ([[ -f "$PREVIOUS_RELEASE_DIR/deploy-bark.sh" ]] || [[ -f "$SCRIPT_DIR/deploy-bark.sh" ]]); then
  log_info "Restoring previous Bark service using previous bundle..."
  bark_img=""
  if [[ -f "$SCRIPT_DIR/release-state.py" ]]; then
    bark_img="$(python3 "$SCRIPT_DIR/release-state.py" get "$STATE_FILE" images.bark 2>/dev/null || true)"
  fi
  if [[ -z "$bark_img" ]]; then
    bark_img="$(get_release_env BARK_IMAGE_REF 2>/dev/null || true)"
  fi
  if [[ -n "$bark_img" ]]; then
    bark_deployer="$PREVIOUS_RELEASE_DIR/deploy-bark.sh"
    [[ -f "$bark_deployer" ]] || bark_deployer="$SCRIPT_DIR/deploy-bark.sh"
    RELEASE_ORCHESTRATED=1 DEFER_RELEASE_STATE=1 \
      RELEASE_DIR="$PREVIOUS_RELEASE_DIR" RELEASE_CONTEXT_DIR="$PREVIOUS_RELEASE_DIR" COMPOSE_ROOT="$PREVIOUS_RELEASE_DIR/compose" \
      bash "$bark_deployer" "$bark_img" || rollback_failures=$((rollback_failures + 1))
  fi
fi

# 4f. TTS
if step_was_completed tts && ([[ -f "$PREVIOUS_RELEASE_DIR/deploy-tts.sh" ]] || [[ -f "$SCRIPT_DIR/deploy-tts.sh" ]]); then
  log_info "Restoring previous TTS service using previous bundle..."
  tts_img=""
  if [[ -f "$SCRIPT_DIR/release-state.py" ]]; then
    tts_img="$(python3 "$SCRIPT_DIR/release-state.py" get "$STATE_FILE" images.tts 2>/dev/null || true)"
  fi
  if [[ -z "$tts_img" ]]; then
    tts_img="$(get_release_env TTS_IMAGE_REF 2>/dev/null || true)"
  fi
  if [[ -n "$tts_img" ]]; then
    tts_deployer="$PREVIOUS_RELEASE_DIR/deploy-tts.sh"
    [[ -f "$tts_deployer" ]] || tts_deployer="$SCRIPT_DIR/deploy-tts.sh"
    RELEASE_ORCHESTRATED=1 DEFER_RELEASE_STATE=1 \
      RELEASE_DIR="$PREVIOUS_RELEASE_DIR" RELEASE_CONTEXT_DIR="$PREVIOUS_RELEASE_DIR" COMPOSE_ROOT="$PREVIOUS_RELEASE_DIR/compose" \
      bash "$tts_deployer" "$tts_img" || rollback_failures=$((rollback_failures + 1))
  fi
fi

# 4g. Auth Browser
if step_was_completed auth_browser && ([[ -f "$PREVIOUS_RELEASE_DIR/deploy-auth-browser.sh" ]] || [[ -f "$SCRIPT_DIR/deploy-auth-browser.sh" ]]); then
  log_info "Restoring previous Auth-Browser service using previous bundle..."
  br_img=""
  if [[ -f "$SCRIPT_DIR/release-state.py" ]]; then
    br_img="$(python3 "$SCRIPT_DIR/release-state.py" get "$STATE_FILE" images.auth_browser 2>/dev/null || true)"
  fi
  if [[ -z "$br_img" ]]; then
    br_img="$(get_release_env BROWSER_IMAGE_REF 2>/dev/null || true)"
  fi
  if [[ -n "$br_img" ]]; then
    ab_deployer="$PREVIOUS_RELEASE_DIR/deploy-auth-browser.sh"
    [[ -f "$ab_deployer" ]] || ab_deployer="$SCRIPT_DIR/deploy-auth-browser.sh"
    RELEASE_ORCHESTRATED=1 DEFER_RELEASE_STATE=1 \
      RELEASE_DIR="$PREVIOUS_RELEASE_DIR" RELEASE_CONTEXT_DIR="$PREVIOUS_RELEASE_DIR" COMPOSE_ROOT="$PREVIOUS_RELEASE_DIR/compose" \
      bash "$ab_deployer" "$br_img" || rollback_failures=$((rollback_failures + 1))
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
    finish_rollout_journal "ROLLED_BACK"
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
