#!/usr/bin/env bash
# deploy/tests/test_component_map_coverage.sh
# Tests that every tracked file in repository is covered by component-map.json (Task 9).
set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "$SCRIPT_DIR/../.." && pwd)"

cd "$REPO_ROOT"

failures=0
checked=0

tmp_f="$(mktemp)"
trap 'rm -f "$tmp_f"' EXIT

while IFS= read -r path; do
  [[ -n "$path" ]] || continue
  checked=$((checked + 1))
  printf 'M\t%s\n' "$path" > "$tmp_f"
  if ! scripts/compute-promotion-scope.sh --files-from "$tmp_f" --format list >/dev/null 2>&1; then
    printf 'FAIL: Unclassified path: %s\n' "$path" >&2
    failures=$((failures + 1))
  fi
done < <(git -c core.quotepath=false ls-files)

if (( failures > 0 )); then
  printf 'Component map coverage failed: %d unclassified file(s) out of %d checked.\n' "$failures" "$checked" >&2
  exit 1
fi

printf 'PASS: All %d tracked files are classified in component-map.json.\n' "$checked"
exit 0
