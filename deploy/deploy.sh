#!/usr/bin/env bash
# Primary deployment entrypoint - executes transactional warm cutover
set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deploy/lib.sh
source "$SCRIPT_DIR/lib.sh"

image_pattern='^[^[:space:]]+@sha256:[a-f0-9]{64}$'
env_file="${ENV_FILE:-$SCRIPT_DIR/.env.production}"
compose_file="${COMPOSE_FILE:-$SCRIPT_DIR/compose.prod.yaml}"

# Options
upgrade_core=0
fresh_init=0
resume_soak=0
soak_seconds="${SOAK_DURATION_SEC:-900}"
detach_soak=0

# Detect CI environment to prevent timeout on 15m soak
if [[ "${CI:-false}" == "true" || "${GITHUB_ACTIONS:-false}" == "true" ]]; then
  detach_soak=1
fi

positional=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --upgrade-core)
      upgrade_core=1
      shift
      ;;
    --fresh-init)
      fresh_init=1
      shift
      ;;
    --resume-soak)
      resume_soak=1
      shift
      ;;
    --soak-seconds)
      soak_seconds="$2"
      shift 2
      ;;
    --detach-soak)
      detach_soak=1
      shift
      ;;
    *)
      positional+=("$1")
      shift
      ;;
  esac
done

if [[ "$fresh_init" -eq 1 ]]; then
  exec "$SCRIPT_DIR/init-fresh-data.sh" "${positional[@]}"
fi

if [[ "$resume_soak" -eq 1 ]]; then
  resume_soak
  exit 0
fi

validate_canonical_env

# Determine positional arguments: $1=gateway, $2=auth-browser, $3=tts-gateway, $4=staged_compose
image_ref="${positional[0]:-${IMAGE_REF:-${GATEWAY_IMAGE_REF:-}}}"
browser_image_ref="${positional[1]:-${AUTH_BROWSER_IMAGE_REF:-${BROWSER_IMAGE_REF:-}}}"
raw_arg3="${positional[2]:-}"
raw_arg4="${positional[3]:-}"

tts_image_ref=""
staged_compose=""

if [[ "$raw_arg3" =~ $image_pattern ]]; then
  tts_image_ref="$raw_arg3"
  staged_compose="$raw_arg4"
elif [[ -n "${TTS_GATEWAY_IMAGE_REF:-}" && "${TTS_GATEWAY_IMAGE_REF}" =~ $image_pattern ]]; then
  tts_image_ref="${TTS_GATEWAY_IMAGE_REF}"
  staged_compose="$raw_arg3"
else
  tts_image_ref="${TTS_IMAGE_REF:-}"
  staged_compose="$raw_arg3"
fi

if [[ -z "$image_ref" ]]; then
  log_error "Usage: $0 [options] <gateway-image-digest> [auth-browser-digest] [tts-digest] [staged-compose]"
  exit 1
fi

validate_digest "$image_ref" "gateway"
export IMAGE_REF="$image_ref"
export GATEWAY_IMAGE_REF="$image_ref"
if [[ -z "${WORKER_IMAGE_REF:-}" && -n "${WORKER_IMAGE:-}" ]]; then
  export WORKER_IMAGE_REF="$WORKER_IMAGE"
fi
if [[ -z "${DBTOOL_IMAGE_REF:-}" && -n "${DBTOOL_IMAGE:-}" ]]; then
  export DBTOOL_IMAGE_REF="$DBTOOL_IMAGE"
fi

if [[ -n "${WORKER_IMAGE_REF:-}" ]]; then
  validate_digest "$WORKER_IMAGE_REF" "worker"
  set_release_env "WORKER_IMAGE_REF" "$WORKER_IMAGE_REF" 2>/dev/null || true
fi

if [[ -n "${DBTOOL_IMAGE_REF:-}" ]]; then
  validate_digest "$DBTOOL_IMAGE_REF" "dbtool"
  set_release_env "DBTOOL_IMAGE_REF" "$DBTOOL_IMAGE_REF" 2>/dev/null || true
fi

if [[ -n "$browser_image_ref" ]]; then
  validate_digest "$browser_image_ref" "auth-browser"
  export AUTH_BROWSER_IMAGE_REF="$browser_image_ref"
  export BROWSER_IMAGE_REF="$browser_image_ref"
  set_release_env "BROWSER_IMAGE_REF" "$browser_image_ref" 2>/dev/null || true
fi

if [[ -n "$tts_image_ref" ]]; then
  validate_digest "$tts_image_ref" "tts-gateway"
  export TTS_GATEWAY_IMAGE_REF="$tts_image_ref"
  export TTS_IMAGE_REF="$tts_image_ref"
  set_release_env "TTS_IMAGE_REF" "$tts_image_ref" 2>/dev/null || true
fi

bark_image_ref="${BARK_IMAGE_REF:-}"
if [[ -n "$bark_image_ref" ]]; then
  validate_digest "$bark_image_ref" "bark"
  export BARK_IMAGE_REF="$bark_image_ref"
  set_release_env "BARK_IMAGE_REF" "$bark_image_ref" 2>/dev/null || true
fi

# If staged compose file was provided, validate and install it atomically
if [[ -n "$staged_compose" && -f "$staged_compose" ]]; then
  log_info "Validating staged Compose file: ${staged_compose}"
  docker compose --env-file "$env_file" --env-file "$RELEASE_ENV_FILE" -f "$staged_compose" config --quiet
  [[ -f "$compose_file" ]] && cp -p "$compose_file" "$SCRIPT_DIR/.previous-compose.yaml"
  mv -f "$staged_compose" "$compose_file"
fi

# Build arguments for dispatch-rollout.sh
rollout_args=()
if [[ -f "$SCRIPT_DIR/release-manifest.json" ]]; then
  rollout_args+=(--manifest "$SCRIPT_DIR/release-manifest.json")
  [[ -f "$SCRIPT_DIR/release-manifest.bundle" ]] && rollout_args+=(--bundle "$SCRIPT_DIR/release-manifest.bundle")
else
  rollout_args+=(--skip-manifest-check)
fi

rollout_args+=(--gateway-image "$image_ref")
if [[ "$upgrade_core" -eq 1 ]]; then
  rollout_args+=(--scope "schema,worker,gateway,auth_browser,tts,bark")
fi
if [[ "$detach_soak" -eq 1 ]]; then
  rollout_args+=(--detach-soak)
fi
rollout_args+=(--soak-seconds "$soak_seconds")

log_info "Executing transactional rollout orchestration via dispatcher..."
exec "$SCRIPT_DIR/dispatch-rollout.sh" "${rollout_args[@]}"
