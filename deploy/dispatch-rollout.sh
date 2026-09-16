#!/usr/bin/env bash
# deploy/dispatch-rollout.sh
# Minimal Trusted CI Promotion Dispatcher and Bounded Rollout Orchestrator.
#
# Invariants:
# 1. Consumes signed release manifest & verified promotion scope.
# 2. Refuses any component promotion not explicitly authorized by the signed scope.
# 3. Executes authorized component transactions in strict dependency order:
#    schema -> auxiliary sidecars (auth-browser, tts, bark) -> worker -> gateway -> platform
# 4. Docs-only changes promote zero runtime containers/migrations and record release receipt.
# 5. Acquires explicit remote release lock (/run/lock/vps-failover/acb.lock) coordinated with failover controller.
# 6. Handles signals (INT, TERM, HUP) with clean traps, failure recording, and journal persistence.
# 7. Performs startup recovery (recover_tx_journal) to clean any interrupted prior transaction.
# 8. Never uses whole-stack compose up/down shortcuts; calls only transactional component scripts.
# 9. Records durable release evidence in data/releases/<release_id>/ and updates last-success release state.
set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deploy/lib.sh
source "$SCRIPT_DIR/lib.sh"

DEPLOY_DIR="$SCRIPT_DIR"
DATA_DIR="${DATA_DIR:-}"
if [[ -z "$DATA_DIR" ]]; then
  if [[ -n "${DEPLOY_PATH:-}" && -d "${DEPLOY_PATH}/data" ]]; then
    DATA_DIR="${DEPLOY_PATH}/data"
  elif [[ -d "$SCRIPT_DIR/../data" ]]; then
    DATA_DIR="$SCRIPT_DIR/../data"
  else
    DATA_DIR="$SCRIPT_DIR/data"
  fi
fi
mkdir -p "$DATA_DIR"

MANIFEST_FILE="${MANIFEST_FILE:-$DEPLOY_DIR/release-manifest.json}"
BUNDLE_FILE="${BUNDLE_FILE:-$DEPLOY_DIR/release-manifest.bundle}"
LAST_RELEASE_FILE="${LAST_RELEASE_FILE:-$DATA_DIR/last-release.json}"
ROLLOUT_JOURNAL_FILE="${ROLLOUT_JOURNAL_FILE:-$DATA_DIR/rollout-journal.json}"
RELEASES_DIR="${RELEASES_DIR:-$DATA_DIR/releases}"
PENDING_GATEWAY_RETIRE_FILE="${PENDING_GATEWAY_RETIRE_FILE:-$DATA_DIR/pending-gateway-retire.env}"
PENDING_FRONTEND_RETIRE_FILE="${PENDING_FRONTEND_RETIRE_FILE:-$DATA_DIR/pending-frontend-retire.env}"

EXPECTED_IDENTITY="${EXPECTED_IDENTITY:-}"
EXPECTED_ISSUER="${EXPECTED_ISSUER:-https://token.actions.githubusercontent.com}"
REQUIRE_COSIGN="${REQUIRE_COSIGN:-0}"
SKIP_MANIFEST_CHECK="${SKIP_MANIFEST_CHECK:-0}"
ALLOW_REDEPLOY="${ALLOW_REDEPLOY:-0}"
MAX_AGE_SECONDS="${MAX_AGE_SECONDS:-86400}"
SOAK_SECONDS="${SOAK_SECONDS:-${SOAK_DURATION_SEC:-900}}"
DETACH_SOAK="${DETACH_SOAK:-0}"
RESUME_SOAK="${RESUME_SOAK:-0}"
REQUESTED_SCOPE="${REQUESTED_SCOPE:-}"

IMAGE_FRONTEND="${FRONTEND_IMAGE_REF:-}"
IMAGE_GATEWAY="${GATEWAY_IMAGE_REF:-${IMAGE_REF:-}}"
IMAGE_WORKER="${WORKER_IMAGE_REF:-}"
IMAGE_DBTOOL="${DBTOOL_IMAGE_REF:-}"
IMAGE_AUTH_BROWSER="${AUTH_BROWSER_IMAGE_REF:-${BROWSER_IMAGE_REF:-}}"
IMAGE_TTS_GATEWAY="${TTS_GATEWAY_IMAGE_REF:-${TTS_IMAGE_REF:-}}"
IMAGE_BARK="${BARK_IMAGE_REF:-}"

usage() {
  cat <<'EOF'
Usage: dispatch-rollout.sh [options]

Options:
  --manifest <path>               Path to release-manifest.json
  --bundle <path>                 Path to release-manifest.bundle
  --deploy-dir <dir>              Directory containing deploy scripts (default: script dir)
  --data-dir <dir>                Directory for persistent deployment data/journals
  --expected-identity <id>        Exact Cosign OIDC identity (required if cosign verified)
  --expected-issuer <issuer>      Expected Cosign OIDC issuer
  --require-cosign                Enforce Cosign signature verification
  --skip-manifest-check           Bypass manifest signature and anti-replay (testing only)
  --allow-redeploy                Allow re-deploying currently deployed commit
  --max-age-seconds <sec>         Maximum acceptable manifest age (default: 86400)
  --soak-seconds <sec>            Soak duration for gateway (default: 900)
  --frontend-image <ref>          Frontend immutable image digest
  --detach-soak                   Detach soak observation in background
  --resume-soak                   Resume active soak observation
  --scope <components>            Explicit comma-separated component scope override
  --gateway-image <ref>           Override candidate gateway image digest
  --worker-image <ref>            Override candidate worker image digest
  --dbtool-image <ref>            Override candidate dbtool image digest
  --browser-image <ref>           Override candidate auth-browser image digest
  --tts-image <ref>               Override candidate tts image digest
  --bark-image <ref>              Override candidate bark image digest
  --help, -h                      Show this help message
EOF
}

# Parse CLI arguments
while [[ $# -gt 0 ]]; do
  case "$1" in
    --manifest)
      MANIFEST_FILE="$2"
      shift 2
      ;;
    --bundle)
      BUNDLE_FILE="$2"
      shift 2
      ;;
    --deploy-dir)
      DEPLOY_DIR="$2"
      shift 2
      ;;
    --data-dir)
      DATA_DIR="$2"
      shift 2
      ;;
    --runtime-root)
      RUNTIME_ROOT="$2"
      shift 2
      ;;
    --expected-identity)
      EXPECTED_IDENTITY="$2"
      shift 2
      ;;
    --expected-issuer)
      EXPECTED_ISSUER="$2"
      shift 2
      ;;
    --require-cosign)
      REQUIRE_COSIGN=1
      shift
      ;;
    --skip-manifest-check)
      SKIP_MANIFEST_CHECK=1
      shift
      ;;
    --allow-redeploy)
      ALLOW_REDEPLOY=1
      shift
      ;;
    --max-age-seconds)
      MAX_AGE_SECONDS="$2"
      shift 2
      ;;
    --soak-seconds)
      SOAK_SECONDS="$2"
      shift 2
      ;;
    --detach-soak)
      DETACH_SOAK=1
      shift
      ;;
    --resume-soak)
      RESUME_SOAK=1
      shift
      ;;
    --scope)
      REQUESTED_SCOPE="$2"
      shift 2
      ;;
    --frontend-image)
      IMAGE_FRONTEND="$2"
      shift 2
      ;;
    --gateway-image)
      IMAGE_GATEWAY="$2"
      shift 2
      ;;
    --worker-image)
      IMAGE_WORKER="$2"
      shift 2
      ;;
    --dbtool-image)
      IMAGE_DBTOOL="$2"
      shift 2
      ;;
    --browser-image)
      IMAGE_AUTH_BROWSER="$2"
      shift 2
      ;;
    --tts-image)
      IMAGE_TTS_GATEWAY="$2"
      shift 2
      ;;
    --bark-image)
      IMAGE_BARK="$2"
      shift 2
      ;;
    --help|-h)
      usage
      exit 0
      ;;
    -*)
      log_warn "Unknown option: $1"
      shift
      ;;
    *)
      if [[ -z "$IMAGE_GATEWAY" && "$1" =~ ^[^[:space:]]+@sha256:[a-f0-9]{64}$ ]]; then
        IMAGE_GATEWAY="$1"
      fi
      shift
      ;;
  esac
done

if [[ "$RESUME_SOAK" -eq 1 ]]; then
  log_info "Resuming soak observation..."
  resume_soak
  exit 0
fi

# Recompute data paths if DATA_DIR was set or overridden
mkdir -p "$DATA_DIR"
LAST_RELEASE_FILE="$DATA_DIR/last-release.json"
ROLLOUT_JOURNAL_FILE="$DATA_DIR/rollout-journal.json"
RELEASES_DIR="$DATA_DIR/releases"
PENDING_GATEWAY_RETIRE_FILE="$DATA_DIR/pending-gateway-retire.env"
PENDING_FRONTEND_RETIRE_FILE="$DATA_DIR/pending-frontend-retire.env"

# CI must wait for soak completion before recording a successful release.
# Detached soak remains an explicit operator-only mode.

ROLLOUT_EXIT_CODE=0
rollback_gateway_route() {
  local old_slot candidate_slot
  if [[ -f "$PENDING_GATEWAY_RETIRE_FILE" ]]; then
    old_slot="$(sed -n 's/^old_slot=//p' "$PENDING_GATEWAY_RETIRE_FILE")"
    candidate_slot="$(sed -n 's/^candidate_slot=//p' "$PENDING_GATEWAY_RETIRE_FILE")"
    if [[ "$old_slot" =~ ^(blue|green)$ && "$candidate_slot" =~ ^(blue|green)$ ]]; then
      log_warn "Reverting gateway route to [$old_slot] because the release did not commit."
      if ! atomic_switch_route "$old_slot"; then
        log_error "Gateway route rollback switch to [$old_slot] failed; preserving pending rollback evidence."
        return 1
      fi
      if ! ack_route_identity "$old_slot" "" 30; then
        log_error "Gateway route rollback ACK failed for [$old_slot]; preserving both slots and evidence."
        return 1
      fi
      printf '%s' "$old_slot" | atomic_write_file "$ACTIVE_SLOT_FILE" 600
      stop_standby_container "$candidate_slot" || true
      rm -f "$PENDING_GATEWAY_RETIRE_FILE"
    else
      log_error "Gateway rollback evidence is malformed; refusing to discard it."
      return 1
    fi
  fi
  return 0
}

rollback_frontend_route() {
  if [[ -f "$PENDING_FRONTEND_RETIRE_FILE" ]]; then
    local PREV_TOP="" CAND_TOP="" PREV_CNT="" CAND_CNT="" GW_SLOT=""
    eval "$(parse_pending_frontend_evidence "$PENDING_FRONTEND_RETIRE_FILE" 2>/dev/null || true)"
    local prev_top="$PREV_TOP" cand_top="$CAND_TOP" prev_cnt="$PREV_CNT" cand_cnt="$CAND_CNT"

    if [[ -z "$cand_cnt" && -n "$cand_top" ]]; then
      cand_cnt="acb-frontend-${cand_top}"
    fi
    if [[ -z "$prev_cnt" && -n "$prev_top" ]]; then
      if [[ "$prev_top" == "legacy" ]]; then
        prev_cnt="acb-frontend"
      else
        prev_cnt="acb-frontend-${prev_top}"
      fi
    fi

    if [[ "$prev_top" =~ ^(blue|green|legacy)$ && "$cand_top" =~ ^(blue|green)$ ]]; then
      log_warn "Reverting frontend route to topology [$prev_top] because the release did not commit."
      if [[ "$prev_top" == "legacy" ]]; then
        if ! docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "acb-frontend" 2>/dev/null | grep -Eq '^(healthy|running)$'; then
          log_error "Legacy frontend container acb-frontend is missing or unhealthy; failing closed."
          return 1
        fi
        local current_gw_slot
        current_gw_slot="$(get_active_slot 2>/dev/null || cat "$ACTIVE_SLOT_FILE" 2>/dev/null || printf 'blue')"
        local tmp_route="${ACB_CONFIG}.rollback.$$"
        render_traefik_config "$current_gw_slot" "$tmp_route" "legacy"
        if ! atomic_write_file "$ACB_CONFIG" 644 < "$tmp_route"; then
          rm -f "$tmp_route"
          log_error "Frontend legacy route rollback file write failed; preserving evidence."
          return 1
        fi
        rm -f "$tmp_route"
        if ! ack_frontend_route 30; then
          log_error "Frontend legacy route rollback ACK failed; preserving both containers and pending evidence."
          return 1
        fi
        rm -f "$FRONTEND_ACTIVE_SLOT_FILE"
        docker stop --time "${FRONTEND_STOP_TIMEOUT:-10}" "$cand_cnt" >/dev/null 2>&1 || true
        rm -f "$PENDING_FRONTEND_RETIRE_FILE"
        log_info "Frontend reverted to legacy container acb-frontend."
      else
        if ! atomic_switch_frontend_route "$prev_top"; then
          log_error "Frontend route rollback switch to [$prev_top] failed; preserving evidence."
          return 1
        fi
        if ! ack_frontend_route 30; then
          log_error "Frontend route rollback ACK failed for [$prev_top]; preserving both containers and pending evidence."
          return 1
        fi
        printf '%s' "$prev_top" | atomic_write_file "$FRONTEND_ACTIVE_SLOT_FILE" 600
        docker stop --time "${FRONTEND_STOP_TIMEOUT:-10}" "$cand_cnt" >/dev/null 2>&1 || true
        rm -f "$PENDING_FRONTEND_RETIRE_FILE"
        log_info "Frontend reverted to slot [$prev_top]."
      fi
    else
      log_error "Frontend rollback evidence is malformed; refusing to discard it."
      return 1
    fi
  fi
  return 0
}

rollback_pending_routes() {
  local failures=0
  rollback_gateway_route || failures=$((failures + 1))
  rollback_frontend_route || failures=$((failures + 1))
  return "$failures"
}

verify_previous_runtime() {
  local current_state="${CURRENT_RELEASE_FILE:-${DEPLOY_PATH:-$(cd -- "$DEPLOY_DIR/.." && pwd)}/state/current-release.json}"
  if [[ ! -s "$current_state" ]]; then
    log_error "Canonical release state is missing: $current_state"
    return 1
  fi
  if [[ "${SKIP_MANIFEST_CHECK:-0}" -eq 1 && -n "${RUNTIME_DRIFT_CHECK_CMD:-}" ]]; then
    eval "$RUNTIME_DRIFT_CHECK_CMD"
    return $?
  fi
  if [[ -f "$DEPLOY_DIR/verify-runtime-drift.sh" ]]; then
    bash "$DEPLOY_DIR/verify-runtime-drift.sh" --state "$current_state"
    return $?
  fi
  return 0
}

release_is_committed() {
  local state_file="${CURRENT_RELEASE_FILE:-${DEPLOY_PATH:-$(cd -- "$DEPLOY_DIR/.." && pwd)}/state/current-release.json}"
  [[ -f "$state_file" ]] || return 1
  python3 - "$state_file" "${RELEASE_ID:-}" <<'PY' >/dev/null 2>&1
import json, sys
state = json.load(open(sys.argv[1], encoding="utf-8"))
raise SystemExit(0 if state.get("status") == "COMPLETED" and state.get("release_id") == sys.argv[2] else 1)
PY
}

rollback_completed_components() {
  local image rollback_sha
  rollback_sha="$(rollout_journal_git_sha)"
  if rollout_step_completed failover_controller; then
    EXPECTED_COMMIT="$rollback_sha" RELEASE_ORCHESTRATED=1 DEFER_RELEASE_STATE=1 FAILOVER_ROLLBACK_ONLY=1 \
      bash "$DEPLOY_DIR/deploy-failover-controller.sh" || return 1
  fi
  if rollout_step_completed worker; then
    image="$(get_release_env WORKER_IMAGE_REF)" || return 1
    RELEASE_ORCHESTRATED=1 DEFER_RELEASE_STATE=1 bash "$DEPLOY_DIR/deploy-worker.sh" "$image" || return 1
  fi
  if rollout_step_completed bark; then
    image="$(get_release_env BARK_IMAGE_REF)" || return 1
    RELEASE_ORCHESTRATED=1 DEFER_RELEASE_STATE=1 bash "$DEPLOY_DIR/deploy-bark.sh" "$image" || return 1
  fi
  if rollout_step_completed tts; then
    image="$(get_release_env TTS_IMAGE_REF)" || return 1
    RELEASE_ORCHESTRATED=1 DEFER_RELEASE_STATE=1 bash "$DEPLOY_DIR/deploy-tts.sh" "$image" || return 1
  fi
  if rollout_step_completed auth_browser; then
    image="$(get_release_env BROWSER_IMAGE_REF)" || return 1
    RELEASE_ORCHESTRATED=1 DEFER_RELEASE_STATE=1 bash "$DEPLOY_DIR/deploy-auth-browser.sh" "$image" || return 1
  fi
}

cleanup_rollout() {
  local code=$?
  if [[ "$code" -ne 0 && "$ROLLOUT_EXIT_CODE" -eq 0 ]]; then
    ROLLOUT_EXIT_CODE="$code"
  fi
  if [[ "$ROLLOUT_EXIT_CODE" -ne 0 ]]; then
    log_warn "Rollout terminated with code ${ROLLOUT_EXIT_CODE}."
    local final_status="INTERRUPTED"
    if ! release_is_committed; then
      local rollback_failures=0
      rollback_gateway_route || rollback_failures=$((rollback_failures + 1))
      rollback_frontend_route || rollback_failures=$((rollback_failures + 1))
      rollback_completed_components || rollback_failures=$((rollback_failures + 1))
      verify_previous_runtime || rollback_failures=$((rollback_failures + 1))
      if (( rollback_failures == 0 )); then
        final_status="ROLLED_BACK"
      else
        log_error "Release rollback did not fully verify ($rollback_failures failure(s)); preserving evidence and blocking subsequent mutation."
        ROLLOUT_EXIT_CODE=1
      fi
    fi
    if [[ -f "${ROLLOUT_JOURNAL_FILE:-}" ]]; then
      finish_rollout_journal "$final_status" ""
    fi
  fi
  release_deploy_lock
  exit "$ROLLOUT_EXIT_CODE"
}

on_signal() {
  local sig="$1"
  log_warn "Caught signal SIG${sig}, aborting rollout orchestration..."
  ROLLOUT_EXIT_CODE=$(( 128 + sig ))
  exit "$ROLLOUT_EXIT_CODE"
}

trap 'on_signal 1' HUP
trap 'on_signal 2' INT
trap 'on_signal 15' TERM
trap cleanup_rollout EXIT

# 1. Acquire explicit remote release lock
acquire_deploy_lock

# 2. Inspect previous rollout state. Recovery runs only after new release preflights.
PREVIOUS_ROLLOUT_STATUS=""
if [[ -f "$ROLLOUT_JOURNAL_FILE" ]]; then
  PREVIOUS_ROLLOUT_STATUS="$(python3 - "$ROLLOUT_JOURNAL_FILE" <<'PY' 2>/dev/null || true
import json, sys
with open(sys.argv[1], encoding='utf-8') as handle:
    print(json.load(handle).get('status', ''))
PY
)"
  if [[ -z "$PREVIOUS_ROLLOUT_STATUS" ]]; then
    log_error "A previous rollout journal is unreadable; preserving it and blocking new mutation."
    exit 1
  fi
  if [[ "$PREVIOUS_ROLLOUT_STATUS" == "RUNNING" ]]; then
    log_warn "Found a RUNNING journal after acquiring the exclusive release lock; treating it as an interrupted process."
    finish_rollout_journal "INTERRUPTED" ""
    PREVIOUS_ROLLOUT_STATUS="INTERRUPTED"
  fi
fi

# 3. Verify Manifest & Promotion Scope
PROMOTION_FRONTEND="false"
PROMOTION_GATEWAY="false"
PROMOTION_WORKER="false"
PROMOTION_SCHEMA="false"
PROMOTION_AUTH_BROWSER="false"
PROMOTION_TTS="false"
PROMOTION_BARK="false"
PROMOTION_FAILOVER_CONTROLLER="false"
PROMOTION_PLATFORM="false"
PROMOTION_DOC_ONLY="false"
PROMOTION_SCOPE=""
GIT_SHA=""
RELEASE_ID=""

if [[ "$SKIP_MANIFEST_CHECK" -eq 1 ]]; then
  log_info "Bypassing manifest verification (--skip-manifest-check enabled)..."
  if [[ -f "$MANIFEST_FILE" ]]; then
    GIT_SHA="$(grep -o '"git_sha":[[:space:]]*"[^"]*"' "$MANIFEST_FILE" 2>/dev/null | head -n1 | cut -d'"' -f4 || true)"
    RELEASE_ID="$(grep -o '"release_id":[[:space:]]*"[^"]*"' "$MANIFEST_FILE" 2>/dev/null | head -n1 | cut -d'"' -f4 || true)"

    grep -q '"frontend":[[:space:]]*true' "$MANIFEST_FILE" 2>/dev/null && PROMOTION_FRONTEND="true"
    grep -q '"gateway":[[:space:]]*true' "$MANIFEST_FILE" 2>/dev/null && PROMOTION_GATEWAY="true"
    grep -q '"worker":[[:space:]]*true' "$MANIFEST_FILE" 2>/dev/null && PROMOTION_WORKER="true"
    grep -q '"schema":[[:space:]]*true' "$MANIFEST_FILE" 2>/dev/null && PROMOTION_SCHEMA="true"
    grep -q '"auth_browser":[[:space:]]*true' "$MANIFEST_FILE" 2>/dev/null && PROMOTION_AUTH_BROWSER="true"
    grep -q '"tts":[[:space:]]*true' "$MANIFEST_FILE" 2>/dev/null && PROMOTION_TTS="true"
    grep -q '"bark":[[:space:]]*true' "$MANIFEST_FILE" 2>/dev/null && PROMOTION_BARK="true"
    grep -q '"failover_controller":[[:space:]]*true' "$MANIFEST_FILE" 2>/dev/null && PROMOTION_FAILOVER_CONTROLLER="true"
    grep -q '"platform":[[:space:]]*true' "$MANIFEST_FILE" 2>/dev/null && PROMOTION_PLATFORM="true"
    grep -q '"promotion_doc_only":[[:space:]]*true' "$MANIFEST_FILE" 2>/dev/null && PROMOTION_DOC_ONLY="true"

    IMAGE_FRONTEND="$(grep -o '"frontend":[[:space:]]*"[^"]*"' "$MANIFEST_FILE" 2>/dev/null | head -n1 | cut -d'"' -f4 || true)"
    IMAGE_GATEWAY="$(grep -o '"gateway":[[:space:]]*"[^"]*"' "$MANIFEST_FILE" 2>/dev/null | head -n1 | cut -d'"' -f4 || true)"
    IMAGE_WORKER="$(grep -o '"worker":[[:space:]]*"[^"]*"' "$MANIFEST_FILE" 2>/dev/null | head -n1 | cut -d'"' -f4 || true)"
    IMAGE_DBTOOL="$(grep -o '"dbtool":[[:space:]]*"[^"]*"' "$MANIFEST_FILE" 2>/dev/null | head -n1 | cut -d'"' -f4 || true)"
    IMAGE_AUTH_BROWSER="$(grep -o '"auth_browser":[[:space:]]*"[^"]*"' "$MANIFEST_FILE" 2>/dev/null | head -n1 | cut -d'"' -f4 || true)"
    IMAGE_TTS_GATEWAY="$(grep -o '"tts":[[:space:]]*"[^"]*"' "$MANIFEST_FILE" 2>/dev/null | head -n1 | cut -d'"' -f4 || true)"
    if [[ -z "$IMAGE_TTS_GATEWAY" ]]; then
      IMAGE_TTS_GATEWAY="$(grep -o '"tts_gateway":[[:space:]]*"[^"]*"' "$MANIFEST_FILE" 2>/dev/null | head -n1 | cut -d'"' -f4 || true)"
    fi
    IMAGE_BARK="$(grep -o '"bark":[[:space:]]*"[^"]*"' "$MANIFEST_FILE" 2>/dev/null | head -n1 | cut -d'"' -f4 || true)"
  elif [[ -n "$REQUESTED_SCOPE" ]]; then
    IFS=',' read -ra scopes <<< "$REQUESTED_SCOPE"
    for sc in "${scopes[@]}"; do
      case "$sc" in
        frontend) PROMOTION_FRONTEND="true" ;;
        gateway) PROMOTION_GATEWAY="true" ;;
        worker) PROMOTION_WORKER="true" ;;
        schema) PROMOTION_SCHEMA="true" ;;
        auth_browser|auth-browser) PROMOTION_AUTH_BROWSER="true" ;;
        tts|tts_gateway|tts-gateway) PROMOTION_TTS="true" ;;
        bark) PROMOTION_BARK="true" ;;
        failover_controller) PROMOTION_FAILOVER_CONTROLLER="true" ;;
        platform) PROMOTION_PLATFORM="true" ;;
        doc_only|doc-only) PROMOTION_DOC_ONLY="true" ;;
      esac
    done
  fi

  [[ -z "$GIT_SHA" ]] && GIT_SHA="${GITHUB_SHA:-$(git rev-parse HEAD 2>/dev/null || echo "0000000000000000000000000000000000000000")}"
  [[ -z "$RELEASE_ID" ]] && RELEASE_ID="rel-${GIT_SHA:0:12}-$(date +%s)"
else
  [[ -f "$MANIFEST_FILE" ]] || { log_error "Missing release manifest: $MANIFEST_FILE"; exit 1; }
  verified_env="$(mktemp)"

  verify_args=(
    --manifest "$MANIFEST_FILE"
    --deploy-dir "$DEPLOY_DIR"
    --output-env "$verified_env"
  )
  [[ -f "$BUNDLE_FILE" ]] && verify_args+=(--bundle "$BUNDLE_FILE")
  [[ -n "$EXPECTED_IDENTITY" ]] && verify_args+=(--expected-identity "$EXPECTED_IDENTITY")
  [[ -n "$EXPECTED_ISSUER" ]] && verify_args+=(--expected-issuer "$EXPECTED_ISSUER")
  [[ "$REQUIRE_COSIGN" -eq 1 ]] && verify_args+=(--require-cosign)
  [[ "$ALLOW_REDEPLOY" -eq 1 ]] && verify_args+=(--allow-redeploy)
  [[ "$MAX_AGE_SECONDS" -gt 0 ]] && verify_args+=(--max-age-seconds "$MAX_AGE_SECONDS")

  if [[ -f "$LAST_RELEASE_FILE" ]]; then
    cur_commit="$(grep -o '"git_sha":[[:space:]]*"[^"]*"' "$LAST_RELEASE_FILE" 2>/dev/null | head -n1 | cut -d'"' -f4 || true)"
    cur_time="$(grep -o '"deployed_at":[[:space:]]*"[^"]*"' "$LAST_RELEASE_FILE" 2>/dev/null | head -n1 | cut -d'"' -f4 || true)"
    [[ -n "$cur_commit" ]] && verify_args+=(--current-deployed-commit "$cur_commit")
    [[ -n "$cur_time" ]] && verify_args+=(--current-deployed-time "$cur_time")
  fi

  log_info "Verifying release manifest signature, immutability, and anti-replay..."
  if ! bash "$DEPLOY_DIR/verify-manifest.sh" "${verify_args[@]}"; then
    log_error "Release manifest verification failed. Aborting rollout."
    rm -f "$verified_env"
    exit 1
  fi

  # shellcheck source=/dev/null
  source "$verified_env"
  rm -f "$verified_env"

  for promotion_var in PROMOTION_FRONTEND PROMOTION_GATEWAY PROMOTION_WORKER PROMOTION_SCHEMA PROMOTION_AUTH_BROWSER PROMOTION_TTS PROMOTION_BARK PROMOTION_FAILOVER_CONTROLLER PROMOTION_PLATFORM PROMOTION_DOC_ONLY; do
    promotion_value="${!promotion_var:-}"
    [[ "$promotion_value" == "true" || "$promotion_value" == "false" ]] || {
      log_error "Invalid verified promotion flag [$promotion_var=$promotion_value]."
      exit 1
    }
  done

  IMAGE_FRONTEND="${IMAGE_FRONTEND:-${FRONTEND_IMAGE_REF:-}}"
  IMAGE_GATEWAY="${IMAGE_GATEWAY:-${IMAGE_GATEWAY:-}}"
  IMAGE_WORKER="${IMAGE_WORKER:-${IMAGE_WORKER:-}}"
  IMAGE_DBTOOL="${IMAGE_DBTOOL:-${IMAGE_DBTOOL:-}}"
  IMAGE_AUTH_BROWSER="${IMAGE_AUTH_BROWSER:-${IMAGE_AUTH_BROWSER:-}}"
  IMAGE_TTS_GATEWAY="${IMAGE_TTS_GATEWAY:-${IMAGE_TTS_GATEWAY:-}}"
  IMAGE_BARK="${IMAGE_BARK:-${IMAGE_BARK:-}}"
fi

# Compose validates the entire production model even for `up --no-deps <service>`.
# Export every verified immutable image before any component transaction runs.
export FRONTEND_IMAGE_REF="$IMAGE_FRONTEND"
export DBTOOL_IMAGE_REF="$IMAGE_DBTOOL"
export WORKER_IMAGE_REF="$IMAGE_WORKER"
export BROWSER_IMAGE_REF="$IMAGE_AUTH_BROWSER"
export TTS_IMAGE_REF="$IMAGE_TTS_GATEWAY"
export BARK_IMAGE_REF="$IMAGE_BARK"
export IMAGE_REF_BLUE="$IMAGE_GATEWAY"
export IMAGE_REF_GREEN="$IMAGE_GATEWAY"

# Validate caller-requested scope against manifest authorization
if [[ -n "$REQUESTED_SCOPE" ]]; then
  IFS=',' read -ra req_scopes <<< "$REQUESTED_SCOPE"
  for sc in "${req_scopes[@]}"; do
    case "$sc" in
      frontend)
        if [[ "$PROMOTION_FRONTEND" != "true" ]]; then
          log_error "Unauthorized promotion request: component [frontend] is NOT authorized by signed manifest."
          exit 1
        fi
        ;;
      gateway)
        if [[ "$PROMOTION_GATEWAY" != "true" ]]; then
          log_error "Unauthorized promotion request: component [gateway] is NOT authorized by signed manifest."
          exit 1
        fi
        ;;
      worker)
        if [[ "$PROMOTION_WORKER" != "true" ]]; then
          log_error "Unauthorized promotion request: component [worker] is NOT authorized by signed manifest."
          exit 1
        fi
        ;;
      schema)
        if [[ "$PROMOTION_SCHEMA" != "true" ]]; then
          log_error "Unauthorized promotion request: component [schema] is NOT authorized by signed manifest."
          exit 1
        fi
        ;;
      auth_browser|auth-browser)
        if [[ "$PROMOTION_AUTH_BROWSER" != "true" ]]; then
          log_error "Unauthorized promotion request: component [auth-browser] is NOT authorized by signed manifest."
          exit 1
        fi
        ;;
      tts|tts_gateway|tts-gateway)
        if [[ "$PROMOTION_TTS" != "true" ]]; then
          log_error "Unauthorized promotion request: component [tts] is NOT authorized by signed manifest."
          exit 1
        fi
        ;;
      bark)
        if [[ "$PROMOTION_BARK" != "true" ]]; then
          log_error "Unauthorized promotion request: component [bark] is NOT authorized by signed manifest."
          exit 1
        fi
        ;;
      failover_controller)
        if [[ "$PROMOTION_FAILOVER_CONTROLLER" != "true" ]]; then
          log_error "Unauthorized promotion request: component [failover_controller] is NOT authorized by signed manifest."
          exit 1
        fi
        ;;
      platform)
        if [[ "$PROMOTION_PLATFORM" != "true" ]]; then
          log_error "Unauthorized promotion request: component [platform] is NOT authorized by signed manifest."
          exit 1
        fi
        ;;
    esac
  done

  # Narrow promotion to only requested components
  PROMOTION_FRONTEND="false"
  PROMOTION_GATEWAY="false"
  PROMOTION_WORKER="false"
  PROMOTION_SCHEMA="false"
  PROMOTION_AUTH_BROWSER="false"
  PROMOTION_TTS="false"
  PROMOTION_BARK="false"
  PROMOTION_FAILOVER_CONTROLLER="false"
  PROMOTION_PLATFORM="false"
  for sc in "${req_scopes[@]}"; do
    case "$sc" in
      frontend) PROMOTION_FRONTEND="true" ;;
      gateway) PROMOTION_GATEWAY="true" ;;
      worker) PROMOTION_WORKER="true" ;;
      schema) PROMOTION_SCHEMA="true" ;;
      auth_browser|auth-browser) PROMOTION_AUTH_BROWSER="true" ;;
      tts|tts_gateway|tts-gateway) PROMOTION_TTS="true" ;;
      bark) PROMOTION_BARK="true" ;;
      failover_controller) PROMOTION_FAILOVER_CONTROLLER="true" ;;
      platform) PROMOTION_PLATFORM="true" ;;
    esac
  done
fi

# 4. Docs-Only Release: Promote zero runtime containers/migrations
is_docs_only=0
if [[ "${PROMOTION_DOC_ONLY:-false}" == "true" ]] || \
   ([[ "${PROMOTION_FRONTEND:-false}" != "true" ]] && \
    [[ "${PROMOTION_GATEWAY:-false}" != "true" ]] && \
    [[ "${PROMOTION_WORKER:-false}" != "true" ]] && \
    [[ "${PROMOTION_SCHEMA:-false}" != "true" ]] && \
    [[ "${PROMOTION_AUTH_BROWSER:-false}" != "true" ]] && \
    [[ "${PROMOTION_TTS:-false}" != "true" ]] && \
    [[ "${PROMOTION_BARK:-false}" != "true" ]] && \
    [[ "${PROMOTION_FAILOVER_CONTROLLER:-false}" != "true" ]] && \
    [[ "${PROMOTION_PLATFORM:-false}" != "true" ]]); then
  is_docs_only=1
fi

evidence_dir="$RELEASES_DIR/${RELEASE_ID:-rel-doc-$GIT_SHA}"
mkdir -p "$evidence_dir"
now="$(date -u +'%Y-%m-%dT%H:%M:%SZ')"

if [[ "$is_docs_only" -eq 1 ]]; then
  log_info "=========================================================="
  log_info "Documentation-Only Release: Git SHA [${GIT_SHA}]"
  log_info "Zero runtime containers, migrations, or routes authorized."
  log_info "=========================================================="

  atomic_write_file "$evidence_dir/receipt.json" 600 <<EOF
{
  "release_id": "${RELEASE_ID:-rel-doc-$GIT_SHA}",
  "git_sha": "${GIT_SHA}",
  "status": "DOC_ONLY",
  "deployed_at": "${now}",
  "promoted_components": [],
  "message": "Documentation-only release successfully acknowledged without runtime changes."
}
EOF

  if [[ -f "${CURRENT_RELEASE_FILE:-}" ]]; then
    tmp_doc_state="$(mktemp "$(dirname "$CURRENT_RELEASE_FILE")/.doc-state.XXXXXX")"
    python3 "$DEPLOY_DIR/release-state.py" advance-doc-only \
      --previous "$CURRENT_RELEASE_FILE" \
      --release-dir "$DEPLOY_DIR" \
      --manifest "$MANIFEST_FILE" \
      --output "$tmp_doc_state"
    atomic_write_file "$CURRENT_RELEASE_FILE" 600 < "$tmp_doc_state"
    rm -f "$tmp_doc_state"
  fi
  atomic_write_file "$LAST_RELEASE_FILE" 600 <<EOF
{
  "release_id": "${RELEASE_ID:-rel-doc-$GIT_SHA}",
  "git_sha": "${GIT_SHA}",
  "status": "SUCCESS",
  "doc_only": true,
  "deployed_at": "${now}",
  "promoted_components": []
}
EOF

  init_rollout_journal "${RELEASE_ID:-rel-doc-$GIT_SHA}" "$GIT_SHA" "doc-only"
  finish_rollout_journal "DOC_ONLY" "$evidence_dir"
  log_info "Documentation-only release receipt recorded: $evidence_dir/receipt.json"
  exit 0
fi

# 5. Dependency-Ordered Rollout Execution
active_scope_list=()
[[ "${PROMOTION_FRONTEND:-false}" == "true" ]] && active_scope_list+=("frontend")
[[ "${PROMOTION_SCHEMA:-false}" == "true" ]] && active_scope_list+=("schema")
[[ "${PROMOTION_AUTH_BROWSER:-false}" == "true" ]] && active_scope_list+=("auth_browser")
[[ "${PROMOTION_TTS:-false}" == "true" ]] && active_scope_list+=("tts")
[[ "${PROMOTION_BARK:-false}" == "true" ]] && active_scope_list+=("bark")
[[ "${PROMOTION_WORKER:-false}" == "true" ]] && active_scope_list+=("worker")
[[ "${PROMOTION_GATEWAY:-false}" == "true" ]] && active_scope_list+=("gateway")
[[ "${PROMOTION_FAILOVER_CONTROLLER:-false}" == "true" ]] && active_scope_list+=("failover_controller")
[[ "${PROMOTION_PLATFORM:-false}" == "true" ]] && active_scope_list+=("platform")

scope_str="$(IFS=,; echo "${active_scope_list[*]}")"

log_info "=========================================================="
log_info "Starting Rollout Orchestration for [${RELEASE_ID}] (${GIT_SHA})"
log_info "Authorized Promotion Scope: [${scope_str}]"
log_info "=========================================================="

# Planned candidate state construction before any runtime mutation
planned_candidate_dir="${RUNTIME_STATE_DIR:-$(dirname "$CURRENT_RELEASE_FILE")}/candidate"
mkdir -p "$planned_candidate_dir"
planned_candidate_file="$planned_candidate_dir/${RELEASE_ID}.json"

planned_gw_slot="$(get_active_slot 2>/dev/null || cat "$ACTIVE_SLOT_FILE" 2>/dev/null || printf 'blue')"
if [[ "${PROMOTION_GATEWAY:-false}" == "true" ]]; then
  case "$planned_gw_slot" in
    blue) planned_gw_slot="green" ;;
    green) planned_gw_slot="blue" ;;
    *) planned_gw_slot="blue" ;;
  esac
fi
planned_fe_slot="$(cat "$FRONTEND_ACTIVE_SLOT_FILE" 2>/dev/null || printf 'legacy')"
if [[ "${PROMOTION_FRONTEND:-false}" == "true" ]]; then
  case "$planned_fe_slot" in
    blue) planned_fe_slot="green" ;;
    green) planned_fe_slot="blue" ;;
    *) planned_fe_slot="blue" ;;
  esac
fi

if [[ -f "$DEPLOY_DIR/release-state.py" ]]; then
  prev_arg=()
  if [[ -f "$CURRENT_RELEASE_FILE" ]]; then
    prev_arg=(--previous "$CURRENT_RELEASE_FILE")
  fi
  python3 "$DEPLOY_DIR/release-state.py" build \
    "${prev_arg[@]}" \
    --release-dir "$DEPLOY_DIR" \
    --manifest "$MANIFEST_FILE" \
    --gateway-slot "$planned_gw_slot" \
    --frontend-slot "$planned_fe_slot" \
    --scope "$scope_str" \
    --output "$planned_candidate_file"
  python3 "$DEPLOY_DIR/release-state.py" validate "$planned_candidate_file" --allow-candidate
  log_info "Planned candidate release state validated: $planned_candidate_file"
fi

if [[ "$PROMOTION_BARK" == "true" || "$PROMOTION_WORKER" == "true" ]]; then
  [[ -n "$IMAGE_BARK" ]] || { log_error "BARK image digest is required to verify shared Bark secrets."; exit 1; }
  validate_secrets
  prepare_bark_secret_permissions
  validate_secrets
  preflight_bark_secret_access "$IMAGE_BARK"
fi

# Recover only after all non-destructive release and secret preflights pass.
if [[ -f "${TX_JOURNAL_FILE:-}" ]]; then
  log_info "Startup recovery: inspecting prior component transaction journal..."
  recover_tx_journal
fi
if [[ "$PREVIOUS_ROLLOUT_STATUS" == "INTERRUPTED" || "$PREVIOUS_ROLLOUT_STATUS" == "ROLLED_BACK" ]]; then
  if [[ "$PREVIOUS_ROLLOUT_STATUS" == "INTERRUPTED" ]]; then
    r_failures=0
    rollback_gateway_route || r_failures=$((r_failures + 1))
    rollback_frontend_route || r_failures=$((r_failures + 1))
    rollback_completed_components || r_failures=$((r_failures + 1))
    if (( r_failures != 0 )); then
      log_error "Interrupted rollout rollback could not be verified; preserving its journal."
      exit 1
    fi
  fi
  current_state="${CURRENT_RELEASE_FILE:-${DEPLOY_PATH:-$(cd -- "$DEPLOY_DIR/.." && pwd)}/state/current-release.json}"
  if [[ ! -s "$current_state" ]]; then
    log_error "Interrupted rollout cannot be reconciled automatically before canonical state bootstrap."
    exit 1
  fi
  if [[ "$SKIP_MANIFEST_CHECK" -eq 1 && -n "${RUNTIME_DRIFT_CHECK_CMD:-}" ]]; then
    if ! eval "$RUNTIME_DRIFT_CHECK_CMD"; then
      log_error "Interrupted rollout test reconciliation failed."
      exit 1
    fi
  elif ! bash "$DEPLOY_DIR/verify-runtime-drift.sh" --state "$current_state"; then
    log_error "Interrupted rollout left runtime different from canonical state; preserving the journal and blocking new mutation."
    exit 1
  fi
  previous_rollout_archive="${ROLLOUT_JOURNAL_FILE}.reconciled.$(date +%s)"
  mv -f "$ROLLOUT_JOURNAL_FILE" "$previous_rollout_archive"
  log_info "Runtime matches canonical state; archived reconciled interrupted rollout journal: $previous_rollout_archive"
fi

init_rollout_journal "$RELEASE_ID" "$GIT_SHA" "$scope_str"

if [[ -f "$MANIFEST_FILE" ]]; then
  cp -f "$MANIFEST_FILE" "$evidence_dir/release-manifest.json" 2>/dev/null || true
fi
if [[ -f "$BUNDLE_FILE" ]]; then
  cp -f "$BUNDLE_FILE" "$evidence_dir/release-manifest.bundle" 2>/dev/null || true
fi

promoted_list=()

# Step 1: Schema Migration
if [[ "${PROMOTION_SCHEMA:-false}" == "true" ]]; then
  log_info "Executing Transaction 1: Database Schema Migration..."
  update_rollout_step "schema" "RUNNING"
  [[ -n "$IMAGE_DBTOOL" ]] || { log_error "DBTOOL image digest is required for schema promotion."; exit 1; }
  validate_digest "$IMAGE_DBTOOL" "dbtool"
  export DBTOOL_IMAGE_REF="$IMAGE_DBTOOL"
  RELEASE_ORCHESTRATED=1 DEFER_RELEASE_STATE=1 bash "$DEPLOY_DIR/deploy-schema.sh" "$IMAGE_DBTOOL"
  update_rollout_step "schema" "STEP_COMPLETED"
  promoted_list+=("schema")
  log_info "Transaction 1: Schema Migration completed."
fi

# Step 2: Auxiliary Sidecars
if [[ "${PROMOTION_AUTH_BROWSER:-false}" == "true" ]]; then
  log_info "Executing Transaction 2a: Sandboxed Auth Browser..."
  update_rollout_step "auth_browser" "RUNNING"
  [[ -n "$IMAGE_AUTH_BROWSER" ]] || { log_error "AUTH_BROWSER image digest is required for browser promotion."; exit 1; }
  validate_digest "$IMAGE_AUTH_BROWSER" "auth-browser"
  RELEASE_ORCHESTRATED=1 DEFER_RELEASE_STATE=1 bash "$DEPLOY_DIR/deploy-auth-browser.sh" "$IMAGE_AUTH_BROWSER"
  update_rollout_step "auth_browser" "STEP_COMPLETED"
  promoted_list+=("auth_browser")
  log_info "Transaction 2a: Auth Browser completed."
fi

if [[ "${PROMOTION_TTS:-false}" == "true" ]]; then
  log_info "Executing Transaction 2b: Auxiliary TTS Gateway..."
  update_rollout_step "tts" "RUNNING"
  [[ -n "$IMAGE_TTS_GATEWAY" ]] || { log_error "TTS image digest is required for TTS promotion."; exit 1; }
  validate_digest "$IMAGE_TTS_GATEWAY" "tts-gateway"
  RELEASE_ORCHESTRATED=1 DEFER_RELEASE_STATE=1 bash "$DEPLOY_DIR/deploy-tts.sh" "$IMAGE_TTS_GATEWAY"
  update_rollout_step "tts" "STEP_COMPLETED"
  promoted_list+=("tts")
  log_info "Transaction 2b: TTS Gateway completed."
fi

if [[ "${PROMOTION_BARK:-false}" == "true" ]]; then
  log_info "Executing Transaction 2c: Auxiliary Bark Service..."
  update_rollout_step "bark" "RUNNING"
  [[ -n "$IMAGE_BARK" ]] || { log_error "BARK image digest is required for Bark promotion."; exit 1; }
  validate_digest "$IMAGE_BARK" "bark"
  RELEASE_ORCHESTRATED=1 DEFER_RELEASE_STATE=1 bash "$DEPLOY_DIR/deploy-bark.sh" "$IMAGE_BARK"
  update_rollout_step "bark" "STEP_COMPLETED"
  promoted_list+=("bark")
  log_info "Transaction 2c: Bark Service completed."
fi

# Step 2d: Frontend-only replacement
if [[ "${PROMOTION_FRONTEND:-false}" == "true" ]]; then
  log_info "Executing Transaction 2d: isolated frontend deployment..."
  update_rollout_step "frontend" "RUNNING"
  [[ -n "$IMAGE_FRONTEND" ]] || { log_error "FRONTEND image digest is required for frontend promotion."; exit 1; }
  validate_digest "$IMAGE_FRONTEND" "frontend"
  RELEASE_ORCHESTRATED=1 DEFER_RELEASE_STATE=1 DEFER_OLD_SLOT_RETIREMENT=1 \
    PENDING_FRONTEND_RETIRE_FILE="$PENDING_FRONTEND_RETIRE_FILE" \
    bash "$DEPLOY_DIR/deploy-frontend.sh" "$IMAGE_FRONTEND"
  update_rollout_step "frontend" "STEP_COMPLETED"
  promoted_list+=("frontend")
  log_info "Transaction 2d: frontend completed without touching gateway or worker."
fi

# Step 3: Singleton Worker Upgrade
if [[ "${PROMOTION_WORKER:-false}" == "true" ]]; then
  log_info "Executing Transaction 3: Worker Singleton Upgrade..."
  update_rollout_step "worker" "RUNNING"
  [[ -n "$IMAGE_WORKER" ]] || { log_error "WORKER image digest is required for worker promotion."; exit 1; }
  validate_digest "$IMAGE_WORKER" "worker"
  RELEASE_ORCHESTRATED=1 DEFER_RELEASE_STATE=1 bash "$DEPLOY_DIR/deploy-worker.sh" "$IMAGE_WORKER"
  update_rollout_step "worker" "STEP_COMPLETED"
  promoted_list+=("worker")
  log_info "Transaction 3: Worker Singleton Upgrade completed."
fi

# Step 4: Gateway Blue/Green Deployment
if [[ "${PROMOTION_GATEWAY:-false}" == "true" ]]; then
  log_info "Executing Transaction 4: Gateway Blue/Green Cutover & Route ACK..."
  update_rollout_step "gateway" "RUNNING"
  [[ -n "$IMAGE_GATEWAY" ]] || { log_error "GATEWAY image digest is required for gateway promotion."; exit 1; }
  validate_digest "$IMAGE_GATEWAY" "gateway"
  gw_args=("$IMAGE_GATEWAY")
  [[ -n "$GIT_SHA" ]] && gw_args+=(--expected-commit "$GIT_SHA")
  gw_args+=(--defer-soak)
  if [[ "$DETACH_SOAK" -eq 1 ]]; then
    log_error "Detached gateway soak is not permitted in a release transaction; success must wait for soak completion."
    exit 1
  fi
  [[ "$SKIP_MANIFEST_CHECK" -eq 1 ]] && gw_args+=(--skip-manifest-check)
  RELEASE_ORCHESTRATED=1 DEFER_RELEASE_STATE=1 DEFER_OLD_SLOT_RETIREMENT=1 \
    PENDING_GATEWAY_RETIRE_FILE="$PENDING_GATEWAY_RETIRE_FILE" \
    bash "$DEPLOY_DIR/deploy-gateway.sh" "${gw_args[@]}"
  update_rollout_step "gateway" "STEP_COMPLETED"
  promoted_list+=("gateway")
  log_info "Transaction 4: Gateway Blue/Green Promotion completed."
fi

# Step 5: Release-managed failover controller
if [[ "${PROMOTION_FAILOVER_CONTROLLER:-false}" == "true" ]]; then
  log_info "Executing Transaction 5: Failover Controller Installation..."
  update_rollout_step "failover_controller" "RUNNING"
  EXPECTED_COMMIT="$GIT_SHA" RELEASE_ORCHESTRATED=1 DEFER_RELEASE_STATE=1 \
    bash "$DEPLOY_DIR/deploy-failover-controller.sh"
  update_rollout_step "failover_controller" "STEP_COMPLETED"
  promoted_list+=("failover_controller")
  log_info "Transaction 5: Failover Controller completed."
fi

# Step 6: Platform / Edge Config (if platform only or platform scoped)
if [[ "${PROMOTION_PLATFORM:-false}" == "true" && "${PROMOTION_GATEWAY:-false}" != "true" ]]; then
  log_info "Executing Transaction 5: Platform / Edge Config Reload..."
  update_rollout_step "platform" "RUNNING"
  active_slot="$(get_active_slot)"
  if ! bash "$DEPLOY_DIR/switch-slot.sh" "$active_slot"; then
    log_error "Transaction 5: Platform Config Reload failed."
    update_rollout_step "platform" "STEP_FAILED"
    exit 1
  fi
  update_rollout_step "platform" "STEP_COMPLETED"
  promoted_list+=("platform")
  log_info "Transaction 5: Platform Config Reload completed."
fi

# 6. Post-Promotion: Update Last Release State and Retain Evidence
promoted_json=""
if [[ ${#promoted_list[@]} -gt 0 ]]; then
  promoted_json="$(printf '    "%s"\n' "${promoted_list[@]}" | paste -sd, -)"
fi

# Commit the complete release environment once after every component and soak succeeded.
if [[ -f "$DEPLOY_DIR/release-env.sh" ]]; then
  # shellcheck source=deploy/release-env.sh
  source "$DEPLOY_DIR/release-env.sh"
  canonical_release_env="$RELEASE_ENV_FILE"
  staged_release_env="$(mktemp "$(dirname "$canonical_release_env")/.release-env.commit.XXXXXX")"
  if [[ -f "$canonical_release_env" ]]; then
    cp "$canonical_release_env" "$staged_release_env"
  fi
  if [[ -f "$CURRENT_RELEASE_FILE" ]]; then
    for release_key in "${REQUIRED_RELEASE_KEYS[@]}" FRONTEND_IMAGE_REF RELEASE_COMMIT; do
      release_value="$(get_release_env "$release_key")" || exit 1
      if [[ -n "$release_value" ]]; then
        RELEASE_ENV_FILE="$staged_release_env" USE_CANONICAL_RELEASE_STATE=0 set_release_env "$release_key" "$release_value"
      fi
    done
  fi
  export USE_CANONICAL_RELEASE_STATE=0
  export RELEASE_ENV_FILE="$staged_release_env"
  if [[ -n "${IMAGE_FRONTEND:-}" && "${PROMOTION_FRONTEND:-false}" == "true" ]]; then
    set_release_env "FRONTEND_IMAGE_REF" "$IMAGE_FRONTEND"
  fi
  if [[ -n "${IMAGE_GATEWAY:-}" && "${PROMOTION_GATEWAY:-false}" == "true" ]]; then
    active_gateway_slot="$(get_active_slot 2>/dev/null || true)"
    case "$active_gateway_slot" in
      blue) set_release_env "IMAGE_REF_BLUE" "$IMAGE_GATEWAY" ;;
      green) set_release_env "IMAGE_REF_GREEN" "$IMAGE_GATEWAY" ;;
      *) log_error "Cannot persist gateway image: active slot is unknown after promotion."; exit 1 ;;
    esac
  fi
  if [[ -n "${IMAGE_WORKER:-}" && "${PROMOTION_WORKER:-false}" == "true" ]]; then
    set_release_env "WORKER_IMAGE_REF" "$IMAGE_WORKER"
  fi
  if [[ -n "${IMAGE_DBTOOL:-}" && "${PROMOTION_SCHEMA:-false}" == "true" ]]; then
    set_release_env "DBTOOL_IMAGE_REF" "$IMAGE_DBTOOL"
  fi
  if [[ -n "${IMAGE_AUTH_BROWSER:-}" && "${PROMOTION_AUTH_BROWSER:-false}" == "true" ]]; then
    set_release_env "BROWSER_IMAGE_REF" "$IMAGE_AUTH_BROWSER"
  fi
  if [[ -n "${IMAGE_TTS_GATEWAY:-}" && "${PROMOTION_TTS:-false}" == "true" ]]; then
    set_release_env "TTS_IMAGE_REF" "$IMAGE_TTS_GATEWAY"
  fi
  if [[ -n "${IMAGE_BARK:-}" && "${PROMOTION_BARK:-false}" == "true" ]]; then
    set_release_env "BARK_IMAGE_REF" "$IMAGE_BARK"
  fi
  set_release_env "RELEASE_COMMIT" "$GIT_SHA"
  export RELEASE_ENV_FILE="$canonical_release_env"
fi

# Commit one canonical release document after every component and soak succeeds.
# Legacy files below are projections only and can be rebuilt from this document.
update_rollout_step "release_commit" "COMMITTING"
CURRENT_RELEASE_FILE="${CURRENT_RELEASE_FILE:-${DEPLOY_PATH:-$(cd -- "$DEPLOY_DIR/.." && pwd)}/state/current-release.json}"
manifest_digest="$(sha256sum "$MANIFEST_FILE" | awk '{print $1}')"
active_gateway_slot="$(get_active_slot 2>/dev/null || true)"
active_frontend_slot=""
if [[ -f "${FRONTEND_ACTIVE_SLOT_FILE:-}" ]]; then
  active_frontend_slot="$(tr -d '[:space:]' < "$FRONTEND_ACTIVE_SLOT_FILE")"
fi
if [[ -z "$active_frontend_slot" ]]; then
  active_frontend_slot="$(resolve_frontend_slot_strict 2>/dev/null || true)"
fi
if [[ -z "$active_frontend_slot" && -f "$CURRENT_RELEASE_FILE" ]]; then
  active_frontend_slot="$(python3 - "$CURRENT_RELEASE_FILE" <<'PY' 2>/dev/null || true
import json, sys
try:
    d = json.load(open(sys.argv[1]))
    print(d.get("active_slots", {}).get("frontend") or "legacy")
except Exception:
    pass
PY
)"
fi
[[ -z "$active_frontend_slot" ]] && active_frontend_slot="legacy"
case "$active_gateway_slot" in blue|green) ;; *) log_error "Cannot commit release: active gateway slot is unknown."; exit 1 ;; esac
case "$active_frontend_slot" in blue|green|legacy) ;; *) log_error "Cannot commit release: active frontend slot is invalid ($active_frontend_slot)."; exit 1 ;; esac
mkdir -p "$(dirname "$CURRENT_RELEASE_FILE")"
controller_candidate="$DEPLOY_DIR/failover/vps-failover-controller.py"
controller_digest=""
controller_bundle_digest=""
if [[ -f "$controller_candidate" ]]; then
  controller_digest="$(sha256sum "$controller_candidate" | awk '{print $1}')"
  controller_bundle_digest="$(python3 - "$(dirname "$controller_candidate")" <<'PY'
import hashlib, pathlib, sys
root = pathlib.Path(sys.argv[1])
paths = [
    pathlib.Path("vps-failover-controller.py"),
    pathlib.Path("vps-failover-controller.service"),
    pathlib.Path("vps-failover-reconcile.service"),
    pathlib.Path("vps-failover-reconcile.timer"),
    pathlib.Path("apps.d/acb.json"),
    pathlib.Path("apps.d/auth-browser.json"),
    pathlib.Path("apps.d/worker.json"),
]
h = hashlib.sha256()
for rel in paths:
    h.update(str(rel).encode() + b"\0" + (root / rel).read_bytes() + b"\0")
print(h.hexdigest())
PY
)"
fi
candidate_release_state="$(mktemp "$(dirname "$CURRENT_RELEASE_FILE")/.current-release.candidate.XXXXXX")"
if [[ -f "$DEPLOY_DIR/release-state.py" ]]; then
  prev_arg=()
  if [[ -f "$CURRENT_RELEASE_FILE" ]]; then
    prev_arg=(--previous "$CURRENT_RELEASE_FILE")
  fi
  python3 "$DEPLOY_DIR/release-state.py" build \
    "${prev_arg[@]}" \
    --release-dir "$DEPLOY_DIR" \
    --manifest "$MANIFEST_FILE" \
    --gateway-slot "$active_gateway_slot" \
    ${active_frontend_slot:+--frontend-slot "$active_frontend_slot"} \
    --scope "$scope_str" \
    --output "$candidate_release_state"
  python3 "$DEPLOY_DIR/release-state.py" validate "$candidate_release_state"
else
  R_STATE_PREVIOUS="$CURRENT_RELEASE_FILE" R_STATE_ENV="$staged_release_env" R_STATE_RELEASE_ID="$RELEASE_ID" \
  R_STATE_SHA="$GIT_SHA" R_STATE_MANIFEST="$manifest_digest" R_STATE_NOW="$now" \
  R_STATE_GATEWAY_SLOT="$active_gateway_slot" R_STATE_FRONTEND_SLOT="$active_frontend_slot" \
  R_STATE_CONTROLLER_HASH="$controller_digest" R_STATE_CONTROLLER_BUNDLE_HASH="$controller_bundle_digest" \
  python3 - <<'PY_STATE' > "$candidate_release_state"
import json, os, re

def read_env(path):
    values = {}
    with open(path, encoding="utf-8") as handle:
        for raw in handle:
            line = raw.strip()
            if not line or line.startswith("#") or "=" not in line:
                continue
            key, value = line.split("=", 1)
            values[key.strip()] = value.strip()
    return values

previous_path = os.environ["R_STATE_PREVIOUS"]
generation = 1
previous_release_id = None
if os.path.isfile(previous_path):
    with open(previous_path, encoding="utf-8") as handle:
        previous = json.load(handle)
    if previous.get("schema_version") not in (1, 2) or previous.get("status") != "COMPLETED":
        raise SystemExit("existing canonical release state is invalid")
    generation = int(previous.get("generation", 0)) + 1
    previous_release_id = previous.get("release_id")

env = read_env(os.environ["R_STATE_ENV"])
required = ["IMAGE_REF_BLUE", "IMAGE_REF_GREEN", "FRONTEND_IMAGE_REF", "WORKER_IMAGE_REF", "DBTOOL_IMAGE_REF", "BROWSER_IMAGE_REF", "TTS_IMAGE_REF", "BARK_IMAGE_REF"]
digest = re.compile(r"^[^\s]+@sha256:[a-f0-9]{64}$")
missing = [key for key in required if not digest.match(env.get(key, ""))]
if missing:
    raise SystemExit("invalid canonical image refs: " + ", ".join(missing))
state = {
    "schema_version": 2,
    "generation": generation,
    "release_id": os.environ["R_STATE_RELEASE_ID"],
    "previous_release_id": previous_release_id,
    "git_sha": os.environ["R_STATE_SHA"],
    "manifest_sha256": os.environ["R_STATE_MANIFEST"],
    "status": "COMPLETED",
    "committed_at": os.environ["R_STATE_NOW"],
    "active_slots": {
        "gateway": os.environ["R_STATE_GATEWAY_SLOT"],
        "frontend": os.environ["R_STATE_FRONTEND_SLOT"] or None,
    },
    "failover_controller": {
        "sha256": os.environ.get("R_STATE_CONTROLLER_HASH") or None,
        "bundle_sha256": os.environ.get("R_STATE_CONTROLLER_BUNDLE_HASH") or None,
    },
    "images": {
        "gateway": {"blue": env["IMAGE_REF_BLUE"], "green": env["IMAGE_REF_GREEN"]},
        "frontend": env["FRONTEND_IMAGE_REF"],
        "worker": env["WORKER_IMAGE_REF"],
        "dbtool": env["DBTOOL_IMAGE_REF"],
        "auth_browser": env["BROWSER_IMAGE_REF"],
        "tts": env["TTS_IMAGE_REF"],
        "bark": env["BARK_IMAGE_REF"],
    },
}
print(json.dumps(state, indent=2, sort_keys=True))
PY_STATE
fi
python3 -m json.tool "$candidate_release_state" >/dev/null

log_info "Executing Pre-Soak Runtime Contract Verification..."
if [[ "$SKIP_MANIFEST_CHECK" -eq 1 && -n "${RUNTIME_DRIFT_CHECK_CMD:-}" ]]; then
  eval "$RUNTIME_DRIFT_CHECK_CMD"
else
  CURRENT_RELEASE_FILE="$candidate_release_state" USE_CANONICAL_RELEASE_STATE=1 \
    bash "$DEPLOY_DIR/verify-runtime-drift.sh" --state "$candidate_release_state"
fi
log_info "Pre-soak runtime contract check PASSED."

if [[ "${PROMOTION_GATEWAY:-false}" == "true" && "${SOAK_SECONDS:-0}" -gt 0 ]]; then
  log_info "Beginning release soak observation (${SOAK_SECONDS}s)..."
  old_gw_slot="$(sed -n 's/^old_slot=//p' "$PENDING_GATEWAY_RETIRE_FILE" 2>/dev/null || true)"
  if [[ -z "$old_gw_slot" ]]; then
    case "$active_gateway_slot" in
      blue) old_gw_slot="green" ;;
      green) old_gw_slot="blue" ;;
    esac
  fi
  soak_rc=0
  if [[ -n "${RELEASE_SOAK_CMD:-}" ]]; then
    eval "$RELEASE_SOAK_CMD" || soak_rc=$?
  else
    run_resumable_soak "$active_gateway_slot" "$old_gw_slot" "$SOAK_SECONDS" || soak_rc=$?
  fi
  if [[ "$soak_rc" -ne 0 ]]; then
    log_error "Release soak observation failed! Aborting rollout."
    exit 1
  fi
  log_info "Release soak completed successfully."

  log_info "Executing Post-Soak Final Runtime Contract Verification..."
  if [[ "$SKIP_MANIFEST_CHECK" -eq 1 && -n "${RUNTIME_DRIFT_CHECK_CMD:-}" ]]; then
    eval "$RUNTIME_DRIFT_CHECK_CMD"
  else
    CURRENT_RELEASE_FILE="$candidate_release_state" USE_CANONICAL_RELEASE_STATE=1 \
      bash "$DEPLOY_DIR/verify-runtime-drift.sh" --state "$candidate_release_state"
  fi
  log_info "Post-soak final runtime contract check PASSED."
fi

atomic_write_file "$CURRENT_RELEASE_FILE" 600 < "$candidate_release_state"
rm -f "$candidate_release_state"
update_rollout_step "release_commit" "STEP_COMPLETED"

# The release is committed. Old blue/green slots may now retire; cleanup failure
# never rolls back an already committed healthy release.
if [[ -f "$PENDING_GATEWAY_RETIRE_FILE" ]]; then
  gateway_cleanup="${PENDING_GATEWAY_RETIRE_FILE}.cleanup"
  mv -f "$PENDING_GATEWAY_RETIRE_FILE" "$gateway_cleanup"
  old_slot="$(sed -n 's/^old_slot=//p' "$gateway_cleanup")"
  if stop_standby_container "$old_slot"; then
    rm -f "$gateway_cleanup"
  else
    log_warn "Committed release is healthy, but old gateway slot cleanup remains pending: $gateway_cleanup"
  fi
fi
if [[ -f "$PENDING_FRONTEND_RETIRE_FILE" ]]; then
  cleanup_pending_frontend "$PENDING_FRONTEND_RETIRE_FILE" || true
fi

# Compatibility projection for Compose and older operator tooling. It is not authoritative.
if [[ -f "$DEPLOY_DIR/release-state.py" ]]; then
  python3 "$DEPLOY_DIR/release-state.py" export-env "$CURRENT_RELEASE_FILE" --output "$canonical_release_env"
  chmod 600 "$canonical_release_env"
else
  if ! atomic_write_file "$canonical_release_env" 600 < "$staged_release_env"; then
    rm -f "$staged_release_env"
    log_error "Canonical release committed, but the legacy .release.env projection could not be refreshed."
    exit 1
  fi
fi
rm -f "$staged_release_env"
export USE_CANONICAL_RELEASE_STATE=1

atomic_write_file "$evidence_dir/receipt.json" 600 <<EOF
{
  "release_id": "${RELEASE_ID}",
  "git_sha": "${GIT_SHA}",
  "status": "SUCCESS",
  "deployed_at": "${now}",
  "promoted_components": [
${promoted_json}
  ]
}
EOF

# This is the sole successful-release commit point.
atomic_write_file "$LAST_RELEASE_FILE" 600 <<EOF
{
  "release_id": "${RELEASE_ID}",
  "git_sha": "${GIT_SHA}",
  "status": "SUCCESS",
  "doc_only": false,
  "deployed_at": "${now}",
  "promoted_components": [
${promoted_json}
  ]
}
EOF

finish_rollout_journal "COMPLETED" "$evidence_dir"

log_info "=========================================================="
log_info "Rollout Orchestration COMPLETED successfully for [${RELEASE_ID}]"
log_info "Evidence retained in: ${evidence_dir}"
log_info "=========================================================="
exit 0
