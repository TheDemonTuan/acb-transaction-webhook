#!/usr/bin/env bash
# Compare an expected canonical release document with the running containers/controller.
set -Eeuo pipefail
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deploy/lib.sh
source "$SCRIPT_DIR/lib.sh"
STATE_FILE="${CURRENT_RELEASE_FILE:-${DEPLOY_PATH:-$(cd -- "$SCRIPT_DIR/.." && pwd)}/state/current-release.json}"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --state) STATE_FILE="$2"; shift 2 ;;
    *) log_error "Unknown drift verifier argument: $1"; exit 1 ;;
  esac
done
[[ -s "$STATE_FILE" ]] || { log_error "Canonical release state is missing: $STATE_FILE"; exit 1; }
command -v docker >/dev/null 2>&1 || { log_error "Docker is required for runtime drift verification."; exit 1; }

mapfile -t expectations < <(python3 - "$STATE_FILE" <<'PY'
import json, re, sys
state = json.load(open(sys.argv[1], encoding="utf-8"))
if state.get("schema_version") not in (1, 2) or state.get("status") != "COMPLETED":
    raise SystemExit("invalid canonical release state")
slots = state.get("active_slots", {})
images = state.get("images", {})
gw = slots.get("gateway")
fe = slots.get("frontend")
if gw not in {"blue", "green"}:
    raise SystemExit("invalid canonical gateway slot")
rows = [
    (f"acb-gateway-{gw}", images.get("gateway", {}).get(gw, "")),
    ("acb-worker", images.get("worker", "")),
    ("acb-auth-browser", images.get("auth_browser", "")),
    ("acb-tts-gateway", images.get("tts", "")),
    ("acb-bark", images.get("bark", "")),
]
if fe in {"blue", "green"}:
    rows.append((f"acb-frontend-{fe}", images.get("frontend", "")))
digest = re.compile(r"^[^\s]+@sha256:[a-f0-9]{64}$")
for container, image in rows:
    if not digest.match(image):
        raise SystemExit(f"invalid expected image for {container}")
    print(f"{container}\t{image}")
controller = state.get("failover_controller", {})
if controller.get("sha256"):
    print(f"@controller\t{controller['sha256']}")
if controller.get("bundle_sha256"):
    print(f"@controller_bundle\t{controller['bundle_sha256']}")
PY
)

failures=0
for row in "${expectations[@]}"; do
  name="${row%%$'\t'*}"
  expected="${row#*$'\t'}"
  if [[ "$name" == "@controller" ]]; then
    controller_path="${FAILOVER_INSTALL_DIR:-/opt/platform/failover}/vps-failover-controller.py"
    actual="$(sha256sum "$controller_path" 2>/dev/null | awk '{print $1}' || true)"
    if [[ "$actual" != "$expected" ]]; then
      log_error "PRODUCTION_DRIFT component=failover_controller expected=$expected actual=${actual:-missing}"
      failures=$((failures + 1))
    fi
    continue
  fi
  if [[ "$name" == "@controller_bundle" ]]; then
    controller_root="${FAILOVER_INSTALL_DIR:-/opt/platform/failover}"
    registry_root="${FAILOVER_REGISTRY_DIR:-/etc/vps-failover/apps.d}"
    systemd_root="${FAILOVER_SYSTEMD_DIR:-/etc/systemd/system}"
    actual="$(python3 - "$controller_root" "$registry_root" "$systemd_root" <<'PY' 2>/dev/null || true
import hashlib, pathlib, sys
controller, registry, systemd = map(pathlib.Path, sys.argv[1:])
paths = [
    ("vps-failover-controller.py", controller / "vps-failover-controller.py"),
    ("vps-failover-controller.service", systemd / "vps-failover-controller.service"),
    ("vps-failover-reconcile.service", systemd / "vps-failover-reconcile.service"),
    ("vps-failover-reconcile.timer", systemd / "vps-failover-reconcile.timer"),
    ("apps.d/acb.json", registry / "acb.json"),
    ("apps.d/auth-browser.json", registry / "auth-browser.json"),
    ("apps.d/worker.json", registry / "worker.json"),
]
h = hashlib.sha256()
for name, path in paths:
    h.update(name.encode() + b"\0" + path.read_bytes() + b"\0")
print(h.hexdigest())
PY
)"
    if [[ "$actual" != "$expected" ]]; then
      log_error "PRODUCTION_DRIFT component=failover_controller_bundle expected=$expected actual=${actual:-missing}"
      failures=$((failures + 1))
    fi
    continue
  fi
  actual="$(docker inspect --format '{{.Config.Image}}' "$name" 2>/dev/null || true)"
  running="$(docker inspect --format '{{.State.Running}}' "$name" 2>/dev/null || true)"
  health="$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{end}}' "$name" 2>/dev/null || true)"
  if [[ "$actual" != "$expected" || "$running" != "true" || "$health" == "unhealthy" ]]; then
    log_error "PRODUCTION_DRIFT component=$name expected=$expected actual=${actual:-missing} running=${running:-unknown} health=${health:-none}"
    failures=$((failures + 1))
  fi
done

actual_gateway_slot="$(get_active_slot 2>/dev/null || true)"
expected_gateway_slot="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["active_slots"]["gateway"])' "$STATE_FILE")"
if [[ "$actual_gateway_slot" != "$expected_gateway_slot" ]]; then
  log_error "PRODUCTION_DRIFT component=gateway_slot expected=$expected_gateway_slot actual=${actual_gateway_slot:-unknown}"
  failures=$((failures + 1))
fi
route_slot=""
if [[ -f "$ACB_CONFIG" ]]; then
  grep -q 'acb-web-blue' "$ACB_CONFIG" && route_slot=blue
  grep -q 'acb-web-green' "$ACB_CONFIG" && route_slot=green
fi
if [[ "$route_slot" != "$expected_gateway_slot" ]]; then
  log_error "PRODUCTION_DRIFT component=gateway_route expected=$expected_gateway_slot actual=${route_slot:-unknown}"
  failures=$((failures + 1))
fi

if [[ "$failures" -gt 0 ]]; then
  log_error "Runtime drift verification failed with $failures mismatch(es)."
  exit 1
fi
log_info "Runtime matches canonical release state."
