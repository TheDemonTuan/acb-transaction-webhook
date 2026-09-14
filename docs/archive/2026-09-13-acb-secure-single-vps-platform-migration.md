> **SUPERSEDED / HISTORICAL ARCHIVE**
>
> This document is retained solely for historical context, audit trails, and design lineage.
> It has been superseded by the canonical 2026-09-14 production architecture and hardening specifications:
> - **Canonical Specification:** [`docs/superpowers/specs/2026-09-14-acb-final-production-invariants.md`](docs/superpowers/specs/2026-09-14-acb-final-production-invariants.md)
> - **Production Architecture:** [`docs/architecture/PRODUCTION_ARCHITECTURE.md`](docs/architecture/PRODUCTION_ARCHITECTURE.md)
> - **Execution Plan & Tracker:** [`docs/superpowers/plans/2026-09-14-acb-production-convergence-execution.md`](docs/superpowers/plans/2026-09-14-acb-production-convergence-execution.md)
>
> Do not implement, deploy, or operate against this document.

---

# ACB Secure Single-VPS Platform Migration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Migrate `TheDemonTuan/acb-transaction-webhook` to the reusable Single-VPS Secure Container Platform v1 so gateway deployments are Active/Standby Blue-Green behind the shared Caddy, ACB polling/session state is isolated in a singleton worker, production attack surface is minimized, and the same deployment/security contract can be applied to later repositories.

**Architecture:** Keep Docker Compose on one VPS and keep the existing shared `/opt/edge` Cloudflare Tunnel + Caddy model. Split the current monolithic Go gateway into W1 HTTP/API/SSE Blue/Green slots and a W2 singleton ACB/notification worker, then isolate public/core/egress networks, preserve SQLite WAL as the source of truth, and use shared Caddy `lb_policy first` health-aware primary/standby routing. CI builds, scans, signs and publishes immutable image digests; the VPS only verifies, pulls, preflights, starts an inactive slot, promotes through Caddy, and rolls back by route-order flip.

**Tech Stack:** Go 1.27.1, React/Vite/Bun 1.4.2, SQLite WAL, Docker Engine + Docker Compose v2, Caddy v2.11.4 shared edge, Cloudflare Tunnel/Access, GitHub Actions/GHCR, Trivy 0.74.0, Sigstore/Cosign v3.x.

**Spec:** `docs/superpowers/specs/2026-09-13-single-vps-secure-container-platform-standard.md`

## Global Constraints

- Repository baseline for this plan: commit `e046a01a13c4f5e7fae9f6b937a6372c6a156664` dated 2026-09-13.
- Do not add Docker Swarm, Kubernetes, K3s, Nomad or a service mesh for this one-VPS migration.
- Production keeps one shared edge stack at `/opt/edge`; this repository must not run its own Caddy or cloudflared.
- Do not mount `/var/run/docker.sock` into Caddy, gateway, worker, auth-browser, TTS, Bark, monitoring or autoheal containers.
- Do not publish production app ports on `0.0.0.0` or `::`.
- Build/test/scan/sign happen before production; the VPS does not compile source during a release.
- Production deployment identity is always `image@sha256:$DIGEST_HEX`; mutable tags are not accepted by deploy scripts.
- Gateway Blue and Green may run concurrently; ACB upstream polling/session verification remains exactly-one-active through `acb-worker`.
- Ordinary gateway release must not restart `acb-worker`, `auth-browser`, `tts-gateway` or Bark.
- Auth-browser must not be restarted while an interactive ACB auth attempt is active.
- SQLite remains the source of truth for this migration; do not add Redis only to support Blue/Green or SSE fan-out.
- Database schema changes must follow expand/backfill/contract compatibility while previous gateway remains rollbackable.
- Keep current Cloudflare Access JWT validation and do not weaken auth to make Blue/Green easier.
- Keep ACB rate-limit/circuit-breaker/session-generation/dedupe protections.
- No task may run `docker compose down` against the whole production application during a normal release.
- Any destructive host-wide hardening change such as rootless Docker or `userns-remap` is a separate later project, not part of the first ACB cutover.

---

# 0. Source Audit and Migration Decisions

## 0.1 Current source baseline

The latest audited production Compose at `e046a01` contains:

```text
gateway
  HTTP/API/UI/SSE
  ACB monitor
  session verifier
  notification dispatcher

auth-browser

tts-gateway

bark
```

`gateway` currently joins both the default app network and `edge-acb`, while Bark joins both as well. `auth-browser` and TTS stay off the edge network.

Positive hardening already present and must be preserved:

```text
restart: unless-stopped
non-root gateway/auth-browser/TTS
read_only for gateway/TTS/Bark
cap_drop: ALL
no-new-privileges
custom auth-browser seccomp
health checks
memory/CPU/PID limits
bounded json-file logs
immutable Bark digest requirement
```

## 0.2 Current critical deployment problem

`deploy/deploy.sh` still executes the equivalent of:

```bash
docker stop acb-transaction-gateway acb-auth-browser
```

before backup/migration/start.

Therefore the current pipeline cannot guarantee zero/near-zero downtime and unnecessarily kills the browser service on releases unrelated to browser code.

## 0.3 Current architectural blocker to Blue/Green

`cmd/gateway/main.go` currently:

```text
acquires exclusive gateway.lock
opens SQLite
starts notification dispatcher
starts ACB monitor
starts session verifier
starts HTTP API/UI/SSE
```

Two gateway slots cannot safely run this code concurrently because:

1. the exclusive `gateway.lock` rejects the second process;
2. removing the lock without refactor would create two ACB pollers;
3. two notification dispatch loops would be unnecessary;
4. in-memory `eventhub` only sees events published in the same process.

**Decision:** Split external side-effect ownership into `cmd/worker` before enabling Blue/Green.

## 0.4 Current CI status

`.github/workflows/deploy.yml` already has a useful foundation:

```text
frontend typecheck/build/test
Go race tests + vet
TTS tests
Buildx
GHCR push
immutable digest outputs
production environment
serialized deploy concurrency
pinned SSH known_hosts
```

But it currently:

- builds all major app images for every push;
- also publishes `latest` tags;
- does not scan/sign images;
- deploys gateway/auth-browser/TTS together;
- has no Blue/Green slot promotion model.

## 0.5 Existing reusable ingress standard

`EDGE_INGRESS_RULES.md` already requires:

```text
one shared Caddy
one shared Cloudflare Tunnel
per-app internal edge network
no production host ports
private app networks
```

This plan does not replace that with per-repo Caddy. It expands that document into the new Platform Standard v1 and updates ACB to comply fully.

---

# 1. Target Architecture

```text
                                   Internet
                                      |
                              Cloudflare Edge
                         Access / WAF / DDoS
                                      |
                              Cloudflare Tunnel
                                      |
                           /opt/edge cloudflared
                                      |
                               /opt/edge Caddy
                                      |
                 +--------------------+--------------------+
                 |                                         |
           edge-acb (internal)                       other app edges
                 |
          +------+-------+
          |              |
   acb-web-blue     acb-web-green
    W1 gateway        W1 gateway
          |              |
          +------+-------+
                 |
          acb-core (internal)
      +----------+-----------+--------------+
      |          |           |              |
 acb-worker   auth-browser  tts-gateway    bark
   W2          W4            W1-ish         W5
      |          |           |              |
      +----------+-----------+--------------+
                 |
             acb-egress
          outbound-capable
                 |
          +------+------+---------+
          |      |      |         |
         ACB   APNs    TTS       CF JWKS /
                        APIs      VietQR /
                                  webhook targets

Shared local state:
  bank-event-gateway_gateway_data
    - gateway.db (WAL)
    - encrypted ACB session
    - event_journal
    - QR files

  bank-event-gateway_bark_data
    - Bark state
```

## Normal runtime

```text
Caddy order: Green first, Blue second
Green = PRIMARY
Blue  = HOT STANDBY

acb-worker = singleton ACTIVE
```

## Next release

```text
1. Update Blue only.
2. Verify Blue.
3. Caddy order becomes Blue first, Green second.
4. Keep Green running as standby.
```

Release after that updates Green.

---

# 2. File Structure Locked by This Plan

## New files

```text
cmd/worker/main.go
cmd/dbtool/main.go

internal/workerapi/client.go
internal/workerapi/client_test.go
internal/workerapi/server.go
internal/workerapi/server_test.go
internal/workerapi/types.go

internal/realtime/journal_watcher.go
internal/realtime/journal_watcher_test.go

internal/maintenance/runner.go
internal/maintenance/runner_test.go

internal/storage/open_options.go
internal/storage/multiprocess_test.go
internal/storage/migration_contract_test.go

Dockerfile.worker

deploy/compose.core.yaml
deploy/compose.slot.yaml
deploy/compose.ops.yaml

deploy/edge/acb.route.caddy
deploy/edge/acb.route.test.caddy

deploy/preflight.sh
deploy/deploy-gateway.sh
deploy/deploy-worker.sh
deploy/deploy-auth-browser.sh
deploy/deploy-tts.sh
deploy/deploy-bark.sh
deploy/switch-slot.sh
deploy/smoke-slot.sh
deploy/verify-release.sh
deploy/check-host.sh
deploy/verify-image-signature.sh

deploy/tests/deploy-state-test.sh
deploy/tests/network-policy-test.sh
deploy/tests/secret-permission-test.sh

scripts/verify-actions-pinned.sh

.github/dependabot.yml

docs/SECURITY_ARCHITECTURE.md
docs/ROLLBACK_RUNBOOK.md
docs/RESTORE_RUNBOOK.md
```

## Existing files to modify

```text
.github/workflows/deploy.yml
.env.example
Dockerfile
Dockerfile.auth-browser
EDGE_INGRESS_RULES.md
cmd/gateway/main.go
internal/config/config.go
internal/httpapi/server.go
internal/httpapi/sse.go
internal/storage/storage.go
internal/monitor/monitor.go
internal/notification/dispatcher.go
deploy/deploy.sh
deploy/rollback.sh
deploy/verify-deployment.sh
deploy/backup.sh
deploy/compose.prod.yaml
deploy/README.md
docs/PRODUCTION_SETUP.md
```

## Files to retire only after production cutover succeeds

```text
deploy/compose.prod.yaml
deploy/deploy.sh       # replaced by component-specific deploy entrypoints
deploy/rollback.sh     # replaced by slot/component rollback behavior
```

Keep them during the rollback window; do not delete in the same commit that introduces the new architecture.

---

# Phase A — Make the application safe to run as multiple processes

### Task 1: Separate schema migration from runtime storage open

**Files:**
- Create: `internal/storage/open_options.go`
- Modify: `internal/storage/storage.go`
- Test: `internal/storage/multiprocess_test.go`

**Interfaces:**
- Produces:

```go
type OpenOptions struct {
    RunMigrations bool
}

func OpenWithOptions(ctx context.Context, path string, opts OpenOptions) (*Store, error)
```

- Keeps compatibility:

```go
func Open(ctx context.Context, path string) (*Store, error)
```

with `RunMigrations: true` for existing tests/dev call sites until production entrypoints are migrated.

- [ ] **Step 1: Write the failing runtime-open test**

Create `internal/storage/multiprocess_test.go` with a test that migrates once, then opens the same DB twice with `RunMigrations: false` and verifies both handles can read the same connection state.

```go
func TestOpenWithOptions_RuntimeDoesNotMigrate(t *testing.T) {
    ctx := context.Background()
    path := filepath.Join(t.TempDir(), "gateway.db")

    seed, err := OpenWithOptions(ctx, path, OpenOptions{RunMigrations: true})
    if err != nil { t.Fatal(err) }
    seed.Close()

    a, err := OpenWithOptions(ctx, path, OpenOptions{RunMigrations: false})
    if err != nil { t.Fatal(err) }
    defer a.Close()

    b, err := OpenWithOptions(ctx, path, OpenOptions{RunMigrations: false})
    if err != nil { t.Fatal(err) }
    defer b.Close()

    if err := a.Health(ctx); err != nil { t.Fatal(err) }
    if err := b.Health(ctx); err != nil { t.Fatal(err) }
}
```

- [ ] **Step 2: Run the test and confirm it fails because the API does not exist**

```bash
go test ./internal/storage -run TestOpenWithOptions_RuntimeDoesNotMigrate -v
```

Expected: compile failure mentioning `OpenWithOptions`/`OpenOptions` undefined.

- [ ] **Step 3: Implement `OpenWithOptions`**

Move current connection setup into `OpenWithOptions`; only call `s.Migrate(ctx)` when `opts.RunMigrations` is true.

Keep exact SQLite pragmas currently used:

```text
busy_timeout=5000
foreign_keys=1
journal_mode=WAL
synchronous=FULL
```

- [ ] **Step 4: Add a two-handle writer stress test**

Open two independent stores to the same file and concurrently exercise representative writes and journal reads. The test must fail on unhandled `SQLITE_BUSY`, duplicate claims or lost committed rows.

- [ ] **Step 5: Run storage race tests**

```bash
go test -race ./internal/storage/...
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/storage/open_options.go internal/storage/storage.go internal/storage/multiprocess_test.go
git commit -m "refactor(storage): separate runtime open from migrations"
```

---

### Task 2: Extract database maintenance into `dbtool`

**Files:**
- Create: `cmd/dbtool/main.go`
- Modify: `Dockerfile`
- Modify: `deploy/backup.sh`
- Test: existing `internal/storage/backup_test.go` plus new CLI-level tests where practical

**Interfaces:**

```text
/dbtool migrate --database /data/gateway.db
/dbtool check --database /data/gateway.db
/dbtool backup --database /data/gateway.db --output /backup/gateway-....db
/dbtool active-auth-count --database /data/gateway.db
/dbtool schema-version --database /data/gateway.db
```

- [ ] **Step 1: Write a failing command parser test or small testable command runner**

Refactor CLI logic into a testable `run(args []string) error` and assert `migrate` rejects missing `--database`.

- [ ] **Step 2: Run the test and confirm failure**

```bash
go test ./cmd/dbtool/... -v
```

- [ ] **Step 3: Implement `migrate`, `check`, `backup`, `active-auth-count`, `schema-version`**

Rules:

```text
migrate -> RunMigrations=true + exclusive /data/db-migrate.lock
check   -> RunMigrations=false
backup  -> RunMigrations=false + existing Store.Backup
```

- [ ] **Step 4: Build `/dbtool` in the gateway image**

Update final image copy step so production operations can execute `/dbtool` without installing Go/sqlite utilities on the VPS.

- [ ] **Step 5: Rewrite `deploy/backup.sh` to mount the actual named volume**

Use:

```bash
docker run --rm \
  --network none \
  --user 1000:1000 \
  -v bank-event-gateway_gateway_data:/data:rw \
  -v "$BACKUP_DIR:/backup:rw" \
  "$IMAGE_REF" \
  /dbtool backup \
    --database /data/gateway.db \
    --output "/backup/$BACKUP_NAME"
```

Do not inspect or copy `/var/lib/docker/volumes/...` directly.

- [ ] **Step 6: Run backup tests**

```bash
go test -race ./internal/storage/... ./cmd/dbtool/...
```

- [ ] **Step 7: Commit**

```bash
git add cmd/dbtool Dockerfile deploy/backup.sh internal/storage
git commit -m "feat(ops): add dedicated database maintenance tool"
```

---

### Task 3: Split ACB and notification side effects into singleton `acb-worker`

**Files:**
- Create: `cmd/worker/main.go`
- Create: `Dockerfile.worker`
- Create: `internal/maintenance/runner.go`
- Create: `internal/maintenance/runner_test.go`
- Modify: `cmd/gateway/main.go`

**Interfaces:**

Worker owns:

```text
monitor.Monitor
monitor.SessionLoader
monitor.SessionVerifier
notification.Dispatcher
journal retention
stale auth-attempt cleanup
ACB upstream gate
/data/acb-worker.lock
```

Gateway owns:

```text
HTTP/API/UI/SSE
Cloudflare Access auth
browser UI/proxy orchestration
TTS client
Bark direct test sender if needed by admin test endpoint
SQLite application reads/writes
workerapi.Client
local JournalWatcher
```

- [ ] **Step 1: Write worker-lock test**

Add a test around the existing file-lock package proving a second worker lock acquisition fails while the first is held.

- [ ] **Step 2: Create `cmd/worker/main.go` with startup skeleton**

Startup order:

```text
config load
-> acquire /data/acb-worker.lock
-> OpenWithOptions(... RunMigrations:false)
-> load keyring
-> create notification registry/dispatcher
-> create ACB client/monitor/session loader
-> start maintenance runner
-> start worker control server (Task 4)
-> monitor.Run
```

- [ ] **Step 3: Move notification dispatcher startup out of gateway**

Remove `notification.NewDispatcher(...).Start(ctx)` from gateway startup.

- [ ] **Step 4: Move monitor startup out of gateway**

Remove `go bankMonitor.Run(ctx)` and in-process session verifier wiring from gateway.

- [ ] **Step 5: Move retention/stale-auth cleanup into `internal/maintenance`**

Implement one runner with bounded timers and context cancellation instead of leaving maintenance goroutines tied to HTTP process lifecycle.

- [ ] **Step 6: Add `--preflight` to worker**

Preflight must:

```text
load config
open DB without migration
load keyring
validate worker secrets
validate schema compatibility
NOT acquire worker lock
NOT call ACB
NOT send notifications
exit 0
```

- [ ] **Step 7: Add graceful worker shutdown**

On SIGTERM:

```text
stop accepting worker-control mutating requests
wait bounded time for current ACB upstream call
persist session snapshot if available
stop dispatcher
close DB
release worker lock
```

- [ ] **Step 8: Run tests**

```bash
go test -race ./internal/monitor/... ./internal/notification/... ./internal/maintenance/... ./cmd/worker/...
```

- [ ] **Step 9: Commit**

```bash
git add cmd/worker Dockerfile.worker cmd/gateway/main.go internal/maintenance
git commit -m "refactor(runtime): isolate ACB side effects in singleton worker"
```

---

### Task 4: Add authenticated private worker control API

**Files:**
- Create: `internal/workerapi/types.go`
- Create: `internal/workerapi/server.go`
- Create: `internal/workerapi/server_test.go`
- Create: `internal/workerapi/client.go`
- Create: `internal/workerapi/client_test.go`
- Modify: `internal/config/config.go`
- Modify: `internal/httpapi/server.go`
- Modify: `.env.example`
- Modify: `cmd/gateway/main.go`
- Modify: `cmd/worker/main.go`

**Interfaces:**

```go
type Client struct {
    BaseURL    string
    Token      string
    HTTPClient *http.Client
}

func (c *Client) RequestSync(ctx context.Context) error
func (c *Client) EnsureHistory(ctx context.Context, fromDay, toDay string) (int, error)
func (c *Client) NotifySettingsChanged(ctx context.Context) error
func (c *Client) VerifySession(ctx context.Context, connectionID string, generation int64, encrypted []byte) error
func (c *Client) WakeNotifications(ctx context.Context) error
func (c *Client) Status(ctx context.Context) (Status, error)
```

Worker routes:

```text
GET  /healthz
GET  /readyz
GET  /internal/v1/status
POST /internal/v1/sync
POST /internal/v1/history
POST /internal/v1/settings/reload
POST /internal/v1/session/verify
POST /internal/v1/notifications/wake
```

- [ ] **Step 1: Write unauthorized tests**

For every `/internal/v1/*` endpoint assert missing/wrong bearer token returns `401` without leaking body secrets.

- [ ] **Step 2: Write success tests against fake monitor/verifier/dispatcher dependencies**

Cover sync, history, verify and wake.

- [ ] **Step 3: Implement worker server with body limits**

Rules:

```text
Authorization: Bearer $WORKER_INTERNAL_TOKEN
request body <= 256 KiB
read header timeout <= 5s
write timeout bounded for non-stream routes
no cookies/session plaintext in logs
```

- [ ] **Step 4: Implement client**

Default internal timeout:

```text
5s simple commands
60s history/verify request unless endpoint already has stricter context
```

- [ ] **Step 5: Refactor HTTP server interfaces to context-aware worker client**

Update `MonitorNotifier` from no-context fire-and-forget to:

```go
type MonitorNotifier interface {
    NotifySettingsChanged(context.Context) error
}
```

Replace the notification `wakeFn func()` with:

```go
type NotificationWaker interface {
    WakeNotifications(context.Context) error
}
```

- [ ] **Step 6: Wire gateway to `workerapi.Client`**

Use:

```text
WORKER_CONTROL_URL=http://acb-worker:8190
WORKER_INTERNAL_TOKEN_FILE=/run/secrets/worker_internal_token
```

- [ ] **Step 7: Run tests**

```bash
go test -race ./internal/workerapi/... ./internal/httpapi/...
```

- [ ] **Step 8: Commit**

```bash
git add internal/workerapi internal/httpapi internal/config cmd/gateway cmd/worker .env.example
git commit -m "feat(worker): add private authenticated control API"
```

---

### Task 5: Make SSE cross-process with SQLite journal watcher

**Files:**
- Create: `internal/realtime/journal_watcher.go`
- Create: `internal/realtime/journal_watcher_test.go`
- Modify: `cmd/gateway/main.go`
- Modify: `internal/httpapi/sse.go`

**Interfaces:**

```go
type JournalWatcher struct { ... }

func NewJournalWatcher(store *storage.Store, hub *eventhub.Hub, interval time.Duration) *JournalWatcher
func (w *JournalWatcher) Run(ctx context.Context)
```

- [ ] **Step 1: Write watcher delivery test**

Start watcher, append a journal event through storage, assert subscriber receives the matching sequence/type.

- [ ] **Step 2: Write ordering and restart tests**

Verify sequence order and that a restarted watcher begins from current max sequence rather than replaying ancient rows into the live hint channel.

- [ ] **Step 3: Implement watcher**

Default interval:

```text
200ms
```

Algorithm:

```text
startup watermark = max seq
loop:
  read journal rows > watermark
  publish local hints
  advance watermark
  sleep 200ms
on storage error:
  log sanitized error
  retry <= 1s backoff
```

- [ ] **Step 4: Keep SSE DB replay as authority**

Do not replace existing `Last-Event-ID`/journal replay logic. Live hub messages remain hints only.

- [ ] **Step 5: Run SSE/realtime tests**

```bash
go test -race ./internal/realtime/... ./internal/httpapi/...
```

- [ ] **Step 6: Commit**

```bash
git add internal/realtime internal/httpapi/sse.go cmd/gateway/main.go
git commit -m "feat(realtime): bridge SQLite journal events across gateway slots"
```

---

### Task 6: Add three-level health contract

**Files:**
- Modify: `internal/httpapi/server.go`
- Modify: `internal/httpapi/server_test.go`
- Modify: `internal/workerapi/server.go`

**Interfaces:**

```text
/healthz          liveness
/readyz           traffic readiness
/internal/deployz release promotion readiness
```

- [ ] **Step 1: Add failing `/internal/deployz` test**

Expected JSON fields:

```json
{
  "status": "ready",
  "release": "e046a01a13c4f5e7fae9f6b937a6372c6a156664",
  "storage": "ready",
  "worker": "ready",
  "authBrowser": "ready",
  "tts": "ready"
}
```

Use test-injected release value; production value comes from OCI/build env.

- [ ] **Step 2: Implement deploy readiness without sensitive data**

Must not output:

```text
account number
session envelope
filesystem secret paths
token values
Bark credentials
```

- [ ] **Step 3: Ensure `AUTH_REQUIRED` does not fail liveness**

Bank business state must not make the container unhealthy.

- [ ] **Step 4: Add worker readiness**

Worker `/readyz` should confirm:

```text
DB open
keyring available when configured
worker owns singleton lock
control server accepting requests
```

It does not need a successful ACB poll every second to be “process ready”. A separate status field tracks last successful poll.

- [ ] **Step 5: Run tests**

```bash
go test -race ./internal/httpapi/... ./internal/workerapi/...
```

- [ ] **Step 6: Commit**

```bash
git add internal/httpapi internal/workerapi
git commit -m "feat(health): separate liveness readiness and deploy readiness"
```

---

# Phase B — Network and runtime security baseline

### Task 7: Create explicit edge/core/egress networks

**Files:**
- Create: `deploy/compose.core.yaml`
- Create: `deploy/compose.slot.yaml`
- Create: `deploy/compose.ops.yaml`
- Create: `deploy/tests/network-policy-test.sh`
- Modify: `deploy/README.md`

**Interfaces:**

Host networks:

```text
edge-acb      external + internal, managed by /opt/edge
acb-core      external + internal
acb-egress    external + outbound capable
```

- [ ] **Step 1: Write network policy test before changing Compose**

Test must inspect generated Compose config and fail if:

```text
gateway slot lacks edge-acb
worker joins edge-acb
auth-browser joins edge-acb
tts joins edge-acb
any DB-only service joins edge-acb
public service exposes 0.0.0.0 host port
```

- [ ] **Step 2: Bootstrap networks manually on test VPS**

```bash
docker network inspect edge-acb >/dev/null

docker network create \
  --internal \
  --label io.tuan.acb.core=true \
  acb-core

docker network create \
  --label io.tuan.acb.egress=true \
  acb-egress
```

If networks already exist, inspect instead of recreating.

- [ ] **Step 3: Define core service membership**

`acb-worker`:

```text
acb-core
acb-egress
```

`auth-browser`:

```text
acb-core
acb-egress
```

`tts-gateway`:

```text
acb-core
acb-egress
```

`bark`:

```text
edge-acb
acb-core
acb-egress
```

- [ ] **Step 4: Define slot membership**

Each gateway slot:

```text
edge-acb
acb-core
acb-egress
```

Gateway needs egress for Cloudflare JWKS and current VietQR/external features; this can later be narrowed through an egress proxy.

- [ ] **Step 5: Make persistent volumes external and stable**

```yaml
volumes:
  gateway_data:
    external: true
    name: bank-event-gateway_gateway_data
  bark_data:
    external: true
    name: bank-event-gateway_bark_data
```

- [ ] **Step 6: Run network test**

```bash
bash deploy/tests/network-policy-test.sh
```

- [ ] **Step 7: Commit**

```bash
git add deploy/compose.core.yaml deploy/compose.slot.yaml deploy/compose.ops.yaml deploy/tests/network-policy-test.sh deploy/README.md
git commit -m "feat(deploy): micro-segment ACB production networks"
```

---

### Task 8: Apply reusable container hardening to every ACB service

**Files:**
- Modify: `deploy/compose.core.yaml`
- Modify: `deploy/compose.slot.yaml`
- Modify: `Dockerfile.auth-browser`
- Create: `deploy/tests/secret-permission-test.sh`

**Interfaces:** none; produces hardened runtime definitions.

- [ ] **Step 1: Apply common baseline to Blue/Green gateway**

Use:

```yaml
restart: unless-stopped
init: true
read_only: true
user: "1000:1000"
cap_drop: [ALL]
security_opt:
  - no-new-privileges:true
pids_limit: 100
mem_limit: 512m
cpus: 0.75
stop_grace_period: 30s
tmpfs:
  - /tmp:rw,nosuid,nodev,noexec,size=64m
logging:
  driver: local
  options:
    max-size: "10m"
    max-file: "3"
```

Preserve actual benchmarked limits if production requires adjustment.

- [ ] **Step 2: Harden worker**

Start with:

```text
read_only=true
user 1000:1000
cap_drop ALL
no-new-privileges
pids <= 100
memory 512m
CPU <= 1.0
/tmp tmpfs bounded
```

Worker writes only to `/data` named volume.

- [ ] **Step 3: Preserve auth-browser seccomp and document writable tmp exception**

Do not add `noexec` to browser `/tmp` if Chromium requires executable/shared-memory behavior.

Attempt read-only rootfs in a test container; keep it only if full login/noVNC/browser lifecycle passes.

- [ ] **Step 4: Change all container log drivers from `json-file` to `local`**

This reduces disk-exhaustion risk and aligns with platform standard.

- [ ] **Step 5: Fix Bark secret world-readability**

Current deployment makes Bark basic-auth files `0644`. Replace that with a documented service-specific readable owner/mode.

For the current upstream Bark image, use root-owned read-only secret files if the process still requires UID 0:

```text
owner root:root
mode 0400
```

Keep `cap_drop: ALL`, `no-new-privileges`, read-only rootfs and no Docker socket.

- [ ] **Step 6: Write secret permission smoke test**

Assert secret files are not group/world writable/readable beyond the explicitly documented service need.

- [ ] **Step 7: Run Compose and security checks**

```bash
docker compose -f deploy/compose.core.yaml config --quiet
SLOT=blue IMAGE_REF=example.invalid/app@sha256:$(printf 'a%.0s' {1..64}) docker compose -f deploy/compose.slot.yaml config --quiet
bash deploy/tests/secret-permission-test.sh
```

- [ ] **Step 8: Commit**

```bash
git add deploy Dockerfile.auth-browser
git commit -m "security(runtime): harden ACB containers and bound logs"
```

---

### Task 9: Convert sensitive runtime values to Compose secrets

**Files:**
- Modify: `deploy/compose.core.yaml`
- Modify: `deploy/compose.slot.yaml`
- Modify: `internal/config/config.go`
- Modify: `.env.example`
- Modify: `deploy/README.md`

**Interfaces:**

Secrets:

```text
app_master_key
worker_internal_token
tts_internal_token
bark_basic_auth_user
bark_basic_auth_password
```

- [ ] **Step 1: Add `_FILE` readers for worker token and any remaining sensitive config**

Follow the existing `APP_MASTER_KEY_FILE`/`TTS_INTERNAL_TOKEN_FILE` pattern.

- [ ] **Step 2: Define top-level Compose secrets with file sources**

Example:

```yaml
secrets:
  worker_internal_token:
    file: ./secrets/worker_internal_token
```

- [ ] **Step 3: Grant minimum secret set per service**

Gateway slots:

```text
app_master_key
worker_internal_token
tts_internal_token
bark_basic_auth_user/password only if direct Bark test endpoint still needs them
```

Worker:

```text
app_master_key
worker_internal_token
bark credentials
```

TTS:

```text
tts_internal_token
```

Auth-browser:

```text
no app master key
no worker token
```

- [ ] **Step 4: Add deploy-time file mode checks**

Abort if a sensitive source file is missing or unexpectedly permissive.

- [ ] **Step 5: Run config/security tests**

```bash
go test -race ./internal/config/...
bash deploy/tests/secret-permission-test.sh
```

- [ ] **Step 6: Commit**

```bash
git add deploy/compose.core.yaml deploy/compose.slot.yaml internal/config .env.example deploy/README.md
git commit -m "security(secrets): scope file-backed secrets per service"
```

---

# Phase C — Active/Standby Blue-Green through shared Caddy

### Task 10: Parameterize gateway Blue/Green slots

**Files:**
- Modify: `deploy/compose.slot.yaml`
- Create: `deploy/smoke-slot.sh`
- Test: `deploy/tests/deploy-state-test.sh`

**Interfaces:**

Environment:

```text
SLOT=blue|green
IMAGE_REF=$GATEWAY_IMAGE_REF
SLOT_HOST_PORT=18091|18092 only in ops/debug override
RELEASE_SHA=$GITHUB_SHA
```

Stable edge aliases:

```text
blue  -> acb-web-blue
green -> acb-web-green
```

- [ ] **Step 1: Write slot validation test**

Assert any value other than `blue|green` fails before Docker is called.

- [ ] **Step 2: Run each slot as a distinct Compose project**

```bash
docker compose -p acb-blue  -f deploy/compose.slot.yaml up -d
docker compose -p acb-green -f deploy/compose.slot.yaml up -d
```

- [ ] **Step 3: Remove shared `container_name: acb-transaction-gateway`**

Use slot-specific names or Compose-generated names so both can exist.

- [ ] **Step 4: Mount the same canonical `gateway_data` volume**

Both HTTP slots can open SQLite after Task 1, but neither runs monitor/notification side effects.

- [ ] **Step 5: Implement direct slot smoke test**

`deploy/smoke-slot.sh` must check:

```text
/healthz
/readyz
/internal/deployz
/
known static asset or API read endpoint
```

It must never start/cancel an ACB login or send a real notification.

- [ ] **Step 6: Run both slots simultaneously in staging**

Confirm:

```text
both healthy
worker remains one instance
no second ACB poll loop
SSE journal watcher works on both
```

- [ ] **Step 7: Commit**

```bash
git add deploy/compose.slot.yaml deploy/smoke-slot.sh deploy/tests/deploy-state-test.sh
git commit -m "feat(deploy): add concurrent Blue Green gateway slots"
```

---

### Task 11: Add shared Caddy primary/standby route for ACB

**Files:**
- Create: `deploy/edge/acb.route.caddy`
- Create: `deploy/edge/acb.route.test.caddy`
- Modify: `EDGE_INGRESS_RULES.md`
- Create: `deploy/switch-slot.sh`

**Interfaces:**

Route template when Green is primary:

```caddyfile
reverse_proxy acb-web-green:8090 acb-web-blue:8090 {
    lb_policy first

    health_uri /readyz
    health_interval 2s
    health_timeout 1s
    health_fails 2
    health_passes 2

    fail_duration 30s
    max_fails 2
    lb_try_duration 3s
    lb_try_interval 250ms

    stream_close_delay 5m
}
```

Blue-primary route reverses the upstream order.

- [ ] **Step 1: Create route file with exact host/security policy from current edge config**

Preserve:

- exact hostname;
- Cloudflare trusted proxy boundary;
- Access behavior;
- SSE streaming behavior;
- unknown host policy.

- [ ] **Step 2: Add public block for `/internal/*`**

Caddy must return 404 for `/internal/*` before proxying.

- [ ] **Step 3: Implement `switch-slot.sh`**

Algorithm:

```text
validate blue|green
verify target /readyz
verify target /internal/deployz through private path
render .next route
copy into /opt/edge/routes/acb.caddy.next
run caddy validate inside shared edge
atomically replace active route file
run caddy reload
probe public/internal Caddy route
write active-slot state only after success
```

- [ ] **Step 4: On reload failure restore file but do not restart Caddy**

Caddy keeps previous working runtime config if the new config fails to load; script state must mirror that behavior.

- [ ] **Step 5: Validate with Caddy v2.11.4 in CI**

Use the same major/minor release as production and a test Docker network/upstream fixture.

- [ ] **Step 6: Commit**

```bash
git add deploy/edge deploy/switch-slot.sh EDGE_INGRESS_RULES.md
git commit -m "feat(edge): add Caddy active standby ACB routing"
```

---

### Task 12: Implement transactional gateway deployment state machine

**Files:**
- Create: `deploy/preflight.sh`
- Create: `deploy/deploy-gateway.sh`
- Create: `deploy/verify-release.sh`
- Modify: `deploy/tests/deploy-state-test.sh`

**Interfaces:**

```bash
./deploy-gateway.sh \
  --release "$GITHUB_SHA" \
  --image "$IMAGE_DIGEST_REF"
```

Runtime state directory:

```text
/opt/acb-transaction-webhook/state/
  active-slot
  blue-image
  green-image
  previous-primary
  last-successful-release
```

- [ ] **Step 1: Write failure-state tests with fake Docker/Caddy commands**

Required scenarios:

```text
pull fails -> active state unchanged
candidate health fails -> active state unchanged
smoke fails -> Caddy not touched
Caddy validation fails -> active state unchanged
Caddy reload fails -> previous route/state retained
```

- [ ] **Step 2: Implement preflight gates**

Abort before candidate startup if:

```text
free disk < configured threshold
free RAM/headroom insufficient for second gateway
required networks missing/wrong internal flag
required secrets missing
active slot state invalid
worker not ready
image reference not immutable digest
image signature invalid
```

- [ ] **Step 3: Determine inactive slot**

```text
active=blue -> target=green
active=green -> target=blue
```

If state file is missing, abort and require bootstrap mode; do not guess production routing.

- [ ] **Step 4: Pull target image before modifying target container**

```bash
docker pull "$IMAGE_REF"
```

- [ ] **Step 5: Run migration/backup gate**

Run Task 17 rules before replacing inactive slot.

- [ ] **Step 6: Recreate inactive slot only**

Never stop active slot.

- [ ] **Step 7: Wait for Docker health and direct smoke**

Use bounded timeout and dump only candidate logs on failure.

- [ ] **Step 8: Promote using `switch-slot.sh`**

Promotion must be the first action that changes public traffic.

- [ ] **Step 9: Soak for 5 minutes by default**

Every 5 seconds check:

```text
Caddy route health
new primary /readyz
worker /readyz
new primary restart count
```

Three consecutive core gateway failures trigger route rollback.

- [ ] **Step 10: Keep previous version running as standby**

Do not stop it after successful soak.

- [ ] **Step 11: Commit**

```bash
git add deploy/preflight.sh deploy/deploy-gateway.sh deploy/verify-release.sh deploy/tests/deploy-state-test.sh
git commit -m "feat(deploy): add transactional hot standby gateway releases"
```

---

### Task 13: Make rollback a Caddy route-order flip

**Files:**
- Modify: `deploy/rollback.sh`
- Modify: `deploy/switch-slot.sh`
- Create: `docs/ROLLBACK_RUNBOOK.md`

**Interfaces:**

```bash
./deploy/rollback.sh gateway
```

- [ ] **Step 1: Write rollback-state test**

Given:

```text
active=blue
previous-primary=green
both healthy
```

expect rollback calls only `switch-slot.sh green`, not image build/migration/full Compose down.

- [ ] **Step 2: Implement hot-standby rollback**

Sequence:

```text
health previous slot
validate Caddy route
reload route with previous first
public smoke
update state
```

- [ ] **Step 3: Implement historical fallback only when previous slot no longer contains expected digest**

Restore old digest into inactive slot, validate, then route-switch. Still never kill current primary first.

- [ ] **Step 4: Document DB compatibility boundary**

If schema contract was violated, application rollback must abort rather than pretend a code route flip is safe.

- [ ] **Step 5: Run deploy-state tests**

```bash
bash deploy/tests/deploy-state-test.sh
```

- [ ] **Step 6: Commit**

```bash
git add deploy/rollback.sh deploy/switch-slot.sh docs/ROLLBACK_RUNBOOK.md
git commit -m "feat(rollback): make gateway rollback an instant Caddy flip"
```

---

# Phase D — Component-specific lifecycle for stateful services

### Task 14: Add safe independent ACB worker deployment

**Files:**
- Create: `deploy/deploy-worker.sh`
- Modify: `deploy/compose.core.yaml`
- Modify: `cmd/worker/main.go`
- Modify: `internal/monitor/monitor.go`

**Interfaces:**

```bash
./deploy-worker.sh --image "$WORKER_IMAGE_REF"
```

- [ ] **Step 1: Add worker preflight smoke**

Run candidate image:

```bash
docker run --rm \
  --network none \
  ...volume/secret mounts... \
  "$WORKER_IMAGE" /worker --preflight
```

No ACB request is allowed in preflight.

- [ ] **Step 2: Pull and verify image before touching current worker**

- [ ] **Step 3: Gracefully stop current worker only after preflight succeeds**

Use worker `stop_grace_period` long enough for an in-flight ACB request/session snapshot.

- [ ] **Step 4: Start new worker and wait for lock/readiness**

- [ ] **Step 5: Require a post-start ACB cycle confirmation without treating temporary external maintenance as container death**

Track status separately from `/readyz`.

- [ ] **Step 6: Restart previous digest automatically if new worker cannot become ready**

- [ ] **Step 7: Confirm gateway public traffic stays up throughout**

- [ ] **Step 8: Commit**

```bash
git add deploy/deploy-worker.sh deploy/compose.core.yaml cmd/worker internal/monitor
git commit -m "feat(deploy): add controlled singleton ACB worker handoff"
```

---

### Task 15: Make auth-browser deployment session-aware

**Files:**
- Create: `deploy/deploy-auth-browser.sh`
- Modify: `deploy/compose.core.yaml`
- Reuse: `/dbtool active-auth-count`

- [ ] **Step 1: Add test that deployment aborts when active auth count is non-zero**

- [ ] **Step 2: Verify candidate image smoke before stopping current browser**

Use existing auth-browser image smoke test logic without touching production `/sessions`.

- [ ] **Step 3: Query active auth count**

If result > 0:

```text
ABORT
leave current auth-browser running
leave gateway/worker unchanged
```

- [ ] **Step 4: Replace only auth-browser**

Do not restart gateway slots or worker.

- [ ] **Step 5: Verify browser health and noVNC/RPC private reachability**

- [ ] **Step 6: Commit**

```bash
git add deploy/deploy-auth-browser.sh deploy/compose.core.yaml
git commit -m "feat(deploy): protect active ACB login during browser updates"
```

---

### Task 16: Make TTS and Bark independent deployable components

**Files:**
- Create: `deploy/deploy-tts.sh`
- Create: `deploy/deploy-bark.sh`
- Modify: `deploy/compose.core.yaml`
- Reuse/update: `deploy/smoke-test-bark.sh`

- [ ] **Step 1: TTS candidate smoke before replacement**

Use current TTS image smoke tests and internal token.

- [ ] **Step 2: Replace TTS only**

Gateway must treat temporary TTS outage as degraded functionality, not banking API unavailability.

- [ ] **Step 3: Bark preflight**

Verify:

```text
image digest valid
persistent volume exists
credentials readable only by intended process
candidate process can boot against disposable data volume
```

- [ ] **Step 4: Backup Bark state before Bark update**

Do not mount the live Bark data volume into a second active Bark server if upstream storage semantics do not guarantee safe concurrency.

- [ ] **Step 5: Replace Bark only and run authenticated private smoke test**

- [ ] **Step 6: Commit**

```bash
git add deploy/deploy-tts.sh deploy/deploy-bark.sh deploy/compose.core.yaml deploy/smoke-test-bark.sh
git commit -m "feat(deploy): isolate TTS and Bark release lifecycles"
```

---

# Phase E — Supply chain and CI/CD security

### Task 17: Add migration contract and consistent backup gate

**Files:**
- Create: `internal/storage/migration_contract_test.go`
- Modify: `deploy/compose.ops.yaml`
- Modify: `deploy/deploy-gateway.sh`
- Modify: `deploy/backup.sh`
- Create: `docs/RESTORE_RUNBOOK.md`

**Interfaces:**

```text
predeploy backup
-> migration compatibility check
-> migration one-shot
-> integrity check
-> candidate start
```

- [ ] **Step 1: Add destructive migration guard test**

Guard ordinary release migrations against obvious patterns such as:

```text
DROP TABLE
DROP COLUMN
incompatible RENAME COLUMN
```

The test is defense-in-depth; review is still mandatory.

- [ ] **Step 2: Run DB backup through `compose.ops.yaml` with no application network**

- [ ] **Step 3: Snapshot matching master key metadata**

Store backup DB and master key backup in protected paths with `0600`; do not upload them as public CI artifacts.

- [ ] **Step 4: Apply migration exactly once through dbtool**

- [ ] **Step 5: Integrity-check after migration and before candidate start**

- [ ] **Step 6: Add restore drill documented command sequence**

Restore to a separate file/volume first, run integrity checks, verify schema and representative counts, then define cutover steps.

- [ ] **Step 7: Run storage tests**

```bash
go test -race ./internal/storage/...
```

- [ ] **Step 8: Commit**

```bash
git add internal/storage/migration_contract_test.go deploy/compose.ops.yaml deploy/deploy-gateway.sh deploy/backup.sh docs/RESTORE_RUNBOOK.md
git commit -m "security(data): enforce rollback-safe migrations and restore-tested backups"
```

---

### Task 18: Add Trivy scan, SBOM and Cosign signing

**Files:**
- Modify: `.github/workflows/deploy.yml`
- Create: `deploy/verify-image-signature.sh`
- Modify: `deploy/preflight.sh`

**Interfaces:**

Production signer identity must match the repository workflow identity:

```text
OIDC issuer: https://token.actions.githubusercontent.com
certificate identity: this repository's production workflow on refs/heads/main
```

- [ ] **Step 1: Add GitHub OIDC permission only to signing job**

```yaml
permissions:
  contents: read
  packages: write
  id-token: write
```

- [ ] **Step 2: Add Trivy 0.74.0 image scan by digest**

Scan after push and before deploy.

Gate:

```text
CRITICAL vulnerabilities -> fail unless explicit reviewed ignore/VEX
HIGH -> report and enforce policy agreed in SECURITY_ARCHITECTURE.md
```

- [ ] **Step 3: Generate SBOM**

Store CycloneDX/SPDX as workflow artifact and/or OCI attestation tied to digest.

- [ ] **Step 4: Sign each app-owned image digest with Cosign keyless GitHub OIDC**

Images:

```text
gateway
worker
auth-browser
tts-gateway
```

For third-party Bark, pin immutable digest and scan it; verify upstream signature if upstream provides one, otherwise maintain an approved digest allowlist.

- [ ] **Step 5: Verify signatures in CI immediately after signing**

- [ ] **Step 6: Implement VPS signature verification before pull/promote**

`deploy/verify-image-signature.sh` must fail closed.

- [ ] **Step 7: Run workflow syntax/lint checks**

```bash
bash -n deploy/*.sh
```

Use actionlint if available in CI.

- [ ] **Step 8: Commit**

```bash
git add .github/workflows/deploy.yml deploy/verify-image-signature.sh deploy/preflight.sh
git commit -m "security(supply-chain): scan attest and sign production images"
```

---

### Task 19: Make GitHub Actions component-aware and remove mutable deploy behavior

**Files:**
- Modify: `.github/workflows/deploy.yml`
- Create: `scripts/verify-actions-pinned.sh`
- Create: `.github/dependabot.yml`

**Interfaces:** component classifier outputs:

```text
gateway_changed
worker_changed
auth_browser_changed
tts_changed
bark_config_changed
edge_route_changed
```

- [ ] **Step 1: Add path classifier job**

Map shared code carefully. Example:

Gateway affected by:

```text
web/**
cmd/gateway/**
internal/httpapi/**
internal/auth/**
internal/realtime/**
internal/config/**
internal/storage/**
Dockerfile
```

Worker affected by:

```text
cmd/worker/**
internal/acb/**
internal/monitor/**
internal/notification/**
internal/workerapi/**
internal/maintenance/**
internal/config/**
internal/storage/**
Dockerfile.worker
```

- [ ] **Step 2: Build only affected app-owned images**

Shared dependency changes may intentionally mark multiple components.

- [ ] **Step 3: Stop publishing `latest` as a deployment artifact**

Human-friendly SHA tags may remain, but production deploy inputs use digest outputs only.

- [ ] **Step 4: Dispatch component-specific remote deploy script**

Examples:

```text
gateway only -> deploy-gateway.sh
worker only  -> deploy-worker.sh
auth browser -> deploy-auth-browser.sh
TTS          -> deploy-tts.sh
```

- [ ] **Step 5: Pin third-party GitHub Actions to full commit SHA**

Create `scripts/verify-actions-pinned.sh` that rejects remote `uses:` values not ending in a 40-hex commit SHA. Local `./.github/actions/...` references are exempt.

- [ ] **Step 6: Add Dependabot GitHub Actions updates**

Use `.github/dependabot.yml` so SHA pins remain maintainable.

- [ ] **Step 7: Keep production serialization**

```yaml
concurrency:
  group: acb-transaction-webhook-production
  cancel-in-progress: false
```

- [ ] **Step 8: Commit**

```bash
git add .github/workflows/deploy.yml .github/dependabot.yml scripts/verify-actions-pinned.sh
git commit -m "ci: deploy ACB components independently by immutable digest"
```

---

# Phase F — Host security and self-healing

### Task 20: Add host preflight and Docker daemon baseline documentation

**Files:**
- Create: `deploy/check-host.sh`
- Create: `docs/SECURITY_ARCHITECTURE.md`
- Modify: `docs/PRODUCTION_SETUP.md`

**Interfaces:** `check-host.sh` returns non-zero when host cannot safely deploy.

- [ ] **Step 1: Implement host checks**

Check:

```text
Docker Engine reachable
Compose v2 available
live-restore enabled
Docker TCP API not exposed
required networks have expected internal flag
AppArmor/SELinux state visible
seccomp available
free disk threshold
free RAM threshold
NTP synchronized
```

- [ ] **Step 2: Document recommended `/etc/docker/daemon.json` delta**

Target merged values:

```json
{
  "live-restore": true,
  "log-driver": "local",
  "log-opts": {
    "max-size": "10m",
    "max-file": "3"
  }
}
```

Do not tell operators to overwrite unrelated existing daemon keys.

- [ ] **Step 3: Validate before daemon reload**

Back up current file, validate candidate daemon config, then use a daemon reload when supported rather than unnecessary full restart.

- [ ] **Step 4: Document rootless/userns as Phase-2 host project**

Do not enable during ACB Blue/Green cutover because existing bind mounts, UID ownership and Chromium must be tested separately.

- [ ] **Step 5: Commit**

```bash
git add deploy/check-host.sh docs/SECURITY_ARCHITECTURE.md docs/PRODUCTION_SETUP.md
git commit -m "docs(host): define single VPS Docker security baseline"
```

---

### Task 21: Add bounded self-healing without Docker-socket autoheal containers

**Files:**
- Create: `deploy/host/acb-health-watchdog.sh`
- Create: `deploy/host/acb-health-watchdog.service`
- Create: `deploy/host/acb-health-watchdog.timer`
- Modify: `docs/PRODUCTION_SETUP.md`

**Interfaces:**

Allowlist behavior:

```text
gateway blue/green -> restart after sustained unhealthy
TTS                -> restart after sustained unhealthy
Bark               -> alert/restart only if state-safe check passes
auth-browser       -> alert only during active auth session
acb-worker         -> alert + controlled deploy-worker recovery, no blind loop
```

- [ ] **Step 1: Write watchdog dry-run mode**

```bash
./acb-health-watchdog.sh --dry-run
```

must print intended action without invoking Docker mutation.

- [ ] **Step 2: Require sustained failure threshold**

Do not restart on one unhealthy sample. Use at least three checks spanning >= 30 seconds.

- [ ] **Step 3: Install as host systemd timer in staging**

Do not containerize it with Docker socket.

- [ ] **Step 4: Test primary gateway failure**

Expected:

```text
Caddy fails traffic to standby before watchdog restart
watchdog later restarts unhealthy old primary/standby
```

- [ ] **Step 5: Test worker failure path does not create two workers**

- [ ] **Step 6: Commit**

```bash
git add deploy/host docs/PRODUCTION_SETUP.md
git commit -m "ops: add host-level bounded health recovery"
```

---

### Task 22: Reduce outbound blast radius

**Files:**
- Modify: `deploy/compose.core.yaml`
- Modify: `deploy/compose.slot.yaml`
- Create: `deploy/tests/egress-inventory.md`
- Modify: `docs/SECURITY_ARCHITECTURE.md`

**Interfaces:** egress is denied by network membership unless service joins `acb-egress`.

- [ ] **Step 1: Inventory actual outbound destinations during controlled test**

Document destination categories, not secrets:

```text
worker -> ACB + configured webhook destinations + Bark internal
browser -> ACB-required web origins
TTS -> configured TTS providers
Bark -> Apple push infrastructure as required
Gateway -> Cloudflare JWKS + VietQR/current external API features
```

- [ ] **Step 2: Confirm no data-only service joins egress**

- [ ] **Step 3: Keep `acb-core` internal and verify Internet access fails from a core-only test container**

- [ ] **Step 4: Add Phase-2 explicit egress proxy design to security doc**

Do not enforce a brittle domain allowlist before browser/provider dependencies are measured.

- [ ] **Step 5: Commit**

```bash
git add deploy/compose.core.yaml deploy/compose.slot.yaml deploy/tests/egress-inventory.md docs/SECURITY_ARCHITECTURE.md
git commit -m "security(network): constrain ACB service egress membership"
```

---

# Phase G — Production cutover and reusable template extraction

### Task 23: Build one-time migration/bootstrap script

**Files:**
- Create: `deploy/bootstrap-v1.sh`
- Modify: `deploy/README.md`

**Interfaces:** one-time migration from legacy monolith to Platform Standard v1.

- [ ] **Step 1: Preflight old production state**

Record:

```text
current image digests
current Compose snapshot
current Caddy route
current DB backup
current active ACB/session state
container IDs
```

- [ ] **Step 2: Create/verify `acb-core` and `acb-egress`**

- [ ] **Step 3: Pull and verify all new images before changing legacy runtime**

- [ ] **Step 4: Apply backward-compatible schema migration**

- [ ] **Step 5: Start Blue and Green API-only slots before stopping legacy gateway**

Because the new gateway no longer owns `gateway.lock`, it can be smoke-tested while legacy monolith still serves traffic.

- [ ] **Step 6: Stage shared Caddy route to new slots but do not reload yet**

- [ ] **Step 7: Cut public traffic to new primary API slot**

- [ ] **Step 8: Stop legacy monolith and immediately start singleton worker**

This is the only architecture-conversion step likely to create a short ACB polling handoff gap. Public HTTP should remain available because new gateway slots are already live.

- [ ] **Step 9: Confirm worker session restore and first successful poll/catch-up**

- [ ] **Step 10: Keep legacy Compose/config/image references for rollback window**

Do not delete old deployment files in bootstrap.

- [ ] **Step 11: Commit**

```bash
git add deploy/bootstrap-v1.sh deploy/README.md
git commit -m "feat(migration): add one-time Platform Standard v1 bootstrap"
```

---

### Task 24: Run mandatory failure drills

**Files:**
- Modify: `deploy/verify-release.sh`
- Modify: `docs/ROLLBACK_RUNBOOK.md`
- Modify: `docs/PRODUCTION_SETUP.md`

- [ ] **Step 1: Broken image digest**

Expected:

```text
pull/verify fails
active primary untouched
Caddy untouched
worker untouched
```

- [ ] **Step 2: Candidate gateway crash**

Expected:

```text
candidate unhealthy
promotion not attempted
current primary continues
```

- [ ] **Step 3: Primary crashes after promotion**

Expected:

```text
Caddy health check removes primary
standby serves new requests
Docker/watchdog recovers failed slot
```

- [ ] **Step 4: Caddy invalid config**

Expected:

```text
validate/reload fails
old Caddy runtime config continues
state file not changed
```

- [ ] **Step 5: SSE cutover**

Expected:

```text
connection drains or reconnects
EventSource reconnects
Last-Event-ID journal replay fills any gap
no lost transaction event
```

- [ ] **Step 6: Worker replacement**

Expected:

```text
no dual ACB poller
session restored
catch-up protects polling gap
HTTP remains available
```

- [ ] **Step 7: Auth browser update while active login exists**

Expected:

```text
browser update aborts
interactive login survives
```

- [ ] **Step 8: Restore drill**

Expected:

```text
backup DB passes integrity
paired master key decrypts expected encrypted session data
representative row counts match snapshot
```

- [ ] **Step 9: Record results in ops docs**

- [ ] **Step 10: Commit**

```bash
git add deploy/verify-release.sh docs/ROLLBACK_RUNBOOK.md docs/PRODUCTION_SETUP.md
git commit -m "test(ops): certify ACB deployment failure recovery"
```

---

### Task 25: Convert repo docs into reusable onboarding template

**Files:**
- Modify: `EDGE_INGRESS_RULES.md`
- Modify: `deploy/README.md`
- Modify: `docs/PRODUCTION_SETUP.md`
- Modify: `docs/SECURITY_ARCHITECTURE.md`
- Add reference to: `docs/superpowers/specs/2026-09-13-single-vps-secure-container-platform-standard.md`

**Interfaces:** Future repository onboarding uses Platform Standard v1 workload classification W1-W6.

- [ ] **Step 1: Mark `EDGE_INGRESS_RULES.md` as the ingress subset of Platform Standard v1**

Do not maintain conflicting duplicate rules.

- [ ] **Step 2: Add workload inventory table for ACB**

Use exactly:

| Service | Class | Edge | Core | Egress | Deployment |
|---|---|---:|---:|---:|---|
| gateway-blue | W1 | yes | yes | yes | Active/Standby |
| gateway-green | W1 | yes | yes | yes | Active/Standby |
| acb-worker | W2 | no | yes | yes | Singleton handoff |
| auth-browser | W4 | no | yes | yes | Session-aware singleton |
| tts-gateway | W1 auxiliary | no | yes | yes | Independent replace |
| bark | W5 | yes | yes | yes | Stateful singleton |
| SQLite data | W3 | no | volume | no | Backup/migration |
| dbtool | W6 | no | no | no | One-shot |

- [ ] **Step 3: Add “new repo migration recipe”**

```text
classify services
-> create edge/core/egress
-> harden containers
-> add health contracts
-> choose W1/W2/W3 deployment strategy
-> add CI digest/scan/sign
-> add backup/rollback
-> onboard shared Caddy
-> failure drill
```

- [ ] **Step 4: Remove stale docs after rollback window**

Only after production has run safely through at least one complete Blue->Green->Blue cycle.

- [ ] **Step 5: Commit**

```bash
git add EDGE_INGRESS_RULES.md deploy/README.md docs
git commit -m "docs(platform): make ACB the reference implementation for future repos"
```

---

# 3. GitHub Actions Target Pipeline

Final workflow shape:

```text
push main
   |
   v
verify
  |- Bun typecheck/test/build
  |- Go race tests + vet
  |- TTS tests
  |- shellcheck/bash -n
  |- Compose config tests
  |- Caddy route validation
  |- action pin verification
   |
   v
classify changed components
   |
   +------------------+--------------------+-------------------+
   |                  |                    |                   |
 gateway            worker             auth-browser          TTS
 build               build               build               build
   |                  |                    |                   |
   v                  v                    v                   v
 push digest        push digest          push digest          push digest
   |                  |                    |                   |
 scan/SBOM          scan/SBOM            scan/SBOM           scan/SBOM
   |                  |                    |                   |
 sign                sign                 sign                sign
   +------------------+--------------------+-------------------+
                              |
                              v
                   protected production env
                              |
                              v
                    remote host preflight
                              |
                 component-specific deploy
```

No CI failure before the production deploy job can alter running containers.

---

# 4. Caddy Route Behavior Required in Production

For Blue primary:

```caddyfile
reverse_proxy acb-web-blue:8090 acb-web-green:8090 {
    lb_policy first
    health_uri /readyz
    health_interval 2s
    health_timeout 1s
    health_fails 2
    health_passes 2
    fail_duration 30s
    max_fails 2
    lb_try_duration 3s
    lb_try_interval 250ms
    stream_close_delay 5m
}
```

For Green primary, reverse upstream order.

Important behavior:

```text
primary container process dies
-> Docker restart policy begins recovery
-> Caddy sees primary unhealthy
-> Caddy sends new requests to standby
-> failed slot can recover without being public primary
```

Do not confuse this with VPS-level HA. If the VPS itself dies, both slots and Caddy die.

---

# 5. Required Runtime State Ownership

After migration, ownership must be unambiguous.

```text
ACB network session          -> acb-worker
ACB polling                  -> acb-worker
ACB history/catch-up         -> acb-worker
notification delivery queue  -> acb-worker
webhook/Bark delivery retry  -> acb-worker
HTTP UI/API                  -> active gateway slot
SSE connections              -> whichever gateway slot Caddy chose
SSE source of truth          -> SQLite event_journal
interactive login browser    -> auth-browser
TTS process                  -> tts-gateway
Bark server state            -> Bark volume
schema migration             -> dbtool one-shot
traffic routing              -> shared Caddy
public tunnel                -> shared cloudflared
```

---

# 6. Production Resource Policy

Start from current measured limits and preserve headroom.

Suggested initial ceilings based on current Compose, subject to measurement:

| Service | Memory ceiling | CPU ceiling | PID |
|---|---:|---:|---:|
| gateway-blue | 512 MiB | 0.75 | 100 |
| gateway-green | 512 MiB | 0.75 | 100 |
| acb-worker | 512 MiB | 1.00 | 100 |
| auth-browser | 1536 MiB | 1.50 | 300 |
| tts-gateway | 256 MiB | 0.50 | 50 |
| Bark | 128-256 MiB | 0.25-0.50 | 50 |

Before enabling permanent hot standby, verify host can hold:

```text
both gateway slots
+
worker
+
browser peak
+
TTS/Bark
+
Caddy/cloudflared
+
OS/Docker
+
backup/migration transient load
+
>=20% RAM headroom
```

If not, use “warm standby on demand” for gateway until VPS RAM is increased; do not fake safety by overcommitting memory.

---

# 7. Security Acceptance Criteria

Migration is not complete until every statement is true.

## Edge and networking

- [ ] `/opt/edge` remains the only production Caddy/cloudflared stack.
- [ ] `edge-acb` is internal and contains only Caddy + public ACB upstreams.
- [ ] `acb-core` is internal.
- [ ] `acb-egress` contains only services with documented outbound need.
- [ ] No production app endpoint is bound on `0.0.0.0` host ports.
- [ ] DB/session/browser RPC/VNC/TTS internal endpoints are not publicly routed.
- [ ] Caddy blocks `/internal/*`.

## Container isolation

- [ ] No ACB app container mounts Docker socket.
- [ ] No app container is privileged.
- [ ] All possible processes run non-root; documented exceptions are minimal.
- [ ] Capabilities are dropped.
- [ ] `no-new-privileges` enabled.
- [ ] read-only rootfs enabled where compatible.
- [ ] Seccomp is not unconfined.
- [ ] resource and PID limits exist.
- [ ] log storage is bounded.

## Secrets

- [ ] No production secret stored in git.
- [ ] Sensitive runtime values are file-backed secrets.
- [ ] Secret permissions are not world-readable unless an explicit reviewed upstream constraint exists; ACB target is no world-readable secrets.
- [ ] Gateway does not receive secrets it does not use.
- [ ] Auth-browser does not receive app master/worker/Bark secrets.

## Deployment

- [ ] Broken build cannot touch VPS production.
- [ ] Broken image/signature cannot replace inactive slot.
- [ ] Broken candidate cannot change Caddy route.
- [ ] Active gateway is not stopped to deploy candidate.
- [ ] Previous version remains hot standby.
- [ ] Runtime primary crash fails over to standby.
- [ ] Rollback during standby window is Caddy route flip.

## ACB-specific state

- [ ] Only worker polls/verifies ACB upstream.
- [ ] Dual worker lock acquisition fails.
- [ ] Worker update does not create dual polling.
- [ ] Auth-browser update refuses active login disruption.
- [ ] Session restoration survives worker replacement.
- [ ] Catch-up/dedupe closes worker restart polling gaps.

## Supply chain

- [ ] App-owned images scanned.
- [ ] SBOM generated.
- [ ] App-owned images signed.
- [ ] VPS verifies signer identity before promotion.
- [ ] Production uses digest only.
- [ ] GitHub Actions third-party actions are SHA-pinned.

## Data recovery

- [ ] Consistent named-volume SQLite backup succeeds.
- [ ] Master key recovery material is protected.
- [ ] Restore drill succeeds.
- [ ] Migration contract test blocks obvious destructive changes in ordinary Blue/Green releases.

---

# 8. Performance Acceptance Criteria

Target values for this one-VPS design:

```text
Gateway public request cutover gap       effectively zero under Caddy reload
Gateway crash failover                   <= health detection + retry window (~seconds)
Gateway rollback                         route flip, no rebuild
Gateway-only deploy ACB poll interruption 0
Worker controlled replacement gap        target < 5 seconds after image pre-pull
SSE cross-process journal hint latency    <= 250ms normal load
Deployment additional memory             one extra gateway slot only
```

Do not claim host-level zero downtime; single VPS remains a single failure domain.

---

# 9. Rollout Order

Implement in exactly this sequence:

```text
1  storage runtime/migration split
2  dbtool
3  singleton worker split
4  private worker API
5  cross-process journal watcher
6  health contract
7  network split
8  container hardening
9  secrets scope
10 Blue/Green slot Compose
11 shared Caddy primary/standby route
12 transactional gateway deploy
13 instant rollback
14 worker deploy
15 auth-browser deploy
16 TTS/Bark deploy
17 backup/migration contract
18 scan/SBOM/signing
19 component-aware CI
20 host baseline
21 bounded watchdog
22 egress reduction
23 one-time bootstrap
24 failure drills
25 reusable docs/template extraction
```

Tasks 1-6 are prerequisites for safe Blue/Green. Do not jump directly to Caddy dual upstreams while the current monolith still runs ACB polling in each gateway process.

---

# 10. One-Time Production Cutover Checklist

Before cutover:

- [ ] CI for new architecture green.
- [ ] Signed immutable images available.
- [ ] Database backup + restore test completed.
- [ ] `acb-core` and `acb-egress` created/verified.
- [ ] Shared Caddy v2.11.4 route candidate validates.
- [ ] Both new gateway slots can start in API-only mode.
- [ ] Worker candidate `--preflight` passes.
- [ ] Auth-browser/TTS/Bark current state healthy.
- [ ] No active interactive ACB auth attempt.
- [ ] VPS RAM/disk headroom sufficient.
- [ ] Old image digests/Compose/Caddy route recorded.

Cutover:

```text
start new Blue/Green API slots
-> smoke both
-> route Caddy to chosen new primary/standby
-> verify public UI/API/SSE
-> stop legacy monolithic gateway
-> start acb-worker
-> verify session restore
-> verify first poll/catch-up
-> monitor 15-30 minutes
```

After rollback window:

- [ ] Remove legacy monolith only after at least one successful Blue->Green->Blue release cycle.
- [ ] Remove old unused host bindings/network artifacts.
- [ ] Rotate temporary migration credentials/tokens if any were created.
- [ ] Keep backups according to retention policy.

---

# 11. Reuse Recipe for the Next Repository

When migrating the next repo, do **not** copy ACB worker code. Reuse the platform contract and classify its services.

Example decision tree:

```text
Does service receive HTTP traffic?
  yes -> W1 -> Blue/Green candidate

Does service perform unique external side effect?
  yes -> W2 -> singleton/lease/handoff

Is it a database/cache with persistent state?
  yes -> W3 -> backup/migration strategy

Is it a browser/session sidecar?
  yes -> W4 -> private singleton

Is it a public stateful helper?
  yes -> W5 -> Caddy + independent lifecycle

Is it one-shot migration/backup?
  yes -> W6 -> no public network
```

Then apply the same folders/scripts shape:

```text
deploy/compose.core.yaml
deploy/compose.slot.yaml
deploy/compose.ops.yaml
deploy/preflight.sh
deploy/deploy-gateway.sh
deploy/rollback.sh
deploy/verify-release.sh
deploy/edge/${APP_ID}.route.caddy
```

Only workload-specific internals change.

---

# 12. Self-Review

## Spec coverage

- [x] Shared Caddy/Cloudflare edge preserved.
- [x] One-VPS limitation stated explicitly.
- [x] Micro-segmented edge/core/egress network design included.
- [x] No-public-port policy included.
- [x] Active/Standby Blue-Green included.
- [x] Runtime failover included.
- [x] Singleton ACB worker included.
- [x] SSE cross-process behavior included.
- [x] SQLite migration/backup safety included.
- [x] Secrets/capabilities/read-only/seccomp/log/resource hardening included.
- [x] Docker socket boundary included.
- [x] Host live-restore/logging baseline included.
- [x] Supply-chain scan/SBOM/signing included.
- [x] Component-aware CI/CD included.
- [x] Backup/restore drill included.
- [x] Auth-browser active-login safety included.
- [x] Bark/TTS independent lifecycle included.
- [x] Failure drills included.
- [x] Reusable next-repo contract included.

## Placeholder scan

- [x] No unresolved implementation markers remain.
- [x] Every task has concrete files, behavior, verification and commit boundary.
- [x] Conditional compatibility cases have a safe default/fail-closed behavior.

## Interface consistency

- [x] `OpenWithOptions` is defined before gateway/worker use.
- [x] Worker owns side effects before Blue/Green begins.
- [x] `workerapi.Client` replaces in-process monitor dependencies.
- [x] Journal watcher restores local live hints without changing DB replay authority.
- [x] Slot aliases match Caddy route names.
- [x] Caddy promotion updates state only after reload/probe success.

---

# 13. Official Research References

The implementation should re-check these official sources while executing tasks because security/runtime guidance can change:

- Docker Engine Security: `https://docs.docker.com/engine/security/`
- Docker Rootless: `https://docs.docker.com/engine/security/rootless/`
- Docker userns-remap: `https://docs.docker.com/engine/security/userns-remap/`
- Docker Seccomp: `https://docs.docker.com/engine/security/seccomp/`
- Docker AppArmor: `https://docs.docker.com/engine/security/apparmor/`
- Docker Network / internal networks: `https://docs.docker.com/reference/cli/docker/network/create/`
- Docker Port Publishing: `https://docs.docker.com/engine/network/port-publishing/`
- Docker Firewall: `https://docs.docker.com/engine/network/firewall-iptables/`
- Docker Resource Constraints: `https://docs.docker.com/engine/containers/resource_constraints/`
- Docker `local` logging driver: `https://docs.docker.com/engine/logging/drivers/local/`
- Docker live-restore: `https://docs.docker.com/engine/daemon/live-restore/`
- Docker Compose secrets: `https://docs.docker.com/compose/how-tos/use-secrets/`
- Caddy zero-downtime reload: `https://caddyserver.com/docs/getting-started`
- Caddy config API rollback: `https://caddyserver.com/docs/api`
- Caddy reverse proxy/health/failover: `https://caddyserver.com/docs/caddyfile/directives/reverse_proxy`
- Caddy latest verified release during research: `https://github.com/caddyserver/caddy/releases/tag/v2.11.4`
- Cloudflare Tunnel: `https://developers.cloudflare.com/tunnel/`
- Cloudflare Access for Infrastructure SSH: `https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/use-cases/ssh/ssh-infrastructure-access/`
- Sigstore CI quickstart: `https://docs.sigstore.dev/quickstart/quickstart-ci/`
- Trivy docs: `https://www.trivy.dev/docs/latest/`

---

# 14. Final Architecture Decision

For `acb-transaction-webhook`, the approved target is:

```text
Docker Compose single VPS
+ one shared Cloudflare Tunnel
+ one shared Caddy v2.11.4 edge
+ per-app internal edge network
+ private core network
+ explicit egress network
+ permanent Active/Standby Blue-Green HTTP gateways
+ singleton ACB/notification worker
+ singleton auth-browser
+ independent TTS/Bark lifecycle
+ SQLite WAL + event journal
+ immutable signed image digests
+ scan + SBOM + signature verification
+ health/readiness/deploy gates
+ Caddy failover + Docker restart + bounded host watchdog
+ consistent backup + restore drill
```

The architectural boundary that matters most is:

```text
PUBLIC TRAFFIC LIFECYCLE != EXTERNAL SIDE-EFFECT LIFECYCLE
```

HTTP gateway versions may overlap and fail over. ACB upstream ownership must remain singular and fenced.
