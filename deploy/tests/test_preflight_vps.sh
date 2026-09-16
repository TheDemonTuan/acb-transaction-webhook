#!/usr/bin/env bash
# deploy/tests/test_preflight_vps.sh
# Tests strict VPS preflight verification (P0. Kiểm runtime/Traefik trước deploy).
set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR="$(cd -- "$SCRIPT_DIR/.." && pwd)"

TEST_TMP="$(mktemp -d "${TMPDIR:-/tmp}/acb-preflight-tests.XXXXXX")"
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

setup_env() {
  local tdir="$1"
  mkdir -p "$tdir/mock_bin" "$tdir/state" "$tdir/data" "$tdir/traefik" "$tdir/deploy"
  cp -r "$DEPLOY_DIR/lib" "$tdir/deploy/"
  cp "$DEPLOY_DIR/lib.sh" "$tdir/deploy/"
  cp "$DEPLOY_DIR/preflight-vps.sh" "$tdir/deploy/"

  cat <<'EOF' > "$tdir/mock_bin/docker"
#!/usr/bin/env bash
cmd="$1"
shift || true
case "$cmd" in
  inspect)
    format=""
    while [[ $# -gt 0 ]]; do
      case "$1" in
        --format) format="$2"; shift 2 ;;
        *) target="$1"; shift ;;
      esac
    done
    if [[ "$format" == "{{.State.Running}}" ]]; then
      echo "true"
      exit 0
    fi
    if [[ "$format" == *"{{.State.Health.Status}}"* ]]; then
      echo "healthy"
      exit 0
    fi
    if [[ "$format" == "{{.Config.Image}}" ]]; then
      case "$target" in
        acb-gateway-blue) echo "ghcr.io/acb/gateway@sha256:1111111111111111111111111111111111111111111111111111111111111111" ;;
        acb-gateway-green) echo "ghcr.io/acb/gateway@sha256:2222222222222222222222222222222222222222222222222222222222222222" ;;
        acb-worker) echo "ghcr.io/acb/worker@sha256:3333333333333333333333333333333333333333333333333333333333333333" ;;
        acb-auth-browser) echo "ghcr.io/acb/auth-browser@sha256:4444444444444444444444444444444444444444444444444444444444444444" ;;
        acb-tts-gateway) echo "ghcr.io/acb/tts-gateway@sha256:5555555555555555555555555555555555555555555555555555555555555555" ;;
        acb-bark) echo "ghcr.io/finb/bark-server@sha256:32d65b07fa835c99b31a396b77727a04ed058377fc2482da3e9dc7397167ffc4" ;;
        acb-frontend-blue) echo "ghcr.io/acb/frontend@sha256:7777777777777777777777777777777777777777777777777777777777777777" ;;
        *) echo "" ;;
      esac
      exit 0
    fi
    echo "unknown inspect" >&2
    exit 1
    ;;
  *)
    exit 0
    ;;
esac
EOF
  chmod +x "$tdir/mock_bin/docker"

  cat <<EOF > "$tdir/state/current-release.json"
{
  "schema_version": 2,
  "status": "COMPLETED",
  "active_slots": {
    "gateway": "blue",
    "frontend": "blue"
  },
  "images": {
    "gateway": {
      "blue": "ghcr.io/acb/gateway@sha256:1111111111111111111111111111111111111111111111111111111111111111",
      "green": "ghcr.io/acb/gateway@sha256:2222222222222222222222222222222222222222222222222222222222222222"
    },
    "worker": "ghcr.io/acb/worker@sha256:3333333333333333333333333333333333333333333333333333333333333333",
    "auth_browser": "ghcr.io/acb/auth-browser@sha256:4444444444444444444444444444444444444444444444444444444444444444",
    "tts": "ghcr.io/acb/tts-gateway@sha256:5555555555555555555555555555555555555555555555555555555555555555",
    "bark": "ghcr.io/finb/bark-server@sha256:32d65b07fa835c99b31a396b77727a04ed058377fc2482da3e9dc7397167ffc4",
    "frontend": "ghcr.io/acb/frontend@sha256:7777777777777777777777777777777777777777777777777777777777777777"
  }
}
EOF

  printf 'blue' > "$tdir/state/gateway-active-slot"
  printf 'blue' > "$tdir/state/frontend-active-slot"

  cat <<'EOF' > "$tdir/traefik/acb.yml"
http:
  services:
    acb-service:
      loadBalancer:
        servers:
          - url: "http://acb-web-blue:8090"
    acb-frontend-service:
      loadBalancer:
        servers:
          - url: "http://acb-frontend-blue:8080"
EOF
}

run_preflight() {
  local tdir="$1"
  PATH="$tdir/mock_bin:$PATH" \
  ACTIVE_SLOT_FILE="$tdir/state/gateway-active-slot" \
  bash "$tdir/deploy/preflight-vps.sh" \
    --state "$tdir/state/current-release.json" \
    --data-dir "$tdir/data" \
    --config "$tdir/traefik/acb.yml"
}

printf "========================================================\n"
printf "Running Strict VPS Preflight Verification Tests\n"
printf "========================================================\n\n"

# Test 1: Clean healthy runtime passes preflight
t1="$TEST_TMP/t1_clean"
setup_env "$t1"
ec=0
run_preflight "$t1" || ec=$?
assert_eq "0" "$ec" "Matching runtime passes strict preflight check"

# Test 2: Missing current-release.json fails preflight
t2="$TEST_TMP/t2_no_state"
setup_env "$t2"
rm -f "$t2/state/current-release.json"
ec=0
run_preflight "$t2" 2>/dev/null || ec=$?
assert_eq "1" "$ec" "Missing current-release.json fails preflight"

# Test 3: Active gateway slot mismatch fails preflight
t3="$TEST_TMP/t3_slot_mismatch"
setup_env "$t3"
printf 'green' > "$t3/state/gateway-active-slot"
ec=0
run_preflight "$t3" 2>/dev/null || ec=$?
assert_eq "1" "$ec" "Active gateway slot mismatch fails preflight"

# Test 4: Traefik routing to wrong slot fails preflight
t4="$TEST_TMP/t4_traefik_mismatch"
setup_env "$t4"
cat <<'EOF' > "$t4/traefik/acb.yml"
http:
  services:
    acb-service:
      loadBalancer:
        servers:
          - url: "http://acb-web-green:8090"
EOF
ec=0
run_preflight "$t4" 2>/dev/null || ec=$?
assert_eq "1" "$ec" "Traefik route pointing to green while state is blue fails preflight"

# Test 5: Container image mismatch fails preflight
t5="$TEST_TMP/t5_image_mismatch"
setup_env "$t5"
cat <<'EOF' > "$t5/mock_bin/docker"
#!/usr/bin/env bash
if [[ "$*" == *"{{.Config.Image}}"* && "$*" == *"acb-bark"* ]]; then
  echo "ghcr.io/finb/bark-server@sha256:0000000000000000000000000000000000000000000000000000000000000000"
  exit 0
fi
if [[ "$*" == *"{{.State.Running}}"* ]]; then echo "true"; exit 0; fi
echo "ghcr.io/acb/gateway@sha256:1111111111111111111111111111111111111111111111111111111111111111"
EOF
chmod +x "$t5/mock_bin/docker"
ec=0
run_preflight "$t5" 2>/dev/null || ec=$?
assert_eq "1" "$ec" "Bark container image mismatch fails preflight"

# Test 6: Lingering pending-gateway-retire file fails preflight
t6="$TEST_TMP/t6_pending_retire"
setup_env "$t6"
touch "$t6/data/pending-gateway-retire.env"
ec=0
run_preflight "$t6" 2>/dev/null || ec=$?
assert_eq "1" "$ec" "Lingering pending-gateway-retire.env fails preflight"

# Test 7: Lingering interrupted rollout journal fails preflight
t7="$TEST_TMP/t7_interrupted_journal"
setup_env "$t7"
cat <<'EOF' > "$t7/data/rollout-journal.json"
{
  "schema_version": 2,
  "status": "INTERRUPTED"
}
EOF
ec=0
run_preflight "$t7" 2>/dev/null || ec=$?
assert_eq "1" "$ec" "Interrupted rollout journal fails preflight"

printf "\n========================================================\n"
printf "Results: %d passed, %d failed\n" "$TESTS_PASSED" "$TESTS_FAILED"
printf "========================================================\n"

if (( TESTS_FAILED > 0 )); then
  exit 1
fi
exit 0
