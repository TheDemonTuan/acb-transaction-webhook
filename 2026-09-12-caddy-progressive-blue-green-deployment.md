# Caddy Progressive Blue-Green Deployment & ACB Worker Isolation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Refactor `TheDemonTuan/acb-transaction-webhook` so future production releases on a single VPS are near-zero-downtime, build/test failures never disturb the active release, Caddy performs atomic traffic cutover/rollback, and ACB polling/session continuity is isolated from web/API deployments.

**Architecture:** Keep Docker Compose on one VPS. Introduce a long-lived Caddy reverse proxy, split the current monolithic gateway into a stateless-ish HTTP/UI/SSE gateway and a singleton ACB worker, share the existing SQLite WAL database/QR data volume, and run gateway releases as temporary Blue/Green slots. The inactive slot is built/pulled, started, health-checked and smoke-tested before Caddy is reloaded. The old slot stays alive during a soak/drain window for instant rollback. ACB polling, session verification and webhook dispatch remain single-owner in `acb-worker` and are not restarted for ordinary gateway releases.

**Tech Stack:** Go 1.27.1, React/Vite/Bun 1.4.2, SQLite WAL, Docker Engine + Docker Compose v2, Caddy 2.11.4, GitHub Actions, GHCR immutable image digests, Cloudflare Tunnel + Access.

**Spec:** Repository `https://github.com/TheDemonTuan/acb-transaction-webhook`, audited at commit `78e27d4a74e1447734162940aaad8a06e3f142f2` on 2026-09-12.

## Global Constraints

- [ ] Do **not** introduce Docker Swarm, Kubernetes or K3s for this one-VPS deployment.
- [ ] Use the official Caddy image; do **not** add `caddy-docker-proxy` or expose the Docker socket to Caddy.
- [ ] Production must deploy immutable `image@sha256:...` references only. `latest` must never be used by the VPS deployment path.
- [ ] A failed source test, image build, image push, migration preflight, candidate startup, health check, smoke test, Caddy validation or post-cutover soak must leave the currently active gateway serving traffic.
- [ ] Ordinary gateway/UI/API releases must **not restart** `acb-worker`, `auth-browser`, or `tts-gateway`.
- [ ] At most one process may own ACB upstream polling/session verification at a time.
- [ ] Never run Blue and Green with ACB polling enabled simultaneously.
- [ ] Preserve the existing encrypted ACB session storage and generation fencing.
- [ ] Preserve SQLite as the source of truth; do not add Redis solely for deployment.
- [ ] Keep Cloudflare Tunnel/Access as the public security boundary. No Docker service in this plan needs a public host port.
- [ ] Caddy's admin API must remain container-local and must never be published to the host or edge network.
- [ ] `/internal/*` deployment/control endpoints must be unreachable through Caddy.
- [ ] Database schema changes used during Blue/Green must obey **expand -> migrate/backfill -> contract** compatibility. No destructive migration may be applied while the previous slot is a rollback target.
- [ ] Do not terminate an interactive ACB login merely to deploy. If an `auth_attempt` is active and a browser image change is required, abort that component rollout and leave production unchanged.
- [ ] Keep the current security hardening: non-root app containers, dropped Linux capabilities, `no-new-privileges`, read-only gateway filesystem, bounded resources, log rotation, secret files rather than public environment values where possible.

---

# 1. Source Audit: What Must Change

## 1.1 Current production topology

`deploy/compose.prod.yaml` currently starts three services:

```text
gateway
  |- HTTP API
  |- React UI
  |- SSE
  |- ACB monitor/poller
  |- ACB session verifier
  `- webhook dispatcher

auth-browser
  `- Chromium/Xvfb/noVNC browser login sidecar

tts-gateway
  `- TTS service
```

The `gateway` also owns the SQLite data volume and joins `edge-acb` directly.

Source:
- `deploy/compose.prod.yaml`
- `cmd/gateway/main.go`

## 1.2 Current deploy is not zero-downtime

`deploy/deploy.sh` explicitly executes:

```bash
docker stop acb-transaction-gateway acb-auth-browser
```

before backup/migration/startup.

That means every current production deploy intentionally stops both the public gateway and auth browser before the replacement is known-good.

**Required change:** Never stop the active gateway to validate a candidate. Never stop `auth-browser` for unrelated releases.

## 1.3 Blue/Green is impossible with the current global gateway lock

`cmd/gateway/main.go` acquires an exclusive `/data/gateway.lock` for the normal process. `internal/lock/lock_posix.go` uses non-blocking `flock(LOCK_EX)`.

Therefore a second gateway sharing the same production volume cannot run beside the first one.

**Required change:**

```text
OLD
gateway.lock = "only one whole gateway process"

NEW
acb-worker.lock = "only one ACB upstream owner"
API gateways     = no global process lock
db-migrate.lock  = only migration process
```

## 1.4 Gateway is currently a monolith

`cmd/gateway/main.go` starts all of these inside one process:

- `webhook.Dispatcher`
- `monitor.Monitor`
- `SessionLoader`
- `SessionVerifier`
- in-memory `eventhub.Hub`
- HTTP API/UI/SSE

The HTTP API calls the monitor through in-process interfaces:

```go
type SyncRequester interface {
    RequestSync(context.Context) error
}

type HistoryEnsurer interface {
    EnsureHistory(context.Context, string, string) (int, error)
}

type MonitorNotifier interface {
    NotifySettingsChanged()
}

type AuthVerifier interface {
    VerifySession(context.Context, string, int64, []byte) error
}
```

**Required change:** Keep these interfaces but provide an internal HTTP `workerapi.Client` implementation from gateway to the singleton worker.

## 1.5 SSE is close to Blue/Green-ready already

`internal/httpapi/sse.go` already has:

- monotonic `event_journal.seq`
- `Last-Event-ID` replay
- retention-expired reset handling
- DB replay before live delivery

The browser uses native `EventSource`, which reconnects after transport failure.

The only process-local dependency is the in-memory `eventHub` live hint.

**Required change:** Add one `JournalWatcher` per API process. It watches SQLite `event_journal` and publishes hints into the local event hub. This avoids Redis while keeping cross-process realtime latency bounded to about 100-250 ms.

## 1.6 SQLite is suitable for the target one-VPS topology, with tests

Storage currently opens SQLite with:

```text
journal_mode=WAL
busy_timeout=5000
synchronous=FULL
foreign_keys=ON
```

That permits multiple processes to share the database, while SQLite continues to serialize writers.

The current `writeMu` is only process-local, so concurrent-process behavior must be covered by integration tests before Blue/Green is enabled.

## 1.7 Current backup path does not match the production volume

Production uses:

```yaml
gateway_data:
  name: bank-event-gateway_gateway_data
```

mounted at `/data`.

But `deploy/deploy.sh` checks:

```text
$DEPLOY_PATH/data/gateway.db
```

on the host before running `backup.sh`.

With the current named-volume production layout, those are not the same path.

**Required change:** Backup through a container that mounts the real `gateway_data` volume. Never infer or copy Docker's host-side volume path.

## 1.8 Current rollback is a redeployment, not an instant rollback

`deploy/rollback.sh` calls `deploy.sh` again with the old images.

That repeats stop/migrate/start behavior.

**Required change:** Gateway rollback becomes a Caddy route flip back to the still-running previous slot. Worker/browser/TTS rollback remains component-specific.

## 1.9 CI already has a good immutable-image foundation

The current GitHub Actions workflow already:

- runs frontend typecheck/build/tests;
- runs `go test -race ./...`;
- runs `go vet ./...`;
- builds images in GitHub Actions, not production;
- pushes GHCR images;
- deploys by digest.

Keep this foundation and replace only the production topology/cutover logic.

---

# 2. Target Production Architecture

```text
                         INTERNET
                            |
                    Cloudflare Access
                            |
                    Cloudflare Tunnel
                            |
                    internal edge-acb
                            |
                +-----------v-----------+
                |        CADDY          |
                |  stable / long-lived  |
                | alias: acb-web:8090   |
                +-----------+-----------+
                            |
                       acb-app network
                            |
             active route --+----------------------+
                            |                      |
                       +----v----+            +----v----+
                       | BLUE API|            |GREEN API|
                       | :8090   |            | :8090   |
                       +----+----+            +----+----+
                            \                    /
                             \                  /
                              +-------+--------+
                                      |
                 +--------------------+--------------------+
                 |                    |                    |
          +------v------+      +------v------+      +------v------+
          | acb-worker  |      |auth-browser |      | tts-gateway |
          | SINGLETON   |      | SINGLETON   |      | SINGLETON   |
          | :8190       |      |8181 / 6080  |      | :8081       |
          +------+------+      +-------------+      +-------------+
                 |
                 | ACB upstream only from here
                 v
              ACB ONE

All API slots + worker
        |
        v
bank-event-gateway_gateway_data
        |
   gateway.db (WAL)
   QR files
   encrypted ACB session
   event_journal
```

## Normal steady state

```text
Caddy -> Blue
Blue  = running
Green = stopped

acb-worker   = running continuously
auth-browser = running continuously
tts-gateway  = running continuously
```

## During a gateway deployment

```text
Caddy -> Blue

Blue  = current production
Green = new candidate, running but receives no public traffic

worker/browser/TTS = untouched
```

After Green passes all gates:

```text
Caddy reload:
Blue -> Green

Green = active
Blue  = kept alive for 5-10 minute soak/rollback window
```

After the soak succeeds:

```text
Caddy -> Green
Blue = stopped
```

---

# 3. Network and Runtime Layout

## 3.1 Preserve the current data volume

Continue using exactly:

```text
bank-event-gateway_gateway_data
```

Do not migrate SQLite to a new volume during the deployment refactor.

## 3.2 Networks

Create two externally named Docker networks.

### Existing edge network

```text
edge-acb
```

Rules:

- Cloudflare Tunnel and Caddy join this network.
- API slots do **not** join this network after migration.
- Move alias `acb-web` from gateway to Caddy.

### New private application network

```text
acb-app
```

Create with:

```bash
docker network create \
  --internal \
  --label io.tuan.acb.app=true \
  acb-app
```

Members:

```text
caddy
gateway-blue
gateway-green
acb-worker
auth-browser
tts-gateway
```

## 3.3 Loopback-only operational ports

Use host loopback for deployment smoke tests only:

```text
Caddy       127.0.0.1:18090 -> 8090
Blue API    127.0.0.1:18091 -> 8090
Green API   127.0.0.1:18092 -> 8090
```

Nothing binds to `0.0.0.0` on the VPS.

---

# 4. Planned File Structure

## Existing files to modify

```text
.github/workflows/deploy.yml
Dockerfile
cmd/gateway/main.go
internal/config/config.go
internal/httpapi/server.go
internal/httpapi/sse.go
internal/monitor/monitor.go
internal/storage/storage.go
deploy/backup.sh
deploy/deploy.sh
deploy/rollback.sh
deploy/verify-deployment.sh
deploy/README.md
docs/PRODUCTION_SETUP.md
.env.example
```

## New source files

```text
cmd/worker/main.go
cmd/dbtool/main.go

internal/workerapi/client.go
internal/workerapi/client_test.go
internal/workerapi/server.go
internal/workerapi/server_test.go
internal/workerapi/types.go

internal/eventhub/journal_watcher.go
internal/eventhub/journal_watcher_test.go

internal/storage/open_options.go
internal/storage/backup.go
internal/storage/backup_test.go
internal/storage/multiprocess_test.go

internal/maintenance/runner.go
internal/maintenance/runner_test.go

Dockerfile.worker

deploy/compose.core.yaml
deploy/compose.slot.yaml
deploy/compose.ops.yaml

deploy/caddy/Caddyfile
deploy/caddy/upstream-blue.caddy
deploy/caddy/upstream-green.caddy

deploy/bootstrap.sh
deploy/switch-caddy.sh
deploy/smoke-slot.sh
deploy/deploy-worker.sh
deploy/check-release.sh

deploy/tests/deploy-state-test.sh
```

Runtime-only files under the VPS deployment directory:

```text
/opt/acb-transaction-webhook/state/active-slot
/opt/acb-transaction-webhook/state/previous-slot
/opt/acb-transaction-webhook/state/blue-image
/opt/acb-transaction-webhook/state/green-image
/opt/acb-transaction-webhook/state/worker-image
/opt/acb-transaction-webhook/state/auth-browser-image
/opt/acb-transaction-webhook/state/tts-image
/opt/acb-transaction-webhook/runtime/caddy/active-upstream.caddy
/opt/acb-transaction-webhook/backups/
```

---

# 5. Task 1 - Make SQLite Safe for Multi-Process API + Worker

**Files:**
- Modify: `internal/storage/storage.go`
- Create: `internal/storage/open_options.go`
- Create: `internal/storage/multiprocess_test.go`
- Modify: `cmd/gateway/main.go`

## Steps

- [ ] Add explicit storage open options.

Target API:

```go
type OpenOptions struct {
    RunMigrations bool
}

func OpenWithOptions(
    ctx context.Context,
    path string,
    opts OpenOptions,
) (*Store, error)
```

Keep existing `Open(ctx, path)` as a compatibility helper for tests/dev:

```go
func Open(ctx context.Context, path string) (*Store, error) {
    return OpenWithOptions(ctx, path, OpenOptions{RunMigrations: true})
}
```

- [ ] In production API/worker processes, open storage with `RunMigrations: false`.
- [ ] Only `dbtool migrate` may mutate schema during production deployment.
- [ ] Remove the whole-process `gateway.lock` from the HTTP gateway startup.
- [ ] Do **not** remove `internal/lock`; it will be reused by `acb-worker.lock` and `db-migrate.lock`.
- [ ] Add a multi-process-style integration test by opening two independent `Store` handles to the same temporary SQLite file.
- [ ] Concurrently run representative operations:
  - transaction read;
  - monitor settings write;
  - audit write;
  - delivery claim/complete;
  - event journal append/read.
- [ ] Fail the test if `SQLITE_BUSY`, corruption, lost writes or duplicate delivery claims escape the storage layer under the test workload.
- [ ] Keep `busy_timeout(5000)`, WAL and `synchronous(FULL)` unless tests prove a change is necessary.

## Verification

```bash
go test -race ./internal/storage/...
go test -race ./...
go vet ./...
```

## Acceptance

- Two independent stores can operate on the same WAL DB.
- API startup no longer depends on an exclusive process lock.
- Schema migration cannot accidentally run because a Blue/Green API process started.

---

# 6. Task 2 - Add Dedicated `dbtool` for Backup, Migration and Integrity Gates

**Files:**
- Create: `cmd/dbtool/main.go`
- Create: `internal/storage/backup.go`
- Create: `internal/storage/backup_test.go`
- Modify: `Dockerfile`
- Modify: `deploy/backup.sh`

## CLI contract

```text
/dbtool backup --database /data/gateway.db --output /backups/<file>.db
/dbtool migrate --database /data/gateway.db
/dbtool check --database /data/gateway.db
/dbtool schema-version --database /data/gateway.db
/dbtool active-auth-count --database /data/gateway.db
```

## Steps

- [ ] Build `/dbtool` into the gateway production image.
- [ ] Implement an online SQLite-consistent backup using SQLite backup semantics or `VACUUM INTO`; do not byte-copy an open WAL database.
- [ ] Refuse to overwrite an existing backup file.
- [ ] Run `PRAGMA integrity_check` against the newly created backup before reporting success.
- [ ] `migrate` must acquire `/data/db-migrate.lock`.
- [ ] `migrate` runs `Store.Migrate()` exactly once and exits.
- [ ] `check` must not run migrations.
- [ ] `active-auth-count` queries active statuses:
  - `STARTING`
  - `IN_PROGRESS`
  - `EXPORTING`
  - `VERIFYING`
- [ ] Rewrite `deploy/backup.sh` so it executes `dbtool` inside a container that mounts the real Docker named volume.
- [ ] Store DB backup and the matching `app_master_key` snapshot under `/opt/acb-transaction-webhook/backups`.
- [ ] Set backup files to mode `0600`.

## Important correction

Remove the current assumption that this is the production DB:

```text
/opt/acb-transaction-webhook/data/gateway.db
```

Production source of truth is the file inside:

```text
bank-event-gateway_gateway_data:/data/gateway.db
```

## Verification

```bash
go test -race ./internal/storage/...
go test -race ./cmd/dbtool/...
```

Perform a restore drill in CI/integration test:

```text
create DB
-> seed data
-> backup
-> modify original
-> restore backup to second file
-> integrity_check
-> verify original seeded rows
```

---

# 7. Task 3 - Split the Singleton ACB Worker from the HTTP Gateway

**Files:**
- Create: `cmd/worker/main.go`
- Create: `Dockerfile.worker`
- Modify: `cmd/gateway/main.go`
- Modify: `internal/monitor/monitor.go`
- Create: `internal/maintenance/runner.go`

## Worker owns

```text
ACB monitor.Run
ACB pollOnce
ACB keepalive
ACB catch-up
SessionLoader
SessionVerifier
webhook Dispatcher
journal retention
ACB upstream mutex
acb-worker.lock
```

## API gateway owns

```text
Cloudflare Access auth
REST API
React static UI
SSE
auth-browser HTTP proxy/UI flow
TTS client
SQLite reads/admin writes
JournalWatcher
workerapi.Client
```

## Steps

- [ ] Move monitor/dispatcher initialization from `cmd/gateway/main.go` to `cmd/worker/main.go`.
- [ ] Worker acquires:

```text
/data/acb-worker.lock
```

using the existing file lock package.
- [ ] If the worker lock is unavailable, worker startup fails instead of creating a second ACB upstream owner.
- [ ] Move journal retention out of HTTP `Server` lifecycle into `internal/maintenance.Runner` and run it only from worker.
- [ ] Add worker graceful shutdown:
  1. stop accepting new sync/history/verify commands;
  2. allow current ACB request to finish;
  3. snapshot/persist the latest session if available;
  4. stop dispatcher;
  5. close DB;
  6. release `acb-worker.lock`.
- [ ] Add worker `--preflight` mode:
  - validates config;
  - loads keyring;
  - opens DB without migration;
  - checks schema/integrity prerequisites;
  - does **not** acquire the ACB worker lock;
  - does **not** call ACB;
  - does **not** dispatch webhooks;
  - exits `0` only when the image is safe to attempt as a replacement.
- [ ] Do not run `auth-browser` inside worker; it remains its own sidecar.
- [ ] Preserve existing ACB `UpstreamGate()` semantics so poll/history/session verification cannot race.

## Worker restart gap protection

- [ ] Record/read the latest successful poll.
- [ ] On worker start, if the previous successful poll is stale enough to risk coverage loss, schedule `catchUp` before resuming ordinary realtime polling.
- [ ] Preserve existing dedupe/generation fencing so catch-up cannot duplicate transaction events.

## Verification

```bash
go test -race ./internal/monitor/...
go test -race ./internal/webhook/...
go test -race ./internal/maintenance/...
go test -race ./cmd/worker/...
```

Add a test proving two worker processes cannot both own `acb-worker.lock`.

---

# 8. Task 4 - Add a Private Worker Control API

**Files:**
- Create: `internal/workerapi/types.go`
- Create: `internal/workerapi/server.go`
- Create: `internal/workerapi/client.go`
- Create: `internal/workerapi/server_test.go`
- Create: `internal/workerapi/client_test.go`
- Modify: `internal/config/config.go`
- Modify: `.env.example`
- Modify: `cmd/gateway/main.go`
- Modify: `cmd/worker/main.go`

## Internal endpoints

Worker listens on:

```text
0.0.0.0:8190
```

only on Docker network `acb-app`.

Routes:

```text
GET  /healthz
GET  /readyz

POST /internal/v1/sync
POST /internal/v1/history
POST /internal/v1/settings/reload
POST /internal/v1/session/verify
GET  /internal/v1/status
```

## Authentication

Add secret:

```text
/run/secrets/worker_internal_token
```

Environment:

```text
WORKER_CONTROL_URL=http://acb-worker:8190
WORKER_INTERNAL_TOKEN_FILE=/run/secrets/worker_internal_token
WORKER_LISTEN_ADDR=0.0.0.0:8190
```

Require:

```http
Authorization: Bearer <token>
```

for all `/internal/v1/*` endpoints.

`/healthz` and `/readyz` are reachable only on `acb-app` and may remain unauthenticated for Docker health checks.

## Interface compatibility

`workerapi.Client` must satisfy the existing gateway dependencies:

```go
SyncRequester
HistoryEnsurer
MonitorNotifier
AuthVerifier
```

This minimizes changes inside `internal/httpapi`.

## Session verification

The API may send the already-encrypted session envelope to worker for verification.

Security requirements:

- body max size;
- internal token required;
- no session envelope logging;
- no handoff/cookie logging;
- request timeout;
- response contains only sanitized status/error code.

## Monitor settings resilience

`NotifySettingsChanged()` can remain a fast hint, but worker must periodically re-read the settings revision from SQLite so a lost internal notification cannot leave stale scheduling indefinitely.

## Verification

Tests must cover:

- valid token;
- missing token;
- wrong token;
- malformed body;
- request timeout/cancellation;
- `ErrSyncUnavailable` mapping;
- history result mapping;
- verification failure sanitization;
- worker unavailable behavior.

---

# 9. Task 5 - Make Realtime SSE Cross-Process Without Redis

**Files:**
- Create: `internal/eventhub/journal_watcher.go`
- Create: `internal/eventhub/journal_watcher_test.go`
- Modify: `cmd/gateway/main.go`
- Modify: `internal/httpapi/sse.go`

## Design

Every API slot runs exactly one journal watcher:

```text
SQLite event_journal
        |
        | every 100-250 ms
        v
JournalWatcher
        |
        v
local eventHub
        |
        +--> all SSE clients connected to that API slot
```

The DB journal stays authoritative.

## Algorithm

At startup:

```text
cursor = current max event_journal seq
```

Loop:

```text
read entries after cursor
publish hints to local eventHub
advance cursor
sleep 200 ms
```

On DB error:

```text
log sanitized error
back off to max 1 second
retry
```

## Why this is safe

The existing SSE handler already drains authoritative journal rows before emitting a live hint. Therefore duplicate hints do not duplicate client events if watermark logic remains intact.

## Tests

- [ ] New journal row reaches watcher subscribers.
- [ ] Multiple rows maintain sequence order.
- [ ] Watcher restart begins at the current watermark.
- [ ] Temporary DB failure recovers.
- [ ] Two API watchers do not interfere.
- [ ] Existing `Last-Event-ID` replay tests remain green.

## Latency target

```text
journal append -> API eventHub hint <= 250 ms under normal VPS load
```

This is small compared with the current ACB poll interval and avoids operating Redis only for fan-out.

---

# 10. Task 6 - Add Deployment Readiness Separate from Liveness

**Files:**
- Modify: `internal/httpapi/server.go`
- Add tests in: `internal/httpapi/server_test.go`
- Modify: `internal/workerapi/client.go`

Keep:

```text
/healthz = process liveness
/readyz  = DB/API serving readiness
```

Add an internal deployment probe:

```text
/internal/deployz
```

It must verify:

```text
gateway process       OK
SQLite                 OK
schema version         compatible
worker control         reachable
release SHA            present
auth-browser health    reachable when configured
```

TTS failure should be reported as `degraded` rather than make the banking dashboard unavailable unless the current product requirements explicitly require TTS for core service.

Caddy must block `/internal/*`, so this endpoint is only used through the loopback Blue/Green port.

Response example:

```json
{
  "status": "ready",
  "release": "78e27d4a...",
  "database": "ready",
  "worker": "ready",
  "authBrowser": "ready",
  "tts": "ready"
}
```

Never return secrets, cookies, account numbers, database paths or tokens.

---

# 11. Task 7 - Introduce Caddy as the Stable Traffic Layer

**Files:**
- Create: `deploy/caddy/Caddyfile`
- Create: `deploy/caddy/upstream-blue.caddy`
- Create: `deploy/caddy/upstream-green.caddy`
- Create: `deploy/switch-caddy.sh`
- Create: `deploy/compose.core.yaml`

## Caddy version

Use:

```text
caddy:2.11.4-alpine
```

for the initial implementation because this is the latest verified release during this plan review and includes 2026 security fixes.

After validation on the VPS, record/pin the production image digest rather than relying indefinitely on a mutable tag.

## Base Caddyfile

Use an HTTP listener because Cloudflare Tunnel is the external HTTPS boundary:

```caddyfile
{
    auto_https off
}

:8090 {
    log {
        output stdout
        format json
    }

    @internal path /internal/*
    respond @internal 404

    import /etc/caddy/runtime/active-upstream.caddy
}
```

The active upstream snippet is one of:

```caddyfile
reverse_proxy gateway-blue:8090 {
    health_uri /readyz
    health_interval 2s
    health_timeout 1s
    health_status 200
    health_fails 2
    health_passes 2
    lb_try_duration 2s
    stream_close_delay 5m
}
```

or:

```caddyfile
reverse_proxy gateway-green:8090 {
    health_uri /readyz
    health_interval 2s
    health_timeout 1s
    health_status 200
    health_fails 2
    health_passes 2
    lb_try_duration 2s
    stream_close_delay 5m
}
```

## Caddy container rules

- [ ] Join `edge-acb`.
- [ ] Join `acb-app`.
- [ ] Set edge alias `acb-web`.
- [ ] Bind only `127.0.0.1:18090:8090` to host for ops smoke tests.
- [ ] Do not mount `/var/run/docker.sock`.
- [ ] Do not publish port `2019`.
- [ ] Persist `/data` and `/config`.
- [ ] Mount static Caddy config read-only.
- [ ] Mount runtime upstream config read-only into Caddy.
- [ ] Use `restart: unless-stopped`.
- [ ] Add a Caddy health check.

## Atomic switch procedure

`deploy/switch-caddy.sh green`:

1. validate requested slot is `blue|green`;
2. verify target slot container is healthy;
3. verify `http://127.0.0.1:18092/internal/deployz`;
4. create candidate Caddy config without modifying active config;
5. run Caddy validation;
6. copy current upstream config to rollback state;
7. atomically replace `active-upstream.caddy`;
8. run:

```bash
docker compose \
  --env-file /opt/acb-transaction-webhook/.env.production \
  -f /opt/acb-transaction-webhook/compose.core.yaml \
  exec -T caddy \
  caddy reload --config /etc/caddy/Caddyfile --adapter caddyfile
```

9. verify through `127.0.0.1:18090`;
10. only then update `state/active-slot`.

If reload fails:

- restore previous upstream file;
- leave Caddy's existing in-memory working configuration untouched;
- return non-zero;
- do not stop either gateway slot.

---

# 12. Task 8 - Split Core Compose from Blue/Green Slot Compose

**Files:**
- Create: `deploy/compose.core.yaml`
- Create: `deploy/compose.slot.yaml`
- Create: `deploy/compose.ops.yaml`
- Retire: `deploy/compose.prod.yaml` after migration

## `compose.core.yaml`

Long-lived services:

```text
caddy
acb-worker
auth-browser
tts-gateway
```

The worker and sidecars are not part of gateway slot deployment.

## `compose.slot.yaml`

One parameterized gateway service:

```text
SLOT=blue  -> network alias gateway-blue
SLOT=green -> network alias gateway-green
```

Run as separate Compose projects:

```bash
docker compose -p acb-blue  -f compose.slot.yaml ...
docker compose -p acb-green -f compose.slot.yaml ...
```

Environment:

```text
SLOT=blue|green
SLOT_HOST_PORT=18091|18092
IMAGE_REF=ghcr.io/thedemontuan/acb-transaction-webhook@sha256:...
```

Both slots mount the existing `gateway_data` volume.

Both slots receive the worker token secret.

Neither slot joins `edge-acb`.

## `compose.ops.yaml`

Operations-only one-off service:

```text
dbtool
```

Mount:

```text
gateway_data:/data
/opt/acb-transaction-webhook/backups:/backups
app_master_key read-only
```

No network is required for backup/migration/check.

## Resource policy

During normal operation:

```text
one API slot only
```

During deploy:

```text
two API slots temporarily
```

Before enabling this architecture, confirm the VPS has enough free RAM/CPU to start both API slots at once. Do not duplicate `auth-browser`, which is currently the largest service.

---

# 13. Task 9 - Rewrite `deploy.sh` as a Transactional Release State Machine

**Files:**
- Rewrite: `deploy/deploy.sh`
- Create: `deploy/smoke-slot.sh`
- Create: `deploy/check-release.sh`
- Modify: `deploy/verify-deployment.sh`
- Modify: `deploy/rollback.sh`
- Create tests: `deploy/tests/deploy-state-test.sh`

## CLI

Replace fragile positional image arguments with named arguments:

```bash
./deploy.sh \
  --release 78e27d4a74e1447734162940aaad8a06e3f142f2 \
  --gateway-image ghcr.io/thedemontuan/acb-transaction-webhook@sha256:... \
  --worker-image ghcr.io/thedemontuan/acb-transaction-webhook-worker@sha256:... \
  --auth-browser-image ghcr.io/thedemontuan/acb-transaction-webhook-auth-browser@sha256:... \
  --tts-image ghcr.io/thedemontuan/acb-transaction-webhook-tts-gateway@sha256:... \
  --changed gateway
```

Every supplied image must match:

```text
^[^[:space:]]+@sha256:[a-f0-9]{64}$
```

## Gateway deployment state machine

```text
LOCK DEPLOY
    |
READ ACTIVE SLOT
    |
TARGET = opposite slot
    |
PULL NEW IMAGE
    |
ONLINE DB BACKUP
    |
MIGRATION COMPATIBILITY GATE
    |
DB MIGRATE
    |
DB CHECK
    |
START TARGET SLOT
    |
WAIT CONTAINER HEALTH
    |
DIRECT /internal/deployz
    |
DIRECT UI SMOKE
    |
CADDY CANDIDATE CONFIG VALIDATE
    |
CADDY SWITCH
    |
CADDY-PATH SMOKE
    |
SOAK
   / \
 FAIL OK
  |    |
ROLL  RECORD SUCCESS
BACK   |
  |    |
  +    STOP OLD SLOT
```

## Rules

- [ ] Never stop the active slot before candidate health/smoke succeeds.
- [ ] Do not call `docker compose down` on the production stack.
- [ ] Do not use `--remove-orphans` across the separate core/slot projects.
- [ ] Pull all candidate images before any component restart.
- [ ] Keep the deploy flock from the current implementation.
- [ ] Record state only after successful action.
- [ ] A failed candidate is stopped/removed; active state remains unchanged.
- [ ] Keep previous gateway slot alive for `SOAK_SECONDS=300` by default.
- [ ] During soak, probe every 5 seconds.
- [ ] Require 3 consecutive failures before automatic rollback to avoid one transient causing a flip.
- [ ] After a successful soak, stop only the previous gateway slot.

## Candidate smoke tests

Against Blue/Green loopback port:

```text
GET /healthz               -> 200
GET /readyz                -> 200
GET /internal/deployz      -> ready
GET /                       -> 200 and text/html
GET a known built asset    -> 200
```

Do not create/cancel ACB login sessions as a smoke test.

## Post-switch smoke

Through Caddy loopback:

```text
http://127.0.0.1:18090/healthz
http://127.0.0.1:18090/readyz
http://127.0.0.1:18090/
```

Optional final public smoke:

```text
https://bank.tuannguyenviet.site/
```

must use the normal Cloudflare Access path and must not bypass Access.

---

# 14. Task 10 - Make Rollback a Traffic Flip

**Files:**
- Rewrite: `deploy/rollback.sh`
- Reuse: `deploy/switch-caddy.sh`

## During soak

Rollback is:

```text
Caddy Green -> Blue
```

or:

```text
Caddy Blue -> Green
```

No image build.
No DB migration.
No gateway recreation.

Expected rollback path:

```bash
./rollback.sh --gateway
```

Algorithm:

1. read `state/previous-slot`;
2. verify previous slot is healthy;
3. validate previous Caddy config;
4. reload Caddy to previous slot;
5. verify Caddy;
6. mark previous slot active;
7. leave failed slot running for logs unless disk/RAM pressure requires stopping it.

## After old slot has been retired

A historical rollback may need to recreate the previous digest into the inactive slot.

That path must still:

```text
start inactive -> health -> smoke -> Caddy switch
```

It must never revert by stopping current production first.

## Database limitation

If a release applied a destructive schema migration, instant code rollback is unsafe.

Therefore destructive migrations are prohibited during the rollback window and must use the expand/contract process defined later in this plan.

---

# 15. Task 11 - Add Safe Independent Worker Deployment

**Files:**
- Create: `deploy/deploy-worker.sh`
- Modify: `cmd/worker/main.go`
- Modify: `internal/monitor/monitor.go`
- Modify: `deploy/compose.core.yaml`

Gateway deployments must not touch the worker.

When worker code itself changes:

## Procedure

1. pull new worker digest while old worker keeps polling;
2. run candidate:

```text
/worker --preflight
```

against the real config/DB but with **zero ACB requests**;
3. verify `auth-browser` remains healthy;
4. request graceful drain from current worker or send SIGTERM;
5. current worker finishes its in-flight ACB request and persists session;
6. replace only the worker container using the already-pulled digest;
7. new worker acquires `acb-worker.lock`;
8. new worker restores encrypted session from SQLite;
9. worker readiness must pass;
10. confirm a successful ACB poll within the configured readiness window;
11. if worker fails before successful activation, restart the recorded previous worker digest.

## Downtime characteristic

This can create a short **polling gap**, but not public web downtime.

The target is:

```text
worker stop -> new worker ready < 5 seconds
```

because:

- image is already pulled;
- binary is preflighted;
- session is persisted;
- database already exists;
- auth-browser is not restarted.

A catch-up/dedupe path protects transaction coverage if the gap becomes longer.

## Future optional enhancement

Only if real worker restarts still threaten the ACB session, implement active/passive worker handoff:

```text
old ACTIVE owns acb-worker.lock
new STANDBY validates only
old drains/releases lock
new acquires lock and becomes ACTIVE
```

Do not implement dual-active polling.

---

# 16. Task 12 - Stop Restarting Auth Browser and TTS on Every Release

**Files:**
- Modify: `.github/workflows/deploy.yml`
- Modify: `deploy/deploy.sh`
- Modify: `deploy/compose.core.yaml`

## Auth browser

Only update when its image/source changed.

Before an auth-browser update:

```bash
/dbtool active-auth-count ...
```

must return:

```text
0
```

If not zero:

```text
ABORT AUTH-BROWSER COMPONENT UPDATE
KEEP CURRENT BROWSER
KEEP CURRENT PRODUCTION
```

Do not kill an interactive login.

## TTS

Deploy TTS independently.

A TTS failure must not stop the ACB worker or active gateway.

## Result

A frontend-only release should perform approximately:

```text
build gateway
start inactive gateway
smoke
Caddy flip
soak
stop old gateway
```

and nothing else.

---

# 17. Task 13 - Refactor GitHub Actions into Build -> Verify -> Publish -> Promote

**Files:**
- Rewrite relevant parts: `.github/workflows/deploy.yml`
- Optionally add: `.github/workflows/ci.yml` checks for deployment files

## Pipeline

```text
verify
  |
  +-- gateway image
  +-- worker image if affected
  +-- auth-browser image if affected
  `-- tts image if affected
          |
          v
publish immutable digests
          |
          v
production deploy
```

## Verify job

Keep and extend:

```bash
cd web
bun install --frozen-lockfile
bunx --bun tsc --noEmit
bunx --bun vite build
bunx --bun vitest run

cd ..
go test -race ./...
go vet ./...
bash -n deploy/*.sh
git diff --check
```

Add:

```bash
shellcheck deploy/*.sh
```

and Compose config validation for core and both slot values.

## Caddy CI validation

Generate a test active-upstream file and run the same Caddy version used in production to validate the config.

## Component change classification

Do not restart components merely because the workflow ran.

Classify changed paths.

### Gateway affected by

```text
web/**
cmd/gateway/**
internal/httpapi/**
internal/auth/**
internal/config/**
internal/eventhub/**
internal/storage/**
Dockerfile
go.mod
go.sum
```

### Worker affected by

```text
cmd/worker/**
internal/acb/**
internal/monitor/**
internal/webhook/**
internal/workerapi/**
internal/maintenance/**
internal/security/**
internal/storage/**
Dockerfile.worker
go.mod
go.sum
```

### Auth browser affected by

```text
cmd/auth-browser/**
internal/authbrowser/**
Dockerfile.auth-browser
```

### TTS affected by

```text
tts-gateway/**
```

Shared code may mark more than one component.

For `workflow_dispatch`, allow an explicit `deploy_all=true` path.

## Image policy

- Push SHA tag for human inspection.
- Deploy digest only.
- Prefer not publishing `latest`; if retained, deployment scripts must reject it.
- Add OCI revision/source labels.
- Enable BuildKit provenance/SBOM where supported.

## Production environment

Keep:

```text
environment: production
concurrency:
  group: acb-transaction-webhook-production
  cancel-in-progress: false
```

Production GitHub environment approval remains recommended.

---

# 18. Task 14 - Caddy + Cloudflare Tunnel One-Time Cutover

This is a one-time migration from the current direct-gateway ingress.

## Target

Cloudflare Tunnel must ultimately reach:

```text
http://acb-web:8090
```

where `acb-web` is **Caddy**, not the gateway container.

## Safe migration order

- [ ] Create `acb-app`.
- [ ] Create/verify `worker_internal_token`.
- [ ] Pull all new images.
- [ ] Perform an online backup of the real named volume.
- [ ] Apply only backward-compatible migrations.
- [ ] Start new API slot in API-only mode while the old monolithic gateway is still running.
- [ ] Start Caddy with a unique temporary edge alias, for example `acb-caddy`.
- [ ] Smoke the new API directly through loopback.
- [ ] Smoke Caddy -> new API through loopback.
- [ ] Move Cloudflare Tunnel ingress to Caddy.
- [ ] Confirm dashboard/API/SSE through Cloudflare Access.
- [ ] Stop the old monolithic gateway.
- [ ] Immediately start singleton `acb-worker`.
- [ ] Confirm worker restores the stored ACB session and resumes polling.
- [ ] Leave `auth-browser` running throughout the transition.
- [ ] After validation, move compatibility alias `acb-web` to Caddy if needed and remove the temporary alias.

## Important limitation

Because the currently deployed binary combines the HTTP server and ACB worker under one exclusive `gateway.lock`, the **first architecture conversion cannot be identical to a normal future Blue/Green deployment**.

The plan minimizes the first cutover by pre-starting the new API and Caddy before stopping the legacy monolith. There may be a very short ACB polling handoff gap while the legacy monolith stops and `acb-worker` starts.

After this one-time migration, normal gateway deploys no longer interrupt ACB polling.

---

# 19. Task 15 - Make Database Migrations Blue/Green-Compatible

**Files:**
- Modify: `internal/storage/storage.go`
- Add: `internal/storage/migration_contract_test.go`
- Document: `docs/PRODUCTION_SETUP.md`

## Allowed during ordinary Blue/Green release

Examples:

```text
ADD nullable column
ADD column with backward-compatible default
CREATE TABLE
CREATE INDEX
backfill data without removing old representation
```

## Forbidden while old slot remains rollbackable

Examples:

```text
DROP TABLE
DROP COLUMN
RENAME COLUMN consumed by old binary
change type incompatibly
make nullable column mandatory before old code writes it
remove enum/value old binary still emits
```

## Expand/contract pattern

Release N:

```text
add new schema
new code understands old + new
```

Release N+1:

```text
backfill
all writers use new schema
```

Release N+2, after rollback window has expired:

```text
remove old schema
```

## CI guard

Add a migration contract test that fails ordinary deployment migrations containing known destructive SQLite patterns unless the change is explicitly moved to a separately reviewed contract-release workflow.

The guard is defense-in-depth, not a substitute for review.

---

# 20. Task 16 - Soak Monitoring and Automatic Rollback

**Files:**
- Create/extend: `deploy/check-release.sh`
- Modify: `deploy/deploy.sh`
- Modify: `deploy/verify-deployment.sh`

Default:

```text
SOAK_SECONDS=300
PROBE_INTERVAL_SECONDS=5
FAILURE_THRESHOLD=3
```

During soak check:

```text
candidate container health
Caddy /healthz
Caddy /readyz
Caddy homepage
worker /readyz
worker restart count
Caddy restart count
```

If any core gateway check fails three consecutive times:

```text
switch Caddy to previous slot
mark release failed
retain failed slot logs
exit non-zero
```

Do **not** roll back the ACB worker just because a gateway release failed if the worker was not part of that release.

## Phase-2 metric gates

After baseline deployment is stable, add metrics-based rollback gates:

```text
HTTP 5xx rate
p95 latency
SSE disconnect/reconnect rate
gateway restart count
worker poll failure rate
ACB 429 rate
webhook dead-letter growth
```

Do not introduce Prometheus/Grafana solely to finish the first Blue/Green refactor if the VPS budget is tight; the health/soak gates above are the initial requirement.

---

# 21. Task 17 - Test Failure Modes Before Production

Run these drills on a staging copy of the database or a controlled VPS window.

## Drill A - Image does not exist

Expected:

```text
pull fails
active slot untouched
Caddy untouched
worker untouched
```

## Drill B - Candidate process crashes

Expected:

```text
inactive slot unhealthy
no Caddy switch
active slot untouched
```

## Drill C - Candidate `/internal/deployz` fails worker connectivity

Expected:

```text
deployment aborts
active slot untouched
```

## Drill D - Bad Caddy configuration

Expected:

```text
caddy validate fails
running Caddy configuration remains active
active slot untouched
```

## Drill E - Post-switch gateway crash

Expected:

```text
soak detects failure
Caddy flips to previous slot
previous slot already running
```

## Drill F - SSE active during cutover

Expected:

```text
SSE either stays connected during drain
or reconnects automatically
Last-Event-ID/journal replay loses no event
```

## Drill G - ACB login active while browser image change requested

Expected:

```text
auth-browser update aborted
active login not killed
production gateway remains available
```

## Drill H - Gateway-only deployment

Expected:

```text
ACB worker container ID unchanged
auth-browser container ID unchanged
tts container ID unchanged
last ACB poll cadence unaffected
```

## Drill I - Worker replacement

Expected:

```text
new image preflight passes before old stops
old session persisted
new worker restores session
no dual ACB requests
polling resumes
catch-up/dedupe covers gap
```

## Drill J - Database backup restore

Expected:

```text
backup integrity check passes
restored DB opens
encrypted session rows remain readable with paired master key
transaction/event counts match snapshot
```

---

# 22. Task 18 - Update Operations Documentation

**Files:**
- Rewrite: `deploy/README.md`
- Rewrite: `docs/PRODUCTION_SETUP.md`
- Update: `.env.example`

The current docs contain stale statements about auth-browser not being deployed and backup behavior.

Document the real architecture:

```text
Cloudflare -> Caddy -> active API slot
                    -> shared SQLite
             worker -> ACB
```

Include runbooks:

```text
normal gateway deploy
manual gateway rollback
worker deploy
auth-browser deploy
TTS deploy
Caddy validation/reload
backup
restore
inspect active slot
inspect image digests
incident: worker unhealthy
incident: ACB session AUTH_REQUIRED
incident: bad release after soak
```

Commands must use the new core/slot Compose files and never instruct operators to run `docker compose down` for ordinary releases.

---

# 23. Proposed Caddy/Compose Operational Commands

## Show active slot

```bash
cat /opt/acb-transaction-webhook/state/active-slot
```

## Inspect Blue

```bash
docker compose \
  -p acb-blue \
  --env-file /opt/acb-transaction-webhook/.env.production \
  -f /opt/acb-transaction-webhook/compose.slot.yaml \
  ps
```

## Inspect Green

```bash
docker compose \
  -p acb-green \
  --env-file /opt/acb-transaction-webhook/.env.production \
  -f /opt/acb-transaction-webhook/compose.slot.yaml \
  ps
```

## Inspect core

```bash
docker compose \
  --env-file /opt/acb-transaction-webhook/.env.production \
  -f /opt/acb-transaction-webhook/compose.core.yaml \
  ps
```

## Validate Caddy

```bash
docker compose \
  --env-file /opt/acb-transaction-webhook/.env.production \
  -f /opt/acb-transaction-webhook/compose.core.yaml \
  exec -T caddy \
  caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile
```

## Graceful Caddy reload

```bash
docker compose \
  --env-file /opt/acb-transaction-webhook/.env.production \
  -f /opt/acb-transaction-webhook/compose.core.yaml \
  exec -T caddy \
  caddy reload --config /etc/caddy/Caddyfile --adapter caddyfile
```

Do not restart Caddy to change the active slot.

---

# 24. Security Checklist

- [ ] Caddy admin endpoint is not published.
- [ ] Docker socket is not mounted into Caddy or app containers.
- [ ] `acb-worker:8190` is only on internal `acb-app`.
- [ ] Worker control token is random, file-backed, `0600`, not committed.
- [ ] Session envelopes are encrypted before worker-control transport and never logged.
- [ ] Cloudflare Access JWT verification stays in gateway.
- [ ] `auth-browser` VNC/CDP/RPC remain non-public.
- [ ] Caddy denies `/internal/*`.
- [ ] Loopback ops ports bind to `127.0.0.1` only.
- [ ] App master key retains `0600`.
- [ ] Backups retain `0600`.
- [ ] GHCR image digests are immutable.
- [ ] Caddy version is pinned and upgraded for security releases deliberately.
- [ ] No deployment script accepts whitespace-bearing image references or shell-injectable release identifiers.
- [ ] SSH host key pinning from the current workflow remains.
- [ ] Production deploy user remains non-root and has only the Docker/deployment permissions actually required.
- [ ] Log output never includes cookies, ACB form state, session envelope plaintext, webhook secrets or worker token.

---

# 25. Performance and Resource Expectations

This design is intentionally optimized for one VPS.

## Steady state

Only one gateway slot runs:

```text
Caddy       small, always on
Gateway     1 active
Worker      1
Auth browser 1
TTS         1
```

## Deployment window

Only the gateway is duplicated:

```text
Gateway Blue  + Gateway Green
```

The expensive Chromium auth-browser is **not** duplicated.

## Expected traffic cutover

Caddy reload is the traffic switch, not a container restart.

Target:

```text
new HTTP request routing gap: effectively zero
rollback route flip: seconds or less
ACB poll interruption on gateway-only deploy: zero
```

Worker-code releases may have a short worker handoff gap but must not cause HTTP downtime or transaction loss.

---

# 26. Recommended Implementation Order

Implement in this exact order:

```text
1. SQLite open/migration separation
2. dbtool backup/migrate/check
3. worker process split
4. worker control API/client
5. journal watcher
6. deployment readiness endpoint
7. core/slot Compose topology
8. Caddy config + safe switch script
9. Blue/Green deploy state machine
10. instant gateway rollback
11. independent worker deployment
12. path-aware GitHub Actions
13. one-time production migration
14. failure drills
15. documentation cleanup
16. metrics-based progressive gates
```

Do not attempt Caddy Blue/Green before tasks 1-6 are complete; the existing monolith/global lock would make the result unsafe.

---

# 27. Definition of Done

The project is considered successfully migrated only when all statements below are true.

- [ ] A broken GitHub build cannot affect running production.
- [ ] A nonexistent/broken candidate image cannot affect active production.
- [ ] Green can run next to Blue against the production DB.
- [ ] Only the worker can talk to ACB for polling/history/session verification.
- [ ] Gateway-only deployment does not restart the worker.
- [ ] Gateway-only deployment does not restart auth-browser.
- [ ] Gateway-only deployment does not restart TTS.
- [ ] Caddy switches traffic only after candidate health + smoke gates.
- [ ] Caddy reload failure leaves the previous config serving.
- [ ] Previous gateway remains available during the rollback window.
- [ ] Rollback during soak is only a Caddy traffic flip.
- [ ] SSE reconnect/replay loses no transaction event during cutover.
- [ ] Worker cannot run dual-active.
- [ ] Worker session is persisted before intentional replacement.
- [ ] SQLite backup uses the real production named volume.
- [ ] Backup restore drill succeeds with the paired master key.
- [ ] Destructive DB migration cannot accidentally break the rollback slot.
- [ ] Cloudflare Tunnel reaches Caddy, not a gateway slot.
- [ ] No Caddy admin/Docker API/internal worker API is public.
- [ ] All deployment scripts pass shell syntax/static checks.
- [ ] Full Go race tests and frontend tests pass.
- [ ] Five-minute post-cutover soak automatically rolls back a broken gateway.

---

# 28. Source Evidence Reviewed

Repository baseline:

```text
https://github.com/TheDemonTuan/acb-transaction-webhook
commit 78e27d4a74e1447734162940aaad8a06e3f142f2
```

Key files audited:

```text
deploy/compose.prod.yaml
deploy/deploy.sh
deploy/rollback.sh
deploy/backup.sh
deploy/verify-deployment.sh
.github/workflows/deploy.yml
cmd/gateway/main.go
internal/lock/lock_posix.go
internal/storage/storage.go
internal/monitor/monitor.go
internal/monitor/session.go
internal/monitor/verifier.go
internal/webhook/dispatcher.go
internal/httpapi/server.go
internal/httpapi/sse.go
web/src/realtime/realtime.client.ts
Dockerfile
Dockerfile.auth-browser
docs/PRODUCTION_SETUP.md
deploy/README.md
```

Caddy references:

```text
https://caddyserver.com/docs/getting-started
https://caddyserver.com/docs/command-line
https://caddyserver.com/docs/caddyfile/directives/reverse_proxy
https://caddyserver.com/docs/running
https://github.com/caddyserver/caddy/releases/tag/v2.11.4
```

Relevant Caddy properties used by this design:

- graceful zero-downtime `caddy reload`;
- failed new config keeps/rolls back to the previous working config;
- active upstream health checks;
- immediate flushing for `text/event-stream`;
- streaming close delay to reduce reconnect storms;
- Docker Compose operation without Docker-socket discovery.

---

# 29. Self-Review

## Correctness

- [x] Plan accounts for the current exclusive `gateway.lock`.
- [x] Plan prevents dual ACB pollers.
- [x] Plan accounts for in-process monitor interfaces.
- [x] Plan accounts for the in-memory SSE live hint.
- [x] Plan preserves event journal replay.
- [x] Plan identifies the current production named-volume/backup-path mismatch.
- [x] Plan changes rollback from redeploy to route flip.
- [x] Plan isolates auth-browser from ordinary app deploys.
- [x] Plan preserves immutable digest deployment.
- [x] Plan explicitly handles database compatibility.

## Scope discipline

- [x] No Swarm/K3s.
- [x] No Redis required.
- [x] No Docker-socket Caddy plugin.
- [x] No duplicated Chromium browser during Blue/Green.
- [x] New infrastructure is limited to Caddy plus one logical worker split.

## Deployment safety

- [x] Candidate validation occurs before traffic switch.
- [x] Active release remains untouched on pre-switch failures.
- [x] Old slot remains available during soak.
- [x] Core stateful services are independent of gateway slot lifecycle.
- [x] First migration from the legacy monolith is called out as a special cutover rather than falsely claiming it can already be perfect zero-downtime.

## Final architectural decision

Use:

```text
Docker Compose
+ Caddy
+ Progressive Blue/Green gateway slots
+ singleton ACB worker
+ SQLite WAL
+ immutable GHCR digests
+ health/smoke/soak gates
+ instant Caddy rollback
```

Do **not** add Docker Swarm for the current one-VPS system.

The most important refactor is not the Caddyfile itself. It is separating ACB ownership from the public gateway so traffic releases and banking-session continuity have independent lifecycles.
