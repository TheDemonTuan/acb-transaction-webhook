#!/usr/bin/env bash
set -euo pipefail

[[ $# == 2 && ( "$1" == blue || "$1" == green ) && ( "$2" == blue || "$2" == green ) ]] || {
  echo 'usage: render-route.sh <blue|green> <blue|green>' >&2
  exit 2
}

# Only validated DNS hosts enter Traefik rules; never interpolate an arbitrary origin.
origin_host() {
  local origin="$1" host label
  [[ "$origin" =~ ^https://([a-z0-9.-]+)/?$ ]] || { echo 'invalid public origin' >&2; return 1; }
  host="${BASH_REMATCH[1]}"
  [[ ${#host} -le 253 && "$host" == *.* ]] || { echo 'invalid public hostname' >&2; return 1; }
  local -a labels
  IFS=. read -r -a labels <<< "$host"
  for label in "${labels[@]}"; do
    [[ ${#label} -le 63 && "$label" =~ ^[a-z0-9]([a-z0-9-]*[a-z0-9])?$ ]] || {
      echo 'invalid public hostname' >&2; return 1;
    }
  done
  [[ "$host" != *. ]] || { echo 'invalid public hostname' >&2; return 1; }
  printf '%s' "$host"
}
route_host="$(origin_host "${PUBLIC_ORIGIN:-https://bank.tuannguyenviet.site}")"
viewer_host="$(origin_host "${PUBLIC_VIEWER_ORIGIN:-https://transactions.tuannguyenviet.site}")"

cat <<EOF
http:
  routers:
    acb-deny-internal:
      rule: "(Host(\`${route_host}\`) || Host(\`${viewer_host}\`)) && PathPrefix(\`/internal\`)"
      entryPoints: [web]
      priority: 1000
      middlewares: [deny-internal]
      service: acb-service
    acb-public-deny-private:
      rule: "Host(\`${viewer_host}\`) && (PathPrefix(\`/api\`) || PathPrefix(\`/internal\`) || PathPrefix(\`/admin\`) || Path(\`/health\`) || Path(\`/healthz\`) || Path(\`/ready\`) || Path(\`/readyz\`))"
      entryPoints: [web]
      priority: 1000
      middlewares: [deny-internal]
      service: acb-service
    acb-public-sse-router:
      rule: "Host(\`${viewer_host}\`) && (Path(\`/api/public/v1/events\`) || Path(\`/api/public/v1/events/stream\`))"
      entryPoints: [web]
      priority: 1200
      middlewares: [tunnel-only, public-sse-rate-limit, public-sse-inflight-ip, public-sse-inflight-global, security-headers]
      service: acb-service
    acb-public-api-router:
      rule: "Host(\`${viewer_host}\`) && (Path(\`/api/public/v1\`) || PathPrefix(\`/api/public/v1/\`))"
      entryPoints: [web]
      priority: 1100
      middlewares: [tunnel-only, public-api-rate-limit, public-api-inflight-ip, public-api-inflight-global, security-headers]
      service: acb-service
    acb-api-router:
      rule: "Host(\`${route_host}\`) && (PathPrefix(\`/api\`) || Path(\`/health\`) || Path(\`/healthz\`) || Path(\`/ready\`) || Path(\`/readyz\`))"
      entryPoints: [web]
      priority: 200
      middlewares: [tunnel-only, security-headers]
      service: acb-service
    acb-public-frontend-router:
      rule: "Host(\`${viewer_host}\`)"
      entryPoints: [web]
      priority: 100
      middlewares: [tunnel-only, security-headers]
      service: acb-frontend-service
    acb-frontend-router:
      rule: "Host(\`${route_host}\`)"
      entryPoints: [web]
      priority: 100
      middlewares: [tunnel-only, security-headers]
      service: acb-frontend-service
    acb-deploy-gateway:
      rule: "Host(\`gateway-deploy.acb.internal.invalid\`) && Path(\`/readyz\`)"
      entryPoints: [slot-probe]
      service: acb-service
    acb-deploy-frontend:
      rule: "Host(\`frontend-deploy.acb.internal.invalid\`)"
      entryPoints: [slot-probe]
      service: acb-frontend-service
  services:
    acb-frontend-service:
      loadBalancer:
        passHostHeader: true
        servers:
          - url: "http://acb-frontend-${2}:8080"
        healthCheck:
          path: /readyz
          interval: 5s
          timeout: 2s
    acb-service:
      loadBalancer:
        passHostHeader: true
        responseForwarding:
          flushInterval: 100ms
        servers:
          - url: "http://acb-web-${1}:8090"
        healthCheck:
          path: /readyz
          interval: 5s
          timeout: 2s
EOF
