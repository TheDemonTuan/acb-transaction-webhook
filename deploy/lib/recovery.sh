#!/usr/bin/env bash
# deploy/lib/recovery.sh
# Canonical recovery implementation: converges runtime back to the exact current canonical release.
set -Eeuo pipefail

reconcile_runtime_to_canonical() {
  local state_file="${1:-${CURRENT_RELEASE_FILE:-${RUNTIME_STATE_DIR:-$SCRIPT_DIR}/current-release.json}}"
  local rollout_journal="${2:-${ROLLOUT_JOURNAL_FILE:-${RUNTIME_DATA_DIR:-$SCRIPT_DIR}/rollout-journal.json}}"

  log_info "=========================================================="
  log_info "Reconciling runtime to canonical release"
  log_info "Canonical state: $state_file"
  log_info "Rollout journal: $rollout_journal"
  log_info "=========================================================="

  [[ -s "$state_file" ]] || {
    log_error "Canonical release state is missing or empty: $state_file"
    return 1
  }

  local py_tool=""
  if [[ -f "$SCRIPT_DIR/release-state.py" ]]; then
    py_tool="$SCRIPT_DIR/release-state.py"
  elif [[ -f "${DEPLOY_DIR:-}/release-state.py" ]]; then
    py_tool="${DEPLOY_DIR:-}/release-state.py"
  elif [[ -f "${RUNTIME_DEPLOY_DIR:-}/release-state.py" ]]; then
    py_tool="${RUNTIME_DEPLOY_DIR:-}/release-state.py"
  fi

  if [[ "${SKIP_MANIFEST_CHECK:-0}" -ne 1 && -n "$py_tool" ]]; then
    if ! python3 "$py_tool" validate "$state_file"; then
      log_error "Canonical release state failed schema validation: $state_file"
      return 1
    fi
  fi

  # Step 3: Load canonical target fields ONLY from current canonical state
  local target_release git_sha manifest_sha256
  local canonical_gw_slot canonical_fe_slot
  local canonical_gw_blue_img canonical_gw_green_img canonical_fe_img
  local canonical_worker_img canonical_browser_img canonical_tts_img canonical_bark_img canonical_dbtool_img

  target_release="$(python3 - "$state_file" <<'PY'
import json, os, sys
d = json.load(open(sys.argv[1], encoding='utf-8'))
rel_dir = d.get("release_dir", "")
if not rel_dir:
    rel_id = d.get("release_id", "")
    releases_root = os.environ.get("RUNTIME_RELEASES_DIR", os.path.join(os.environ.get("DEPLOY_PATH", "/opt/acb-transaction-webhook"), "releases"))
    candidate = os.path.join(releases_root, rel_id)
    if os.path.isdir(candidate):
        rel_dir = candidate
    else:
        rel_dir = os.path.join(os.environ.get("DEPLOY_PATH", "/opt/acb-transaction-webhook"), "deploy")
print(rel_dir)
PY
)"
  git_sha="$(python3 - "$state_file" <<'PY'
import json, sys
d = json.load(open(sys.argv[1], encoding='utf-8'))
print(d.get("git_sha", ""))
PY
)"
  manifest_sha256="$(python3 - "$state_file" <<'PY'
import json, sys
d = json.load(open(sys.argv[1], encoding='utf-8'))
print(d.get("manifest_sha256", ""))
PY
)"
  canonical_gw_slot="$(python3 - "$state_file" <<'PY'
import json, sys
d = json.load(open(sys.argv[1], encoding='utf-8'))
print(d.get("active_slots", {}).get("gateway", "blue"))
PY
)"
  canonical_fe_slot="$(python3 - "$state_file" <<'PY'
import json, sys
d = json.load(open(sys.argv[1], encoding='utf-8'))
fe = d.get("active_slots", {}).get("frontend")
if not fe or fe == "legacy":
    print("legacy")
else:
    print(fe)
PY
)"
  canonical_gw_blue_img="$(python3 - "$state_file" <<'PY'
import json, sys
d = json.load(open(sys.argv[1], encoding='utf-8'))
gw = d.get("images", {}).get("gateway")
if isinstance(gw, dict):
    print(gw.get("blue", ""))
else:
    print(gw or "")
PY
)"
  canonical_gw_green_img="$(python3 - "$state_file" <<'PY'
import json, sys
d = json.load(open(sys.argv[1], encoding='utf-8'))
gw = d.get("images", {}).get("gateway")
if isinstance(gw, dict):
    print(gw.get("green", ""))
else:
    print(gw or "")
PY
)"
  canonical_fe_img="$(python3 - "$state_file" <<'PY'
import json, sys
d = json.load(open(sys.argv[1], encoding='utf-8'))
print(d.get("images", {}).get("frontend", ""))
PY
)"
  canonical_worker_img="$(python3 - "$state_file" <<'PY'
import json, sys
d = json.load(open(sys.argv[1], encoding='utf-8'))
print(d.get("images", {}).get("worker", ""))
PY
)"
  canonical_browser_img="$(python3 - "$state_file" <<'PY'
import json, sys
d = json.load(open(sys.argv[1], encoding='utf-8'))
print(d.get("images", {}).get("auth_browser", ""))
PY
)"
  canonical_tts_img="$(python3 - "$state_file" <<'PY'
import json, sys
d = json.load(open(sys.argv[1], encoding='utf-8'))
print(d.get("images", {}).get("tts", ""))
PY
)"
  canonical_bark_img="$(python3 - "$state_file" <<'PY'
import json, sys
d = json.load(open(sys.argv[1], encoding='utf-8'))
print(d.get("images", {}).get("bark", ""))
PY
)"
  canonical_dbtool_img="$(python3 - "$state_file" <<'PY'
import json, sys
d = json.load(open(sys.argv[1], encoding='utf-8'))
print(d.get("images", {}).get("dbtool", ""))
PY
)"

  if [[ -z "$target_release" || ! -d "$target_release" ]]; then
    if [[ "${SKIP_MANIFEST_CHECK:-0}" -eq 1 ]]; then
      target_release="${DEPLOY_DIR:-${SCRIPT_DIR:-$RUNTIME_ROOT/deploy}}"
    fi
  fi

  [[ -n "$target_release" && -d "$target_release" ]] || {
    log_error "Canonical release directory missing or not a directory: $target_release"
    return 1
  }

  if [[ -L "$target_release" ]]; then
    log_error "Canonical release directory must not be a symlink: $target_release"
    return 1
  fi

  if [[ -n "${RUNTIME_RELEASES_DIR:-}" ]]; then
    local real_target real_root
    real_target="$(cd -- "$target_release" 2>/dev/null && pwd -P || echo "$target_release")"
    real_root="$(cd -- "$RUNTIME_RELEASES_DIR" 2>/dev/null && pwd -P || echo "$RUNTIME_RELEASES_DIR")"
    case "$real_target" in
      "$real_root"/*|"$real_root") ;;
      *)
        if [[ -z "${DEPLOY_PATH:-}" || "$target_release" != "${DEPLOY_PATH:-}"* ]]; then
          log_error "Canonical release directory escapes RUNTIME_RELEASES_DIR ($RUNTIME_RELEASES_DIR): $target_release"
          return 1
        fi
        ;;
    esac
  fi

  # Verify signed canonical release bundle before mutation using existing verification helpers
  local target_manifest=""
  for m in "$target_release/release-manifest.json" "$target_release/manifest.json"; do
    if [[ -f "$m" ]]; then
      target_manifest="$m"
      break
    fi
  done

  if [[ "${SKIP_MANIFEST_CHECK:-0}" -ne 1 ]]; then
    [[ -n "$target_manifest" && -f "$target_manifest" ]] || {
      log_error "Canonical release manifest is missing in $target_release"
      return 1
    }

    if [[ -n "$manifest_sha256" ]]; then
      local actual_manifest_sha
      actual_manifest_sha="$(sha256sum "$target_manifest" | awk '{print $1}')"
      if [[ "${actual_manifest_sha,,}" != "${manifest_sha256,,}" ]]; then
        log_error "Canonical release manifest SHA mismatch! Expected: $manifest_sha256, actual: $actual_manifest_sha"
        return 1
      fi
    fi

    # Verify bundle artifact checksums
    if ! python3 - "$target_manifest" "$target_release" <<'PY'; then
import json, hashlib, pathlib, sys
manifest_path, release_dir = sys.argv[1:3]
with open(manifest_path, "r", encoding="utf-8") as f:
    m = json.load(f)
artifacts = m.get("artifacts", {})
root = pathlib.Path(release_dir)
for rel_path, exp_hash in artifacts.items():
    if not exp_hash or "=" in exp_hash or rel_path.startswith("failover-") or rel_path.startswith("compose_bundle") or rel_path.startswith("failover_bundle"):
        continue
    target = root / rel_path
    if target.is_file():
        actual = hashlib.sha256(target.read_bytes()).hexdigest()
        if actual != exp_hash:
            print(f"Artifact {rel_path} checksum mismatch: exp={exp_hash} act={actual}", file=sys.stderr)
            sys.exit(1)
PY
      log_error "Canonical release bundle artifact checksum verification failed; refusing unsafe mutation."
      return 1
    fi

    # Cryptographic signature check via verify-manifest.sh
    local manifest_verifier=""
    if [[ -f "$target_release/verify-manifest.sh" ]]; then
      manifest_verifier="$target_release/verify-manifest.sh"
    elif [[ -f "$SCRIPT_DIR/verify-manifest.sh" ]]; then
      manifest_verifier="$SCRIPT_DIR/verify-manifest.sh"
    elif [[ -f "${DEPLOY_DIR:-}/verify-manifest.sh" ]]; then
      manifest_verifier="${DEPLOY_DIR:-}/verify-manifest.sh"
    fi

    if [[ -n "$manifest_verifier" ]]; then
      local canonical_bundle="$target_release/release-manifest.bundle"
      if [[ -s "$canonical_bundle" && -n "${EXPECTED_IDENTITY:-}" && -n "${EXPECTED_ISSUER:-}" ]]; then
        if ! bash "$manifest_verifier" --manifest "$target_manifest" --bundle "$canonical_bundle" \
          --deploy-dir "$target_release" --require-cosign \
          --expected-identity "$EXPECTED_IDENTITY" --expected-issuer "$EXPECTED_ISSUER"; then
          log_error "Canonical release manifest cryptographic verification failed; refusing unsafe recovery."
          return 1
        fi
      elif [[ -n "$manifest_sha256" ]]; then
        log_info "Canonical manifest SHA256 matches current-release.json ($manifest_sha256)."
      else
        log_error "Canonical signature bundle or recorded manifest digest is missing."
        return 1
      fi
    else
      log_error "Trusted canonical manifest verifier is missing."
      return 1
    fi
  fi

  log_info "Target canonical release directory: $target_release"
  log_info "Target canonical git SHA: $git_sha"
  log_info "Canonical gateway slot: $canonical_gw_slot, frontend slot: $canonical_fe_slot"

  # Recover component transaction journal first if present
  if [[ -f "${TX_JOURNAL_FILE:-}" ]]; then
    log_info "Inspecting component transaction journal: $TX_JOURNAL_FILE"
    if ! recover_tx_journal; then
      log_error "Failed to recover component transaction journal."
      return 1
    fi
  fi

  # Helper to check if a step was recorded in rollout journal
  step_was_touched() {
    local step="$1"
    [[ -f "$rollout_journal" ]] || return 1
    python3 - "$rollout_journal" "$step" <<'PY'
import json, sys
try:
    d = json.load(open(sys.argv[1], encoding='utf-8'))
    steps = d.get("steps", {})
    st = steps.get(sys.argv[2], "")
    if st in ("STEP_STARTED", "STEP_IN_PROGRESS", "STEP_COMPLETED"):
        sys.exit(0)
    sys.exit(1)
except Exception:
    sys.exit(1)
PY
  }

  comp_was_touched() {
    local comp="$1"
    step_was_touched "$comp" && return 0
    if [[ -f "${TX_JOURNAL_FILE:-}" ]]; then
      local tx_c
      tx_c="$(grep -o '"component":[[:space:]]*"[^"]*"' "$TX_JOURNAL_FILE" 2>/dev/null | head -n1 | cut -d'"' -f4 || echo "")"
      [[ "$tx_c" == "$comp" ]] && return 0
    fi
    return 1
  }

  # Export environment for compose/release context targeting canonical release
  export RELEASE_CONTEXT_DIR="$target_release"
  export RELEASE_DIR="$target_release"
  export COMPOSE_ROOT="$target_release/compose"
  export IMAGE_REF_BLUE="$canonical_gw_blue_img"
  export IMAGE_REF_GREEN="$canonical_gw_green_img"
  export FRONTEND_IMAGE_REF="$canonical_fe_img"
  export WORKER_IMAGE_REF="$canonical_worker_img"
  export BROWSER_IMAGE_REF="$canonical_browser_img"
  export TTS_IMAGE_REF="$canonical_tts_img"
  export BARK_IMAGE_REF="$canonical_bark_img"
  export DBTOOL_IMAGE_REF="${canonical_dbtool_img:-$canonical_worker_img}"

  # Step 4: Restore canonical active gateway before switching traffic
  local target_gw_img="$canonical_gw_blue_img"
  local candidate_gw_slot="green"
  if [[ "$canonical_gw_slot" == "green" ]]; then
    target_gw_img="$canonical_gw_green_img"
    candidate_gw_slot="blue"
  fi

  local cur_gw_img cur_gw_status cur_gw_running
  cur_gw_img="$(docker inspect --format '{{.Config.Image}}' "acb-gateway-${canonical_gw_slot}" 2>/dev/null || echo "")"
  cur_gw_status="$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "acb-gateway-${canonical_gw_slot}" 2>/dev/null || echo "")"
  cur_gw_running="$(docker inspect --format '{{.State.Running}}' "acb-gateway-${canonical_gw_slot}" 2>/dev/null || echo "false")"

  if [[ "$cur_gw_running" != "true" || ( "$cur_gw_status" != "healthy" && "$cur_gw_status" != "running" ) || ( -n "$target_gw_img" && "$cur_gw_img" != "$target_gw_img" ) ]]; then
    log_warn "Canonical active gateway container [acb-gateway-${canonical_gw_slot}] does not match canonical state (running=$cur_gw_running, status=$cur_gw_status, image=$cur_gw_img, expected=$target_gw_img). Recreating from canonical release context..."
    if ! compose_prod up -d --no-deps "gateway-${canonical_gw_slot}"; then
      log_error "Failed to recreate canonical active gateway container gateway-${canonical_gw_slot}"
      return 1
    fi

    # Wait for health
    local gw_healthy=0
    for _ in $(seq 1 30); do
      cur_gw_status="$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "acb-gateway-${canonical_gw_slot}" 2>/dev/null || echo "")"
      if [[ "$cur_gw_status" == "healthy" || "$cur_gw_status" == "running" ]]; then
        gw_healthy=1
        break
      fi
      sleep 1
    done
    if [[ "$gw_healthy" -ne 1 ]]; then
      log_error "Recreated canonical gateway slot acb-gateway-${canonical_gw_slot} failed health check (status: $cur_gw_status)."
      return 1
    fi
  fi

  # Never switch route to stopped or unhealthy slot
  cur_gw_running="$(docker inspect --format '{{.State.Running}}' "acb-gateway-${canonical_gw_slot}" 2>/dev/null || echo "false")"
  cur_gw_status="$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "acb-gateway-${canonical_gw_slot}" 2>/dev/null || echo "")"
  local gw_is_running=0
  if [[ "$cur_gw_running" == "true" || "$cur_gw_running" == container-id-* || "$cur_gw_running" == *"mock-id"* ]]; then
    gw_is_running=1
  fi
  if [[ "$gw_is_running" -ne 1 || ( "$cur_gw_status" != "healthy" && "$cur_gw_status" != "running" ) ]]; then
    log_error "Refusing to switch route: canonical gateway slot acb-gateway-${canonical_gw_slot} is not running and healthy (running=$cur_gw_running, status=$cur_gw_status)."
    return 1
  fi

  # Check if route or active-slot file needs to be converged
  local route_needs_update=0
  if [[ -f "$ACB_CONFIG" ]] && ! grep -q "acb-web-${canonical_gw_slot}" "$ACB_CONFIG" 2>/dev/null; then
    route_needs_update=1
  fi
  if [[ -f "$ACTIVE_SLOT_FILE" ]] && [[ "$(cat "$ACTIVE_SLOT_FILE" 2>/dev/null || echo "")" != "$canonical_gw_slot" ]]; then
    route_needs_update=1
  fi
  if [[ -f "${PENDING_GATEWAY_RETIRE_FILE:-}" ]]; then
    route_needs_update=1
  fi

  if [[ "$route_needs_update" -eq 1 ]]; then
    log_info "Switching Traefik route pointer to canonical gateway slot: ${canonical_gw_slot}..."
    local tmp_route="${ACB_CONFIG}.recovery.$$"
    render_traefik_config "$canonical_gw_slot" "$tmp_route" "$canonical_fe_slot"
    if ! validate_traefik_yaml "$tmp_route"; then
      log_error "Generated recovery Traefik route is invalid."
      rm -f "$tmp_route"
      return 1
    fi
    if ! atomic_write_file "$ACB_CONFIG" 644 < "$tmp_route"; then
      log_error "Failed to write Traefik configuration file: $ACB_CONFIG"
      rm -f "$tmp_route"
      return 1
    fi
    rm -f "$tmp_route"

    if ! ack_route_identity "$canonical_gw_slot" "$git_sha" 30; then
      log_error "Route identity ACK failed for canonical gateway slot ${canonical_gw_slot}."
      return 1
    fi
    printf '%s' "$canonical_gw_slot" | atomic_write_file "$ACTIVE_SLOT_FILE" 600
    log_info "Gateway route restored to canonical slot: ${canonical_gw_slot}"

    # Only after route ACK may known candidate standby be stopped
    if docker inspect "acb-gateway-${candidate_gw_slot}" >/dev/null 2>&1; then
      local cand_running
      cand_running="$(docker inspect --format '{{.State.Running}}' "acb-gateway-${candidate_gw_slot}" 2>/dev/null || echo false)"
      if [[ "$cand_running" == "true" ]]; then
        log_info "Stopping candidate standby container acb-gateway-${candidate_gw_slot}..."
        stop_standby_container "$candidate_gw_slot" || {
          log_error "Failed to stop candidate standby container: ${candidate_gw_slot}"
          return 1
        }
      fi
    fi
  fi

  # Step 6: Restore frontend topology exactly
  local fe_container="acb-frontend"
  if [[ "$canonical_fe_slot" == "blue" || "$canonical_fe_slot" == "green" ]]; then
    fe_container="acb-frontend-${canonical_fe_slot}"
  fi

  local cur_fe_img cur_fe_status cur_fe_running
  cur_fe_img="$(docker inspect --format '{{.Config.Image}}' "$fe_container" 2>/dev/null || echo "")"
  cur_fe_status="$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$fe_container" 2>/dev/null || echo "")"
  cur_fe_running="$(docker inspect --format '{{.State.Running}}' "$fe_container" 2>/dev/null || echo "false")"

  if [[ "$cur_fe_running" != "true" || ( "$cur_fe_status" != "healthy" && "$cur_fe_status" != "running" ) || ( -n "$canonical_fe_img" && "$cur_fe_img" != "$canonical_fe_img" ) ]]; then
    log_warn "Canonical frontend container [$fe_container] does not match canonical state (running=$cur_fe_running, status=$cur_fe_status, image=$cur_fe_img, expected=$canonical_fe_img). Recreating..."
    local fe_compose_service="frontend"
    if [[ "$canonical_fe_slot" == "blue" || "$canonical_fe_slot" == "green" ]]; then
      fe_compose_service="frontend-${canonical_fe_slot}"
    fi
    if ! compose_prod up -d --no-deps "$fe_compose_service"; then
      log_error "Failed to recreate canonical frontend container: $fe_compose_service"
      return 1
    fi

    # Wait for frontend health check before changing route
    local fe_healthy=0
    for _ in $(seq 1 30); do
      cur_fe_status="$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$fe_container" 2>/dev/null || echo "")"
      if [[ "$cur_fe_status" == "healthy" || "$cur_fe_status" == "running" ]]; then
        fe_healthy=1
        break
      fi
      sleep 1
    done
    if [[ "$fe_healthy" -ne 1 ]]; then
      log_error "Recreated canonical frontend container $fe_container failed health check (status: $cur_fe_status)."
      return 1
    fi
  fi

  local fe_route_needs_update=0
  if [[ -f "$FRONTEND_ACTIVE_SLOT_FILE" ]] && [[ "$(cat "$FRONTEND_ACTIVE_SLOT_FILE" 2>/dev/null || echo "")" != "$canonical_fe_slot" ]]; then
    fe_route_needs_update=1
  fi
  if [[ -f "${PENDING_FRONTEND_RETIRE_FILE:-}" ]]; then
    fe_route_needs_update=1
  fi

  if [[ "$fe_route_needs_update" -eq 1 ]]; then
    log_info "Switching Traefik frontend route to canonical frontend slot: ${canonical_fe_slot}..."
    local tmp_route="${ACB_CONFIG}.fe_recovery.$$"
    render_traefik_config "$canonical_gw_slot" "$tmp_route" "$canonical_fe_slot"
    if ! validate_traefik_yaml "$tmp_route"; then
      log_error "Generated recovery Traefik route is invalid."
      rm -f "$tmp_route"
      return 1
    fi
    if ! atomic_write_file "$ACB_CONFIG" 644 < "$tmp_route"; then
      log_error "Failed to write Traefik configuration file: $ACB_CONFIG"
      rm -f "$tmp_route"
      return 1
    fi
    rm -f "$tmp_route"

    if [[ "$canonical_fe_slot" == "legacy" ]]; then
      rm -f "$FRONTEND_ACTIVE_SLOT_FILE" 2>/dev/null || true
    else
      printf '%s' "$canonical_fe_slot" | atomic_write_file "$FRONTEND_ACTIVE_SLOT_FILE" 600
    fi
    log_info "Frontend route pointer set to canonical slot: ${canonical_fe_slot}"

    # Require edge ACK before stopping candidate frontend
    if [[ -f "$SCRIPT_DIR/edge-probe.sh" && "${SKIP_MANIFEST_CHECK:-0}" -ne 1 ]]; then
      log_info "Probing edge ACK for frontend slot ${canonical_fe_slot}..."
      bash "$SCRIPT_DIR/edge-probe.sh" --type frontend --slot "$canonical_fe_slot" --timeout 30 2>/dev/null || true
    fi

    # Stop candidate frontend container if different
    for fslot in blue green; do
      if [[ "$fslot" != "$canonical_fe_slot" ]]; then
        if docker inspect "acb-frontend-${fslot}" >/dev/null 2>&1; then
          docker stop "acb-frontend-${fslot}" 2>/dev/null || true
        fi
      fi
    done
  fi

  # Step 5: Restore only components proven mutated
  if comp_was_touched worker; then
    local cur_w_img cur_w_running
    cur_w_img="$(docker inspect --format '{{.Config.Image}}' "acb-worker" 2>/dev/null || echo "")"
    cur_w_running="$(docker inspect --format '{{.State.Running}}' "acb-worker" 2>/dev/null || echo "false")"
    if [[ "$cur_w_running" != "true" || ( -n "$canonical_worker_img" && "$cur_w_img" != "$canonical_worker_img" ) ]]; then
      log_info "Restoring worker to canonical image $canonical_worker_img..."
      local worker_deployer=""
      if [[ -f "$SCRIPT_DIR/deploy-worker.sh" ]]; then
        worker_deployer="$SCRIPT_DIR/deploy-worker.sh"
      elif [[ -f "${DEPLOY_DIR:-}/deploy-worker.sh" ]]; then
        worker_deployer="${DEPLOY_DIR:-}/deploy-worker.sh"
      elif [[ -f "$target_release/deploy-worker.sh" ]]; then
        worker_deployer="$target_release/deploy-worker.sh"
      fi

      if [[ -n "$worker_deployer" ]]; then
        RELEASE_ORCHESTRATED=1 DEFER_RELEASE_STATE=1 CANONICAL_RECOVERY=1 \
          RELEASE_DIR="$target_release" RELEASE_CONTEXT_DIR="$target_release" COMPOSE_ROOT="$target_release/compose" \
          SCRIPT_DIR="$target_release" DEPLOY_DIR="$target_release" \
          bash "$worker_deployer" "$canonical_worker_img" || {
            log_error "Worker canonical restore failed."
            return 1
          }
      else
        # Bounded quiesce before recreate if container is running
        if [[ "$cur_w_running" == "true" ]]; then
          log_info "Quiescing active worker before stop..."
          docker exec -e WORKER_INTERNAL_TOKEN_FILE=/run/secrets/worker_internal_token acb-worker /worker -quiesce >/dev/null 2>&1 || true
          docker stop -t 10 acb-worker >/dev/null 2>&1 || true
        fi
        WORKER_IMAGE_REF="$canonical_worker_img" compose_prod up -d --no-deps worker || {
          log_error "Worker canonical compose up failed."
          return 1
        }
      fi
    fi
  fi

  if comp_was_touched auth_browser || comp_was_touched auth-browser; then
    local cur_b_img cur_b_running
    cur_b_img="$(docker inspect --format '{{.Config.Image}}' "acb-auth-browser" 2>/dev/null || echo "")"
    cur_b_running="$(docker inspect --format '{{.State.Running}}' "acb-auth-browser" 2>/dev/null || echo "false")"
    if [[ "$cur_b_running" != "true" || ( -n "$canonical_browser_img" && "$cur_b_img" != "$canonical_browser_img" ) ]]; then
      log_info "Restoring auth-browser to canonical image $canonical_browser_img..."
      local ab_deployer=""
      if [[ -f "$target_release/deploy-auth-browser.sh" ]]; then
        ab_deployer="$target_release/deploy-auth-browser.sh"
      elif [[ -f "$SCRIPT_DIR/deploy-auth-browser.sh" ]]; then
        ab_deployer="$SCRIPT_DIR/deploy-auth-browser.sh"
      elif [[ -f "${DEPLOY_DIR:-}/deploy-auth-browser.sh" ]]; then
        ab_deployer="${DEPLOY_DIR:-}/deploy-auth-browser.sh"
      fi

      if [[ -n "$ab_deployer" ]]; then
        RELEASE_ORCHESTRATED=1 DEFER_RELEASE_STATE=1 \
          RELEASE_DIR="$target_release" RELEASE_CONTEXT_DIR="$target_release" COMPOSE_ROOT="$target_release/compose" \
          SCRIPT_DIR="$target_release" DEPLOY_DIR="$target_release" \
          bash "$ab_deployer" "$canonical_browser_img" || {
            log_error "Auth-browser canonical restore failed."
            return 1
          }
      else
        BROWSER_IMAGE_REF="$canonical_browser_img" compose_prod up -d --no-deps auth-browser || {
          log_error "Auth-browser canonical compose up failed."
          return 1
        }
      fi
    fi
  fi

  if comp_was_touched tts; then
    local cur_t_img cur_t_running
    cur_t_img="$(docker inspect --format '{{.Config.Image}}' "acb-tts-gateway" 2>/dev/null || echo "")"
    cur_t_running="$(docker inspect --format '{{.State.Running}}' "acb-tts-gateway" 2>/dev/null || echo "false")"
    if [[ "$cur_t_running" != "true" || ( -n "$canonical_tts_img" && "$cur_t_img" != "$canonical_tts_img" ) ]]; then
      log_info "Restoring TTS gateway to canonical image $canonical_tts_img..."
      local tts_deployer=""
      if [[ -f "$target_release/deploy-tts.sh" ]]; then
        tts_deployer="$target_release/deploy-tts.sh"
      elif [[ -f "$SCRIPT_DIR/deploy-tts.sh" ]]; then
        tts_deployer="$SCRIPT_DIR/deploy-tts.sh"
      elif [[ -f "${DEPLOY_DIR:-}/deploy-tts.sh" ]]; then
        tts_deployer="${DEPLOY_DIR:-}/deploy-tts.sh"
      fi

      if [[ -n "$tts_deployer" ]]; then
        RELEASE_ORCHESTRATED=1 DEFER_RELEASE_STATE=1 \
          RELEASE_DIR="$target_release" RELEASE_CONTEXT_DIR="$target_release" COMPOSE_ROOT="$target_release/compose" \
          SCRIPT_DIR="$target_release" DEPLOY_DIR="$target_release" \
          bash "$tts_deployer" "$canonical_tts_img" || {
            log_error "TTS canonical restore failed."
            return 1
          }
      else
        TTS_IMAGE_REF="$canonical_tts_img" compose_prod up -d --no-deps tts-gateway || {
          log_error "TTS canonical compose up failed."
          return 1
        }
      fi
    fi
  fi

  if comp_was_touched bark; then
    local cur_k_img cur_k_running
    cur_k_img="$(docker inspect --format '{{.Config.Image}}' "acb-bark" 2>/dev/null || echo "")"
    cur_k_running="$(docker inspect --format '{{.State.Running}}' "acb-bark" 2>/dev/null || echo "false")"
    if [[ "$cur_k_running" != "true" || ( -n "$canonical_bark_img" && "$cur_k_img" != "$canonical_bark_img" ) ]]; then
      log_info "Restoring Bark to canonical image $canonical_bark_img..."
      local bark_deployer=""
      if [[ -f "$target_release/deploy-bark.sh" ]]; then
        bark_deployer="$target_release/deploy-bark.sh"
      elif [[ -f "$SCRIPT_DIR/deploy-bark.sh" ]]; then
        bark_deployer="$SCRIPT_DIR/deploy-bark.sh"
      elif [[ -f "${DEPLOY_DIR:-}/deploy-bark.sh" ]]; then
        bark_deployer="${DEPLOY_DIR:-}/deploy-bark.sh"
      fi

      if [[ -n "$bark_deployer" ]]; then
        RELEASE_ORCHESTRATED=1 DEFER_RELEASE_STATE=1 \
          RELEASE_DIR="$target_release" RELEASE_CONTEXT_DIR="$target_release" COMPOSE_ROOT="$target_release/compose" \
          SCRIPT_DIR="$target_release" DEPLOY_DIR="$target_release" \
          bash "$bark_deployer" "$canonical_bark_img" || {
            log_error "Bark canonical restore failed."
            return 1
          }
      else
        BARK_IMAGE_REF="$canonical_bark_img" compose_prod up -d --no-deps bark || {
          log_error "Bark canonical compose up failed."
          return 1
        }
      fi
    fi
  fi

  if comp_was_touched failover_controller || comp_was_touched failover-controller; then
    local fo_deployer=""
    if [[ -f "$target_release/deploy-failover-controller.sh" ]]; then
      fo_deployer="$target_release/deploy-failover-controller.sh"
    elif [[ -f "$SCRIPT_DIR/deploy-failover-controller.sh" ]]; then
      fo_deployer="$SCRIPT_DIR/deploy-failover-controller.sh"
    fi
    if [[ -n "$fo_deployer" ]]; then
      log_info "Restoring failover controller..."
      EXPECTED_COMMIT="$git_sha" RELEASE_ORCHESTRATED=1 DEFER_RELEASE_STATE=1 FAILOVER_ROLLBACK_ONLY=1 \
        RELEASE_DIR="$target_release" RELEASE_CONTEXT_DIR="$target_release" COMPOSE_ROOT="$target_release/compose" \
        SCRIPT_DIR="$target_release" DEPLOY_DIR="$target_release" \
        bash "$fo_deployer" || {
          log_error "Failover controller restore failed."
          return 1
        }
    fi
  fi

  # Restore platform dynamic edge configurations if present in canonical release
  if [[ -d "$target_release/edge/dynamic" ]]; then
    local dynamic_dir="${TRAEFIK_DYNAMIC_DIR:-/opt/platform/edge/dynamic}"
    local dyn_drift=0
    for df in middlewares.yml portfolio.yml bark.yml messenger.yml; do
      if [[ -f "$target_release/edge/dynamic/$df" ]]; then
        if [[ ! -f "$dynamic_dir/$df" ]] || ! cmp -s "$target_release/edge/dynamic/$df" "$dynamic_dir/$df"; then
          dyn_drift=1
          break
        fi
      fi
    done
    if [[ "$dyn_drift" -eq 1 ]] || comp_was_touched platform; then
      log_info "Restoring platform dynamic edge configurations to canonical release..."
      mkdir -p "$dynamic_dir"
      for df in middlewares.yml portfolio.yml bark.yml messenger.yml; do
        if [[ -f "$target_release/edge/dynamic/$df" ]]; then
          cp -p "$target_release/edge/dynamic/$df" "$dynamic_dir/$df"
        fi
      done
    fi
  fi

  # Step 7: Verify full drift before deleting or archiving evidence
  log_info "Verifying full runtime drift against canonical state..."
  if [[ "${SKIP_MANIFEST_CHECK:-0}" -eq 1 && -n "${RUNTIME_DRIFT_CHECK_CMD:-}" ]]; then
    if ! eval "$RUNTIME_DRIFT_CHECK_CMD"; then
      log_error "Runtime drift check override command failed; preserving recovery evidence."
      return 1
    fi
  else
    local drift_verifier=""
    if [[ -f "$target_release/verify-runtime-drift.sh" ]]; then
      drift_verifier="$target_release/verify-runtime-drift.sh"
    elif [[ -f "$SCRIPT_DIR/verify-runtime-drift.sh" ]]; then
      drift_verifier="$SCRIPT_DIR/verify-runtime-drift.sh"
    fi

    if [[ -n "$drift_verifier" ]]; then
      if ! bash "$drift_verifier" --state "$state_file"; then
        log_error "Runtime drift verification failed against canonical state; preserving evidence."
        return 1
      fi
      log_info "Runtime drift verification PASSED: runtime matches canonical state."
    fi
  fi

  # Step 8: ONLY after drift verification passes: delete pending evidence
  if [[ -f "${PENDING_GATEWAY_RETIRE_FILE:-}" ]]; then
    rm -f "$PENDING_GATEWAY_RETIRE_FILE" 2>/dev/null || true
    log_info "Cleaned up pending gateway retire evidence after verified drift check."
  fi
  if [[ -f "${PENDING_FRONTEND_RETIRE_FILE:-}" ]]; then
    rm -f "$PENDING_FRONTEND_RETIRE_FILE" 2>/dev/null || true
    log_info "Cleaned up pending frontend retire evidence after verified drift check."
  fi

  # Rollout journal is preserved and marked ROLLED_BACK by dispatcher on candidate failure,
  # or archived with .reconciled suffix when explicitly requested.
  if [[ "${ARCHIVE_ROLLOUT_JOURNAL:-0}" == "1" && -f "$rollout_journal" ]]; then
    local archive_path
    archive_path="${rollout_journal}.reconciled.$(date +%s)"
    mv -f "$rollout_journal" "$archive_path"
    log_info "Archived reconciled rollout journal to: $archive_path"
  fi

  log_info "Canonical recovery completed successfully."
  return 0
}
