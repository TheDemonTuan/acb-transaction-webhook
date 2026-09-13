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
 │               └─ PR15 (Full CI Verification Pipeline)
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
| **Task 2** | Explicit runtime roles & forbid production monolith | PR02 | PENDING | `internal/config/*`, `cmd/gateway/*`, `cmd/worker/*` | Gateway fails on missing role/RPC; worker fails without role; production rejects monolith. |
| **Task 3** | Centralize browser CSRF handling | PR02 | PENDING | `web/src/api.ts`, query/mutation hooks | Single CSRF interceptor; mutations include token; unit tests pass. |
| **Task 4** | Fail closed on database errors before ACB requests | PR02 | PENDING | `internal/monitor/*`, `internal/storage/*` | DB lookup failure produces zero upstream ACB calls in test mocks. |
| **Task 5** | Single-owner ACB upstream scheduler | PR03 | PENDING | `internal/scheduler/*`, `internal/monitor/*` | Priority queue test: interactive > realtime > catchup; single active goroutine. |
| **Task 6** | Realtime and manual sync tasks | PR04 | PENDING | `internal/monitor/*` | Deduplication and coalescing tests; manual sync returns immediately if poll in flight. |
| **Task 7** | Bootstrap-only keepalive task | PR04 | PENDING | `internal/monitor/*` | Keepalive runs only when idle; skipped during active polling. |
| **Task 8** | Preemptible multi-day catch-up | PR04 | PENDING | `internal/monitor/*` | Catch-up yields after 1 page; realtime poll executes without waiting for 7-day range. |
| **Task 9** | Scheduler invariant verifier & retire UpstreamGate | PR04 | PENDING | `internal/monitor/*` | Concurrency tests prove zero simultaneous ACB requests; UpstreamGate removed. |
| **Task 10** | Database schema for durable history jobs | PR05 | PENDING | `internal/storage/*`, `cmd/dbtool/*` | Migration 8 applies cleanly; active index constraint tested. |
| **Task 11** | Atomic history job lifecycle storage | PR05 | PENDING | `internal/storage/history_jobs.go` | Concurrent claim/create CAS tests; stale recovery test. |
| **Task 12** | History job runner with quantum yielding | PR06 | PENDING | `internal/monitor/history_runner.go` | Multi-page mock job yields between pages; FILTER_SYNC source tagged. |
| **Task 13** | Short-lived history RPC on worker | PR06 | PENDING | `internal/workerrpc/*` | Create/Get/Cancel RPCs respond < 500ms; durable idempotency verified. |
| **Task 14** | Asynchronous HTTP history endpoints | PR07 | PENDING | `internal/httpapi/*` | 202 Accepted returned; cancel transitions state; invalid range returns 400. |
| **Task 15** | Async Transactions UI & multi-tab coordination | PR07 | PENDING | `web/src/pages/viewer/TransactionsPage.tsx` | Browser polling reflects progress; closing tab does not cancel; reload restores job ID. |
| **Task 16** | Move maintenance tasks into worker | PR08 | PENDING | `cmd/gateway/*`, `cmd/worker/*` | Gateway startup performs zero background maintenance; worker runs retention. |
| **Task 17** | Bounded SSE replay across Blue/Green cutover | PR08 | PENDING | `internal/httpapi/sse.go`, `internal/storage/journal.go` | Reconnect with Last-Event-ID replays all intervening events across slot promotion. |
| **Task 18** | Graceful worker shutdown & state persistence | PR08 | PENDING | `cmd/worker/main.go`, `internal/monitor/*` | Shutdown persists freshest session in bounded context; releases lock cleanly. |
| **Task 19** | Auth-browser transient failure handling | PR08 | PENDING | `internal/authbrowser/*` | Transient 5xx does not fail attempt; 404 or terminal browser state marks failed. |
| **Task 20** | Split secret provisioning from validation | PR09 | PENDING | `deploy/init-fresh-data.sh`, `deploy/lib.sh` | Deploy fails if secrets missing; no secrets generated automatically on VPS. |
| **Task 21** | Least-privilege secret distribution | PR09 | PENDING | `deploy/compose.prod.yaml` | Container mounts inspect proves each service sees only permitted secret files. |
| **Task 22** | age encrypted backup & restore scripts | PR09 | PENDING | `deploy/backup.sh`, `deploy/restore.sh` | Plaintext tmpfs securely unlinked; backup artifact encrypted; manifest valid. |
| **Task 23** | Disaster recovery restore drill | PR09 | PENDING | `deploy/tests/test_restore.sh` | Isolated container restores `.db.age`, verifies integrity, starts application. |
| **Task 24** | Segregate Docker networks (edge, core, egress) | PR10 | PENDING | `deploy/compose.prod.yaml` | Docker network inspect proves strict network isolation. |
| **Task 25** | Hardened container runtime profiles | PR10 | PENDING | `deploy/compose.prod.yaml`, `Dockerfile*` | Read-only rootfs, `no-new-privileges`, capability drop, memory/PID limits. |
| **Task 26** | Health & readiness probe alignment | PR10 | PENDING | `deploy/compose.prod.yaml`, `internal/httpapi/*` | Liveness (/healthz) vs Readiness (/readyz) vs Deploy (/internal/deployz). |
| **Task 27** | CI promotion scope classifier | PR11 | PENDING | `.github/workflows/ci.yml`, `scripts/ci/*` | Diff calculation identifies changed components; matches promotion scope. |
| **Task 28** | Cosign keyless signing & SBOM generation | PR11 | PENDING | `.github/workflows/ci.yml` | Images and release manifest signed with GitHub Actions OIDC. |
| **Task 29** | Signed release manifest verification on VPS | PR11 | PENDING | `deploy/verify-manifest.sh` | Cosign fails closed on invalid signature or unauthorized subject identity. |
| **Task 30** | Separate gateway and worker deploy transactions | PR12 | PENDING | `deploy/deploy-warm.sh`, `deploy/deploy-worker.sh` | Gateway deploy leaves worker container untouched; timestamps confirm continuous polling. |
| **Task 31** | Schema deployment transaction with drain | PR12 | PENDING | `deploy/deploy-schema.sh`, `cmd/dbtool/*` | Pre-migration backup verified; DB locked; migrations applied; health checked. |
| **Task 32** | Traefik route cutover & ACK verification | PR12 | PENDING | `deploy/switch-slot.sh`, `deploy/rollback-warm.sh` | Positive ACK asserts `X-Platform-Slot`; route rollback verified on forced failure. |
| **Task 33** | Controlled worker singleton upgrade | PR13 | PENDING | `deploy/deploy-worker.sh` | Candidate starts only after old releases lock; rolls back if candidate unready. |
| **Task 34** | Auth-browser safety gate | PR13 | PENDING | `deploy/deploy-auth-browser.sh` | Active auth attempt blocks browser redeployment. |
| **Task 35** | Auxiliary services (TTS, Bark) deployment | PR13 | PENDING | `deploy/deploy-tts.sh`, `deploy/deploy-bark.sh` | TTS/Bark upgrades run independently without restarting core services. |
| **Task 36** | Failover controller state reconciliation | PR14 | PENDING | `platform/failover/vps-failover-controller.py` | Standby slot started when primary dies; route switched after ACK. |
| **Task 37** | Failover controller worker recovery policy | PR14 | PENDING | `platform/failover/vps-failover-controller.py` | Worker restarted with exponential backoff; never spawns duplicate worker. |
| **Task 38** | Deploy & failover controller coordination | PR14 | PENDING | `deploy/lib.sh`, `platform/failover/*` | Shared lock prevents failover during intentional deployment transitions. |
| **Task 39** | Hardened Edge Traefik configuration | PR14 | PENDING | `platform/edge/*` | Dynamic configuration parsing; TLS termination; header sanitization. |
| **Task 40** | Unit tests for failover controller | PR14 | PENDING | `platform/failover/test_failover.py` | Python test suite covers failover scenarios and edge cases. |
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
| **GATE-04** | Database error during session verifier produces zero ACB calls | Mock Upstream Unit Test | PENDING | Backend Lead | `internal/monitor/verifier_test.go` |
| **GATE-05** | Active-auth lookup failure produces zero ACB poll calls | Mock Upstream Unit Test | PENDING | Backend Lead | `internal/monitor/poller_test.go` |
| **GATE-06** | Gateway-only deployment leaves worker/browser/TTS/Bark IDs unchanged | Docker Container Audit | PENDING (Host Blocker) | Release Engineer | Host Docker inspect diff before/after deploy |
| **GATE-07** | Worker upgrade never permits two lock owners; candidate rollback on error | Process Lock Harness | PENDING | Release Engineer | `deploy/tests/test_worker_handoff.sh` |
| **GATE-08** | Active auth attempt blocks browser & worker replacement | Deploy Preflight Test | PENDING | Auth Lead | `deploy/smoke-test-auth-browser.sh` |
| **GATE-09** | Missing production secrets abort deployment; no auto-generation | Deploy Negative Test | PENDING | Security Lead | `deploy/tests/test_missing_secrets.sh` |
| **GATE-10** | Durable backup directory contains ciphertext (.db.age) and manifests only | Artifact Directory Audit | PENDING | Platform Operator | `deploy/tests/test_backup_format.sh` |
| **GATE-11** | Off-host disaster recovery drill succeeds using recovery private key | Isolated Container Drill | PENDING (Host Blocker) | Platform Operator | `deploy/tests/test_restore.sh` output log |
| **GATE-12** | Traefik route switch positively acknowledged via X-Platform-Slot header | Live Route Probe | PENDING (Host Blocker) | Edge Platform Lead | `deploy/switch-slot.sh` probe trace |
| **GATE-13** | SSE subscriber across Blue/Green cutover replays all sequence events | Browser E2E Replay Test | PENDING | Frontend Lead | `web/tests/e2e/sse_cutover.spec.ts` |
| **GATE-14** | Missing immutable image digest fails deployment before container mutation | Compose Validation Test | PENDING | Release Engineer | `deploy/test-supply-chain.sh` |
| **GATE-15** | Missing Cosign on VPS causes signed manifest verification to fail closed | Verifier Harness | PENDING (Host Blocker) | Security Lead | `deploy/verify-manifest.sh` negative test |
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
