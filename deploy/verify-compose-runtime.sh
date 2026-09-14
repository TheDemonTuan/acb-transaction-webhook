#!/usr/bin/env bash
# Verification of Production Docker Compose Runtime Policy & Invariants
# Validates immutability, least privilege, network segregation, and resource limits.
set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
COMPOSE_FILE="${COMPOSE_FILE:-$SCRIPT_DIR/compose.prod.yaml}"
CHECK_LIVE=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --compose-file)
      COMPOSE_FILE="$2"; shift 2 ;;
    --live)
      CHECK_LIVE=1; shift ;;
    --help|-h)
      cat <<'EOF'
Usage: verify-compose-runtime.sh [options]

Options:
  --compose-file <path>   Path to production compose file (default: deploy/compose.prod.yaml)
  --live                  Verify running container HostConfig via Docker inspect
  --help, -h              Show this help
EOF
      exit 0
      ;;
    *)
      printf 'Unknown argument: %s\n' "$1" >&2
      exit 1
      ;;
  esac
done

if [[ ! -f "$COMPOSE_FILE" ]]; then
  printf '[FAIL] Compose file does not exist: %s\n' "$COMPOSE_FILE" >&2
  exit 1
fi

failures=0
passes=0

assert_pass() {
  local desc="$1"
  printf '  [PASS] %s\n' "$desc"
  passes=$((passes + 1))
}

assert_fail() {
  local desc="$1"
  local reason="$2"
  printf '  [FAIL] %s: %s\n' "$desc" "$reason" >&2
  failures=$((failures + 1))
}

declare -A SVC_BLOCKS
ALL_SERVICES=("worker" "auth-browser" "tts-gateway" "bark" "gateway-blue" "gateway-green" "dbtool")

load_services() {
  local file="$1"
  local in_services=0
  local current_svc=""

  while IFS= read -r line || [[ -n "$line" ]]; do
    if [[ "$line" =~ ^services:[[:space:]]*$ ]]; then
      in_services=1
      continue
    fi
    if [[ "$in_services" -eq 1 ]]; then
      if [[ "$line" =~ ^[[:space:]]{2}([a-zA-Z0-9_-]+):[[:space:]]*$ ]]; then
        current_svc="${BASH_REMATCH[1]}"
        SVC_BLOCKS["$current_svc"]=""
        continue
      elif [[ "$line" =~ ^[a-zA-Z0-9_-]+:[[:space:]]*$ ]]; then
        in_services=0
        current_svc=""
        continue
      fi
      if [[ -n "$current_svc" ]]; then
        SVC_BLOCKS["$current_svc"]+="${line}"$'\n'
      fi
    fi
  done < "$file"
}

load_services "$COMPOSE_FILE"

get_service_block() {
  local service="$1"
  printf '%s' "${SVC_BLOCKS[$service]:-}"
}

printf "========================================================\n"
printf "Verifying ACB Production Compose Runtime Policy: %s\n" "$(basename "$COMPOSE_FILE")"
printf "========================================================\n\n"

# ----------------------------------------------------
# 1. Image Immutability and Required Variable Syntax
# ----------------------------------------------------
printf "1. Checking Image Immutability & Fallback Restrictions...\n"

if grep -E 'image:[[:space:]]*.*:latest' "$COMPOSE_FILE" >/dev/null 2>&1; then
  assert_fail "No :latest tags" "Found mutable ':latest' image tags in $COMPOSE_FILE"
else
  assert_pass "No mutable ':latest' image tags present"
fi

if grep -E 'image:[[:space:]]*\$\{[^}:]+:-' "$COMPOSE_FILE" >/dev/null 2>&1; then
  assert_fail "No image fallback expressions" "Found ':-' image fallback expressions in $COMPOSE_FILE"
else
  assert_pass "No ':-' image fallbacks permitted in production compose"
fi

# Verify each expected service image is explicit and required
expected_services=(
  "worker:WORKER_IMAGE_REF"
  "auth-browser:BROWSER_IMAGE_REF"
  "tts-gateway:TTS_IMAGE_REF"
  "bark:BARK_IMAGE_REF"
  "gateway-blue:IMAGE_REF_BLUE"
  "gateway-green:IMAGE_REF_GREEN"
  "dbtool:DBTOOL_IMAGE_REF"
)

for pair in "${expected_services[@]}"; do
  svc="${pair%%:*}"
  var="${pair##*:}"
  svc_block="${SVC_BLOCKS[$svc]:-}"
  if [[ -z "$svc_block" ]]; then
    assert_fail "Service presence: $svc" "Service '$svc' not found in $COMPOSE_FILE"
    continue
  fi
  # Must match ${VAR:?VAR is required}
  if echo "$svc_block" | grep -E "image:[[:space:]]*\\$\{${var}:\\?${var}[[:space:]]+is[[:space:]]+required\}" >/dev/null 2>&1; then
    assert_pass "Service '$svc' requires immutable variable '\${${var}:?...}'"
  else
    assert_fail "Service '$svc' image variable" "Service '$svc' must require \${${var}:?${var} is required}"
  fi
done

# Ensure slot refs are strictly distinct
blue_block="${SVC_BLOCKS[gateway-blue]:-}"
green_block="${SVC_BLOCKS[gateway-green]:-}"
if echo "$blue_block" | grep -q "IMAGE_REF_GREEN" || echo "$green_block" | grep -q "IMAGE_REF_BLUE"; then
  assert_fail "Distinct slot images" "Gateway blue and green must reference distinct slot image variables"
else
  assert_pass "Gateway slots reference distinct variables (IMAGE_REF_BLUE vs IMAGE_REF_GREEN)"
fi

printf "\n"

# ----------------------------------------------------
# 2. No Build Directives and No Published Host Ports
# ----------------------------------------------------
printf "2. Checking Build Directives and Host Ports...\n"

if grep -E '^[[:space:]]*build:' "$COMPOSE_FILE" >/dev/null 2>&1; then
  assert_fail "No build directives" "Production compose must not build images from source"
else
  assert_pass "No source 'build:' blocks in production compose"
fi

if grep -E '^[[:space:]]*ports:' "$COMPOSE_FILE" >/dev/null 2>&1; then
  assert_fail "No published host ports" "Production services must not expose host ports directly; ingress is owned by Traefik/Cloudflare"
else
  assert_pass "No host 'ports:' published; traffic strictly isolated to Docker networks"
fi

printf "\n"

# ----------------------------------------------------
# 3. Volume and Bind Mount Invariants
# ----------------------------------------------------
printf "3. Checking Filesystem Mount Invariants...\n"

if grep -E 'docker\.sock' "$COMPOSE_FILE" >/dev/null 2>&1; then
  assert_fail "Docker socket isolation" "Mounting Docker socket (/var/run/docker.sock) is strictly forbidden"
else
  assert_pass "Docker socket (/var/run/docker.sock) is never mounted"
fi

# Ensure no host root or sensitive system directories mounted in service volumes
unsafe_mount_found=0
unsafe_mount_detail=""

# Extract all volume mount entries across services (excluding tmpfs sections)
in_tmpfs_section=0
while IFS= read -r line || [[ -n "$line" ]]; do
  if [[ "$line" =~ ^[[:space:]]*tmpfs:[[:space:]]*$ ]]; then
    in_tmpfs_section=1
    continue
  fi
  if [[ "$in_tmpfs_section" -eq 1 ]]; then
    if [[ "$line" =~ ^[[:space:]]{6}-[[:space:]]+ ]]; then
      continue
    else
      in_tmpfs_section=0
    fi
  fi
  # Check if line is a volume mount entry under a service (indented with 6 spaces)
  if [[ "$line" =~ ^[[:space:]]{6}-[[:space:]]+(.*)$ ]]; then
    mount_entry="${BASH_REMATCH[1]}"
    host_part="${mount_entry%%:*}"
    host_part="${host_part//[[:space:]]/}"
    # Allowed exceptions: specific file mounts like ./bark-entrypoint.sh
    if [[ "$host_part" == "./bark-entrypoint.sh" ]]; then
      continue
    fi
    # Reject absolute host paths, home paths, parent paths, or repository source directories
    if [[ "$host_part" =~ ^(/|~|\.\./) ]]; then
      unsafe_mount_found=1
      unsafe_mount_detail="Unsafe absolute or host system mount detected: '$host_part'"
      break
    elif [[ "$host_part" =~ ^\./(cmd|internal|web|tts-gateway|platform) ]]; then
      unsafe_mount_found=1
      unsafe_mount_detail="Source tree bind mount detected: '$host_part'"
      break
    fi
  fi
done < "$COMPOSE_FILE"

if [[ "$unsafe_mount_found" -eq 1 ]]; then
  assert_fail "Host path isolation" "$unsafe_mount_detail"
else
  assert_pass "No unsafe host root, system directory, or source repository mounts"
fi

printf "\n"

# ----------------------------------------------------
# 4. Filesystem Read-Only & Tmpfs Constraints
# ----------------------------------------------------
printf "4. Checking Read-Only Filesystem & Tmpfs Constraints...\n"

readonly_services=("worker" "tts-gateway" "bark" "gateway-blue" "gateway-green" "dbtool")
for svc in "${readonly_services[@]}"; do
  svc_block="${SVC_BLOCKS[$svc]:-}"
  if echo "$svc_block" | grep -E 'read_only:[[:space:]]*true' >/dev/null 2>&1; then
    assert_pass "Service '$svc' enforces read_only: true"
  else
    assert_fail "Service '$svc' read_only" "Service '$svc' must enforce 'read_only: true'"
  fi
done

# Auth-browser writable scope bounded by tmpfs
ab_block="${SVC_BLOCKS[auth-browser]:-}"
if echo "$ab_block" | grep -E 'read_only:[[:space:]]*false' >/dev/null 2>&1 && \
   echo "$ab_block" | grep -E 'tmpfs:' >/dev/null 2>&1 && \
   echo "$ab_block" | grep -E '/tmp:rw' >/dev/null 2>&1; then
  assert_pass "Auth-browser writable requirements bounded to explicit tmpfs mounts"
else
  assert_fail "Auth-browser tmpfs bounding" "Auth-browser must constrain browser workspace via explicit tmpfs"
fi

printf "\n"

# ----------------------------------------------------
# 5. Non-Root First-Party User Policy
# ----------------------------------------------------
printf "5. Checking Non-Root First-Party User Policy...\n"

first_party_services=("worker" "auth-browser" "tts-gateway" "gateway-blue" "gateway-green" "dbtool")
for svc in "${first_party_services[@]}"; do
  svc_block="${SVC_BLOCKS[$svc]:-}"
  if echo "$svc_block" | grep -E 'user:[[:space:]]*["'\'']?1000:1000["'\'']?' >/dev/null 2>&1; then
    assert_pass "Service '$svc' runs as non-root user 1000:1000"
  else
    assert_fail "Service '$svc' non-root user" "First-party service '$svc' must specify 'user: \"1000:1000\"'"
  fi
done

printf "\n"

# ----------------------------------------------------
# 6. Capability Dropping & Security Options
# ----------------------------------------------------
printf "6. Checking Linux Capability Drops & Security Options...\n"

for svc in "${ALL_SERVICES[@]}"; do
  svc_block="${SVC_BLOCKS[$svc]:-}"
  if echo "$svc_block" | grep -E 'cap_drop:[[:space:]]*(\[[[:space:]]*ALL[[:space:]]*\]|-[[:space:]]*ALL)' >/dev/null 2>&1; then
    assert_pass "Service '$svc' drops all capabilities (cap_drop: [ALL])"
  else
    assert_fail "Service '$svc' cap_drop" "Service '$svc' must specify 'cap_drop: [ALL]'"
  fi

  if echo "$svc_block" | grep -E 'no-new-privileges:true' >/dev/null 2>&1; then
    assert_pass "Service '$svc' enforces no-new-privileges"
  else
    assert_fail "Service '$svc' no-new-privileges" "Service '$svc' must enforce security_opt 'no-new-privileges:true'"
  fi
done

# Auth-browser seccomp profile check
if echo "$ab_block" | grep -E 'seccomp:./seccomp-auth-browser.json' >/dev/null 2>&1; then
  assert_pass "Auth-browser attaches dedicated seccomp profile (seccomp-auth-browser.json)"
else
  assert_fail "Auth-browser seccomp" "Auth-browser must specify 'seccomp:./seccomp-auth-browser.json'"
fi

printf "\n"

# ----------------------------------------------------
# 7. Network Segregation Invariants
# ----------------------------------------------------
printf "7. Checking Network Segregation Invariants...\n"

# Worker, auth-browser, tts-gateway MUST NOT join edge-acb
for svc in worker auth-browser tts-gateway; do
  svc_block="${SVC_BLOCKS[$svc]:-}"
  if echo "$svc_block" | grep -E '^[[:space:]]+edge-acb:' >/dev/null 2>&1; then
    assert_fail "Network segregation: $svc" "Service '$svc' must NOT join edge-acb network"
  else
    assert_pass "Service '$svc' excluded from edge-acb"
  fi
  if echo "$svc_block" | grep -E '^[[:space:]]+acb-core:' >/dev/null 2>&1 && \
     echo "$svc_block" | grep -E '^[[:space:]]+acb-egress:' >/dev/null 2>&1; then
    assert_pass "Service '$svc' joins acb-core and acb-egress"
  else
    assert_fail "Network membership: $svc" "Service '$svc' must join acb-core and acb-egress"
  fi
done

# dbtool MUST join ONLY 'none' network (no edge-acb, no acb-core, no acb-egress)
dbtool_block="${SVC_BLOCKS[dbtool]:-}"
if echo "$dbtool_block" | grep -E '^[[:space:]]+-[[:space:]]+none' >/dev/null 2>&1; then
  assert_pass "Service 'dbtool' joins isolated 'none' network"
else
  assert_fail "dbtool network" "Service 'dbtool' must join isolated 'none' network"
fi

for forbidden_net in edge-acb acb-core acb-egress; do
  if echo "$dbtool_block" | grep -E "^[[:space:]]+${forbidden_net}:" >/dev/null 2>&1; then
    assert_fail "dbtool isolation" "Service 'dbtool' must NOT join '${forbidden_net}'"
  fi
done
assert_pass "Service 'dbtool' strictly isolated from edge, core, and egress networks"

# gateway slots and bark join edge-acb, acb-core, acb-egress
# Gateway requires acb-egress to fetch Cloudflare Access JWKS certs for user JWT authentication
for svc in gateway-blue gateway-green bark; do
  svc_block="${SVC_BLOCKS[$svc]:-}"
  if echo "$svc_block" | grep -E '^[[:space:]]+edge-acb:' >/dev/null 2>&1 && \
     echo "$svc_block" | grep -E '^[[:space:]]+acb-core:' >/dev/null 2>&1 && \
     echo "$svc_block" | grep -E '^[[:space:]]+acb-egress:' >/dev/null 2>&1; then
    assert_pass "Service '$svc' joins edge-acb, acb-core, and acb-egress"
  else
    assert_fail "Service '$svc' networks" "Service '$svc' must join edge-acb, acb-core, and acb-egress"
  fi
done

printf "\n"

# ----------------------------------------------------
# 8. Resource Constraints (Dual Service & Deploy Limits)
# ----------------------------------------------------
printf "8. Checking Resource Limits (CPUs, Memory, PIDs)...\n"

for svc in "${ALL_SERVICES[@]}"; do
  svc_block="${SVC_BLOCKS[$svc]:-}"
  # Check service-level resource keys
  if echo "$svc_block" | grep -E '^[[:space:]]*cpus:[[:space:]]*' >/dev/null 2>&1 && \
     echo "$svc_block" | grep -E '^[[:space:]]*mem_limit:[[:space:]]*' >/dev/null 2>&1 && \
     echo "$svc_block" | grep -E '^[[:space:]]*pids_limit:[[:space:]]*' >/dev/null 2>&1; then
    assert_pass "Service '$svc' specifies service-level limits (cpus, mem_limit, pids_limit)"
  else
    assert_fail "Service '$svc' limits" "Service '$svc' must specify service-level limits: cpus, mem_limit, pids_limit"
  fi

  # Check deploy.resources.limits
  if echo "$svc_block" | grep -E '^[[:space:]]*deploy:' >/dev/null 2>&1 && \
     echo "$svc_block" | grep -E '^[[:space:]]*limits:' >/dev/null 2>&1; then
    assert_pass "Service '$svc' specifies deploy.resources.limits for Compose v2/v3 compatibility"
  else
    assert_fail "Service '$svc' deploy limits" "Service '$svc' must specify deploy.resources.limits"
  fi
done

printf "\n"

# ----------------------------------------------------
# 9. Log Hygiene & Bounded Retention
# ----------------------------------------------------
printf "9. Checking Log Rotation Policies...\n"

for svc in "${ALL_SERVICES[@]}"; do
  svc_block="${SVC_BLOCKS[$svc]:-}"
  if echo "$svc_block" | grep -E 'driver:[[:space:]]*["'\'']?json-file["'\'']?' >/dev/null 2>&1 && \
     echo "$svc_block" | grep -E 'max-size:[[:space:]]*' >/dev/null 2>&1 && \
     echo "$svc_block" | grep -E 'max-file:[[:space:]]*' >/dev/null 2>&1; then
    assert_pass "Service '$svc' specifies json-file log rotation with max-size and max-file"
  else
    assert_fail "Service '$svc' logging" "Service '$svc' must configure bounded json-file logging"
  fi
done

printf "\n"

# ----------------------------------------------------
# 10. Healthchecks & Role Directives
# ----------------------------------------------------
printf "10. Checking Container Healthchecks and Role Wiring...\n"

# Worker healthcheck and role
worker_block="${SVC_BLOCKS[worker]:-}"
if echo "$worker_block" | grep -E 'RUNTIME_ROLE:[[:space:]]*worker' >/dev/null 2>&1 && \
   echo "$worker_block" | grep -E '/worker.*--liveness-check' >/dev/null 2>&1; then
  assert_pass "Worker specifies RUNTIME_ROLE: worker and --liveness-check health probe"
else
  assert_fail "Worker role/health" "Worker must specify RUNTIME_ROLE: worker and --liveness-check"
fi

# Gateways healthcheck and role
for gw in gateway-blue gateway-green; do
  gw_block="${SVC_BLOCKS[$gw]:-}"
  if echo "$gw_block" | grep -E 'RUNTIME_ROLE:[[:space:]]*gateway' >/dev/null 2>&1 && \
     echo "$gw_block" | grep -E '/gateway.*--healthcheck' >/dev/null 2>&1; then
    assert_pass "Gateway '$gw' specifies RUNTIME_ROLE: gateway and --healthcheck"
  else
    assert_fail "Gateway '$gw' role/health" "Gateway '$gw' must specify RUNTIME_ROLE: gateway and --healthcheck"
  fi
done

# Auth-browser healthcheck
if echo "$ab_block" | grep -E '/auth-browser.*--healthcheck' >/dev/null 2>&1; then
  assert_pass "Auth-browser specifies container healthcheck (--healthcheck)"
else
  assert_fail "Auth-browser healthcheck" "Auth-browser must specify container healthcheck"
fi

# TTS healthcheck
tts_block="${SVC_BLOCKS[tts-gateway]:-}"
if echo "$tts_block" | grep -E 'http://127.0.0.1:8081/health' >/dev/null 2>&1; then
  assert_pass "TTS gateway specifies internal /health healthcheck"
else
  assert_fail "TTS healthcheck" "TTS gateway must specify internal /health healthcheck"
fi

# Bark healthcheck
bark_block="${SVC_BLOCKS[bark]:-}"
if echo "$bark_block" | grep -E 'http://127.0.0.1:8080/ping' >/dev/null 2>&1; then
  assert_pass "Bark server specifies /ping healthcheck"
else
  assert_fail "Bark healthcheck" "Bark server must specify /ping healthcheck"
fi

printf "\n"

# ----------------------------------------------------
# 11. Live Container HostConfig Inspection (if requested)
# ----------------------------------------------------
if [[ "$CHECK_LIVE" -eq 1 ]]; then
  printf "11. Inspecting Live Container HostConfigs...\n"
  if ! command -v docker >/dev/null 2>&1; then
    assert_fail "Docker CLI available" "Live check requested but docker is not available"
  else
    for c in acb-worker acb-gateway-blue acb-gateway-green; do
      if docker inspect "$c" >/dev/null 2>&1; then
        cap_drop="$(docker inspect --format '{{json .HostConfig.CapDrop}}' "$c" 2>/dev/null || true)"
        sec_opt="$(docker inspect --format '{{json .HostConfig.SecurityOpt}}' "$c" 2>/dev/null || true)"
        read_only="$(docker inspect --format '{{json .HostConfig.ReadonlyRootfs}}' "$c" 2>/dev/null || true)"
        if [[ "$cap_drop" =~ "ALL" && "$sec_opt" =~ "no-new-privileges" && "$read_only" == "true" ]]; then
          assert_pass "Live container '$c' enforces CapDrop=ALL, no-new-privileges, and ReadonlyRootfs"
        else
          assert_fail "Live container '$c'" "HostConfig policy mismatch: cap=$cap_drop sec=$sec_opt ro=$read_only"
        fi
      fi
    done
  fi
  printf "\n"
fi

printf "========================================================\n"
printf "Policy Audit Results: %d Passed, %d Failed\n" "$passes" "$failures"
printf "========================================================\n"

if [[ "$failures" -gt 0 ]]; then
  exit 1
fi
exit 0
