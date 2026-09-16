#!/usr/bin/env bash
set -euo pipefail
root="$(mktemp -d)"
trap 'rm -rf "$root"' EXIT
export RUNTIME_ROOT="$root/runtime"
export RELEASE_DIR="$root/releases/rel-test"
mkdir -p "$RUNTIME_ROOT" "$RELEASE_DIR"
source deploy/runtime-layout.sh

expected=(
  "$RUNTIME_ROOT/data/deploy-journal.json"
  "$RUNTIME_ROOT/data/rollout-journal.json"
  "$RUNTIME_ROOT/state/current-release.json"
  "$RUNTIME_ROOT/state/gateway-active-slot"
  "$RUNTIME_ROOT/state/frontend-active-slot"
)
for path in "${expected[@]}"; do
  [[ "$path" != "$RELEASE_DIR"/* ]]
done
[[ "$TX_JOURNAL_FILE" == "$RUNTIME_ROOT/data/deploy-journal.json" ]]
[[ "$ROLLOUT_JOURNAL_FILE" == "$RUNTIME_ROOT/data/rollout-journal.json" ]]
echo "test_runtime_layout passed"
