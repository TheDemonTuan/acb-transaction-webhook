#!/usr/bin/env bash
# deploy/tests/test_deploy_workflow_runtime_preflight.sh
# Validates preflight-runtime.sh detects marker-free runtime drift (run #249 scenario)
# and converges dirty runtime to canonical when requested.
set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR="$(cd -- "$SCRIPT_DIR/.." && pwd)"

TEST_TMP="$(mktemp -d "${TMPDIR:-/tmp}/acb-runtime-preflight-tests.XXXXXX")"
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

assert_file_exists() {
  local file="$1"
  local msg="$2"
  if [[ ! -f "$file" ]]; then
    printf 'FAIL: %s (file %s does not exist)\n' "$msg" "$file" >&2
    TESTS_FAILED=$(( TESTS_FAILED + 1 ))
    return 1
  fi
  printf 'PASS: %s\n' "$msg"
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
  return 0
}

printf "========================================================\n"
printf "Running Runtime Preflight Drift Regression Tests\n"
printf "========================================================\n\n"

tdir="$TEST_TMP"
mkdir -p "$tdir/releases/rel-A/compose" \
         "$tdir/state" "$tdir/data" "$tdir/secrets" "$tdir/mock_bin" "$tdir/deploy" "$tdir/traefik"

# Copy deployment engine
cp -r "$DEPLOY_DIR/lib"* "$tdir/deploy/"
cp "$DEPLOY_DIR/release-env.sh" "$tdir/deploy/"
cp "$DEPLOY_DIR/runtime-layout.sh" "$tdir/deploy/"
cp "$DEPLOY_DIR/release-state.py" "$tdir/deploy/"
cp "$DEPLOY_DIR/reconcile-release.sh" "$tdir/deploy/"
cp "$DEPLOY_DIR/edge-probe.sh" "$tdir/deploy/"
cp "$DEPLOY_DIR/verify-runtime-drift.sh" "$tdir/deploy/"
cp "$DEPLOY_DIR/preflight-runtime.sh" "$tdir/deploy/"

cp -r "$DEPLOY_DIR/compose/"* "$tdir/releases/rel-A/compose/"
cp "$DEPLOY_DIR/verify-runtime-drift.sh" "$tdir/releases/rel-A/"
cp -r "$DEPLOY_DIR/lib"* "$tdir/releases/rel-A/"
cp "$DEPLOY_DIR/release-env.sh" "$tdir/releases/rel-A/"
cp "$DEPLOY_DIR/runtime-layout.sh" "$tdir/releases/rel-A/"
cp "$DEPLOY_DIR/release-state.py" "$tdir/releases/rel-A/"

img_a="ghcr.io/test/gateway@sha256:000000000000000000000000000000000000000000000000000000000000000a"
img_b="ghcr.io/test/gateway@sha256:000000000000000000000000000000000000000000000000000000000000000b"
common_img="ghcr.io/test/app@sha256:1111111111111111111111111111111111111111111111111111111111111111"

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

manifest_a_sha="$(sha256sum "$tdir/releases/rel-A/release-manifest.json" | cut -d' ' -f1)"

# Canonical state describes A: active gateway is blue
cat <<EOF > "$tdir/state/current-release.json"
{
  "schema_version": 2,
  "generation": 2,
  "release_id": "rel-A",
  "release_dir": "$tdir/releases/rel-A",
  "git_sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "manifest_sha256": "$manifest_a_sha",
  "status": "COMPLETED",
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

# Model run #249 starting condition:
# NO rollout journal, NO pending retire files
rm -f "$tdir/data/rollout-journal.json"
rm -f "$tdir/data/pending-gateway-retire.env"
rm -f "$tdir/data/pending-frontend-retire.env"
rm -f "$tdir/data/deploy-journal.json"

# But runtime has drifted: Traefik route points to green, actual running slot is green
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

# Mock docker state
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

# Mock docker
cat <<'EOF' > "$tdir/mock_bin/docker"
#!/usr/bin/env bash
set -euo pipefail
STATE_DIR="${DOCKER_STATE_DIR:-/tmp/docker_state}"
target="${1:-}"

if [[ "$target" == "inspect" ]]; then
  shift
  format=""
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --format|-f) format="$2"; shift 2 ;;
      -*) shift ;;
      *) break ;;
    esac
  done
  cname="${1:-}"
  if [[ "$cname" == "edge-traefik" || "$*" == *"edge-traefik"* ]]; then
    echo "${TRAEFIK_DYNAMIC_DIR:-/opt/platform/edge/dynamic}"
    exit 0
  fi
  img="none"
  status="exited"
  health="unhealthy"
  running="false"
  if [[ -f "$STATE_DIR/$cname.image" ]]; then img="$(cat "$STATE_DIR/$cname.image")"; fi
  if [[ -f "$STATE_DIR/$cname.status" ]]; then status="$(cat "$STATE_DIR/$cname.status")"; fi
  if [[ -f "$STATE_DIR/$cname.health" ]]; then health="$(cat "$STATE_DIR/$cname.health")"; fi
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

if [[ "$target" == "compose" ]]; then
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
  if [[ "$compose_cmd" == "up" ]]; then
    if [[ "$svc" == "gateway-blue" || -z "$svc" ]]; then
      printf 'running' > "$STATE_DIR/acb-gateway-blue.status"
      printf 'healthy' > "$STATE_DIR/acb-gateway-blue.health"
      if [[ -f "$proj_dir/release-manifest.json" ]]; then
        gw_img="$(sed -n 's/.*"gateway":[[:space:]]*"\([^"]*\)".*/\1/p' "$proj_dir/release-manifest.json")"
        printf '%s' "$gw_img" > "$STATE_DIR/acb-gateway-blue.image"
      fi
    fi
  elif [[ "$compose_cmd" == "stop" ]]; then
    if [[ "$svc" == "gateway-green" || -z "$svc" ]]; then
      printf 'exited' > "$STATE_DIR/acb-gateway-green.status"
      printf 'unhealthy' > "$STATE_DIR/acb-gateway-green.health"
    fi
  fi
  exit 0
fi

if [[ "$target" == "stop" ]]; then
  cname="${2:-}"
  if [[ -f "$STATE_DIR/$cname.status" ]]; then
    printf 'exited' > "$STATE_DIR/$cname.status"
  fi
  exit 0
fi

exit 0
EOF
chmod +x "$tdir/mock_bin/docker"

# Mock edge probe to succeed
cat <<'EOF' > "$tdir/mock_bin/edge-probe"
#!/usr/bin/env bash
exit 0
EOF
chmod +x "$tdir/mock_bin/edge-probe"

# Step 1: Run preflight in --check-only mode on marker-free dirty runtime
# Must fail closed (nonzero)
ec=0
PATH="$tdir/mock_bin:$PATH" \
DOCKER_STATE_DIR="$DOCKER_STATE_DIR" \
RUNTIME_ROOT="$tdir" \
RUNTIME_RELEASES_DIR="$tdir/releases" \
DEPLOY_PATH="$tdir" \
TRAEFIK_DYNAMIC_DIR="$tdir/traefik" \
ACB_CONFIG="$tdir/traefik/acb.yml" \
ACTIVE_SLOT_FILE="$tdir/state/gateway-active-slot" \
FRONTEND_ACTIVE_SLOT_FILE="$tdir/state/frontend-active-slot" \
CURRENT_RELEASE_FILE="$tdir/state/current-release.json" \
SKIP_MANIFEST_CHECK=1 \
EDGE_PROBE_SCRIPT="$tdir/mock_bin/edge-probe" \
bash "$tdir/deploy/preflight-runtime.sh" --check-only \
  --state "$tdir/state/current-release.json" \
  --data-dir "$tdir/data" \
  --config "$tdir/traefik/acb.yml" || ec=$?

assert_eq "1" "$ec" "Marker-free drift (run #249 condition) detected as dirty by preflight --check-only"

# Invariant: --check-only must not mutate runtime state or containers
assert_eq "green" "$(cat "$tdir/state/gateway-active-slot")" "Preflight --check-only did not mutate gateway active slot"
assert_eq "running" "$(cat "$DOCKER_STATE_DIR/acb-gateway-green.status")" "Preflight --check-only did not mutate running container state"
if grep -q "acb-web-green" "$tdir/traefik/acb.yml"; then
  printf 'PASS: Preflight --check-only did not mutate Traefik route\n'
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
else
  printf 'FAIL: Preflight --check-only unexpectedly modified Traefik route\n' >&2
  TESTS_FAILED=$(( TESTS_FAILED + 1 ))
fi

# Invariant: Normal deploy pipeline halts on dirty runtime before stable-deployer runs
deployer_marker="$tdir/stable_deployer_ran.marker"
rm -f "$deployer_marker"
cat <<EOF > "$tdir/mock_bin/stable-deployer.sh"
#!/usr/bin/env bash
touch "$deployer_marker"
exit 0
EOF
chmod +x "$tdir/mock_bin/stable-deployer.sh"

pipeline_ec=0
PATH="$tdir/mock_bin:$PATH" \
DOCKER_STATE_DIR="$DOCKER_STATE_DIR" \
RUNTIME_ROOT="$tdir" RUNTIME_RELEASES_DIR="$tdir/releases" DEPLOY_PATH="$tdir" \
TRAEFIK_DYNAMIC_DIR="$tdir/traefik" ACB_CONFIG="$tdir/traefik/acb.yml" \
ACTIVE_SLOT_FILE="$tdir/state/gateway-active-slot" FRONTEND_ACTIVE_SLOT_FILE="$tdir/state/frontend-active-slot" \
CURRENT_RELEASE_FILE="$tdir/state/current-release.json" SKIP_MANIFEST_CHECK=1 \
EDGE_PROBE_SCRIPT="$tdir/mock_bin/edge-probe" \
bash -c '
  set -euo pipefail
  bash "$1/deploy/preflight-runtime.sh" --check-only \
    --state "$1/state/current-release.json" \
    --data-dir "$1/data" \
    --config "$1/traefik/acb.yml"
  bash "$1/mock_bin/stable-deployer.sh"
' _ "$tdir" || pipeline_ec=$?

assert_eq "1" "$pipeline_ec" "Deploy pipeline halts on dirty runtime preflight failure"
if [[ ! -f "$deployer_marker" ]]; then
  printf 'PASS: stable-deployer never executed when runtime is dirty\n'
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
else
  printf 'FAIL: stable-deployer executed despite dirty runtime preflight check\n' >&2
  TESTS_FAILED=$(( TESTS_FAILED + 1 ))
fi

# Invariant: .github/workflows/deploy.yml invokes preflight with --check-only, never --reconcile
workflow_deploy_yml="$DEPLOY_DIR/../.github/workflows/deploy.yml"
if grep -q 'deploy/preflight-runtime\.sh.*--reconcile' "$workflow_deploy_yml"; then
  printf 'FAIL: deploy.yml still calls preflight-runtime.sh with --reconcile\n' >&2
  TESTS_FAILED=$(( TESTS_FAILED + 1 ))
else
  printf 'PASS: deploy.yml does not call preflight-runtime.sh with --reconcile\n'
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
fi
if grep -q 'deploy/preflight-runtime\.sh.*--check-only' "$workflow_deploy_yml"; then
  printf 'PASS: deploy.yml calls preflight-runtime.sh with --check-only\n'
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
else
  printf 'FAIL: deploy.yml missing --check-only invocation for preflight-runtime.sh\n' >&2
  TESTS_FAILED=$(( TESTS_FAILED + 1 ))
fi

# Step 2: Run preflight in --reconcile mode
# Must converge runtime to canonical release (A), recreate blue gateway, update route, and return 0
ec=0
PATH="$tdir/mock_bin:$PATH" \
DOCKER_STATE_DIR="$DOCKER_STATE_DIR" \
RUNTIME_ROOT="$tdir" \
RUNTIME_RELEASES_DIR="$tdir/releases" \
DEPLOY_PATH="$tdir" \
TRAEFIK_DYNAMIC_DIR="$tdir/traefik" \
ACB_CONFIG="$tdir/traefik/acb.yml" \
ACTIVE_SLOT_FILE="$tdir/state/gateway-active-slot" \
FRONTEND_ACTIVE_SLOT_FILE="$tdir/state/frontend-active-slot" \
CURRENT_RELEASE_FILE="$tdir/state/current-release.json" \
SKIP_MANIFEST_CHECK=1 \
EDGE_PROBE_SCRIPT="$tdir/mock_bin/edge-probe" \
bash "$tdir/deploy/preflight-runtime.sh" --reconcile \
  --state "$tdir/state/current-release.json" \
  --data-dir "$tdir/data" \
  --config "$tdir/traefik/acb.yml" || ec=$?

assert_eq "0" "$ec" "Preflight --reconcile successfully converged dirty runtime and re-verified cleanly"

# Reusing a signed artifact after canonical generation changes must fail before mutation.
ec=0
PATH="$tdir/mock_bin:$PATH" \
DOCKER_STATE_DIR="$DOCKER_STATE_DIR" \
RUNTIME_ROOT="$tdir" RUNTIME_RELEASES_DIR="$tdir/releases" DEPLOY_PATH="$tdir" \
TRAEFIK_DYNAMIC_DIR="$tdir/traefik" ACB_CONFIG="$tdir/traefik/acb.yml" \
ACTIVE_SLOT_FILE="$tdir/state/gateway-active-slot" FRONTEND_ACTIVE_SLOT_FILE="$tdir/state/frontend-active-slot" \
CURRENT_RELEASE_FILE="$tdir/state/current-release.json" SKIP_MANIFEST_CHECK=1 \
EDGE_PROBE_SCRIPT="$tdir/mock_bin/edge-probe" \
bash "$tdir/deploy/preflight-runtime.sh" --reconcile --state "$tdir/state/current-release.json" \
  --data-dir "$tdir/data" --config "$tdir/traefik/acb.yml" \
  --expected-base-sha "$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["git_sha"])' "$tdir/state/current-release.json")" \
  --expected-base-generation 1 || ec=$?
[[ "$ec" -ne 0 ]] && printf 'PASS: Stale signed baseline refused on deploy-only rerun\n' || {
  printf 'FAIL: Stale signed baseline accepted on deploy-only rerun\n' >&2
  exit 1
}

# Assert gateway slot is now blue
assert_eq "blue" "$(cat "$tdir/state/gateway-active-slot")" "Gateway active slot converged to blue"
# Assert Traefik route is now blue
if grep -q "acb-web-blue" "$tdir/traefik/acb.yml"; then
  printf 'PASS: Traefik route converged to acb-web-blue\n'
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
else
  printf 'FAIL: Traefik route not pointing to acb-web-blue\n' >&2
  TESTS_FAILED=$(( TESTS_FAILED + 1 ))
fi

# Step 3: Regression test - uncompleted/interrupted journal is archived upon successful reconcile
printf "\nTesting uncompleted rollout journal is archived upon successful reconcile...\n"
echo '{"status": "INTERRUPTED", "release_id": "rel-dirty"}' > "$tdir/data/rollout-journal.json"
assert_file_exists "$tdir/data/rollout-journal.json" "Interrupted rollout journal created"

ec=0
PATH="$tdir/mock_bin:$PATH" \
DOCKER_STATE_DIR="$DOCKER_STATE_DIR" \
RUNTIME_ROOT="$tdir" \
RUNTIME_RELEASES_DIR="$tdir/releases" \
DEPLOY_PATH="$tdir" \
TRAEFIK_DYNAMIC_DIR="$tdir/traefik" \
ACB_CONFIG="$tdir/traefik/acb.yml" \
ACTIVE_SLOT_FILE="$tdir/state/gateway-active-slot" \
FRONTEND_ACTIVE_SLOT_FILE="$tdir/state/frontend-active-slot" \
CURRENT_RELEASE_FILE="$tdir/state/current-release.json" \
SKIP_MANIFEST_CHECK=1 \
EDGE_PROBE_SCRIPT="$tdir/mock_bin/edge-probe" \
bash "$tdir/deploy/preflight-runtime.sh" --reconcile \
  --state "$tdir/state/current-release.json" \
  --data-dir "$tdir/data" \
  --config "$tdir/traefik/acb.yml" || ec=$?

assert_eq "0" "$ec" "Preflight --reconcile passes and clears dirty interrupted journal"

if [[ -f "$tdir/data/rollout-journal.json" ]]; then
  printf 'FAIL: Active rollout-journal.json still present after reconcile\n' >&2
  TESTS_FAILED=$(( TESTS_FAILED + 1 ))
else
  printf 'PASS: Active rollout-journal.json was archived away\n'
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
fi

archived_count="$(ls "$tdir/data"/rollout-journal.json.reconciled.* 2>/dev/null | wc -l)"
if [[ "$archived_count" -ge 1 ]]; then
  printf 'PASS: Archived rollout journal file exists with timestamp\n'
  TESTS_PASSED=$(( TESTS_PASSED + 1 ))
else
  printf 'FAIL: Archived rollout journal file was not created\n' >&2
  TESTS_FAILED=$(( TESTS_FAILED + 1 ))
fi

printf "\n========================================================\n"
printf "Results: %d Passed, %d Failed\n" "$TESTS_PASSED" "$TESTS_FAILED"
printf "========================================================\n"

if (( TESTS_FAILED > 0 )); then
  exit 1
fi
exit 0
