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

EXPECTED_IDENTITY="${EXPECTED_IDENTITY:-}"
EXPECTED_ISSUER="${EXPECTED_ISSUER:-https://token.actions.githubusercontent.com}"
REQUIRE_COSIGN="${REQUIRE_COSIGN:-0}"
SKIP_MANIFEST_CHECK="${SKIP_MANIFEST_CHECK:-0}"
ALLOW_REDEPLOY="${ALLOW_REDEPLOY:-0}"
MAX_AGE_SECONDS="${MAX_AGE_SECONDS:-86400}"
SOAK_SECONDS="${SOAK_DURATION_SEC:-900}"
DETACH_SOAK="${DETACH_SOAK:-0}"
RESUME_SOAK="${RESUME_SOAK:-0}"
REQUESTED_SCOPE="${REQUESTED_SCOPE:-}"

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

# Detect CI environment to detach soak automatically
if [[ "${CI:-false}" == "true" || "${GITHUB_ACTIONS:-false}" == "true" ]]; then
  DETACH_SOAK=1
fi

# Rollout Journal Helpers
init_rollout_journal() {
  local r_id="$1"
  local git_sha="$2"
  local scope="$3"
  mkdir -p "$(dirname "$ROLLOUT_JOURNAL_FILE")"
  local now
  now="$(date -u +'%Y-%m-%dT%H:%M:%SZ')"
  cat <<EOF > "$ROLLOUT_JOURNAL_FILE"
{
  "rollout_id": "${r_id}",
  "git_sha": "${git_sha}",
  "status": "RUNNING",
  "scope": "${scope}",
  "started_at": "${now}",
  "updated_at": "${now}",
  "current_step": "INITIALIZED",
  "completed_steps": []
}
EOF
}

update_rollout_step() {
  local step="$1"
  local status="$2"
  if [[ -f "$ROLLOUT_JOURNAL_FILE" ]]; then
    local now
    now="$(date -u +'%Y-%m-%dT%H:%M:%SZ')"
    sed -i -e "s/\"current_step\":[[:space:]]*\"[^\"]*\"/\"current_step\": \"$step\"/" \
           -e "s/\"status\":[[:space:]]*\"[^\"]*\"/\"status\": \"$status\"/" \
           -e "s/\"updated_at\":[[:space:]]*\"[^\"]*\"/\"updated_at\": \"$now\"/" "$ROLLOUT_JOURNAL_FILE" 2>/dev/null || true
    if command -v node >/dev/null 2>&1; then
      node - "$ROLLOUT_JOURNAL_FILE" "$step" "$status" "$now" <<'JSEOF' 2>/dev/null || true
const fs = require('fs');
const [,, file, step, status, now] = process.argv;
try {
  const j = JSON.parse(fs.readFileSync(file, 'utf8'));
  j.current_step = step;
  j.status = status;
  j.updated_at = now;
  if (status === 'STEP_COMPLETED' && !j.completed_steps.includes(step)) {
    j.completed_steps.push(step);
  }
  fs.writeFileSync(file, JSON.stringify(j, null, 2));
} catch (e) {}
JSEOF
    fi
  fi
}

finish_rollout_journal() {
  local final_status="$1"
  local evidence_dir="${2:-}"
  if [[ -f "$ROLLOUT_JOURNAL_FILE" ]]; then
    local now
    now="$(date -u +'%Y-%m-%dT%H:%M:%SZ')"
    sed -i -e "s/\"status\":[[:space:]]*\"[^\"]*\"/\"status\": \"$final_status\"/" \
           -e "s/\"updated_at\":[[:space:]]*\"[^\"]*\"/\"updated_at\": \"$now\"/" "$ROLLOUT_JOURNAL_FILE" 2>/dev/null || true
    if command -v node >/dev/null 2>&1; then
      node - "$ROLLOUT_JOURNAL_FILE" "$final_status" "$now" <<'JSEOF' 2>/dev/null || true
const fs = require('fs');
const [,, file, status, now] = process.argv;
try {
  const j = JSON.parse(fs.readFileSync(file, 'utf8'));
  j.status = status;
  j.updated_at = now;
  fs.writeFileSync(file, JSON.stringify(j, null, 2));
} catch (e) {}
JSEOF
    fi
    if [[ -n "$evidence_dir" && -d "$evidence_dir" ]]; then
      cp -f "$ROLLOUT_JOURNAL_FILE" "$evidence_dir/rollout-journal.json" 2>/dev/null || true
    fi
    if [[ "$final_status" == "COMPLETED" || "$final_status" == "DOC_ONLY" ]]; then
      mv -f "$ROLLOUT_JOURNAL_FILE" "${ROLLOUT_JOURNAL_FILE}.previous" 2>/dev/null || true
    fi
  fi
}

ROLLOUT_EXIT_CODE=0
cleanup_rollout() {
  local code=$?
  if [[ "$code" -ne 0 && "$ROLLOUT_EXIT_CODE" -eq 0 ]]; then
    ROLLOUT_EXIT_CODE="$code"
  fi
  if [[ "$ROLLOUT_EXIT_CODE" -ne 0 ]]; then
    log_warn "Rollout terminated with code ${ROLLOUT_EXIT_CODE}."
    if [[ -f "${ROLLOUT_JOURNAL_FILE:-}" ]]; then
      finish_rollout_journal "INTERRUPTED" ""
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

# 2. Startup Recovery: recover any uncommitted prior transaction
if [[ -f "${TX_JOURNAL_FILE:-}" ]]; then
  log_info "Startup recovery: inspecting prior component transaction journal..."
  recover_tx_journal
fi

if [[ -f "$ROLLOUT_JOURNAL_FILE" ]]; then
  prev_st="$(grep -o '"status":[[:space:]]*"[^"]*"' "$ROLLOUT_JOURNAL_FILE" | head -n1 | cut -d'"' -f4 || echo "")"
  if [[ "$prev_st" == "RUNNING" || "$prev_st" == "INTERRUPTED" ]]; then
    log_warn "Startup recovery: archiving previous interrupted rollout journal (status: ${prev_st})..."
    mv -f "$ROLLOUT_JOURNAL_FILE" "${ROLLOUT_JOURNAL_FILE}.interrupted.$(date +%s)" 2>/dev/null || true
  fi
fi

# Initialize early rollout journal for trap capture
init_rollout_journal "pending" "pending" "unresolved"

# 3. Verify Manifest & Promotion Scope
PROMOTION_GATEWAY="false"
PROMOTION_WORKER="false"
PROMOTION_SCHEMA="false"
PROMOTION_AUTH_BROWSER="false"
PROMOTION_TTS="false"
PROMOTION_BARK="false"
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

    grep -q '"gateway":[[:space:]]*true' "$MANIFEST_FILE" 2>/dev/null && PROMOTION_GATEWAY="true"
    grep -q '"worker":[[:space:]]*true' "$MANIFEST_FILE" 2>/dev/null && PROMOTION_WORKER="true"
    grep -q '"schema":[[:space:]]*true' "$MANIFEST_FILE" 2>/dev/null && PROMOTION_SCHEMA="true"
    grep -q '"auth_browser":[[:space:]]*true' "$MANIFEST_FILE" 2>/dev/null && PROMOTION_AUTH_BROWSER="true"
    grep -q '"tts":[[:space:]]*true' "$MANIFEST_FILE" 2>/dev/null && PROMOTION_TTS="true"
    grep -q '"bark":[[:space:]]*true' "$MANIFEST_FILE" 2>/dev/null && PROMOTION_BARK="true"
    grep -q '"platform":[[:space:]]*true' "$MANIFEST_FILE" 2>/dev/null && PROMOTION_PLATFORM="true"
    grep -q '"promotion_doc_only":[[:space:]]*true' "$MANIFEST_FILE" 2>/dev/null && PROMOTION_DOC_ONLY="true"

    [[ -z "$IMAGE_GATEWAY" ]] && IMAGE_GATEWAY="$(grep -o '"gateway":[[:space:]]*"[^"]*"' "$MANIFEST_FILE" 2>/dev/null | head -n1 | cut -d'"' -f4 || true)"
    [[ -z "$IMAGE_WORKER" ]] && IMAGE_WORKER="$(grep -o '"worker":[[:space:]]*"[^"]*"' "$MANIFEST_FILE" 2>/dev/null | head -n1 | cut -d'"' -f4 || true)"
    [[ -z "$IMAGE_DBTOOL" ]] && IMAGE_DBTOOL="$(grep -o '"dbtool":[[:space:]]*"[^"]*"' "$MANIFEST_FILE" 2>/dev/null | head -n1 | cut -d'"' -f4 || true)"
    [[ -z "$IMAGE_AUTH_BROWSER" ]] && IMAGE_AUTH_BROWSER="$(grep -o '"auth_browser":[[:space:]]*"[^"]*"' "$MANIFEST_FILE" 2>/dev/null | head -n1 | cut -d'"' -f4 || true)"
    [[ -z "$IMAGE_TTS_GATEWAY" ]] && IMAGE_TTS_GATEWAY="$(grep -o '"tts_gateway":[[:space:]]*"[^"]*"' "$MANIFEST_FILE" 2>/dev/null | head -n1 | cut -d'"' -f4 || true)"
    [[ -z "$IMAGE_BARK" ]] && IMAGE_BARK="$(grep -o '"bark":[[:space:]]*"[^"]*"' "$MANIFEST_FILE" 2>/dev/null | head -n1 | cut -d'"' -f4 || true)"
  elif [[ -n "$REQUESTED_SCOPE" ]]; then
    IFS=',' read -ra scopes <<< "$REQUESTED_SCOPE"
    for sc in "${scopes[@]}"; do
      case "$sc" in
        gateway) PROMOTION_GATEWAY="true" ;;
        worker) PROMOTION_WORKER="true" ;;
        schema) PROMOTION_SCHEMA="true" ;;
        auth_browser|auth-browser) PROMOTION_AUTH_BROWSER="true" ;;
        tts|tts_gateway|tts-gateway) PROMOTION_TTS="true" ;;
        bark) PROMOTION_BARK="true" ;;
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

  for promotion_var in PROMOTION_GATEWAY PROMOTION_WORKER PROMOTION_SCHEMA PROMOTION_AUTH_BROWSER PROMOTION_TTS PROMOTION_BARK PROMOTION_PLATFORM PROMOTION_DOC_ONLY; do
    promotion_value="${!promotion_var:-}"
    [[ "$promotion_value" == "true" || "$promotion_value" == "false" ]] || {
      log_error "Invalid verified promotion flag [$promotion_var=$promotion_value]."
      exit 1
    }
  done

  IMAGE_GATEWAY="${IMAGE_GATEWAY:-${IMAGE_GATEWAY:-}}"
  IMAGE_WORKER="${IMAGE_WORKER:-${IMAGE_WORKER:-}}"
  IMAGE_DBTOOL="${IMAGE_DBTOOL:-${IMAGE_DBTOOL:-}}"
  IMAGE_AUTH_BROWSER="${IMAGE_AUTH_BROWSER:-${IMAGE_AUTH_BROWSER:-}}"
  IMAGE_TTS_GATEWAY="${IMAGE_TTS_GATEWAY:-${IMAGE_TTS_GATEWAY:-}}"
  IMAGE_BARK="${IMAGE_BARK:-${IMAGE_BARK:-}}"
fi

# Validate caller-requested scope against manifest authorization
if [[ -n "$REQUESTED_SCOPE" ]]; then
  IFS=',' read -ra req_scopes <<< "$REQUESTED_SCOPE"
  for sc in "${req_scopes[@]}"; do
    case "$sc" in
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
      platform)
        if [[ "$PROMOTION_PLATFORM" != "true" ]]; then
          log_error "Unauthorized promotion request: component [platform] is NOT authorized by signed manifest."
          exit 1
        fi
        ;;
    esac
  done

  # Narrow promotion to only requested components
  PROMOTION_GATEWAY="false"
  PROMOTION_WORKER="false"
  PROMOTION_SCHEMA="false"
  PROMOTION_AUTH_BROWSER="false"
  PROMOTION_TTS="false"
  PROMOTION_BARK="false"
  PROMOTION_PLATFORM="false"
  for sc in "${req_scopes[@]}"; do
    case "$sc" in
      gateway) PROMOTION_GATEWAY="true" ;;
      worker) PROMOTION_WORKER="true" ;;
      schema) PROMOTION_SCHEMA="true" ;;
      auth_browser|auth-browser) PROMOTION_AUTH_BROWSER="true" ;;
      tts|tts_gateway|tts-gateway) PROMOTION_TTS="true" ;;
      bark) PROMOTION_BARK="true" ;;
      platform) PROMOTION_PLATFORM="true" ;;
    esac
  done
fi

# 4. Docs-Only Release: Promote zero runtime containers/migrations
is_docs_only=0
if [[ "${PROMOTION_DOC_ONLY:-false}" == "true" ]] || \
   ([[ "${PROMOTION_GATEWAY:-false}" != "true" ]] && \
    [[ "${PROMOTION_WORKER:-false}" != "true" ]] && \
    [[ "${PROMOTION_SCHEMA:-false}" != "true" ]] && \
    [[ "${PROMOTION_AUTH_BROWSER:-false}" != "true" ]] && \
    [[ "${PROMOTION_TTS:-false}" != "true" ]] && \
    [[ "${PROMOTION_BARK:-false}" != "true" ]] && \
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

  cat <<EOF > "$evidence_dir/receipt.json"
{
  "release_id": "${RELEASE_ID:-rel-doc-$GIT_SHA}",
  "git_sha": "${GIT_SHA}",
  "status": "DOC_ONLY",
  "deployed_at": "${now}",
  "promoted_components": [],
  "message": "Documentation-only release successfully acknowledged without runtime changes."
}
EOF

  cat <<EOF > "$LAST_RELEASE_FILE"
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
[[ "${PROMOTION_SCHEMA:-false}" == "true" ]] && active_scope_list+=("schema")
[[ "${PROMOTION_AUTH_BROWSER:-false}" == "true" ]] && active_scope_list+=("auth_browser")
[[ "${PROMOTION_TTS:-false}" == "true" ]] && active_scope_list+=("tts")
[[ "${PROMOTION_BARK:-false}" == "true" ]] && active_scope_list+=("bark")
[[ "${PROMOTION_WORKER:-false}" == "true" ]] && active_scope_list+=("worker")
[[ "${PROMOTION_GATEWAY:-false}" == "true" ]] && active_scope_list+=("gateway")
[[ "${PROMOTION_PLATFORM:-false}" == "true" ]] && active_scope_list+=("platform")

scope_str="$(IFS=,; echo "${active_scope_list[*]}")"

log_info "=========================================================="
log_info "Starting Rollout Orchestration for [${RELEASE_ID}] (${GIT_SHA})"
log_info "Authorized Promotion Scope: [${scope_str}]"
log_info "=========================================================="

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
  bash "$DEPLOY_DIR/deploy-schema.sh" "$IMAGE_DBTOOL"
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
  bash "$DEPLOY_DIR/deploy-auth-browser.sh" "$IMAGE_AUTH_BROWSER"
  update_rollout_step "auth_browser" "STEP_COMPLETED"
  promoted_list+=("auth_browser")
  log_info "Transaction 2a: Auth Browser completed."
fi

if [[ "${PROMOTION_TTS:-false}" == "true" ]]; then
  log_info "Executing Transaction 2b: Auxiliary TTS Gateway..."
  update_rollout_step "tts" "RUNNING"
  [[ -n "$IMAGE_TTS_GATEWAY" ]] || { log_error "TTS image digest is required for TTS promotion."; exit 1; }
  validate_digest "$IMAGE_TTS_GATEWAY" "tts-gateway"
  bash "$DEPLOY_DIR/deploy-tts.sh" "$IMAGE_TTS_GATEWAY"
  update_rollout_step "tts" "STEP_COMPLETED"
  promoted_list+=("tts")
  log_info "Transaction 2b: TTS Gateway completed."
fi

if [[ "${PROMOTION_BARK:-false}" == "true" ]]; then
  log_info "Executing Transaction 2c: Auxiliary Bark Service..."
  update_rollout_step "bark" "RUNNING"
  [[ -n "$IMAGE_BARK" ]] || { log_error "BARK image digest is required for Bark promotion."; exit 1; }
  validate_digest "$IMAGE_BARK" "bark"
  bash "$DEPLOY_DIR/deploy-bark.sh" "$IMAGE_BARK"
  update_rollout_step "bark" "STEP_COMPLETED"
  promoted_list+=("bark")
  log_info "Transaction 2c: Bark Service completed."
fi

# Step 3: Singleton Worker Upgrade
if [[ "${PROMOTION_WORKER:-false}" == "true" ]]; then
  log_info "Executing Transaction 3: Worker Singleton Upgrade..."
  update_rollout_step "worker" "RUNNING"
  [[ -n "$IMAGE_WORKER" ]] || { log_error "WORKER image digest is required for worker promotion."; exit 1; }
  validate_digest "$IMAGE_WORKER" "worker"
  bash "$DEPLOY_DIR/deploy-worker.sh" "$IMAGE_WORKER"
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
  [[ "$SOAK_SECONDS" -gt 0 ]] && gw_args+=(--soak-seconds "$SOAK_SECONDS")
  [[ "$DETACH_SOAK" -eq 1 ]] && gw_args+=(--detach-soak)
  [[ "$SKIP_MANIFEST_CHECK" -eq 1 ]] && gw_args+=(--skip-manifest-check)
  bash "$DEPLOY_DIR/deploy-gateway.sh" "${gw_args[@]}"
  update_rollout_step "gateway" "STEP_COMPLETED"
  promoted_list+=("gateway")
  log_info "Transaction 4: Gateway Blue/Green Promotion completed."
fi

# Step 5: Platform / Edge Config (if platform only or platform scoped)
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

cat <<EOF > "$evidence_dir/receipt.json"
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

cat <<EOF > "$LAST_RELEASE_FILE"
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

# Update release environment
if [[ -f "$DEPLOY_DIR/release-env.sh" ]]; then
  # shellcheck source=deploy/release-env.sh
  source "$DEPLOY_DIR/release-env.sh"
  [[ -n "${IMAGE_GATEWAY:-}" && "${PROMOTION_GATEWAY:-false}" == "true" ]] && set_release_env "GATEWAY_IMAGE_REF" "$IMAGE_GATEWAY" 2>/dev/null || true
  [[ -n "${IMAGE_WORKER:-}" && "${PROMOTION_WORKER:-false}" == "true" ]] && set_release_env "WORKER_IMAGE_REF" "$IMAGE_WORKER" 2>/dev/null || true
  [[ -n "${IMAGE_DBTOOL:-}" && "${PROMOTION_SCHEMA:-false}" == "true" ]] && set_release_env "DBTOOL_IMAGE_REF" "$IMAGE_DBTOOL" 2>/dev/null || true
  [[ -n "${IMAGE_AUTH_BROWSER:-}" && "${PROMOTION_AUTH_BROWSER:-false}" == "true" ]] && set_release_env "BROWSER_IMAGE_REF" "$IMAGE_AUTH_BROWSER" 2>/dev/null || true
  [[ -n "${IMAGE_TTS_GATEWAY:-}" && "${PROMOTION_TTS:-false}" == "true" ]] && set_release_env "TTS_IMAGE_REF" "$IMAGE_TTS_GATEWAY" 2>/dev/null || true
  [[ -n "${IMAGE_BARK:-}" && "${PROMOTION_BARK:-false}" == "true" ]] && set_release_env "BARK_IMAGE_REF" "$IMAGE_BARK" 2>/dev/null || true
fi

finish_rollout_journal "COMPLETED" "$evidence_dir"

log_info "=========================================================="
log_info "Rollout Orchestration COMPLETED successfully for [${RELEASE_ID}]"
log_info "Evidence retained in: ${evidence_dir}"
log_info "=========================================================="
exit 0
