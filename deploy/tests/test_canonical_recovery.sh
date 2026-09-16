#!/usr/bin/env bash
# deploy/tests/test_canonical_recovery.sh
# Regression test: verify failed-candidate recovery converges to current canonical release (A),
# never jumping backward to previous canonical release (A0).
set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR="$(cd -- "$SCRIPT_DIR/.." && pwd)"

TEST_TMP="$(mktemp -d "${TMPDIR:-/tmp}/acb-canonical-recovery-test.XXXXXX")"
trap 'rm -rf "$TEST_TMP"' EXIT

export ALLOW_TEST_LOCK_PATH=1
export FAILOVER_STATE_DIR="$TEST_TMP/failover"
mkdir -p "$FAILOVER_STATE_DIR"

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
printf "Running Canonical Recovery Target Selection Regression\n"
printf "========================================================\n\n"

tdir="$TEST_TMP"
mkdir -p "$tdir/releases/rel-A0/compose" \
         "$tdir/releases/rel-A/compose" \
         "$tdir/releases/rel-B/compose" \
         "$tdir/state" "$tdir/data" "$tdir/secrets" "$tdir/mock_bin" "$tdir/deploy" "$tdir/traefik"

# Copy deployment engine
cp -r "$DEPLOY_DIR/lib"* "$tdir/deploy/"
cp "$DEPLOY_DIR/release-env.sh" "$tdir/deploy/"
cp "$DEPLOY_DIR/runtime-layout.sh" "$tdir/deploy/"
cp "$DEPLOY_DIR/release-state.py" "$tdir/deploy/"
cp "$DEPLOY_DIR/reconcile-release.sh" "$tdir/deploy/"
cp "$DEPLOY_DIR/rollback-release.sh" "$tdir/deploy/"
cp "$DEPLOY_DIR/edge-probe.sh" "$tdir/deploy/"
cp "$DEPLOY_DIR/verify-runtime-drift.sh" "$tdir/deploy/"

for r in rel-A0 rel-A rel-B; do
  cp -r "$DEPLOY_DIR/compose/"* "$tdir/releases/$r/compose/"
  cp "$DEPLOY_DIR/verify-runtime-drift.sh" "$tdir/releases/$r/"
  cp -r "$DEPLOY_DIR/lib"* "$tdir/releases/$r/"
  cp "$DEPLOY_DIR/release-env.sh" "$tdir/releases/$r/"
  cp "$DEPLOY_DIR/runtime-layout.sh" "$tdir/releases/$r/"
  cp "$DEPLOY_DIR/release-state.py" "$tdir/releases/$r/"
done

img_a0="ghcr.io/test/gateway@sha256:00000000000000000000000000000000000000000000000000000000000000a0"
img_a="ghcr.io/test/gateway@sha256:000000000000000000000000000000000000000000000000000000000000000a"
img_b="ghcr.io/test/gateway@sha256:000000000000000000000000000000000000000000000000000000000000000b"
common_img="ghcr.io/test/app@sha256:1111111111111111111111111111111111111111111111111111111111111111"

# Create release manifests
cat <<EOF > "$tdir/releases/rel-A0/release-manifest.json"
{
  "release_id": "rel-A0",
  "git_sha": "00000000000000000000000000000000000000a0",
  "images": {
    "gateway": "$img_a0",
    "frontend": "$common_img",
    "worker": "$common_img",
    "dbtool": "$common_img",
    "auth_browser": "$common_img",
    "tts": "$common_img",
    "bark": "$common_img"
  }
}
EOF

cat <<EOF > "$tdir/releases/rel-A/release-manifest.json"
{
  "release_id": "rel-A",
  "git_sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "images": {
    "gateway": "$img_a",
    "frontend": "$common_img",
    "worker": "$common_img",
    "dbtool": "$common_img",
    "auth_browser": "$common_img",
    "tts": "$common_img",
    "bark": "$common_img"
  }
}
EOF

cat <<EOF > "$tdir/releases/rel-B/release-manifest.json"
{
  "release_id": "rel-B",
  "git_sha": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
  "images": {
    "gateway": "$img_b",
    "frontend": "$common_img",
    "worker": "$common_img",
    "dbtool": "$common_img",
    "auth_browser": "$common_img",
    "tts": "$common_img",
    "bark": "$common_img"
  }
}
EOF

manifest_a_sha="$(sha256sum "$tdir/releases/rel-A/release-manifest.json" | cut -d' ' -f1)"
manifest_a0_sha="$(sha256sum "$tdir/releases/rel-A0/release-manifest.json" | cut -d' ' -f1)"

# Canonical state describes A and includes previous.release_dir=A0
cat <<EOF > "$tdir/state/current-release.json"
{
  "schema_version": 2,
  "generation": 2,
  "release_id": "rel-A",
  "release_dir": "$tdir/releases/rel-A",
  "git_sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "manifest_sha256": "$manifest_a_sha",
  "status": "COMPLETED",
  "previous": {
    "generation": 1,
    "release_id": "rel-A0",
    "release_dir": "$tdir/releases/rel-A0",
    "git_sha": "00000000000000000000000000000000000000a0",
    "manifest_sha256": "$manifest_a0_sha"
  },
  "active_slots": {
    "gateway": "blue",
    "frontend": "blue"
  },
  "images": {
    "gateway": {
      "blue": "$img_a",
      "green": "$img_a"
    },
    "frontend": "$common_img",
    "worker": "$common_img",
    "dbtool": "$common_img",
    "auth_browser": "$common_img",
    "tts": "$common_img",
    "bark": "$common_img"
  }
}
EOF

# Candidate journal for rel-B failed mid-rollout
source "$tdir/deploy/lib.sh"
ROLLOUT_JOURNAL_FILE="$tdir/data/rollout-journal.json" \
  init_rollout_journal "rel-B" "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" "gateway" "$tdir/releases/rel-B" "$tdir/releases/rel-A"
ROLLOUT_JOURNAL_FILE="$tdir/data/rollout-journal.json" update_rollout_step "gateway" "STEP_IN_PROGRESS"

# Set up dirty runtime:
# Traefik points to green
cat <<'EOF' > "$tdir/traefik/acb.yml"
http:
  routers:
    acb-router:
      rule: "Host(`example.com`)"
      service: acb-service
    acb-frontend-router:
      rule: "Host(`example.com`) && PathPrefix(`/ui`)"
      service: acb-frontend-service
  services:
    acb-service:
      loadBalancer:
        servers:
          - url: "http://acb-web-green:8090"
    acb-frontend-service:
      loadBalancer:
        servers:
          - url: "http://acb-frontend-blue:8080"
EOF

printf 'green' > "$tdir/state/gateway-active-slot"
printf 'blue' > "$tdir/state/frontend-active-slot"
printf 'old_slot=blue\ncandidate_slot=green\n' > "$tdir/data/pending-gateway-retire.env"

# Mock docker state tracking
DOCKER_STATE_DIR="$tdir/docker_state"
mkdir -p "$DOCKER_STATE_DIR"

printf '%s' "$img_b" > "$DOCKER_STATE_DIR/acb-gateway-blue.image"
printf 'exited' > "$DOCKER_STATE_DIR/acb-gateway-blue.status"

printf '%s' "$img_b" > "$DOCKER_STATE_DIR/acb-gateway-green.image"
printf 'running' > "$DOCKER_STATE_DIR/acb-gateway-green.status"
printf 'healthy' > "$DOCKER_STATE_DIR/acb-gateway-green.health"

for comp in worker auth-browser tts-gateway bark; do
  printf '%s' "$common_img" > "$DOCKER_STATE_DIR/acb-$comp.image"
  printf 'running' > "$DOCKER_STATE_DIR/acb-$comp.status"
  printf 'healthy' > "$DOCKER_STATE_DIR/acb-$comp.health"
done
printf '%s' "$common_img" > "$DOCKER_STATE_DIR/acb-frontend-blue.image"
printf 'running' > "$DOCKER_STATE_DIR/acb-frontend-blue.status"
printf 'healthy' > "$DOCKER_STATE_DIR/acb-frontend-blue.health"

# Mock docker command
cat <<'EOF' > "$tdir/mock_bin/docker"
#!/usr/bin/env bash
set -euo pipefail
STATE_DIR="${DOCKER_STATE_DIR:-/tmp/docker_state}"
LOG_FILE="${DOCKER_LOG_FILE:-/tmp/docker.log}"

echo "docker $*" >> "$LOG_FILE"

if [[ "${1:-}" == "inspect" ]]; then
  shift
  format=""
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --format|-f) format="$2"; shift 2 ;;
      -*) shift ;;
      *) break ;;
    esac
  done
  target="${1:-}"
  if [[ "$target" == "edge-traefik" || "$*" == *"edge-traefik"* ]]; then
    echo "${TRAEFIK_DYNAMIC_DIR:-$tdir/traefik}"
    exit 0
  fi
  if [[ "$target" == "edge-cloudflared" || "$*" == *"edge-cloudflared"* ]]; then
    echo "true"
    exit 0
  fi
  img="none"
  status="exited"
  health="unhealthy"
  running="false"
  if [[ -f "$STATE_DIR/$target.image" ]]; then img="$(cat "$STATE_DIR/$target.image")"; fi
  if [[ -f "$STATE_DIR/$target.status" ]]; then status="$(cat "$STATE_DIR/$target.status")"; fi
  if [[ -f "$STATE_DIR/$target.health" ]]; then health="$(cat "$STATE_DIR/$target.health")"; fi
  if [[ "$status" == "running" ]]; then running="true"; fi

  if [[ "$format" == *"Config.Image"* ]]; then
    printf '%s\n' "$img" | tr -d '\r'
    exit 0
  fi
  if [[ "$format" == *"State.Health.Status"* ]]; then
    printf '%s\n' "$health" | tr -d '\r'
    exit 0
  fi
  if [[ "$format" == *"State.Running"* ]]; then
    printf '%s\n' "$running" | tr -d '\r'
    exit 0
  fi
  if [[ "$format" == *"State.Status"* ]]; then
    printf '%s\n' "$status" | tr -d '\r'
    exit 0
  fi
  printf '%s\n' "$img" | tr -d '\r'
  exit 0
fi

if [[ "${1:-}" == "compose" ]]; then
  shift
  proj_dir=""
  compose_cmd=""
  svc=""
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --project-directory) proj_dir="$2"; shift 2 ;;
      --env-file|-f) shift 2 ;;
      up|stop) compose_cmd="$1"; shift ;;
      -d|--no-deps) shift ;;
      gateway-*|frontend-*|worker|bark|tts*|auth-browser) svc="$1"; shift ;;
      *) shift ;;
    esac
  done
  echo "compose_cmd: dir=$proj_dir cmd=$compose_cmd svc=$svc" >> "$LOG_FILE"
  if [[ "$compose_cmd" == "up" ]]; then
    if [[ "$svc" == "gateway-blue" || "$cmd" == "gateway-blue" ]]; then
      # read IMAGE_REF_BLUE from env or release context
      printf 'running' > "$STATE_DIR/acb-gateway-blue.status"
      printf 'healthy' > "$STATE_DIR/acb-gateway-blue.health"
      # Record which project directory was used
      printf '%s' "$proj_dir" > "$STATE_DIR/gateway-blue.restored_from"
      if [[ -f "$proj_dir/release-manifest.json" ]]; then
        gw_img="$(sed -n 's/.*"gateway":[[:space:]]*"\([^"]*\)".*/\1/p' "$proj_dir/release-manifest.json")"
        printf '%s' "$gw_img" > "$STATE_DIR/acb-gateway-blue.image"
      fi
    fi
  elif [[ "$cmd" == "stop" ]]; then
    if [[ "$svc" == "gateway-green" || "$cmd" == "gateway-green" ]]; then
      printf 'exited' > "$STATE_DIR/acb-gateway-green.status"
      printf 'unhealthy' > "$STATE_DIR/acb-gateway-green.health"
    fi
  fi
  exit 0
fi

if [[ "${1:-}" == "stop" ]]; then
  target="${2:-}"
  if [[ -f "$STATE_DIR/$target.status" ]]; then
    printf 'exited' > "$STATE_DIR/$target.status"
  fi
  exit 0
fi

exit 0
EOF
chmod +x "$tdir/mock_bin/docker"

# Mock edge probe to succeed for canonical blue slot
cat <<'EOF' > "$tdir/mock_bin/edge-probe"
#!/usr/bin/env bash
exit 0
EOF
chmod +x "$tdir/mock_bin/edge-probe"

# Run reconcile-release
export DOCKER_STATE_DIR="$DOCKER_STATE_DIR"
export DOCKER_LOG_FILE="$tdir/docker.log"

ec=0
PATH="$tdir/mock_bin:$PATH" \
RUNTIME_ROOT="$tdir" \
RUNTIME_RELEASES_DIR="$tdir/releases" \
DEPLOY_PATH="$tdir" \
TRAEFIK_DYNAMIC_DIR="$tdir/traefik" \
ACB_CONFIG="$tdir/traefik/acb.yml" \
ACTIVE_SLOT_FILE="$tdir/state/gateway-active-slot" \
FRONTEND_ACTIVE_SLOT_FILE="$tdir/state/frontend-active-slot" \
CURRENT_RELEASE_FILE="$tdir/state/current-release.json" \
ROLLOUT_JOURNAL_FILE="$tdir/data/rollout-journal.json" \
PENDING_GATEWAY_RETIRE_FILE="$tdir/data/pending-gateway-retire.env" \
SKIP_MANIFEST_CHECK=1 \
RUNTIME_DRIFT_CHECK_CMD="true" \
EDGE_PROBE_SCRIPT="$tdir/mock_bin/edge-probe" \
bash "$tdir/deploy/reconcile-release.sh" --runtime-root "$tdir" --recovery-only || ec=$?

assert_eq "0" "$ec" "Reconcile exits cleanly"

# Check gateway active slot
active_gw="$(cat "$tdir/state/gateway-active-slot")"
assert_eq "blue" "$active_gw" "Active gateway slot restored to blue"

# Check Traefik route points to blue, NOT green
if grep -q "acb-web-blue:8090" "$tdir/traefik/acb.yml"; then
  printf 'PASS: Traefik route restored to acb-web-blue\n'
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
else
  printf 'FAIL: Traefik route does not point to acb-web-blue\n' >&2
  TESTS_FAILED=$(( TESTS_FAILED + 1 ))
fi

# Check gateway blue container was recreated with canonical release A image
blue_img="$(cat "$DOCKER_STATE_DIR/acb-gateway-blue.image")"
assert_eq "$img_a" "$blue_img" "acb-gateway-blue running canonical release A image (NOT A0, NOT B)"

# Check that blue container was restored from release A directory, NEVER rel-A0
restored_from="$(cat "$DOCKER_STATE_DIR/gateway-blue.restored_from" 2>/dev/null || echo "")"
if [[ "$restored_from" == *"/releases/rel-A0"* ]]; then
  printf 'FAIL: Recovery wrongly restored gateway from previous canonical release rel-A0!\n' >&2
  TESTS_FAILED=$(( TESTS_FAILED + 1 ))
elif [[ "$restored_from" == *"/releases/rel-A"* ]]; then
  printf 'PASS: Recovery restored gateway from canonical release rel-A\n'
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
else
  printf 'FAIL: Recovery did not restore from rel-A (restored_from=%s)\n' "$restored_from" >&2
  TESTS_FAILED=$(( TESTS_FAILED + 1 ))
fi

# Check standby green was stopped
green_status="$(cat "$DOCKER_STATE_DIR/acb-gateway-green.status")"
assert_eq "exited" "$green_status" "Standby candidate green slot was stopped"

# Check pending evidence was cleaned up
if [[ ! -f "$tdir/data/pending-gateway-retire.env" ]]; then
  printf 'PASS: pending-gateway-retire.env was cleaned up\n'
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
else
  printf 'FAIL: pending-gateway-retire.env still present\n' >&2
  TESTS_FAILED=$(( TESTS_FAILED + 1 ))
fi

printf "\n========================================================\n"
printf "Results: %d Passed, %d Failed\n" "$TESTS_PASSED" "$TESTS_FAILED"
printf "========================================================\n"

if (( TESTS_FAILED > 0 )); then
  exit 1
fi
exit 0
