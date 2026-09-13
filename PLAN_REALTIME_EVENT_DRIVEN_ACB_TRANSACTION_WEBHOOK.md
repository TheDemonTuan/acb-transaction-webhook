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

# PLAN — Refactor ACB Transaction Webhook thành kiến trúc Realtime Event-Driven

> Repo: `TheDemonTuan/acb-transaction-webhook`  
> Mục tiêu ưu tiên: **bảo mật, nhanh, an toàn, mượt, ổn định và realtime nhất có thể**.  
> Nguyên tắc bắt buộc: **sau khi backend nhận được dữ liệu mới từ ACB, hệ thống nội bộ không được tạo thêm độ trễ có chủ ý**. Độ trễ đáng kể duy nhất được chấp nhận phải nằm ở biên:
>
> `Backend -> ACB -> Backend`
>
> Không còn polling UI 5 giây, không còn dispatcher ticker 2 giây, không còn refresh định kỳ để “chờ” dữ liệu mới xuất hiện.

---

## 1. Mục tiêu kiến trúc

Kiến trúc mới phải đạt các mục tiêu sau:

1. Chỉ có **một thành phần duy nhất** được phép gọi ACB.
2. Không để số lượng tab/browser/client làm tăng số request tới ACB.
3. Không có hai poll ACB chạy đồng thời.
4. Không dùng timer để truyền dữ liệu từ backend ra UI.
5. Không dùng timer để kích hoạt webhook mới.
6. Transaction mới phải được:
   - parse;
   - deduplicate;
   - persist;
   - phát event;
   - đẩy ra dashboard;
   - đánh thức webhook dispatcher;

   ngay trong cùng một pipeline.
7. Webhook retry vẫn được scheduling theo thời gian, nhưng **delivery mới không được chờ scheduler**.
8. Tự backoff khi ACB có dấu hiệu rate-limit, maintenance, timeout hoặc bất thường.
9. Session ACB không được expose ra frontend.
10. Dashboard có thể mở nhiều client nhưng upstream ACB vẫn chỉ chịu tải từ một monitor duy nhất.
11. Sau khi ACB trả response chứa giao dịch mới, mục tiêu latency nội bộ:
   - `ingest -> event`: p95 < 20 ms
   - `event -> SSE browser`: p95 < 50 ms trong cùng DC/VPS
   - `event -> webhook attempt`: p95 < 100 ms
12. Không hy sinh tính nhất quán hoặc bảo mật chỉ để giảm vài mili-giây.

---

# 2. Trạng thái hiện tại cần thay đổi

Repo hiện có ba nguồn delay độc lập.

## 2.1 Frontend polling

React hiện refresh dữ liệu bằng `setInterval(..., 5000)`.

Tác động:

- UI trễ tối đa gần 5 giây sau khi backend đã có transaction.
- mỗi tab browser tạo request riêng;
- nhiều user mở dashboard làm tăng tải API + SQLite;
- `/status`, `/connection`, `/webhooks`, `/csrf`, `/transactions`... bị gọi lặp lại dù không có gì thay đổi;
- trải nghiệm chỉ là “near realtime bằng polling”, không phải realtime push.

### Quyết định

**Xóa polling định kỳ của dashboard.**

Thay bằng:

```text
REST initial snapshot
        +
SSE incremental events
```

---

## 2.2 Backend polling ACB

Backend vẫn phải chủ động lấy dữ liệu ACB nếu ACB Web không có push API usable cho tài khoản hiện tại.

Đây là phần polling duy nhất được phép tồn tại.

```text
ACB Poller
    |
    +--> ACB
```

Không thành phần nào khác được tự gọi ACB.

---

## 2.3 Webhook dispatcher ticker 2 giây

Hiện dispatcher dùng ticker để tìm delivery pending.

Điều đó có nghĩa:

```text
transaction đã ingest
        |
        v
delivery pending
        |
     chờ 0-2s
        |
        v
dispatcher phát hiện
```

### Quyết định

Delivery mới phải **wake dispatcher ngay lập tức**.

Ticker chỉ còn dùng để:

- retry delivery đến hạn;
- recovery sau process restart;
- fallback safety scan.

---

# 3. Kiến trúc đích

```text
                         ACB WEB
                            ^
                            |
                controlled HTTP requests
                            |
                            v
                 +---------------------+
                 |   ACB Poll Engine   |
                 |---------------------|
                 | single-flight       |
                 | rate limiter        |
                 | adaptive backoff    |
                 | jitter              |
                 | session guard       |
                 +----------+----------+
                            |
                       response
                            |
                            v
                 +---------------------+
                 | Transaction Parser  |
                 +----------+----------+
                            |
                            v
                 +---------------------+
                 |  Transaction Store  |
                 |  + deduplication    |
                 |  + atomic outbox    |
                 +----------+----------+
                            |
                            v
                      +-----------+
                      | Event Hub |
                      +-----+-----+
                            |
          +-----------------+------------------+
          |                 |                  |
          v                 v                  v
    SSE Dashboard     Webhook Wake       Metrics/Logs
          |                 |
          v                 v
      React UI        Dispatcher
                            |
                            v
                       Webhook target
```

Nguyên tắc cốt lõi:

```text
ACB response -> parse -> commit -> publish -> push
```

Không có:

```text
sleep
ticker
poll DB every N seconds
frontend refresh interval
```

ở đường đi transaction mới.

---

# 4. Event-driven boundary

Phải coi thời điểm backend nhận được response ACB là mốc `T0`.

Ví dụ:

```text
T0      HTTP body từ ACB đã đọc xong
T0+2ms  parse xong
T0+5ms  transaction commit vào SQLite
T0+6ms  event publish
T0+8ms  dispatcher wake
T0+12ms SSE gửi browser
T0+30ms webhook HTTP request bắt đầu
```

Không yêu cầu đúng các con số trên trong mọi máy, nhưng architecture phải không tự thêm:

```text
+ 2 giây
+ 5 giây
+ 10 giây
```

chỉ vì ticker.

---

# 5. ACB Poll Engine

Đây là thành phần quan trọng nhất.

## 5.1 Single owner

Chỉ một goroutine/worker được sở hữu quyền poll ACB.

Không để:

```text
HTTP handler
manual sync
dashboard
background job
auth worker
```

tự gọi transaction history trực tiếp.

Tất cả phải request qua `PollEngine`.

---

## 5.2 Single-flight

Tại mọi thời điểm:

```text
max_inflight_acb_poll = 1
```

Nếu một poll đang chạy:

```text
manual sync
scheduled poll
recovery poll
```

phải coalesce thành một yêu cầu tiếp theo, không tạo request song song.

Pseudo:

```go
type PollEngine struct {
    pollMu sync.Mutex
    wakeCh chan PollReason
}
```

Một vòng:

```go
func (p *PollEngine) executePoll(ctx context.Context) {
    p.pollMu.Lock()
    defer p.pollMu.Unlock()

    // only one upstream ACB fetch at a time
}
```

---

# 6. Không dùng fixed polling ngây thơ

Không nên hard-code:

```text
poll every 5 seconds forever
```

Nên dùng `Adaptive Poll Scheduler`.

## 6.1 Normal mode

Ví dụ production ban đầu:

```env
ACB_POLL_BASE_INTERVAL=5s
ACB_POLL_JITTER=500ms
```

Actual:

```text
4.5s - 5.5s
```

Jitter giúp tránh pattern quá máy móc.

> Khoảng 5 giây chỉ là giá trị triển khai thử nghiệm. Rate-limit thực tế của ACB Web không nên được giả định khi chưa đo.

---

## 6.2 Không overlap

Nếu ACB request mất 4 giây và interval là 5 giây:

Không được:

```text
0s request A
5s request B
```

theo clock cứng.

Phải:

```text
request A
    |
finish
    |
wait next interval
    |
request B
```

Tức:

```text
nextPoll = previousPollFinishedAt + interval
```

---

# 7. Adaptive backoff

Khi bình thường:

```text
5s
```

Khi nhận dấu hiệu upstream không ổn:

```text
10s
20s
40s
60s
```

hoặc profile khác theo config.

Trigger backoff:

- HTTP 429
- HTTP 403 đáng ngờ sau hoạt động liên tục
- 5xx
- timeout
- connection reset
- maintenance page
- CAPTCHA
- OTP challenge
- session anomaly
- response classifier không hợp lệ
- nhiều parse failure liên tiếp

Không retry aggressive.

---

# 8. Rate-limit protection

Tạo `ACBRateGuard`.

State:

```go
type RateGuard struct {
    LastRequestAt time.Time
    BackoffUntil  time.Time
    ConsecutiveFailures int
    RateLimited bool
}
```

Trước mọi request:

```go
if now < BackoffUntil {
    skip
}
```

Không endpoint HTTP nào được bypass guard.

Ngay cả:

```text
POST /connection/sync
```

cũng chỉ được:

```text
wake poll engine
```

chứ không trực tiếp gọi ACB.

---

# 9. Manual Sync phải coalesce

User nhấn sync 10 lần:

Không được tạo 10 poll.

Phải:

```text
click x10
  |
  v
single pending sync flag
  |
  v
one ACB poll
```

Có thể dùng buffered channel size 1:

```go
syncCh := make(chan struct{}, 1)

select {
case syncCh <- struct{}{}:
default:
}
```

---

# 10. Realtime poll scope

Realtime monitor không nên query lịch sử dài một cách không cần thiết.

## 10.1 Normal realtime window

Ưu tiên:

```text
FromDate = today
ToDate   = today
```

---

## 10.2 Midnight overlap

Để tránh miss transaction qua mốc ngày:

Trong khoảng:

```text
00:00 - 00:10 Asia/Ho_Chi_Minh
```

có thể query:

```text
yesterday -> today
```

---

## 10.3 Backfill riêng

Tách hẳn:

```text
RealtimePoller
BackfillWorker
```

`BackfillWorker` dùng cho:

- startup reconciliation;
- manual history sync;
- lịch sử 7/30 ngày;
- recovery khi downtime.

Không để backfill làm chậm realtime poll.

---

# 11. Parse tối ưu

Parser cần:

- không regex quá nặng lặp nhiều lần;
- precompile regex;
- tránh copy body không cần thiết;
- parse transaction mới nhất trước nếu cấu trúc HTML cho phép;
- có giới hạn response body;
- reject HTML bất thường.

Không log raw HTML chứa dữ liệu ngân hàng.

---

# 12. Deduplication bắt buộc

Mỗi transaction phải có semantic identity ổn định.

Ví dụ:

```text
ACB:<transaction-number>
```

và canonical hash.

DB phải enforce uniqueness.

Không chỉ check trong memory.

```sql
CREATE UNIQUE INDEX ...
ON transactions(connection_id, semantic_key);
```

Mục tiêu:

```text
poll 100 lần
same transaction
=
1 transaction record
1 event
1 webhook logical event
```

---

# 13. Atomic transaction + outbox

Đây là phần quan trọng để vừa nhanh vừa an toàn.

Không nên:

```text
INSERT transaction
COMMIT

process crash

EmitEvent()
```

vì có thể mất event.

Phải dùng transaction DB:

```text
BEGIN

INSERT transaction

IF inserted:
    INSERT event
    INSERT webhook_delivery

COMMIT
```

Sau `COMMIT` mới publish event vào in-memory EventHub.

---

# 14. Transactional Outbox

DB là source of truth.

EventHub chỉ là low-latency notification layer.

Nếu process crash đúng lúc sau commit nhưng trước publish:

```text
outbox recovery
```

phải phát hiện event chưa dispatched.

Schema gợi ý:

```sql
CREATE TABLE outbox_events (
    id TEXT PRIMARY KEY,
    event_type TEXT NOT NULL,
    aggregate_id TEXT NOT NULL,
    payload BLOB NOT NULL,
    created_at TEXT NOT NULL,
    published_at TEXT NULL
);
```

Không bắt buộc phải dùng đúng table riêng nếu schema hiện có đã đóng vai trò tương tự, nhưng phải giữ invariant:

```text
transaction committed
=> event cannot be lost
```

---

# 15. EventHub

Tạo package riêng:

```text
internal/eventbus/
```

Ví dụ:

```go
type Event struct {
    ID        string
    Type      string
    Timestamp time.Time
    Data      any
}
```

Supported event:

```text
transaction.created
connection.changed
poll.started
poll.completed
poll.failed
webhook.delivery.created
webhook.delivery.succeeded
webhook.delivery.failed
auth.changed
system.health.changed
```

---

# 16. EventHub không được block poller

Sai:

```go
for each client {
    client <- event
}
```

nếu một client chậm có thể giữ cả poll pipeline.

Đúng:

- bounded subscriber buffers;
- non-blocking publish;
- disconnect slow consumer;
- hoặc per-client writer goroutine.

Ví dụ:

```text
Poller
  |
Publish()
  |
  +--> queue SSE client A
  +--> queue SSE client B
  +--> dispatcher wake
```

`Publish()` phải rất nhanh.

---

# 17. SSE thay frontend polling

API mới:

```http
GET /api/v1/events
Accept: text/event-stream
```

Response:

```http
Content-Type: text/event-stream
Cache-Control: no-cache
Connection: keep-alive
```

Ví dụ:

```text
id: evt_01
event: transaction.created
data: {...}

```

---

# 18. Tại sao SSE thay vì WebSocket

Use case dashboard chủ yếu là:

```text
server -> browser
```

Client command vẫn dùng REST:

```text
pause
resume
sync
configure
webhook setup
auth
```

SSE có lợi thế:

- đơn giản;
- native browser reconnect;
- hợp HTTP reverse proxy;
- dễ audit;
- dễ giữ RBAC cùng HTTP stack;
- không cần custom bidirectional protocol;
- giảm code surface;
- ít lỗi reconnect/state hơn WebSocket.

WebSocket chỉ nên dùng nếu sau này thực sự cần bidirectional high-frequency messaging.

Hiện tại: **SSE là lựa chọn ưu tiên.**

---

# 19. SSE authentication

SSE endpoint phải đi qua authentication giống REST.

Không tạo endpoint public kiểu:

```text
/events?token=...
```

Token trong query string có nguy cơ bị log.

Nếu dùng Cloudflare Access:

- tiếp tục enforce Access ở edge;
- backend vẫn verify identity;
- RBAC viewer/operator/owner;
- SSE không bypass middleware.

---

# 20. SSE initial snapshot

Để tránh race:

```text
client GET /transactions
event xảy ra
client connect SSE
```

có thể miss event trong khoảng giữa.

Có hai cách.

## Cách khuyên dùng

SSE endpoint có `Last-Event-ID`.

Flow:

```text
1. client connect SSE
2. server establishes subscription
3. client loads snapshot
4. incremental events continue
```

hoặc:

```text
GET /events?after=<event_cursor>
```

Dùng monotonic event cursor.

---

# 21. Reconnect

Browser mất mạng:

```text
EventSource reconnect
```

Client gửi:

```text
Last-Event-ID
```

Server replay một lượng event gần đây từ DB/outbox.

Không chỉ dựa vào RAM.

---

# 22. Slow consumer protection

Mỗi SSE connection:

```text
buffer 64-256 events
```

Nếu đầy:

```text
disconnect client
```

Client reconnect + replay.

Không bao giờ block transaction ingestion chỉ vì browser treo.

---

# 23. SSE heartbeat

Dùng heartbeat để reverse proxy không cắt idle connection.

Ví dụ:

```text
: ping

```

mỗi:

```text
15-30s
```

Heartbeat không gọi DB và không gọi ACB.

Nó chỉ giữ HTTP connection.

---

# 24. React architecture mới

Bỏ:

```ts
setInterval(load, 5000)
```

Thay:

```text
Initial REST snapshot
      +
useEventStream()
```

Tạo:

```text
web/src/services/api/
web/src/services/events/
web/src/hooks/useEventStream.ts
web/src/store/
```

---

# 25. Client state

Khi event:

```text
transaction.created
```

React:

```text
prepend transaction
update totals
update last transaction timestamp
show subtle notification
```

Không reload toàn page.

Không gọi `/transactions` lại chỉ vì có event.

---

# 26. Event-based cache patching

Ví dụ:

```ts
case "transaction.created":
    transactionStore.upsert(event.data)
    break

case "connection.changed":
    connectionStore.update(event.data)
    break

case "webhook.delivery.succeeded":
    deliveryStore.patch(event.data)
    break
```

---

# 27. Resync fallback

Nếu SSE reconnect mà replay cursor quá cũ:

server trả event:

```text
system.resync_required
```

Client khi đó mới:

```text
GET latest snapshot
```

Đây là exception/recovery, không phải polling.

---

# 28. Webhook dispatcher event-driven

Dispatcher hiện tại phải thay từ:

```text
ticker -> query pending
```

thành:

```text
new delivery -> wake dispatcher
```

Struct:

```go
type Dispatcher struct {
    wakeCh chan struct{}
}
```

---

# 29. Wake không block

Sau khi commit delivery:

```go
func (d *Dispatcher) Wake() {
    select {
    case d.wakeCh <- struct{}{}:
    default:
    }
}
```

Nếu đã có wake pending thì không cần thêm.

---

# 30. Dispatcher loop mới

Pseudo:

```go
for {
    nextRetry := store.NextRetryAt()

    select {
    case <-ctx.Done():
        return

    case <-d.wakeCh:
        d.drainReady(ctx)

    case <-timerUntil(nextRetry):
        d.drainReady(ctx)
    }
}
```

`drainReady()`:

```text
claim
dispatch
claim
dispatch
...
until none
```

---

# 31. Delivery mới không chờ retry scheduler

Điều kiện acceptance:

```text
transaction commit
     |
     +-> new delivery
     |
     +-> dispatcher.Wake()
```

Không được phải đợi tới `next retry tick`.

---

# 32. Webhook concurrency

Không nên unlimited goroutines.

Config:

```env
WEBHOOK_MAX_CONCURRENCY=8
```

Hoặc thấp hơn tùy VPS.

Mỗi endpoint có thể có concurrency guard riêng nếu cần bảo toàn ordering.

---

# 33. Retry vẫn dùng backoff

Ví dụ:

```text
2s
5s
15s
30s
1m
2m
5m
15m
```

Nhưng chỉ retry failure.

Delivery đầu tiên:

```text
immediate
```

---

# 34. Webhook timeout

Timeout nên ngắn vừa đủ.

Ví dụ:

```text
connect: 3-5s
overall: 10s
```

Không để một endpoint chết chiếm worker quá lâu.

---

# 35. SSRF protection

Repo đã có logic public dialer/security; phải giữ và tăng cường.

Webhook URL không được:

- localhost;
- RFC1918;
- link-local;
- metadata endpoint;
- Docker internal network;
- Unix socket bridge;
- DNS rebinding.

Resolve DNS ở thời điểm connect và validate IP sau resolve.

---

# 36. Webhook signatures

Giữ HMAC signature.

Payload cần có:

```text
event id
delivery id
timestamp
nonce
signature
```

Consumer verify:

- signature;
- timestamp tolerance;
- nonce replay;
- event idempotency.

---

# 37. Secret management

Không trả webhook secret sau thời điểm tạo ban đầu ngoài trường hợp UI explicitly reveals once.

Không log:

- HMAC secret;
- ACB cookies;
- session form state;
- Cloudflare token;
- auth token;
- master encryption key.

---

# 38. ACB session isolation

ACB cookies chỉ tồn tại:

```text
auth-browser
gateway encrypted storage
ACB HTTP client
```

Không bao giờ:

```text
React
SSE payload
logs
webhook
```

---

# 39. Encryption at rest

Session persistence:

- master key ngoài DB;
- file permission `0600`;
- Docker secret/read-only mount;
- encryption authenticated (AEAD);
- key rotation design.

Không commit key vào `.env` repository.

---

# 40. ACB host allowlist

Client ACB phải chỉ được request:

```text
https://online.acb.com.vn
```

Redirect ra host khác:

```text
reject
```

Giữ invariant hiện tại.

---

# 41. Request concurrency

ACB HTTP transport:

```text
MaxConnsPerHost = 1 hoặc rất thấp
MaxIdleConnsPerHost = 1-2
IdleConnTimeout hợp lý
```

Mục tiêu:

- keep-alive;
- reuse TCP/TLS;
- không parallel poll;
- giảm handshake latency;
- giảm pattern bất thường.

---

# 42. Keep-Alive

Nên reuse HTTP connection nếu ACB cho phép.

Không tạo `http.Client` mới mỗi poll.

Một long-lived client:

```text
gateway lifetime
```

---

# 43. Timeout phân lớp

Không chỉ một timeout tổng.

Gợi ý:

```text
DialTimeout            5s
TLSHandshakeTimeout    5s
ResponseHeaderTimeout  10s
OverallRequestTimeout  20-30s
```

Giá trị cuối cần tuning từ production telemetry.

---

# 44. Circuit breaker

Nếu ACB liên tục lỗi:

```text
5 failures
```

không tiếp tục hammer upstream.

State:

```text
CLOSED
OPEN
HALF_OPEN
```

OPEN:

```text
pause 30-60s
```

HALF_OPEN:

```text
1 probe
```

success:

```text
CLOSED
```

---

# 45. Không coi tất cả lỗi như nhau

Classifier:

```text
AUTH_REQUIRED
CAPTCHA_REQUIRED
OTP_REQUIRED
RATE_LIMITED
MAINTENANCE
TEMPORARY_NETWORK
INVALID_HTML
PARSE_FAILED
SESSION_INVALID
SUCCESS
```

Mỗi class có policy khác nhau.

---

# 46. Auth-related errors

Nếu:

```text
LoginPage
OTPChallenge
CaptchaPage
```

không retry 5 giây liên tục.

Phải transition connection state và thông báo UI realtime:

```text
connection.changed
```

---

# 47. Poll scheduling state machine

```text
MONITORING
    |
    +-- success --------> NORMAL
    |
    +-- transient ------> BACKOFF
    |
    +-- rate limit -----> RATE_LIMITED
    |
    +-- auth lost ------> AUTH_REQUIRED
    |
    +-- maintenance ----> DEGRADED
```

---

# 48. Poll reason

Record lý do poll:

```text
scheduled
manual
startup
recovery
post-auth
backfill
```

Giúp audit.

---

# 49. Poll metrics

Mỗi request ghi:

```text
started_at
finished_at
duration_ms
status
HTTP status
classifier
rows_seen
new_rows
reason
next_interval_ms
backoff_level
```

Không log sensitive request fields.

---

# 50. Realtime metrics

Các metrics bắt buộc:

```text
acb_poll_duration_ms
acb_poll_interval_ms
acb_poll_success_total
acb_poll_failure_total
acb_rate_limit_total

transactions_ingested_total
transactions_duplicate_total

event_publish_latency_ms
sse_connected_clients
sse_dropped_clients
sse_replay_total

webhook_first_attempt_latency_ms
webhook_success_total
webhook_retry_total
webhook_dead_letter_total
```

---

# 51. Latency telemetry

Transaction mới nên lưu:

```text
acb_response_received_at
parsed_at
persisted_at
event_published_at
webhook_started_at
```

Không phải tất cả nhất thiết persist vĩnh viễn; có thể metrics/log.

Mục tiêu đo được:

```text
internal_latency =
webhook_started_at
-
acb_response_received_at
```

---

# 52. SLO

## SLO A — internal transaction propagation

Sau khi backend đã đọc xong ACB response có giao dịch mới:

```text
p50 < 20 ms
p95 < 100 ms
p99 < 250 ms
```

từ ingestion đến bắt đầu webhook attempt.

Tuning tùy VPS.

---

## SLO B — Dashboard

Sau `event_published_at`:

```text
p95 SSE delivery < 100 ms
```

trong điều kiện network client bình thường.

---

## SLO C — No intentional delay

Không có code path:

```text
Sleep(...)
Ticker(...)
setInterval(...)
```

trong happy-path propagation:

```text
ACB response -> UI/webhook
```

---

# 53. SQLite tuning

SQLite phù hợp cho một gateway đơn instance.

Khuyên:

```sql
PRAGMA journal_mode=WAL;
PRAGMA synchronous=NORMAL;
PRAGMA busy_timeout=5000;
PRAGMA foreign_keys=ON;
```

Cân nhắc `synchronous=FULL` nếu ưu tiên durability tuyệt đối hơn latency.

Không blindly đổi nếu chưa benchmark.

---

# 54. Transaction ngắn

Không giữ DB transaction trong lúc:

- gọi ACB;
- gọi webhook;
- gửi SSE.

DB transaction chỉ bao quanh:

```text
dedup
insert transaction
insert event
insert delivery
commit
```

---

# 55. Writer contention

SQLite một writer tại một thời điểm.

Không để audit spam hoặc UI request gây lock dài.

Các query dashboard:

- index đầy đủ;
- pagination;
- không full-table scan.

---

# 56. API mới

Giữ REST:

```text
GET  /api/v1/status
GET  /api/v1/connection
GET  /api/v1/transactions
GET  /api/v1/webhooks
GET  /api/v1/deliveries
GET  /api/v1/audit

POST /api/v1/connection/sync
POST /api/v1/connection/pause
POST /api/v1/connection/resume
...
```

Thêm:

```text
GET /api/v1/events
```

---

# 57. Optional realtime health endpoint

Có thể thêm:

```text
GET /api/v1/realtime/status
```

Trả:

```json
{
  "connectedClients": 3,
  "lastEventAt": "...",
  "lastACBPollAt": "...",
  "nextACBPollAt": "...",
  "pollIntervalMs": 5000,
  "backoff": false
}
```

Không expose secret.

---

# 58. Cache headers

SSE:

```text
Cache-Control: no-cache, no-transform
```

API dynamic:

```text
Cache-Control: no-store
```

Static fingerprint assets:

```text
Cache-Control: public, max-age=31536000, immutable
```

---

# 59. Reverse proxy / Cloudflare

SSE phải test qua Cloudflare Tunnel thực tế.

Yêu cầu:

- streaming không bị buffer;
- response flush ngay;
- timeout/reconnect hoạt động;
- Access auth không bị bypass;
- heartbeat giữ connection alive.

---

# 60. CSRF

GET SSE không mutate state.

Các POST/PUT/PATCH/DELETE vẫn dùng CSRF protection nếu auth dựa cookie/browser context.

Không remove CSRF chỉ vì đã có Cloudflare Access.

---

# 61. CORS

Nếu UI và API cùng origin:

```text
không cần permissive CORS
```

Không dùng:

```text
Access-Control-Allow-Origin: *
```

cho admin API.

---

# 62. Security headers

Giữ/tăng:

```text
Content-Security-Policy
X-Content-Type-Options
Referrer-Policy
Permissions-Policy
frame-ancestors
```

No-VNC/auth browser path có thể cần CSP riêng.

Không relax toàn application để sửa iframe.

---

# 63. Audit

Audit event:

```text
auth.start
auth.cancel
connection.sync
connection.pause
connection.resume
webhook.create
webhook.disable
webhook.enable
```

Không audit mỗi SSE heartbeat.

---

# 64. Logging

Structured JSON logs.

Fields:

```text
request_id
component
event
connection_generation
poll_id
duration_ms
result
```

Redact:

```text
cookie
authorization
form processor state
secret
account full number
transaction full sensitive raw body
```

---

# 65. Graceful shutdown

Khi SIGTERM:

1. stop accepting new HTTP mutations;
2. stop scheduling new ACB poll;
3. cancel current poll safely;
4. stop SSE accept;
5. drain webhook workers trong bounded timeout;
6. close DB;
7. exit.

Không corrupt DB.

---

# 66. Restart recovery

Sau restart:

```text
open DB
recover pending deliveries
restore encrypted ACB session
start EventHub
start Dispatcher
start HTTP/SSE
start PollEngine
```

Không cần browser login lại nếu session còn hợp lệ.

---

# 67. Startup thundering herd

Sau container restart không gọi ACB 3-4 lần từ các subsystem.

Chỉ:

```text
PollEngine initial sync
```

Một request path duy nhất.

---

# 68. Proposed package structure

```text
internal/
├── acb/
│   ├── client.go
│   ├── parser.go
│   └── classify.go
│
├── monitor/
│   ├── engine.go
│   ├── scheduler.go
│   ├── rate_guard.go
│   ├── circuit_breaker.go
│   ├── backfill.go
│   └── session.go
│
├── eventbus/
│   ├── hub.go
│   ├── event.go
│   └── replay.go
│
├── realtime/
│   └── sse.go
│
├── webhook/
│   ├── dispatcher.go
│   ├── worker.go
│   ├── signer.go
│   └── retry.go
│
├── storage/
│   ├── transactions.go
│   ├── events.go
│   ├── deliveries.go
│   ├── outbox.go
│   └── migrations/
│
└── httpapi/
    ├── server.go
    ├── events.go
    └── ...
```

---

# 69. Dependency direction

```text
httpapi
   |
   v
application services

monitor --> acb
monitor --> storage
monitor --> eventbus

webhook --> storage
webhook --> eventbus(optional status events)

realtime --> eventbus
realtime --> storage(replay)
```

Không để:

```text
storage -> httpapi
storage -> React
acb -> webhook
```

---

# 70. Không đưa Redis vào nếu chưa cần

Với một gateway instance:

- SQLite + in-memory EventHub là đủ;
- Redis làm tăng dependency;
- thêm network hop;
- thêm auth/secrets;
- tăng failure modes.

Chỉ cân nhắc Redis/NATS nếu sau này chạy:

```text
multiple gateway replicas
multiple consumers
distributed event bus
```

Hiện tại mục tiêu tối ưu đơn VPS: **không cần Redis**.

---

# 71. Event ordering

SSE event cần monotonic cursor.

Có thể dùng:

```text
ULID
```

hoặc DB sequence.

Không dựa hoàn toàn vào timestamp mili-giây.

---

# 72. Idempotency

Webhook consumer có thể nhận lại event do retry.

Event phải có:

```text
eventId
transactionId
semanticKey
```

Consumer phải xử lý idempotent.

---

# 73. Không phát event trước commit

Sai:

```text
publish event
commit DB
```

Nếu commit fail UI đã thấy transaction không tồn tại.

Đúng:

```text
commit DB
publish event
```

Durability gap được xử lý bằng outbox replay.

---

# 74. Poll batching

Một ACB response có 5 transaction mới:

Không publish một cách khiến DB bị reopen mỗi row.

Flow:

```text
parse all
one DB transaction
insert all missing
insert events
commit
publish N events
wake dispatcher once
```

Dispatcher tự drain queue.

---

# 75. Event coalescing cho status

Không cần push `poll.completed` quá nhiều cho public transaction page nếu không dùng.

Subscription có thể theo scope:

```text
admin
transactions
```

Admin nhận diagnostic events.

Viewer page chỉ nhận:

```text
transaction.created
connection.public_status
```

---

# 76. Public/view-only transaction page

Nếu repo redesign thành admin + transaction viewer:

Transaction viewer phải:

- read-only;
- không expose audit nội bộ;
- không expose poll internals;
- không expose session state technical codes;
- chỉ subscribe transaction-safe events.

---

# 77. Realtime UX

UI nên hiển thị:

```text
Đang kết nối trực tiếp
Đã kết nối
Đang kết nối lại
Mất kết nối tạm thời
```

Không hiển thị technical key:

```text
SSE_DISCONNECTED
AUTH_REQUIRED
SESSION_EXPIRED
```

cho end-user.

---

# 78. Không giả realtime khi SSE chết

Nếu SSE disconnect:

UI phải nói rõ:

```text
Đang kết nối lại...
```

Không silently hiện dữ liệu cũ như thể live.

Hiển thị:

```text
Cập nhật lần cuối: 23:41:08
```

---

# 79. Background tab behavior

Browser có thể throttle JS timer.

Đây cũng là lý do bỏ frontend polling.

SSE/network events ổn định hơn timer-based refresh cho use case live dashboard.

---

# 80. Visibility recovery

Khi tab trở lại `visible` sau thời gian dài:

- nếu SSE vẫn healthy: không cần reload;
- nếu connection vừa reconnect: replay cursor;
- nếu replay gap quá lớn: resync snapshot một lần.

---

# 81. Poll interval config cleanup

Dọn các biến trùng/không dùng:

```text
DEFAULT_POLL_INTERVAL_SEC
FAST_POLL_INTERVAL_SEC
POLL_MIN_INTERVAL_SEC
POLL_MAX_INTERVAL_SEC
```

Chuyển thành schema rõ nghĩa.

Đề xuất:

```env
ACB_POLL_BASE_INTERVAL_MS=5000
ACB_POLL_JITTER_MS=500

ACB_POLL_BACKOFF_INITIAL_MS=10000
ACB_POLL_BACKOFF_MAX_MS=60000

ACB_REQUEST_TIMEOUT_MS=30000

ACB_CIRCUIT_FAILURE_THRESHOLD=5
ACB_CIRCUIT_OPEN_MS=60000
```

---

# 82. Guardrail config

Không cho config vô lý như:

```text
ACB_POLL_BASE_INTERVAL_MS=100
```

Production validation:

```text
minimum 2000-5000 ms
```

Giá trị chính xác quyết định sau telemetry ACB.

---

# 83. Dynamic adaptive mode

Có thể bổ sung:

```text
busy mode
normal mode
backoff mode
```

Nhưng không nên đoán activity quá phức tạp ở version đầu.

Version đầu tiên:

```text
stable base interval
+
jitter
+
failure backoff
```

dễ kiểm chứng và an toàn hơn.

---

# 84. Nếu muốn realtime hơn nữa

Giới hạn cuối cùng của web polling:

```text
poll interval
```

Ví dụ interval 5 giây:

```text
transaction xuất hiện ngay sau poll
=> gần 5 giây mới thấy
```

Không SSE/WebSocket nào giải quyết được phần đó.

Muốn thấp hơn:

```text
3s
2s
```

thì tăng tần suất ACB request và tăng nguy cơ rate-limit/session detection.

Do đó phải benchmark thay vì giảm mù quáng.

---

# 85. Auto tuning tùy health

Có thể triển khai sau:

```text
success 100 polls
no warning
latency stable
=> giữ interval

429/403/transient rise
=> increase interval
```

Không tự giảm interval dưới production floor.

---

# 86. ACB response timing

Metric quan trọng:

```text
acb_http_ttfb_ms
acb_http_total_ms
```

Để phân biệt:

```text
delay do poll interval
delay do ACB response
delay nội bộ
```

---

# 87. End-to-end latency decomposition

```text
Transfer xảy ra
       |
       v
ACB core cập nhật
       |
       v
ACB web history visible
       |
       |  UNKNOWN / external
       v
Gateway poll begins
       |
       |  controlled interval
       v
ACB response received
       |
       |  INTERNAL TARGET <100ms
       v
Webhook starts + SSE pushed
```

Hệ thống chỉ kiểm soát từ `Gateway poll` trở xuống.

---

# 88. Không quảng cáo “true realtime” sai nghĩa

Nếu vẫn dựa web polling thì nên gọi:

```text
near-realtime
```

Bên trong hệ thống là realtime event-driven.

External freshness vẫn phụ thuộc:

- ACB publication delay;
- poll interval;
- network;
- ACB response time.

---

# 89. Test strategy

## Unit tests

### Poll Engine

- no overlapping polls;
- sync coalescing;
- jitter bounds;
- backoff;
- circuit breaker;
- auth transition;
- maintenance behavior.

### EventHub

- publish;
- multiple subscribers;
- slow subscriber;
- disconnect;
- replay cursor.

### Dispatcher

- immediate wake;
- no delay on first attempt;
- retries;
- retry ordering;
- dead letter.

---

# 90. Integration tests

Test:

```text
fake ACB returns transaction
```

Measure:

```text
fake response finished
    ->
DB committed
    ->
SSE received
```

Fail CI nếu vượt generous test bound.

---

# 91. Realtime webhook test

Fake receiver timestamp:

```text
T0 = ACB mock response complete
T1 = webhook request received
```

Assert:

```text
T1 - T0 < 250ms
```

trên local integration environment.

Không assert vài ms để tránh flaky CI.

---

# 92. Race tests

Go:

```bash
go test -race ./...
```

Đặc biệt:

- EventHub subscribe/unsubscribe;
- PollEngine wake;
- Dispatcher wake;
- shutdown.

---

# 93. Load test SSE

Mở:

```text
50
100
500
```

SSE connections tùy expected usage.

Verify:

- memory stable;
- publish latency stable;
- slow client không block;
- reconnect ổn.

---

# 94. Failure injection

Test:

- SQLite busy;
- ACB timeout;
- 429;
- 403;
- connection reset;
- invalid HTML;
- session expired;
- webhook 500;
- webhook timeout;
- app restart giữa transaction commit và publish;
- SSE disconnect giữa event.

---

# 95. Security tests

- SSRF;
- redirect SSRF;
- DNS rebinding;
- forged webhook signature;
- replay nonce;
- invalid Cloudflare JWT;
- wrong audience;
- expired JWT;
- owner-only routes;
- SSE RBAC;
- sensitive logs scan.

---

# 96. Deployment topology

```text
Internet
   |
Cloudflare Access
   |
Cloudflare Tunnel
   |
Docker network
   |
+----------------------+
| gateway              |
| Go + React static    |
| SSE                  |
| ACB poll engine      |
| dispatcher           |
+----------+-----------+
           |
       SQLite volume

+----------------------+
| auth-browser         |
| isolated             |
+----------------------+
```

Không expose SQLite/auth-browser trực tiếp Internet.

---

# 97. Docker hardening

Gateway:

```text
read_only: true
cap_drop:
  - ALL
security_opt:
  - no-new-privileges:true
```

Chỉ mount writable:

```text
/data
/run cần thiết
```

Auth browser cần sandbox/seccomp riêng phù hợp Chromium.

---

# 98. Container health

`/healthz`:

Process alive.

`/readyz`:

- DB ready;
- core initialization ready.

Không fail readiness chỉ vì ACB temporary unavailable, nếu không Kubernetes/Docker sẽ restart loop và làm tình trạng tệ hơn.

Expose ACB status riêng.

---

# 99. Realtime health

Dashboard admin:

```text
Realtime stream: Connected
ACB monitor: Active
Last poll: 2s ago
Last successful ACB response: 2s ago
Current poll interval: 5.2s
Webhook queue: 0
```

Text thân thiện, không raw code.

---

# 100. Migration plan

## Phase 1 — Event infrastructure

Tạo:

```text
internal/eventbus
internal/realtime
```

Thêm SSE endpoint.

Chưa xóa polling UI ngay.

---

## Phase 2 — Publish transaction events

Sau successful commit:

```text
EventHub.Publish(transaction.created)
```

Test end-to-end.

---

## Phase 3 — React SSE

Tạo `useEventStream`.

Update transaction store incremental.

Sau khi ổn:

```text
remove 5s setInterval
```

---

## Phase 4 — Dispatcher wake

Thêm `wakeCh`.

Delivery mới:

```text
Wake()
```

Ticker 2 giây bỏ khỏi primary path.

Retry chuyển sang deadline timer.

---

## Phase 5 — Poll engine refactor

Tách:

```text
scheduler
rate guard
circuit breaker
poll executor
```

Đảm bảo one-inflight.

---

## Phase 6 — Realtime window optimization

Realtime chỉ lấy data cần thiết.

Backfill thành worker riêng.

---

## Phase 7 — Atomic outbox/recovery

Đảm bảo crash không mất event.

---

## Phase 8 — Remove legacy config

Dọn poll config cũ.

---

## Phase 9 — Hardening + benchmarks

Benchmark:

- ACB request rate;
- DB write latency;
- event propagation;
- webhook first-attempt latency;
- SSE fanout.

---

# 101. Thay đổi cụ thể trong `web/src/App.tsx`

Loại bỏ:

```ts
useEffect(() => {
  void load();
  const timer = window.setInterval(() => void load(), 5_000);
  return () => window.clearInterval(timer);
}, [active]);
```

Thay bằng:

```text
on mount:
    load initial snapshot
    connect EventSource

on transaction.created:
    upsert transaction

on connection.changed:
    update connection state

on delivery changed:
    patch delivery

on reconnect gap:
    one-time resync
```

---

# 102. Thay đổi cụ thể `internal/webhook/dispatcher.go`

Bỏ primary:

```go
ticker := time.NewTicker(2 * time.Second)
```

Thay bằng:

```text
wake channel
+
next retry timer
```

`ticker` nếu giữ chỉ là fallback reconciliation rất thưa, ví dụ hàng chục giây/phút, không nằm trong happy path.

---

# 103. Thay đổi `internal/monitor/monitor.go`

Tách trách nhiệm hiện tại thành:

```text
PollExecutor
PollScheduler
RateGuard
SessionPolicy
```

`PollOnce()` không tự quyết định lịch tiếp theo.

Scheduler là owner duy nhất của cadence.

---

# 104. Refactor manual sync

`RequestSync()`:

- không reset limiter;
- không bypass backoff;
- coalesce;
- nếu poll đang running thì mark `pending`;
- sau poll hiện tại chỉ run thêm nếu policy cho phép.

---

# 105. API ACB call budget

Thêm metric/counter:

```text
requests/minute
requests/hour
```

Dashboard admin hiển thị.

Có daily graph để phát hiện config quá aggressive.

---

# 106. Poll rate safety

Hard guard:

```text
if lastACBRequest + minimumInterval > now:
    wait until allowed
```

Manual sync cũng tuân theo.

---

# 107. Không để frontend quyết định poll rate

Không API kiểu:

```text
POST /poll?interval=1000
```

Frontend chỉ:

```text
sync now
```

Backend policy quyết định khi nào request upstream được phép chạy.

---

# 108. Security boundary

```text
Browser
  |
  | Cloudflare Access
  v
Gateway
  |
  | private encrypted session
  v
ACB
```

Không forward ACB credentials ra ngoài.

---

# 109. Realtime transaction page

Transaction viewer chỉ tải initial:

```text
GET /transactions?limit=50
```

Sau đó live bằng SSE.

Scroll pagination vẫn dùng REST cursor:

```text
GET /transactions?cursor=...
```

Pagination historical không liên quan realtime.

---

# 110. Live insert UX

Khi transaction mới:

- prepend row;
- animation nhẹ;
- cập nhật tổng tiền;
- không jump scroll nếu user đang xem lịch sử;
- hiện “1 giao dịch mới” nếu đang scroll xuống sâu;
- click để về top.

Không rerender toàn bảng lớn.

---

# 111. Memory control frontend

Không giữ vô hạn transaction trong React memory.

Ví dụ live buffer:

```text
500-1000 rows
```

Historical pages có thể virtualized/paginated.

---

# 112. Database cleanup

Poll run logs có retention.

Ví dụ:

```text
30 ngày
```

Audit có retention dài hơn.

Transaction financial records tùy business requirement.

Không để technical telemetry tăng DB mãi.

---

# 113. Event retention

SSE replay events không cần giữ vĩnh viễn.

Ví dụ:

```text
1-24h
```

hoặc dựa số lượng event.

Transaction vẫn nằm table chính.

---

# 114. Data privacy

Webhook payload chỉ chứa dữ liệu cần consumer.

Không default gửi:

- full account data;
- session;
- raw ACB HTML;
- internal diagnostic.

---

# 115. Error mapping

Internal:

```text
AUTH_REQUIRED
RATE_LIMITED
ACB_MAINTENANCE
```

UI:

```text
Cần đăng nhập lại ACB
ACB đang phản hồi chậm, hệ thống sẽ tự thử lại
Dịch vụ ACB đang tạm gián đoạn
```

Technical code chỉ trong admin diagnostic.

---

# 116. Alerting

Alert khi:

```text
last successful ACB poll > threshold
connection auth required
rate limit detected
webhook dead-letter > 0
DB error
dispatcher stuck
```

Không alert khi một transient timeout đơn lẻ.

---

# 117. Watchdog

Có internal watchdog:

```text
monitor heartbeat
dispatcher heartbeat
```

Nếu goroutine chết bất ngờ, process nên fail-fast hoặc health degraded rõ ràng thay vì im lặng.

---

# 118. Context cancellation

Mọi external call:

```go
http.NewRequestWithContext(...)
```

Shutdown phải cancel được.

Không background goroutine leak.

---

# 119. Panic recovery

HTTP middleware có recover.

Worker goroutine critical cũng cần wrapper/log/fail strategy.

Critical PollEngine panic không nên để process tiếp tục chạy như thể đang monitor.

---

# 120. Versioned events

SSE payload có version:

```json
{
  "version": 1,
  "eventId": "...",
  "type": "transaction.created",
  "occurredAt": "...",
  "data": {}
}
```

Giúp frontend/backward compatibility.

---

# 121. Webhook event schema

Webhook và SSE có thể dùng cùng domain event, nhưng serialization outward nên explicit.

Không đưa Go internal struct trực tiếp ra public contract nếu struct còn thay đổi.

---

# 122. Benchmark trước/sau

Trước refactor đo:

```text
ACB response -> transaction visible UI
ACB response -> webhook started
```

Sau refactor đo cùng metric.

Expected:

```text
UI: bỏ được tối đa ~5s internal waiting
Webhook: bỏ được tối đa ~2s internal waiting
```

---

# 123. Definition of Done

Không được coi là hoàn thành nếu chỉ “thêm WebSocket/SSE”.

Hoàn thành khi tất cả điều kiện sau đạt:

- [ ] React không còn interval 5 giây để refresh live data.
- [ ] Dashboard sử dụng SSE.
- [ ] SSE reconnect + replay hoạt động.
- [ ] Transaction mới patch state trực tiếp.
- [ ] Chỉ một PollEngine gọi ACB.
- [ ] Không concurrent ACB poll.
- [ ] Manual sync coalesce.
- [ ] Rate guard không thể bypass.
- [ ] Adaptive backoff hoạt động.
- [ ] Dispatcher được wake ngay khi có delivery mới.
- [ ] First webhook attempt không chờ ticker.
- [ ] Retry dùng deadline scheduler.
- [ ] Transaction + event + delivery durable/atomic.
- [ ] Crash recovery không mất event.
- [ ] ACB cookie không xuất hiện frontend/log.
- [ ] SSRF webhook protection còn nguyên hoặc tốt hơn.
- [ ] Cloudflare Access/RBAC áp dụng cho SSE.
- [ ] No-store/no-cache headers đúng.
- [ ] p95 internal propagation đạt target.
- [ ] Test race pass.
- [ ] Integration realtime test pass.
- [ ] Failure injection pass.
- [ ] Dashboard hiển thị realtime connection health.
- [ ] Không raw technical error key cho end-user.
- [ ] Poll config cũ được cleanup.
- [ ] Production documentation cập nhật.

---

# 124. Kiến trúc cuối cùng mong muốn

```text
                    +--------------------+
                    |        ACB         |
                    +----------+---------+
                               |
                 ONLY CONTROLLED DELAY
                               |
                               v
                    +--------------------+
                    |    Poll Engine     |
                    | single-flight      |
                    | rate protected     |
                    | adaptive backoff   |
                    +----------+---------+
                               |
                        HTTP response
                               |
========================= T0 =================================
                               |
                               v
                    +--------------------+
                    |       Parse        |
                    +----------+---------+
                               |
                               v
                    +--------------------+
                    | Atomic DB Commit   |
                    | txn/event/delivery |
                    +----------+---------+
                               |
                               v
                         +-----------+
                         | Event Hub |
                         +-----+-----+
                               |
           +-------------------+-------------------+
           |                                       |
           v                                       v
      SSE immediately                        Wake immediately
           |                                       |
           v                                       v
       React UI                              Dispatcher
                                                   |
                                                   v
                                                Webhook
```

Từ đường `T0` trở xuống:

```text
NO FIXED WAIT
NO POLLING
NO PERIODIC REFRESH
NO ARTIFICIAL DELAY
```

Đây là invariant quan trọng nhất của toàn bộ refactor.

---

# 125. Thứ tự ưu tiên triển khai

Nếu cần làm theo PR nhỏ, ưu tiên:

## PR 1 — Realtime transport

- EventHub
- SSE
- React EventSource
- bỏ frontend 5s polling

## PR 2 — Instant webhook dispatch

- `wakeCh`
- deadline retry scheduler
- bỏ dispatcher 2s ticker khỏi happy path

## PR 3 — PollEngine safety

- single owner
- single-flight
- rate guard
- jitter
- adaptive backoff
- circuit breaker

## PR 4 — Atomicity/recovery

- transactional outbox
- replay
- crash recovery

## PR 5 — Poll efficiency

- today-only realtime window
- midnight overlap
- separate backfill

## PR 6 — Security/observability

- SSE RBAC
- metrics
- sensitive log audit
- SSRF test
- latency SLO

## PR 7 — Production hardening

- load test
- chaos/failure tests
- documentation
- config cleanup
- deployment verification

---

# 126. Kết luận

Thiết kế phù hợp nhất cho repo không phải là:

```text
ACB polling
+
frontend polling
+
dispatcher polling
```

mà là:

```text
ONE controlled ACB poller
+
event-driven everything else
```

Cụ thể:

```text
ACB Web
  |
  v
Single-flight Adaptive Poll Engine
  |
  v
Atomic Transaction Ingestion
  |
  v
EventHub
  |
  +--> SSE -> React
  |
  +--> Instant Dispatcher Wake -> Webhook
```

Khi hoàn tất refactor, latency nội bộ không còn phụ thuộc bất kỳ interval 2s/5s nào.

**Độ trễ đáng kể duy nhất còn lại phải là:**

```text
thời điểm backend được phép gọi ACB
+
thời gian network
+
thời gian ACB xử lý/trả response
```

Ngay khi response ACB chứa giao dịch mới về tới gateway, hệ thống phải chuyển sang pipeline event-driven và phát transaction ra UI/webhook gần như tức thì.
