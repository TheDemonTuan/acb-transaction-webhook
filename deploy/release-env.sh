#!/usr/bin/env bash
# Helpers for atomic read/write and validation of deploy/.release.env
# Release state contains non-secret immutable image refs and slot identities.
set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
export SCRIPT_DIR

RELEASE_ENV_FILE="${RELEASE_ENV_FILE:-$SCRIPT_DIR/.release.env}"
IMAGE_DIGEST_PATTERN='^[^[:space:]]+@sha256:[a-f0-9]{64}$'

REQUIRED_RELEASE_KEYS=(
  "IMAGE_REF_BLUE"
  "IMAGE_REF_GREEN"
  "WORKER_IMAGE_REF"
  "DBTOOL_IMAGE_REF"
  "BROWSER_IMAGE_REF"
  "TTS_IMAGE_REF"
  "BARK_IMAGE_REF"
)

log_release_info() {
  printf '[%s] [RELEASE-INFO] %s\n' "$(date -u +'%Y-%m-%dT%H:%M:%SZ')" "$*"
}

log_release_error() {
  printf '[%s] [RELEASE-ERROR] %s\n' "$(date -u +'%Y-%m-%dT%H:%M:%SZ')" "$*" >&2
}

validate_image_ref() {
  local ref="$1"
  local component="${2:-image}"

  if [[ -z "$ref" ]]; then
    log_release_error "Image ref for ${component} cannot be empty."
    return 1
  fi

  if [[ ! "$ref" =~ $IMAGE_DIGEST_PATTERN ]]; then
    log_release_error "Invalid or non-immutable digest for ${component}: '${ref}' (must match @sha256:<64-hex>)"
    return 1
  fi

  # For Bark third-party image, verify against allowlist if allowlist script exists
  if [[ "$component" == "bark" || "$component" == "BARK_IMAGE_REF" ]]; then
    local policy_script="$SCRIPT_DIR/verify-third-party-policy.sh"
    local allowlist_file="$SCRIPT_DIR/third-party-allowlist.json"
    if [[ -f "$policy_script" && -f "$allowlist_file" ]]; then
      if ! bash "$policy_script" --image "$ref" --allowlist "$allowlist_file" >/dev/null 2>&1; then
        log_release_error "Bark image '${ref}' is rejected by third-party allowlist policy."
        return 1
      fi
    fi
  fi

  return 0
}

get_release_env() {
  local key="$1"
  local default_val="${2:-}"
  local file="${RELEASE_ENV_FILE}"

  if [[ ! -f "$file" ]]; then
    printf '%s\n' "$default_val"
    return 0
  fi

  local val
  val="$(grep -E "^[[:space:]]*${key}=" "$file" | tail -n 1 | cut -d'=' -f2- | tr -d '\r')"
  if [[ -z "$val" ]]; then
    printf '%s\n' "$default_val"
  else
    printf '%s\n' "$val"
  fi
}

set_release_env() {
  local key="$1"
  local value="$2"
  local file="${RELEASE_ENV_FILE}"

  # If key looks like an image ref, validate format fail-closed
  if [[ "$key" =~ _IMAGE_REF$ || "$key" =~ ^IMAGE_REF_ || "$key" == "WORKER_IMAGE_REF" || "$key" == "DBTOOL_IMAGE_REF" || "$key" == "BROWSER_IMAGE_REF" || "$key" == "TTS_IMAGE_REF" || "$key" == "BARK_IMAGE_REF" ]]; then
    if ! validate_image_ref "$value" "$key"; then
      return 1
    fi
  fi

  local target_dir
  target_dir="$(dirname "$file")"
  mkdir -p "$target_dir"

  local prev_value=""
  if [[ -f "$file" ]]; then
    prev_value="$(get_release_env "$key")"
  fi

  local tmp_file
  tmp_file="$(mktemp "${target_dir}/.release.env.tmp.XXXXXX")"

  local key_found=0
  local prev_key="PREVIOUS_${key}"
  local prev_key_found=0

  if [[ -f "$file" ]]; then
    while IFS= read -r line || [[ -n "$line" ]]; do
      # Preserve empty lines and comments
      if [[ "$line" =~ ^[[:space:]]*# || -z "${line//[[:space:]]/}" ]]; then
        printf '%s\n' "$line" >> "$tmp_file"
        continue
      fi

      local line_key
      line_key="$(printf '%s' "$line" | cut -d'=' -f1 | tr -d '[:space:]')"

      if [[ "$line_key" == "$key" ]]; then
        printf '%s=%s\n' "$key" "$value" >> "$tmp_file"
        key_found=1
      elif [[ "$line_key" == "$prev_key" ]]; then
        if [[ -n "$prev_value" && "$prev_value" != "$value" ]]; then
          printf '%s=%s\n' "$prev_key" "$prev_value" >> "$tmp_file"
        else
          printf '%s\n' "$line" >> "$tmp_file"
        fi
        prev_key_found=1
      else
        printf '%s\n' "$line" >> "$tmp_file"
      fi
    done < "$file"
  fi

  if [[ "$key_found" -eq 0 ]]; then
    printf '%s=%s\n' "$key" "$value" >> "$tmp_file"
  fi

  # Record previous ref if previous ref existed, changed, and was not already written
  if [[ "$prev_key_found" -eq 0 && -n "$prev_value" && "$prev_value" != "$value" ]]; then
    if [[ "$key" =~ _IMAGE_REF$ || "$key" =~ ^IMAGE_REF_ || "$key" == "WORKER_IMAGE_REF" || "$key" == "DBTOOL_IMAGE_REF" || "$key" == "BROWSER_IMAGE_REF" || "$key" == "TTS_IMAGE_REF" || "$key" == "BARK_IMAGE_REF" ]]; then
      printf '%s=%s\n' "$prev_key" "$prev_value" >> "$tmp_file"
    fi
  fi

  chmod 600 "$tmp_file" 2>/dev/null || true
  mv -f "$tmp_file" "$file"
  return 0
}

rollback_release_env() {
  local key="$1"
  local file="${RELEASE_ENV_FILE}"

  if [[ ! -f "$file" ]]; then
    log_release_error "Cannot rollback ${key}: release file '${file}' does not exist."
    return 1
  fi

  local prev_key="PREVIOUS_${key}"
  local prev_value
  prev_value="$(get_release_env "$prev_key")"

  if [[ -z "$prev_value" ]]; then
    log_release_error "Cannot rollback ${key}: no '${prev_key}' found in ${file}."
    return 1
  fi

  if ! validate_image_ref "$prev_value" "$key"; then
    log_release_error "Cannot rollback ${key}: '${prev_key}' value '${prev_value}' is not a valid immutable digest."
    return 1
  fi

  local current_value
  current_value="$(get_release_env "$key")"

  # Swap current and previous
  set_release_env "$key" "$prev_value"
  if [[ -n "$current_value" ]]; then
    set_release_env "$prev_key" "$current_value"
  fi

  log_release_info "Successfully rolled back ${key} to ${prev_value}."
  return 0
}

validate_release_env_file() {
  local file="${1:-$RELEASE_ENV_FILE}"

  if [[ ! -f "$file" ]]; then
    log_release_error "Release file does not exist: '${file}'"
    return 1
  fi

  local missing=()
  local invalid=()

  for req_key in "${REQUIRED_RELEASE_KEYS[@]}"; do
    local val
    val="$(grep -E "^[[:space:]]*${req_key}=" "$file" | tail -n 1 | cut -d'=' -f2- | tr -d '\r[:space:]' || true)"
    if [[ -z "$val" ]]; then
      missing+=("$req_key")
    elif ! validate_image_ref "$val" "$req_key"; then
      invalid+=("$req_key")
    fi
  done

  if [[ ${#missing[@]} -gt 0 ]]; then
    log_release_error "Missing required release image references in ${file}: [${missing[*]}]"
    return 1
  fi

  if [[ ${#invalid[@]} -gt 0 ]]; then
    log_release_error "Invalid immutable image references in ${file}: [${invalid[*]}]"
    return 1
  fi

  return 0
}

init_release_env() {
  local target_file="${RELEASE_ENV_FILE}"
  local gw_blue=""
  local gw_green=""
  local worker=""
  local dbtool=""
  local browser=""
  local tts=""
  local bark=""

  while [[ $# -gt 0 ]]; do
    case "$1" in
      --file)
        target_file="$2"; shift 2 ;;
      --gateway-image)
        gw_blue="$2"; gw_green="$2"; shift 2 ;;
      --gateway-blue)
        gw_blue="$2"; shift 2 ;;
      --gateway-green)
        gw_green="$2"; shift 2 ;;
      --worker-image)
        worker="$2"; shift 2 ;;
      --dbtool-image)
        dbtool="$2"; shift 2 ;;
      --auth-browser-image|--browser-image)
        browser="$2"; shift 2 ;;
      --tts-image)
        tts="$2"; shift 2 ;;
      --bark-image)
        bark="$2"; shift 2 ;;
      *)
        log_release_error "Unknown init option: $1"
        return 1
        ;;
    esac
  done

  local missing=()
  [[ -z "$gw_blue" ]] && missing+=("IMAGE_REF_BLUE")
  [[ -z "$gw_green" ]] && missing+=("IMAGE_REF_GREEN")
  [[ -z "$worker" ]] && missing+=("WORKER_IMAGE_REF")
  [[ -z "$dbtool" ]] && missing+=("DBTOOL_IMAGE_REF")
  [[ -z "$browser" ]] && missing+=("BROWSER_IMAGE_REF")
  [[ -z "$tts" ]] && missing+=("TTS_IMAGE_REF")
  [[ -z "$bark" ]] && missing+=("BARK_IMAGE_REF")

  if [[ ${#missing[@]} -gt 0 ]]; then
    log_release_error "Cannot initialize ${target_file}: missing required components: [${missing[*]}]"
    return 1
  fi

  validate_image_ref "$gw_blue" "IMAGE_REF_BLUE"
  validate_image_ref "$gw_green" "IMAGE_REF_GREEN"
  validate_image_ref "$worker" "WORKER_IMAGE_REF"
  validate_image_ref "$dbtool" "DBTOOL_IMAGE_REF"
  validate_image_ref "$browser" "BROWSER_IMAGE_REF"
  validate_image_ref "$tts" "TTS_IMAGE_REF"
  validate_image_ref "$bark" "BARK_IMAGE_REF"

  local target_dir
  target_dir="$(dirname "$target_file")"
  mkdir -p "$target_dir"

  local tmp_file
  tmp_file="$(mktemp "${target_dir}/.release.env.tmp.XXXXXX")"

  cat <<EOF > "$tmp_file"
# ACB Canonical Release State - Immutable Container Digests
# Auto-managed by deploy/release-env.sh and deployment transactions.
# DO NOT place secrets in this file.

IMAGE_REF_BLUE=${gw_blue}
IMAGE_REF_GREEN=${gw_green}
WORKER_IMAGE_REF=${worker}
DBTOOL_IMAGE_REF=${dbtool}
BROWSER_IMAGE_REF=${browser}
TTS_IMAGE_REF=${tts}
BARK_IMAGE_REF=${bark}
EOF

  chmod 600 "$tmp_file" 2>/dev/null || true
  mv -f "$tmp_file" "$target_file"
  log_release_info "Initialized canonical release environment at ${target_file}."
  return 0
}

export_release_env() {
  local file="${1:-$RELEASE_ENV_FILE}"
  if [[ ! -f "$file" ]]; then
    return 0
  fi

  while IFS='=' read -r key val || [[ -n "$key" ]]; do
    key="$(printf '%s' "$key" | tr -d '[:space:]')"
    [[ -z "$key" || "$key" =~ ^# ]] && continue
    val="$(printf '%s' "$val" | tr -d '\r')"
    export "$key=$val"
  done < "$file"
}

# CLI dispatcher if executed directly
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  cmd="${1:-}"
  shift || true
  case "$cmd" in
    get)
      get_release_env "$@"
      ;;
    set)
      set_release_env "$@"
      ;;
    rollback)
      rollback_release_env "$@"
      ;;
    validate)
      validate_release_env_file "$@"
      ;;
    validate-ref)
      validate_image_ref "$@"
      ;;
    init)
      init_release_env "$@"
      ;;
    export)
      export_release_env "$@"
      ;;
    *)
      cat <<'EOF'
Usage: release-env.sh <command> [arguments...]

Commands:
  get <KEY> [default]        Retrieve a release key value
  set <KEY> <VALUE>          Atomically set a key/value with previous backup
  rollback <KEY>             Restore PREVIOUS_<KEY> to <KEY>
  validate [file]            Validate all 7 required components in release file
  validate-ref <REF> [name]  Validate single image digest format
  init [options]             Initialize a complete .release.env file
  export [file]              Load and export release variables into environment
EOF
      exit 1
      ;;
  esac
fi
