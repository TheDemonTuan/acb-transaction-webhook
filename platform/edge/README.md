# Platform Edge Gateway & Hardened Discovery

This directory contains the unified edge platform configuration based on Traefik 3.x, Cloudflare Tunnel connector, hardened Docker socket proxy, and trusted namespace probes.

## Architecture Overview

```text
Internet / Client Requests
          ???
          ???
   Cloudflare Edge
          ??? (Encrypted Tunnel)
          ???
  edge-cloudflared (172.31.250.2)
          ???
   edge-cf-ingress network (172.31.250.0/28)
          ???
          ??? (Port 8080)
    edge-traefik (172.31.250.4)
          ???
   ?????????????????????????????????????????????????????????????????????????????????????????????????????????????????????????????????????????????????????????
   ???                                                 ???
   ??? (Docker Provider Discovery)                     ??? (Dynamic Traffic Routing)
edge-docker-socket-proxy                      acb-core / edge-acb services
   ??? (HAProxy strict allowlist)               - acb-web-blue / acb-web-green:8090
   ???                                          - acb-bark:8080
/var/run/docker.sock
```

---

## 1. Hardened Docker Socket Proxy

Traefik uses the Docker Provider to discover container services, health statuses, and metadata. To prevent Traefik or any edge component from obtaining root-equivalent host control:

- Traefik connects exclusively to `edge-docker-socket-proxy:2375` on the internal `edge-management` network.
- HAProxy enforces a strict, exact method and path allowlist (`haproxy.cfg.template`):
  - **Mutations Denied**: All non-GET methods (`POST`, `PUT`, `DELETE`, `PATCH`) are rejected with `403 Forbidden`.
  - **Sensitive Operations Denied**: Requests matching `/containers/[^/]+/(logs|archive|export|attach|exec)` or `/secrets`, `/volumes`, `/networks`, `/exec`, etc., are explicitly denied.
  - **No Broad Container Paths**: Unqualified `/containers`, `/containers/`, or arbitrary subpaths are rejected.
  - **Exact Allowlist**:
    1. `GET /_ping` / `GET /v1.44/_ping` (Ping check)
    2. `GET /version` / `GET /v1.44/version` (Version check)
    3. `GET /events` / `GET /v1.44/events` (Streaming container lifecycle events, routed to `docker-events` with `timeout server 0`)
    4. `GET /containers/json` / `GET /v1.44/containers/json` (Container listing, supports `?all=1` and query filters)
    5. `GET /containers/[a-zA-Z0-9_.-]+/json` (Container inspect by ID or name, supports query params)
  - **API Version Normalization**: Rewrites older `v1.xx` requests to `v1.44` while matching with regex `^(/v[\d\.]+)?/` to handle versioned and unversioned paths.

---

## 2. Ingress & Access Log Hardening

### Ingress Restriction
- Public HTTP traffic is accepted only via Cloudflare Tunnel.
- The `web` entrypoint sets `trustedIPs: ["172.31.250.2/32"]`.
- The `tunnel-only` middleware enforces `ipAllowList.sourceRange: ["172.31.250.2/32"]`, rejecting any other source IP with `403 Forbidden`. Legacy edge proxies (such as `172.31.250.3/32`) and broad subnets are strictly disallowed.

### Public Viewer Anti-Abuse & Inflight Protection
The anonymous transaction viewer (`transactions.tuannguyenviet.site`) is hardened directly at Traefik with tiered rate and inflight concurrency limits:
- **Public REST (`/api/public/v1/*`)**:
  - Sustained Rate Limit: `10 req/s`, burst `20` (`CF-Connecting-IP`).
  - Inflight Concurrency per IP: max `32` concurrent requests (`CF-Connecting-IP`).
  - Global Inflight Cap: max `128` concurrent requests for the hostname (`requestHost: true`).
- **Public SSE (`/api/public/v1/events*`)**:
  - Reconnection Limiter: `2 conn/s`, burst `5` (`CF-Connecting-IP`).
  - Active SSE per IP: max `12` concurrent connections (`CF-Connecting-IP`).
  - Global Active SSE Cap: max `128` concurrent connections for the hostname (`requestHost: true`).
- **Router Priorities**:
  - `1200`: Public SSE Router (`acb-public-sse-router`)
  - `1100`: Public REST API Router (`acb-public-api-router`)
  - `1000`: Public Deny Router (`acb-public-deny-private`, blocking `/api`, `/internal`, `/admin`, `/healthz`, etc.)
  - `100`: Public Frontend Router (`acb-public-frontend-router`)

### Access Log Privacy
Configured strictly according to Traefik 3.7.13 specifications:
- `headers.defaultMode: drop`: All request and response headers are dropped by default, preventing leakage of `Authorization`, `Cookie`, `Cf-Access-Jwt-Assertion`, or internal tokens.
- `queryParameters.defaultMode: drop`: Strips query strings from logged URLs to protect sensitive query parameters.
- `names`: Explicitly drops `ClientUsername`, `RequestPath`, and `RequestLine`.

### Container Resource Bounds & Capabilities
- `socket-proxy`: `pids_limit: 100`, `mem_limit: 128m`, `cpus: 0.25`, `json-file` logging (`max-size: 10m`, `max-file: 3`).
- `traefik`: `pids_limit: 200`, `mem_limit: 256m`, `cpus: 0.5`, `json-file` logging (`max-size: 10m`, `max-file: 3`).
- `NET_BIND_SERVICE` dropped: Traefik listens on ports `8080` (web) and `18080` (slot-probe), both `> 1024`, so `cap_drop: [ALL]` is maintained with zero privileged capabilities.

---

## 3. Route Protection for Private Endpoints (`/internal/deployz`)

Endpoints under `/internal/*` (such as `/internal/deployz`) contain deployment release metadata and must never be exposed publicly. The shared Messenger route applies the same deny-first policy to `/internal`, `/health`, `/healthz`, `/ready`, and `/readyz` on `messenger.tuannguyenviet.site`, while forwarding through the external `edge-portfolio` network to the stable `messenger-core:3000` alias.

Defense-in-depth is applied across both ACB and Bark dynamic configurations:
1. **Higher-Priority Deny Router (`priority: 1000`)**:
   - Matches `Host(<domain>) && PathPrefix(/internal)`
   - Attached to `deny-internal` middleware (`ipAllowList` restricted to `127.0.0.1/32` with `403 Forbidden`).
2. **Public Router Exclusion (`priority: 100`)**:
   - Matches `Host(<domain>) && !PathPrefix(/internal)`
   - Ensures requests to `/internal/*` do not match the public router even if the deny router were altered.

---

## 4. Trusted Helper Probes

### Architecture
- Candidate slots (Blue/Green) and internal diagnostics must be tested before traffic cutover.
- Rather than opening host ports or weakening firewall allowlists, probes leverage Docker network namespace sharing (`--network container:<name>`):
  - **Slot-Probe**: Connects to `127.0.0.1:18080` inside `edge-traefik` network namespace, routing to candidate slots via `Host: acb-<slot>.internal.invalid`.
  - **Production Acknowledgement**: Connects to `172.31.250.4:8080` inside `edge-cloudflared` network namespace (`172.31.250.2`), satisfying `tunnel-only` allowlist natively without host port exposure.

### Helper Script: `probe.sh`
```bash
# Probe inactive slot ready status
platform/edge/probe.sh --target slot-probe --slot green --path /readyz

# Probe candidate slot release identity and digest
platform/edge/probe.sh --target slot-probe --slot green --path /internal/deployz --expected-digest 7c59899

# Acknowledge production routing
platform/edge/probe.sh --target production --service acb --path /readyz
```

---

## 5. Cloudflare Configuration Rules for Voice Audio Streaming

Cloudflare Tunnel defaults to buffering HTTP response bodies. While Server-Sent Events (`text/event-stream`) bypass buffering automatically, `audio/mpeg` streaming responses are buffered by Cloudflare's edge proxy unless explicitly disabled via Configuration Rules.

### Path-Scoped Configuration Rule
To enable immediate first-byte streaming while preserving Cloudflare WAF inspection and Bot Management across the rest of the zone:
- **Rule Type**: Configuration Rules -> Response Body Buffering
- **Setting**: `Response Body Buffering = None`
- **Scope Expression**:
  ```text
  (http.request.uri.path wildcard "/api/public/v1/voice/*/stream*") or
  (http.request.uri.path wildcard "/api/v1/voice/*/stream*")
  ```
- **Constraint**: Do **not** disable response body buffering globally across the zone, as global disabling degrades edge security inspection and DDoS mitigation capabilities for non-streaming endpoints.

### Pinned Probe Containers
Defined in `compose.yml` under the `probe` profile using pinned `curlimages/curl:8.12.1`:
- `edge-probe-traefik` (`network_mode: "container:edge-traefik"`)
- `edge-probe-cloudflared` (`network_mode: "container:edge-cloudflared"`)

---

## 5. Verification & Tests

Deterministic integration tests in `edge_test.go` validate:
1. HAProxy ACL simulation for all allowed and denied operations.
2. Traefik static and dynamic YAML configurations.
3. Access log privacy and Cloudflare IP restrictions.
4. Defense-in-depth route protection on ACB, Bark, and Messenger.
5. Resource and capability limits in Compose.
6. Helper probe CLI parameter handling and dry-run execution.

Run the test suite with:
```bash
go test -v ./platform/edge/...
```
