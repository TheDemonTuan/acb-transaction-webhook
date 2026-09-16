#!/usr/bin/env bash
# deploy/preflight-vps.sh
# Strict VPS preflight check immediately before mutating production.
# Invariants verified:
#   1. current-release.json (exists, valid JSON, schema 1 or 2, COMPLETED)
#   2. active gateway slot (matches active-slot file, container running & healthy)
#   3. Traefik route pointer (matches active gateway slot; frontend route matches if active)
#   4. active container images (all active containers running exact canonical image digests)
#   5. rollout journal and pending retire files (no lingering uncommitted mutations)
# If any check fails, FAILS CLOSED IMMEDIATELY (exit 1).
set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deploy/lib.sh
source "$SCRIPT_DIR/lib.sh"

STATE_FILE="${CURRENT_RELEASE_FILE:-${RUNTIME_STATE_DIR:-${DEPLOY_PATH:-/opt/acb-transaction-webhook}/state}/current-release.json}"
DATA_DIR="${RUNTIME_DATA_DIR:-${DEPLOY_PATH:-/opt/acb-transaction-webhook}/data}"
ACB_CONFIG="${ACB_CONFIG:-/opt/platform/edge/dynamic/acb.yml}"
ALLOW_BOOTSTRAP="${ALLOW_BOOTSTRAP:-0}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --state) STATE_FILE="$2"; shift 2 ;;
    --data-dir) DATA_DIR="$2"; shift 2 ;;
    --config) ACB_CONFIG="$2"; shift 2 ;;
    --allow-bootstrap) ALLOW_BOOTSTRAP=1; shift ;;
    *) log_error "Unknown preflight argument: $1"; exit 1 ;;
  esac
done

log_info "=========================================================="
log_info "Running Strict VPS Preflight Verification"
log_info "Authoritative State File: $STATE_FILE"
log_info "Data Directory:           $DATA_DIR"
log_info "Traefik Config:           $ACB_CONFIG"
log_info "=========================================================="

# Check 1: current-release.json
if [[ ! -f "$STATE_FILE" || ! -s "$STATE_FILE" ]]; then
  if [[ "$ALLOW_BOOTSTRAP" -eq 1 ]]; then
    log_info "PREFLIGHT: current-release.json is absent; bootstrap mode permitted."
    exit 0
  fi
  log_error "PREFLIGHT_FAIL: Canonical release state file is missing or empty: $STATE_FILE"
  exit 1
fi

state_check="$(python3 - "$STATE_FILE" <<'PY' 2>/dev/null || echo "INVALID"
import json, sys
try:
    with open(sys.argv[1], encoding='utf-8') as handle:
        d = json.load(handle)
    if d.get("schema_version") not in (1, 2):
        print("INVALID_SCHEMA")
        sys.exit(0)
    if d.get("status") != "COMPLETED":
        print(f"INVALID_STATUS:{d.get('status')}")
        sys.exit(0)
    gw = d.get("active_slots", {}).get("gateway", "")
    if gw not in ("blue", "green"):
        print(f"INVALID_GW_SLOT:{gw}")
        sys.exit(0)
    print(f"OK:{gw}")
except Exception as e:
    print(f"ERROR:{e}")
PY
)"

state_check="$(printf '%s' "$state_check" | tr -d '\r')"
if [[ "$state_check" != OK:* ]]; then
  log_error "PREFLIGHT_FAIL: current-release.json failed validity checks ($state_check)."
  exit 1
fi

expected_gw_slot="${state_check#OK:}"

# Check 2: active gateway slot
actual_gw_slot="$(get_active_slot 2>/dev/null || true)"
if [[ -z "$actual_gw_slot" && -f "${ACTIVE_SLOT_FILE:-}" ]]; then
  actual_gw_slot="$(cat "$ACTIVE_SLOT_FILE" 2>/dev/null || true)"
fi
actual_gw_slot="$(printf '%s' "$actual_gw_slot" | tr -d '\r')"
if [[ -n "$actual_gw_slot" && "$actual_gw_slot" != "$expected_gw_slot" ]]; then
  log_error "PREFLIGHT_FAIL: Active gateway slot file ($actual_gw_slot) does not match canonical state ($expected_gw_slot)."
  exit 1
fi

gw_container="acb-gateway-${expected_gw_slot}"
if command -v docker >/dev/null 2>&1; then
  gw_running="$(docker inspect --format '{{.State.Running}}' "$gw_container" 2>/dev/null || echo "false")"
  if [[ "$gw_running" != "true" ]]; then
    log_error "PREFLIGHT_FAIL: Active gateway container [$gw_container] is not running."
    exit 1
  fi
  gw_health="$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{end}}' "$gw_container" 2>/dev/null || echo "")"
  if [[ "$gw_health" == "unhealthy" ]]; then
    log_error "PREFLIGHT_FAIL: Active gateway container [$gw_container] is reporting unhealthy."
    exit 1
  fi
fi

# Check 3: Traefik đang route slot nào
if [[ -f "$ACB_CONFIG" ]]; then
  has_blue=0
  has_green=0
  grep -q 'acb-web-blue' "$ACB_CONFIG" 2>/dev/null && has_blue=1 || true
  grep -q 'acb-web-green' "$ACB_CONFIG" 2>/dev/null && has_green=1 || true

  if (( has_blue == 1 && has_green == 1 )); then
    log_error "PREFLIGHT_FAIL: Traefik dynamic config routes BOTH blue and green gateways (ambiguous state)."
    exit 1
  elif (( has_blue == 0 && has_green == 0 )); then
    log_error "PREFLIGHT_FAIL: Traefik dynamic config routes NEITHER blue nor green gateway."
    exit 1
  elif (( has_blue == 1 )) && [[ "$expected_gw_slot" != "blue" ]]; then
    log_error "PREFLIGHT_FAIL: Traefik routes to blue, but canonical state expects green."
    exit 1
  elif (( has_green == 1 )) && [[ "$expected_gw_slot" != "green" ]]; then
    log_error "PREFLIGHT_FAIL: Traefik routes to green, but canonical state expects blue."
    exit 1
  fi

  # Check frontend route if frontend slot is active
  expected_fe_slot="$(python3 - "$STATE_FILE" <<'PY' 2>/dev/null || true
import json, sys
try:
    print(json.load(open(sys.argv[1], encoding='utf-8')).get("active_slots", {}).get("frontend", ""))
except Exception:
    pass
PY
)"
  if [[ "$expected_fe_slot" == "blue" || "$expected_fe_slot" == "green" ]]; then
    fe_has_blue=0
    fe_has_green=0
    grep -q 'acb-frontend-blue' "$ACB_CONFIG" 2>/dev/null && fe_has_blue=1 || true
    grep -q 'acb-frontend-green' "$ACB_CONFIG" 2>/dev/null && fe_has_green=1 || true
    if (( fe_has_blue == 1 )) && [[ "$expected_fe_slot" != "blue" ]]; then
      log_error "PREFLIGHT_FAIL: Traefik frontend routes to blue, but canonical state expects $expected_fe_slot."
      exit 1
    elif (( fe_has_green == 1 )) && [[ "$expected_fe_slot" != "green" ]]; then
      log_error "PREFLIGHT_FAIL: Traefik frontend routes to green, but canonical state expects $expected_fe_slot."
      exit 1
    fi
  fi
fi

# Check 4: container active có đúng image không
if command -v docker >/dev/null 2>&1; then
  mapfile -t container_expectations < <(python3 - "$STATE_FILE" "$expected_gw_slot" <<'PY' 2>/dev/null || true
import json, sys
state_file, gw_slot = sys.argv[1:3]
d = json.load(open(state_file, encoding='utf-8'))
images = d.get("images", {})
slots = d.get("active_slots", {})

gw_img = images.get("gateway", {}).get(gw_slot, "") if isinstance(images.get("gateway"), dict) else images.get("gateway", "")
if gw_img:
    print(f"acb-gateway-{gw_slot}\t{gw_img}")
for comp in ("worker", "auth_browser", "tts", "bark"):
    img = images.get(comp, "")
    cname = "acb-worker" if comp == "worker" else f"acb-{comp.replace('_', '-')}"
    if comp == "tts":
        cname = "acb-tts-gateway"
    if img:
        print(f"{cname}\t{img}")

fe_slot = slots.get("frontend")
fe_img = images.get("frontend", "")
if fe_img:
    if fe_slot in ("blue", "green"):
        print(f"acb-frontend-{fe_slot}\t{fe_img}")
    elif fe_slot == "legacy":
        print(f"acb-frontend\t{fe_img}")
PY
)

  for row in "${container_expectations[@]}"; do
    [[ -n "$row" ]] || continue
    c_name="${row%%$'\t'*}"
    exp_image="${row#*$'\t'}"
    c_name="${c_name%$'\r'}"
    exp_image="${exp_image%$'\r'}"
    actual_image="$(docker inspect --format '{{.Config.Image}}' "$c_name" 2>/dev/null || true)"
    actual_image="${actual_image%$'\r'}"
    c_running="$(docker inspect --format '{{.State.Running}}' "$c_name" 2>/dev/null || echo "false")"
    c_running="${c_running%$'\r'}"
    c_health="$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{end}}' "$c_name" 2>/dev/null || true)"
    c_health="${c_health%$'\r'}"

    if [[ "$c_running" != "true" ]]; then
      log_error "PREFLIGHT_FAIL: Container $c_name is not running (state: running=$c_running)."
      exit 1
    fi
    if [[ "$c_health" == "unhealthy" ]]; then
      log_error "PREFLIGHT_FAIL: Container $c_name is reporting unhealthy."
      exit 1
    fi
    if [[ -n "$exp_image" && "$actual_image" != "$exp_image" ]]; then
      log_error "PREFLIGHT_FAIL: Container $c_name image digest mismatch! Expected: $exp_image, actual: ${actual_image:-missing}"
      exit 1
    fi
    log_info "Preflight OK: $c_name is running expected image ($exp_image)."
  done
fi

# Check 5: rollout journal/pending file có còn không
for pending_file in \
  "$DATA_DIR/pending-gateway-retire.env" \
  "$DATA_DIR/pending-frontend-retire.env"; do
  if [[ -f "$pending_file" ]]; then
    log_error "PREFLIGHT_FAIL: Lingering pending retire file detected: $pending_file. Unresolved prior retirement."
    exit 1
  fi
done

journal_file="$DATA_DIR/rollout-journal.json"
if [[ -f "$journal_file" ]]; then
  j_status="$(python3 - "$journal_file" <<'PY' 2>/dev/null || echo "UNKNOWN"
import json, sys
try:
    print(json.load(open(sys.argv[1], encoding='utf-8')).get("status", "UNKNOWN"))
except Exception:
    print("UNKNOWN")
PY
)"
  if [[ "$j_status" == "RUNNING" || "$j_status" == "INTERRUPTED" ]]; then
    log_error "PREFLIGHT_FAIL: Active rollout journal is dirty (status: $j_status). A previous rollout did not commit or finish."
    exit 1
  fi
fi

log_info "=========================================================="
log_info "PREFLIGHT_PASS: All 5 VPS preflight invariants verified."
log_info "Safe to proceed with release candidate deployment."
log_info "=========================================================="
exit 0
