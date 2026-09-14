# ACB Observability & Operational Runbook

This document defines the observability contracts, health check semantics, bounded telemetry metrics, cardinality/privacy controls, and operational alerting thresholds for `TheDemonTuan/acb-transaction-webhook`.

---

## 1. Three-Level Health Semantics

In accordance with architectural invariant **Phase 11 (Task 41)** and **Invariant 21**:

| Endpoint | Level | Semantics & Constraints | Failure Behavior |
| :--- | :--- | :--- | :--- |
| `/healthz` | **Process Liveness** | Proves process runtime is responding to HTTP. Database-independent and upstream-independent. ACB login expiry, session maintenance, or rate-limiting **NEVER** fail liveness. | Returns `200 OK` unless the process is deadlocked or shutting down. |
| `/readyz` | **Role Readiness** | Proves the process can serve its runtime role (`ROLE=gateway`, `ROLE=worker`, or `ROLE=auth-browser`). For gateway: storage healthy and schema compatible. For worker: singleton lock held, schema compatible, scheduler initialized, not draining. | Returns `503 Service Unavailable` with JSON `{ "status": "not_ready", "error": ... }`. Draining worker returns 503. |
| `/internal/deployz` | **Promotion Gate** | Dependency-aware pre-promotion verification. Requires internal token auth (`X-Worker-Internal-Token`). Checks storage, schema version, worker reachability, auth-browser, TTS, Bark, and mutation gate. | Returns `200 OK` (status: `ready` or `degraded`), or `503 Service Unavailable` (status: `not_ready`). TTS failure marks status `degraded` without blocking promotions. |

---

## 2. Realtime Fast-Path Health

Gateway telemetry exposes bounded process-local fast-path state and counters:

- `streamEnabled`, `streamState`, `streamReason`, and `streamStateSince` describe the Worker-to-Gateway SSE connection. States are `disabled`, `connecting`, `connected`, `degraded`, and `stopped`.
- `streamReconnectTotal` counts connection attempts after the first. `streamDisconnectTotal` counts established streams that ended outside an intentional shutdown. Counters reset when the Gateway process restarts.
- `fallbackRecoveryTotal` counts journal events published by coordinator recovery. `gapRepairTotal` counts gap reconciliations that recovered events. An isolated reconnect or deployment cutover can increase these counters without indicating data loss.
- `p95CommitToGatewayMs` measures an observed successful commit to Gateway publish for live credit events. `p95CommitToBrowserSseMs` ends at successful server flush to the browser connection; it does not measure browser rendering or acknowledgement.

`/readyz` remains based on local serving capability because SQLite recovery keeps the Gateway usable. `/internal/deployz` reports `degraded` when realtime is enabled but disconnected, and `/api/v1/ops/alerts` exposes:

- `ALERT_REALTIME_STREAM_DEGRADED`: warning while connecting/degraded and critical after 120 seconds or for a terminal stop such as unauthorized credentials.
- `ALERT_REALTIME_FALLBACK_RECOVERY`: warning when at least two separate reconciliations recover events within 60 seconds. A single restart/cutover recovery does not trigger it.

For `unauthorized`, verify that Worker and Gateway use the same `WORKER_INTERNAL_TOKEN`, then restart the Gateway. For `idle_timeout` or `transport_error`, verify the Worker realtime listener, Docker network, and both Blue/Green Gateway telemetry independently. Never place tokens or transaction payloads in logs or telemetry.

## 3. Route Identity ACK & Split Role Checks

### 3.1 Response Headers (Route ACK)
Every HTTP response from edge services provides identity acknowledgement headers:
- `X-Platform-Slot`: The active Blue/Green slot (`blue` or `green`).
- `X-Release-Commit`: The Git commit SHA deployed in the container.
- `X-Runtime-Role`: The explicit role (`gateway`, `worker`, `auth-browser`).

Traefik route switch scripts (`deploy/switch-slot.sh` and `deploy/lib/traefik.sh`) assert both `X-Platform-Slot` and `X-Release-Commit` before completing candidate promotion.

### 3.2 Split Role Probes
Endpoints enforce role validation when queried with `?role=<expected>` or header `X-Expected-Role: <expected>`:
- `GET /healthz?role=gateway` on gateway returns `200 OK`.
- `GET /healthz?role=worker` on gateway returns `503 Service Unavailable` (`role mismatch`).
- `GET /readyz?role=worker` on worker returns `200 OK`.
- `GET /readyz?role=gateway` on worker returns `503 Service Unavailable` (`role mismatch`).

---

## 4. Privacy & Cardinality Controls

To ensure production compliance and avoid secret leakage or memory bloat:
1. **Zero Secret Leakage:**
   - Telemetry reports and health endpoints **NEVER** expose ACB session cookies, passwords, internal tokens (`WORKER_INTERNAL_TOKEN`, `TTS_INTERNAL_TOKEN`), Cloudflare Access JWTs (`Cf-Access-Jwt-Assertion`), Bark basic auth credentials, or private decryption keys (`age1...`).
   - Raw ACB HTML responses (`<table`, `<html`) are strictly stripped before logging or telemetry collection.
2. **Cardinality Bounds:**
   - High-cardinality entity IDs (history sync job UUIDs, delivery IDs, user subjects) are **strictly forbidden** as keys or dimensions in global metric maps.
   - History metrics report aggregated counts grouped strictly by fixed status enum: `QUEUED`, `RUNNING`, `COMPLETED`, `FAILED`, `CANCELED`.
   - Latency percentiles (P95) use fixed rolling ring buffers capped at 500–1000 samples.

---

## 5. Operational Alert Thresholds & Runbooks

Operational alerts are evaluated continuously and exposed via `/api/v1/telemetry` and `/api/v1/ops/alerts`.

| Alert Name | Severity | Threshold | Root Cause / Impact | Immediate Action Runbook |
| :--- | :--- | :--- | :--- | :--- |
| `ALERT_STALE_REALTIME_POLL` | **WARNING** (>120s)<br>**CRITICAL** (>300s) | No successful ACB realtime poll for >120s / >300s. | ACB banking portal down, session expired, or worker blocked. | 1. Check `GET /api/v1/status`.<br>2. Inspect worker logs: `docker compose logs --tail 100 worker`.<br>3. Verify connection state. If `AUTH_REQUIRED`, trigger re-authentication via auth-browser. |
| `ALERT_STALE_WORKER` | **CRITICAL** | Worker heartbeat >60s without update or state `!= READY`. | Worker crashed, deadlocked, or OOMKilled. | 1. Check `docker compose ps worker`.<br>2. Check `data/worker.lock` singleton owner.<br>3. Inspect OOM / kernel logs: `dmesg -T \| grep -i oom`.<br>4. Restart worker service safely via `deploy/deploy-worker.sh`. |
| `ALERT_AUTH_STUCK` | **WARNING** | Active auth attempt >600s (10m) without completion or session `AUTH_REQUIRED`. | Operator abandoned auth tab or Chromium sidecar hung. | 1. Verify `acb-auth-browser` container health via `docker exec acb-auth-browser /auth-browser --healthcheck`.<br>2. Expire stuck attempt via `/api/v1/auth/cancel` or restart session. |
| `ALERT_QUEUE_SATURATION` | **WARNING** (>20)<br>**CRITICAL** (>50) | Upstream priority scheduler queue depth >20, or oldest queued >300s. | High background history workload or bank rate limiting. | 1. Scheduler quantum yielding automatically prioritizes realtime polls over history.<br>2. Inspect active history jobs: cancel non-urgent jobs via `POST /api/v1/history-sync-jobs/{id}/cancel`. |
| `ALERT_HISTORY_STALL` | **WARNING** | History job running with heartbeat older than 300s. | Worker crash during execution or upstream freeze. | 1. Runner auto-recovers stale jobs via `RequeueStaleHistorySyncJobs`.<br>2. If persistent, verify database locks and worker thread pool. |
| `ALERT_NOTIFICATION_BACKLOG_STUCK` | **WARNING** | Dead-letter deliveries >10 or pending backlog >50. | Target webhook endpoint down, DNS failure, or Bark push service unreachable. | 1. Inspect delivery failures via `GET /api/v1/deliveries?status=DEAD_LETTER`.<br>2. Verify destination endpoint responsiveness.<br>3. Trigger channel test via `POST /api/v1/channels/{id}/test`. |
| `ALERT_BACKUP_OVERDUE` | **WARNING** | Latest encrypted `.db.age` artifact is older than 24 hours (86400s). | Automated preflight or cron backup failed. | 1. Run immediate manual backup: `deploy/backup-db.sh`.<br>2. Verify `BACKUP_AGE_RECIPIENT` public key in `.env.production`.<br>3. Verify disk space on `/var/backups/acb`. |
| `ALERT_RESTORE_DRILL_OVERDUE` | **WARNING** (>30d)<br>**CRITICAL** (failed) | Off-host disaster recovery restore drill older than 30 days or failed. | Disaster recovery confidence degraded. | 1. Execute isolated canary restore drill: `scripts/ops/restore-drill.sh --drill-dir /tmp/acb-drill`.<br>2. Verify recovery private key matches public recipient. |
| `ALERT_MUTATION_GATE_LOCKED` | **WARNING** | Deployment mutation gate locked for >15 minutes (900s). | Deployment failed mid-transaction without releasing gate lease. | 1. Check deployment status receipts under `data/releases/`.<br>2. Recover journal: `deploy/dispatch-rollout.sh` automatically recovers dangling locks.<br>3. If abandoned, release mutation gate via database unlock. |
| `ALERT_WRONG_ROLE_RELEASE_SCHEMA` | **CRITICAL** | Container runtime role mismatch or schema version incompatible. | Configuration error or incomplete rollout. | 1. Verify container environment variables (`RUNTIME_ROLE`).<br>2. Re-run schema migration via `dbtool --migrate`.<br>3. Verify Blue/Green slot route ACK. |
