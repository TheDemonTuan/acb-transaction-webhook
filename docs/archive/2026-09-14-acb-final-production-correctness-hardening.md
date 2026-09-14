> **SUPERSEDED / HISTORICAL ARCHIVE**
>
> This document is retained solely for historical context, audit trails, and design lineage.
> It has been superseded by the canonical 2026-09-14 production architecture and hardening specifications:
> - **Canonical Specification:** [`docs/superpowers/specs/2026-09-14-acb-final-production-invariants.md`](../superpowers/specs/2026-09-14-acb-final-production-invariants.md)
> - **Production Architecture:** [`docs/architecture/PRODUCTION_ARCHITECTURE.md`](../architecture/PRODUCTION_ARCHITECTURE.md)
> - **Execution Plan & Tracker:** [`docs/superpowers/plans/2026-09-14-acb-production-convergence-execution.md`](../superpowers/plans/2026-09-14-acb-production-convergence-execution.md)
>
> Do not implement, deploy, or operate against this document.

---

# ACB Final Production Correctness & Hardening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Converge `TheDemonTuan/acb-transaction-webhook` from the audited baseline `90ba3fb6ee41c016c94361dc05b033ed6fcb4c1e` to one production architecture where gateway deploys do not interrupt ACB polling, long history/catch-up work cannot block realtime polling, singleton/session rules fail closed, production secrets and backups are safe, Traefik promotion is positively acknowledged, and CI promotes only the components named by a signed immutable release scope.

**Architecture:** Keep Docker Compose on one VPS, Cloudflare Tunnel/Access, the shared Traefik edge under `/opt/edge`, warm Blue/Green stateless gateways, one singleton ACB worker, one auth-browser, one TTS gateway, Bark, and SQLite. Replace the worker's long-held upstream mutex with a single-owner priority scheduler whose low-priority jobs yield after one ACB page. Make filter-history durable/asynchronous, move all singleton maintenance into the worker, make production runtime roles explicit, and split deployment into gateway/schema/worker/auxiliary transactions.

**Tech Stack:** Go 1.27.1, SQLite WAL via `modernc.org/sqlite`, React + TypeScript + TanStack Query, Bun 1.4.2, Docker Compose, Traefik shared edge, Cloudflare Tunnel/Access, GitHub Actions, GHCR, Trivy 0.74.0 or the pinned repository version at implementation time, Cosign keyless OIDC, `age` for backup encryption, Bash + ShellCheck, Python failover controller.

**Spec:** `docs/superpowers/specs/2026-09-14-acb-final-production-invariants.md`

## Global Constraints

- [ ] Work from the exact latest `main` commit at implementation start and record it in the implementation PR description. Reconcile any later commits against this plan before editing overlapping code.
- [ ] Do not introduce Kubernetes, K3s, Docker Swarm, Redis, RabbitMQ, or an additional reverse proxy.
- [ ] Preserve shared Traefik under `/opt/edge`; do not add a per-repository Traefik/Caddy/cloudflared instance.
- [ ] Preserve SQLite as canonical application state. Do not migrate to a network database as part of this plan.
- [ ] Preserve atomic generation-fenced transaction/event/delivery ingestion in `internal/storage/transactions.go`.
- [ ] Preserve Cloudflare JWT verification, same-origin/CSRF enforcement, DNS-aware webhook SSRF protection, delivery leasing, ACB official-host checks, and SSE durable journal replay unless a task below explicitly changes their boundary.
- [ ] A production HTTP gateway must never perform ACB polling or notification-dispatch loops.
- [ ] A gateway-only promotion must leave `acb-worker`, `acb-auth-browser`, `acb-tts-gateway`, and `acb-bark` container IDs unchanged.
- [ ] Long-running ACB work must be asynchronous or scheduler-driven; do not solve long history operations by merely increasing HTTP timeouts.
- [ ] Every safety check that protects session state, production data, release identity, or an active login is fail-closed.
- [ ] No normal deployment path generates secrets.
- [ ] No durable backup stores a plaintext SQLite snapshot or plaintext secret copy.
- [ ] No production image reference falls back to a mutable tag.
- [ ] Tests are written or updated before the implementation change they prove.
- [ ] Run `go test -race ./...`, `go vet ./...`, frontend typecheck/build/tests, TTS tests, deployment tests, failover tests, ShellCheck, and `git diff --check` before final completion.
- [ ] Commit after each task or tightly coupled pair using the suggested commit message. Do not combine unrelated phases into one commit.

---

## Baseline Audit Findings This Plan Must Close

The following are treated as known defects or architectural gaps at audited commit `90ba3fb6ee41c016c94361dc05b033ed6fcb4c1e`:

1. `internal/monitor/monitor.go` holds `m.mu` for entire filter-history and catch-up operations, so a long history/catch-up can block realtime polling.
2. `internal/workerrpc/rpc.go` gives `EnsureHistory` a 120-second context/server timeout while the underlying HTTP client remains 30 seconds and the worker HTTP server write timeout remains 30 seconds.
3. `history_sync_jobs` is an audit record around a synchronous request, not a durable asynchronous queue; completion updates use the request context and can remain `RUNNING` after cancellation.
4. `cmd/worker/main.go` generation verification proceeds if connection lookup itself fails.
5. `internal/monitor/monitor.go` proceeds with polling if `HasActiveAuthAttempt` returns an error.
6. `cmd/gateway/main.go` still supports production monolith fallback when worker RPC configuration is absent.
7. journal retention and stale-auth periodic maintenance are launched from gateway processes, so Blue and Green can duplicate singleton maintenance during soak.
8. `web/src/shared/api/queries.ts` posts `ensure-history` without explicitly obtaining a CSRF token while `web/src/api.ts` does not automatically protect generic mutations.
9. `deploy/lib.sh` can generate missing production secrets during deployment and fresh init reuses the same helper.
10. `deploy/lib.sh` active-auth deployment gate converts dbtool/sqlite failures into an apparent zero active-auth count.
11. backup logic encrypts a copy using `app_master_key`, retains plaintext, copies the decryption key and other secrets beside it, and sends the plaintext path to the off-host hook.
12. `deploy/compose.prod.yaml` contains `latest` fallbacks and grants worker/gateway more secrets than required.
13. `deploy/deploy-warm.sh` warns and continues if worker readiness times out.
14. gateway candidate pre-promotion currently proves generic health before route switch, not the full deployment dependency contract twice consecutively.
15. route acknowledgement can succeed without proving that Traefik actually loaded and is serving the candidate route when `ROUTE_ACK_URL` is absent.
16. `.github/workflows/deploy.yml` currently exports `UPGRADE_CORE=1` and runs `deploy-warm.sh --upgrade-core` on every automatic main deployment, so ordinary gateway/UI changes can replace core singleton services.
17. the VPS manifest verifier can skip Cosign when it is absent and defaults to a wildcard certificate identity when no expected identity is supplied.
18. the Cosign fallback download is executed without a checksum verification step.
19. production env-path ownership is inconsistent: Compose expects `deploy/.env.production` while the current remote workflow also mutates a root-level `.env.production`.
20. older Caddy/incremental fix documents remain at repository root even though the active deployment path is Traefik, creating implementation ambiguity.

These findings define the scope. New unrelated features are excluded until the final acceptance gate passes.

---

## Phase 0 — Freeze the Contract Before Refactoring

### Task 1: Add the final invariant spec to the repository and mark older plans superseded

**Files:**
- Create: `docs/superpowers/specs/2026-09-14-acb-final-production-invariants.md`
- Create: `docs/architecture/PRODUCTION_ARCHITECTURE.md`
- Modify: `README.md`
- Modify: `2026-09-12-caddy-progressive-blue-green-deployment.md`
- Modify: `2026-09-13-acb-secure-single-vps-platform-migration.md`
- Modify: `2026-09-13-single-vps-secure-container-platform-standard.md`
- Modify: `fix.md`
- Modify: `fix_2.md`

- [ ] Copy the invariant spec supplied with this plan into `docs/superpowers/specs/2026-09-14-acb-final-production-invariants.md` without weakening any MUST/forbidden rule.
- [ ] Add `docs/architecture/PRODUCTION_ARCHITECTURE.md` as a short operator-facing architecture overview that names Traefik, gateway Blue/Green, singleton worker, auth-browser, TTS, Bark, SQLite, and the three Docker networks.
- [ ] Add a top banner to each older root plan stating that it is historical and that the 2026-09-14 invariant spec is authoritative. Do not delete historical context in this task.
- [ ] Update `README.md` so new contributors are directed first to the final invariant spec and this implementation plan.
- [ ] Add a repository test script `scripts/verify-architecture-docs.sh` that fails if the authoritative architecture document mentions a per-app Caddy/cloudflared production stack or if README points at an older plan as current.
- [ ] Add the script to CI's verify job.
- [ ] Run `bash scripts/verify-architecture-docs.sh` and `git diff --check`.

**Commit:** `docs: establish final ACB production invariants`

---

## Phase 1 — Make Runtime Roles and HTTP Mutation Safety Explicit

### Task 2: Introduce explicit runtime roles and forbid production monolith fallback

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `cmd/gateway/main.go`
- Modify: `cmd/gateway/main_test.go`
- Modify: `cmd/worker/main.go`
- Modify: `deploy/compose.prod.yaml`
- Modify: `.env.example`

**Target API:**

```go
type RuntimeRole string

const (
    RuntimeRoleGateway     RuntimeRole = "gateway"
    RuntimeRoleWorker      RuntimeRole = "worker"
    RuntimeRoleMonolithDev RuntimeRole = "monolith-dev"
)
```

- [ ] Write config tests proving production rejects an empty role, `monolith-dev`, and any unknown role.
- [ ] Write config tests proving a production gateway rejects missing `WORKER_RPC_URL` and missing `WORKER_INTERNAL_TOKEN`/file.
- [ ] Write config tests proving `monolith-dev` is allowed only when `APP_ENV` is not `production`.
- [ ] Add `RuntimeRole` to `config.Config` and parse `RUNTIME_ROLE`.
- [ ] In `cmd/gateway/main.go`, use only two branches: production `gateway` path or non-production `monolith-dev` path. Remove the implicit meaning “empty WorkerRPCURL means monolith”.
- [ ] In `cmd/worker/main.go`, fail startup unless role is `worker` in production.
- [ ] Set `RUNTIME_ROLE=worker` on worker and `RUNTIME_ROLE=gateway` on both gateway slots in Compose.
- [ ] Update `.env.example` with the role explanation but do not put a production default that makes a wrong binary silently valid.
- [ ] Run `go test -race ./internal/config ./cmd/gateway ./cmd/worker`.

**Acceptance:** starting two production gateway slots with missing worker RPC configuration must fail both gateways before either can construct an ACB client.

**Commit:** `fix(runtime): make production gateway and worker roles fail closed`

### Task 3: Centralize CSRF handling in the frontend transport

**Files:**
- Modify: `web/src/api.ts`
- Modify: `web/src/api.test.ts`
- Modify: `web/src/shared/api/queries.ts`
- Modify: `web/src/shared/api/queries.test.ts`
- Modify: existing feature modules that manually call `getCsrfToken()` only for ordinary JSON mutations

**Target behavior:**

```ts
const isMutation = (method: string) =>
  method === 'POST' || method === 'PUT' || method === 'PATCH' || method === 'DELETE';
```

- [ ] Add tests for POST/PUT/PATCH/DELETE proving `api()` requests a CSRF token and injects `X-CSRF-Token` while preserving caller headers.
- [ ] Add a test proving GET/HEAD do not request a CSRF token.
- [ ] Add a test proving `CSRF_TOKEN_INVALID` triggers one forced token refresh and one retry only.
- [ ] Add a test proving ordinary 400/401/403/409/500 responses are not automatically replayed.
- [ ] Add a test proving an `AbortSignal` is preserved through the wrapper.
- [ ] Refactor `api()` to implement the policy once.
- [ ] Change `getCsrfToken()` to continue using a GET so there is no recursion.
- [ ] Remove repeated `getCsrfToken()` plumbing from ordinary JSON mutations after tests prove behavior is unchanged.
- [ ] Leave specialized binary/audio behavior using the same shared CSRF helper rather than a separate policy.
- [ ] Verify `ensureHistory()` succeeds through a real backend test with CSRF middleware enabled.
- [ ] Run `cd web && bunx --bun tsc --noEmit && bunx --bun vitest run`.

**Commit:** `fix(web): centralize CSRF protection for all mutations`

### Task 4: Close database-error fail-open paths before ACB requests

**Files:**
- Modify: `cmd/worker/main.go`
- Modify: `cmd/worker/service_test.go`
- Modify: `internal/monitor/monitor.go`
- Modify: `internal/monitor/monitor_test.go`
- Modify/Create: `internal/monitor/fail_closed_test.go`

- [ ] Add a worker-service test with a failing connection store proving `VerifySession` returns an error and the verifier client receives zero calls.
- [ ] Refactor `workerService.VerifySession` so `store.Connection(ctx)` errors immediately return a wrapped error.
- [ ] Keep strict `conn.Generation == generation` equality.
- [ ] Add a monitor test where `HasActiveAuthAttempt` returns a database error and prove `Bootstrap`/`History` receive zero calls.
- [ ] Refactor `pollOnce()` so active-auth lookup error aborts locally; do not treat unknown as “no active auth”.
- [ ] Add structured sanitized log fields for both fail-closed paths.
- [ ] Run `go test -race ./cmd/worker ./internal/monitor`.

**Commit:** `fix(session): fail closed before ACB calls on state lookup errors`

---

## Phase 2 — Replace the Global ACB Mutex With a Priority Scheduler

### Task 5: Implement scheduler primitives with deterministic priority and bounded quanta

**Files:**
- Create: `internal/monitor/upstream_scheduler.go`
- Create: `internal/monitor/upstream_scheduler_test.go`
- Create: `internal/monitor/upstream_task.go`
- Modify: `internal/monitor/monitor.go`

**Target types:**

```go
type UpstreamPriority uint8

const (
    PriorityInteractiveVerify UpstreamPriority = iota
    PriorityRealtimePoll
    PriorityManualSync
    PriorityKeepalive
    PriorityCatchUp
    PriorityFilterHistory
)

type TaskStepResult struct {
    Done       bool
    RequeueAt  time.Time
}

type UpstreamTask interface {
    ID() string
    Priority() UpstreamPriority
    Step(context.Context) (TaskStepResult, error)
}
```

- [ ] Write a deterministic test proving priorities are selected in the order above.
- [ ] Write a test proving an unfinished low-priority task is requeued after one `Step`, allowing an arriving realtime task to run before the low-priority task's next step.
- [ ] Write a test proving duplicate realtime/manual task keys are coalesced rather than queued unboundedly.
- [ ] Write a cancellation test proving scheduler shutdown does not leave a blocked submitter/goroutine.
- [ ] Write a bounded-queue test proving overload returns a typed local error instead of blocking forever.
- [ ] Implement one scheduler goroutine; no task executes its ACB `Step` concurrently with another task.
- [ ] Add metrics hooks for queue depth by priority, current task kind, and task duration.
- [ ] Keep the scheduler independent from HTTP/RPC contexts after a durable background task is accepted.
- [ ] Do not remove `m.mu` until realtime/manual/keepalive/catch-up/verifier paths have migrated in later tasks.
- [ ] Run `go test -race ./internal/monitor -run Scheduler`.

**Commit:** `feat(worker): add priority ACB upstream scheduler`

### Task 6: Move realtime and manual polling onto scheduler tasks

**Files:**
- Create: `internal/monitor/realtime_task.go`
- Create: `internal/monitor/realtime_task_test.go`
- Modify: `internal/monitor/monitor.go`
- Modify: `internal/monitor/monitor_test.go`
- Modify: `cmd/worker/main.go`

- [ ] Extract the existing `pollOnce` body into a realtime task owned by the worker scheduler without changing parsing, generation fencing, partial-page handling, ingestion policy, event emission, or session persistence semantics.
- [ ] Add a test for the existing five-page realtime budget and `PARTIAL` behavior.
- [ ] Add a test proving a partial realtime poll schedules catch-up but still commits its safely parsed rows.
- [ ] Change scheduled realtime timers to enqueue/coalesce `PriorityRealtimePoll` rather than synchronously executing a poll.
- [ ] Change `RequestSync` to enqueue/coalesce `PriorityManualSync` and return after acceptance.
- [ ] Make manual sync generation-aware so a queued request from an old generation is discarded before any ACB request.
- [ ] Remove outer `m.mu` ownership from the realtime path once scheduler ownership is active.
- [ ] Preserve `acb.Client`'s internal mutex as defensive state protection; scheduler serialization is the architectural owner.
- [ ] Run `go test -race ./internal/monitor ./cmd/worker`.

**Commit:** `refactor(worker): schedule realtime and manual ACB polls`

### Task 7: Move keepalive onto the scheduler

**Files:**
- Create: `internal/monitor/keepalive_task.go`
- Create: `internal/monitor/keepalive_task_test.go`
- Modify: `internal/monitor/monitor.go`

- [ ] Add a test proving keepalive performs bootstrap/session refresh but never a history call.
- [ ] Add a test proving a queued realtime poll runs before keepalive when both are available.
- [ ] Add tests for login expiry, 429, and maintenance backoff behavior.
- [ ] Convert keepalive timer execution to `PriorityKeepalive` scheduler work.
- [ ] Remove the keepalive use of the legacy global upstream mutex.
- [ ] Keep persistence of refreshed cookies/session after successful keepalive.
- [ ] Run `go test -race ./internal/monitor -run Keepalive`.

**Commit:** `refactor(worker): schedule ACB keepalive without global lock`

### Task 8: Make catch-up resumable and preemptible between ACB pages

**Files:**
- Create: `internal/monitor/catchup_task.go`
- Create: `internal/monitor/catchup_task_test.go`
- Modify: `internal/monitor/monitor.go`
- Modify: `internal/storage/history.go` or the file containing coverage/checkpoint helpers
- Modify: relevant storage tests

- [ ] Write a test with a multi-day/multi-page catch-up where a realtime task arrives after page one; prove realtime executes before catch-up page two.
- [ ] Write a test proving coverage/checkpoint advances only after the full current day completes.
- [ ] Write a test proving worker restart can reconstruct catch-up from durable checkpoint/coverage without persisting ACB form tokens.
- [ ] Write a test proving `CATCH_UP` still creates deliveries for newly discovered credits while suppressing realtime voice behavior.
- [ ] Refactor catch-up into an in-memory task whose `Step` performs at most one ACB page plus local ingest/checkpoint work.
- [ ] On startup/transition to realtime, enqueue catch-up but also enqueue an immediate realtime poll; do not block realtime until catch-up finishes.
- [ ] On transient network/maintenance/rate-limit errors, requeue catch-up with bounded backoff while allowing realtime/keepalive decisions to continue.
- [ ] Remove `m.catchUpPending` as a long synchronous execution flag once durable checkpoint + task presence is sufficient. If a small boolean remains for coalescing, it must not imply exclusive execution.
- [ ] Delete the old monolithic `catchUp()` lock-held loop after tests pass.
- [ ] Run `go test -race ./internal/monitor ./internal/storage`.

**Commit:** `refactor(worker): make catch-up preemptible by realtime polling`

### Task 9: Route session verification through the scheduler and retire `UpstreamGate`

**Files:**
- Modify: `internal/monitor/verifier.go`
- Modify: `internal/monitor/verifier_test.go`
- Modify: `internal/monitor/monitor.go`
- Modify: `cmd/worker/main.go`
- Modify: `cmd/worker/service_test.go`

- [ ] Add a test proving interactive verification outranks queued realtime/history work.
- [ ] Add a test proving verification and realtime ACB calls never overlap.
- [ ] Change `SessionVerifier` from taking `*sync.Mutex` to submitting an `INTERACTIVE_VERIFY` task to the scheduler.
- [ ] Keep the dedicated verifier ACB client if it is still useful for isolation, but serialize the verification request through the same upstream scheduler policy.
- [ ] Remove `Monitor.UpstreamGate()` after all callers migrate.
- [ ] Remove the worker's dependency on the legacy global `m.mu` for ACB ownership.
- [ ] Run `go test -race ./internal/monitor ./cmd/worker` and confirm the race detector stays clean.

**Commit:** `refactor(session): serialize verification through ACB scheduler`

---

## Phase 3 — Convert User History Sync Into a Durable Asynchronous Job

### Task 10: Expand `history_sync_jobs` for durable queue semantics

**Files:**
- Modify: `internal/storage/storage.go` or migration registry
- Prefer Create: `internal/storage/migrations/008_history_job_queue.sql`
- Prefer Create: `internal/storage/migrations/embed.go`
- Modify: `internal/storage/history_jobs.go`
- Modify: `internal/storage/history_jobs_test.go`
- Modify: `cmd/dbtool/main.go` only if migration discovery changes

**Migration target fields:**

```text
generation INTEGER
pages_done INTEGER
current_day TEXT
attempts INTEGER
next_attempt_at TEXT
error_code TEXT
started_at TEXT
heartbeat_at TEXT
finished_at TEXT
```

- [ ] First extract future SQL migrations from the growing Go literal into `internal/storage/migrations/` while retaining exact checksums/version ordering for versions already applied.
- [ ] Add migration 8 as additive-only; do not rebuild/drop the current table during the same release.
- [ ] Backfill existing rows with safe defaults and keep historical `COMPLETED`/`FAILED` rows readable.
- [ ] Add a partial unique index that prevents duplicate active jobs for the same `(connection_id, generation, range_from, range_to)` where status is `QUEUED` or `RUNNING`.
- [ ] Define typed status constants `QUEUED`, `RUNNING`, `COMPLETED`, `FAILED`, `CANCELED`.
- [ ] Add storage tests for migration from a version-7 database, active-job uniqueness, and historical-row readability.
- [ ] Run `go test -race ./internal/storage`.

**Commit:** `feat(storage): add durable history job queue schema`

### Task 11: Implement atomic history-job create/claim/heartbeat/requeue/complete operations

**Files:**
- Rewrite: `internal/storage/history_jobs.go`
- Expand: `internal/storage/history_jobs_test.go`

**Required store methods:**

```go
CreateOrGetHistorySyncJob(ctx context.Context, connectionID string, generation int64, fromDay, toDay string) (HistorySyncJob, bool, error)
ClaimNextHistorySyncJob(ctx context.Context, now time.Time) (HistorySyncJob, bool, error)
HeartbeatHistorySyncJob(ctx context.Context, jobID, currentDay string, pagesDone, rowsSeen int) error
RequeueHistorySyncJob(ctx context.Context, jobID, errorCode, errorMessage string, nextAttemptAt time.Time) error
CompleteHistorySyncJob(ctx context.Context, jobID string, pagesDone, rowsSeen int) error
FailHistorySyncJob(ctx context.Context, jobID, errorCode, errorMessage string) error
CancelHistorySyncJob(ctx context.Context, jobID string) error
RequeueStaleHistorySyncJobs(ctx context.Context, staleBefore time.Time) (int, error)
GetHistorySyncJob(ctx context.Context, jobID string) (HistorySyncJob, error)
```

- [ ] Test concurrent `CreateOrGet` calls and prove one active durable job is returned.
- [ ] Test atomic claim so two callers cannot both own one job.
- [ ] Test heartbeat/progress updates.
- [ ] Test transient requeue with `next_attempt_at`.
- [ ] Test terminal completion/failure/cancel transitions and reject invalid transitions.
- [ ] Test stale `RUNNING` recovery.
- [ ] Sanitize stored error messages and enforce a length bound; do not store HTML bodies, cookies, tokens, or passwords.
- [ ] Implement all state transitions in SQLite transactions where a race could otherwise double-claim or revive a terminal job.
- [ ] Run `go test -race ./internal/storage -run HistorySyncJob`.

**Commit:** `feat(storage): make history jobs durable and recoverable`

### Task 12: Implement the low-priority history job runner

**Files:**
- Create: `internal/monitor/history_job_runner.go`
- Create: `internal/monitor/history_job_runner_test.go`
- Refactor: `internal/monitor/monitor.go`
- Refactor: `internal/monitor/singleflight.go` and tests if no longer needed by history
- Modify: `cmd/worker/main.go`

- [ ] Build a fake paginated ACB client and write a 31-day history test where each day has multiple pages.
- [ ] Prove each runner `Step` performs at most one ACB page request.
- [ ] Prove a realtime task inserted after a history page runs before the next history page.
- [ ] Prove filter-history ingestion uses `FILTER_SYNC` and creates no notification deliveries/events.
- [ ] Prove a worker shutdown/canceled scheduler context leaves the job requeueable rather than permanently `RUNNING`.
- [ ] Prove a simulated worker restart requeues stale work and safely repeats the current day from page one without duplicate transactions.
- [ ] Classify transient failures into requeue/backoff and auth/parser/invalid-session failures into terminal typed errors.
- [ ] Use an independent bounded context when recording final/requeue state after the task's request context is canceled.
- [ ] Delete the old synchronous `Monitor.EnsureHistory` implementation once runner tests cover its behavior.
- [ ] Remove history's dependency on the home-grown blocking `Group` if it no longer serves another caller.
- [ ] Start the history runner/scanner from `cmd/worker/main.go`; combine durable scan with an in-memory wake signal so accepted jobs start promptly without busy polling.
- [ ] Run `go test -race ./internal/monitor ./internal/storage ./cmd/worker`.

**Commit:** `feat(worker): run durable history jobs as low-priority ACB work`

### Task 13: Replace synchronous history worker RPC with short job RPCs

**Files:**
- Modify: `internal/workerrpc/rpc.go`
- Modify: `internal/workerrpc/rpc_test.go`
- Modify: `cmd/worker/main.go`
- Modify: `cmd/worker/service_test.go`
- Modify: `deploy/generate-release-manifest.sh`

**New worker RPC contract:**

```text
POST /rpc/history-jobs
POST /rpc/history-jobs/{jobID}/cancel
```

The first route returns the durable job descriptor immediately. Gateway reads ongoing job status from shared SQLite through the ordinary storage API.

- [ ] Add RPC tests for valid enqueue, duplicate-range reuse, invalid range, canceled context before enqueue, auth failure, body limit, and bounded concurrency.
- [ ] Remove `EnsureHistory` from `WorkerHandler` and replace it with `CreateHistoryJob`/`CancelHistoryJob` methods.
- [ ] Reduce ordinary worker RPC timeout defaults to bounded control-plane values; no RPC path should require a 120-second handler timeout after this refactor.
- [ ] Set the underlying `http.Client.Timeout` consistently above the largest short control RPC timeout or rely on explicit request contexts, but never keep contradictory 30/120 second layers.
- [ ] Keep worker HTTP server read/write timeouts compatible with the short RPC contract.
- [ ] Increment worker RPC compatibility version in `deploy/generate-release-manifest.sh` because gateway and worker must be promoted together for this contract change.
- [ ] Add compatibility tests proving an old gateway/new worker or new gateway/old worker is rejected by deployment manifest rules for this release.
- [ ] Run `go test -race ./internal/workerrpc ./cmd/worker`.

**Commit:** `refactor(rpc): replace long history RPC with durable job control`

### Task 14: Change the dashboard HTTP API to async history jobs

**Files:**
- Modify: `internal/httpapi/server.go`
- Modify: `internal/httpapi/ensure_history_test.go`
- Modify/Create: `internal/httpapi/history_jobs_test.go`
- Modify: `internal/httpapi/server_test.go`

**HTTP contract:**

```text
POST /api/v1/transactions/ensure-history          -> 202 job descriptor
GET  /api/v1/transactions/history-sync-jobs/{id} -> 200 job descriptor
DELETE /api/v1/transactions/history-sync-jobs/{id} -> 202 canceled/terminal descriptor
```

- [ ] Add tests for CSRF/auth/RBAC on POST and DELETE.
- [ ] Add input tests for malformed date, from-after-to, and maximum 31-day range.
- [ ] Add test proving POST returns 202 quickly without waiting for the ACB fake to finish pages.
- [ ] Add test proving GET cannot leak a job from a different connection if multi-connection support is later introduced; current singleton connection should still validate ownership/context.
- [ ] Map job status/error codes to bounded user-safe JSON and never echo raw upstream HTML/errors.
- [ ] Audit every job mutation.
- [ ] Publish job state changes into the event journal where useful for UI refresh.
- [ ] Run `go test -race ./internal/httpapi`.

**Commit:** `feat(api): expose asynchronous history synchronization jobs`

### Task 15: Update Transactions UI for asynchronous progress instead of a long spinner

**Files:**
- Modify: `web/src/shared/api/queries.ts`
- Modify: `web/src/shared/api/query-keys.ts`
- Modify: `web/src/realtime-types.ts`
- Modify: `web/src/pages/viewer/TransactionsPage.tsx`
- Modify: `web/e2e/transactions-sync.spec.ts`
- Add/Modify relevant Vitest component tests

- [ ] Define a typed `HistorySyncJob` frontend model matching the API.
- [ ] Change `ensureHistory()` to return the accepted job.
- [ ] Add `fetchHistorySyncJob(jobID)` and `cancelHistorySyncJob(jobID)`.
- [ ] Use TanStack Query polling only while status is `QUEUED` or `RUNNING`, at a 1-second initial interval that may back off to 2 seconds for long jobs.
- [ ] Stop status polling immediately on terminal state or component unmount.
- [ ] Refetch transaction data exactly once when the job reaches `COMPLETED`.
- [ ] Display Vietnamese user-facing progress: current day, pages processed, rows seen, and a non-technical failure message.
- [ ] Do not show internal error constants as raw UI labels.
- [ ] Preserve the separate “Tải lại dữ liệu đã lưu” button as a local refetch that never contacts ACB.
- [ ] E2E test: accepted job -> running -> completed -> transaction refetch.
- [ ] E2E test: page navigation/unmount while job continues -> returning to page can display current/latest job rather than creating duplicates.
- [ ] E2E test: CSRF refresh path succeeds.
- [ ] Run `cd web && bunx --bun tsc --noEmit && bunx --bun vite build && bunx --bun vitest run` and the repository's Playwright command used in CI/local docs.

**Commit:** `feat(web): show durable ACB history sync progress`

---

## Phase 4 — Make the Worker the Only Singleton Maintenance Owner

### Task 16: Move journal retention and stale-auth maintenance out of gateways

**Files:**
- Create: `internal/maintenance/runner.go`
- Create: `internal/maintenance/runner_test.go`
- Modify: `internal/httpapi/realtime.go`
- Modify: `cmd/gateway/main.go`
- Modify: `cmd/worker/main.go`

- [ ] Add maintenance runner tests with controllable clocks/tickers.
- [ ] Move hourly `DeleteJournalBefore` scheduling into `internal/maintenance.Runner` started only by worker.
- [ ] Move periodic `ExpireStaleAuthAttempts` into the same worker-owned runner.
- [ ] Keep a one-time stale-auth reap on the specific auth API request/start path where correctness requires it; remove generic duplicate gateway background loops.
- [ ] Delete `Server.RunJournalRetention` after callers migrate.
- [ ] Run two gateway instances against the same test DB and prove no gateway starts singleton maintenance.
- [ ] Run `go test -race ./internal/maintenance ./internal/httpapi ./cmd/gateway ./cmd/worker`.

**Commit:** `refactor(worker): own singleton maintenance outside HTTP gateways`

### Task 17: Bound detached journal writes and preserve SSE replay across slot switches

**Files:**
- Modify: `internal/httpapi/events.go`
- Modify: `internal/httpapi/sse_test.go`
- Modify: `internal/httpapi/realtime.go`
- Modify: `internal/storage/journal.go` if helper support is needed

- [ ] Replace unbounded `context.Background()` journal append with a helper that creates a short independent timeout context, for example 2 seconds.
- [ ] Add a test proving canceled HTTP request context does not cancel a state event already committed as an accepted mutation, while the bounded append still terminates.
- [ ] Add a Blue/Green replay test: gateway A writes events, client reconnects through gateway B using the last journal sequence, gateway B replays every missing event exactly once from its perspective.
- [ ] Keep watcher batch-drain logic for more than 100 queued events.
- [ ] Add a retention-boundary test proving reconnect behavior is explicit when a cursor is older than retained history.
- [ ] Run `go test -race ./internal/httpapi ./internal/storage`.

**Commit:** `fix(realtime): bound journal writes and prove cross-slot replay`

### Task 18: Add worker drain state and graceful singleton shutdown

**Files:**
- Create: `internal/workerstate/runtime.go`
- Create: `internal/workerstate/runtime_test.go`
- Modify: `cmd/worker/main.go`
- Modify: `internal/workerrpc/rpc.go`
- Modify: `internal/workerrpc/rpc_test.go`
- Modify: `deploy/compose.prod.yaml`

**Worker state:** `STARTING`, `READY`, `DRAINING`, `STOPPING`.

- [ ] Add readiness tests proving only `READY` returns worker-ready.
- [ ] Add a private authenticated `POST /rpc/drain` command used only by deployment tooling.
- [ ] On drain, reject new upstream-producing commands with 503/typed draining error while allowing health/status calls.
- [ ] Let the current bounded scheduler quantum finish up to a strict drain deadline.
- [ ] Persist the latest live session snapshot using a fresh bounded shutdown context.
- [ ] Stop maintenance/scanners, stop accepting RPC, cancel scheduler, allow notification sends currently in progress a bounded grace period, then release the lock.
- [ ] Configure `stop_grace_period: 30s` or a tested value in Compose.
- [ ] Add SIGTERM tests around the runtime coordinator without requiring real ACB.
- [ ] Run `go test -race ./internal/workerstate ./internal/workerrpc ./cmd/worker`.

**Commit:** `feat(worker): add graceful drain and session-preserving shutdown`

### Task 19: Harden auth-browser lifecycle and shutdown

**Files:**
- Modify: `cmd/auth-browser/main.go`
- Modify: auth-browser command tests
- Modify: `internal/authbrowser/client.go` only if additional typed errors are needed
- Modify: `internal/httpapi/server.go`
- Modify: auth-flow tests

- [ ] Replace `httpServer.Shutdown(context.Background())` with a bounded shutdown context.
- [ ] Add tests proving transient `Status` network/5xx failures retain active attempts rather than marking them failed.
- [ ] Keep confirmed 404 behavior terminal because the browser session no longer exists.
- [ ] Add a test proving an explicit cancellation persists `CANCELLED` even if upstream browser deletion already returns 404.
- [ ] Add a test proving handoff generation changes make stale verification fail before session adoption.
- [ ] Run `go test -race ./internal/authbrowser ./internal/httpapi ./cmd/auth-browser`.

**Commit:** `fix(auth): bound browser shutdown and preserve transient login state`

---

## Phase 5 — Make Secrets and Backups Fail-Closed

### Task 20: Split secret validation from one-time secret provisioning

**Files:**
- Modify: `deploy/lib.sh`
- Modify: `deploy/init-fresh-data.sh`
- Create: `deploy/provision-secrets.sh`
- Create/Modify: `deploy/tests/test_secrets.sh`
- Modify: `deploy/tests/test_deploy.sh`
- Modify: `README.md`

**Required shell functions:**

```text
check_required_secrets
check_secret_permissions
provision_fresh_secrets
assert_fresh_installation
```

- [ ] Write a deploy test where `app_master_key` is missing and prove normal deploy exits non-zero without creating the file.
- [ ] Write the same missing-secret tests for worker token, TTS token, and Bark credentials.
- [ ] Write a fresh-provision test proving secrets are generated only after the explicit fresh-init confirmation and only when data state proves the installation is new.
- [ ] Add `assert_fresh_installation` checks for an existing `gateway.db`, an existing non-empty production data volume, and pre-existing key files; default behavior is to abort rather than rotate/adopt.
- [ ] Refactor current `validate_secrets` into validation-only behavior and move generation code to `provision-secrets.sh`/fresh-init.
- [ ] Generate `app_master_key` as exactly 32 bytes in a format already accepted by the keyring loader; generate independent high-entropy worker/TTS/Bark secrets.
- [ ] Set restrictive ownership/modes and verify first-party UID 1000 containers can read only the files they are assigned.
- [ ] Remove Bark credential mode 0644. Use an explicit owner/read mode compatible with the Bark entrypoint; if Bark runs as root, use root-owned 0400. If an upstream UID is required, prove it in a container test and grant only that UID.
- [ ] Add `umask 077` to provisioning scripts.
- [ ] Run `bash deploy/tests/test_secrets.sh`, `bash deploy/tests/test_deploy.sh`, and `shellcheck --severity=error deploy/*.sh deploy/tests/*.sh`.

**Commit:** `fix(secrets): forbid implicit production secret generation`

### Task 21: Make configuration role-aware and reduce secret scope

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `cmd/gateway/main.go`
- Modify: `cmd/worker/main.go`
- Modify: `internal/workerrpc/rpc.go`
- Modify: `internal/httpapi/server.go`
- Modify: `deploy/compose.prod.yaml`

- [ ] Add role-aware config tests proving worker does not require TTS configuration/token and gateway does not require Bark server credentials once notification test execution is worker-owned.
- [ ] Remove production `APP_MASTER_KEY` env-to-file auto-write; production accepts `APP_MASTER_KEY_FILE` only.
- [ ] Keep direct env secrets only for local development/test if still useful.
- [ ] Add a worker RPC command for notification-channel test execution so gateway does not construct a Bark sender with Bark basic-auth credentials.
- [ ] Move the actual provider test send to worker and return only bounded test result data to gateway.
- [ ] Remove `tts_internal_token` from worker secret mounts and clear worker's TTS URL default/config.
- [ ] Remove Bark user/password secret mounts from gateway slots after worker-owned test send passes.
- [ ] Keep gateway secrets to app master key + worker internal token + TTS internal token.
- [ ] Keep worker secrets to app master key + worker internal token + Bark credentials.
- [ ] Add Compose/config tests or a script that renders service secret assignments and compares them to the expected matrix.
- [ ] Run `go test -race ./internal/config ./internal/workerrpc ./internal/httpapi ./cmd/gateway ./cmd/worker`.

**Commit:** `refactor(security): enforce role-specific secret scope`

### Task 22: Replace backup-at-rest logic with independent age encryption

**Files:**
- Create: `deploy/backup-db.sh`
- Create: `deploy/backup-secrets.sh`
- Create: `deploy/restore-db.sh`
- Create: `deploy/tests/test_backup.sh`
- Modify: `deploy/lib.sh`
- Modify: `deploy/README.md`
- Modify: `.env.example`

**Configuration:**

```text
BACKUP_AGE_RECIPIENT=age1...
BACKUP_DIR=/var/backups/acb
OFFHOST_BACKUP_HOOK=/usr/local/bin/acb-offhost-backup
```

The repository must never commit a real age recipient/private identity. Production supplies the recipient through host configuration.

- [ ] Write a test proving durable backup output contains `gateway-YYYYMMDDHHMMSS.db.age` and a manifest, but no plaintext `.db` file after success.
- [ ] Write a test proving the off-host hook receives the `.db.age` path and manifest path, never the temporary plaintext path.
- [ ] Write a test proving missing `age`, missing recipient, failed encryption, failed integrity check, or failed required off-host hook aborts a schema deployment.
- [ ] Write a test proving `app_master_key` is not used as an encryption password and is not copied into the routine backup directory.
- [ ] In `backup-db.sh`, create a mode-0600 temporary online SQLite backup using dbtool, run integrity/schema checks, encrypt it with `age -r "$BACKUP_AGE_RECIPIENT"`, compute SHA-256/size, atomically place the encrypted artifact and manifest, then remove the plaintext staging file.
- [ ] Keep the staging directory mode 0700 and `umask 077`.
- [ ] Implement `backup-secrets.sh` as a separate explicit recovery operation that streams a tar archive of required secrets directly into `age`; no plaintext secrets directory is created in `BACKUP_DIR`.
- [ ] Implement `restore-db.sh` to require an explicit recovery identity path, decrypt to a new staging location, run `PRAGMA integrity_check`, verify manifest SHA-256, and only then offer the restored DB path to the operator. It must not overwrite the live DB automatically.
- [ ] Remove the old AES-256-CBC/PBKDF2 backup block and plaintext secret-copy block from `create_preflight_backup`.
- [ ] Convert `create_preflight_backup` into a wrapper around the new script or remove it after callers migrate.
- [ ] Document that the age private recovery identity is stored off-VPS.
- [ ] Run `bash deploy/tests/test_backup.sh` and ShellCheck.

**Commit:** `fix(backup): encrypt recovery artifacts independently with age`

### Task 23: Add an executable restore drill and evidence record

**Files:**
- Create: `scripts/ops/restore-drill.sh`
- Create: `docs/runbooks/RESTORE_RUNBOOK.md`
- Create: `docs/runbooks/BACKUP_RUNBOOK.md`
- Create: `deploy/tests/test_restore_drill.sh`

- [ ] Create a test fixture database, back it up/encrypt it, decrypt in an isolated temp environment using a test-only age identity, and prove integrity/schema/data counts match.
- [ ] Add a test encrypted secret bundle and prove it restores only into an isolated 0700 directory.
- [ ] Make `restore-drill.sh` refuse the live production data volume and require an explicit drill working directory.
- [ ] Record drill evidence as JSON containing artifact hash, schema version, integrity result, test timestamp, and test release commit without secret values.
- [ ] Document the exact disaster-recovery order: recover secrets, decrypt DB, integrity check, create fresh volumes, restore DB, install production env/release state, start worker/core, start gateway, verify Access/Traefik, verify ACB session or perform interactive re-auth.
- [ ] Run `bash deploy/tests/test_restore_drill.sh`.

**Commit:** `test(dr): add repeatable encrypted backup restore drill`

---

## Phase 6 — Make Production Compose Immutable and Deterministic

### Task 24: Remove all mutable production image fallbacks and introduce `.release.env`

**Files:**
- Modify: `deploy/compose.prod.yaml`
- Create: `deploy/release-env.sh`
- Create: `deploy/tests/test_compose_policy.sh`
- Modify: `deploy/README.md`

**Canonical release state:**

`/opt/acb-transaction-webhook/deploy/.release.env`

It contains non-secret immutable image refs and active slot release identity, for example:

```text
IMAGE_REF_BLUE=ghcr.io/thedemontuan/acb-transaction-webhook@sha256:$BLUE_DIGEST
IMAGE_REF_GREEN=ghcr.io/thedemontuan/acb-transaction-webhook@sha256:$GREEN_DIGEST
WORKER_IMAGE_REF=ghcr.io/thedemontuan/acb-transaction-webhook-worker@sha256:$WORKER_DIGEST
DBTOOL_IMAGE_REF=ghcr.io/thedemontuan/acb-transaction-webhook-dbtool@sha256:$DBTOOL_DIGEST
BROWSER_IMAGE_REF=ghcr.io/thedemontuan/acb-transaction-webhook-auth-browser@sha256:$BROWSER_DIGEST
TTS_IMAGE_REF=ghcr.io/thedemontuan/acb-transaction-webhook-tts-gateway@sha256:$TTS_DIGEST
BARK_IMAGE_REF=ghcr.io/finb/bark-server@sha256:$BARK_DIGEST
```

The shell variable names are literal; the digest values are populated by signed release state at runtime.

- [ ] Add a policy test that fails if `deploy/compose.prod.yaml` contains `:latest` or `:-ghcr.io` image fallbacks.
- [ ] Change image expressions to required variables such as `${WORKER_IMAGE_REF:?WORKER_IMAGE_REF is required}`.
- [ ] Make both slot refs explicit; do not allow a generic `IMAGE_REF` fallback to silently make both slots identical.
- [ ] Implement atomic read/write helpers for `.release.env`, validating every ref against the immutable sha256 pattern before replacement.
- [ ] Store previous refs before a component promotion so rollback can restore the exact digest.
- [ ] Render Compose in CI with a synthetic immutable `.release.env` and prove `docker compose config` succeeds.
- [ ] Prove missing any required image ref fails Compose validation before container mutation.
- [ ] Run `bash deploy/tests/test_compose_policy.sh`.

**Commit:** `fix(deploy): require immutable release state in Compose`

### Task 25: Establish one canonical production env path and remove configuration ambiguity

**Files:**
- Modify: `deploy/lib.sh`
- Modify: `deploy/deploy.sh`
- Modify: `.github/workflows/deploy.yml`
- Modify: `README.md`
- Modify: `deploy/README.md`
- Modify: `.env.example`
- Modify: `deploy/tests/test_deploy.sh`

**Canonical file:** `/opt/acb-transaction-webhook/deploy/.env.production`

- [ ] Add a test proving deployment aborts if the canonical env file is missing.
- [ ] Add a test proving no root-level `.env.production` is read or rewritten by deployment scripts.
- [ ] Change CI remote logic to stop mutating `$DEPLOY_PATH/.env.production`.
- [ ] Treat `deploy/.env.production` as host-managed runtime configuration. Release-specific image refs live only in `.release.env`.
- [ ] Make every `docker compose` invocation consistently pass both `--env-file deploy/.env.production` and `--env-file deploy/.release.env` where interpolation needs both, or centralize the exact invocation in one helper.
- [ ] Keep service `env_file: .env.production` relative to the Compose file or explicitly remove it only after equivalent environment injection is proven.
- [ ] Update README bootstrap instructions to create exactly this file.
- [ ] Run deployment tests and a real `docker compose config --quiet` with sanitized fixtures.

**Commit:** `fix(config): use one canonical production env file`

### Task 26: Verify resource limits, networks, and filesystem hardening under real Compose semantics

**Files:**
- Modify: `deploy/compose.prod.yaml`
- Create: `deploy/verify-compose-runtime.sh`
- Create: `deploy/tests/test_runtime_policy.sh`
- Modify: `deploy/check-host.sh` if present

- [ ] Add policy assertions for `read_only`, non-root first-party user, `cap_drop: ALL`, `no-new-privileges`, PID/memory/CPU limits, log rotation, no host `ports`, and allowed networks per service.
- [ ] On the target Docker Compose version, verify the chosen CPU/memory/PID syntax is actually represented in `docker inspect HostConfig`. If `deploy.resources.limits` alone is not enforced by the installed local Compose path, add service-level `cpus`, `mem_limit`, and `pids_limit` fields.
- [ ] Preserve auth-browser writable requirements only where Chromium needs them; keep `/tmp` and profile writable space bounded via tmpfs.
- [ ] Verify `edge-acb` and `acb-core` are internal networks as intended by platform policy and `acb-egress` is the explicit outbound-capable network.
- [ ] Ensure worker does not join `edge-acb`.
- [ ] Ensure dbtool does not join egress/core/edge.
- [ ] Verify no service mounts Docker socket or host root.
- [ ] Add runtime verification to host preflight.
- [ ] Run `bash deploy/tests/test_runtime_policy.sh` and ShellCheck.

**Commit:** `hardening(compose): verify enforceable runtime isolation limits`

---

## Phase 7 — Split Deployment Into Explicit Transactions

### Task 27: Create a shared deploy transaction library with fail-closed preflight

**Files:**
- Refactor: `deploy/lib.sh`
- Create: `deploy/lib/common.sh`
- Create: `deploy/lib/images.sh`
- Create: `deploy/lib/state.sh`
- Create: `deploy/lib/traefik.sh`
- Create: `deploy/lib/database.sh`
- Modify: deployment tests

- [ ] Keep `deploy/lib.sh` as a compatibility shim during the migration, sourcing the split libraries.
- [ ] Centralize immutable-ref validation, deploy lock acquisition, release-state read/write, active-auth gate, Compose invocation, container image verification, and log helpers.
- [ ] Make `check_active_auth_gate` return failure if dbtool fails, JSON cannot be parsed, SQLite fallback fails, the table cannot be read, or no supported check method exists.
- [ ] Add tests for every active-auth fail-closed case, including a simulated dbtool exit code 1 that previously became zero active sessions.
- [ ] Remove shell fallbacks that convert safety-check failure into a safe-looking value.
- [ ] Keep per-app deploy lock compatible with `/run/lock/vps-failover/acb.lock` so deploy and host failover cannot switch the same app simultaneously.
- [ ] Run `bash deploy/tests/test_deploy.sh` and ShellCheck.

**Commit:** `refactor(deploy): centralize fail-closed transaction primitives`

### Task 28: Implement gateway-only Blue/Green promotion

**Files:**
- Create: `deploy/deploy-gateway.sh`
- Modify: `deploy/deploy-warm.sh` to delegate or become compatibility wrapper
- Modify: `deploy/verify-deployment.sh`
- Create: `deploy/tests/test_gateway_deploy.sh`
- Modify: `cmd/gateway/main.go` if release/slot response headers are missing
- Modify: `internal/httpapi/server.go`

**Required sequence:**

```text
lock
-> verify signed promotion scope includes gateway
-> verify immutable candidate digest
-> read active slot/release state
-> pull candidate
-> start inactive slot with candidate ref
-> liveness
-> /internal/deployz twice consecutively
-> route switch
-> real edge identity ACK
-> soak
-> intentional stop old slot
-> atomically update .release.env and active-slot state
```

- [ ] Add test snapshots of worker/auth-browser/TTS/Bark container IDs before and after a gateway-only deploy and assert exact equality.
- [ ] Add a test proving gateway-only deploy never calls migration/backup helpers.
- [ ] Add a candidate failure test proving the active slot and route remain untouched.
- [ ] Require candidate `/internal/deployz` to report expected slot, release commit, `storage=ready`, `schema=compatible`, and `worker=ready` twice consecutively with a short gap.
- [ ] Ensure `/internal/deployz` uses an internal token and cannot be reached through the public Traefik route.
- [ ] Add `X-Platform-Slot` and `X-Release-Commit` response headers on ordinary health/identity-safe edge responses used by ACK.
- [ ] Preserve old slot through soak and record intentional stop/cooldown before stopping it.
- [ ] Do not alter worker/core image refs in `.release.env` during gateway-only promotion.
- [ ] Run `bash deploy/tests/test_gateway_deploy.sh`.

**Commit:** `feat(deploy): isolate gateway Blue Green promotion from ACB worker`

### Task 29: Implement schema promotion as a separate transaction

**Files:**
- Create: `deploy/deploy-schema.sh`
- Modify: `deploy/backup-db.sh`
- Modify: `deploy/tests/test_schema_deploy.sh`
- Modify: `cmd/dbtool/main.go` if a compatibility command is needed

- [ ] Add a test proving active auth blocks schema promotion.
- [ ] Add a test proving inability to determine active-auth state blocks schema promotion.
- [ ] Add a test proving backup/integrity/encryption failure prevents migration.
- [ ] Add a test proving migration failure does not automatically overwrite the live DB from backup.
- [ ] Run encrypted backup only when promotion scope includes `schema`.
- [ ] Run dbtool migration by immutable digest with no egress network.
- [ ] Run schema compatibility verification after migration and before any runtime component promotion that needs the new schema.
- [ ] Persist a migration result record containing previous/new schema version and backup artifact ID.
- [ ] Never change the Traefik route from this script.
- [ ] Run `bash deploy/tests/test_schema_deploy.sh`.

**Commit:** `feat(deploy): isolate schema migration transaction`

### Task 30: Implement controlled singleton worker promotion and rollback

**Files:**
- Create: `deploy/deploy-worker.sh`
- Create: `deploy/tests/test_worker_deploy.sh`
- Modify: `deploy/compose.prod.yaml`
- Modify: worker drain/readiness implementation from Task 18

**Required sequence:**

```text
lock
-> verify signed scope includes worker
-> fail-closed active-auth gate
-> verify candidate + previous immutable refs
-> ask old worker to drain
-> wait bounded drain
-> stop old worker
-> start candidate worker
-> require liveness + readiness
-> require singleton lock ownership evidence
-> verify scheduler/session state is initialized
-> commit candidate ref
```

- [ ] Add test proving only one worker can acquire the singleton lock.
- [ ] Add test proving candidate readiness timeout is fatal; remove the current warn-and-continue behavior.
- [ ] Add test proving failed candidate causes the candidate to stop and previous worker digest to restart.
- [ ] Add test proving rollback worker becomes ready before script returns failure.
- [ ] Add test proving an active auth attempt prevents worker replacement unless a future explicitly documented migration requires it.
- [ ] Track the worker's previous digest in release state before mutation.
- [ ] Do not restart gateway slots as a normal consequence of worker promotion if the worker RPC compatibility version is unchanged.
- [ ] For the history-RPC compatibility change in this plan, coordinate gateway/worker promotion through the signed compatibility manifest and a tested order that prevents incompatible traffic.
- [ ] Run `bash deploy/tests/test_worker_deploy.sh`.

**Commit:** `feat(deploy): add session-preserving singleton worker promotion`

### Task 31: Implement independent auth-browser, TTS, and Bark promotions

**Files:**
- Create: `deploy/deploy-auth-browser.sh`
- Create: `deploy/deploy-tts.sh`
- Create: `deploy/deploy-bark.sh`
- Create: `deploy/tests/test_aux_deploy.sh`

- [ ] Auth-browser deploy: fail-closed active-auth check, immutable image verification, replace only auth-browser, require its health, rollback previous digest on failure.
- [ ] TTS deploy: replace only TTS, require health/smoke synthesis, rollback on failure, never restart worker/gateway.
- [ ] Bark deploy: validate approved third-party immutable digest, preserve Bark data volume, require local authenticated health/smoke, rollback previous digest on failure.
- [ ] Add tests proving each script leaves unrelated container IDs unchanged.
- [ ] Add test proving auth-browser upgrade aborts when an attempt is active.
- [ ] Add test proving Bark/TTS failure cannot stop ACB polling worker.
- [ ] Run `bash deploy/tests/test_aux_deploy.sh`.

**Commit:** `feat(deploy): separate browser TTS and Bark lifecycles`

---

## Phase 8 — Make Traefik Promotion Positively Verifiable

### Task 32: Replace file-content route acknowledgement with actual edge identity acknowledgement

**Files:**
- Modify: `deploy/lib/traefik.sh`
- Modify: `deploy/deploy-gateway.sh`
- Create: `deploy/render-traefik-route.sh`
- Create: `deploy/tests/test_traefik_switch.sh`
- Modify: `platform/failover/apps.d/acb.json` only if switch command changes
- Modify: platform switch helper under `scripts/ops` or `/opt/platform/bin` source file in repository

- [ ] Render the candidate Traefik YAML outside the watched `/opt/edge/dynamic` directory.
- [ ] Parse/validate YAML before installation using an already pinned/available parser; do not rely solely on grep.
- [ ] Keep a byte-for-byte previous route copy before atomic replacement.
- [ ] Atomically move the candidate route into `/opt/edge/dynamic/acb.yml`.
- [ ] Define one required `ROUTE_ACK_URL` that actually traverses the shared Traefik route. It may be an internal Cloudflare-bypassing edge probe exposed only on loopback/private platform network, or the protected public host with the necessary Access service credential; choose one deployment-wide method and document it.
- [ ] Verify candidate `X-Platform-Slot` and `X-Release-Commit` headers through that route.
- [ ] Remove the current behavior where absent `ROUTE_ACK_URL` can degrade ACK to file/container checks.
- [ ] On ACK failure, restore previous route atomically and require old slot/release identity through the same edge probe.
- [ ] Add tests for invalid rendered YAML, Traefik not consuming the new route, wrong slot header, wrong release header, rollback ACK failure, and successful switch.
- [ ] Run `bash deploy/tests/test_traefik_switch.sh`.

**Commit:** `fix(edge): require real Traefik route identity acknowledgement`

### Task 33: Make hostname and edge dependencies explicit rather than hard-coded

**Files:**
- Modify: `deploy/lib/traefik.sh`
- Modify: `deploy/render-traefik-route.sh`
- Modify: `.env.example`
- Modify: `deploy/README.md`
- Modify: `deploy/check-host.sh`

- [ ] Derive the route host from validated `PUBLIC_ORIGIN` or a canonical `PUBLIC_HOST`; reject scheme/path/query where a hostname is expected.
- [ ] Remove hard-coded `bank.tuannguyenviet.site` from generated route logic while allowing the current production value through host config.
- [ ] Preflight required shared Traefik entrypoints/middlewares/network assumptions and abort if the platform contract is missing.
- [ ] Verify the ACB deployment never restarts shared Traefik/cloudflared.
- [ ] Document ownership boundary: app may write only `/opt/edge/dynamic/acb.yml` and its own backup/staging files.
- [ ] Add host-preflight tests with mocked missing middleware/network/entrypoint metadata.

**Commit:** `refactor(edge): make ACB Traefik route platform-driven`

---

## Phase 9 — Align Host Failover With Deployment State

### Task 34: Extend failover tests around warm standby and intentional stops

**Files:**
- Modify: `platform/failover/vps-failover-controller.py`
- Modify: `platform/failover/test_failover.py`
- Modify: `platform/failover/apps.d/acb.json`
- Modify: `platform/failover/README.md`

- [ ] Add test proving an intentionally stopped old slot after soak is not immediately restarted by reconcile.
- [ ] Add test proving an unhealthy active gateway can start a healthy standby and switch only after candidate identity/health verification.
- [ ] Add test proving deploy/failover lock contention causes one actor to wait/fail safely rather than racing the route file.
- [ ] Add test proving corrupt controller state does not arbitrarily switch traffic; recovered degraded state requires current route/container observation before mutation.
- [ ] Keep gateway auto-failover bounded by cooldown/max restart policy.
- [ ] Ensure switch command goes through the same hardened route switch/ACK primitive used by deployment, not a weaker alternate implementation.
- [ ] Run `python3 -m unittest platform/failover/test_failover.py` or the repository's established invocation and `python3 -m py_compile platform/failover/vps-failover-controller.py`.

**Commit:** `test(failover): align warm standby recovery with deploy state`

### Task 35: Give singleton workloads explicit recovery policy instead of generic restarts

**Files:**
- Modify: `platform/failover/vps-failover-controller.py`
- Modify: `platform/failover/test_failover.py`
- Create/Modify: `platform/failover/apps.d/worker.json`
- Modify: `platform/failover/apps.d/auth-browser.json`

- [ ] Register `acb-worker` as a singleton with a worker-specific health command/readiness policy if automatic recovery is enabled.
- [ ] On worker liveness failure, allow bounded container restart only when no deployment lease is active; singleton file lock remains final fencing.
- [ ] Do not repeatedly restart a worker that is live but not ready because ACB is auth-required/maintenance unless the worker readiness contract itself says local initialization failed.
- [ ] Keep auth-browser recovery conservative; a crash may restart the service, but deployment never replaces it during active auth.
- [ ] Add restart storm/cooldown tests.
- [ ] Add operator-visible degraded reason/state for singleton recovery exhaustion.

**Commit:** `feat(failover): add state-aware singleton recovery policies`

---

## Phase 10 — Harden Supply Chain and Make Promotion Component-Aware

### Task 36: Pin all GitHub Actions by commit SHA and remove unchecked binary fallback

**Files:**
- Modify: `.github/workflows/deploy.yml`
- Modify: `.github/workflows/codacy.yml` if third-party actions are present
- Modify: `.github/dependabot.yml`
- Create/Modify: `scripts/verify-actions-pinned.sh`
- Modify: CI verify job

- [ ] Extend `verify-actions-pinned.sh` to fail any `uses:` entry that is not a local action and is not pinned to a 40-character commit SHA.
- [ ] Replace tag references for checkout, setup-bun, setup-go, setup-python, Docker buildx/login/build-push and any other third-party action with verified full SHAs plus human-readable version comments.
- [ ] Keep Dependabot configured for GitHub Actions so pinned commits receive update PRs.
- [ ] Prefer making the pinned Cosign installer a hard failure with normal GitHub retry/rerun rather than downloading an alternate executable.
- [ ] If fallback download is retained, download the matching pinned release checksum file and verify SHA-256 before `chmod`/execution, mirroring the existing Trivy checksum pattern.
- [ ] Add a negative test fixture showing an unpinned action or mismatched checksum fails CI verification.
- [ ] Run `bash scripts/verify-actions-pinned.sh`.

**Commit:** `hardening(ci): pin actions and verify executable fallbacks`

### Task 37: Make VPS signature verification mandatory and signer-specific

**Files:**
- Modify: `deploy/verify-manifest.sh`
- Modify: `deploy/test-supply-chain.sh`
- Modify: `.github/workflows/deploy.yml`
- Modify: `deploy/check-host.sh`

- [ ] Add tests proving missing Cosign on the VPS causes verification failure.
- [ ] Add tests proving a valid bundle signed by the wrong certificate identity fails.
- [ ] Add tests proving a wildcard expected identity is rejected in production mode.
- [ ] Make `--require-cosign` the normal production path and have the workflow pass the exact expected identity derived from repository/workflow/ref.
- [ ] Preinstall a pinned/verified Cosign binary in host bootstrap and make `check-host.sh` verify its availability/version before promotion.
- [ ] Verify signed manifest before any runtime config, release state, Compose file, route, database, or container mutation.
- [ ] Keep manifest artifact hashes and immutable image refs binding the exact deployment bundle.
- [ ] Optionally verify individual first-party image signatures on VPS as defense in depth; signed manifest verification remains mandatory regardless.
- [ ] Run `bash deploy/test-supply-chain.sh`.

**Commit:** `fix(supply-chain): fail closed on VPS manifest signature verification`

### Task 38: Introduce signed component promotion scope

**Files:**
- Create: `scripts/compute-promotion-scope.sh`
- Create: `scripts/test-promotion-scope.sh`
- Modify: `deploy/generate-release-manifest.sh`
- Modify: `deploy/verify-manifest.sh`
- Modify: `.github/workflows/deploy.yml`

**Manifest section:**

```json
{
  "promotion": {
    "gateway": true,
    "worker": false,
    "schema": false,
    "auth_browser": false,
    "tts": false,
    "bark": false,
    "platform": false
  }
}
```

- [ ] Build table-driven shell tests for path classifications.
- [ ] Classify `web/**`, `cmd/gateway/**`, gateway-only HTTP/UI packages as gateway.
- [ ] Classify `internal/monitor/**`, `internal/acb/**`, `cmd/worker/**` as worker.
- [ ] Classify `internal/workerrpc/**` as gateway + worker.
- [ ] Classify `internal/storage/migrations/**` as schema + gateway + worker + dbtool.
- [ ] Conservatively classify shared storage/config/security packages according to the binaries that import them; false-positive building/promoting is acceptable, false-negative incompatible promotion is not.
- [ ] Classify auth-browser source/Dockerfile, TTS source, Bark policy, and platform/deploy paths explicitly.
- [ ] For ambiguous shared changes, set the broader safe scope rather than guessing narrow.
- [ ] Embed the computed scope into the signed release manifest.
- [ ] Make the VPS dispatcher refuse a component promotion not authorized by the signed scope.
- [ ] Make a zero-code documentation-only change produce no runtime component promotion.
- [ ] Run `bash scripts/test-promotion-scope.sh`.

**Commit:** `feat(ci): sign component promotion scope into release manifest`

### Task 39: Stop automatically upgrading core on every push

**Files:**
- Rewrite deployment decision portion of `.github/workflows/deploy.yml`
- Create: `deploy/promote-release.sh`
- Create: `deploy/tests/test_promote_release.sh`

- [ ] Add a regression test that would fail if an automatic gateway-only release invokes worker/auth-browser/TTS/Bark deploy scripts.
- [ ] Remove unconditional `export UPGRADE_CORE=1` and unconditional `--upgrade-core` from the main deployment workflow.
- [ ] `promote-release.sh` reads the already verified signed promotion scope and dispatches schema first when needed, then compatible worker/gateway/core components in the tested order.
- [ ] For gateway-only scope, call only `deploy-gateway.sh`.
- [ ] For TTS-only scope, call only `deploy-tts.sh`.
- [ ] For auth-browser-only scope, call only `deploy-auth-browser.sh` after active-auth gate.
- [ ] For worker + gateway RPC compatibility scope, apply the specific coordinated sequence encoded in compatibility metadata.
- [ ] Preserve `environment: production` and serialized concurrency with `cancel-in-progress: false`.
- [ ] Keep workflow_dispatch as an operator override only within signed/verified artifact constraints; it must not permit an arbitrary mutable tag.
- [ ] Run `bash deploy/tests/test_promote_release.sh`.

**Commit:** `fix(ci): promote only components authorized by signed release scope`

### Task 40: Tighten vulnerability policy for first-party sidecars and third-party Bark

**Files:**
- Modify: `.github/workflows/deploy.yml`
- Modify: `deploy/cve-allowlist.json`
- Modify: `deploy/validate-cve-allowlist.sh`
- Modify: `deploy/third-party-allowlist.json`
- Modify: `deploy/verify-third-party-policy.sh`

- [ ] Keep gateway/worker/dbtool HIGH/CRITICAL release-blocking except explicit time-boxed allowlist entries.
- [ ] Make auth-browser/TTS CRITICAL release-blocking and HIGH release-blocking unless a documented allowlist entry has CVE ID, reason, owner, and future expiry.
- [ ] Keep Bark CRITICAL release-blocking; require approved immutable digest and record HIGH findings with the same expiry discipline.
- [ ] Make expired exceptions fail CI automatically.
- [ ] Upload Trivy JSON + CycloneDX SBOMs for all promoted images.
- [ ] Run supply-chain test script locally with fixtures for valid/expired exceptions.

**Commit:** `hardening(ci): enforce expiring CVE policy across all images`

---

## Phase 11 — Align Health, Release Identity, and Observability

### Task 41: Finalize three-level health semantics

**Files:**
- Modify: `internal/httpapi/server.go`
- Modify: `internal/httpapi/deployz_test.go`
- Modify: `internal/workerrpc/rpc.go`
- Modify: `internal/workerrpc/rpc_test.go`
- Modify: `cmd/gateway/main.go`
- Modify: `cmd/worker/main.go`

- [ ] Keep gateway `/healthz` process-only and database-independent where possible; a transient dependency outage must not create a restart loop.
- [ ] Keep gateway `/readyz` focused on local ability to serve HTTP/storage.
- [ ] Keep `/internal/deployz` as the dependency-aware promotion gate.
- [ ] Worker `/healthz` remains process liveness; `/readyz` proves singleton lock, schema compatibility, initialized scheduler/dispatcher, and not draining.
- [ ] Do not make worker readiness depend on a successful ACB poll, authenticated bank state, or current transactions.
- [ ] Include release commit, slot, runtime role, schema version, worker RPC compatibility version, and bounded dependency statuses in deployz.
- [ ] Add tests for ACB maintenance/auth-required where health remains live.
- [ ] Add tests for draining worker where liveness stays 200 but readiness becomes 503.
- [ ] Run `go test -race ./internal/httpapi ./internal/workerrpc ./cmd/gateway ./cmd/worker`.

**Commit:** `refactor(health): separate liveness readiness and promotion gates`

### Task 42: Add scheduler/job/deploy observability without leaking secrets

**Files:**
- Modify: `internal/telemetry/*`
- Modify: `internal/monitor/upstream_scheduler.go`
- Modify: `internal/monitor/history_job_runner.go`
- Modify: `internal/httpapi/server.go`
- Modify: frontend status/admin components if a dashboard view is desired
- Create: `docs/runbooks/OBSERVABILITY.md`

- [ ] Add telemetry for queue depth per priority, current scheduler task, last realtime poll time/duration/status, catch-up day, history job counts/status/oldest queue age, notification backlog, worker drain state, active slot/release, and last promotion result.
- [ ] Add tests that telemetry snapshots do not include cookie values, passwords, Cloudflare JWTs, internal tokens, Bark credentials, decrypted session envelopes, or raw ACB HTML.
- [ ] Keep high-cardinality job IDs out of global metrics; job IDs may appear in structured logs/audit where appropriate.
- [ ] Add operator status endpoint fields using customer-friendly values while retaining technical structured logs server-side.
- [ ] Document alert thresholds suitable for one VPS: stale realtime poll, repeated auth-required, scheduler backlog, history job age, dead-letter deliveries, failed backup, failed route promotion, worker restart exhaustion.
- [ ] Run telemetry and HTTP API tests.

**Commit:** `feat(ops): expose bounded scheduler and deployment observability`

---

## Phase 12 — Production Failure Drills and Regression Gates

### Task 43: Add the realtime-vs-history concurrency regression suite

**Files:**
- Create: `internal/monitor/realtime_history_integration_test.go`
- Extend: `internal/monitor/history_job_runner_test.go`
- Extend: `internal/monitor/upstream_scheduler_test.go`

- [ ] Simulate a 31-day history job with at least three mocked pages per day.
- [ ] Use a fake ACB page request duration, enqueue realtime after a low-priority page begins, and prove realtime begins before history page two after the current page yields.
- [ ] Assert history job still eventually completes with exact rows/pages and no duplicate transactions.
- [ ] Add a catch-up version of the same preemption test.
- [ ] Add rate-limit/maintenance backoff while realtime/manual priorities remain schedulable according to circuit policy.
- [ ] Run with race detector at least 20 repeated iterations to expose scheduling races.

**Commit:** `test(realtime): prove history and catch-up cannot starve polling`

### Task 44: Add session/crash/restart recovery drills

**Files:**
- Create: `internal/monitor/recovery_integration_test.go`
- Create/Modify: `deploy/tests/test_worker_recovery.sh`
- Modify: worker/store tests

- [ ] Simulate worker cancellation during a running history job and prove restart requeues stale work.
- [ ] Simulate cancellation after ACB response but before final job-state update and prove durable dedupe/recovery reaches one correct terminal state.
- [ ] Simulate worker promotion failure and prove previous digest is restarted and ready.
- [ ] Simulate stale generation handoff and DB lookup failure; prove zero ACB verification requests.
- [ ] Simulate active-auth lookup DB failure; prove zero ACB poll requests.
- [ ] Simulate worker crash after cookie refresh and prove the last successfully persisted session is restored; document that a crash during an unpersisted in-flight request may require the next bootstrap/re-auth depending on ACB behavior.

**Commit:** `test(worker): cover session and durable job crash recovery`

### Task 45: Add gateway release and Traefik rollback drills

**Files:**
- Extend: `deploy/tests/test_gateway_deploy.sh`
- Extend: `deploy/tests/test_traefik_switch.sh`
- Extend: `platform/failover/test_failover.py`

- [ ] Broken candidate image/health -> active route unchanged.
- [ ] Candidate deployz dependency failure -> active route unchanged.
- [ ] Invalid route YAML -> active route unchanged.
- [ ] Traefik ignores/rejects candidate route -> ACK failure -> old route restored.
- [ ] Candidate returns wrong slot/release header -> rollback.
- [ ] Candidate crashes during soak -> failover/soak restores old route.
- [ ] Browser SSE reconnect after route switch -> durable journal replay fills the gap.
- [ ] Assert worker container ID never changes in every gateway release drill.

**Commit:** `test(deploy): cover gateway promotion and Traefik rollback failures`

### Task 46: Add secret, backup, and supply-chain failure drills

**Files:**
- Extend: `deploy/tests/test_secrets.sh`
- Extend: `deploy/tests/test_backup.sh`
- Extend: `deploy/test-supply-chain.sh`
- Extend: `scripts/verify-actions-pinned.sh` tests

- [ ] Missing app master key -> deployment fails and file remains absent.
- [ ] Missing worker/TTS/Bark secret -> affected promotion fails and does not generate replacements.
- [ ] Backup encryption failure -> schema promotion aborts before migration.
- [ ] Durable backup directory plaintext scan -> zero unencrypted `.db` files.
- [ ] Secret backup scan -> zero plaintext copied secret files.
- [ ] Tampered encrypted backup/manifest -> restore drill fails.
- [ ] Missing Cosign on host -> manifest verify fails.
- [ ] Wrong signer identity -> manifest verify fails.
- [ ] Tampered Cosign fallback checksum -> CI install step fails.
- [ ] Mutable/missing image ref -> Compose/promotion fails before pull/up.

**Commit:** `test(security): add fail-closed production drills`

---

## Phase 13 — Clean Up Compatibility Paths After One Successful Production Cycle

### Task 47: Retire obsolete deployment paths only after proven Blue -> Green -> Blue cycles

**Files:**
- Remove or reduce to wrappers after acceptance: `deploy/deploy-warm.sh`
- Remove or reduce to wrappers after acceptance: legacy rollback scripts superseded by component scripts
- Modify: `deploy/README.md`
- Modify: `README.md`
- Modify: `.github/workflows/deploy.yml`

- [ ] Complete one successful gateway Blue -> Green production promotion with soak and recorded edge ACK.
- [ ] Complete one later Green -> Blue promotion and recorded edge ACK.
- [ ] Complete one worker promotion and rollback drill.
- [ ] Only then remove unreachable legacy code that can trigger all-in-one core deploy behavior.
- [ ] Leave compatibility wrappers that print the replacement command and fail safely if external automation still calls them; remove those wrappers in a later clean release after caller inventory is complete.
- [ ] Remove docs that instruct operators to use the old all-in-one path.
- [ ] Search repository for `UPGRADE_CORE`, `--upgrade-core`, legacy Caddy route references, and old root `.env.production` paths; every remaining occurrence must be historical documentation explicitly marked superseded or a test fixture.

**Commit:** `chore(deploy): retire superseded all-in-one deployment path`

### Task 48: Move historical fix documents under an archive directory

**Files:**
- Create: `docs/archive/README.md`
- Move: `2026-09-12-caddy-progressive-blue-green-deployment.md`
- Move: `2026-09-13-acb-secure-single-vps-platform-migration.md`
- Move: `2026-09-13-single-vps-secure-container-platform-standard.md`
- Move: `fix.md`
- Move: `fix_2.md`
- Update any links

- [ ] Move superseded documents only after the final architecture is implemented so history remains easy to consult during refactor.
- [ ] `docs/archive/README.md` explains dates, why each document was superseded, and points to the final invariant spec.
- [ ] Run repository link/search checks and `git diff --check`.

**Commit:** `docs: archive superseded ACB deployment plans`

---

## Phase 14 — Final Verification Before Declaring the Refactor Complete

### Task 49: Run the complete local/CI verification matrix

- [ ] `go test -race ./...`
- [ ] `go vet ./...`
- [ ] `cd web && bun install --frozen-lockfile`
- [ ] `cd web && bunx --bun tsc --noEmit`
- [ ] `cd web && bunx --bun vite build`
- [ ] `cd web && bunx --bun vitest run`
- [ ] Run the repository Playwright E2E suite including asynchronous history sync.
- [ ] `cd tts-gateway && pytest -v`
- [ ] `bash -n deploy/*.sh deploy/lib/*.sh scripts/ops/*.sh scripts/*.sh`
- [ ] `shellcheck --severity=error deploy/*.sh deploy/lib/*.sh deploy/tests/*.sh scripts/ops/*.sh scripts/*.sh`
- [ ] `bash deploy/tests/test_secrets.sh`
- [ ] `bash deploy/tests/test_backup.sh`
- [ ] `bash deploy/tests/test_restore_drill.sh`
- [ ] `bash deploy/tests/test_compose_policy.sh`
- [ ] `bash deploy/tests/test_runtime_policy.sh`
- [ ] `bash deploy/tests/test_gateway_deploy.sh`
- [ ] `bash deploy/tests/test_schema_deploy.sh`
- [ ] `bash deploy/tests/test_worker_deploy.sh`
- [ ] `bash deploy/tests/test_aux_deploy.sh`
- [ ] `bash deploy/tests/test_traefik_switch.sh`
- [ ] `bash deploy/tests/test_promote_release.sh`
- [ ] `bash deploy/test-supply-chain.sh`
- [ ] `bash scripts/test-promotion-scope.sh`
- [ ] `bash scripts/verify-actions-pinned.sh`
- [ ] Run failover controller unit tests and Python compile check.
- [ ] `git diff --check`
- [ ] Confirm CI scan/sign job succeeds and produces SBOM/security/release manifest artifacts.

**Commit:** no new code should be introduced solely to make this checklist pass without adding the corresponding regression test at the failing layer.

### Task 50: Run the production acceptance drill in a controlled release window

**Preconditions:** encrypted off-host backup completed; restore drill completed; previous immutable release refs recorded; Cloudflare/Traefik access available; no active auth attempt.

- [ ] Record `docker inspect` IDs for worker/auth-browser/TTS/Bark and active gateway.
- [ ] Perform a gateway-only UI release. Verify worker/auth-browser/TTS/Bark IDs are unchanged.
- [ ] Verify the shared Traefik route ACK reports the intended slot and release commit.
- [ ] During gateway soak, generate/read a journal event and reconnect SSE across slots; verify no gap.
- [ ] Start a long mocked/staging history job where available and verify realtime scheduler preemption metrics/logs.
- [ ] In production without synthetic bank mutation, observe that normal realtime poll cadence does not develop a deployment-caused gap during gateway promotion.
- [ ] Perform an auth-browser deploy only when no active attempt; then perform a real login/handoff and confirm session is preserved by the worker.
- [ ] Perform a controlled worker promotion. Verify old worker drains, candidate becomes ready, singleton lock never overlaps, session is restored, and polling resumes.
- [ ] Trigger/execute one rollback drill using a deliberately failing non-production candidate or staging-equivalent environment; do not intentionally deploy a malicious/broken artifact to live banking traffic.
- [ ] Verify backup manifest and off-host encrypted artifact for the most recent schema change.
- [ ] Record acceptance evidence in `docs/operations/production-acceptance-2026-09.md` with release SHAs/digests and timestamps but no secrets.

### Task 51: Final invariant audit

- [ ] Search production Compose for `:latest`; result must be empty.
- [ ] Search deploy scripts for secret generation commands outside explicit provisioning scripts; result must be empty.
- [ ] Search deploy scripts for patterns that convert safety-check command failure into zero/false-safe values; review every match and eliminate fail-open behavior.
- [ ] Search gateway production path for construction/start of `monitor.Monitor` or notification dispatcher; production gateway must not start either.
- [ ] Search gateway for background journal retention/stale-auth ticker; singleton versions must exist only in worker maintenance runner.
- [ ] Search monitor code for a mutex held across multi-page/multi-day loops; ACB low-priority loops must use scheduler quanta instead.
- [ ] Search worker RPC for 120-second history request handling; synchronous history route must be absent.
- [ ] Search frontend mutations for manual CSRF logic that duplicates the central transport; keep only specialized exceptions with tests.
- [ ] Search backup directory generation logic for plaintext DB/secret persistence; none allowed.
- [ ] Search VPS manifest verification for wildcard signer identity or optional Cosign behavior; none allowed in production path.
- [ ] Search GitHub workflow for unconditional `UPGRADE_CORE=1`/`--upgrade-core`; none allowed.
- [ ] Search repository root for authoritative Caddy deployment instructions; active docs must name Traefik.
- [ ] Compare implemented behavior line-by-line against `docs/superpowers/specs/2026-09-14-acb-final-production-invariants.md` and document any intentional deviation before merging.

**Commit:** `chore(release): complete ACB production invariant audit`

---

## Dependency Order and Parallelization Rules

Do not implement these tasks in arbitrary order.

**Strict dependency chain:**

```text
Tasks 1-4
  -> Tasks 5-9 scheduler migration
  -> Tasks 10-15 async history
  -> Tasks 16-19 singleton ownership/shutdown
  -> Tasks 20-23 secret/backup safety
  -> Tasks 24-26 immutable Compose/config
  -> Tasks 27-33 deployment/Traefik
  -> Tasks 34-35 failover integration
  -> Tasks 36-40 CI/supply-chain/promotion scope
  -> Tasks 41-42 health/observability
  -> Tasks 43-46 failure drills
  -> Tasks 47-48 cleanup
  -> Tasks 49-51 final acceptance
```

Safe parallel work after the prerequisite interfaces are stable:

- frontend CSRF Task 3 can proceed alongside backend fail-closed Task 4;
- backup Tasks 20-23 can proceed alongside scheduler Tasks 5-9 if deploy files and Go monitor files are owned by separate workers;
- supply-chain Tasks 36-37 can proceed alongside Compose Tasks 24-26 after canonical image/env variable names are agreed;
- failover tests Task 34 may proceed while deployment scripts are being refactored only if the switch-command interface is frozen first.

Do not parallelize two workers editing `internal/monitor/monitor.go`, `deploy/lib.sh`, `deploy/compose.prod.yaml`, or `.github/workflows/deploy.yml` at the same time.

---

## Migration Strategy From Current Production

The final production transition must avoid combining every architectural change into one risky cutover.

### Release A — Safety guardrails with no scheduler behavior change

- runtime roles/fail-closed config;
- centralized frontend CSRF;
- VerifySession/active-auth DB fail-closed behavior;
- secret validation separation;
- signed manifest fail-closed verification;
- no unconditional core deploy;
- canonical env path.

Deploy this release using a carefully reviewed transitional path. Confirm worker remains singleton and session survives.

### Release B — Scheduler migration

- priority scheduler;
- realtime/manual/keepalive/verifier migration;
- preemptible catch-up;
- worker observability.

Keep old synchronous filter-history disabled during the exact cutover if necessary rather than allowing it to bypass the scheduler. Re-enable only through Release C async jobs.

### Release C — Async history jobs and RPC compatibility bump

- schema migration 8;
- durable history job runner;
- new worker RPC;
- new gateway API;
- new frontend progress UX.

Because gateway and worker RPC contract changes together, use the signed compatibility version and tested coordinated deployment order. Do not rely on old/new RPC interoperability that tests do not prove.

### Release D — Deployment/backup/Traefik transaction convergence

- immutable `.release.env`;
- age backup/restore;
- component deploy scripts;
- real route ACK;
- failover integration.

After Release D completes Blue -> Green and Green -> Blue drills, retire the legacy all-in-one deployment path.

---

## Rollback Principles

### Gateway rollback

Rollback is a route operation to the still-running previous slot during soak. After soak, the previous immutable digest in `.release.env` can be started in the inactive slot and promoted through the same health/deployz/ACK gates.

### Worker rollback

Worker rollback is a controlled singleton replacement to the previously recorded digest. Never start two workers simultaneously to achieve rollback.

### Schema rollback

Do not automatically restore an old SQLite backup over a live database. Schema migrations in this plan are additive and old/new runtime compatibility is tested before promotion. A destructive emergency restore is an explicit operator disaster-recovery action using the restore runbook.

### Secret rollback

Normal releases do not rotate secrets, so code rollback does not require secret rollback. Any future intentional key rotation must have its own migration/runbook and is outside this plan.

### Edge rollback

Restore the byte-for-byte previous ACB Traefik dynamic route atomically and prove the old slot/release through the same route ACK probe.

---

## Definition of Done

This project is not considered complete because “tests are green” alone. All of the following must be true simultaneously:

- [ ] Production gateways cannot become pollers.
- [ ] Exactly one worker owns ACB side effects.
- [ ] History/catch-up yield between pages and cannot hold realtime hostage for an entire range.
- [ ] History UI is backed by durable async jobs, not a long HTTP request.
- [ ] Session/generation/auth state uncertainty fails closed before ACB calls.
- [ ] Gateway-only deploy provably leaves singleton/core container IDs unchanged.
- [ ] Worker upgrade is a drain/stop/start singleton handoff with rollback.
- [ ] Active login protects auth-browser/core from deployment replacement.
- [ ] Normal deploy never generates secrets.
- [ ] Backup encryption key is independent from app encryption key and durable backups contain no plaintext DB/secrets.
- [ ] Production Compose cannot render without immutable image digests.
- [ ] VPS cryptographically verifies exact signed promotion identity before mutation.
- [ ] CI no longer upgrades core on every main push.
- [ ] Traefik promotion is accepted only after a real route identity check.
- [ ] SSE replay survives Blue/Green switching.
- [ ] Full failure-drill matrix passes.
- [ ] Final invariant audit against the spec records no unexplained deviations.

---

## Self-Review Checklist for This Plan

- [ ] Every audited critical gap is mapped to at least one implementation task and regression test.
- [ ] Scheduler work precedes async history/catch-up completion so no long-range operation can retain the old mutex architecture.
- [ ] API/RPC/schema/frontend changes have an explicit compatibility release boundary.
- [ ] Secret provisioning and backup encryption are separated from normal deploy behavior.
- [ ] Gateway, schema, worker, auth-browser, TTS, Bark, and Traefik deployment responsibilities are independently testable.
- [ ] CI promotion scope is signed and verified before use.
- [ ] No task instructs automatic destructive database rollback.
- [ ] No task requires restarting the shared Traefik/cloudflared stack.
- [ ] The plan contains no intentionally deferred implementation gaps; optional enhancements are excluded instead of left ambiguous.
- [ ] The plan's file/type names are internally consistent with the final invariant spec.
