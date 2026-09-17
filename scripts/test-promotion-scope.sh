#!/usr/bin/env bash
set -Eeuo pipefail

# test-promotion-scope.sh
# Table-driven test suite for component promotion scope classifier

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
classifier="$script_dir/compute-promotion-scope.sh"

test_tmp="$(mktemp -d)"
trap 'rm -rf "$test_tmp"' EXIT

pass_count=0
fail_count=0

assert_eq() {
  local desc="$1"
  local actual="$2"
  local expected="$3"
  if [[ "$actual" == "$expected" ]]; then
    printf '  [PASS] %s\n' "$desc"
    pass_count=$((pass_count + 1))
  else
    printf '  [FAIL] %s (expected "%s", got "%s")\n' "$desc" "$expected" "$actual" >&2
    fail_count=$((fail_count + 1))
  fi
}

printf "========================================\n"
printf "Running Promotion Scope Classifier Tests\n"
printf "========================================\n\n"

# Helper to run classifier on a list of changed lines
run_case() {
  local file="$1"
  shift
  bash "$classifier" --files-from "$file" "$@"
}

# 1. Documentation-only change
printf "1. Testing documentation-only changes...\n"
cat <<'EOF' > "$test_tmp/doc_only.txt"
M	docs/architecture/COMPATIBILITY_CONTRACTS.md
M	README.md
A	docs/runbooks/TEST.md
M	.github/dependabot.yml
EOF
doc_out="$(run_case "$test_tmp/doc_only.txt" --format env)"
assert_eq "doc-only: PROMOTION_FRONTEND is false" "$(printf '%s' "$doc_out" | grep '^PROMOTION_FRONTEND=' | cut -d= -f2)" "false"
assert_eq "doc-only: PROMOTION_GATEWAY is false" "$(printf '%s' "$doc_out" | grep '^PROMOTION_GATEWAY=' | cut -d= -f2)" "false"
assert_eq "doc-only: PROMOTION_WORKER is false" "$(printf '%s' "$doc_out" | grep '^PROMOTION_WORKER=' | cut -d= -f2)" "false"
assert_eq "doc-only: PROMOTION_SCHEMA is false" "$(printf '%s' "$doc_out" | grep '^PROMOTION_SCHEMA=' | cut -d= -f2)" "false"
assert_eq "doc-only: PROMOTION_DOC_ONLY is true" "$(printf '%s' "$doc_out" | grep '^PROMOTION_DOC_ONLY=' | cut -d= -f2)" "true"
assert_eq "doc-only: PROMOTION_SCOPE is empty" "$(printf '%s' "$doc_out" | grep '^PROMOTION_SCOPE=' | cut -d= -f2)" ""

# 2. Frontend-only change
printf "\n2. Testing frontend-only changes...\n"
cat <<'EOF' > "$test_tmp/frontend_only.txt"
M	web/src/App.tsx
M	web/src/styles.css
EOF
frontend_out="$(run_case "$test_tmp/frontend_only.txt" --format env)"
assert_eq "frontend-only: PROMOTION_FRONTEND is true" "$(printf '%s' "$frontend_out" | grep '^PROMOTION_FRONTEND=' | cut -d= -f2)" "true"
assert_eq "frontend-only: PROMOTION_GATEWAY is false" "$(printf '%s' "$frontend_out" | grep '^PROMOTION_GATEWAY=' | cut -d= -f2)" "false"
assert_eq "frontend-only: PROMOTION_WORKER is false" "$(printf '%s' "$frontend_out" | grep '^PROMOTION_WORKER=' | cut -d= -f2)" "false"

# 3. Gateway-only change
printf "\n3. Testing gateway-only changes...\n"
cat <<'EOF' > "$test_tmp/gateway_only.txt"
M	cmd/gateway/main.go
M	internal/httpapi/routes.go
EOF
gw_out="$(run_case "$test_tmp/gateway_only.txt" --format env)"
assert_eq "gateway: PROMOTION_GATEWAY is true" "$(printf '%s' "$gw_out" | grep '^PROMOTION_GATEWAY=' | cut -d= -f2)" "true"
assert_eq "gateway: PROMOTION_WORKER is false" "$(printf '%s' "$gw_out" | grep '^PROMOTION_WORKER=' | cut -d= -f2)" "false"
assert_eq "gateway: PROMOTION_SCHEMA is false" "$(printf '%s' "$gw_out" | grep '^PROMOTION_SCHEMA=' | cut -d= -f2)" "false"
assert_eq "gateway: PROMOTION_SCOPE is gateway" "$(printf '%s' "$gw_out" | grep '^PROMOTION_SCOPE=' | cut -d= -f2)" "gateway"

# 3. Worker-only change
printf "\n3. Testing worker-only changes...\n"
cat <<'EOF' > "$test_tmp/worker_only.txt"
M	cmd/worker/main.go
M	internal/maintenance/cleanup.go
M	internal/workerstate/coordinator.go
EOF
w_out="$(run_case "$test_tmp/worker_only.txt" --format env)"
assert_eq "worker: PROMOTION_GATEWAY is false" "$(printf '%s' "$w_out" | grep '^PROMOTION_GATEWAY=' | cut -d= -f2)" "false"
assert_eq "worker: PROMOTION_WORKER is true" "$(printf '%s' "$w_out" | grep '^PROMOTION_WORKER=' | cut -d= -f2)" "true"
assert_eq "worker: PROMOTION_SCHEMA is false" "$(printf '%s' "$w_out" | grep '^PROMOTION_SCHEMA=' | cut -d= -f2)" "false"
assert_eq "worker: PROMOTION_SCOPE is worker" "$(printf '%s' "$w_out" | grep '^PROMOTION_SCOPE=' | cut -d= -f2)" "worker"

# Shared polling/runtime packages are imported by both gateway and worker.
cat <<'EOF' > "$test_tmp/polling_shared.txt"
M	internal/monitor/realtime_task.go
M	internal/acb/client.go
M	internal/scheduler/scheduler.go
EOF
poll_out="$(run_case "$test_tmp/polling_shared.txt" --format env)"
assert_eq "shared polling: PROMOTION_GATEWAY is true" "$(printf '%s' "$poll_out" | grep '^PROMOTION_GATEWAY=' | cut -d= -f2)" "true"
assert_eq "shared polling: PROMOTION_WORKER is true" "$(printf '%s' "$poll_out" | grep '^PROMOTION_WORKER=' | cut -d= -f2)" "true"

# 4. Shared RPC change (gateway + worker)
printf "\n4. Testing shared workerrpc changes...\n"
cat <<'EOF' > "$test_tmp/rpc_shared.txt"
M	internal/workerrpc/rpc.go
EOF
rpc_out="$(run_case "$test_tmp/rpc_shared.txt" --format env)"
assert_eq "shared RPC: PROMOTION_GATEWAY is true" "$(printf '%s' "$rpc_out" | grep '^PROMOTION_GATEWAY=' | cut -d= -f2)" "true"
assert_eq "shared RPC: PROMOTION_WORKER is true" "$(printf '%s' "$rpc_out" | grep '^PROMOTION_WORKER=' | cut -d= -f2)" "true"
assert_eq "shared RPC: PROMOTION_SCHEMA is false" "$(printf '%s' "$rpc_out" | grep '^PROMOTION_SCHEMA=' | cut -d= -f2)" "false"

# 5. Schema migration change (schema + gateway + worker)
printf "\n5. Testing schema migration changes...\n"
cat <<'EOF' > "$test_tmp/schema_migration.txt"
A	internal/storage/migrations/010_new_table.sql
EOF
schema_out="$(run_case "$test_tmp/schema_migration.txt" --format env)"
assert_eq "schema: PROMOTION_SCHEMA is true" "$(printf '%s' "$schema_out" | grep '^PROMOTION_SCHEMA=' | cut -d= -f2)" "true"
assert_eq "schema: PROMOTION_GATEWAY is true" "$(printf '%s' "$schema_out" | grep '^PROMOTION_GATEWAY=' | cut -d= -f2)" "true"
assert_eq "schema: PROMOTION_WORKER is true" "$(printf '%s' "$schema_out" | grep '^PROMOTION_WORKER=' | cut -d= -f2)" "true"

# 6. Shared config package change
printf "\n6. Testing shared internal/config changes...\n"
cat <<'EOF' > "$test_tmp/config_shared.txt"
M	internal/config/config.go
EOF
cfg_out="$(run_case "$test_tmp/config_shared.txt" --format env)"
assert_eq "config: PROMOTION_GATEWAY is true" "$(printf '%s' "$cfg_out" | grep '^PROMOTION_GATEWAY=' | cut -d= -f2)" "true"
assert_eq "config: PROMOTION_WORKER is true" "$(printf '%s' "$cfg_out" | grep '^PROMOTION_WORKER=' | cut -d= -f2)" "true"

# 7. Shared root dependencies (go.mod)
printf "\n7. Testing root go.mod change...\n"
cat <<'EOF' > "$test_tmp/gomod_shared.txt"
M	go.mod
M	go.sum
EOF
mod_out="$(run_case "$test_tmp/gomod_shared.txt" --format env)"
assert_eq "go.mod: PROMOTION_GATEWAY is true" "$(printf '%s' "$mod_out" | grep '^PROMOTION_GATEWAY=' | cut -d= -f2)" "true"
assert_eq "go.mod: PROMOTION_WORKER is true" "$(printf '%s' "$mod_out" | grep '^PROMOTION_WORKER=' | cut -d= -f2)" "true"
assert_eq "go.mod: PROMOTION_SCHEMA is true" "$(printf '%s' "$mod_out" | grep '^PROMOTION_SCHEMA=' | cut -d= -f2)" "true"

# 8. Auth-browser sidecar
printf "\n8. Testing auth-browser changes...\n"
cat <<'EOF' > "$test_tmp/auth_browser.txt"
M	Dockerfile.auth-browser
M	cmd/auth-browser/main.go
EOF
ab_out="$(run_case "$test_tmp/auth_browser.txt" --format env)"
assert_eq "auth-browser: PROMOTION_AUTH_BROWSER is true" "$(printf '%s' "$ab_out" | grep '^PROMOTION_AUTH_BROWSER=' | cut -d= -f2)" "true"
assert_eq "auth-browser: PROMOTION_GATEWAY is false" "$(printf '%s' "$ab_out" | grep '^PROMOTION_GATEWAY=' | cut -d= -f2)" "false"

# 9. TTS sidecar
printf "\n9. Testing tts-gateway changes...\n"
cat <<'EOF' > "$test_tmp/tts.txt"
M	tts-gateway/server.py
EOF
tts_out="$(run_case "$test_tmp/tts.txt" --format env)"
assert_eq "tts: PROMOTION_TTS is true" "$(printf '%s' "$tts_out" | grep '^PROMOTION_TTS=' | cut -d= -f2)" "true"
assert_eq "tts: PROMOTION_GATEWAY is false" "$(printf '%s' "$tts_out" | grep '^PROMOTION_GATEWAY=' | cut -d= -f2)" "false"

# 10. Bark policy change
printf "\n10. Testing bark third-party policy changes...\n"
cat <<'EOF' > "$test_tmp/bark.txt"
M	deploy/third-party-allowlist.json
EOF
bark_out="$(run_case "$test_tmp/bark.txt" --format env)"
assert_eq "bark: PROMOTION_BARK is true" "$(printf '%s' "$bark_out" | grep '^PROMOTION_BARK=' | cut -d= -f2)" "true"
assert_eq "bark: PROMOTION_GATEWAY is false" "$(printf '%s' "$bark_out" | grep '^PROMOTION_GATEWAY=' | cut -d= -f2)" "false"

# 11. Deleted and renamed file detection
printf "\n11. Testing deleted and renamed files...\n"
cat <<'EOF' > "$test_tmp/deleted_and_renamed.txt"
D	cmd/gateway/old_file.go
R100	cmd/gateway/foo.go	cmd/gateway/bar.go
EOF
del_out="$(run_case "$test_tmp/deleted_and_renamed.txt" --format env)"
assert_eq "deleted/renamed: PROMOTION_GATEWAY is true" "$(printf '%s' "$del_out" | grep '^PROMOTION_GATEWAY=' | cut -d= -f2)" "true"
assert_eq "deleted/renamed: PROMOTION_WORKER is false" "$(printf '%s' "$del_out" | grep '^PROMOTION_WORKER=' | cut -d= -f2)" "false"

# 12. JSON format schema verification
printf "\n12. Testing JSON output schema...\n"
json_out="$(run_case "$test_tmp/gateway_only.txt" --format json)"
if command -v node >/dev/null 2>&1; then
  node - "$json_out" <<'JSEOF'
const data = JSON.parse(process.argv[2]);
if (!data.promotion || typeof data.promotion !== 'object') throw new Error('missing promotion');
if (data.promotion.gateway !== true) throw new Error('gateway must be true');
if (data.promotion.worker !== false) throw new Error('worker must be false');
if (!Array.isArray(data.promotion_scope)) throw new Error('promotion_scope must be array');
if (!data.promotion_scope.includes('gateway')) throw new Error('promotion_scope must contain gateway');
JSEOF
  printf '  [PASS] JSON structure parsed and verified with node\n'
  pass_count=$((pass_count + 1))
fi

# 13. Platform-only changes
printf "\n13. Testing platform-only changes...\n"
cat <<'EOF' > "$test_tmp/platform_only.txt"
M	platform/failover/vps-failover-controller.py
M	platform/edge/haproxy.cfg
EOF
plat_out="$(run_case "$test_tmp/platform_only.txt" --format env)"
assert_eq "platform: PROMOTION_PLATFORM is true" "$(printf '%s' "$plat_out" | grep '^PROMOTION_PLATFORM=' | cut -d= -f2)" "true"
assert_eq "failover: PROMOTION_FAILOVER_CONTROLLER is true" "$(printf '%s' "$plat_out" | grep '^PROMOTION_FAILOVER_CONTROLLER=' | cut -d= -f2)" "true"
assert_eq "platform: PROMOTION_GATEWAY is false" "$(printf '%s' "$plat_out" | grep '^PROMOTION_GATEWAY=' | cut -d= -f2)" "false"
assert_eq "platform: PROMOTION_WORKER is false" "$(printf '%s' "$plat_out" | grep '^PROMOTION_WORKER=' | cut -d= -f2)" "false"
assert_eq "platform: PROMOTION_DOC_ONLY is false" "$(printf '%s' "$plat_out" | grep '^PROMOTION_DOC_ONLY=' | cut -d= -f2)" "false"

# 14. Mixed: worker + auth-browser
printf "\n14. Testing mixed worker + auth-browser...\n"
cat <<'EOF' > "$test_tmp/worker_and_browser.txt"
M	cmd/worker/main.go
M	cmd/auth-browser/main.go
EOF
wb_out="$(run_case "$test_tmp/worker_and_browser.txt" --format env)"
assert_eq "worker+browser: PROMOTION_WORKER is true" "$(printf '%s' "$wb_out" | grep '^PROMOTION_WORKER=' | cut -d= -f2)" "true"
assert_eq "worker+browser: PROMOTION_AUTH_BROWSER is true" "$(printf '%s' "$wb_out" | grep '^PROMOTION_AUTH_BROWSER=' | cut -d= -f2)" "true"
assert_eq "worker+browser: PROMOTION_GATEWAY is false" "$(printf '%s' "$wb_out" | grep '^PROMOTION_GATEWAY=' | cut -d= -f2)" "false"

# 15. Mixed: gateway + tts
printf "\n15. Testing mixed gateway + tts...\n"
cat <<'EOF' > "$test_tmp/gw_and_tts.txt"
M	cmd/gateway/main.go
M	tts-gateway/server.py
EOF
gt_out="$(run_case "$test_tmp/gw_and_tts.txt" --format env)"
assert_eq "gw+tts: PROMOTION_GATEWAY is true" "$(printf '%s' "$gt_out" | grep '^PROMOTION_GATEWAY=' | cut -d= -f2)" "true"
assert_eq "gw+tts: PROMOTION_TTS is true" "$(printf '%s' "$gt_out" | grep '^PROMOTION_TTS=' | cut -d= -f2)" "true"
assert_eq "gw+tts: PROMOTION_WORKER is false" "$(printf '%s' "$gt_out" | grep '^PROMOTION_WORKER=' | cut -d= -f2)" "false"

# 16. Mixed: schema + bark
printf "\n16. Testing mixed schema + bark...\n"
cat <<'EOF' > "$test_tmp/schema_and_bark.txt"
A	internal/storage/migrations/011_custom_alerts.sql
M	deploy/third-party-allowlist.json
EOF
sb_out="$(run_case "$test_tmp/schema_and_bark.txt" --format env)"
assert_eq "schema+bark: PROMOTION_SCHEMA is true" "$(printf '%s' "$sb_out" | grep '^PROMOTION_SCHEMA=' | cut -d= -f2)" "true"
assert_eq "schema+bark: PROMOTION_BARK is true" "$(printf '%s' "$sb_out" | grep '^PROMOTION_BARK=' | cut -d= -f2)" "true"
assert_eq "schema+bark: PROMOTION_GATEWAY is true" "$(printf '%s' "$sb_out" | grep '^PROMOTION_GATEWAY=' | cut -d= -f2)" "true"
assert_eq "schema+bark: PROMOTION_WORKER is true" "$(printf '%s' "$sb_out" | grep '^PROMOTION_WORKER=' | cut -d= -f2)" "true"

# 17. Full multi-component stack
printf "\n17. Testing full stack promotion...\n"
cat <<'EOF' > "$test_tmp/full_stack.txt"
M	cmd/gateway/main.go
M	cmd/worker/main.go
A	internal/storage/migrations/012_full.sql
M	cmd/auth-browser/main.go
M	tts-gateway/server.py
M	deploy/third-party-allowlist.json
M	platform/edge/haproxy.cfg
EOF
full_out="$(run_case "$test_tmp/full_stack.txt" --format env)"
assert_eq "full-stack: PROMOTION_GATEWAY is true" "$(printf '%s' "$full_out" | grep '^PROMOTION_GATEWAY=' | cut -d= -f2)" "true"
assert_eq "full-stack: PROMOTION_WORKER is true" "$(printf '%s' "$full_out" | grep '^PROMOTION_WORKER=' | cut -d= -f2)" "true"
assert_eq "full-stack: PROMOTION_SCHEMA is true" "$(printf '%s' "$full_out" | grep '^PROMOTION_SCHEMA=' | cut -d= -f2)" "true"
assert_eq "full-stack: PROMOTION_AUTH_BROWSER is true" "$(printf '%s' "$full_out" | grep '^PROMOTION_AUTH_BROWSER=' | cut -d= -f2)" "true"
assert_eq "full-stack: PROMOTION_TTS is true" "$(printf '%s' "$full_out" | grep '^PROMOTION_TTS=' | cut -d= -f2)" "true"
assert_eq "full-stack: PROMOTION_BARK is true" "$(printf '%s' "$full_out" | grep '^PROMOTION_BARK=' | cut -d= -f2)" "true"
assert_eq "full-stack: PROMOTION_PLATFORM is true" "$(printf '%s' "$full_out" | grep '^PROMOTION_PLATFORM=' | cut -d= -f2)" "true"
assert_eq "full-stack: PROMOTION_DOC_ONLY is false" "$(printf '%s' "$full_out" | grep '^PROMOTION_DOC_ONLY=' | cut -d= -f2)" "false"

# 18. Docs modified alongside code (code scope preserved, doc-only is false)
printf "\n18. Testing doc modified alongside code...\n"
cat <<'EOF' > "$test_tmp/code_and_doc.txt"
M	README.md
M	cmd/gateway/main.go
EOF
cd_out="$(run_case "$test_tmp/code_and_doc.txt" --format env)"
assert_eq "code+doc: PROMOTION_DOC_ONLY is false" "$(printf '%s' "$cd_out" | grep '^PROMOTION_DOC_ONLY=' | cut -d= -f2)" "false"
assert_eq "code+doc: PROMOTION_GATEWAY is true" "$(printf '%s' "$cd_out" | grep '^PROMOTION_GATEWAY=' | cut -d= -f2)" "true"
assert_eq "code+doc: PROMOTION_WORKER is false" "$(printf '%s' "$cd_out" | grep '^PROMOTION_WORKER=' | cut -d= -f2)" "false"

# 19. Unclassified unknown path fails closed
printf "\n19. Testing unknown path fails closed...\n"
cat <<'EOF' > "$test_tmp/unknown_path.txt"
M	unknown/mystery_tool.go
EOF
set +e
unk_out="$(run_case "$test_tmp/unknown_path.txt" --format env 2>&1)"
unk_rc=$?
set -e
assert_eq "unknown: classifier exits nonzero" "$(( unk_rc != 0 ? 1 : 0 ))" "1"
assert_eq "unknown: error identifies path" "$([[ "$unk_out" == *"unknown/mystery_tool.go"* ]] && echo true || echo false)" "true"

# 20. Empty changed files list
printf "\n20. Testing empty file changes...\n"
touch "$test_tmp/empty.txt"
emp_out="$(run_case "$test_tmp/empty.txt" --format env)"
assert_eq "empty: PROMOTION_DOC_ONLY is true" "$(printf '%s' "$emp_out" | grep '^PROMOTION_DOC_ONLY=' | cut -d= -f2)" "true"
assert_eq "empty: PROMOTION_GATEWAY is false" "$(printf '%s' "$emp_out" | grep '^PROMOTION_GATEWAY=' | cut -d= -f2)" "false"
assert_eq "empty: PROMOTION_WORKER is false" "$(printf '%s' "$emp_out" | grep '^PROMOTION_WORKER=' | cut -d= -f2)" "false"

# 21. Testing compose fragment ownership
printf "\n21. Testing compose fragment ownership...\n"
printf "M\tdeploy/compose/worker.yaml\n" > "$test_tmp/compose_worker.txt"
cw_out="$(run_case "$test_tmp/compose_worker.txt" --format env)"
assert_eq "compose-worker: PROMOTION_WORKER is true" "$(printf '%s' "$cw_out" | grep '^PROMOTION_WORKER=' | cut -d= -f2)" "true"
assert_eq "compose-worker: PROMOTION_GATEWAY is false" "$(printf '%s' "$cw_out" | grep '^PROMOTION_GATEWAY=' | cut -d= -f2)" "false"
assert_eq "compose-worker: PROMOTION_FRONTEND is false" "$(printf '%s' "$cw_out" | grep '^PROMOTION_FRONTEND=' | cut -d= -f2)" "false"

printf "M\tdeploy/compose/frontend.yaml\n" > "$test_tmp/compose_frontend.txt"
cf_out="$(run_case "$test_tmp/compose_frontend.txt" --format env)"
assert_eq "compose-frontend: PROMOTION_FRONTEND is true" "$(printf '%s' "$cf_out" | grep '^PROMOTION_FRONTEND=' | cut -d= -f2)" "true"
assert_eq "compose-frontend: PROMOTION_WORKER is false" "$(printf '%s' "$cf_out" | grep '^PROMOTION_WORKER=' | cut -d= -f2)" "false"
assert_eq "compose-frontend: PROMOTION_GATEWAY is false" "$(printf '%s' "$cf_out" | grep '^PROMOTION_GATEWAY=' | cut -d= -f2)" "false"

printf "M\tdeploy/compose/bark.yaml\n" > "$test_tmp/compose_bark.txt"
cb_out="$(run_case "$test_tmp/compose_bark.txt" --format env)"
assert_eq "compose-bark: PROMOTION_BARK is true" "$(printf '%s' "$cb_out" | grep '^PROMOTION_BARK=' | cut -d= -f2)" "true"
assert_eq "compose-bark: PROMOTION_WORKER is false" "$(printf '%s' "$cb_out" | grep '^PROMOTION_WORKER=' | cut -d= -f2)" "false"

printf "M\tdeploy/compose/base.yaml\n" > "$test_tmp/compose_base.txt"
cbase_out="$(run_case "$test_tmp/compose_base.txt" --format env)"
assert_eq "compose-base: PROMOTION_WORKER is true" "$(printf '%s' "$cbase_out" | grep '^PROMOTION_WORKER=' | cut -d= -f2)" "true"
assert_eq "compose-base: PROMOTION_GATEWAY is true" "$(printf '%s' "$cbase_out" | grep '^PROMOTION_GATEWAY=' | cut -d= -f2)" "true"
assert_eq "compose-base: PROMOTION_FRONTEND is true" "$(printf '%s' "$cbase_out" | grep '^PROMOTION_FRONTEND=' | cut -d= -f2)" "true"

# 23. Deploy workflow only vs orchestrator scripts
printf "\n23. Testing deploy.yml (platform only) vs dispatch-rollout.sh (orchestrator)...\n"
printf "M\t.github/workflows/deploy.yml\n" > "$test_tmp/deploy_workflow.txt"
dw_out="$(run_case "$test_tmp/deploy_workflow.txt" --format env)"
assert_eq "deploy-workflow: PROMOTION_PLATFORM is true" "$(printf '%s' "$dw_out" | grep '^PROMOTION_PLATFORM=' | cut -d= -f2)" "true"
assert_eq "deploy-workflow: PROMOTION_GATEWAY is false" "$(printf '%s' "$dw_out" | grep '^PROMOTION_GATEWAY=' | cut -d= -f2)" "false"
assert_eq "deploy-workflow: PROMOTION_WORKER is false" "$(printf '%s' "$dw_out" | grep '^PROMOTION_WORKER=' | cut -d= -f2)" "false"
assert_eq "deploy-workflow: PROMOTION_FRONTEND is false" "$(printf '%s' "$dw_out" | grep '^PROMOTION_FRONTEND=' | cut -d= -f2)" "false"
assert_eq "deploy-workflow: PROMOTION_SCHEMA is false" "$(printf '%s' "$dw_out" | grep '^PROMOTION_SCHEMA=' | cut -d= -f2)" "false"

printf "M\tdeploy/dispatch-rollout.sh\n" > "$test_tmp/dispatch_rollout.txt"
dr_out="$(run_case "$test_tmp/dispatch_rollout.txt" --format env)"
assert_eq "dispatch-rollout: PROMOTION_GATEWAY is true" "$(printf '%s' "$dr_out" | grep '^PROMOTION_GATEWAY=' | cut -d= -f2)" "true"
assert_eq "dispatch-rollout: PROMOTION_WORKER is true" "$(printf '%s' "$dr_out" | grep '^PROMOTION_WORKER=' | cut -d= -f2)" "true"
assert_eq "dispatch-rollout: PROMOTION_FRONTEND is true" "$(printf '%s' "$dr_out" | grep '^PROMOTION_FRONTEND=' | cut -d= -f2)" "true"
assert_eq "dispatch-rollout: PROMOTION_SCHEMA is true" "$(printf '%s' "$dr_out" | grep '^PROMOTION_SCHEMA=' | cut -d= -f2)" "true"

# 24. Git diff with a Unicode documentation path
printf "\n24. Testing Git-real-diff Unicode documentation path...\n"
unicode_repo="$test_tmp/unicode-repo"
mkdir -p "$unicode_repo"
git -C "$unicode_repo" init -q
git -C "$unicode_repo" config user.name "Promotion Scope Test"
git -C "$unicode_repo" config user.email "promotion-scope-test@example.invalid"
printf 'base\n' > "$unicode_repo/README.md"
git -C "$unicode_repo" add README.md
git -C "$unicode_repo" commit -q -m base
unicode_base="$(git -C "$unicode_repo" rev-parse HEAD)"
printf 'documentation\n' > "$unicode_repo/Kế hoạch triển khai — thử nghiệm.md"
git -C "$unicode_repo" add .
git -C "$unicode_repo" commit -q -m unicode-doc
unicode_head="$(git -C "$unicode_repo" rev-parse HEAD)"
unicode_out="$(cd "$unicode_repo" && COMPONENT_MAP_FILE="$script_dir/../deploy/component-map.json" bash "$classifier" --base "$unicode_base" --head "$unicode_head" --format env)"
assert_eq "unicode Git diff: PROMOTION_DOC_ONLY is true" "$(printf '%s' "$unicode_out" | grep '^PROMOTION_DOC_ONLY=' | cut -d= -f2)" "true"
assert_eq "unicode Git diff: PROMOTION_GATEWAY is false" "$(printf '%s' "$unicode_out" | grep '^PROMOTION_GATEWAY=' | cut -d= -f2)" "false"
assert_eq "unicode Git diff: PROMOTION_SCOPE is empty" "$(printf '%s' "$unicode_out" | grep '^PROMOTION_SCOPE=' | cut -d= -f2)" ""

printf "\n========================================\n"
printf "Results: %d passed, %d failed\n" "$pass_count" "$fail_count"
printf "========================================\n"

if [[ $fail_count -gt 0 ]]; then
  exit 1
fi
exit 0
