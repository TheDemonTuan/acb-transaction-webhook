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

# Bark Self-Hosted Notification Provider Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Tích hợp Bark self-host như một notification provider hạng nhất trong `acb-transaction-webhook`, dùng chung durable event/outbox/retry/dead-letter hiện có, gửi thông báo giao dịch ACB gần realtime tới iPhone nhưng không biến Bark thành một generic webhook chắp vá.

**Architecture:** Giữ `bank.transaction.credit` hiện tại làm canonical event. Refactor dispatcher thành provider-aware dispatcher với registry (`WEBHOOK`, `BARK`). Không tạo queue riêng cho Bark. Để giảm rủi ro migration production, giữ nguyên tên vật lý legacy `webhook_endpoints`, `endpoint_versions`, `endpoint_secrets`, `deliveries` trong SQLite ở phase này nhưng mở rộng chúng bằng `provider` và `provider_config_json`; code/API/UI dùng khái niệm generic `NotificationChannel`. Bark Server chạy container riêng trong cùng Compose, gateway gọi `http://bark:8080/push` qua private Docker network; iPhone đăng ký vào Bark Server qua hostname Cloudflare Tunnel riêng. Bark `/push` được bảo vệ bằng Basic Auth; device key được mã hóa AES-256-GCM trong DB bằng master key hiện tại.

**Tech Stack:** Go 1.27.1, SQLite WAL (`modernc.org/sqlite`), React 19 + TypeScript + TanStack Query, Docker Compose, Cloudflare Tunnel, Bark Server API V2 (`POST /push`), Apple APNs.

## Global Constraints

- Không tạo một Bark webhook receiver/bridge riêng. Bark là provider trong gateway.
- Không bypass durable outbox: mọi notification giao dịch thật phải đi qua `events -> deliveries -> dispatcher`.
- Không hard-code hoặc log Bark device key, Basic Auth password, ACB session, account number đầy đủ.
- Không cho UI nhập `BARK_SERVER_URL`; URL transport Bark là deployment config đáng tin cậy, không phải user-controlled URL. Điều này giữ nguyên ranh giới SSRF hiện tại.
- Generic webhook vẫn phải giữ HMAC, public-IP SSRF validation và API compatibility hiện tại.
- Một Bark channel tương ứng một iPhone/device key trong v1. Không dùng `device_keys` batch vì cần retry/dead-letter độc lập theo từng thiết bị.
- Bark channel mới luôn tạo ở trạng thái `DISABLED`; phải `Send Test` thành công rồi người dùng chủ động enable.
- `REALTIME` và `CATCH_UP` đều có thể phát notification credit như webhook hiện tại; notification CATCH_UP phải được gắn nhãn rõ để không giả vờ là vừa xảy ra.
- `FILTER_SYNC` và `BOOTSTRAP` tiếp tục không phát notification.
- Không đổi physical table names `webhook_*` trong phase này. Việc rename/rebuild SQLite là debt riêng sau khi provider model ổn định.
- Production Bark image phải pin immutable digest, không dùng `latest` trong deployment thực tế.
- Cloudflare Access interactive login không đặt trước hostname Bark vì Bark iOS không thể thực hiện login flow đó. Dùng Tunnel + Bark Basic Auth + rate limiting; `/register`, `/ping`, `/healthz` của Bark upstream vốn được miễn Basic Auth.
- Default Bark notification level là `timeSensitive`, không mặc định `critical`.
- Default `includeBalance=false` để giảm lộ dữ liệu nhạy cảm trên lock screen; người dùng có thể bật trong channel config.

---

## 1. Research snapshot / hiện trạng đã xác minh

Plan này được viết trên `main` tại commit:

```text
64cbe2a08400258c5816b272b549384ab0496f72
```

Các điểm hiện tại cần tận dụng:

1. `cmd/gateway/main.go`
   - khởi tạo `webhook.NewDispatcher(store, nil)`;
   - chạy 4-worker dispatcher ở background;
   - monitor publish `EventNotification` sang SSE hub rồi gọi `dispatcher.Wake()`.

2. `internal/storage/transactions.go`
   - ingest transaction + event + delivery + journal trong cùng SQLite transaction;
   - canonical event `bank.transaction.credit` đã có đủ `credit`, `balance`, `description`, `transactionNumber`, `source`, `accountMasked`, `detectedAt`;
   - `REALTIME` và `CATCH_UP` hiện enqueue webhook; `FILTER_SYNC`/`BOOTSTRAP` bị suppress.

3. `internal/storage/deliveries.go`
   - durable leasing;
   - một in-flight delivery/endpoint;
   - retry-safe claim token;
   - crash recovery qua expired lease.

4. `internal/webhook/dispatcher.go`
   - retry schedule: 2s, 5s, 15s, 30s, 1m, 2m, 5m, 15m;
   - worker pool 4;
   - hiện đang hard-code HTTP webhook signing/URL validation.

5. `internal/webhook/webhook.go`
   - HMAC-SHA256;
   - public-only dialer chống SSRF;
   - redirect disabled;
   - helper `IsRetriable()` đã định nghĩa 408/429/5xx/network là retryable nhưng dispatcher cũ chưa dùng đúng contract này.

6. `web/src/pages/admin/NotificationChannelsPage.tsx`
   - tên page đã là Notification Channels nhưng UI bên trong vẫn chỉ quản lý webhook URL;
   - đây là đúng nơi để thêm provider selector/card Bark, không tạo page mới.

7. `deploy/compose.prod.yaml`
   - gateway/auth-browser/tts-gateway đã harden bằng `cap_drop`, `no-new-privileges`, healthcheck, resource limits;
   - external edge network `edge-acb` đã dùng cho Cloudflare Tunnel;
   - Bark nên theo cùng convention.

8. Bark upstream
   - image: `ghcr.io/finb/bark-server`;
   - data mặc định `/data`;
   - health: `/ping`, `/healthz`;
   - API V2: `POST /push` với `device_key`, `body`, optional `title`, `level`, `group`, `sound`, `url`, `isArchive`, ...;
   - `level`: `critical | active | timeSensitive | passive`;
   - Basic Auth config bằng `BARK_SERVER_BASIC_AUTH_USER` và `BARK_SERVER_BASIC_AUTH_PASSWORD`;
   - upstream cố ý miễn auth cho `/ping`, `/register`, `/healthz`, còn `/push` phải auth khi Basic Auth bật;
   - auth failure upstream hiện trả HTTP `418`, vì vậy provider phải classify riêng thay vì retry vô hạn.

### Blocker P0 phát hiện trong source

`internal/storage/storage.go` tạo `delivery_attempts` với các cột:

```text
outcome
sanitized_error
created_at
```

nhưng `internal/storage/events.go::RecordAttempt()` hiện INSERT vào:

```text
error_envelope
executed_at
```

Đây là schema-contract mismatch. Bark sẽ dùng chính delivery log này nên phải fix và khóa bằng test trước khi refactor dispatcher.

---

## 2. Target architecture

```text
                         ACB ONE WEB
                              |
                              v
                      ACB Monitor / Parser
                              |
                              v
                   IngestTransactionsBatch
                              |
                 SQLite atomic transaction
                              |
              +---------------+----------------+
              |                                |
              v                                v
      bank.transaction.credit             event_journal
              |
              v
          deliveries
              |
              v
      Notification Dispatcher
              |
       Provider Registry
        /            \
       /              \
      v                v
 WEBHOOK Provider    BARK Provider
      |                |
 HMAC + SSRF       render payload
      |           + Basic Auth
      v                |
 public HTTPS          v
 endpoint        http://bark:8080/push
                       |
                       v
                   Bark Server
                       |
                       v
                    Apple APNs
                       |
                       v
                    iPhone
```

Public onboarding path:

```text
iPhone Bark App
      |
      | HTTPS
      v
Cloudflare Tunnel hostname
      |
      v
edge-acb -> acb-bark:8080
      |
      +-> /register  (upstream auth-free)
```

Transaction push path không đi qua Cloudflare:

```text
gateway -> private Compose network -> bark:8080 -> APNs
```

---

## 3. Provider contracts

Create `internal/notification/provider.go`:

```go
package notification

import (
    "context"
    "encoding/json"
)

const (
    ProviderWebhook = "WEBHOOK"
    ProviderBark    = "BARK"
)

type Target struct {
    ID       string
    Name     string
    Provider string
    Revision int
    URL      string
    Config   json.RawMessage
    Secret   []byte
}

type Event struct {
    ID      string
    Type    string
    Payload []byte
}

type Request struct {
    DeliveryID string
    Target     Target
    Event      Event
}

type Result struct {
    Success        bool
    Retriable      bool
    HTTPStatus     int
    Outcome        string
    ProviderCode   string
    SanitizedError string
}

type Provider interface {
    Kind() string
    Deliver(ctx context.Context, req Request) Result
}
```

Provider registry behavior:

```text
WEBHOOK -> current signed HTTP behavior
BARK    -> Bark API V2 behavior
unknown -> terminal DEAD_LETTER / UNKNOWN_PROVIDER
```

Dispatcher owns:

- claim/lease;
- load target + event;
- provider lookup;
- attempt recording;
- retry/backoff/dead-letter;
- telemetry.

Provider owns:

- transform/render payload;
- network call;
- response classification;
- provider-specific redaction.

Provider không được tự update delivery DB.

---

# Implementation Tasks

## Task 1: Lock the existing delivery schema contract before changing architecture

**Files:**
- Modify: `internal/storage/events.go`
- Modify: `internal/storage/deliveries_test.go`
- Modify: `internal/webhook/dispatcher_test.go`

- [ ] Add a regression test that opens a brand-new SQLite DB via `storage.Open()`, creates a transaction/event/delivery, records one failed attempt, and reads it back.
- [ ] Make the test fail against the current `error_envelope/executed_at` INSERT.
- [ ] Change `RecordAttempt` to use the columns actually created by migration 1:

```sql
INSERT INTO delivery_attempts(
    id,
    delivery_id,
    attempt_number,
    status_code,
    latency_ms,
    outcome,
    sanitized_error,
    created_at
) VALUES(?,?,?,?,?,?,?,?)
```

- [ ] Change the method signature to make outcome explicit:

```go
func (s *Store) RecordAttempt(
    ctx context.Context,
    deliveryID string,
    attemptNum int,
    httpStatus int,
    durationMs int,
    outcome string,
    sanitizedError string,
) error
```

- [ ] Allowed outcomes initially:

```text
SUCCESS
RETRY
TERMINAL_FAILURE
```

- [ ] Ensure raw response bodies, device keys, HMAC secrets and Authorization headers are never put into `sanitized_error`.

**Verification:**

```bash
go test -race ./internal/storage ./internal/webhook
```

---

## Task 2: Add provider metadata to the existing storage model without destructive table renames

**Files:**
- Modify: `internal/storage/storage.go`
- Modify: `internal/storage/repository.go`
- Modify: `internal/storage/events.go`
- Create: `internal/storage/notifications.go`
- Create: `internal/storage/notifications_test.go`

### Migration 7

```sql
ALTER TABLE webhook_endpoints
    ADD COLUMN provider TEXT NOT NULL DEFAULT 'WEBHOOK';

ALTER TABLE endpoint_versions
    ADD COLUMN provider_config_json TEXT NOT NULL DEFAULT '{}';

ALTER TABLE endpoint_secrets
    ADD COLUMN secret_kind TEXT NOT NULL DEFAULT 'WEBHOOK_HMAC';

ALTER TABLE delivery_attempts
    ADD COLUMN provider TEXT NOT NULL DEFAULT 'WEBHOOK';

ALTER TABLE delivery_attempts
    ADD COLUMN provider_code TEXT;

CREATE INDEX IF NOT EXISTS idx_notification_provider_status
    ON webhook_endpoints(provider, status);
```

Reason for not renaming tables now:

- production DB already depends on these FKs;
- `deliveries.endpoint_id` already references `webhook_endpoints`;
- renaming/rebuilding both `deliveries` and `delivery_attempts` introduces unnecessary rollback risk;
- table names are internal implementation debt and do not prevent a clean provider domain model.

### New generic storage DTO

```go
type NotificationChannel struct {
    ID               string          `json:"id"`
    Name             string          `json:"name"`
    Provider         string          `json:"provider"`
    Status           string          `json:"status"`
    Revision         int             `json:"revision"`
    URL              string          `json:"url,omitempty"`
    Config           json.RawMessage `json:"config"`
    SecretKind       string          `json:"-"`
    SecretConfigured bool            `json:"secretConfigured"`
    CreatedAt        string          `json:"createdAt"`
    UpdatedAt        string          `json:"updatedAt"`
}
```

### Secret storage rules

Existing webhook AAD remains:

```text
webhook-secret:<channelID>:<keyID>
```

Bark device key:

```text
notification-secret:bark:<channelID>:<keyID>
```

Bark secret row:

```text
secret_kind = BARK_DEVICE_KEY
encoding_version = aes-gcm-v1
```

Do not allow persistent Bark channel creation if `Store.keyring == nil`.

### Bark public config JSON

```json
{
  "group": "ACB",
  "level": "timeSensitive",
  "sound": "",
  "includeBalance": false,
  "includeDescription": true,
  "openDashboard": true,
  "deviceKeyFingerprint": "sha256:xxxxxxxxxxxx"
}
```

---

## Task 3: Refactor durable delivery into a provider-aware dispatcher

**Files:**
- Create: `internal/notification/provider.go`
- Create: `internal/notification/registry.go`
- Create: `internal/notification/dispatcher.go`
- Create: `internal/notification/dispatcher_test.go`
- Modify: `internal/webhook/dispatcher.go`
- Modify: `internal/webhook/webhook.go`
- Modify: `cmd/gateway/main.go`

Requirements:

- [ ] Registry keyed by exact provider kind.
- [ ] Preserve worker pool `4`, burst `50`, lease `30s`, wake channel, retry schedule.
- [ ] Use provider `Result.Retriable`.
- [ ] Retry only network/timeout/408/429/5xx.
- [ ] Terminal 4xx config/auth errors -> dead letter.
- [ ] Unknown provider -> `UNKNOWN_PROVIDER`.
- [ ] Webhook provider keeps HMAC, X-Bank headers, SSRF protection, redirect blocking.

Composition:

```go
registry := notification.NewRegistry(
    webhook.NewProvider(webhook.NewHTTPClient()),
    barkProvider,
)

dispatcher := notification.NewDispatcher(store, registry)
go dispatcher.Run(ctx)
```

---

## Task 4: Implement the Bark API V2 provider

**Files:**
- Create: `internal/notification/bark/client.go`
- Create: `internal/notification/bark/provider.go`
- Create: `internal/notification/bark/render.go`
- Create corresponding tests.

### Client constraints

```text
POST <BARK_SERVER_URL>/push
total timeout: 5s
response header timeout: 3s
TLS handshake timeout: 3s
redirects: disabled
max response body: 16 KiB
connection keepalive: enabled
```

Use Basic Auth when configured.

### Payload

```go
type PushRequest struct {
    DeviceKey string `json:"device_key"`
    Title     string `json:"title,omitempty"`
    Body      string `json:"body"`
    Level     string `json:"level,omitempty"`
    Group     string `json:"group,omitempty"`
    Sound     string `json:"sound,omitempty"`
    URL       string `json:"url,omitempty"`
    IsArchive string `json:"isArchive,omitempty"`
}
```

Success:

```text
HTTP 2xx AND response.code == 200
```

Classification:

```text
network/timeout -> retryable
408             -> retryable
429             -> retryable
5xx             -> retryable
400             -> terminal BARK_BAD_REQUEST
401/403/418     -> terminal BARK_AUTH_REJECTED
3xx             -> terminal BARK_REDIRECT_REJECTED
```

Renderer realtime:

```text
Title: 💰 ACB • +500.000 ₫
Body:
Tài khoản: ***1234
Nội dung: NGUYEN VAN A chuyen tien
Mã GD: 9988
```

Catch-up:

```text
Title: ⏱ ACB • +500.000 ₫
Phát hiện khi đồng bộ bù
```

Default:
- `group=ACB`
- `level=timeSensitive`
- `includeBalance=false`
- no archive by default
- trusted dashboard URL from `PUBLIC_ORIGIN`

---

## Task 5: Add Bark runtime configuration

Modify `internal/config/config.go`, tests, `.env.example`.

Add:

```text
BARK_SERVER_URL=http://bark:8080
BARK_PUBLIC_URL=https://push.example.com
BARK_BASIC_AUTH_USER_FILE=/run/secrets/bark_basic_user
BARK_BASIC_AUTH_PASSWORD_FILE=/run/secrets/bark_basic_password
```

Rules:
- server URL is trusted deployment config only;
- public URL must be HTTPS in production;
- reject partial credentials;
- never expose Basic Auth password via API.

---

## Task 6: Add provider-aware Notification Channel API while preserving `/webhooks`

Routes:

```text
GET  /api/v1/notification-channels
GET  /api/v1/notification-providers
POST /api/v1/notification-channels/bark
PUT  /api/v1/notification-channels/{id}/bark-device
POST /api/v1/notification-channels/{id}/test
POST /api/v1/notification-channels/{id}/{action:enable|disable}
```

Permissions:
- list/providers: all authenticated roles
- create/rotate/enable/disable: OWNER
- test: OWNER|OPERATOR

Create Bark:

```json
{
  "name": "iPhone Tuấn",
  "deviceKey": "...",
  "config": {
    "group": "ACB",
    "level": "timeSensitive",
    "includeBalance": false,
    "includeDescription": true,
    "openDashboard": true
  }
}
```

Response never returns device key.

Test message:

```text
✅ Bark đã kết nối
ACB Transaction Webhook có thể gửi thông báo tới thiết bị này.
```

Test works even while channel is DISABLED.

---

## Task 7: Fan out through the existing atomic outbox

Modify `internal/storage/transactions.go`.

Active channels query:

```sql
SELECT id, current_revision
FROM webhook_endpoints
WHERE status='ACTIVE'
  AND provider IN ('WEBHOOK','BARK')
```

Policy:

```text
REALTIME   credit -> WEBHOOK/BARK delivery
CATCH_UP   credit -> WEBHOOK/BARK delivery
FILTER_SYNC       -> none
BOOTSTRAP         -> none
baseline          -> none
```

Example:

```text
1 transaction
2 webhook channels
2 Bark iPhones
=> 1 event
=> 4 independent durable deliveries
```

---

## Task 8: Redesign Notification Channels UI

Modify existing `NotificationChannelsPage.tsx`; do not create a new page.

Provider cards:

```text
Webhook
Gửi JSON có chữ ký HMAC tới hệ thống khác.

Bark • iPhone
Đẩy thông báo trực tiếp tới iPhone qua Bark self-host.
```

Bark fields:
- Tên thiết bị/kênh
- Bark Device Key
- Nhóm
- Mức thông báo
- Âm thanh
- Hiện số dư
- Hiện nội dung giao dịch
- Mở dashboard khi chạm

Helper:

```text
1. Cài Bark trên iPhone.
2. Thêm server self-host từ BARK_PUBLIC_URL.
3. Copy Device Key.
4. Dán vào dashboard.
5. Tạo -> Gửi thử -> Enable.
```

Never show device key again after submit.

---

## Task 9: Add Bark service to production Compose

Add `bark` to `deploy/compose.prod.yaml`.

Concept:

```yaml
bark:
  image: ${BARK_IMAGE_REF:?immutable digest required}
  container_name: acb-bark
  restart: unless-stopped
  read_only: true
  cap_drop: [ALL]
  security_opt: ["no-new-privileges:true"]
  environment:
    TZ: Asia/Ho_Chi_Minh
    BARK_SERVER_ADDRESS: 0.0.0.0:8080
    BARK_SERVER_DATA_DIR: /data
    BARK_SERVER_REDUCE_MEMORY_USAGE: "true"
  expose:
    - "8080"
  networks:
    default:
      aliases: [bark]
    edge:
      aliases: [acb-bark]
  volumes:
    - bark_data:/data:rw
```

Use mounted secret files and a tiny shell entrypoint wrapper to export:

```text
BARK_SERVER_BASIC_AUTH_USER
BARK_SERVER_BASIC_AUTH_PASSWORD
```

Gateway also mounts those same secrets read-only.

Do not publish host port `8080`.

---

## Task 10: Configure Cloudflare Tunnel onboarding

Dedicated hostname:

```text
push.example.com -> http://acb-bark:8080
```

Rules:

1. no host port;
2. HTTPS via Cloudflare;
3. no interactive Cloudflare Access on Bark hostname;
4. keep Bark Basic Auth enabled;
5. `/register`, `/ping`, `/healthz` remain auth-free upstream;
6. `/push` Basic-Auth protected;
7. gateway uses internal `http://bark:8080`, not public hostname.

---

## Task 11: Extend deploy/verify/rollback

Modify:
- `deploy/deploy.sh`
- `deploy/verify-deployment.sh`
- `deploy/rollback.sh`
- `.github/workflows/deploy.yml`

Add immutable Bark image tracking:

```text
.deployed-bark-image
.previous-bark-image
```

Deploy verification checks:
- Bark container healthy
- actual image equals pinned digest
- gateway healthy
- auth-browser healthy

Do not auto-send phone push during deploy verification.

---

## Task 12: Provider-specific observability

Add aggregate provider metrics only:

```text
delivery_success_total{provider}
delivery_failure_total{provider}
delivery_latency_ms{provider}
pending by provider
dead-letter by provider
```

Never use transaction/account/device fingerprint as metric labels.

Delivery API adds:

```json
{
  "channelId": "chn_...",
  "channelName": "iPhone Tuấn",
  "provider": "BARK"
}
```

Keep `endpointId` temporarily for compatibility.

---

## Task 13: Failure/security test matrix

Required backend tests:

- [ ] Bark 200/code200 -> delivered.
- [ ] network timeout -> retry.
- [ ] 429 -> retry.
- [ ] 500 -> retry.
- [ ] 400 invalid device -> dead-letter.
- [ ] 418 auth reject -> immediate dead-letter.
- [ ] redirect -> reject.
- [ ] oversized response body bounded.
- [ ] gateway/Bark restart doesn't lose pending deliveries.
- [ ] duplicate poll doesn't duplicate Bark delivery.
- [ ] two iPhones have independent retry state.
- [ ] device key never appears in API/log/audit/delivery error.
- [ ] webhook HMAC behavior unchanged.
- [ ] webhook SSRF protection unchanged.

Full verification:

```bash
go test -race ./...
go vet ./...

cd web
bun install --frozen-lockfile
bunx --bun tsc --noEmit
bunx --bun vitest run
bunx --bun vite build

cd ..
bash -n deploy/*.sh
docker compose --env-file deploy/.env.production -f deploy/compose.prod.yaml config --quiet
git diff --check
```

---

## Task 14: Production rollout

### Stage A — infrastructure

```text
1. Backup gateway SQLite.
2. Generate Bark Basic Auth secrets once.
3. Resolve and pin Bark image digest.
4. Deploy Bark.
5. Verify internal /healthz.
6. Configure Cloudflare Tunnel hostname.
7. Add server in Bark iOS and obtain Device Key.
```

### Stage B — gateway

```text
1. Run migration-only.
2. Run integrity check.
3. Deploy provider-capable gateway.
4. Verify existing webhook delivery.
5. Verify BARK provider reports available.
```

### Stage C — first iPhone

```text
1. Create Bark channel -> DISABLED.
2. Send Test.
3. Confirm iPhone receives it.
4. Enable.
5. Observe one real credit.
6. Verify exactly one delivery + one phone notification.
```

### Stage D — failure drill

```text
1. Stop Bark.
2. Inject test event in integration environment.
3. Confirm retry/pending.
4. Start Bark.
5. Confirm eventual DELIVERED.
```

---

# Definition of Done

- [ ] Bark is `provider=BARK`, not a fake webhook.
- [ ] Existing webhook remains backward-compatible.
- [ ] One canonical bank event fans out to independent durable deliveries.
- [ ] Bark uses existing lease/retry/dead-letter path.
- [ ] Gateway -> Bark stays on private Docker DNS.
- [ ] No VPS Bark port is published.
- [ ] iPhone registers through Cloudflare Tunnel.
- [ ] `/push` is Basic-Auth protected.
- [ ] Bark Device Key is encrypted at rest and never returned after creation.
- [ ] Default level is `timeSensitive`.
- [ ] Catch-up alerts are visibly marked.
- [ ] Filter-sync/bootstrap never phone-alert.
- [ ] RecordAttempt schema mismatch is fixed and regression-tested.
- [ ] Retry classification avoids retry storms.
- [ ] Restart doesn't lose notifications.
- [ ] Multiple iPhones are isolated deliveries.
- [ ] Bark image is immutable/pinned in production.
- [ ] Go race tests, frontend tests/build, shell validation, compose validation and diff check all pass.

---

# Deferred follow-up: Bark end-to-end encrypted payload

Bark supports `ciphertext` notifications and the iOS app can decrypt using locally configured crypto settings. Do **not** mix that into the first provider rollout because it creates a second device-key lifecycle.

After the provider path is stable, create a separate `BARK_E2E` plan for:

```text
per-device Bark crypto key
key rotation
AES mode/IV compatibility
ciphertext-only APNs payload
lost-key recovery UX
```

Until then:
- self-host Bark;
- Cloudflare HTTPS on public onboarding path;
- private Docker transport gateway -> Bark;
- `includeBalance=false` default;
- no Bark archive by default;
- no secret logging.

---

# Recommended execution order

```text
Task 1  schema blocker
  -> Task 2 storage provider metadata
  -> Task 3 generic dispatcher + webhook parity
  -> Task 4 Bark provider
  -> Task 5 runtime config
  -> Task 6 API
  -> Task 7 atomic fan-out
  -> Task 8 UI
  -> Task 9 Bark Compose
  -> Task 10 Cloudflare onboarding
  -> Task 11 CI/deploy/rollback
  -> Task 12 observability
  -> Task 13 full failure/security tests
  -> Task 14 rollout docs
```

Do not enable a live Bark channel before Tasks 1–7 and the Bark `Send Test` path pass.
