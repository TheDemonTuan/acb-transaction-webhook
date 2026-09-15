#!/usr/bin/env bash
set -Eeuo pipefail

# compute-promotion-scope.sh
# Analyzes changed, deleted, and renamed paths between a base reference (e.g.
# last successful release) and candidate commit to determine which components
# must be promoted.

base_ref="${LAST_RELEASE_COMMIT:-}"
head_ref="${GITHUB_SHA:-HEAD}"
files_from=""
format="json"
output_file=""
verbose=0

usage() {
  cat <<'EOF'
Usage: compute-promotion-scope.sh [options]

Options:
  --base <ref>            Verified production base ref (required unless LAST_RELEASE_COMMIT is set)
  --head <ref>            Head git ref/commit (default: GITHUB_SHA or HEAD)
  --files-from <path>     Read changed paths from file (supports name-status format)
  --format <json|env|list> Output format (default: json)
  --output <path>         Write output to file instead of stdout
  --verbose, -v           Print classification details to stderr
  --help, -h              Show this help message
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --base)
      base_ref="$2"
      shift 2
      ;;
    --head)
      head_ref="$2"
      shift 2
      ;;
    --files-from)
      files_from="$2"
      shift 2
      ;;
    --format)
      format="$2"
      shift 2
      ;;
    --output)
      output_file="$2"
      shift 2
      ;;
    --verbose|-v)
      verbose=1
      shift
      ;;
    --help|-h)
      usage
      exit 0
      ;;
    *)
      printf 'Unknown argument: %s\n' "$1" >&2
      usage >&2
      exit 1
      ;;
  esac
done

# Collect raw file changes
raw_paths=()

if [[ -n "$files_from" ]]; then
  if [[ ! -f "$files_from" ]]; then
    printf 'Error: files-from path not found: %s\n' "$files_from" >&2
    exit 1
  fi
  while IFS= read -r line || [[ -n "$line" ]]; do
    line="$(printf '%s' "$line" | tr -d '\r')"
    [[ -z "$line" ]] && continue
    # Handle git diff --name-status format (e.g. "M\tpath", "R100\told\tnew")
    if [[ "$line" =~ ^[A-Z][0-9]*[[:space:]]+(.+)$ ]]; then
      rest="${BASH_REMATCH[1]}"
      while IFS=$'\t ' read -r -a tokens; do
        for tok in "${tokens[@]}"; do
          [[ -n "$tok" ]] && raw_paths+=("$tok")
        done
      done <<< "$rest"
    else
      raw_paths+=("$line")
    fi
  done < "$files_from"
else
  # Query Git
  if ! command -v git >/dev/null 2>&1; then
    printf 'Error: git CLI required when --files-from is not provided\n' >&2
    exit 1
  fi

  if [[ -z "$base_ref" ]]; then
    printf 'Error: --base or LAST_RELEASE_COMMIT is required when reading changes from git\n' >&2
    exit 1
  fi
  if ! git cat-file -e "${base_ref}^{commit}" 2>/dev/null && ! git cat-file -e "${base_ref}^{tree}" 2>/dev/null; then
    printf 'Error: production base ref is not available locally: %s\n' "$base_ref" >&2
    exit 1
  fi
  if ! git cat-file -e "${head_ref}^{commit}" 2>/dev/null; then
    printf 'Error: candidate head ref is not available locally: %s\n' "$head_ref" >&2
    exit 1
  fi

  while IFS= read -r line || [[ -n "$line" ]]; do
    line="$(printf '%s' "$line" | tr -d '\r')"
    [[ -z "$line" ]] && continue
    if [[ "$line" =~ ^[A-Z][0-9]*[[:space:]]+(.+)$ ]]; then
      rest="${BASH_REMATCH[1]}"
      while IFS=$'\t ' read -r -a tokens; do
        for tok in "${tokens[@]}"; do
          [[ -n "$tok" ]] && raw_paths+=("$tok")
        done
      done <<< "$rest"
    fi
  done < <(git diff --name-status --find-renames "$base_ref" "$head_ref" 2>/dev/null || true)
fi

# Component flags
scope_frontend=false
scope_gateway=false
scope_worker=false
scope_schema=false
scope_auth_browser=false
scope_tts=false
scope_bark=false
scope_platform=false

all_doc_only=true
total_files=0

# Normalize path helper (convert backslashes to forward slashes)
normalize_path() {
  printf '%s' "$1" | tr '\\' '/'
}

is_doc_path() {
  local p="$1"
  if [[ "$p" =~ \.md$ ]] || [[ "$p" =~ ^docs/ ]] || [[ "$p" =~ ^LICENSE ]] || \
     [[ "$p" == ".gitignore" ]] || [[ "$p" == ".gitattributes" ]] || \
     [[ "$p" == ".github/dependabot.yml" ]]; then
    return 0
  fi
  return 1
}

classify_path() {
  local p="$1"
  local matched=0

  if ! is_doc_path "$p"; then
    all_doc_only=false
  fi

  # 1. Schema & dbtool
  if [[ "$p" =~ ^internal/storage/migrations/ ]] || [[ "$p" =~ ^cmd/dbtool/ ]]; then
    scope_schema=true
    scope_gateway=true
    scope_worker=true
    matched=1
    [[ $verbose -eq 1 ]] && printf '  [CLASSIFY] %s -> schema, gateway, worker (migration/dbtool)\n' "$p" >&2
  fi

  # 2. Frontend paths
  if [[ "$p" =~ ^web/ ]] || [[ "$p" == "deploy/frontend-nginx.conf" ]]; then
    scope_frontend=true
    matched=1
    [[ $verbose -eq 1 ]] && printf '  [CLASSIFY] %s -> frontend\n' "$p" >&2
  fi

  # 3. Gateway paths
  if [[ "$p" =~ ^cmd/gateway/ ]] || [[ "$p" =~ ^internal/httpapi/ ]] || \
     [[ "$p" =~ ^internal/csrf/ ]]; then
    scope_gateway=true
    matched=1
    [[ $verbose -eq 1 ]] && printf '  [CLASSIFY] %s -> gateway\n' "$p" >&2
  fi

  # 3. Worker-only paths
  if [[ "$p" =~ ^cmd/worker/ ]] || [[ "$p" =~ ^internal/workerstate/ ]] || \
     [[ "$p" =~ ^internal/maintenance/ ]] || [[ "$p" =~ ^internal/upstream/ ]]; then
    scope_worker=true
    matched=1
    [[ $verbose -eq 1 ]] && printf '  [CLASSIFY] %s -> worker\n' "$p" >&2
  fi

  # 4. Shared runtime packages imported by both production binaries.
  if [[ "$p" =~ ^internal/workerrpc/ ]] || [[ "$p" =~ ^internal/storage/ ]] || \
     [[ "$p" =~ ^internal/config/ ]] || [[ "$p" =~ ^internal/security/ ]] || \
     [[ "$p" =~ ^internal/domain/ ]] || [[ "$p" =~ ^internal/monitor/ ]] || \
     [[ "$p" =~ ^internal/acb/ ]] || [[ "$p" =~ ^internal/scheduler/ ]] || \
     [[ "$p" =~ ^internal/notification/ ]] || [[ "$p" =~ ^internal/webhook/ ]] || \
     [[ "$p" =~ ^internal/bark/ ]]; then
    scope_gateway=true
    scope_worker=true
    matched=1
    [[ $verbose -eq 1 ]] && printf '  [CLASSIFY] %s -> gateway, worker (shared package)\n' "$p" >&2
  fi

  # 5. Root Go modules and lock files
  if [[ "$p" == "go.mod" ]] || [[ "$p" == "go.sum" ]]; then
    scope_gateway=true
    scope_worker=true
    scope_schema=true
    matched=1
    [[ $verbose -eq 1 ]] && printf '  [CLASSIFY] %s -> gateway, worker, schema (go.mod/sum)\n' "$p" >&2
  fi

  # 6. Auth-browser
  if [[ "$p" == "Dockerfile.auth-browser" ]] || [[ "$p" =~ ^cmd/auth-browser/ ]] || \
     [[ "$p" =~ ^internal/authbrowser/ ]] || [[ "$p" == "deploy/seccomp-auth-browser.json" ]] || \
     [[ "$p" == "deploy/smoke-test-auth-browser.sh" ]]; then
    scope_auth_browser=true
    if [[ "$p" =~ ^internal/authbrowser/ ]]; then
      scope_gateway=true
      scope_worker=true
    fi
    matched=1
    [[ $verbose -eq 1 ]] && printf '  [CLASSIFY] %s -> auth-browser%s\n' "$p" "$([[ "$p" =~ ^internal/authbrowser/ ]] && printf ', gateway, worker')" >&2
  fi

  # 7. TTS
  if [[ "$p" =~ ^tts-gateway/ ]] || [[ "$p" == "deploy/smoke-test-tts-gateway.sh" ]]; then
    scope_tts=true
    matched=1
    [[ $verbose -eq 1 ]] && printf '  [CLASSIFY] %s -> tts\n' "$p" >&2
  fi

  # 8. Bark
  if [[ "$p" == "deploy/third-party-allowlist.json" ]] || \
     [[ "$p" == "deploy/verify-third-party-policy.sh" ]] || \
     [[ "$p" == "deploy/smoke-test-bark.sh" ]] || \
     [[ "$p" == "deploy/bark-entrypoint.sh" ]]; then
    scope_bark=true
    matched=1
    [[ $verbose -eq 1 ]] && printf '  [CLASSIFY] %s -> bark\n' "$p" >&2
  fi

  # 9. Root Dockerfile edits require an explicit full first-party rebuild review.
  if [[ "$p" == "Dockerfile" ]]; then
    scope_frontend=true
    scope_gateway=true
    scope_worker=true
    scope_schema=true
    scope_auth_browser=true
    scope_platform=true
    matched=1
    [[ $verbose -eq 1 ]] && printf '  [CLASSIFY] %s -> frontend, gateway, worker, schema, auth-browser, platform (root Dockerfile)\n' "$p" >&2
  fi

  # 10. Platform / deploy / CI
  if [[ "$p" =~ ^deploy/ ]] || [[ "$p" =~ ^platform/ ]] || \
     [[ "$p" =~ ^\.github/workflows/ ]] || [[ "$p" =~ ^scripts/ ]] || \
     [[ "$p" == "compose.yaml" ]]; then
    scope_platform=true
    matched=1
    [[ $verbose -eq 1 ]] && printf '  [CLASSIFY] %s -> platform\n' "$p" >&2
  fi

  # 11. Doc-only
  if is_doc_path "$p"; then
    matched=1
    [[ $verbose -eq 1 ]] && printf '  [CLASSIFY] %s -> doc-only\n' "$p" >&2
  fi

  # 12. Fallback for unclassified files: fail-safe broader scope
  if [[ $matched -eq 0 ]]; then
    [[ $verbose -eq 1 ]] && printf '  [CLASSIFY] %s -> UNCLASSIFIED (assigning broader scope: gateway, worker, platform)\n' "$p" >&2
    scope_gateway=true
    scope_worker=true
    scope_platform=true
  fi
}

for path_entry in "${raw_paths[@]}"; do
  cleaned="$(normalize_path "$path_entry")"
  [[ -z "$cleaned" ]] && continue
  total_files=$((total_files + 1))
  classify_path "$cleaned"
done

# If there were zero changed files, or all files were documentation-only,
# all runtime components evaluate to false.
if [[ $total_files -eq 0 ]] || [[ "$all_doc_only" == "true" ]]; then
  scope_frontend=false
  scope_gateway=false
  scope_worker=false
  scope_schema=false
  scope_auth_browser=false
  scope_tts=false
  scope_bark=false
  scope_platform=false
fi

# Build list of active scopes
active_scopes=()
[[ "$scope_frontend" == "true" ]] && active_scopes+=("frontend")
[[ "$scope_gateway" == "true" ]] && active_scopes+=("gateway")
[[ "$scope_worker" == "true" ]] && active_scopes+=("worker")
[[ "$scope_schema" == "true" ]] && active_scopes+=("schema")
[[ "$scope_auth_browser" == "true" ]] && active_scopes+=("auth_browser")
[[ "$scope_tts" == "true" ]] && active_scopes+=("tts")
[[ "$scope_bark" == "true" ]] && active_scopes+=("bark")
[[ "$scope_platform" == "true" ]] && active_scopes+=("platform")

format_output() {
  case "$format" in
    env)
      local scope_str
      scope_str="$(IFS=,; echo "${active_scopes[*]}")"
      cat <<EOF
PROMOTION_FRONTEND=$scope_frontend
PROMOTION_GATEWAY=$scope_gateway
PROMOTION_WORKER=$scope_worker
PROMOTION_SCHEMA=$scope_schema
PROMOTION_AUTH_BROWSER=$scope_auth_browser
PROMOTION_TTS=$scope_tts
PROMOTION_BARK=$scope_bark
PROMOTION_PLATFORM=$scope_platform
PROMOTION_SCOPE=$scope_str
PROMOTION_DOC_ONLY=$all_doc_only
PROMOTION_FILES_COUNT=$total_files
EOF
      ;;
    list)
      for s in "${active_scopes[@]}"; do
        printf '%s\n' "$s"
      done
      ;;
    json|*)
      # Construct scopes JSON array
      local json_scopes="[]"
      if [[ ${#active_scopes[@]} -gt 0 ]]; then
        local parts=()
        for s in "${active_scopes[@]}"; do
          parts+=("\"$s\"")
        done
        json_scopes="[ $(IFS=,; echo "${parts[*]}") ]"
      fi

      cat <<EOF
{
  "promotion": {
    "frontend": $scope_frontend,
    "gateway": $scope_gateway,
    "worker": $scope_worker,
    "schema": $scope_schema,
    "auth_browser": $scope_auth_browser,
    "tts": $scope_tts,
    "bark": $scope_bark,
    "platform": $scope_platform
  },
  "promotion_scope": $json_scopes,
  "doc_only": $all_doc_only,
  "changed_files_count": $total_files,
  "base_ref": "$base_ref",
  "head_ref": "$head_ref"
}
EOF
      ;;
  esac
}

formatted_result="$(format_output)"

if [[ -n "$output_file" ]]; then
  mkdir -p "$(dirname "$output_file")"
  printf '%s\n' "$formatted_result" > "$output_file"
else
  printf '%s\n' "$formatted_result"
fi

exit 0
