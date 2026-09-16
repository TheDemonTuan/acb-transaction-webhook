#!/usr/bin/env bash
# deploy/tests/test_component_tx_recovery.sh
# Validates that recover_tx_journal fails closed on any component recovery failure.
set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR="$(cd -- "$SCRIPT_DIR/.." && pwd)"

TEST_TMP="$(mktemp -d "${TMPDIR:-/tmp}/acb-tx-fail-closed-test.XXXXXX")"
trap 'rm -rf "$TEST_TMP"' EXIT

TESTS_PASSED=0
TESTS_FAILED=0

assert_eq() {
  local expected="$1"
  local actual="$2"
  local msg="$3"
  if [[ "$expected" != "$actual" ]]; then
    printf 'FAIL: %s (expected: "%s", got: "%s")\n' "$msg" "$expected" "$actual" >&2
    TESTS_FAILED=$(( TESTS_FAILED + 1 ))
    return 1
  fi
  printf 'PASS: %s\n' "$msg"
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
  return 0
}

printf "========================================================\n"
printf "Running Component Transaction Recovery Fail-Closed Tests\n"
printf "========================================================\n\n"

setup_test_env() {
  local tdir="$1"
  rm -rf "$tdir"
  mkdir -p "$tdir/deploy" "$tdir/state" "$tdir/data" "$tdir/mock_bin" "$tdir/traefik" "$tdir/compose"

  cp -r "$DEPLOY_DIR/lib"* "$tdir/deploy/"
  cp "$DEPLOY_DIR/release-env.sh" "$tdir/deploy/"
  cp "$DEPLOY_DIR/runtime-layout.sh" "$tdir/deploy/"
  cp "$DEPLOY_DIR/edge-probe.sh" "$tdir/deploy/"
  cp -r "$DEPLOY_DIR/compose/"* "$tdir/compose/"
}

# 1. Unknown component fails closed
tdir="$TEST_TMP/unknown_comp"
setup_test_env "$tdir"
cat <<'EOF' > "$tdir/data/tx-journal.json"
{
  "component": "unknown_component_xyz",
  "state": "TX_IN_PROGRESS",
  "started_at": "2026-09-16T12:00:00Z"
}
EOF

ec=0
PATH="$tdir/mock_bin:$PATH" \
RUNTIME_ROOT="$tdir" \
TX_JOURNAL_FILE="$tdir/data/tx-journal.json" \
DEPLOY_DIR="$tdir/deploy" \
bash -c 'source "$DEPLOY_DIR/lib.sh"; recover_tx_journal' || ec=$?

assert_eq "1" "$ec" "Unknown component in tx journal returns nonzero"
[[ -f "$tdir/data/tx-journal.json" ]] && printf 'PASS: Journal preserved on unknown component\n' && TESTS_PASSED=$((TESTS_PASSED + 1))

# 2. Worker start failure fails closed
tdir="$TEST_TMP/worker_fail"
setup_test_env "$tdir"
cat <<'EOF' > "$tdir/data/tx-journal.json"
{
  "component": "worker",
  "state": "TX_IN_PROGRESS",
  "previous_digest": "ghcr.io/test/worker@sha256:0000000000000000000000000000000000000000000000000000000000000001",
  "started_at": "2026-09-16T12:00:00Z"
}
EOF

# Mock docker where up fails
cat <<'EOF' > "$tdir/mock_bin/docker"
#!/usr/bin/env bash
if [[ "$*" == *"up"* ]]; then
  echo "mock docker up error" >&2
  exit 1
fi
exit 0
EOF
chmod +x "$tdir/mock_bin/docker"

ec=0
PATH="$tdir/mock_bin:$PATH" \
RUNTIME_ROOT="$tdir" \
TX_JOURNAL_FILE="$tdir/data/tx-journal.json" \
DEPLOY_DIR="$tdir/deploy" \
bash -c 'source "$DEPLOY_DIR/lib.sh"; recover_tx_journal' || ec=$?

assert_eq "1" "$ec" "Worker restart failure returns nonzero"
[[ -f "$tdir/data/tx-journal.json" ]] && printf 'PASS: Journal preserved on worker restart failure\n' && TESTS_PASSED=$((TESTS_PASSED + 1))

# 3. Gateway candidate stop failure fails closed
tdir="$TEST_TMP/gw_stop_fail"
setup_test_env "$tdir"
cat <<'EOF' > "$tdir/data/tx-journal.json"
{
  "component": "gateway",
  "state": "TX_IN_PROGRESS",
  "active_slot": "blue",
  "candidate_slot": "green",
  "started_at": "2026-09-16T12:00:00Z"
}
EOF

# Mock docker where stop fails and container remains running
cat <<'EOF' > "$tdir/mock_bin/docker"
#!/usr/bin/env bash
if [[ "$*" == *"stop"* ]]; then
  exit 1
fi
if [[ "$*" == *"inspect"* ]]; then
  echo "true"
  exit 0
fi
exit 0
EOF
chmod +x "$tdir/mock_bin/docker"

ec=0
PATH="$tdir/mock_bin:$PATH" \
RUNTIME_ROOT="$tdir" \
TX_JOURNAL_FILE="$tdir/data/tx-journal.json" \
DEPLOY_DIR="$tdir/deploy" \
bash -c 'source "$DEPLOY_DIR/lib.sh"; recover_tx_journal' || ec=$?

assert_eq "1" "$ec" "Gateway candidate stop failure returns nonzero"
[[ -f "$tdir/data/tx-journal.json" ]] && printf 'PASS: Journal preserved on gateway stop failure\n' && TESTS_PASSED=$((TESTS_PASSED + 1))

# 4. Auth-browser failure fails closed
tdir="$TEST_TMP/auth_fail"
setup_test_env "$tdir"
cat <<'EOF' > "$tdir/data/tx-journal.json"
{
  "component": "auth-browser",
  "state": "TX_IN_PROGRESS",
  "previous_digest": "ghcr.io/test/auth@sha256:0000000000000000000000000000000000000000000000000000000000000001",
  "started_at": "2026-09-16T12:00:00Z"
}
EOF

cat <<'EOF' > "$tdir/mock_bin/docker"
#!/usr/bin/env bash
if [[ "$*" == *"up"* ]]; then
  exit 1
fi
exit 0
EOF
chmod +x "$tdir/mock_bin/docker"

ec=0
PATH="$tdir/mock_bin:$PATH" \
RUNTIME_ROOT="$tdir" \
TX_JOURNAL_FILE="$tdir/data/tx-journal.json" \
DEPLOY_DIR="$tdir/deploy" \
bash -c 'source "$DEPLOY_DIR/lib.sh"; recover_tx_journal' || ec=$?

assert_eq "1" "$ec" "Auth-browser restart failure returns nonzero"
[[ -f "$tdir/data/tx-journal.json" ]] && printf 'PASS: Journal preserved on auth-browser failure\n' && TESTS_PASSED=$((TESTS_PASSED + 1))

# 5. TTS failure fails closed
tdir="$TEST_TMP/tts_fail"
setup_test_env "$tdir"
cat <<'EOF' > "$tdir/data/tx-journal.json"
{
  "component": "tts",
  "state": "TX_IN_PROGRESS",
  "previous_digest": "ghcr.io/test/tts@sha256:0000000000000000000000000000000000000000000000000000000000000001",
  "started_at": "2026-09-16T12:00:00Z"
}
EOF

cat <<'EOF' > "$tdir/mock_bin/docker"
#!/usr/bin/env bash
if [[ "$*" == *"up"* ]]; then
  exit 1
fi
exit 0
EOF
chmod +x "$tdir/mock_bin/docker"

ec=0
PATH="$tdir/mock_bin:$PATH" \
RUNTIME_ROOT="$tdir" \
TX_JOURNAL_FILE="$tdir/data/tx-journal.json" \
DEPLOY_DIR="$tdir/deploy" \
bash -c 'source "$DEPLOY_DIR/lib.sh"; recover_tx_journal' || ec=$?

assert_eq "1" "$ec" "TTS restart failure returns nonzero"
[[ -f "$tdir/data/tx-journal.json" ]] && printf 'PASS: Journal preserved on TTS failure\n' && TESTS_PASSED=$((TESTS_PASSED + 1))

# 6. Bark failure fails closed
tdir="$TEST_TMP/bark_fail"
setup_test_env "$tdir"
cat <<'EOF' > "$tdir/data/tx-journal.json"
{
  "component": "bark",
  "state": "TX_IN_PROGRESS",
  "previous_digest": "ghcr.io/test/bark@sha256:0000000000000000000000000000000000000000000000000000000000000001",
  "started_at": "2026-09-16T12:00:00Z"
}
EOF

cat <<'EOF' > "$tdir/mock_bin/docker"
#!/usr/bin/env bash
if [[ "$*" == *"up"* ]]; then
  exit 1
fi
exit 0
EOF
chmod +x "$tdir/mock_bin/docker"

ec=0
PATH="$tdir/mock_bin:$PATH" \
RUNTIME_ROOT="$tdir" \
TX_JOURNAL_FILE="$tdir/data/tx-journal.json" \
DEPLOY_DIR="$tdir/deploy" \
bash -c 'source "$DEPLOY_DIR/lib.sh"; recover_tx_journal' || ec=$?

assert_eq "1" "$ec" "Bark restart failure returns nonzero"
[[ -f "$tdir/data/tx-journal.json" ]] && printf 'PASS: Journal preserved on Bark failure\n' && TESTS_PASSED=$((TESTS_PASSED + 1))

# 7. Successful recovery archives journal and returns 0
tdir="$TEST_TMP/success_rec"
setup_test_env "$tdir"
cat <<'EOF' > "$tdir/data/tx-journal.json"
{
  "component": "worker",
  "state": "TX_IN_PROGRESS",
  "previous_digest": "ghcr.io/test/worker@sha256:0000000000000000000000000000000000000000000000000000000000000001",
  "started_at": "2026-09-16T12:00:00Z"
}
EOF

cat <<'EOF' > "$tdir/mock_bin/docker"
#!/usr/bin/env bash
if [[ "$*" == *"inspect"* ]]; then
  echo "false"
  exit 0
fi
exit 0
EOF
chmod +x "$tdir/mock_bin/docker"

ec=0
PATH="$tdir/mock_bin:$PATH" \
RUNTIME_ROOT="$tdir" \
TX_JOURNAL_FILE="$tdir/data/tx-journal.json" \
DEPLOY_DIR="$tdir/deploy" \
bash -c 'source "$DEPLOY_DIR/lib.sh"; recover_tx_journal' || ec=$?

assert_eq "0" "$ec" "Successful component recovery returns 0"
[[ ! -f "$tdir/data/tx-journal.json" ]] && printf 'PASS: Active journal archived on success\n' && TESTS_PASSED=$((TESTS_PASSED + 1))

printf "\n========================================================\n"
printf "Results: %d Passed, %d Failed\n" "$TESTS_PASSED" "$TESTS_FAILED"
printf "========================================================\n"

if (( TESTS_FAILED > 0 )); then
  exit 1
fi
exit 0
