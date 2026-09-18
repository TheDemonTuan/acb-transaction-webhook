#!/usr/bin/env bash
set -Eeuo pipefail

# compute-promotion-scope.sh
# Analyzes changed, deleted, and renamed paths between a base reference (e.g.
# last successful release) and candidate commit to determine which components
# must be promoted.

base_ref="${LAST_RELEASE_COMMIT:-}"
head_ref="${GITHUB_SHA:-HEAD}"
files_from=""
component_map="${COMPONENT_MAP_FILE:-}"
base_component_map="${BASE_COMPONENT_MAP_FILE:-}"
format="json"
output_file=""
verbose=0

usage() {
  cat <<'EOF'
Usage: compute-promotion-scope.sh [options]

Options:
  --base <ref>                Verified production base ref (required unless LAST_RELEASE_COMMIT is set)
  --head <ref>                Head git ref/commit (default: GITHUB_SHA or HEAD)
  --files-from <path>         Read changed paths from file (supports name-status format)
  --component-map <path>      Path to component-map.json for head/candidate (default: deploy/component-map.json)
  --base-component-map <path> Path to component-map.json for base release
  --format <json|env|list>    Output format (default: json)
  --output <path>             Write output to file instead of stdout
  --verbose, -v               Print classification details to stderr
  --help, -h                  Show this help message
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
    --component-map)
      component_map="$2"
      shift 2
      ;;
    --base-component-map)
      base_component_map="$2"
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

if [[ -z "$component_map" ]]; then
  component_map="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)/deploy/component-map.json"
fi
[[ -f "$component_map" ]] || { printf 'Component map not found: %s\n' "$component_map" >&2; exit 1; }

tmp_base_map=""
cleanup() {
  if [[ -n "$tmp_base_map" && -f "$tmp_base_map" ]]; then
    rm -f "$tmp_base_map"
  fi
}
trap cleanup EXIT

if [[ -z "$base_component_map" ]]; then
  if [[ -n "$base_ref" ]] && command -v git >/dev/null 2>&1 && git cat-file -e "${base_ref}:deploy/component-map.json" 2>/dev/null; then
    tmp_base_map="$(mktemp)"
    git show "${base_ref}:deploy/component-map.json" > "$tmp_base_map"
    base_component_map="$tmp_base_map"
  else
    base_component_map="$component_map"
  fi
fi
[[ -f "$base_component_map" ]] || { printf 'Base component map not found: %s\n' "$base_component_map" >&2; exit 1; }

items_to_classify=()

process_change_line() {
  local line="$1"
  line="$(printf '%s' "$line" | tr -d '\r')"
  [[ -z "$line" ]] && return 0
  if [[ "$line" =~ ^([A-Z][0-9]*)[[:space:]]+(.+)$ ]]; then
    local status="${BASH_REMATCH[1]}"
    local rest="${BASH_REMATCH[2]}"
    if [[ "$status" =~ ^[RC] ]]; then
      local old_p new_p
      if [[ "$rest" == *$'\t'* ]]; then
        old_p="${rest%%$'\t'*}"
        new_p="${rest#*$'\t'}"
      else
        old_p="$(awk '{print $1}' <<< "$rest")"
        new_p="$(awk '{print $2}' <<< "$rest")"
      fi
      [[ -n "$old_p" ]] && items_to_classify+=("BASE"$'\t'"$old_p")
      [[ -n "$new_p" ]] && items_to_classify+=("HEAD"$'\t'"$new_p")
    elif [[ "$status" =~ ^D ]]; then
      [[ -n "$rest" ]] && items_to_classify+=("BASE"$'\t'"$rest")
    else
      [[ -n "$rest" ]] && items_to_classify+=("HEAD"$'\t'"$rest")
    fi
  else
    items_to_classify+=("HEAD"$'\t'"$line")
  fi
}

if [[ -n "$files_from" ]]; then
  if [[ ! -f "$files_from" ]]; then
    printf 'Error: files-from path not found: %s\n' "$files_from" >&2
    exit 1
  fi
  while IFS= read -r line || [[ -n "$line" ]]; do
    process_change_line "$line"
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
    process_change_line "$line"
  done < <(git -c core.quotepath=false diff --name-status --find-renames "$base_ref" "$head_ref" 2>/dev/null || true)
fi

# Run classifier in Python
py_output="$(
  if [[ ${#items_to_classify[@]} -gt 0 ]]; then
    printf '%s\n' "${items_to_classify[@]}"
  fi | python3 -c '
import json, re, sys

head_map_path = sys.argv[1]
base_map_path = sys.argv[2]
display_map_path = sys.argv[3]
verbose = (sys.argv[4] == "1")

with open(head_map_path, encoding="utf-8") as f:
    head_map = json.load(f)

if base_map_path == head_map_path:
    base_map = head_map
else:
    try:
        with open(base_map_path, encoding="utf-8") as f:
            base_map = json.load(f)
    except Exception:
        base_map = head_map

VALID_COMPONENTS = {
    "frontend", "gateway", "worker", "schema",
    "auth_browser", "tts", "bark", "failover_controller", "platform", "orchestrator"
}

def classify(path, mapping):
    for pattern in mapping.get("documentation", []):
        if re.search(pattern, path):
            return ("doc", [])
    for rule in mapping.get("rules", []):
        if re.search(rule["pattern"], path):
            return ("runtime", list(dict.fromkeys(rule["components"])))
    return ("unclassified", [])

all_doc_only = True
matched_scopes = set()
count = 0

for raw_line in sys.stdin:
    line = raw_line.rstrip("\r\n")
    if not line:
        continue
    parts = line.split("\t", 1)
    if len(parts) == 2:
        target, p = parts[0], parts[1]
    else:
        target, p = "HEAD", parts[0]
    p = p.replace("\\", "/")
    if not p:
        continue
    count += 1

    primary_map = base_map if target == "BASE" else head_map
    kind, components = classify(p, primary_map)

    # If unclassified in BASE map, fallback to HEAD map
    if kind == "unclassified" and target == "BASE":
        kind, components = classify(p, head_map)

    if kind == "doc":
        if verbose:
            sys.stderr.write(f"  [CLASSIFY] {p} -> doc-only\n")
    elif kind == "runtime":
        all_doc_only = False
        for c in components:
            if c not in VALID_COMPONENTS:
                sys.stderr.write(f"Unknown component {c!r} in {display_map_path}\n")
                sys.exit(1)
            if c == "orchestrator":
                matched_scopes.update([
                    "frontend", "gateway", "worker", "schema",
                    "auth_browser", "tts", "bark", "failover_controller", "platform"
                ])
            else:
                matched_scopes.add(c)
        if verbose:
            comp_str = ",".join(components)
            sys.stderr.write(f"  [CLASSIFY] {p} -> {comp_str}\n")
    else:
        sys.stderr.write(f"Unclassified runtime path: {p}. Add explicit ownership to {display_map_path}.\n")
        sys.exit(1)

if count == 0 or all_doc_only:
    all_doc_only = True
    matched_scopes = set()

scope_str = ",".join(sorted(list(matched_scopes)))
doc_str = "true" if all_doc_only else "false"
print(f"TOTAL_FILES={count}")
print(f"DOC_ONLY={doc_str}")
print(f"SCOPES={scope_str}")
' "$component_map" "$base_component_map" "$component_map" "$verbose"
)"

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

while IFS='=' read -r k v; do
  case "$k" in
    TOTAL_FILES) total_files="$v" ;;
    DOC_ONLY) all_doc_only="$v" ;;
    SCOPES)
      if [[ -n "$v" ]]; then
        IFS=',' read -r -a parsed_scopes <<< "$v"
        for s in "${parsed_scopes[@]}"; do
          case "$s" in
            frontend) scope_frontend=true ;;
            gateway) scope_gateway=true ;;
            worker) scope_worker=true ;;
            schema) scope_schema=true ;;
            auth_browser) scope_auth_browser=true ;;
            tts) scope_tts=true ;;
            bark) scope_bark=true ;;
            failover_controller) scope_failover_controller=true ;;
            platform) scope_platform=true ;;
          esac
        done
      fi
      ;;
  esac
done <<< "$py_output"

# Build list of active scopes in canonical order
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
