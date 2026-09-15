#!/usr/bin/env bash
# scripts/verify.sh
# Canonical verification entrypoint across the entire repository.
# Executes local/CI verification gates: Go tests, Go vet, web build/typecheck/vitest,
# Playwright E2E suite, Python tests, shell syntax, and deployment failure drill suites.
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "$SCRIPT_DIR/.." && pwd)"
cd "$REPO_ROOT"

PASSED_STEPS=0
FAILED_STEPS=0
SKIPPED_STEPS=0

log_header() {
  printf '\n================================================================================\n'
  printf '>>> %s\n' "$*"
  printf '================================================================================\n'
}

record_pass() {
  printf '[PASS] %s\n' "$*"
  PASSED_STEPS=$(( PASSED_STEPS + 1 ))
}

record_fail() {
  printf '[FAIL] %s\n' "$*" >&2
  FAILED_STEPS=$(( FAILED_STEPS + 1 ))
}

record_skip() {
  printf '[SKIP] %s\n' "$*"
  SKIPPED_STEPS=$(( SKIPPED_STEPS + 1 ))
}

# 1. Shell scripts syntax check (bash -n)
log_header "Gate 1: Shell Scripts Syntax Check (bash -n)"
shopt -s nullglob
shell_scripts=(deploy/*.sh deploy/lib/*.sh deploy/tests/*.sh scripts/ops/*.sh scripts/*.sh platform/edge/*.sh)
if [[ ${#shell_scripts[@]} -eq 0 ]]; then
  record_fail "No shell scripts found"
else
  shell_syntax_ok=1
  for script in "${shell_scripts[@]}"; do
    if ! bash -n "$script"; then
      shell_syntax_ok=0
    fi
  done
  if [[ "$shell_syntax_ok" -eq 1 ]]; then
    record_pass "Shell syntax check passed across all scripts"
  else
    record_fail "Shell syntax check failed"
  fi
fi

# Shellcheck (if installed)
if command -v shellcheck >/dev/null 2>&1; then
  if shellcheck --severity=error deploy/*.sh deploy/lib/*.sh deploy/tests/*.sh scripts/ops/*.sh scripts/*.sh; then
    record_pass "Shellcheck passed"
  else
    record_fail "Shellcheck reported errors"
  fi
else
  record_skip "Shellcheck not installed in current environment"
fi

# 2. Go static analysis (vet)
log_header "Gate 2: Go Static Analysis (go vet)"
if go vet ./...; then
  record_pass "go vet ./... passed"
else
  record_fail "go vet ./... failed"
fi

# 3. Go unit & integration test suite
log_header "Gate 3: Go Test Suite"
if go test -count=1 ./...; then
  record_pass "go test ./... passed"
else
  record_fail "go test ./... failed"
fi

# Go race detector (if CGO enabled / gcc installed)
if command -v gcc >/dev/null 2>&1; then
  if go test -race -count=1 ./internal/scheduler ./internal/monitor ./tests/integration; then
    record_pass "go test -race passed on concurrency packages"
  else
    record_fail "go test -race failed"
  fi
else
  record_skip "gcc/cgo unavailable for go test -race in current environment"
fi

# 4. Web typecheck, build, and unit tests
log_header "Gate 4: Web Application Suite"
if (cd web && bunx --bun tsc --noEmit); then
  record_pass "web tsc --noEmit passed"
else
  record_fail "web tsc --noEmit failed"
fi

if (cd web && bunx --bun vite build); then
  record_pass "web vite build passed"
else
  record_fail "web vite build failed"
fi

if (cd web && bun run test); then
  record_pass "web vitest suite passed"
else
  record_fail "web vitest suite failed"
fi

# 5. Playwright E2E browser regression suite
log_header "Gate 5: Browser Playwright E2E Regression Suite"
if (cd web && bun run e2e); then
  record_pass "Playwright E2E suite passed on configured desktop and mobile projects"
else
  record_fail "Playwright E2E suite failed"
fi

# 6. Python test suites (Failover controller & TTS Gateway)
log_header "Gate 6: Python Test Suites"
PYTHON_BIN=""
if command -v pytest >/dev/null 2>&1; then
  PYTHON_BIN="python"
elif [[ -f "C:/Program Files/PostgreSQL/17/pgAdmin 4/python/python.exe" ]]; then
  PYTHON_BIN="C:/Program Files/PostgreSQL/17/pgAdmin 4/python/python.exe"
fi

if [[ -n "$PYTHON_BIN" ]]; then
  if "$PYTHON_BIN" -m pytest platform/failover/test_failover.py; then
    record_pass "failover controller pytest passed"
  else
    record_fail "failover controller pytest failed"
  fi

  if "$PYTHON_BIN" -m pytest tts-gateway; then
    record_pass "tts-gateway pytest passed"
  else
    record_fail "tts-gateway pytest failed"
  fi
else
  record_skip "Python pytest runner unavailable"
fi

# 7. Deployment verification & failure drill suites
log_header "Gate 7: Deployment Failure Drill Suites"
DEPLOY_DRILL_TESTS=(
  "deploy/tests/test_secrets.sh"
  "deploy/tests/test_backup.sh"
  "deploy/tests/test_restore_drill.sh"
  "deploy/tests/test_compose_policy.sh"
  "deploy/tests/test_runtime_policy.sh"
  "deploy/tests/test_deploy.sh"
  "deploy/tests/test_gateway_deploy.sh"
  "deploy/tests/test_schema_deploy.sh"
  "deploy/tests/test_worker_deploy.sh"
  "deploy/tests/test_aux_deploy.sh"
  "deploy/tests/test_traefik_switch.sh"
  "deploy/tests/test_promotion_dispatcher.sh"
  "deploy/tests/test_health_telemetry.sh"
  "deploy/tests/test_ci_workflow.sh"
  "deploy/test-supply-chain.sh"
  "scripts/test-promotion-scope.sh"
  "scripts/verify-actions-pinned.sh"
  "scripts/verify-architecture-docs.sh"
)

for drill in "${DEPLOY_DRILL_TESTS[@]}"; do
  printf 'Running drill: %s...\n' "$drill"
  if bash "$drill" >/dev/null 2>&1; then
    record_pass "$drill"
  else
    record_fail "$drill"
  fi
done

# 8. Git diff check
log_header "Gate 8: Workspace Formatting & Tree Check"
if git diff --check; then
  record_pass "git diff --check clean"
else
  record_fail "git diff --check reported whitespace or conflict markers"
fi

log_header "Verification Summary"
printf 'Passed:  %d\n' "$PASSED_STEPS"
printf 'Failed:  %d\n' "$FAILED_STEPS"
printf 'Skipped: %d\n' "$SKIPPED_STEPS"

if [[ "$FAILED_STEPS" -gt 0 ]]; then
  printf '\nResult: FAILED (%d failures)\n' "$FAILED_STEPS" >&2
  exit 1
fi

printf '\nResult: ALL EXECUTABLE SUITES PASSED\n'
exit 0
