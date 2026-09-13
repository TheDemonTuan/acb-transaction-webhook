# Bark Provider Production Hardening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Hoàn thiện Bark self-host notification provider trong `acb-transaction-webhook` thành trạng thái production-ready: deploy xanh, không làm mất backup, giữ realtime throughput, fail-closed với secrets/config, immutable rollout/rollback, và có observability/test đầy đủ.

**Architecture:** Giữ kiến trúc hiện tại `bank.transaction.credit -> durable deliveries -> notification dispatcher -> WEBHOOK/BARK`. Không viết lại provider model. Kế hoạch này chỉ harden những regression/gap đã tìm thấy trên HEAD `6e4ceafd9314ebebedda82ee3132396d1e28dab4`, đồng thời sửa lỗi deploy Bark đã được xác minh từ GitHub Actions production logs.

**Tech Stack:** Go 1.27.1, SQLite WAL (`modernc.org/sqlite`), React 19, Docker Compose, GitHub Actions, Cloudflare Tunnel, Bark Server API V2.

**Spec:** `PLAN_BARK_NOTIFICATION_PROVIDER.md`

## Global Constraints

- Không bỏ durable outbox/retry/dead-letter hiện tại.
- Không tạo queue riêng cho Bark.
- Một Bark channel vẫn tương ứng một device key/iPhone để retry độc lập.
- Webhook HMAC và SSRF protections phải giữ nguyên.
- Bark device key tiếp tục được AES-GCM encrypt at rest.
- Không log device key, Basic Auth password, Authorization header, raw Bark payload, ACB session/cookie.
- Production không được chạy Bark bằng mutable tag như `latest`.
- Bark channel mới tiếp tục mặc định `DISABLED`.
- Default Bark notification level vẫn là `timeSensitive`.
- Default `includeBalance=false`.
- `REALTIME` và `CATCH_UP` có thể phát notification; `FILTER_SYNC`/`BOOTSTRAP` không phát.
- Production deployment chỉ được đánh dấu thành công khi gateway, auth-browser, TTS và Bark đều qua verification.
- Không đổi physical table names `webhook_*` trong kế hoạch hardening này.
- Mỗi task phải kết thúc bằng test/verification riêng và một commit nhỏ.
- Không được bỏ qua deploy failure hiện tại bằng cách tăng timeout hoặc tắt healthcheck.

---

# 0. Review Baseline và Findings đã xác minh

## Audited revision

```text
HEAD: 6e4ceafd9314ebebedda82ee3132396d1e28dab4
Feature commit: 6a67a4dee195a24a379a192a5576d6ae3f60f675
Previous plan baseline: 64cbe2a08400258c5816b272b549384ab0496f72
```

## CI state tại HEAD

Các gate sau đã pass:

```text
frontend:
- tsc --noEmit
- vite build
- vitest

backend:
- go test -race ./...
- go vet ./...

tts:
- pytest

scripts:
- bash -n deploy/*.sh
- git diff --check

images:
- gateway build
- auth-browser build + smoke test
- tts-gateway build + smoke test
```

Nhưng workflow tổng thể **FAIL** ở:

```text
Deploy to VPS
  -> Deploy immutable image
```

## Root cause deploy Bark đã được xác minh từ Actions log

Container Bark crash-loop:

```text
acb-bark | /bin/sh: can't open '/bark-entrypoint.sh': Permission denied
```

Ngay trước đó deployment sync scripts bằng:

```bash
chmod 750 "$DEPLOY_PATH/"*.sh
```

Trong Compose:

```yaml
entrypoint: ["/bin/sh", "/bark-entrypoint.sh"]
volumes:
  - ./bark-entrypoint.sh:/bark-entrypoint.sh:ro
```

Vì `bark-entrypoint.sh` là bind-mounted vào image upstream, blanket mode `0750` trên host có thể làm user trong Bark container không đọc được script. Đây là P0 đã có bằng chứng runtime, không phải giả thuyết.

## Additional Critical Finding: production backup path không khớp active Docker volume

`deploy/compose.prod.yaml` dùng:

```yaml
volumes:
  gateway_data:
    name: bank-event-gateway_gateway_data

gateway:
  volumes:
    - gateway_data:/data:rw
```

Nhưng `deploy.sh` đang backup:

```bash
DATABASE_PATH="$script_dir/data/gateway.db"
BACKUP_DIR="$script_dir/data/backups"
"$script_dir/backup.sh"
```

Nghĩa là script host có thể backup một file stale/legacy trong `deploy/data/`, trong khi production DB thật nằm trong named volume `bank-event-gateway_gateway_data:/data/gateway.db`.

Actions log cho thấy migration/check chạy trên named volume và đọc được:

```text
transactionsCount=98
eventsCount=82
```

Do đó pre-deployment backup hiện **không đủ bằng chứng là đang backup active DB**. Đây là P0 deployment-safety và phải sửa trước khi gọi rollout/rollback an toàn.

## Remaining hardening findings

```text
P1  Dispatcher provider mới chạy tuần tự thay vì worker pool 4.
P1  Bark image vẫn fallback ghcr.io/finb/bark-server:latest.
P1  verify-deployment.sh chưa verify Bark image/health/auth.
P1  smoke-test-bark.sh được copy lên VPS nhưng không được chạy.
P1  rotateChannelSecret() ignore kết quả decode() và có thể side-effect sau HTTP 4xx.
P1  Bark config có hai loader độc lập; một path fail-open/log-warning.
P1  Bark entrypoint không fail nếu auth secret thiếu/rỗng.

P2  Bark 3xx bị retry thay vì terminal.
P2  Bark response body bound 64 KiB thay vì nhỏ, transport timeout chưa chia nhỏ.
P2  Bark telemetry/provider summary chưa đầy đủ.
P2  Registry dùng magic strings thay vì provider constants.
```

---

# 1. File Responsibility Map

```text
internal/notification/sender.go
  Provider constants, Sender contract, SendResult.

internal/notification/dispatcher.go
  Worker pool, leasing orchestration, retry/dead-letter state transitions.

internal/notification/dispatcher_test.go
  Mixed-provider, concurrency, retry/replay tests.

internal/bark/config.go
  Chỉ giữ Bark runtime Config type/validation helper.

internal/bark/sender.go
  HTTP client, request payload, response classification.

internal/bark/sender_test.go
  HTTP status/timeout/redirect/body-limit tests.

internal/config/config.go
  Single source of truth cho env/file config.

internal/config/config_test.go
  Production validation.

cmd/gateway/main.go
  Construct providers từ config.Config; operational backup command.

internal/httpapi/notifications.go
  CRUD/test/replay notification APIs.

internal/httpapi/notifications_test.go
  Body validation, permissions, no-side-effect tests.

internal/storage/backup.go
  Safe SQLite backup API.

internal/storage/backup_test.go
  Backup integrity/count tests.

deploy/bark-entrypoint.sh
  Fail-closed secret loading, start Bark.

deploy/compose.prod.yaml
  Immutable Bark ref only, Bark service runtime.

deploy/deploy.sh
  Image validation, real named-volume backup, start/verify.

deploy/verify-deployment.sh
  Verify Bark health + exact image.

deploy/smoke-test-bark.sh
  Internal authenticated/unauthenticated API smoke checks.

deploy/rollback.sh
  Restore previous Bark digest together with other images.

.github/workflows/deploy.yml
  Pass exact Bark digest and correct file permissions.
```

---

# Task 1: Fix confirmed Bark crash-loop caused by entrypoint permissions

**Severity:** P0

**Files:**
- Modify: `.github/workflows/deploy.yml`
- Modify: `deploy/deploy.sh`
- Modify: `deploy/bark-entrypoint.sh`
- Modify: `deploy/compose.prod.yaml`

**Interfaces:**
- Consumes: Bark service bind-mount `/bark-entrypoint.sh`.
- Produces: Bark container can always read entrypoint; startup fails clearly if required secret files are unreadable/empty.

- [ ] **Step 1: Remove blanket permission change**

Replace remote:

```bash
chmod 750 "$DEPLOY_PATH/"*.sh
```

with:

```bash
chmod 0750   "$DEPLOY_PATH/deploy.sh"   "$DEPLOY_PATH/rollback.sh"   "$DEPLOY_PATH/verify-deployment.sh"   "$DEPLOY_PATH/backup.sh"   "$DEPLOY_PATH/smoke-test-bark.sh"

chmod 0644 "$DEPLOY_PATH/bark-entrypoint.sh"
```

- [ ] **Step 2: Make Bark secret loading fail closed**

Use:

```sh
read_secret() {
    path="$1"
    name="$2"

    if [ ! -r "$path" ]; then
        echo "fatal: $name secret is missing or unreadable: $path" >&2
        exit 1
    fi

    value="$(tr -d '\r\n' < "$path")"
    if [ -z "$value" ]; then
        echo "fatal: $name secret is empty" >&2
        exit 1
    fi

    printf '%s' "$value"
}

export BARK_SERVER_BASIC_AUTH_USER="$(
    read_secret /run/secrets/bark_basic_auth_user BARK_SERVER_BASIC_AUTH_USER
)"
export BARK_SERVER_BASIC_AUTH_PASSWORD="$(
    read_secret /run/secrets/bark_basic_auth_password BARK_SERVER_BASIC_AUTH_PASSWORD
)"
```

Keep:

```sh
if [ "${1:-}" = "bark-server" ]; then
    shift
fi
```

- [ ] **Step 3: Add host preflight**

In `deploy/deploy.sh`:

```bash
[[ -r "$script_dir/bark-entrypoint.sh" ]] || {
  printf 'Bark entrypoint is not readable: %s\n' "$script_dir/bark-entrypoint.sh" >&2
  exit 1
}

[[ -s "$bark_user_file" ]] || exit 1
[[ -s "$bark_pass_file" ]] || exit 1
```

- [ ] **Step 4: Add real container preflight**

```bash
docker compose --env-file "$env_file" -f "$compose_file" run   --rm --no-deps   --entrypoint /bin/sh   bark   -c 'test -r /bark-entrypoint.sh &&
      test -r /run/secrets/bark_basic_auth_user &&
      test -r /run/secrets/bark_basic_auth_password'
```

Expected: exit `0`.

- [ ] **Step 5: Add CI permission regression check**

```bash
tmp="$(mktemp -d)"
cp deploy/bark-entrypoint.sh "$tmp/bark-entrypoint.sh"
chmod 0644 "$tmp/bark-entrypoint.sh"
test -r "$tmp/bark-entrypoint.sh"
test "$(stat -c '%a' "$tmp/bark-entrypoint.sh")" = "644"
```

- [ ] **Step 6: Commit**

```bash
git add .github/workflows/deploy.yml deploy
git commit -m "fix(deploy): make Bark entrypoint readable and fail closed"
```

---

# Task 2: Fix production backup so it backs up the real named-volume database

**Severity:** P0

**Files:**
- Create: `internal/storage/backup.go`
- Create: `internal/storage/backup_test.go`
- Modify: `cmd/gateway/main.go`
- Modify: `deploy/deploy.sh`
- Modify: `deploy/backup.sh`
- Modify: `deploy/README.md`

**Interfaces:**

```go
func (s *Store) Backup(ctx context.Context, destination string) error
```

CLI:

```text
/gateway --backup-to /backup/gateway-YYYYMMDDHHMMSS.db
```

- [ ] **Step 1: Write failing backup test**

Create a live DB, insert one known row, call `Store.Backup`, open backup read-only, assert row exists.

- [ ] **Step 2: Implement backup through SQLite, not filesystem copy**

Preferred:

```go
func (s *Store) Backup(ctx context.Context, destination string) error {
    if !filepath.IsAbs(destination) {
        return errors.New("backup destination must be absolute")
    }

    if err := os.MkdirAll(filepath.Dir(destination), 0o750); err != nil {
        return err
    }

    s.writeMu.Lock()
    defer s.writeMu.Unlock()

    if _, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
        return fmt.Errorf("checkpoint WAL: %w", err)
    }

    escaped := strings.ReplaceAll(destination, "'", "''")
    if _, err := s.db.ExecContext(ctx, "VACUUM INTO '"+escaped+"'"); err != nil {
        return fmt.Errorf("backup database: %w", err)
    }

    return os.Chmod(destination, 0o600)
}
```

If `VACUUM INTO` is unsupported in this exact driver path, use the driver's online backup API. Do not manually copy `.db/.wal/.shm`.

- [ ] **Step 3: Add `--backup-to`**

In `cmd/gateway/main.go`:

```go
backupTo := flag.String("backup-to", "", "create SQLite backup and exit")
```

After `storage.Open()`:

```go
if *backupTo != "" {
    if err := store.Backup(ctx, *backupTo); err != nil {
        logger.Error("database backup failed", "error", err)
        os.Exit(1)
    }
    return
}
```

- [ ] **Step 4: Back up through Compose so `/data` is the active named volume**

```bash
backup_dir="$script_dir/backups"
mkdir -p "$backup_dir"
chmod 0700 "$backup_dir"

backup_name="gateway-$(date -u +%Y%m%d%H%M%S).db"

docker compose --env-file "$env_file" -f "$compose_file" run   --rm --no-deps   --entrypoint /gateway   -v "$backup_dir:/backup"   gateway   --backup-to "/backup/$backup_name"
```

- [ ] **Step 5: Stop treating `$DEPLOY_PATH/data/gateway.db` as production truth**

Production `backup.sh` must either wrap the Compose command above or clearly refuse to infer the active DB from the host path.

- [ ] **Step 6: Run**

```bash
go test -race ./internal/storage
go test -race ./...
```

- [ ] **Step 7: Commit**

```bash
git add internal/storage cmd/gateway deploy
git commit -m "fix(deploy): back up the active gateway Docker volume"
```

---

# Task 3: Prevent malformed rotate-secret requests from performing side effects

**Severity:** P1

**Files:**
- Modify: `internal/httpapi/notifications.go`
- Modify: `internal/httpapi/notifications_test.go`

- [ ] **Step 1: Add regression tests**

Cases:

```text
malformed JSON -> 400 + active secret unchanged
wrong Content-Type -> 415 + active secret unchanged
```

- [ ] **Step 2: Fix control flow**

Replace:

```go
_ = decode(w, r, &in)
```

with:

```go
if !decode(w, r, &in) {
    return
}
```

Webhook callers with no arguments must send `{}`.

- [ ] **Step 3: Run**

```bash
go test -race ./internal/httpapi
```

- [ ] **Step 4: Commit**

```bash
git add internal/httpapi
git commit -m "fix(api): prevent secret rotation after decode failure"
```

---

# Task 4: Make `config.Config` the only source of truth for Bark runtime config

**Severity:** P1

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `internal/bark/config.go`
- Modify: `cmd/gateway/main.go`

- [ ] **Step 1: Add config tests**

Required cases:

```text
BARK_SERVER_URL set, missing user            -> error in production
BARK_SERVER_URL set, missing password        -> error in production
BARK_PUBLIC_URL=http://...                   -> error in production
BARK_SERVER_URL=ftp://...                    -> error
BARK_SERVER_URL=http://bark:8080             -> valid
BARK_PUBLIC_URL=https://push.example.com     -> valid
query/userinfo/fragment                      -> error
unreadable *_FILE                            -> error
```

- [ ] **Step 2: Require `http|https` for server URL**

```go
switch strings.ToLower(u.Scheme) {
case "http", "https":
default:
    return Config{}, fmt.Errorf("BARK_SERVER_URL must use http or https")
}
```

- [ ] **Step 3: Require HTTPS public URL in production**

```go
if production && barkPublicURL != "" {
    pu, err := url.Parse(barkPublicURL)
    if err != nil || pu.Scheme != "https" || pu.Host == "" {
        return Config{}, fmt.Errorf(
            "BARK_PUBLIC_URL must be an absolute https URL in production",
        )
    }
}
```

- [ ] **Step 4: Require Basic Auth pair**

```go
if production && barkServerURL != "" {
    if barkAuthUser == "" || barkAuthPassword == "" {
        return Config{}, fmt.Errorf(
            "Bark basic auth user and password are required in production",
        )
    }
}
```

- [ ] **Step 5: Remove `bark.LoadConfigFromEnv()`**

`internal/bark` no longer reads environment.

- [ ] **Step 6: Construct Bark config from `cfg` once in main**

```go
barkCfg := bark.Config{
    ServerURL:         cfg.BarkServerURL,
    PublicURL:         cfg.BarkPublicURL,
    BasicAuthUser:     cfg.BarkBasicAuthUser,
    BasicAuthPassword: cfg.BarkBasicAuthPassword,
    Timeout:           cfg.BarkTimeout,
    DefaultGroup:      cfg.BarkDefaultGroup,
    DefaultLevel:      cfg.BarkDefaultLevel,
    DefaultSound:      cfg.BarkDefaultSound,
}
```

- [ ] **Step 7: Run**

```bash
go test -race ./internal/config ./internal/bark
go test -race ./...
```

- [ ] **Step 8: Commit**

```bash
git add internal/config internal/bark cmd/gateway
git commit -m "refactor(config): centralize Bark runtime configuration"
```

---

# Task 5: Restore a bounded 4-worker notification dispatcher

**Severity:** P1

**Files:**
- Modify: `internal/notification/dispatcher.go`
- Modify: `internal/notification/dispatcher_test.go`

**Required behavior:**

```text
default concurrency = 4
max 1 in-flight per endpoint/channel
one slow Bark device cannot block unrelated channels
bounded goroutines
graceful cancellation
```

- [ ] **Step 1: Write a concurrency regression test**

Create 4 active channels and a blocking sender. Start dispatcher, wake it, assert all 4 distinct channel deliveries enter `Send()` before any are released.

Current sequential implementation should fail.

- [ ] **Step 2: Add worker count**

```go
const defaultWorkers = 4
```

and:

```go
func (d *Dispatcher) SetWorkerCount(n int) *Dispatcher
```

- [ ] **Step 3: Add bounded work signaling**

```go
type Dispatcher struct {
    store      *storage.Store
    registry   *Registry
    maxRetries int
    backoffs   []time.Duration
    workers    int
    wakeCh     chan struct{}
    workCh     chan struct{}
}
```

- [ ] **Step 4: Add worker loop**

```go
func (d *Dispatcher) worker(ctx context.Context) {
    for {
        select {
        case <-ctx.Done():
            return
        case <-d.workCh:
            for {
                processed, err := d.DispatchOne(ctx)
                if err != nil || !processed {
                    break
                }
            }
        }
    }
}
```

- [ ] **Step 5: Wake all workers without unbounded goroutines**

```go
func (d *Dispatcher) kickWorkers() {
    for i := 0; i < d.workers; i++ {
        select {
        case d.workCh <- struct{}{}:
        default:
            return
        }
    }
}
```

- [ ] **Step 6: Preserve per-endpoint DB serialization**

Do not remove the existing `NOT EXISTS` lease guard in `ClaimDelivery`.

- [ ] **Step 7: Add same-endpoint serialization test**

Two due deliveries for one endpoint must never enter sender concurrently.

- [ ] **Step 8: Run race detector**

```bash
go test -race ./internal/notification ./internal/storage
```

- [ ] **Step 9: Commit**

```bash
git add internal/notification
git commit -m "fix(notification): restore bounded concurrent delivery workers"
```

---

# Task 6: Harden Bark HTTP transport and retry classification

**Severity:** P1/P2

**Files:**
- Modify: `internal/bark/sender.go`
- Modify: `internal/bark/sender_test.go`

## Required classification

```text
2xx + Bark code 200 -> SUCCESS

network/timeout -> RETRY
408             -> RETRY
429             -> RETRY
5xx             -> RETRY

3xx             -> TERMINAL / BARK_REDIRECT_REJECTED
400             -> TERMINAL / BARK_BAD_REQUEST
401/403/418     -> TERMINAL / BARK_AUTH_FAILED
404             -> TERMINAL / BARK_NOT_FOUND
410             -> TERMINAL / BARK_DEVICE_GONE
413             -> TERMINAL / BARK_PAYLOAD_TOO_LARGE
422             -> TERMINAL / BARK_BAD_REQUEST
other 4xx       -> TERMINAL
```

- [ ] **Step 1: Add table-driven response tests**

Include 302, 400, 401, 418, 429, 500, 503.

- [ ] **Step 2: Use explicit transport**

```go
&http.Transport{
    Proxy:                 http.ProxyFromEnvironment,
    DialContext:           (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
    TLSHandshakeTimeout:   3 * time.Second,
    ResponseHeaderTimeout: 3 * time.Second,
    IdleConnTimeout:       60 * time.Second,
    MaxIdleConns:          16,
    MaxIdleConnsPerHost:   8,
}
```

Keep total timeout from config.

- [ ] **Step 3: Bound Bark response body at 16 KiB**

```go
const maxResponseBody = 16 << 10
```

- [ ] **Step 4: Do not persist raw network error strings**

Use stable:

```text
NETWORK_TIMEOUT
NETWORK_ERROR
Bark network request failed
```

- [ ] **Step 5: Make 3xx terminal**

```go
if resp.StatusCode >= 300 && resp.StatusCode < 400 {
    return notification.SendResult{
        Outcome:           notification.OutcomeTerminalFailure,
        StatusCode:        resp.StatusCode,
        LatencyMs:         latencyMs,
        ProviderErrorCode: "BARK_REDIRECT_REJECTED",
        SanitizedError:    "Bark server returned a redirect",
    }
}
```

- [ ] **Step 6: Run**

```bash
go test -race ./internal/bark
```

- [ ] **Step 7: Commit**

```bash
git add internal/bark
git commit -m "fix(bark): harden HTTP transport and retry classification"
```

---

# Task 7: Pin Bark to an immutable digest through deploy and rollback

**Severity:** P1

**Files:**
- Modify: `.github/workflows/deploy.yml`
- Modify: `deploy/deploy.sh`
- Modify: `deploy/rollback.sh`
- Modify: `deploy/compose.prod.yaml`
- Modify: `deploy/README.md`

- [ ] **Step 1: Remove `latest` fallback**

```yaml
image: ${BARK_IMAGE_REF:?BARK_IMAGE_REF immutable digest is required}
```

- [ ] **Step 2: Extend deploy CLI**

```text
deploy.sh <gateway> <auth-browser> <tts> <bark> [staged-compose]
```

Validate Bark with same immutable digest regex.

- [ ] **Step 3: Require repo/environment variable**

```text
BARK_IMAGE_REF=ghcr.io/finb/bark-server@sha256:<64 hex>
```

- [ ] **Step 4: Pass Bark digest explicitly over SSH**

Remote deploy:

```bash
./deploy.sh   "$IMAGE"   "$BROWSER_IMAGE"   "$TTS_IMAGE"   "$BARK_IMAGE"   "$staged_compose"
```

- [ ] **Step 5: Store `.deployed-bark-image` only after verification**

Same semantics as gateway/TTS.

- [ ] **Step 6: Make rollback require valid previous Bark digest**

No fallback to current or `latest` when doing a rollback.

- [ ] **Step 7: Commit**

```bash
git add .github/workflows/deploy.yml deploy
git commit -m "fix(deploy): require immutable Bark image digest"
```

---

# Task 8: Verify Bark as a first-class production service

**Severity:** P1

**Files:**
- Modify: `deploy/verify-deployment.sh`
- Modify: `deploy/smoke-test-bark.sh`
- Modify: `deploy/deploy.sh`

- [ ] **Step 1: Verify exact Bark image**

```bash
expected_bark_image="${BARK_IMAGE_REF:-}"
verify_image   "${BARK_CONTAINER:-acb-bark}"   "$expected_bark_image"   "bark"
```

- [ ] **Step 2: Wait for Bark health**

Bounded 60-second loop reading Docker health status.

- [ ] **Step 3: Smoke test auth without sending APNs notification**

Required:

```text
GET /healthz -> 200
GET /ping -> pong
POST /push unauthenticated -> rejected
POST /push authenticated with {} -> request reaches Bark parser
```

Do not provide a real device key during deployment smoke.

- [ ] **Step 4: Run Bark verification before recording deployed digests**

Order:

```text
compose up
gateway verify
auth-browser verify
TTS verify
Bark verify
Bark auth smoke
write .deployed-* markers
Deployment successful
```

- [ ] **Step 5: On failure print Bark diagnostics**

```bash
docker inspect acb-bark
docker compose ... logs --tail 100 bark
```

Never cat secret files.

- [ ] **Step 6: Commit**

```bash
git add deploy
git commit -m "test(deploy): verify Bark health auth and immutable image"
```

---

# Task 9: Add provider constants and remove routing magic strings

**Severity:** P2

**Files:**
- Create: `internal/provider/provider.go`
- Modify provider comparisons across backend.

- [ ] **Step 1: Add constants**

```go
package provider

const (
    Webhook = "WEBHOOK"
    Bark    = "BARK"
)
```

- [ ] **Step 2: Replace routing magic strings**

Examples:

```go
registry.Register(provider.Bark, barkSender)
if target.Provider == provider.Webhook { ... }
```

- [ ] **Step 3: No DB migration**

Persisted values remain exactly `WEBHOOK` and `BARK`.

- [ ] **Step 4: Run**

```bash
go test -race ./...
```

- [ ] **Step 5: Commit**

```bash
git add internal cmd
git commit -m "refactor(notification): centralize provider identifiers"
```

---

# Task 10: Add provider-specific observability

**Severity:** P2

**Files:**
- Modify: current telemetry implementation
- Modify: `internal/notification/dispatcher.go`
- Modify: `internal/storage/queries.go`
- Modify: `internal/httpapi/server.go`
- Modify frontend types/status UI/tests as required.

- [ ] **Step 1: Add grouped delivery summary query**

```sql
SELECT COALESCE(e.provider, 'WEBHOOK'),
       d.status,
       count(*)
FROM deliveries d
JOIN webhook_endpoints e ON e.id = d.endpoint_id
GROUP BY COALESCE(e.provider, 'WEBHOOK'), d.status
```

- [ ] **Step 2: Add generic telemetry API**

```go
RecordNotification(provider string, duration time.Duration, success bool)
```

- [ ] **Step 3: Remove Webhook-only telemetry branch**

Bark and Webhook both record success/failure/latency.

- [ ] **Step 4: Fix `/api/v1/status`**

Do not assign the same aggregate summary to both `"webhooks"` and `"notifications"`.

Return:

```json
{
  "notifications": {
    "total": {},
    "byProvider": {
      "WEBHOOK": {},
      "BARK": {}
    }
  }
}
```

Keep legacy `"webhooks"` only if needed for compatibility.

- [ ] **Step 5: Keep metric labels low-cardinality**

Allowed:

```text
provider
outcome
```

Forbidden:

```text
endpointId
deviceKey
transactionId
account
description
```

- [ ] **Step 6: Run**

```bash
go test -race ./...
cd web
bunx --bun tsc --noEmit
bunx --bun vitest run
```

- [ ] **Step 7: Commit**

```bash
git add internal web
git commit -m "feat(observability): report notification health by provider"
```

---

# Task 11: Add full Bark failure/recovery matrix

**Severity:** Release Gate

**Files:**
- Modify: `internal/bark/sender_test.go`
- Modify: `internal/notification/dispatcher_test.go`
- Modify: `internal/storage/notifications_test.go`
- Modify: `internal/httpapi/notifications_test.go`
- Modify: `web/e2e/notification-channels.spec.ts`

- [ ] **Step 1: Sender matrix**

```text
200/code200          success
400                  terminal
401/403/418          terminal auth
429                  retry
500/503              retry
302                  terminal redirect
network timeout      retry
oversized response   bounded
```

- [ ] **Step 2: Dispatcher matrix**

```text
Webhook + Bark both succeed
slow Bark does not block unrelated Webhook
two Bark devices independent
retry exhaustion -> DEAD_LETTER
replay -> fresh retry cycle
context cancel -> workers exit
```

- [ ] **Step 3: Storage secret matrix**

```text
device key encrypted
list API never returns plaintext
rotation retires old key
pending deliveries remain decryptable according to snapshot semantics
new deliveries use new key
```

Important: if current rotation makes old pending deliveries undecryptable, fix that before release.

- [ ] **Step 4: API matrix**

```text
malformed body -> zero side effect
test while DISABLED -> allowed
Bark create -> no device key in response
Bark rotate -> no device key in response
Webhook HMAC secret -> one-time response behavior preserved
```

- [ ] **Step 5: UI E2E**

```text
create Bark
test
enable
edit config
rotate key
key no longer displayed
```

- [ ] **Step 6: Full gate**

```bash
go test -race ./...
go vet ./...

cd web
bun install --frozen-lockfile
bunx --bun tsc --noEmit
bunx --bun vitest run
bunx --bun vite build

cd ..
python -m pytest tts-gateway/tests -v
bash -n deploy/*.sh
git diff --check
```

Also run Compose config validation with a fixture env containing a valid immutable `BARK_IMAGE_REF`.

- [ ] **Step 7: Commit**

```bash
git add internal web deploy
git commit -m "test(bark): cover provider failure and recovery matrix"
```

---

# Task 12: Production rollout and rollback drill

**Severity:** Final Release Gate

**Files:**
- Modify: `deploy/README.md`

- [ ] **Step 1: Confirm all CI jobs green**

No deploy failure may be ignored.

- [ ] **Step 2: Confirm the new backup is from active named volume**

Compare live and backup inventory:

```text
transactions
events
schema migrations
```

- [ ] **Step 3: Deploy**

Expected ordered gates:

```text
migration
integrity
Bark preflight
compose up
gateway health
auth-browser health
TTS health
Bark health
Bark auth smoke
Deployment successful
```

- [ ] **Step 4: Create one Bark channel**

```text
Create -> DISABLED
Send Test -> received on iPhone
Enable
```

- [ ] **Step 5: Verify exactly-once app-level behavior for one real credit**

```text
1 canonical event
1 Bark channel
=> 1 Bark delivery
=> 1 iPhone notification
```

- [ ] **Step 6: Bark outage drill**

```bash
docker stop acb-bark
```

Expected:

```text
Bark delivery retries
gateway remains healthy
ACB monitor continues
Webhook remains independent
```

Restart:

```bash
docker start acb-bark
```

Expected eventual delivery.

- [ ] **Step 7: Rollback drill**

```bash
./deploy/rollback.sh
```

Expected:

```text
previous gateway digest
previous auth-browser digest
previous TTS digest
previous Bark digest
same database state
all health checks green
```

- [ ] **Step 8: Document timings**

Record only aggregate timing:

```text
ACB detection latency
enqueue latency
Bark server latency
perceived iPhone latency
```

No sensitive identifiers.

---

# Final Acceptance Checklist

## Deployment
- [ ] GitHub Actions fully green.
- [ ] Bark entrypoint permission bug is gone.
- [ ] Bark secret loading fails closed.
- [ ] Bark image is immutable.
- [ ] Bark exact image is verified.
- [ ] Bark health is verified.
- [ ] Bark unauthenticated push is rejected.
- [ ] Bark smoke test is executed, not merely copied.

## Data Safety
- [ ] Production backup targets the real named volume.
- [ ] Backup inventory matches live DB.
- [ ] Rollback preserves DB.

## Runtime
- [ ] Dispatcher has 4 bounded workers.
- [ ] One slow provider cannot block unrelated channels.
- [ ] One endpoint remains serialized.
- [ ] Retry/dead-letter behavior is deterministic.
- [ ] Multiple Bark devices are isolated deliveries.

## Security
- [ ] Device key encrypted.
- [ ] Device key never returned after create/rotate.
- [ ] Device key and Basic Auth never logged.
- [ ] Production Bark public URL requires HTTPS.
- [ ] Basic Auth pair is mandatory.
- [ ] Webhook HMAC/SSRF behavior unchanged.
- [ ] Malformed rotate request cannot mutate secret.

## Observability
- [ ] Delivery health is broken down by provider.
- [ ] Bark latency/success/failure recorded.
- [ ] No sensitive high-cardinality labels.

## UX
- [ ] Bark starts DISABLED.
- [ ] Test works before enable.
- [ ] Default timeSensitive.
- [ ] Balance hidden by default.
- [ ] CATCH_UP clearly labeled.

---

# Recommended Execution Order

```text
Task 1  confirmed Bark entrypoint crash
  -> Task 2  real named-volume backup
  -> Task 3  rotate-secret side-effect
  -> Task 4  config single source
  -> Task 5  restore 4 workers
  -> Task 6  HTTP/retry hardening
  -> Task 7  immutable Bark digest
  -> Task 8  Bark deploy verification
  -> Task 9  provider constants
  -> Task 10 observability
  -> Task 11 failure matrix
  -> Task 12 rollout + rollback drill
```

Do not fix the current deployment by increasing timeout or disabling the Bark healthcheck. The runtime log already identifies the immediate failure as entrypoint file permission, and the backup path mismatch must be corrected before relying on rollback safety.
