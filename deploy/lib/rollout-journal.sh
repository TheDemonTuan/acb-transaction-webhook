#!/usr/bin/env bash
# deploy/lib/rollout-journal.sh
# Canonical Rollout Journal Schema v2 and Lifecycle API.
# shellcheck disable=SC2034

ROLLOUT_JOURNAL_FILE="${ROLLOUT_JOURNAL_FILE:-${RUNTIME_DATA_DIR:-${SCRIPT_DIR:-deploy}/data}/rollout-journal.json}"

init_rollout_journal() {
  local r_id="${1:-rel-$(date +%s)}"
  local git_sha="${2:-}"
  local scope="${3:-all}"
  local cand_dir="${4:-${DEPLOY_DIR:-${SCRIPT_DIR:-deploy}}}"
  local prev_dir="${5:-}"
  local now
  now="$(date -u +'%Y-%m-%dT%H:%M:%SZ')"

  local prev_id="" prev_gen=0
  local current_state="${CURRENT_RELEASE_FILE:-${DEPLOY_PATH:-$(cd -- "${SCRIPT_DIR:-deploy}/.." && pwd)}/state/current-release.json}"
  if [[ -f "$current_state" ]]; then
    {
      read -r prev_id || true
      read -r p_dir || true
      read -r prev_gen || true
    } < <(python3 - "$current_state" <<'PY' 2>/dev/null || true
import json, sys
try:
    d = json.load(open(sys.argv[1]))
    print(d.get("release_id") or "")
    print(d.get("release_dir") or "")
    print(d.get("generation") or 0)
except Exception:
    pass
PY
)
    if [[ -z "$prev_dir" && -n "${p_dir:-}" ]]; then
      prev_dir="$p_dir"
    fi
  fi
  [[ -z "$prev_gen" ]] && prev_gen=0

  mkdir -p "$(dirname "$ROLLOUT_JOURNAL_FILE")"
  local tmp
  tmp="$(mktemp "$(dirname "$ROLLOUT_JOURNAL_FILE")/.rollout-init.XXXXXX")"

  R_ID="$r_id" R_GIT_SHA="$git_sha" R_SCOPE="$scope" R_NOW="$now" \
  R_CAND_DIR="$cand_dir" R_PREV_DIR="$prev_dir" R_PREV_ID="$prev_id" R_PREV_GEN="$prev_gen" \
  python3 - <<'PY_JSON' > "$tmp"
import json, os

standard_steps = [
    "schema",
    "auth_browser",
    "tts",
    "bark",
    "frontend",
    "worker",
    "gateway",
    "failover_controller",
    "platform",
    "release_commit"
]

steps = {s: "NOT_STARTED" for s in standard_steps}

data = {
    "schema_version": 2,
    "rollout_id": os.environ["R_ID"],
    "git_sha": os.environ["R_GIT_SHA"],
    "status": "RUNNING",
    "scope": os.environ["R_SCOPE"],
    "started_at": os.environ["R_NOW"],
    "updated_at": os.environ["R_NOW"],
    "candidate_release_dir": os.environ.get("R_CAND_DIR", ""),
    "previous_release_dir": os.environ.get("R_PREV_DIR", ""),
    "previous_release_id": os.environ.get("R_PREV_ID", ""),
    "previous_generation": int(os.environ.get("R_PREV_GEN") or 0),
    "current_step": "INITIALIZED",
    "steps": steps
}
print(json.dumps(data, indent=2, sort_keys=True))
PY_JSON

  atomic_write_file "$ROLLOUT_JOURNAL_FILE" 600 < "$tmp"
  rm -f "$tmp"
}

update_rollout_step() {
  local step="$1"
  local status="$2"
  [[ -f "$ROLLOUT_JOURNAL_FILE" ]] || return 0
  local now tmp
  now="$(date -u +'%Y-%m-%dT%H:%M:%SZ')"
  tmp="$(mktemp "$(dirname "$ROLLOUT_JOURNAL_FILE")/.rollout-update.XXXXXX")"

  R_FILE="$ROLLOUT_JOURNAL_FILE" R_STEP="$step" R_STATUS="$status" R_NOW="$now" python3 - <<'PY_JSON' > "$tmp"
import json, os
with open(os.environ["R_FILE"], encoding="utf-8") as handle:
    data = json.load(handle)

step = os.environ["R_STEP"]
status = os.environ["R_STATUS"]
data["current_step"] = step
data["updated_at"] = os.environ["R_NOW"]

steps = data.setdefault("steps", {})
steps[step] = status

if status == "STEP_FAILED":
    data["status"] = "FAILED"

print(json.dumps(data, indent=2, sort_keys=True))
PY_JSON

  atomic_write_file "$ROLLOUT_JOURNAL_FILE" 600 < "$tmp"
  rm -f "$tmp"
}

finish_rollout_journal() {
  local final_status="$1"
  local evidence_dir="${2:-}"
  [[ -f "$ROLLOUT_JOURNAL_FILE" ]] || return 0
  local now tmp
  now="$(date -u +'%Y-%m-%dT%H:%M:%SZ')"
  tmp="$(mktemp "$(dirname "$ROLLOUT_JOURNAL_FILE")/.rollout-finish.XXXXXX")"

  R_FILE="$ROLLOUT_JOURNAL_FILE" R_STATUS="$final_status" R_NOW="$now" python3 - <<'PY_JSON' > "$tmp"
import json, os
with open(os.environ["R_FILE"], encoding="utf-8") as handle:
    data = json.load(handle)
data["status"] = os.environ["R_STATUS"]
data["updated_at"] = os.environ["R_NOW"]
print(json.dumps(data, indent=2, sort_keys=True))
PY_JSON

  atomic_write_file "$ROLLOUT_JOURNAL_FILE" 600 < "$tmp"
  rm -f "$tmp"

  if [[ -n "$evidence_dir" && -d "$evidence_dir" ]]; then
    atomic_write_file "$evidence_dir/rollout-journal.json" 600 < "$ROLLOUT_JOURNAL_FILE"
  fi
  if [[ "$final_status" == "COMPLETED" || "$final_status" == "DOC_ONLY" ]]; then
    atomic_write_file "${ROLLOUT_JOURNAL_FILE}.previous" 600 < "$ROLLOUT_JOURNAL_FILE"
    rm -f "$ROLLOUT_JOURNAL_FILE"
  fi
}

step_was_completed() {
  local step="$1"
  local journal="${2:-$ROLLOUT_JOURNAL_FILE}"
  if [[ ! -f "$journal" ]]; then
    return 1
  fi
  python3 - "$journal" "$step" <<'PY' 2>/dev/null
import json, sys
try:
    data = json.load(open(sys.argv[1], encoding="utf-8"))
    target = sys.argv[2]
    # Schema v2: steps dictionary
    steps = data.get("steps")
    if isinstance(steps, dict):
        if steps.get(target) == "STEP_COMPLETED":
            sys.exit(0)
    # Compatibility reader for legacy completed_steps array
    completed = data.get("completed_steps")
    if isinstance(completed, list):
        if target in completed:
            sys.exit(0)
    sys.exit(1)
except Exception:
    sys.exit(1)
PY
}

rollout_step_completed() {
  step_was_completed "$1" "${2:-$ROLLOUT_JOURNAL_FILE}"
}

rollout_journal_git_sha() {
  local journal="${1:-$ROLLOUT_JOURNAL_FILE}"
  if [[ ! -f "$journal" ]]; then
    echo ""
    return 0
  fi
  python3 - "$journal" <<'PY' 2>/dev/null
import json, sys
try:
    data = json.load(open(sys.argv[1], encoding="utf-8"))
    print(data.get("git_sha", ""))
except Exception:
    pass
PY
}

rollout_journal_get() {
  local field="$1"
  local journal="${2:-$ROLLOUT_JOURNAL_FILE}"
  if [[ ! -f "$journal" ]]; then
    echo ""
    return 0
  fi
  python3 - "$journal" "$field" <<'PY' 2>/dev/null
import json, sys
try:
    data = json.load(open(sys.argv[1], encoding="utf-8"))
    val = data.get(sys.argv[2])
    print("" if val is None else str(val))
except Exception:
    pass
PY
}
