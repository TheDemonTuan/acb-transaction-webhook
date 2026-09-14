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

# ACB Transaction Webhook Final Production Invariants

**Repository:** `TheDemonTuan/acb-transaction-webhook`

**Audited baseline:** `90ba3fb6ee41c016c94361dc05b033ed6fcb4c1e`

**Date:** 2026-09-14

**Purpose:** This document is the single source of truth for the production architecture that the implementation plan must converge to. It replaces incremental fix notes as the authority for correctness, deployment safety, realtime behavior, and security.

## 1. Product Goals

The production system must prioritize the following in order:

1. Detect new ACB transactions as close to the configured polling interval as ACB permits.
2. Never allow a dashboard-only deployment to interrupt the singleton ACB poller.
3. Never run two production ACB pollers for the same connection generation.
4. Preserve session continuity across normal deploys and controlled worker upgrades.
5. Keep user-requested historical synchronization from delaying realtime detection.
6. Keep HTTP traffic available during gateway releases by warm Blue/Green promotion through the shared Traefik edge.
7. Fail closed when production safety checks, secrets, image identity, active-auth state, or route acknowledgement cannot be verified.
8. Keep operational overhead suitable for one VPS using Docker Compose; no Kubernetes or Swarm is introduced.
9. Make the same deployment and security model reusable for future repositories on the VPS.

## 2. Non-Goals

This architecture does not claim host-level high availability. A single VPS remains a single machine failure domain. Blue/Green protects application-process and release failures, not VPS, hypervisor, storage-device, regional, or provider failures.

The system also does not attempt to make ACB itself realtime. The unavoidable latency is the ACB polling interval plus the duration of the current ACB request and local processing.

## 3. Final Runtime Topology

```text
Internet
  |
Cloudflare Edge / Access
  |
Cloudflare Tunnel
  |
shared Traefik under /opt/edge
  |
edge-acb internal edge network
  |
  +------------------------+
  |                        |
gateway-blue            gateway-green
HTTP/API/UI/SSE         HTTP/API/UI/SSE
W1 warm Blue/Green      W1 warm Blue/Green
  |                        |
  +-----------+------------+
              |
          acb-core
              |
      +-------+---------+-------------+
      |                 |             |
 acb-worker        auth-browser   tts-gateway
 singleton W2      singleton W4   auxiliary W1
      |
      +-----------------------------+
              |
           bark W5
              |
          acb-egress
              |
 ACB / Cloudflare JWKS / TTS upstream / Bark clients / webhook destinations
```

The shared Traefik stack is external to this repository. Application deployments may atomically replace only the ACB-owned dynamic route file and must never restart the shared edge stack.

## 4. Runtime Ownership Invariants

### 4.1 Gateway owns only HTTP concerns

A production gateway may own:

- HTTP routing and API validation;
- Cloudflare Access authentication and authorization;
- CSRF enforcement;
- UI delivery;
- SQLite reads and ordinary dashboard writes;
- SSE subscriptions and journal replay/watch;
- auth-browser reverse proxy for the owner UI;
- worker RPC client calls;
- TTS HTTP client calls when a user requests audio.

A production gateway must not own:

- ACB polling;
- ACB keepalive;
- ACB catch-up;
- ACB historical fetch execution;
- notification delivery dispatch;
- journal retention;
- stale-auth background reaping;
- any singleton external side-effect loop.

### 4.2 Worker owns external side effects

Exactly one production worker owns:

- the live ACB client/session state;
- the upstream scheduler;
- scheduled realtime polls;
- manual immediate polls;
- keepalive;
- catch-up tasks;
- user history-sync jobs;
- session verification coordination;
- session persistence;
- notification delivery dispatch;
- journal retention;
- stale-auth maintenance;
- durable background-job recovery.

The worker must hold the singleton lock for its entire lifetime.

### 4.3 Production monolith mode is forbidden

`RUNTIME_ROLE` is explicit:

- `gateway` for `cmd/gateway` production slots;
- `worker` for `cmd/worker`;
- `monolith-dev` only for non-production local development.

When `APP_ENV=production`:

- gateway startup fails unless `RUNTIME_ROLE=gateway`;
- worker startup fails unless `RUNTIME_ROLE=worker`;
- gateway startup fails if `WORKER_RPC_URL` or the worker token is missing;
- `monolith-dev` is rejected.

This prevents an environment-variable mistake from creating two ACB pollers when both Blue and Green gateways are running.

## 5. ACB Upstream Scheduling Invariant

A single worker-side scheduler serializes access to the active ACB session. Long operations do not hold a global mutex for an entire date range.

Priority order is:

1. `INTERACTIVE_VERIFY`
2. `REALTIME_POLL`
3. `MANUAL_SYNC`
4. `KEEPALIVE`
5. `CATCH_UP`
6. `FILTER_HISTORY`

Every low-priority task is divided into bounded quanta. A quantum performs at most one ACB page request plus its associated local ingest/checkpoint work, then yields back to the scheduler. A pending higher-priority task is therefore served before the next low-priority page.

There is never concurrent access to the live ACB session from two scheduler tasks.

### 5.1 Realtime invariant

Historical work may delay a realtime task only until the current single ACB page quantum completes. It may not delay realtime until a 7-day catch-up or 31-day filter synchronization finishes.

### 5.2 Catch-up invariant

Worker startup and transition into a realtime schedule enqueue catch-up, but realtime polling is allowed to proceed. Catch-up advances coverage one completed day at a time and yields between pages.

Catch-up source remains `CATCH_UP`:

- newly discovered credit transactions create webhook/Bark deliveries;
- browser voice playback remains suppressed for old catch-up records;
- dedupe prevents duplicate events when realtime and catch-up overlap.

### 5.3 Filter-history invariant

User-requested date-range synchronization uses source `FILTER_SYNC`:

- no webhook/Bark delivery is generated;
- no voice-eligible realtime event is generated;
- database rows and coverage are updated;
- execution is asynchronous and low priority.

## 6. Durable History Job Contract

`POST /api/v1/transactions/ensure-history` is a short operation. It validates input, asks the worker to create or reuse a durable job, and returns HTTP 202.

The durable state machine is:

```text
QUEUED
  |
  v
RUNNING
  |  \
  |   \ transient failure / worker shutdown
  |    +--------------------> QUEUED
  |
  +-------------------------> COMPLETED
  |
  +-------------------------> FAILED
  |
  +-------------------------> CANCELED
```

The job stores:

- job ID;
- connection ID;
- connection generation;
- requested from/to days;
- status;
- current day;
- rows seen;
- pages completed;
- attempt count;
- next-attempt timestamp;
- error code and sanitized error message;
- created/started/heartbeat/finished/updated timestamps.

The job never persists ACB cookies, form-state fields, or navigation tokens. If the worker restarts, the current day is safely restarted from page one and dedupe absorbs duplicates.

A partial unique index prevents two active jobs for the same connection generation and date range.

A worker startup recovery pass requeues stale `RUNNING` jobs whose heartbeat is older than the configured stale threshold.

## 7. Session and Generation Fencing

All session-sensitive operations fail closed.

Before session verification:

- current connection lookup must succeed;
- requested connection ID must match;
- requested generation must equal current generation exactly;
- any database error aborts before an ACB request.

Before realtime polling:

- active-auth lookup must succeed;
- if an auth attempt is active, polling is skipped;
- if active-auth lookup fails, polling aborts locally and performs no ACB request.

All transaction ingestion retains the current generation fence in the same SQLite transaction as event/delivery creation.

## 8. Worker Shutdown and Upgrade Contract

A worker upgrade is a controlled singleton handoff, not Blue/Green overlap.

The old worker performs:

1. mark readiness false;
2. stop accepting new worker commands that create upstream work;
3. finish or cancel the current bounded upstream quantum;
4. persist the freshest ACB session snapshot using an independent bounded shutdown context;
5. stop the RPC listener;
6. release the singleton lock;
7. exit.

The new worker then:

1. starts from an immutable verified digest;
2. acquires the singleton lock;
3. opens SQLite without migrations;
4. verifies schema compatibility;
5. restores current connection/session state;
6. recovers durable background jobs;
7. starts the scheduler and dispatcher;
8. becomes ready.

If the candidate worker does not become ready, deployment restores the previous worker digest. At no time are two workers allowed to make ACB requests.

## 9. Authentication Browser Contract

An auth-browser deployment is not allowed while an auth attempt is in `STARTING`, `IN_PROGRESS`, `EXPORTING`, or `VERIFYING`.

Transient auth-browser network/5xx status errors must not mark an active attempt `FAILED`. Only a confirmed 404, terminal browser status, explicit cancel, expiry, or a failed start/handoff transition may terminally close the attempt.

The auth-browser shutdown path uses a bounded shutdown context.

## 10. Realtime Journal and SSE Contract

SQLite `event_journal` remains the durable cross-process realtime source of truth.

Each gateway:

- starts its watcher from the current maximum sequence;
- watches every 200 ms under normal conditions;
- drains batches until caught up;
- serves reconnect replay using `Last-Event-ID` / journal sequence;
- keeps the in-memory hub only as a local fan-out mechanism.

Journal retention runs in the singleton worker only.

Blue/Green promotion must not lose an event if a browser reconnects to the other slot. The new slot can always replay from SQLite.

Background journal writes that outlive an HTTP request use a short independent timeout rather than an unbounded `context.Background()` call.

## 11. Frontend Mutation Transport Contract

The frontend has exactly one mutation transport policy.

The generic `api()` layer:

- automatically obtains and attaches `X-CSRF-Token` for POST, PUT, PATCH, and DELETE;
- keeps GET/HEAD free of mutation token work;
- on `CSRF_TOKEN_INVALID`, refreshes the token and retries the mutation exactly once;
- preserves caller headers and abort signals;
- never retries arbitrary 4xx/5xx mutations.

Feature code does not manually duplicate CSRF boilerplate.

History UI behavior is:

1. enqueue job;
2. display queued/running progress;
3. poll job status at a bounded interval;
4. refetch transactions when completed;
5. surface a friendly sanitized error when failed;
6. leaving the page does not cancel the server job.

## 12. SQLite and Migration Contract

Runtime gateway/worker processes never auto-migrate.

Only `dbtool` applies migrations.

Schema changes follow expand-first compatibility:

1. backup when a schema change is actually being promoted;
2. integrity check;
3. apply additive migration;
4. verify schema version;
5. run old/new runtime compatibility tests;
6. deploy runtime components;
7. destructive contraction, if ever needed, occurs in a later release after rollback compatibility expires.

A pure gateway/UI release does not create a database backup and does not run migrations.

## 13. Secret Invariants

Production deployment scripts never generate or silently rotate secrets.

Secret generation is allowed only in an explicit fresh-bootstrap command that first proves the installation is new.

Required production secrets are validated, not created:

- `app_master_key`;
- `worker_internal_token`;
- `tts_internal_token`;
- `bark_basic_auth_user`;
- `bark_basic_auth_password`.

The application master key is immutable during normal operation. Missing key material aborts deployment.

Secret scope is least privilege:

- gateway: `app_master_key`, `worker_internal_token`, `tts_internal_token`;
- worker: `app_master_key`, `worker_internal_token`, Bark credentials;
- TTS: `tts_internal_token`;
- Bark: Bark credentials;
- auth-browser: none of the above;
- dbtool migration/check: none of the above.

Gateway-side notification tests are executed through the worker so gateways do not need Bark credentials.

Secret files are never world-readable. First-party UID 1000 containers must have an explicit preflight proving they can read only their assigned secret files.

## 14. Backup and Disaster-Recovery Contract

Routine database backup encryption is independent from `app_master_key`.

The host stores only an `age` public recipient for routine encryption. The matching private recovery identity is stored off the VPS.

A schema-changing deployment creates:

- a temporary mode-0600 SQLite online backup;
- an integrity-checked encrypted `.db.age` artifact;
- a JSON manifest with filename, byte size, SHA-256, schema version, release ID, and creation time.

The plaintext temporary database is removed immediately after successful encryption. Durable backup directories contain no plaintext `.db` snapshot.

The normal off-host hook receives only the encrypted database artifact and manifest.

Secret recovery is a separate operation. A recovery bundle streams selected secret files directly into `age`; it does not copy plaintext secrets into the backup directory. The encrypted secret bundle is stored off-host.

A restore drill must prove that an off-host recovery environment with the private age identity can decrypt, pass SQLite `PRAGMA integrity_check`, restore app secrets, and start the stack.

## 15. Production Compose Invariants

No production service has a mutable `latest` fallback.

Image interpolation fails immediately when an immutable digest is missing. Release image state is stored in a non-secret atomic file at:

`/opt/acb-transaction-webhook/deploy/.release.env`

The runtime config source is exactly:

`/opt/acb-transaction-webhook/deploy/.env.production`

There is no second competing root-level production env file.

No application service publishes a host port. Shared Traefik and Cloudflare Tunnel own ingress.

Network membership remains:

- gateway slots: `edge-acb`, `acb-core`, `acb-egress`;
- worker: `acb-core`, `acb-egress`;
- auth-browser: `acb-core`, `acb-egress`;
- TTS: `acb-core`, `acb-egress`;
- Bark: `edge-acb`, `acb-core`, `acb-egress`;
- dbtool: data volume plus isolated `none` network.

Resource and PID limits are defined in a form verified to be enforced by the installed Docker Compose version on the VPS.

## 16. Deployment Transaction Invariants

Deployments are split by responsibility.

### 16.1 Gateway promotion

A gateway-only promotion:

- does not stop/recreate worker;
- does not stop/recreate auth-browser;
- does not stop/recreate TTS;
- does not stop/recreate Bark;
- does not run a database migration;
- does not create a migration backup.

It performs:

```text
verify signed release scope
-> pull candidate digest
-> start inactive slot
-> liveness
-> deploy readiness twice consecutively
-> stage Traefik ACB route
-> atomic route replacement
-> real route identity acknowledgement
-> soak
-> stop old slot intentionally
```

### 16.2 Schema promotion

A schema promotion performs:

```text
fail-closed active-auth check
-> encrypted backup
-> backup integrity evidence
-> migration
-> schema check
-> runtime compatibility verification
```

No route changes occur if any step fails.

### 16.3 Worker promotion

A worker promotion performs the controlled singleton handoff from Section 8 and is never implicitly triggered by an unrelated gateway/UI change.

### 16.4 Auth-browser promotion

An auth-browser promotion first proves there is no active authentication attempt. Failure to determine active-auth state aborts.

### 16.5 TTS and Bark promotion

TTS and Bark have independent lifecycle scripts. Their failure cannot restart the ACB worker as a side effect.

## 17. Traefik Route Ownership and Acknowledgement

The ACB repository owns only its ACB dynamic route file in the shared `/opt/edge/dynamic` directory.

A route switch uses:

- a rendered file outside the watched directory;
- YAML syntax validation;
- an atomic rename into the watched directory;
- a saved previous route artifact for rollback.

Promotion is not considered successful merely because the file contains the target alias.

The deployment must perform a real request through the edge route and verify response identity headers from the candidate gateway:

- `X-Platform-Slot` equals the candidate slot;
- `X-Release-Commit` equals the intended release commit.

If edge acknowledgement fails, the previous dynamic route is restored atomically and the old slot identity must be acknowledged before the deployment returns failure.

## 18. Failover Controller Contract

The host failover controller may recover the stateless gateway Blue/Green service, but it does not blindly restart ACB side-effect workloads based only on generic health.

For ACB:

- gateway failover may start the warm standby and switch route after health/identity checks;
- worker recovery uses a worker-specific controlled policy and singleton lock;
- auth-browser is never restarted by deployment while an active login exists;
- intentional standby stops create cooldown/desired-state markers so the controller does not immediately undo deployment intent.

The deploy scripts and failover controller share the same per-app lock/state protocol.

## 19. Supply-Chain Invariants

Production promotion identity is always an immutable digest.

CI must:

- run frontend typecheck/build/tests;
- run Go tests with race detector and vet;
- run TTS tests;
- run shell syntax + ShellCheck;
- run failover-controller tests;
- scan first-party and approved third-party images;
- generate SBOMs;
- sign first-party images with Cosign keyless GitHub OIDC;
- generate and sign a release/promotion manifest;
- upload security evidence.

All third-party GitHub Actions are pinned by full commit SHA. Dependabot maintains those pins.

A downloaded fallback binary is never executed without cryptographic checksum verification against a pinned release checksum.

The VPS must cryptographically verify the signed promotion manifest with the exact expected GitHub Actions identity and issuer. Missing Cosign or failed signature verification aborts. A wildcard signer identity is forbidden.

## 20. Promotion Scope Invariant

A main-branch push does not imply a core restart.

CI computes a promotion scope from changed paths. The signed manifest records that scope.

At minimum:

- `web/**`, gateway HTTP/UI code -> gateway;
- monitor/ACB worker code -> worker;
- worker RPC contract -> gateway + worker;
- storage runtime code -> gateway + worker + dbtool;
- migration files -> schema + gateway + worker + dbtool;
- auth-browser code/image -> auth-browser and gateway if shared client contracts changed;
- `tts-gateway/**` -> TTS;
- deployment/platform code -> platform bundle;
- Bark digest-policy change -> Bark.

The VPS deploy controller promotes only components named in the signed scope.

## 21. Health Semantics

`/healthz` means process liveness only.

`/readyz` means the local process can serve its role.

`/internal/deployz` is the promotion gate and returns slot/release identity plus dependency status.

ACB login expiry, ACB maintenance, no recent transactions, or bank rate limiting do not make the HTTP process fail liveness.

A candidate gateway promotion requires worker reachability/schema compatibility. TTS may report degraded without blocking the bank dashboard if audio is intentionally non-critical.

## 22. Observability Contract

At minimum expose/log:

- last successful realtime poll time;
- last poll duration/status;
- scheduler queue depth by priority;
- scheduler current task kind;
- history jobs by state;
- oldest queued history-job age;
- catch-up pending/current day;
- current connection generation;
- worker readiness and drain state;
- notification pending/in-flight/dead-letter counts;
- current active gateway slot and release commit;
- last promotion result;
- backup artifact ID and last successful restore-drill date.

Logs must not contain ACB cookies, passwords, internal tokens, Cloudflare JWTs, Bark credentials, or decrypted secret envelopes.

## 23. Acceptance Gates

The architecture is considered converged only when all of these are automated or recorded as repeatable drills:

1. A 31-day history job with many mocked pages runs while realtime polls continue between history page quanta.
2. Browser cancellation during history enqueue does not leave a permanent `RUNNING` job.
3. Worker crash during history work is recovered from durable state.
4. Worker database error during generation verification causes zero ACB verifier requests.
5. Active-auth lookup failure causes zero ACB poll requests.
6. Gateway-only deployment leaves worker/auth-browser/TTS/Bark container IDs unchanged.
7. Worker promotion never has two lock-owning workers and rolls back on candidate readiness failure.
8. Active auth prevents auth-browser/core replacement.
9. Missing production secrets abort; no deploy path generates them.
10. Durable backup directory contains only encrypted DB artifacts and manifests.
11. Off-host restore drill succeeds using the recovery private key not stored on the VPS.
12. Candidate route acknowledgement proves Traefik serves the intended slot/release; a forced acknowledgement failure rolls back.
13. SSE reconnect across Blue/Green promotion replays all journal events after the browser's last sequence.
14. A missing immutable image digest makes Compose/deployment validation fail before mutation.
15. Missing Cosign on the VPS makes signed-manifest verification fail closed.
16. All repository automated test/lint/security gates pass on the final commit.

## 24. Source Areas Reviewed for This Specification

The baseline audit covered the active implementation across:

- `cmd/gateway`;
- `cmd/worker`;
- `cmd/dbtool`;
- `cmd/auth-browser`;
- `internal/acb`;
- `internal/monitor`;
- `internal/storage`;
- `internal/workerrpc`;
- `internal/httpapi`;
- `internal/auth`;
- `internal/authbrowser`;
- `internal/security`;
- `internal/webhook`;
- `internal/notification`;
- `internal/bark`;
- frontend API/query/realtime transaction paths;
- `deploy/compose.prod.yaml`;
- deployment libraries, warm deploy, verification, manifest, backup, and fresh-init scripts;
- host failover controller and ACB/auth-browser registrations;
- GitHub Actions build, test, scan, sign, and VPS promotion workflow.

This specification deliberately preserves the current correct pieces such as generation-fenced atomic ingestion, durable journal replay, Cloudflare JWT validation, CSRF middleware, DNS-aware webhook SSRF protection, delivery leasing, official-host ACB request restrictions, and immutable-digest release artifacts. The implementation plan changes only the boundaries that violate the invariants above.
