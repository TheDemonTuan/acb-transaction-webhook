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
elif fe == "legacy":
    rows.append(("acb-frontend", images.get("frontend", "")))
dbtool_img = images.get("dbtool", "")
digest = re.compile(r"^[^\s]+@sha256:[a-f0-9]{64}$")
if dbtool_img and not digest.match(dbtool_img):
    raise SystemExit("invalid expected image for dbtool")
for container, image in rows:
    if not digest.match(image):
        raise SystemExit(f"invalid expected image for {container}")
    print(f"{container}\t{image}")
rel_dir = state.get("release_dir", "")
if rel_dir:
    print(f"@release_dir\t{rel_dir}")
cfg = state.get("config", {})
if cfg.get("compose_bundle_sha256"):
    print(f"@compose_bundle\t{cfg['compose_bundle_sha256']}")
if cfg.get("traefik_template_sha256"):
    print(f"@traefik_template\t{cfg['traefik_template_sha256']}")
if cfg.get("platform_bundle_sha256"):
    print(f"@platform_bundle\t{cfg['platform_bundle_sha256']}")
controller = state.get("failover_controller", {})
if controller.get("sha256"):
    print(f"@controller\t{controller['sha256']}")
if controller.get("bundle_sha256"):
    print(f"@controller_bundle\t{controller['bundle_sha256']}")
PY
)

failures=0
release_dir=""
for row in "${expectations[@]}"; do
  name="${row%%$'\t'*}"
  expected="${row#*$'\t'}"
  if [[ "$name" == "@release_dir" ]]; then
    release_dir="$expected"
    continue
  fi
  if [[ "$name" == "@compose_bundle" ]]; then
    compose_root=""
    if [[ -n "${release_dir:-}" && -d "$release_dir/compose" ]]; then
      compose_root="$release_dir/compose"
    elif [[ -d "${COMPOSE_ROOT:-}" ]]; then
      compose_root="$COMPOSE_ROOT"
    elif [[ -d "${DEPLOY_PATH:-}/compose" ]]; then
      compose_root="${DEPLOY_PATH}/compose"
    elif [[ -d "$SCRIPT_DIR/compose" ]]; then
      compose_root="$SCRIPT_DIR/compose"
    fi
    if [[ -z "$compose_root" || ! -d "$compose_root" ]]; then
      log_error "PRODUCTION_DRIFT component=compose_bundle expected=$expected actual=missing_directory"
      failures=$((failures + 1))
    else
      actual_bundle="$(python3 - "$compose_root" <<'PY' 2>/dev/null || true
import hashlib, pathlib, sys
root = pathlib.Path(sys.argv[1])
h = hashlib.sha256()
for p in sorted(root.glob("*.yaml")):
    rel = f"compose/{p.name}"
    h.update(rel.encode() + b"\0" + p.read_bytes() + b"\0")
print(h.hexdigest())
PY
)"
      if [[ "$actual_bundle" != "$expected" ]]; then
        log_error "PRODUCTION_DRIFT component=compose_bundle expected=$expected actual=${actual_bundle:-mismatch}"
        failures=$((failures + 1))
      fi
    fi
    continue
  fi
  if [[ "$name" == "@traefik_template" ]]; then
    traefik_path=""
    if [[ -n "${release_dir:-}" && -f "$release_dir/lib/traefik.sh" ]]; then
      traefik_path="$release_dir/lib/traefik.sh"
    elif [[ -f "${DEPLOY_PATH:-}/lib/traefik.sh" ]]; then
      traefik_path="${DEPLOY_PATH}/lib/traefik.sh"
    elif [[ -f "$SCRIPT_DIR/lib/traefik.sh" ]]; then
      traefik_path="$SCRIPT_DIR/lib/traefik.sh"
    fi
    actual_traefik="$(sha256sum "$traefik_path" 2>/dev/null | awk '{print $1}' || true)"
    if [[ "$actual_traefik" != "$expected" ]]; then
      log_error "PRODUCTION_DRIFT component=traefik_template expected=$expected actual=${actual_traefik:-missing}"
      failures=$((failures + 1))
    fi
    continue
  fi
  if [[ "$name" == "@platform_bundle" ]]; then
    platform_path=""
    if [[ -n "${release_dir:-}" && -f "$release_dir/runtime-layout.sh" ]]; then
      platform_path="$release_dir/runtime-layout.sh"
    elif [[ -f "${DEPLOY_PATH:-}/runtime-layout.sh" ]]; then
      platform_path="${DEPLOY_PATH}/runtime-layout.sh"
    elif [[ -f "$SCRIPT_DIR/runtime-layout.sh" ]]; then
      platform_path="$SCRIPT_DIR/runtime-layout.sh"
    fi
    actual_platform="$(sha256sum "$platform_path" 2>/dev/null | awk '{print $1}' || true)"
    if [[ "$actual_platform" != "$expected" ]]; then
      log_error "PRODUCTION_DRIFT component=platform_bundle expected=$expected actual=${actual_platform:-missing}"
      failures=$((failures + 1))
    fi
    continue
  fi
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
# Audit exact registry set: extra or missing files are rejected
expected_files = sorted(["acb.json", "auth-browser.json", "worker.json"])
actual_files = sorted([p.name for p in registry.glob("*.json")]) if registry.is_dir() else []
if actual_files != expected_files:
    sys.exit(2)

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
    if not path.is_file():
        sys.exit(1)
    h.update(name.encode() + b"\0" + path.read_bytes() + b"\0")
print(h.hexdigest())
PY
)"
    py_code=$?
    if [[ "$py_code" -eq 2 ]]; then
      log_error "PRODUCTION_DRIFT component=failover_registry extra or missing configuration files detected in $registry_root"
      failures=$((failures + 1))
    elif [[ "$actual" != "$expected" ]]; then
      log_error "PRODUCTION_DRIFT component=failover_controller_bundle expected=$expected actual=${actual:-missing}"
      failures=$((failures + 1))
    fi

    if [[ "${SKIP_SYSTEMD_DRIFT_CHECK:-0}" -ne 1 ]] && command -v systemctl >/dev/null 2>&1; then
      for unit in vps-failover-controller.service vps-failover-reconcile.timer; do
        if ! systemctl is-active --quiet "$unit" 2>/dev/null; then
          log_error "PRODUCTION_DRIFT component=systemd unit=$unit expected=active actual=inactive"
          failures=$((failures + 1))
        fi
        if ! systemctl is-enabled --quiet "$unit" 2>/dev/null; then
          log_error "PRODUCTION_DRIFT component=systemd unit=$unit expected=enabled actual=disabled"
          failures=$((failures + 1))
        fi
      done
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
  has_gw_blue=0
  has_gw_green=0
  grep -q 'acb-web-blue' "$ACB_CONFIG" && has_gw_blue=1 || true
  grep -q 'acb-web-green' "$ACB_CONFIG" && has_gw_green=1 || true
  if (( has_gw_blue == 1 && has_gw_green == 1 )); then
    log_error "PRODUCTION_DRIFT component=gateway_route both blue and green discovered in active route (ambiguous)"
    failures=$((failures + 1))
  elif (( has_gw_blue == 1 )); then
    route_slot="blue"
  elif (( has_gw_green == 1 )); then
    route_slot="green"
  else
    log_error "PRODUCTION_DRIFT component=gateway_route neither blue nor green discovered in route"
    failures=$((failures + 1))
  fi
fi
if [[ -n "$route_slot" && "$route_slot" != "$expected_gateway_slot" ]]; then
  log_error "PRODUCTION_DRIFT component=gateway_route expected=$expected_gateway_slot actual=${route_slot:-unknown}"
  failures=$((failures + 1))
fi

# Frontend drift verification
expected_frontend_slot="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["active_slots"].get("frontend") or "")' "$STATE_FILE" 2>/dev/null || true)"
if [[ -n "$expected_frontend_slot" ]]; then
  actual_fe_slot="$(resolve_frontend_slot_strict 2>/dev/null || cat "${FRONTEND_ACTIVE_SLOT_FILE:-}" 2>/dev/null || true)"
  if [[ "$actual_fe_slot" != "$expected_frontend_slot" ]]; then
    log_error "PRODUCTION_DRIFT component=frontend_slot expected=$expected_frontend_slot actual=${actual_fe_slot:-unknown}"
    failures=$((failures + 1))
  fi

  fe_route_slot=""
  if [[ -f "$ACB_CONFIG" ]]; then
    has_fe_blue=0
    has_fe_green=0
    grep -q 'acb-frontend-blue' "$ACB_CONFIG" && has_fe_blue=1 || true
    grep -q 'acb-frontend-green' "$ACB_CONFIG" && has_fe_green=1 || true
    if (( has_fe_blue == 1 && has_fe_green == 1 )); then
      log_error "PRODUCTION_DRIFT component=frontend_route both blue and green discovered in active route (ambiguous)"
      failures=$((failures + 1))
    elif (( has_fe_blue == 1 )); then
      fe_route_slot="blue"
    elif (( has_fe_green == 1 )); then
      fe_route_slot="green"
    elif grep -q 'http://acb-frontend:8080' "$ACB_CONFIG"; then
      fe_route_slot="legacy"
    fi
  fi
  if [[ -n "$fe_route_slot" && "$fe_route_slot" != "$expected_frontend_slot" ]]; then
    log_error "PRODUCTION_DRIFT component=frontend_route expected=$expected_frontend_slot actual=${fe_route_slot:-unknown}"
    failures=$((failures + 1))
  fi
fi

# Journal path identity check
canonical_journal="${RUNTIME_DATA_DIR:-${DEPLOY_PATH:-/opt/acb-transaction-webhook}/data}/deploy-journal.json"
if [[ -n "${TX_JOURNAL_FILE:-}" ]]; then
  actual_journal_norm="$(readlink -f "$TX_JOURNAL_FILE" 2>/dev/null || echo "$TX_JOURNAL_FILE")"
  canonical_journal_norm="$(readlink -f "$canonical_journal" 2>/dev/null || echo "$canonical_journal")"
  if [[ "$actual_journal_norm" != "$canonical_journal_norm" ]]; then
    log_error "PRODUCTION_DRIFT component=tx_journal_path expected=$canonical_journal actual=$TX_JOURNAL_FILE"
    failures=$((failures + 1))
  fi
fi

if [[ "$failures" -gt 0 ]]; then
  log_error "Runtime drift verification failed with $failures mismatch(es)."
  exit 1
fi
log_info "Runtime matches canonical release state."
