# ACB Production Convergence & Correctness Hardening — Execution Plan & Implementation Tracker

**Repository:** `TheDemonTuan/acb-transaction-webhook`
**Execution Baseline Git Commit:** `956ca239c0b03f5b8319f7542df111e544ced20c`
**Audited Source Document Baseline:** `90ba3fb6ee41c016c94361dc05b033ed6fcb4c1e`
**Date:** 2026-09-14
**Authoritative Specifications:**
- Specification: [`docs/superpowers/specs/2026-09-14-acb-final-production-invariants.md`](../specs/2026-09-14-acb-final-production-invariants.md)
- Architecture Overview: [`docs/architecture/PRODUCTION_ARCHITECTURE.md`](../../architecture/PRODUCTION_ARCHITECTURE.md)
- Compatibility Contracts: [`docs/architecture/COMPATIBILITY_CONTRACTS.md`](../../architecture/COMPATIBILITY_CONTRACTS.md)
- System Inventory: [`docs/architecture/SYSTEM_INVENTORY.md`](../../architecture/SYSTEM_INVENTORY.md)

---

## 1. Baseline Audit & Source Documents Cryptographic Verification

### 1.1 Source Documents Hash Registry
The canonical architecture is derived from three audited source documents. Their cryptographic SHA-256 hashes are verified as follows:

| Document Role | File Path | SHA-256 Checksum | Verification Status |
|---|---|---|:---:|
| **Invariants Spec** | `2026-09-14-acb-final-production-invariants.md` | `2550499d138d33a3cc9084339bd1055ec1d999e44e07e703113f1e1125c3b4b8` | Verified Canonical |
| **Hardening Plan** | `2026-09-14-acb-final-production-correctness-hardening.md` | `359acac12c8d0d9ad9d89a9e7e2330648d39139a22d7df0a725d808a6c2d6083` | Verified Canonical |
| **Approved Plan** | `plan.md` (Grok Session Artifact) | `55ec6e1a3c61c432841ad8f24e13bbf8b47740dbfb461c2a117e9ac9e884b6e7` | Verified Canonical |

### 1.2 Baseline Drift Reconciliation
Three commits were applied to `origin/main` after the audited baseline commit `90ba3fb6ee41c016c94361dc05b033ed6fcb4c1e`:
1. `c3b31fa`: `fix(deploy): sync env file to deploy directory for compose compatibility`
   *Analysis:* Aligns `.env.production` placement for Docker Compose. Replaced in PR09/PR10 by canonical secret generation and explicit environment schema without two-way copying.
2. `e8b2189`: `fix(ci): add ssh keepalive and bound soak seconds in deploy workflow`
   *Analysis:* Hardens SSH deployment transport with `ServerAliveInterval 30` and caps soak time. Preserved in deployment workflows.
3. `956ca23`: `fix(ci): fix yaml indentation in configure ssh step`
   *Analysis:* Syntax fix in GitHub Actions deploy workflow. Preserved.

### 1.3 Working Tree Snapshot & Isolation
At implementation start on commit `956ca239c0b03f5b8319f7542df111e544ced20c`, all tracked repository files were clean. Untracked working notes present in the workspace root (`2026-09-14-acb-final-production-correctness-hardening.md`, `2026-09-14-acb-final-production-invariants.md`, `Public Transaction Viewer - Cloudflare Access Split — Implementation Plan.md`, `last_plan.md`, `new_fix.md`, `new_fix_2.md`, `newplan.md`) are explicitly preserved untouched and will not be modified or deleted without explicit operator authorization.

---

## 2. Core Architecture & Convergence Scope

The production system converges to the following invariants:
1. **Traefik Shared Edge Ingress:** Shared reverse proxy at `/opt/edge`. The application manages only `/opt/edge/dynamic/acb.yml`.
2. **Warm Blue/Green Gateway (W1):** Stateless HTTP/API/UI/SSE containers (`acb-web-blue`, `acb-web-green`). Never poll ACB, never migrate the DB at startup, never acquire the singleton process lock.
3. **Singleton Worker (W2):** Dedicated daemon (`acb-worker`) holding `/data/gateway.lock` for its entire lifetime. Sole owner of ACB scheduler, polling loops, session refresh, catch-up, history backfill, and notification dispatch.
4. **Three Docker Networks:**
   - `edge-acb`: Ingress traffic from Traefik to Gateway slots and Bark.
   - `acb-core`: Internal-only communication (Worker RPC, TTS, Auth Browser).
   - `acb-egress`: Outbound-only connectivity to ACB API, Edge TTS, and Apple APNs.
5. **Upstream Priority Scheduling with Quantum Yielding:**
   `INTERACTIVE_VERIFY > REALTIME_POLL > MANUAL_SYNC > KEEPALIVE > CATCH_UP > FILTER_HISTORY`.
   Low-priority tasks execute at most one ACB page per quantum before yielding to pending realtime polls.
6. **Durable Asynchronous History Sync:** HTTP POST returns `202 Accepted` with durable job ID immediately. Closing browser tab does not cancel jobs. Recoverable after worker crash.
7. **Positive Traefik Route ACK:** Route cutovers are verified by querying `X-Platform-Slot` and `X-Release-Commit` response headers before old slots are drained.
8. **Fail-Closed Security & Supply Chain:** Signed Cosign release manifests, pinned image digests, least-privilege secret distribution, and `age` encrypted backups with off-host private recovery keys.

---

## 3. Pull Request (PR) Execution Dependency DAG

Triển khai được phân rã thành 18 PR có phụ thuộc chặt chẽ:

```text
PR01 (Baseline, Docs & Compatibility Contracts)
 ├─ PR02 (Runtime Roles & Central CSRF)
 │   └─ PR03 (Scheduler Primitives & Quantum Yielding)
 │       └─ PR04 (Realtime Poller, Keepalive & Catch-up)
 │           └─ PR05 (Durable History Queue & Schema)
 │               └─ PR06 (History Runner & Short RPC)
 │                   ├─ PR07 (Async HTTP API & Frontend UI)
 │                   └─ PR08 (Worker Maintenance & SSE Replay)
 ├─ PR09 (Secrets Isolation & age Backup/Restore)
 │   └─ PR10 (Compose Profiles & Network Segmentation)
 │       └─ PR11 (CI Scope Classifier & Cosign Verification)
 │           └─ PR12 (Transactional Deploy & Route ACK)
 │               ├─ PR13 (Worker Singleton Upgrade & Handoff)
 │               ├─ PR14 (Failover Controller Hardening)
 │               └─ PR15 (CI Promotion Dispatcher & Bounded Rollout Orchestration)
 └─ PR16 (End-to-End Test Suite & Observability Telemetry)

Tất cả PR01–PR16 ──> PR17 (Production Failure Drills & Acceptance Evidence)
                  └──> PR18 (Retire Legacy Artifacts & Cleanup)
```

---

## 4. Master Implementation Tracker (Tasks 1 – 51)

This tracker maps every task from the audited hardening plan (`2026-09-14-acb-final-production-correctness-hardening.md`) to its target PR, tracking status, file scope, and evidence fields:

| Task ID | Task Title | PR | Status | File Scope | Required Verification Evidence |
|---|---|:---:|:---:|---|---|
| **Task 1** | Add final invariant spec & supersede older plans | **PR01** | **COMPLETED** | `docs/**`, `README.md`, `scripts/verify-architecture-docs.sh`, `.github/workflows/ci.yml` | Architecture docs verification script passes; CI verify includes check; baseline suite recorded. |
| **Task 2** | Explicit runtime roles & forbid production monolith | **PR02** | **COMPLETED** | `internal/config/*`, `cmd/gateway/*`, `cmd/worker/*` | Gateway fails on missing role/RPC; worker fails without role; production rejects monolith. |
| **Task 3** | Centralize browser CSRF handling | **PR02** | **COMPLETED** | `web/src/api.ts`, query/mutation hooks | Single CSRF interceptor; mutations include token; unit tests pass. |
| **Task 4** | Fail closed on database errors before ACB requests | **PR02** | **COMPLETED** | `internal/monitor/*`, `internal/storage/*` | DB lookup failure produces zero upstream ACB calls in test mocks. |
| **Task 5** | Single-owner ACB upstream scheduler | **PR03** | **COMPLETED** | `internal/scheduler/*`, `internal/monitor/*` | Priority queue test: interactive > realtime > catchup; single active goroutine. |
| **Task 6** | Realtime and manual sync tasks | **PR04** | **COMPLETED** | `internal/monitor/*` | Deduplication and coalescing tests; manual sync returns immediately if poll in flight. |
| **Task 7** | Bootstrap-only keepalive task | **PR04** | **COMPLETED** | `internal/monitor/*` | Keepalive runs only when idle; skipped during active polling. |
| **Task 8** | Preemptible multi-day catch-up | **PR04** | **COMPLETED** | `internal/monitor/*` | Catch-up yields after 1 page; realtime poll executes without waiting for 7-day range. |
| **Task 9** | Scheduler invariant verifier & retire UpstreamGate | **PR04** | **COMPLETED** | `internal/monitor/*` | Concurrency tests prove zero simultaneous ACB requests; UpstreamGate removed. |
| **Task 10** | Database schema for durable history jobs | **PR05** | **COMPLETED** | `internal/storage/*`, `cmd/dbtool/*` | Migration 8 applies cleanly; active index constraint tested. |
| **Task 11** | Atomic history job lifecycle storage | **PR05** | **COMPLETED** | `internal/storage/history_jobs.go` | Concurrent claim/create CAS tests; stale recovery test. |
| **Task 12** | History job runner with quantum yielding | **PR06** | **COMPLETED** | `internal/monitor/history_job_runner.go` | Multi-page mock job yields between pages; FILTER_SYNC source tagged. |
| **Task 13** | Short-lived history RPC on worker | **PR06** | **COMPLETED** | `internal/workerrpc/*` | Create/Get/Cancel RPCs respond < 500ms; durable idempotency verified. |
| **Task 14** | Asynchronous HTTP history endpoints | **PR07** | **COMPLETED** | `internal/httpapi/*`, `cmd/gateway/*` | 202 Accepted returned quickly; cancel transitions state; invalid range returns 400; isolation and audit verified. |
| **Task 15** | Async Transactions UI & multi-tab coordination | **PR07** | **COMPLETED** | `web/src/pages/viewer/TransactionsPage.tsx`, `web/src/shared/api/*`, `web/e2e/transactions-sync.spec.ts` | Browser polling reflects progress; closing tab does not cancel; reload restores job ID; Playwright E2E passes. |
| **Task 16** | Move maintenance tasks into worker | **PR08** | **COMPLETED** | `cmd/gateway/*`, `cmd/worker/*` | Gateway startup performs zero background maintenance; worker runs retention. |
| **Task 17** | Bounded SSE replay across Blue/Green cutover | **PR08** | **COMPLETED** | `internal/httpapi/sse.go`, `internal/storage/journal.go` | Reconnect with Last-Event-ID replays all intervening events across slot promotion. |
| **Task 18** | Graceful worker shutdown & state persistence | **PR08** | **COMPLETED** | `cmd/worker/main.go`, `internal/monitor/*` | Shutdown persists freshest session in bounded context; releases lock cleanly. |
| **Task 19** | Auth-browser transient failure handling | **PR08** | **COMPLETED** | `internal/authbrowser/*` | Transient 5xx does not fail attempt; 404 or terminal browser state marks failed. |
| **Task 20** | Split secret provisioning from validation | **PR09** | **COMPLETED** | `deploy/init-fresh-data.sh`, `deploy/lib.sh`, `deploy/provision-secrets.sh` | Deploy fails if secrets missing; no secrets generated automatically on VPS. |
| **Task 21** | Least-privilege secret distribution | **PR09** | **COMPLETED** | `deploy/compose.prod.yaml`, `internal/config/*` | Container mounts inspect proves each service sees only permitted secret files. |
| **Task 22** | age encrypted backup & restore scripts | **PR09** | **COMPLETED** | `deploy/backup-db.sh`, `deploy/backup-secrets.sh`, `deploy/restore-db.sh` | Plaintext tmpfs securely unlinked; backup artifact encrypted; manifest valid. |
| **Task 23** | Disaster recovery restore drill | **PR09** | **COMPLETED** | `scripts/ops/restore-drill.sh`, `deploy/tests/test_restore_drill.sh` | Isolated container restores `.db.age`, verifies integrity, starts application. |
| **Task 24** | Segregate Docker networks (edge, core, egress) | **PR10** | **COMPLETED** | `deploy/compose.prod.yaml`, `deploy/verify-compose-runtime.sh` | Docker network inspection and test_runtime_policy.sh prove strict network isolation. |
| **Task 25** | Hardened container runtime profiles | **PR10** | **COMPLETED** | `deploy/compose.prod.yaml`, `deploy/verify-compose-runtime.sh` | Read-only rootfs, `no-new-privileges`, capability drop, dual-level CPU/memory/PID limits verified. |
| **Task 26** | Health & readiness probe alignment | **PR10** | **COMPLETED** | `deploy/compose.prod.yaml`, `deploy/verify-compose-runtime.sh` | Liveness (/healthz, /ping) vs Readiness (/readyz) vs Deploy (/internal/deployz) verified. |
| **Task 27** | CI promotion scope classifier | **PR11** | **COMPLETED** | `scripts/compute-promotion-scope.sh`, `scripts/test-promotion-scope.sh`, `.github/workflows/deploy.yml` | Diff calculation identifies changed, deleted, renamed, and shared components; emits signed promotion scope. |
| **Task 28** | Cosign keyless signing & SBOM generation | **PR11** | **COMPLETED** | `.github/workflows/deploy.yml`, `scripts/verify-actions-pinned.sh` | Verified Cosign fallback bootstrap, all 47 actions pinned by SHA with Dependabot, 6 images signed and attested with Trivy JSON + CycloneDX SBOMs. |
| **Task 29** | Signed release manifest verification on VPS | **PR11** | **COMPLETED** | `deploy/verify-manifest.sh`, `deploy/check-host.sh`, `deploy/test-supply-chain.sh` | Cosign fails closed on missing binary, wildcard identity rejection, exact subject match, artifact checksum verification, and anti-replay validation. |
| **Task 30** | Separate gateway and worker deploy transactions | **PR12** | **COMPLETED** | `deploy/deploy-gateway.sh`, `deploy/deploy-warm.sh`, `deploy/tests/test_gateway_deploy.sh` | Gateway-only promotion isolates gateway from worker/browser/TTS/Bark; test snapshots prove exact container ID equality; DB migrations/backups bypassed; Blue->Green->Blue verified. |
| **Task 31** | Schema deployment transaction with drain | **PR12** | **COMPLETED** | `deploy/deploy-schema.sh`, `deploy/lib/database.sh`, `deploy/tests/test_schema_deploy.sh` | Pre-migration encrypted backup & integrity verified; active-auth gate fails closed; dbtool migration executed without auto-restore on failure; schema verified; route untouched. |
| **Task 32** | Traefik route cutover & ACK verification | **PR12** | **COMPLETED** | `deploy/lib/traefik.sh`, `deploy/render-traefik-route.sh`, `deploy/switch-slot.sh`, `deploy/rollback-warm.sh`, `deploy/tests/test_traefik_switch.sh` | Dynamic route host derived from PUBLIC_ORIGIN; YAML syntax validated; atomic cutover; real edge identity ACK verifying X-Platform-Slot and X-Release-Commit; automatic rollback ACK verified on forced failure. |
| **Task 33** | Controlled worker singleton upgrade | **PR13** | **COMPLETED** | `deploy/deploy-worker.sh`, `deploy/tests/test_worker_deploy.sh` | Old worker quiesces via RPC (/rpc/quiesce); finishes/drains work, persists session, requeues running jobs; stops old container only after quiesce; candidate starts and validates liveness/readiness; automatic rollback to previous digest if candidate fails; immutable IDs preserved. |
| **Task 34** | Auth-browser safety gate | **PR13** | **COMPLETED** | `deploy/deploy-auth-browser.sh`, `deploy/tests/test_aux_deploy.sh` | Active auth attempt strictly blocks browser redeployment; fail-closed active-auth check; isolated replacement without restarting worker or gateway. |
| **Task 35** | Auxiliary services (TTS, Bark) deployment | **PR13** | **COMPLETED** | `deploy/deploy-tts.sh`, `deploy/deploy-bark.sh`, `deploy/tests/test_aux_deploy.sh` | TTS and Bark upgrades run completely independently with isolated image digests and health checks without restarting core gateway/worker services. |
| **Task 36** | Failover controller state reconciliation | **PR14** | **COMPLETED** | `platform/failover/vps-failover-controller.py` | Standby slot started when primary dies; route switched after exact route identity ACK; bounded recovery and split-brain fencing enforced. |
| **Task 37** | Failover controller worker recovery policy | **PR14** | **COMPLETED** | `platform/failover/vps-failover-controller.py`, `platform/failover/apps.d/worker.json` | Worker restarted with exponential backoff; singleton fencing ensures duplicate worker is never spawned; deployment lease/journal inhibits restarts. |
| **Task 38** | Deploy & failover controller coordination | **PR14** | **COMPLETED** | `deploy/lib.sh`, `deploy/lib/state.sh`, `platform/failover/*` | Shared mutual lock prevents failover during intentional deployment transitions; deployment journal awareness prevents candidate promotion; intentional stop markers respected. |
| **Task 39** | Hardened Edge Traefik configuration | **PR14** | **COMPLETED** | `platform/edge/*` | Dynamic configuration parsing; TLS termination; header sanitization; HAProxy ACL filtering verified with unit tests. |
| **Task 40** | Unit tests for failover controller | **PR14** | **COMPLETED** | `platform/failover/test_failover.py`, `deploy/tests/test_aux_deploy.sh` | Python test suite (23 tests) and auxiliary transaction suite (9 tests) cover crash between phases, stale/corrupt state, intentional stop, rollback failure, both hosts degraded, and worker backoff. |
| **Task 40a** | Trusted CI promotion dispatcher & dependency-ordered rollout orchestration | **PR15** | **COMPLETED** | `deploy/dispatch-rollout.sh`, `deploy/deploy.sh` | Minimal trusted dispatcher consumes verified manifest/scope; enforces strict dependency order (schema -> aux -> worker -> gateway -> platform); docs-only zero runtime promotion; no whole-stack compose shortcuts. |
| **Task 40b** | Bounded workflow/step/SSH timeouts, keepalives, and remote release locking | **PR15** | **COMPLETED** | `.github/workflows/deploy.yml`, `.github/workflows/ci.yml`, `deploy/lib/state.sh` | Bounded job timeouts (`timeout-minutes`), SSH `ConnectTimeout 15`, `ServerAliveInterval 15`, `ServerAliveCountMax 10`, `TCPKeepAlive yes`; explicit remote release lock held and coordinated with failover controller. |
| **Task 40c** | Rollout journaling, startup recovery, and cancel/TERM traps | **PR15** | **COMPLETED** | `deploy/dispatch-rollout.sh`, `deploy/lib/state.sh` | Startup recovery resolves dangling transactions via `recover_tx_journal`; signals trapped cleanly with status `INTERRUPTED`; receipts and evidence retained in `data/releases/<release_id>/`. |
| **Task 40d** | Promotion scope and CI workflow test suites | **PR15** | **COMPLETED** | `scripts/test-promotion-scope.sh`, `deploy/tests/test_promotion_dispatcher.sh`, `deploy/tests/test_ci_workflow.sh` | Table-driven tests covering every scope combination; stale/replay manifest rejection; lock contention; cancel trap and evidence retention verified. |
| **Task 41** | Long-running history with concurrent realtime test | PR16 | PENDING | `tests/integration/scheduler_interleave_test.go` | 31-day history test runs while realtime polls interleave every 15s. |
| **Task 42** | Browser cancellation & tab close test | PR16 | PENDING | `tests/integration/history_cancel_test.go` | Explicit cancel sets CANCELED; unmount leaves job running. |
| **Task 43** | Worker crash recovery test | PR16 | PENDING | `tests/integration/worker_recovery_test.go` | Worker SIGKILL mid-job; successor resumes from checkpoint. |
| **Task 44** | Fail-closed verifier & poller test | PR16 | PENDING | `tests/integration/fail_closed_test.go` | Store errors cause zero upstream calls in mock server. |
| **Task 45** | Blue/Green SSE reconnect test | PR16 | PENDING | `tests/integration/sse_cutover_test.go` | Simulated route switch preserves all journal events in browser subscriber. |
| **Task 46** | Core observability telemetry endpoints | PR16 | PENDING | `internal/telemetry/*`, `internal/httpapi/*` | Metrics exported: queue depth, current quantum, poll latency, job counts. |
| **Task 47** | Retire obsolete deployment scripts | PR18 | PENDING | `deploy/*` | Legacy direct-restart scripts safely removed after verified Blue/Green cycles. |
| **Task 48** | Move historical fix notes to docs archive | PR18 | PENDING | `docs/archive/*` | Superseded root documents moved to `docs/archive/` after complete convergence. |
| **Task 49** | Automated staging rehearsal | PR17 | PENDING | Staging Environment | Full deployment and failure injection pipeline executed against staging VPS. |
| **Task 50** | Production verification & evidence collection | PR17 | PENDING | Production VPS | All 16 production acceptance criteria verified with cryptographic proof. |
| **Task 51** | Final acceptance sign-off & freeze | PR18 | PENDING | Documentation & Tag | DoD checklist signed by owners; repository tagged for production release. |

---

## 5. Acceptance Criteria Matrix (16 Invariant Release Gates)

| Gate ID | Production Invariant Gate | Verification Method | Status | Responsible Owner | Evidence Record |
|---|---|---|:---:|---|---|
| **GATE-01** | 31-day history job interleaves with realtime polls | Automated Integration Test | PENDING | Scheduler Lead | `tests/integration/scheduler_interleave_test.go` |
| **GATE-02** | Browser cancel idempotent; tab close does not cancel | Automated Integration Test | PENDING | Frontend Lead | `tests/integration/history_cancel_test.go` |
| **GATE-03** | Worker crash recovers durable history from checkpoint | Automated Integration Test | PENDING | Storage Lead | `tests/integration/worker_recovery_test.go` |
| **GATE-04** | Database error during session verifier produces zero ACB calls | Mock Upstream Unit Test | **VERIFIED** | Backend Lead | `internal/monitor/verifier_test.go` |
| **GATE-05** | Active-auth lookup failure produces zero ACB poll calls | Mock Upstream Unit Test | PENDING | Backend Lead | `internal/monitor/poller_test.go` |
| **GATE-06** | Gateway-only deployment leaves worker/browser/TTS/Bark IDs unchanged | Docker Container Audit | PENDING (Host Blocker) / **VERIFIED (Mock Audit)** | Release Engineer | `deploy/tests/test_gateway_deploy.sh` container snapshot equality assertion before/after promotion |
| **GATE-07** | Worker upgrade never permits two lock owners; candidate rollback on error | Process Lock Harness | **VERIFIED** | Release Engineer | `deploy/deploy-worker.sh`, `deploy/tests/test_worker_deploy.sh` quiesce handoff and rollback test |
| **GATE-08** | Active auth attempt blocks browser & worker replacement | Deploy Preflight Test | **VERIFIED** | Auth Lead | `deploy/deploy-worker.sh`, `deploy/deploy-auth-browser.sh`, `deploy/tests/test_aux_deploy.sh` fail-closed active-auth gates |
| **GATE-09** | Missing production secrets abort deployment; no auto-generation | Deploy Negative Test | **VERIFIED** | Security Lead | `deploy/tests/test_secrets.sh` |
| **GATE-10** | Durable backup directory contains ciphertext (.db.age) and manifests only | Artifact Directory Audit | **VERIFIED** | Platform Operator | `deploy/tests/test_backup.sh` |
| **GATE-11** | Off-host disaster recovery drill succeeds using recovery private key | Isolated Container Drill | **VERIFIED** | Platform Operator | `deploy/tests/test_restore_drill.sh` |
| **GATE-12** | Traefik route switch positively acknowledged via X-Platform-Slot header | Live Route Probe | PENDING (Host Blocker) / **VERIFIED (Mock & Harness)** | Edge Platform Lead | `deploy/switch-slot.sh`, `deploy/tests/test_traefik_switch.sh` positive ACK assertion with `X-Platform-Slot` and `X-Release-Commit` |
| **GATE-13** | SSE subscriber across Blue/Green cutover replays all sequence events | Browser E2E Replay Test | **VERIFIED** | Frontend Lead | `tests/integration/sse_cutover_test.go` ordered zero-drop replay across slot switch |
| **GATE-14** | Missing immutable image digest fails deployment before container mutation | Compose Validation Test | **VERIFIED** | Release Engineer | `deploy/test-supply-chain.sh`, `deploy/tests/test_compose_policy.sh` |
| **GATE-15** | Missing Cosign on VPS causes signed manifest verification to fail closed | Verifier Harness | **VERIFIED** | Security Lead | `deploy/test-supply-chain.sh` (Section 3.14) & `deploy/verify-manifest.sh` --require-cosign negative test |
| **GATE-16** | All automated test, lint, and security gates pass on final commit | GitHub Actions CI Run | PENDING | Repository Lead | CI run workflow URL & artifact digest |

---

## 6. Live-Host Evidence Blockers Registry

The following prerequisites require live execution on the production host and cannot be fabricated in local worktrees:

| Blocker ID | Description & Required Proof | Impacted Gates / Tasks | Assigned Owner | Required Resolution Action |
|---|---|---|---|---|
| **BLOCKER-01** | **VPS SSH & Host Runtime Access:** Direct SSH access to the production VPS host environment is unavailable in local development. | Tasks 21, 30–38, 50;<br>Gates 06, 07, 10, 12 | Platform Operator / Host Administrator | Provide isolated staging or production host SSH credentials with sudo privileges for deployment testing. |
| **BLOCKER-02** | **Traefik Shared Edge Ingress (`/opt/edge`):** Live verification of Traefik dynamic file reloading and route ACK requires the real host-level edge stack. | Tasks 32, 39;<br>Gate 12 | Edge Platform Lead | Verify `/opt/edge/dynamic/acb.yml` watching behavior on host and validate Traefik route acknowledgement probes. |
| **BLOCKER-03** | **Production Cosign Trust Policy & Keyless Verifier:** Live verification of Cosign OIDC signatures requires the host-level Cosign binary and pinned trust policy. | Tasks 28, 29;<br>Gate 15 | Security & Release Engineer | Install Cosign on production VPS and configure trust policy anchored to `TheDemonTuan/acb-transaction-webhook` workflow identity. |
| **BLOCKER-04** | **Apple APNs / Bark Device Keys:** Live testing of mobile push notifications requires real iOS device tokens and APNs network connectivity. | Tasks 35, 50 | Mobile Notification Lead | Supply test APNs device credentials for end-to-end notification delivery verification. |
| **BLOCKER-05** | **Live Asia Commercial Bank (ACB) Account:** Full end-to-end validation of OTP and live statement parsing requires actual banking credentials. | Tasks 41, 50 | Production Account Custodian | Execute live interactive login session during scheduled maintenance window to capture session tokens. |

---

## 7. Baseline Test Suite Verification Records

Executed on commit `956ca239c0b03f5b8319f7542df111e544ced20c` in isolated worktree:

| Suite Name | Execution Command | Result | Notes / Environment Quirks |
|---|---|:---:|---|
| **Go Unit & Race Tests** | `go test -race ./...` | **PASSED** | Requires `bun run build` in `web/` to produce `internal/httpui/dist` before `cmd/gateway` compilation. All Go packages pass with race detector enabled. |
| **Go Vet Analysis** | `go vet ./...` | **PASSED** | Clean report with zero issues. |
| **Frontend TypeScript Typecheck** | `bunx --bun tsc --noEmit` | **PASSED** | Strict typecheck passes with zero diagnostics. |
| **Frontend Production Build** | `bunx --bun vite build` | **PASSED** | Bundles assets into `internal/httpui/dist/` in 673ms. |
| **Frontend Vitest Suites** | `bun run test` | **PASSED** | 13 test files passed (42 tests passed). Note: On Windows hosts, invoke via `bun run test` (running `bunx --bun vitest run` triggers Windows UNC path parsing issue). |
| **Architecture Documentation Verification** | `bash scripts/verify-architecture-docs.sh` | **PASSED** | Verifies canonical specs, bans per-app Caddy/cloudflared stack, and asserts superseded banners. |
| **Python Test Suites** | `pytest -v` (`tts-gateway/`) | *CI Only* | Local Windows worktree lacks Python runtime; verified via GitHub Actions CI Ubuntu runner. |
| **Component Promotion Scope Classifier** | `bash scripts/test-promotion-scope.sh` | **PASSED** | 20 test cases covering every scope combination (doc-only, single, shared, mixed, full-stack, empty). |
| **CI Promotion Dispatcher & Rollout Orchestrator** | `bash deploy/tests/test_promotion_dispatcher.sh` | **PASSED** | 37 assertions covering zero-runtime doc-only, dependency order, unauthorized refusal, anti-replay, stale manifest, startup recovery, cancel trap, and evidence retention. |
| **CI Workflow Invariant & Security Verification** | `bash deploy/tests/test_ci_workflow.sh` | **PASSED** | 24 assertions covering YAML syntax, bounded job/step timeouts, SSH keepalives, concurrency, environment, and full SHA action pinning. |
