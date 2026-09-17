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
      while IFS=$'\t' read -r -a tokens; do
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
      while IFS=$'\t' read -r -a tokens; do
        for tok in "${tokens[@]}"; do
          [[ -n "$tok" ]] && raw_paths+=("$tok")
        done
      done <<< "$rest"
    fi
  done < <(git -c core.quotepath=false diff --name-status --find-renames "$base_ref" "$head_ref" 2>/dev/null || true)
fi

# Component flags
scope_frontend=false
scope_gateway=false
scope_worker=false
scope_schema=false
scope_auth_browser=false
scope_tts=false
scope_bark=false
scope_failover_controller=false
scope_platform=false

all_doc_only=true
total_files=0

# Normalize path helper (convert backslashes to forward slashes)
normalize_path() {
  printf '%s' "$1" | tr '\\' '/'
}
component_map="${COMPONENT_MAP_FILE:-$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)/deploy/component-map.json}"
[[ -f "$component_map" ]] || { printf 'Component map not found: %s\n' "$component_map" >&2; exit 1; }

classify_path() {
  local p="$1"
  local result kind components
  result="$(python3 - "$component_map" "$p" <<'PY_MAP'
import json, re, sys
with open(sys.argv[1], encoding='utf-8') as handle:
    mapping = json.load(handle)
path = sys.argv[2]
if any(re.search(pattern, path) for pattern in mapping.get('documentation', [])):
    print('doc:')
    raise SystemExit
matched = []
for rule in mapping.get('rules', []):
    if re.search(rule['pattern'], path):
        matched = list(dict.fromkeys(rule['components']))
        break
if not matched:
    print('unclassified:')
else:
    print('runtime:' + ','.join(matched))
PY_MAP
)"
  kind="${result%%:*}"
  components="${result#*:}"
  if [[ "$kind" == "doc" ]]; then
    if [[ $verbose -eq 1 ]]; then
      printf '  [CLASSIFY] %s -> doc-only\n' "$p" >&2
    fi
    return 0
  fi
  if [[ "$kind" == "unclassified" || -z "$components" ]]; then
    printf 'Unclassified runtime path: %s. Add explicit ownership to %s.\n' "$p" "$component_map" >&2
    exit 1
  fi
  all_doc_only=false
  IFS=',' read -r -a matched_components <<< "$components"
  for component in "${matched_components[@]}"; do
    case "$component" in
      frontend) scope_frontend=true ;;
      gateway) scope_gateway=true ;;
      worker) scope_worker=true ;;
      schema) scope_schema=true ;;
      auth_browser) scope_auth_browser=true ;;
      tts) scope_tts=true ;;
      bark) scope_bark=true ;;
      failover_controller) scope_failover_controller=true ;;
      platform) scope_platform=true ;;
      orchestrator)
        scope_frontend=true
        scope_gateway=true
        scope_worker=true
        scope_schema=true
        scope_auth_browser=true
        scope_tts=true
        scope_bark=true
        scope_failover_controller=true
        scope_platform=true
        ;;
      *) printf 'Unknown component %q in %s\n' "$component" "$component_map" >&2; exit 1 ;;
    esac
  done
  if [[ $verbose -eq 1 ]]; then
    printf '  [CLASSIFY] %s -> %s\n' "$p" "$components" >&2
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
[[ "$scope_failover_controller" == "true" ]] && active_scopes+=("failover_controller")
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
PROMOTION_FAILOVER_CONTROLLER=$scope_failover_controller
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
    "failover_controller": $scope_failover_controller,
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
