#!/usr/bin/env bash
# Atomically switch the active Gateway slot in Traefik File Provider
set -euo pipefail

TARGET_SLOT="${1:-blue}"
TRAEFIK_DYNAMIC_DIR="${TRAEFIK_DYNAMIC_DIR:-/opt/edge/dynamic}"
ACB_CONFIG="${TRAEFIK_DYNAMIC_DIR}/acb.yml"
ACTIVE_SLOT_FILE="${ACTIVE_SLOT_FILE:-.active-slot}"

if [[ "$TARGET_SLOT" != "blue" && "$TARGET_SLOT" != "green" ]]; then
  printf "ERROR: Invalid target slot '%s'. Must be 'blue' or 'green'.\n" "$TARGET_SLOT" >&2
  exit 1
fi

printf "Switching Traefik active route pointer to slot: %s (acb-web-%s)...\n" "$TARGET_SLOT" "$TARGET_SLOT"

# Verify candidate readiness before switching route
bash deploy/smoke-slot.sh "$TARGET_SLOT"

mkdir -p "$TRAEFIK_DYNAMIC_DIR"
TMP_CONFIG="${ACB_CONFIG}.tmp.$$"

cat <<EOF > "$TMP_CONFIG"
http:
  routers:
    acb-router:
      rule: "Host(\`bank.tuannguyenviet.site\`)"
      entryPoints:
        - web
      middlewares:
        - tunnel-only
        - security-headers
      service: acb-service

  services:
    acb-service:
      loadBalancer:
        passHostHeader: true
        responseForwarding:
          flushInterval: "100ms"
        servers:
          - url: "http://acb-web-${TARGET_SLOT}:8090"
        healthCheck:
          path: "/readyz"
          interval: "5s"
          timeout: "2s"
EOF

# Atomic replace
mv -f "$TMP_CONFIG" "$ACB_CONFIG"
printf "%s" "$TARGET_SLOT" > "$ACTIVE_SLOT_FILE"

printf "Traefik route pointer successfully updated to %s (acb-web-%s).\n" "$TARGET_SLOT" "$TARGET_SLOT"
