#!/usr/bin/env bash
set -Eeuo pipefail

# verify-actions-pinned.sh
# Verifies that every third-party GitHub Action in workflow files is pinned
# to a full 40-character commit SHA rather than a mutable tag or branch.

files=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --file)
      files+=("$2")
      shift 2
      ;;
    --help|-h)
      cat <<'EOF'
Usage: verify-actions-pinned.sh [--file <path>] [files...]

Verifies that all third-party GitHub Action references ('uses:') in workflows
are pinned to full 40-character hexadecimal commit SHAs.
EOF
      exit 0
      ;;
    *)
      files+=("$1")
      shift
      ;;
  esac
done

if [[ ${#files[@]} -eq 0 ]]; then
  shopt -s nullglob
  files=(.github/workflows/*.yml .github/workflows/*.yaml)
  shopt -u nullglob
fi

if [[ ${#files[@]} -eq 0 ]]; then
  printf 'Error: no workflow files found to verify\n' >&2
  exit 1
fi

violations=0
checked=0

for file in "${files[@]}"; do
  if [[ ! -f "$file" ]]; then
    printf 'Error: file not found: %s\n' "$file" >&2
    exit 1
  fi

  line_num=0
  while IFS= read -r raw_line || [[ -n "$raw_line" ]]; do
    line_num=$((line_num + 1))

    # Strip inline comments
    line="${raw_line%%#*}"

    # Match uses: directive
    if [[ "$line" =~ uses:[[:space:]]*[\'\"]?([^\'\"[:space:]]+)[\'\"]? ]]; then
      action="${BASH_REMATCH[1]}"

      # Ignore local repository actions or direct docker references
      if [[ "$action" =~ ^\./ || "$action" =~ ^docker:// ]]; then
        continue
      fi

      checked=$((checked + 1))

      # Require @ delimiter
      if [[ "$action" != *"@"* ]]; then
        printf 'Error: [%s:%d] Action "%s" is not pinned to a commit SHA (missing @)\n' "$file" "$line_num" "$action" >&2
        violations=$((violations + 1))
        continue
      fi

      sha="${action##*@}"
      if [[ ! "$sha" =~ ^[0-9a-fA-F]{40}$ ]]; then
        printf 'Error: [%s:%d] Action "%s" is pinned to "%s", which is not a 40-character commit SHA\n' "$file" "$line_num" "$action" "$sha" >&2
        violations=$((violations + 1))
      fi
    fi
  done < "$file"
done

if [[ $violations -gt 0 ]]; then
  printf '\nVerification failed: %d action(s) unpinned or invalid (checked %d action(s) across %d file(s))\n' "$violations" "$checked" "${#files[@]}" >&2
  exit 1
fi

printf 'Verification passed: all %d action(s) across %d workflow file(s) are pinned to 40-character commit SHAs.\n' "$checked" "${#files[@]}"
exit 0
