#!/usr/bin/env bash
# deploy/lib/traefik.sh
# Dynamic Traefik route rendering, YAML validation, atomic cutover, and real edge identity ACK.
set -euo pipefail

get_route_host() {
  local origin="${PUBLIC_ORIGIN:-${PUBLIC_HOST:-bank.tuannguyenviet.site}}"
  # Strip protocol
  local host="${origin#*://}"
  # Strip path / query
  host="${host%%/*}"
  host="${host%%\?*}"
  # Strip port if standard or split
  if [[ "$host" =~ ^([a-zA-Z0-9.-]+):[0-9]+$ ]]; then
    host="${BASH_REMATCH[1]}"
  fi
  # Validate hostname RFC format
  if [[ ! "$host" =~ ^[a-zA-Z0-9.-]+$ ]] || [[ "$host" == *"/"* ]] || [[ "$host" == *" "* ]]; then
    log_error "Invalid derived route hostname: '${host}'. Hostname must not contain path, scheme, or special characters."
    return 1
  fi
  printf '%s' "$host"
}

verify_traefik_prerequisites() {
  local dynamic_dir
  dynamic_dir="$(dirname "$ACB_CONFIG")"
  if ! mkdir -p "$dynamic_dir" 2>/dev/null; then
    log_error "Traefik dynamic configuration directory '$dynamic_dir' is not writable or cannot be created."
    return 1
  fi

  # In live container environment, verify Docker network edge-acb exists if docker is available
  if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
    if ! docker network inspect edge-acb >/dev/null 2>&1 && ! docker network inspect acb_edge-acb >/dev/null 2>&1; then
      log_warn "Edge network 'edge-acb' not found in Docker. In live production this network is required."
    fi

    # Verify edge-traefik mounts the configured dynamic directory if running
    if docker inspect edge-traefik >/dev/null 2>&1; then
      local mounted_source
      mounted_source="$(docker inspect edge-traefik --format '{{range .Mounts}}{{if eq .Destination "/etc/traefik/dynamic"}}{{.Source}}{{end}}{{end}}' 2>/dev/null || true)"
      if [[ -n "$mounted_source" ]]; then
        local real_dyn real_mount
        real_dyn="$(readlink -f "$dynamic_dir" 2>/dev/null || echo "$dynamic_dir")"
        real_mount="$(readlink -f "$mounted_source" 2>/dev/null || echo "$mounted_source")"
        if [[ "$real_dyn" != "$real_mount" ]]; then
          log_error "Traefik dynamic directory mismatch! Configured: '$dynamic_dir' (resolved: '$real_dyn'), but running edge-traefik mounts: '$mounted_source' (resolved: '$real_mount'). Aborting before route switch."
          return 1
        fi
      fi
    fi
  fi

  # Verify edge probe runtime preflight if probe script is present
  local probe_script="${EDGE_PROBE_SCRIPT:-}"
  if [[ -z "$probe_script" ]]; then
    if [[ -f "${SCRIPT_DIR:-deploy}/edge-probe.sh" ]]; then
      probe_script="${SCRIPT_DIR:-deploy}/edge-probe.sh"
    elif [[ -f "${SCRIPT_DIR:-deploy}/../platform/edge/probe.sh" ]]; then
      probe_script="${SCRIPT_DIR:-deploy}/../platform/edge/probe.sh"
    fi
  fi
  if [[ -n "$probe_script" && -f "$probe_script" ]]; then
    if [[ "${SKIP_PROBE_RUNTIME_CHECK:-0}" != "1" ]]; then
      if ! bash "$probe_script" --target production --check-runtime >/dev/null 2>&1; then
        log_warn "Preflight: edge probe runtime check failed or reported warning. Verify edge-cloudflared container is running."
      fi
    fi
  fi
  return 0
}

validate_traefik_yaml() {
  local yaml_file="$1"
  if [[ ! -s "$yaml_file" ]]; then
    log_error "Traefik YAML validation failed: file '$yaml_file' is empty or missing."
    return 1
  fi

  # 1. Use python3 / python yaml parser if available and working
  if command -v python3 >/dev/null 2>&1 && python3 --version >/dev/null 2>&1; then
    if python3 -c 'import sys, yaml; yaml.safe_load(sys.stdin)' < "$yaml_file" 2>/dev/null; then
      return 0
    else
      log_error "Traefik YAML syntax validation failed via python3: $yaml_file"
      return 1
    fi
  elif command -v python >/dev/null 2>&1 && python --version >/dev/null 2>&1; then
    if python -c 'import sys, yaml; yaml.safe_load(sys.stdin)' < "$yaml_file" 2>/dev/null; then
      return 0
    else
      log_error "Traefik YAML syntax validation failed via python: $yaml_file"
      return 1
    fi
  fi

  # 2. Use ruby if available and working
  if command -v ruby >/dev/null 2>&1 && ruby --version >/dev/null 2>&1; then
    if ruby -ryaml -e 'YAML.load_file(ARGV[0])' "$yaml_file" 2>/dev/null; then
      return 0
    else
      log_error "Traefik YAML syntax validation failed via ruby: $yaml_file"
      return 1
    fi
  fi

  # 3. Use perl if available and working
  if command -v perl >/dev/null 2>&1 && perl --version >/dev/null 2>&1; then
    if perl -e '
      my $depth = 0;
      while (<>) {
        if (/^(\s*)[^\s#]/) {
          my $indent = length($1);
          if ($indent % 2 != 0) { exit 1; }
        }
      }
      exit 0;
    ' "$yaml_file" 2>/dev/null; then
      :
    else
      log_error "Traefik YAML indentation validation failed via perl: $yaml_file"
      return 1
    fi
  fi

  # 4. Mandatory structural checks: must contain http routers and service
  if ! grep -q 'http:' "$yaml_file" || ! grep -q 'routers:' "$yaml_file" || ! grep -q 'services:' "$yaml_file" || ! grep -q 'acb-service:' "$yaml_file"; then
    log_error "Traefik YAML failed structural invariant checks: missing http, routers, or acb-service definitions."
    return 1
  fi
  return 0
}

render_traefik_config() {
  local target_slot="$1"
  local output_path="$2"
  local route_host
  route_host="$(get_route_host)"

  mkdir -p "$(dirname "$output_path")"
  cat <<EOF > "$output_path"
http:
  routers:
    acb-deny-internal:
      rule: "Host(\`${route_host}\`) && PathPrefix(\`/internal\`)"
      entryPoints:
        - web
      priority: 1000
      middlewares:
        - deny-internal
      service: acb-service

    acb-api-router:
      rule: "Host(\`${route_host}\`) && (PathPrefix(\`/api\`) || Path(\`/health\`) || Path(\`/healthz\`) || Path(\`/ready\`) || Path(\`/readyz\`))"
      entryPoints:
        - web
      priority: 200
      middlewares:
        - tunnel-only
        - security-headers
      service: acb-service

    acb-frontend-router:
      rule: "Host(\`${route_host}\`)"
      entryPoints:
        - web
      priority: 100
      middlewares:
        - tunnel-only
        - security-headers
      service: acb-frontend-service

  services:
    acb-frontend-service:
      loadBalancer:
        passHostHeader: true
        servers:
          - url: "http://acb-frontend:8080"
        healthCheck:
          path: "/readyz"
          interval: "5s"
          timeout: "2s"

    acb-service:
      loadBalancer:
        passHostHeader: true
        responseForwarding:
          flushInterval: "100ms"
        servers:
          - url: "http://acb-web-${target_slot}:8090"
        healthCheck:
          path: "/readyz"
          interval: "5s"
          timeout: "2s"
EOF
}

atomic_switch_route() {
  local target_slot="$1"
  if [[ "$target_slot" != "blue" && "$target_slot" != "green" ]]; then
    log_error "atomic_switch_route: invalid target slot '${target_slot}'. Must be 'blue' or 'green'."
    return 1
  fi

  log_info "Atomically updating Traefik route pointer to slot: ${target_slot}..."
  verify_traefik_prerequisites

  local tmp_config="${ACB_CONFIG}.tmp.$$"
  render_traefik_config "$target_slot" "$tmp_config"

  if ! validate_traefik_yaml "$tmp_config"; then
    log_error "Refusing to install invalid Traefik route configuration. Aborting route switch."
    rm -f "$tmp_config" 2>/dev/null || true
    return 1
  fi

  # Preserve byte-for-byte previous route for instant rollback
  if [[ -f "$ACB_CONFIG" ]]; then
    cp -p "$ACB_CONFIG" "${ACB_CONFIG}.prev" 2>/dev/null || true
  fi

  mv -f "$tmp_config" "$ACB_CONFIG"
  printf '%s' "$target_slot" > "$ACTIVE_SLOT_FILE"
  log_info "Dynamic route pointer successfully set to acb-web-${target_slot} (atomic rename committed)."
  return 0
}

ack_route_identity() {
  local target_slot="$1"
  local expected_commit=""
  local timeout="${ROUTE_ACK_TIMEOUT:-15}"

  if [[ $# -ge 3 ]]; then
    expected_commit="$2"
    timeout="$3"
  elif [[ $# -eq 2 ]]; then
    if [[ "$2" =~ ^[0-9]+$ ]] && [[ "$2" -le 3600 ]]; then
      timeout="$2"
    else
      expected_commit="$2"
    fi
  fi

  log_info "Acknowledging route identity for target slot [${target_slot}] (timeout: ${timeout}s)..."

  local route_host
  route_host="$(get_route_host)"

  local probe_script="${EDGE_PROBE_SCRIPT:-}"
  if [[ -z "$probe_script" ]]; then
    if [[ -f "${SCRIPT_DIR:-deploy}/edge-probe.sh" ]]; then
      probe_script="${SCRIPT_DIR:-deploy}/edge-probe.sh"
    elif [[ -f "${SCRIPT_DIR:-deploy}/../platform/edge/probe.sh" ]]; then
      probe_script="${SCRIPT_DIR:-deploy}/../platform/edge/probe.sh"
    fi
  fi

  local ack_url="${ROUTE_ACK_URL:-http://172.31.250.4:8080/readyz}"

  local start
  start="$(date +%s)"
  local attempts=0

  while true; do
    attempts=$(( attempts + 1 ))

    # Prefer edge probe helper if available and ROUTE_ACK_URL not explicitly overridden
    if [[ -n "$probe_script" && -f "$probe_script" && -z "${ROUTE_ACK_URL:-}" ]]; then
      local probe_cmd=(bash "$probe_script" --target production --host "${route_host}" --timeout 3 --expected-slot "${target_slot}")
      if [[ -n "${expected_commit}" && "${expected_commit}" != "unknown" ]]; then
        probe_cmd+=(--expected-commit "${expected_commit}")
      fi

      local probe_out
      local probe_rc=0
      probe_out="$("${probe_cmd[@]}" 2>&1)" || probe_rc=$?

      if [[ "$probe_rc" -eq 0 ]]; then
        log_info "Route identity ACK VERIFIED for slot [${target_slot}] via edge probe on attempt ${attempts}."
        return 0
      fi

      # Fallback check for rollback to legacy deployments lacking identity headers:
      # If expected_commit was empty (legacy rollback), check if endpoint is answering HTTP 200 via edge ingress
      if [[ -z "$expected_commit" || "$expected_commit" == "unknown" ]]; then
        if bash "$probe_script" --target production --host "${route_host}" --timeout 3 --expected-status 200 >/dev/null 2>&1; then
          log_warn "Route identity ACK: target slot [${target_slot}] responded HTTP 200 via production edge ingress, but lacks identity headers (legacy deployment). Proceeding with availability confirmation."
          return 0
        fi
      fi

      log_warn "Route ACK probe attempt ${attempts} failed (rc=${probe_rc}): $(printf '%s' "$probe_out" | tail -n 2 | tr '\n' ' ')"
    elif command -v curl >/dev/null 2>&1; then
      local resp_headers
      local http_code
      # Request route probe with host header and capture response headers
      local tmp_hdr
      tmp_hdr="$(mktemp "${TMPDIR:-/tmp}/ack-hdr.XXXXXX")"
      http_code="$(curl --max-time 3 --silent --show-error -o /dev/null -w "%{http_code}" -D "$tmp_hdr" -H "Host: ${route_host}" "$ack_url" 2>/dev/null || echo "000")"
      resp_headers="$(cat "$tmp_hdr" 2>/dev/null || echo "")"
      rm -f "$tmp_hdr" 2>/dev/null || true

      if [[ "$http_code" == "200" ]]; then
        local slot_header
        slot_header="$(printf '%s' "$resp_headers" | grep -i '^x-platform-slot:' | head -n1 | tr -d '\r\n' | sed -e 's/^[^:]*:[[:space:]]*//' -e 's/[[:space:]]*$//' || echo "")"
        local commit_header
        commit_header="$(printf '%s' "$resp_headers" | grep -i '^x-release-commit:' | head -n1 | tr -d '\r\n' | sed -e 's/^[^:]*:[[:space:]]*//' -e 's/[[:space:]]*$//' || echo "")"

        if [[ "$slot_header" == "$target_slot" ]]; then
          if [[ -z "$expected_commit" || "$expected_commit" == "unknown" || "$commit_header" == "$expected_commit" ]]; then
            log_info "Route identity ACK VERIFIED for slot [${target_slot}] (HTTP 200, X-Platform-Slot: ${slot_header}, X-Release-Commit: ${commit_header:-none}) on attempt ${attempts}."
            return 0
          else
            log_warn "Route ACK probe returned slot [${slot_header}] but commit mismatch: expected '${expected_commit}', got '${commit_header}'"
          fi
        elif [[ -z "$slot_header" && ( -z "$expected_commit" || "$expected_commit" == "unknown" ) ]]; then
          log_warn "Route identity ACK: target slot [${target_slot}] responded HTTP 200, but lacks X-Platform-Slot header (legacy deployment). Proceeding with availability confirmation."
          return 0
        else
          log_warn "Route ACK probe reached slot [${slot_header:-missing}] instead of target [${target_slot}]"
        fi
      else
        log_warn "Route ACK probe attempt ${attempts} returned HTTP ${http_code} from ${ack_url}"
      fi
    elif [[ -n "${MOCK_ACK_SUCCESS:-}" && "${MOCK_ACK_SUCCESS}" == "1" ]]; then
      log_info "Mock route identity ACK verified for slot [${target_slot}]."
      return 0
    else
      log_warn "Neither edge probe script nor curl available to verify route ACK."
      return 1
    fi

    local now
    now="$(date +%s)"
    if (( now - start >= timeout )); then
      log_error "Route identity ACK TIMEOUT after ${timeout}s (${attempts} attempts)! Target slot [${target_slot}] was not positively acknowledged through edge route."
      return 1
    fi
    sleep 1
  done
}

rollback_route() {
  local previous_slot="$1"
  local expected_commit="${2:-}"
  local ack_timeout="${3:-15}"
  log_warn "Restoring previous Traefik route configuration for [${previous_slot}]..."
  if [[ -f "${ACB_CONFIG}.prev" ]]; then
    cp -f "${ACB_CONFIG}.prev" "$ACB_CONFIG" 2>/dev/null || true
    printf '%s' "$previous_slot" > "$ACTIVE_SLOT_FILE"
  else
    atomic_switch_route "$previous_slot"
  fi
  log_info "Traefik route pointer reverted to [${previous_slot}]."
  ack_route_identity "$previous_slot" "$expected_commit" "$ack_timeout"
}

central_switch_route() {
  local target_slot="$1"
  local skip_lock="${2:-0}"
  local expected_commit="${3:-}"
  if [[ "$skip_lock" != "1" && "${DEPLOY_LOCK_HELD:-0}" != "1" ]]; then
    acquire_deploy_lock
  fi
  atomic_switch_route "$target_slot"
  ack_route_identity "$target_slot" "$expected_commit" 15
}
