# ACB Production Compatibility & Interface Contracts

**Document Version:** 1.0 (Canonical)
**Date:** 2026-09-14
**Authoritative Reference:** [`docs/superpowers/specs/2026-09-14-acb-final-production-invariants.md`](../superpowers/specs/2026-09-14-acb-final-production-invariants.md)
**Implementation Plan:** [`docs/superpowers/plans/2026-09-14-acb-production-convergence-execution.md`](../superpowers/plans/2026-09-14-acb-production-convergence-execution.md)

---

## 1. Overview & Purpose

This specification establishes the stable interface contracts across HTTP, RPC, scheduling, database migrations, container deployments, and route verification for `TheDemonTuan/acb-transaction-webhook`. All PR implementations must adhere to these contracts to prevent regressions, races, or downtime during Blue/Green promotions and component restarts.

---

## 2. HTTP & Worker RPC Asynchronous History Contracts

### 2.1 Endpoint Specification

| Endpoint | Method | Success Code | Authorization & Security | Semantics & Execution Invariants |
|---|---|---|---|---|
| `/api/v1/transactions/ensure-history` | `POST` | `202 Accepted` (job queued or active)<br>`200 OK` (cache already covered) | Roles: `OWNER`, `OPERATOR`<br>Header: `Origin` validation, CSRF token | Returns job metadata snapshot immediately. Never runs multi-day ACB requests inline. Forwards create/claim request to worker via Worker RPC. |
| `/api/v1/history-sync-jobs/{jobId}` | `GET` | `200 OK` | Roles: `OWNER`, `OPERATOR`, `VIEWER` | Read-only state snapshot from SQLite. Never calls ACB directly. Never mutates job state. |
| `/api/v1/history-sync-jobs/{jobId}/cancel` | `POST` | `200 OK` | Roles: `OWNER`, `OPERATOR`<br>Header: `Origin`, CSRF | Explicit user cancellation. Transitions job to `CANCELED` via Worker RPC. Closing browser tab does NOT trigger cancel. |
| `/healthz` | `GET` | `200 OK` | Public / Edge | Pure process liveness check. Checks if HTTP server responds. Never fails on ACB session expiry or rate limits. |
| `/readyz` | `GET` | `200 OK` (ready)<br>`503 Service Unavailable` | Public / Edge | Role readiness check. For gateway: verifies DB readable, Worker RPC reachable. For worker: verifies lock held, DB connected. |
| `/internal/deployz` | `GET` | `200 OK`<br>`503 Service Unavailable` | Internal / Deploy Probe | Promotion gate probe. Returns JSON with `status`, `slot`, `releaseCommit`, `schemaVersion`, `activeAuthCount`. Supplies `X-Platform-Slot` and `X-Release-Commit` headers. |

### 2.2 Response DTO Specification

#### Ensure History Response (`POST /api/v1/transactions/ensure-history`)

When work is queued or in progress (`202 Accepted`):
```json
{
  "status": "QUEUED",
  "coverage": "PENDING",
  "synced": false,
  "job": {
    "id": "syncjob_01j7h...",
    "status": "QUEUED",
    "rangeFrom": "2026-09-01",
    "rangeTo": "2026-09-14",
    "rowsSeen": 0,
    "pagesDone": 0,
    "currentDay": null,
    "errorCode": null
  }
}
```

When requested range is already complete in database (`200 OK`):
```json
{
  "status": "COMPLETED",
  "coverage": "COMPLETE",
  "synced": false,
  "job": null
}
```

#### Standard Error Response Envelope
```json
{
  "error": "Mô tả lỗi thân thiện cho người dùng",
  "code": "ERROR_CODE_ENUM",
  "requestId": "req_01a09c31..."
}
```

#### Standard HTTP Error Codes
- `400 Bad Request`: Invalid date range (e.g. `from > to`, range > 31 days, invalid date format). Code: `INVALID_RANGE`.
- `401 Unauthorized`: Missing or invalid Cloudflare Access JWT or session identity. Code: `UNAUTHORIZED`.
- `403 Forbidden`: Insufficient role permissions or CSRF token mismatch. Code: `FORBIDDEN` or `CSRF_TOKEN_INVALID`.
- `404 Not Found`: Requested resource or sync job ID does not exist. Code: `JOB_NOT_FOUND`.
- `409 Conflict`: Generation mismatch or concurrent conflicting operation. Code: `GENERATION_CONFLICT`.
- `429 Too Many Requests`: History sync admission quota exceeded. Code: `RATE_LIMITED`.
- `503 Service Unavailable`: Worker RPC unreachable, deployment write-gate active, or database in maintenance. Code: `SERVICE_UNAVAILABLE`.

### 2.3 Worker RPC Protocol

All inter-process communication between Gateway and Worker uses bounded HTTP/1.1 RPC over the internal network `acb-core`:
- **Headers:**
  - `X-Worker-Internal-Token`: Bearer secret shared only between Gateway and Worker.
  - `X-Request-Id`: Monotonic tracing identifier propagated across service boundaries.
  - `X-Actor-Id`: Cloudflare Access user subject initiating the request.
  - `Idempotency-Key`: Deterministic deduplication key for mutation commands.
- **Timeouts:**
  - RPC calls have a hard `30s` context deadline.
  - No 120s long-polling HTTP requests. Long operations return durable job IDs.

---

## 3. ACB Upstream Scheduler & Quantum Interface

### 3.1 Priority Hierarchy
1. `INTERACTIVE_VERIFY` (Weight: 100) — Manual login/screen verifications triggered by operator.
2. `REALTIME_POLL` (Weight: 80) — 15s adaptive transaction detection loop.
3. `MANUAL_SYNC` (Weight: 60) — Operator-triggered immediate transaction refresh.
4. `KEEPALIVE` (Weight: 40) — Background session refresh / ping.
5. `CATCH_UP` (Weight: 20) — Multi-day gap scan following downtime or startup.
6. `FILTER_HISTORY` (Weight: 10) — Durable background sync for historical date ranges.

### 3.2 Go Interface Definition
```go
package scheduler

import (
    "context"
    "time"
)

type UpstreamPriority int

const (
    PriorityInteractiveVerify UpstreamPriority = 100
    PriorityRealtimePoll      UpstreamPriority = 80
    PriorityManualSync        UpstreamPriority = 60
    PriorityKeepalive         UpstreamPriority = 40
    PriorityCatchUp           UpstreamPriority = 20
    PriorityFilterHistory     UpstreamPriority = 10
)

type TaskStepResult struct {
    Done      bool
    RequeueAt time.Time
    Error     error
}

type UpstreamTask interface {
    ID() string
    Kind() string
    Priority() UpstreamPriority
    Generation() int64
    Step(ctx context.Context) (TaskStepResult, error)
}
```

### 3.3 Single-Page Quantum Yielding Rule
- Any task executing at `PriorityCatchUp` or `PriorityFilterHistory` must execute at most **one HTTP request to the ACB API** during its `Step` method.
- Upon receiving the page response, it persists parsed transactions and updates the day checkpoint in a single database transaction, then returns `TaskStepResult{Done: false}`.
- The scheduler loop re-evaluates the queue before invoking the next task step, guaranteeing that any pending `REALTIME_POLL` executes before the subsequent historical page.

---

## 4. Database Schema Migration Strategy

### 4.1 Invariants
1. **Migrations Are Additive and Forward-Only:** Columns and tables may be added. Columns must not be renamed or dropped while older binary versions might access them.
2. **Gateway Never Migrates:** `cmd/gateway` opens SQLite with `storage.OpenOptions{RunMigrations: false}`. If migrations are missing, it logs an error and enters degraded state.
3. **Explicit Migration Execution via dbtool:** Migrations are applied exclusively by running `cmd/dbtool --migrate` during schema deployment phases before candidate gateway promotion.
4. **Checksum & WAL Verification:** Every migration has a static checksum recorded in `schema_migrations`. Modifying existing migrations is strictly rejected.

### 4.2 Schema Version Sequence
- Baseline Versions 1–7 (audited and frozen):
  - `1`: Foundation (`connections`, `sessions`, `auth_attempts`, `poll_runs`, `checkpoints`, `coverage_gaps`, `transactions`, `events`, `deliveries`)
  - `2`: Webhook endpoints & replay logs
  - `3`: Monitor settings & polling parameters
  - `4`: Voice settings & TTS configuration
  - `5`: Payment QR static & dynamic configurations
  - `6`: Notification channels & multi-provider dispatch
  - `7`: Canonical date backfills & indexes
- Planned Additive Versions:
  - `8`: `history_sync_jobs` durable queue schema & indexes (`008_history_job_queue.sql`)
  - `9`: `deployment_control` durable admission gate table (`009_deployment_control.sql`)

---

## 5. Release Manifest & Environment Schema

### 5.1 Promotion Scope Contract
The release pipeline calculates changes against the latest successfully deployed release commit. Components are categorized into explicit promotion scopes:

| Scope Identifier | Monitored Code Paths | Triggered Promotion Action |
|---|---|---|
| `frontend` | `web/**`, `deploy/frontend-nginx.conf` | Isolated frontend service replacement; gateway and worker remain untouched |
| `gateway` | `cmd/gateway/**`, `internal/httpapi/**`, `internal/csrf/**` | Blue/Green gateway slot promotion |
| `worker` | `cmd/worker/**`, `internal/monitor/**`, `internal/acb/**` | Controlled singleton worker upgrade |
| `schema` | `internal/storage/migrations/**`, `cmd/dbtool/**` | Offline backup + dbtool migration |
| `auth-browser` | `cmd/auth-browser/**`, `Dockerfile.auth-browser` | Sandboxed browser container restart |
| `tts` | `tts-gateway/**` | TTS container update |
| `bark` | `deploy/compose.prod.yaml` (bark service/digest change) | Bark container update |
| `platform` | `deploy/**`, `platform/**` | Host script / Traefik route updates |

### 5.2 Release Manifest Schema (JSON)
```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "ACBReleaseManifest",
  "type": "object",
  "required": [
    "releaseId",
    "gitCommit",
    "buildTimestamp",
    "promotionScope",
    "images",
    "cosignSignature"
  ],
  "properties": {
    "releaseId": { "type": "string" },
    "gitCommit": { "type": "string", "pattern": "^[0-9a-f]{40}$" },
    "buildTimestamp": { "type": "string", "format": "date-time" },
    "promotionScope": {
      "type": "array",
      "items": { "type": "string", "enum": ["gateway", "worker", "schema", "auth-browser", "tts", "bark", "platform"] }
    },
    "images": {
      "type": "object",
      "properties": {
        "gateway": { "type": "string", "pattern": "^.+@sha256:[0-9a-f]{64}$" },
        "worker": { "type": "string", "pattern": "^.+@sha256:[0-9a-f]{64}$" },
        "authBrowser": { "type": "string", "pattern": "^.+@sha256:[0-9a-f]{64}$" },
        "ttsGateway": { "type": "string", "pattern": "^.+@sha256:[0-9a-f]{64}$" }
      }
    },
    "cosignSignature": { "type": "string" }
  }
}
```

---

## 6. Route Switch & Positive Acknowledgement (ACK) Contract

### 6.1 Traefik Route Switch Protocol
1. **Dynamic File Target:** `/opt/edge/dynamic/acb.yml`.
2. **Atomic Write Procedure:**
   ```bash
   # Render target route pointing to candidate slot
   render_traefik_route "$TARGET_SLOT" > /opt/edge/dynamic/acb.yml.tmp
   # Verify YAML syntax
   yamllint /opt/edge/dynamic/acb.yml.tmp
   # Atomic POSIX replacement
   mv /opt/edge/dynamic/acb.yml.tmp /opt/edge/dynamic/acb.yml
   ```
3. **Rollback Backup:** Previous route configuration is saved to `/opt/edge/dynamic/acb.yml.prev` before replacement.

### 6.2 Traefik Identity ACK Verification
The deployment script must positively verify that Traefik is serving requests from the candidate slot before declaring promotion success:

- **Probe Endpoint:** `http://172.31.250.4:8080/internal/deployz` (via Traefik edge network) or internal test routing.
- **Required Response Headers:**
  - `X-Platform-Slot`: Must exactly equal candidate slot (`blue` or `green`).
  - `X-Release-Commit`: Must exactly equal intended Git commit SHA.
- **Retry & Deadline:** Probes repeat every `1s` up to `30s`.
- **Failure Trigger:** If the response does not match within 30 seconds:
  1. Immediately restore `/opt/edge/dynamic/acb.yml.prev` to `/opt/edge/dynamic/acb.yml`.
  2. Verify previous slot identity headers.
  3. Abort deployment transaction and return non-zero exit code.
