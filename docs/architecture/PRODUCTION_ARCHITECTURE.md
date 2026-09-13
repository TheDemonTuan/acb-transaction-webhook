# ACB Transaction Webhook — Production Architecture

**Document Version:** 1.0 (Canonical)
**Date:** 2026-09-14
**Authoritative Reference:** [`docs/superpowers/specs/2026-09-14-acb-final-production-invariants.md`](../superpowers/specs/2026-09-14-acb-final-production-invariants.md)
**Implementation Plan:** [`docs/superpowers/plans/2026-09-14-acb-production-convergence-execution.md`](../superpowers/plans/2026-09-14-acb-production-convergence-execution.md)

---

## 1. Executive & Operator Overview

This document describes the production runtime architecture for `TheDemonTuan/acb-transaction-webhook` deployed on a single hardened Linux VPS running Docker Compose.

The architecture strictly enforces:
1. **Near-zero downtime HTTP deployments** through warm Blue/Green gateway slots behind a shared Traefik 3.x edge ingress.
2. **Zero interruption to bank polling** during dashboard/gateway updates by isolating the Asia Commercial Bank (ACB) polling loop, session state, and webhook/notification dispatching into a dedicated **singleton worker**.
3. **Fail-closed security**: Explicit runtime roles, absence of monolith mode in production, signed immutable container digests, and encrypted off-host database backups using `age`.

---

## 2. Core Components

The production topology consists of the following dedicated containers running on the host:

| Component | Container Name(s) | Role & Responsibilities | Lifecycle & Deployment |
|---|---|---|---|
| **Traefik Edge Ingress** | `edge-traefik` | Shared host-level reverse proxy located at `/opt/edge`. Ingress route for Cloudflare Tunnel. Terminates public requests and routes to active gateway slot based on dynamic configuration. | Independent platform lifecycle. Not managed by app deploy. |
| **Gateway Slots (W1)** | `acb-web-blue`, `acb-web-green` | Stateless HTTP servers serving SPA dashboard, REST API, SSE event streams. Validates Cloudflare Access JWTs and CSRF tokens. Forwards upstream operations to worker via Worker RPC. **Never polls ACB directly, never runs DB migrations at startup, never acquires singleton lock.** | Deployed via warm Blue/Green cutover. Inactive slot starts, passes health checks, is verified via Traefik route ACK, and old slot is drained. |
| **Singleton Worker (W2)** | `acb-worker` | Stateful daemon holding singleton process lock (`/data/gateway.lock`). Exclusively owns the ACB client session, upstream scheduler (realtime polling, keepalive, catch-up, history backfill), webhook delivery engine, notification dispatcher, and SQLite maintenance. | Stateful singleton handoff. Retains container ID across gateway releases. Replaced only during explicit worker upgrades. |
| **Auth Browser** | `acb-auth-browser` | Sandboxed Chromium container with seccomp profile and restricted capabilities for interactive ACB web login and session capture. | Independent lifecycle. Deploy aborted if an authentication attempt is actively in progress. |
| **TTS Gateway** | `acb-tts-gateway` | Python-based text-to-speech service running Microsoft Edge TTS with gTTS fallback for transaction audio synthesis. | Independent auxiliary service. Internal RPC only. |
| **Bark Notification** | `acb-bark` | Self-hosted Bark push notification server for iOS APNs delivery. | Independent auxiliary service. Data volume preserved across releases. |
| **Database Tool** | `acb-dbtool` (ephemeral) | Standalone Go CLI used for explicit schema migrations, integrity checks, and online SQLite backup triggers. | Runs as a one-shot container during schema release transactions. |
| **Failover Controller** | `vps-failover-controller` | Systemd service running on the host that monitors Docker events and reconciles container states according to policy. | Host daemon. Interacts via `/var/run/docker.sock` from host only. |

---

## 3. Network Segmentation (The Three Docker Networks)

To minimize blast radius, production containers communicate across three strictly segregated Docker bridge networks:

```text
[Internet]
    │
    ▼
[Cloudflare Tunnel / Access]
    │
    ▼
[edge-traefik] (172.31.250.4) ─── Network: edge-acb (172.31.250.0/28)
                                       │
                ┌──────────────────────┴──────────────────────┐
                ▼                                             ▼
        [acb-web-blue] :8090                          [acb-web-green] :8090
                │                                             │
                └──────────────────────┬──────────────────────┘
                                       │
                      Network: acb-core (172.31.251.0/24 - Internal Only)
                                       │
         ┌─────────────────────────────┼─────────────────────────────┐
         ▼                             ▼                             ▼
   [acb-worker]                [acb-auth-browser]            [acb-tts-gateway]
   (/data/gateway.lock)          (seccomp sandboxed)          (audio synthesis)
         │                                                           │
         │                             ┌─────────────────────────────┘
         │                             │
         ▼                             ▼
   Network: acb-egress (172.31.252.0/24 - Outbound Internet Access Only)
         │                             │
         ▼                             ▼
  Asia Commercial Bank          Microsoft Edge TTS /
  (ACB ONE Web API)             gTTS fallback endpoints
```

1. **`edge-acb` (Ingress Network):**
   - Connects `edge-traefik` with gateway slots (`acb-web-blue`, `acb-web-green`) and `acb-bark`.
   - Worker, auth-browser, and internal services have **no interface** on `edge-acb`.
   - No container in this network publishes ports directly to host interfaces `0.0.0.0`.

2. **`acb-core` (Internal Service Network):**
   - Strictly internal (`internal: true`), non-routable to the external internet.
   - Connects gateway slots, `acb-worker`, `acb-auth-browser`, `acb-tts-gateway`, and `acb-bark`.
   - Used for Worker RPC (`internal/workerrpc`), TTS synthesis requests, and auth-browser screen/automation commands.

3. **`acb-egress` (Outbound Egress Network):**
   - Provides external outbound internet access for components that need external connectivity:
     - `acb-worker`: Communicates exclusively with official ACB endpoints (`online.acb.com.vn`).
     - `acb-tts-gateway`: Fetches synthesis audio from Microsoft Edge TTS servers.
     - `acb-bark`: Connects to Apple APNs servers.
   - Gateway slots and auth-browser have restricted or zero direct internet routing outside monitored paths.

---

## 4. Runtime Roles and Failure Boundaries

Production enforces strict runtime roles via the `RUNTIME_ROLE` environment variable:

```text
                  ┌───────────────────────────────┐
                  │ RUNTIME_ROLE validation gate  │
                  └──────────────┬────────────────┘
                                 │
         ┌───────────────────────┼───────────────────────┐
         ▼                       ▼                       ▼
RUNTIME_ROLE=gateway    RUNTIME_ROLE=worker     RUNTIME_ROLE=monolith-dev
- Requires WORKER_RPC_URL- Requires gateway.lock - ONLY allowed when
  and worker token        ownership               APP_ENV != production
- Fails closed if ACB   - Sole owner of ACB     - Instantly aborts
  client initialized      scheduler & dispatcher  if APP_ENV=production
- No DB auto-migration  - Runs durable recovery - Never deployed to VPS
- Stateless HTTP only   - Holds session state
```

### Fail-Closed Role Constraints
- If `APP_ENV=production`, `cmd/gateway` checks that `RUNTIME_ROLE=gateway`. If unset or set to `monolith-dev`, the process aborts immediately before initializing HTTP handlers or database connections.
- If `APP_ENV=production`, `cmd/worker` checks that `RUNTIME_ROLE=worker`. It acquires `/data/gateway.lock` using `flock` and aborts if the lock is held by another process.
- Monolith fallback in production is strictly forbidden to eliminate split-brain polling risks.

---

## 5. ACB Upstream Priority Scheduling

ACB upstream calls are managed by a single-owner cooperative priority scheduler inside `acb-worker`:

```text
Priority 1: INTERACTIVE_VERIFY (operator login verification)
     │
Priority 2: REALTIME_POLL      (15s recurring transaction check)
     │
Priority 3: MANUAL_SYNC        (dashboard "Sync Now" button)
     │
Priority 4: KEEPALIVE          (session refresh / ping)
     │
Priority 5: CATCH_UP           (post-downtime gap fill)
     │
Priority 6: FILTER_HISTORY     (multi-day user historical backfill)
```

### Quantum Yielding Invariant
- Every low-priority task (`CATCH_UP`, `FILTER_HISTORY`) is divided into bounded **quanta**.
- A quantum performs at most **one ACB page request** plus local database persistence, then yields execution back to the scheduler.
- A waiting `REALTIME_POLL` will execute immediately after the current single-page quantum completes, preventing 31-day history requests from starving realtime transaction detection.

---

## 6. Edge Ingress, Route Ownership, and Acknowledgement

The ACB stack does not manage Traefik container lifecycles or cloudflared tunnels. It interacts with the shared platform edge exclusively via the dynamic file provider:

1. **Route File Ownership:**
   - Platform watches `/opt/edge/dynamic`.
   - The application manages `/opt/edge/dynamic/acb.yml`.

2. **Atomic Cutover:**
   - The deploy script renders candidate route configuration into a temporary file `/opt/edge/dynamic/acb.yml.tmp`.
   - Validates YAML syntax.
   - Atomically replaces `/opt/edge/dynamic/acb.yml` via POSIX `mv`.

3. **Positive Acknowledgement (ACK Contract):**
   - Promotion is **not** complete upon file write.
   - Deploy script sends HTTP probe requests through Traefik and asserts response headers:
     - `X-Platform-Slot`: matches target candidate slot (`blue` or `green`).
     - `X-Release-Commit`: matches intended release commit SHA.
   - If route ACK fails within the deadline, the previous configuration is atomically restored and the rollback is verified before reporting failure.

---

## 7. Storage, Persistence, and Concurrency Model

- **Database:** SQLite running in Write-Ahead Logging (`WAL`) mode located at `/data/gateway.db`.
- **Busy Timeout:** Configured to `5000ms` with WAL synchronous mode set to `NORMAL`.
- **Connection Isolation:** Max open connections bounded to prevent SQLite thread contention.
- **Generation Fencing:** Every connection and session possesses a monotonic `generation` integer. Transactions, events, deliveries, and poll runs record the active generation in the same database transaction. Queries check current generation to fence against stale writes from un-drained tasks.
- **Migrations:** Applied exclusively via `cmd/dbtool` during explicit schema promotion steps. Runtime processes (`gateway`, `worker`) open the database with `RunMigrations: false` and fail closed if schema version does not match expected compatibility level.

---

## 8. Secrets Management and Disaster Recovery

### Least-Privilege Secret Distribution
| Component | Required Secrets | Prohibited Secrets |
|---|---|---|
| Gateway | `app_master_key`, `worker_internal_token`, `tts_internal_token` | Bark credentials, ACB credentials |
| Worker | `app_master_key`, `worker_internal_token`, `bark_basic_auth_user`, `bark_basic_auth_password` | TTS internal token |
| Auth-Browser | None (ephemeral session capture) | All production secrets |
| TTS Gateway | `tts_internal_token` | Bank credentials, master key |
| Bark | `bark_basic_auth_user`, `bark_basic_auth_password` | Master key, internal tokens |

### Encrypted Backup Architecture
- **Routine DB Backups:** Created via `deploy/backup.sh` before schema migrations.
- **Encryption:** Uses `age` with a public recipient key (`AGE_RECIPIENT_KEY`) stored on the host. Plaintext database copies are kept only in temporary restricted memory/tmpfs and securely unlinked.
- **Private Key Storage:** The `age` recovery private key is **never stored on the VPS**.
- **Manifest:** Each backup generates a cryptographic JSON manifest containing SHA-256 hash, byte size, schema version, release ID, and timestamp.
- **Off-host Hook:** Pushes only `.db.age` ciphertext and `.json` manifest to secure off-host storage.
