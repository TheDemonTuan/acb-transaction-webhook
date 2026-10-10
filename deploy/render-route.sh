#!/usr/bin/env bash
set -euo pipefail

[[ $# == 1 && ( "$1" == blue || "$1" == green ) ]] || {
  echo 'usage: render-route.sh <blue|green>' >&2
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
    acb-payos-webhook-router:
      rule: "Host(\`${viewer_host}\`) && Path(\`/api/integrations/payos/webhook\`) && Method(\`POST\`)"
      entryPoints: [web]
      priority: 1150
      middlewares: [tunnel-only, security-headers, payment-privacy, payos-webhook-rate-limit, payos-webhook-body-limit]
      service: acb-service
    acb-sepay-telegram-router:
      rule: "Host(\`${viewer_host}\`) && Path(\`/api/integrations/sepay/telegram\`) && Method(\`POST\`)"
      entryPoints: [web]
      priority: 1150
      middlewares: [tunnel-only, security-headers, payment-privacy, sepay-telegram-rate-limit, sepay-telegram-body-limit]
      observability:
        accessLogs: false
      service: acb-service
    acb-public-payments-router:
      rule: "Host(\`${viewer_host}\`) && (Path(\`/api/public/v1/payment-config\`) || Path(\`/api/public/v1/payments\`) || PathPrefix(\`/api/public/v1/payments/\`))"
      entryPoints: [web]
      priority: 1120
      middlewares: [tunnel-only, public-api-rate-limit, public-api-inflight-ip, public-api-inflight-global, security-headers, payment-privacy]
      observability:
        accessLogs: false
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
    acb-admin-payments-router:
      rule: "Host(\`${route_host}\`) && (Path(\`/api/v1/payments\`) || PathPrefix(\`/api/v1/payments/\`) || Path(\`/api/public/v1/payment-config\`) || Path(\`/api/public/v1/payments\`) || PathPrefix(\`/api/public/v1/payments/\`))"
      entryPoints: [web]
      priority: 210
      middlewares: [tunnel-only, security-headers, payment-privacy]
      observability:
        accessLogs: false
      service: acb-service
    acb-deploy-gateway:
      rule: "Host(\`gateway-deploy.acb.internal.invalid\`) && Path(\`/readyz\`)"
      entryPoints: [slot-probe]
      service: acb-service
  middlewares:
    # Bundle-local policies do not require changing shared edge middleware.
    payment-privacy:
      headers:
        customResponseHeaders:
          Referrer-Policy: "no-referrer"
          Cache-Control: "no-store"
    payos-webhook-body-limit:
      buffering:
        maxRequestBodyBytes: 65536
        memRequestBodyBytes: 65536
    payos-webhook-rate-limit:
      rateLimit:
        average: 60
        period: 1s
        burst: 120
        sourceCriterion:
          requestHost: true
    sepay-telegram-body-limit:
      buffering:
        maxRequestBodyBytes: 65536
        memRequestBodyBytes: 65536
    sepay-telegram-rate-limit:
      rateLimit:
        average: 60
        period: 1s
        burst: 120
        sourceCriterion:
          requestHost: true
  services:
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
