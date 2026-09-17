# QR Activation + Adaptive ACB Polling Implementation Plan

> **For agentic workers:** triển khai task-by-task, test từng task trước khi sang task kế tiếp. Không thêm Redis, message queue hoặc service mới chỉ để phục vụ adaptive polling.

**Suggested path:** `docs/superpowers/plans/2026-09-17-qr-activation-adaptive-polling.md`

## 1. Mục tiêu

Thay polling ACB theo kiểu luôn luôn nhanh bằng mô hình **event-assisted adaptive polling**:

```text
Không có khách
    ↓
20–30 giây / poll
    ↓
Khách scan QR kích hoạt
    ↓
đợi 5 giây
    ↓
2–4 giây / poll
    ↓
3–6 giây / poll
    ↓
6–10 giây / poll
    ↓
20–30 giây / poll
```

Mục tiêu:

- khi cửa hàng không có giao dịch: tải ACB thấp;
- khi khách chuẩn bị thanh toán: độ trễ phát hiện giao dịch thấp;
- không chạy polling riêng theo từng khách;
- không spam API ACB;
- giữ nguyên scheduler, backoff, event journal, SSE và notification pipeline hiện tại;
- scan nhiều lần chỉ kéo dài/khởi động lại cửa sổ tăng tốc;
- lỗi ACB/rate-limit/network phải ưu tiên backoff hơn QR boost;
- không cần persistent database state cho boost;
- restart worker chỉ làm boost mất và quay về idle, không làm hỏng dữ liệu.

---

# 2. Polling profile đề xuất

## Phase 0 — IDLE

```text
interval: random 20–30s
duration: vô hạn
```

Trung bình:

```text
~25s / request
≈ 2.4 ACB poll / phút
```

Đây phải là `REALTIME` polling chậm, không phải `KEEPALIVE_ONLY`, nếu yêu cầu của bạn là dù không ai scan thì hệ thống vẫn kiểm tra giao dịch mỗi 20–30 giây.

Source hiện tại phân biệt rõ:

```text
REALTIME
KEEPALIVE_ONLY
PAUSED
```

và profile REALTIME hiện cho phép min từ 3 giây trở lên.

Config thông thường có thể thành:

```json
{
  "mode": "REALTIME",
  "minSeconds": 20,
  "maxSeconds": 30
}
```

Không nên đổi validation thông thường xuống 2 giây.

**2 giây chỉ được phép bên trong QR burst override.**

---

# 3. Phase 1 — SCAN_GRACE

Khi khách scan:

```text
T = 0s
QR_ACTIVATED
```

Không poll ACB ngay.

```text
T = 0 → 5s
SCAN_GRACE
```

Lý do:

Khách vừa scan URL thì còn phải:

- mở landing page;
- mở app ngân hàng;
- xác nhận ngân hàng nhận;
- nhập/xác nhận số tiền;
- Face ID/PIN;
- bấm chuyển.

Poll ACB ngay ở giây 0–1 thường không mang lại giá trị.

Do đó:

```text
fresh activation:
firstPollNotBefore = now + 5s
```

### Quan trọng

Grace 5 giây **chỉ áp dụng khi chuyển từ IDLE sang boost**.

Nếu hệ thống đang HOT/WARM/COOL mà có khách khác scan:

```text
KHÔNG đợi lại 5 giây
```

Ví dụ:

```text
00:00 khách A scan
00:05 bắt đầu HOT

00:38 khách B scan

KHÔNG:
00:38 → 00:43 ngừng poll

MÀ:
00:38 tiếp tục HOT
      +
      kéo dài HOT window
```

---

# 4. Phase 2 — HOT

Từ:

```text
T = 5s
```

đến:

```text
T = 60s
```

poll ngẫu nhiên:

```text
2–4 giây
```

Tức khoảng:

```text
average = ~3s
```

Khoảng HOT thực tế:

```text
55 giây
```

Số request trung bình nếu không có lỗi:

```text
55 / 3
≈ 18 poll
```

### Detection latency

Sau khi giao dịch đã xuất hiện trên ACB:

```text
best case   ≈ gần 0s
average     ≈ 1.5s
worst poll  ≈ 4s
```

chưa tính latency của HTTP request ACB.

Sau khi worker đã lấy được transaction thì pipeline downstream hiện tại không cần bị làm chậm theo cadence ACB; gateway fallback `RunJournalWatcher` của repo đang chạy ở 200ms.

---

# 5. Phase 3 — WARM

Từ:

```text
T = 60s
```

đến:

```text
T = 120s
```

poll:

```text
3–6 giây
```

average:

```text
~4.5s
```

Khoảng này giữ độ realtime khá cao cho các trường hợp:

- khách mở app chậm;
- app ngân hàng yêu cầu xác thực;
- mạng điện thoại chậm;
- khách nhập lại số tiền;
- khách scan nhưng chưa chuyển ngay.

Khoảng 60 giây WARM tạo:

```text
60 / 4.5
≈ 13 poll
```

---

# 6. Phase 4 — COOL

Từ:

```text
T = 120s
```

đến:

```text
T = 180s
```

poll:

```text
6–10 giây
```

average:

```text
~8s
```

Khoảng:

```text
~7–8 poll
```

Nếu đến phút thứ 2 mà khách vẫn chưa thanh toán thì xác suất giao dịch liên quan đến scan ban đầu đã thấp hơn nhiều.

Không còn lý do duy trì 2–4s.

---

# 7. Trở lại IDLE

Sau:

```text
T >= 180s
```

nếu không có activation mới:

```text
20–30s
```

Toàn bộ một activation không thành công sẽ tạo khoảng:

```text
HOT   ≈ 18 polls
WARM  ≈ 13 polls
COOL  ≈ 7–8 polls

≈ 39 polls trong khoảng 3 phút
```

Trong khi idle 3 phút:

```text
180 / 25
≈ 7 polls
```

Như vậy tải tăng mạnh **chỉ trong cửa sổ có tín hiệu khách chuẩn bị thanh toán**.

---

# 8. State machine cuối cùng

```text
                         QR SCAN
                            │
                            ▼
┌──────────────┐       ┌──────────────┐
│     IDLE     │──────▶│ SCAN_GRACE   │
│              │       │              │
│ 20–30 sec    │       │ exact 5 sec  │
└──────────────┘       └───────┬──────┘
        ▲                       │
        │                       ▼
        │               ┌──────────────┐
        │               │     HOT      │
        │               │              │
        │               │    2–4s      │
        │               │ until T=60s  │
        │               └───────┬──────┘
        │                       │
        │                       ▼
        │               ┌──────────────┐
        │               │     WARM     │
        │               │              │
        │               │    3–6s      │
        │               │ 60s → 120s   │
        │               └───────┬──────┘
        │                       │
        │                       ▼
        │               ┌──────────────┐
        │               │     COOL     │
        │               │              │
        │               │    6–10s     │
        │               │120s → 180s   │
        │               └───────┬──────┘
        │                       │
        └───────────────────────┘
```

---

# 9. Scan mới trong khi boost đang chạy

Đây là phần rất quan trọng.

Không tạo:

```text
scan A → goroutine polling A
scan B → goroutine polling B
scan C → goroutine polling C
```

Sai hoàn toàn vì 10 khách có thể tạo 10 luồng gọi ACB.

Thay vào đó:

```text
                  ┌───────────────┐
scan A ──────────▶│               │
scan B ──────────▶│ PollBoostState│
scan C ──────────▶│               │
                  └───────┬───────┘
                          │
                          ▼
                 ONE ACB SCHEDULER
                          │
                          ▼
                         ACB
```

Repo hiện đã có scheduler với logic coalesce task realtime trùng key thay vì queue vô hạn.

### Rule activation

Fresh activation:

```go
if !boost.Active(now) {
    boost.FirstPollNotBefore = now.Add(5 * time.Second)
}
```

Sau đó:

```go
boost.HotUntil  = max(boost.HotUntil,  now.Add(60*time.Second))
boost.WarmUntil = max(boost.WarmUntil, now.Add(120*time.Second))
boost.CoolUntil = max(boost.CoolUntil, now.Add(180*time.Second))
```

Nhưng tôi đề xuất behavior mạnh hơn một chút:

### Scan mới khi WARM/COOL

Cho trở về HOT:

```text
WARM + scan → HOT
COOL + scan → HOT
```

không grace.

Ví dụ:

```text
12:00:00 scan A
12:00:05 HOT

12:01:25 đang WARM

12:01:30 scan B
       ↓
HOT ngay
       ↓
HOT kéo đến khoảng 12:02:30
```

Điều này rất hợp với cửa hàng có nhiều khách.

---

# 10. Cấu trúc code

## Create

`internal/monitor/poll_boost.go`

Chỉ chịu trách nhiệm state machine.

Ví dụ interface:

```go
type PollBoostPhase string

const (
    PollBoostIdle  PollBoostPhase = "IDLE"
    PollBoostGrace PollBoostPhase = "GRACE"
    PollBoostHot   PollBoostPhase = "HOT"
    PollBoostWarm  PollBoostPhase = "WARM"
    PollBoostCool  PollBoostPhase = "COOL"
)

type PollBoostProfile struct {
    Phase          PollBoostPhase
    Active         bool
    MinInterval    time.Duration
    MaxInterval    time.Duration
    NextTransition time.Time
    FirstPollAt    time.Time
}

type PollBoostState struct {
    FirstPollNotBefore time.Time
    HotUntil           time.Time
    WarmUntil          time.Time
    CoolUntil          time.Time
}
```

API thuần:

```go
func (b *PollBoostState) Activate(now time.Time)

func (b PollBoostState) Resolve(now time.Time) PollBoostProfile
```

Không HTTP.

Không DB.

Không scheduler.

Không ACB client.

Nhờ vậy unit test cực dễ.

---

# 11. Profile constants

Không hard-code rải rác.

Trong `poll_boost.go`:

```go
const (
    paymentGraceDuration = 5 * time.Second

    paymentHotDuration  = 60 * time.Second
    paymentWarmDuration = 120 * time.Second
    paymentCoolDuration = 180 * time.Second

    paymentHotMin  = 2 * time.Second
    paymentHotMax  = 4 * time.Second

    paymentWarmMin = 3 * time.Second
    paymentWarmMax = 6 * time.Second

    paymentCoolMin = 6 * time.Second
    paymentCoolMax = 10 * time.Second
)
```

Ở version đầu **không cần biến tất cả thành ENV**.

Đây là YAGNI.

Khi có production metrics thực tế rồi mới quyết định có expose tuning hay không.

---

# 12. Integrate vào `Monitor`

Modify:

`internal/monitor/monitor.go`

Monitor hiện có:

- `cachedSettings`;
- `settingsCh`;
- timer;
- scheduler;
- random `nextInterval`;
- network backoff;
- realtime poll scheduling.

Thêm:

```go
boostMu sync.RWMutex
boost   PollBoostState
boostCh chan struct{}
```

Constructor:

```go
boostCh: make(chan struct{}, 1),
```

Method:

```go
func (m *Monitor) ActivatePaymentWindow() {
    now := m.now()

    m.boostMu.Lock()
    m.boost.Activate(now)
    m.boostMu.Unlock()

    select {
    case m.boostCh <- struct{}{}:
    default:
    }
}
```

`boostCh` chỉ có nhiệm vụ:

```text
WAKE CURRENT TIMER
```

Không trực tiếp gọi ACB.

---

# 13. Không dùng goroutine per activation

Không làm:

```go
go func() {
    time.Sleep(5 * time.Second)

    for {
        m.PollOnce(...)
        time.Sleep(...)
    }
}()
```

Lý do:

- race;
- duplicate poll;
- không cooperate scheduler;
- khó stop khi deploy;
- khó quiesce;
- goroutine leak;
- scan spam tạo rất nhiều loop;
- phá logic singleton ACB.

Thay vào đó **duy nhất `Monitor.Run()` quyết định lần poll tiếp theo**.

---

# 14. Resolve effective polling profile

Trong mỗi vòng `Monitor.Run()`:

```go
base := storage.ResolveSchedule(now, &m.cachedSettings)
boost := m.resolvePollBoost(now)
```

Sau đó:

```go
effective := resolveEffectiveProfile(base, boost)
```

Priority:

```text
1. Context shutdown/quiesce
2. ACB circuit breaker / network backoff
3. Explicit admin PAUSED
4. QR boost
5. Base monitoring schedule
```

### Vì sao PAUSED thắng QR

Nếu admin đã bấm:

```text
PAUSED
```

thì một anonymous QR scan không được phép tự ý khởi động ACB polling.

### Nhưng KEEPALIVE_ONLY thì khác

QR activation có thể:

```text
KEEPALIVE_ONLY
        ↓ scan
REALTIME burst
        ↓ hết boost
KEEPALIVE_ONLY
```

Điều này rất hữu ích ngoài giờ.

---

# 15. Timer phải được interrupt khi có scan

Hiện có thể đang:

```text
next poll = 24 giây nữa
```

Khách scan ở giây hiện tại.

Nếu không wake timer:

```text
scan
↓
vẫn đợi 24s
```

thì adaptive polling vô nghĩa.

Do đó:

```go
case <-m.boostCh:
    stopAndDrainTimer(timer)
    continue
```

Run loop lập tức recompute:

```text
fresh scan
↓
GRACE
↓
next timer = 5s
```

Đến 5 giây:

```text
enqueue NewRealtimeTask(...)
```

Sau poll đó:

```text
jitter(2s, 4s)
```

---

# 16. Không thay global REALTIME minimum thành 2s

Hiện validation của schedule bắt:

```text
REALTIME min >= 3s
```



Tôi đề xuất **giữ nguyên**.

Không sửa thành:

```go
if p.MinSeconds < 2
```

vì như vậy admin có thể vô tình cấu hình:

```text
2–2s
24/7
```

và biến temporary boost thành permanent pressure lên ACB.

QR burst là special-case, bounded và tự hết hạn.

---

# 17. Backoff phải luôn thắng QR boost

Monitor hiện đã có network failure backoff tăng:

```text
5s
10s
20s
40s
80s
```

theo số lỗi liên tiếp.

Do đó:

```text
QR says:
2–4s

BUT

ACB/network says:
backoff 20s

→ wait 20s
```

Không được QR scan reset circuit breaker.

Không được activation tạo manual retry bypass backoff.

Jitter/backoff là pattern tốt để tránh request đồng bộ tạo spike và retry storm; AWS cũng khuyến nghị bounded retries + backoff + jitter cho dependency failure.

---

# 18. QR architecture

Tôi **không đề xuất gọi VietQR API từ xa sau mỗi lần scan** trong version này.

Repo hiện đã có payment QR lưu local và public endpoint trả `imageURL` cho QR đã lưu.

Vì vậy:

```text
VietQR generation
      ↓
generate once
      ↓
save locally
      ↓
reuse
```

VietQR có API `v2/generate`, hỗ trợ account, BIN, amount, addInfo và template.

Nhưng với QR nhận tiền cố định hiện tại, gọi provider mỗi scan chỉ thêm:

- external dependency;
- latency;
- possible provider outage;
- API authentication;
- rate limit;
- thêm failure mode.

### Kiến trúc nên là

```text
QR kích hoạt
     ↓
URL của bạn
     ↓
Landing Page
     ↓
POST activation
     ↓
boost ACB worker
     ↓
show cached VietQR
     +
button mở ngân hàng
```

---

# 19. QR kích hoạt

QR mà khách thấy ngoài quầy chứa URL ví dụ:

```text
https://transactions.example.com/pay/x8K3q7...
```

Không chứa API key.

Không chứa worker address.

Không chứa account session.

`x8K3q7...` là random public identifier.

Dùng ít nhất:

```text
128-bit random
```

Identifier này không cần xem là authentication secret.

Nó chủ yếu chống:

- crawler dễ đoán URL;
- bot quét `/pay/1`, `/pay/2`;
- accidental activation.

---

# 20. Đừng kích hoạt chỉ bằng GET

Không nên:

```http
GET /pay/x8K3...
→ activate polling
```

vì GET có thể bị gọi bởi:

- browser prefetch;
- link preview;
- crawler;
- security scanner;
- proxy.

Thay vào đó:

```text
GET /pay/x8K3...
      ↓
load HTML/React
      ↓
browser JS
      ↓
POST /api/public/v1/payment-qr/activate
```

---

# 21. Public activation endpoint

Modify router:

`internal/httpapi/server.go`

Thêm:

```text
POST /api/public/v1/payment-qr/activate
```

Không để endpoint gọi ACB trực tiếp.

Handler chỉ:

```text
validate
↓
rate/debounce
↓
worker RPC
↓
return
```

Response ví dụ:

```json
{
  "trackingActive": true,
  "phase": "GRACE"
}
```

Nếu worker đang unavailable:

```json
{
  "trackingActive": false
}
```

nhưng **khách vẫn được thanh toán**.

QR/deeplink không được phụ thuộc vào việc worker boost thành công.

---

# 22. Gateway → Worker RPC

Repo hiện đã có `workerrpc` để gateway yêu cầu worker:

- sync;
- settings changed;
- dispatcher wake;
- các thao tác worker khác.

Extend:

`internal/workerrpc/rpc.go`

thêm:

```go
ActivatePaymentWindow(ctx context.Context) error
```

Worker:

`cmd/worker/main.go`

implementation:

```go
func (w *workerService) ActivatePaymentWindow(ctx context.Context) error {
    if w.bankMonitor == nil {
        return errors.New("bank monitor not initialized")
    }

    w.bankMonitor.ActivatePaymentWindow()
    return nil
}
```

Luồng:

```text
Internet
   ↓
Cloudflare
   ↓
Traefik
   ↓
Gateway
   ↓
Worker RPC
   ↓
bankMonitor.ActivatePaymentWindow()
   ↓
boostCh
   ↓
Monitor.Run()
   ↓
Scheduler
   ↓
ACB
```

Không expose worker ra Internet.

---

# 23. Anti-abuse

Đây là requirement bắt buộc.

Public viewer hiện đã có edge limits cho anonymous REST và SSE.

Nhưng activation nguy hiểm hơn `GET /transactions`, vì một activation có thể dẫn tới hàng chục ACB requests.

## Application-level collapse

Tất cả activation đến trong khoảng:

```text
2–3 giây
```

được xem như một activation.

Ví dụ:

```text
10 users scan gần nhau

POST
POST
POST
POST
POST
      ↓
one shared state update
```

Return vẫn 200/202.

Không queue từng activation.

---

# 24. Per-IP rate limit

Đề xuất riêng cho endpoint:

```text
burst:        2
sustained:    ~1 request / 5s / IP
minute cap:   ~10–12 / IP
```

Nhưng **global boost không phụ thuộc IP**.

Một attacker đổi IP vẫn không tạo N polling loops vì monitor chỉ có một shared state.

Đây mới là defense quan trọng nhất.

---

# 25. Idempotency

Activation phải idempotent.

```text
POST #1 → activate
POST #2 → extend/collapse
POST #3 → extend/collapse
```

Không có:

```text
3 DB rows
3 jobs
3 timers
3 loops
```

---

# 26. Frontend landing page

Create:

`web/src/pages/viewer/PaymentPage.tsx`

hoặc theo cấu trúc route viewer hiện có.

Page:

```text
┌──────────────────────────────┐
│ Thanh toán                   │
│                              │
│ [ Mở ACB ONE ]               │
│                              │
│ Hoặc quét mã VietQR          │
│                              │
│       ███████████            │
│       █ VietQR ██            │
│       ███████████            │
│                              │
│ ACB                          │
│ STK: ********                │
│                              │
│ Đang chờ giao dịch...        │
└──────────────────────────────┘
```

On first mount:

```ts
activatePaymentPolling()
```

Nhưng React development StrictMode có thể chạy effect hơn một lần.

Client nên có:

```ts
const activatedRef = useRef(false)
```

và server vẫn phải idempotent.

Không tin client guard là security boundary.

---

# 27. Deeplink ACB ONE

VietQR hiện đã document deeplink ACB ONE có autofill thông tin thanh toán.

Vì vậy landing page có thể:

```text
[Mở ACB ONE]
```

và fallback:

```text
[VietQR image]
```

Điều này tốt hơn bắt khách:

```text
scan QR kích hoạt
↓
thấy QR thứ hai trên cùng điện thoại
↓
không có camera khác để scan
```

---

# 28. Current ReceivingQRModal

Current:

`web/src/features/payment-qr/ReceivingQRModal.tsx`

đã:

- load Payment QR;
- render `imageURL`;
- subscribe `bank.transaction.credit`;
- chỉ xem `source === REALTIME`;
- cập nhật giao dịch nhận tiền realtime.

Không cần rewrite component này.

Chỉ cần quyết định UI nào hiển thị:

```text
Activation QR
```

thay cho raw VietQR ở màn hình dành cho khách.

Luồng giao dịch realtime hiện tại giữ nguyên.

---

# 29. Không correlate transaction với scan ở V1

Ở version đầu:

```text
scan ≠ payment session
```

Scan chỉ là:

```text
"hệ thống có xác suất cao sắp có giao dịch"
```

Do đó không làm:

```text
scan A → transaction X
scan B → transaction Y
```

vì với static QR không có unique transfer reference đủ mạnh.

Điều này giúp implementation đơn giản và chính xác.

Sau này nếu muốn checkout/session thực sự:

```text
unique addInfo
+
amount
+
payment session
```

thì VietQR API hỗ trợ cả `amount` và `addInfo`.

Đó nên là Phase 2 riêng, không nhét vào lần này.

---

# 30. Không stop boost ngay khi thấy transaction

Ví dụ:

```text
A scan
B scan
C scan

A chuyển tiền
↓
transaction xuất hiện
```

Nếu stop boost ngay:

```text
B/C bị trở về 20–30s
```

Không tốt.

Vì vậy V1:

```text
transaction detected
≠
terminate boost
```

Boost chỉ hết bởi thời gian hoặc được extend bởi scan mới.

---

# 31. Telemetry

Thêm metrics nhẹ:

```text
acb_poll_boost_activations_total
acb_poll_boost_collapsed_total
acb_poll_boost_phase
acb_poll_interval_seconds
```

Có thể log:

```text
poll_boost activated
phase=GRACE

poll_boost transitioned
from=GRACE
to=HOT

poll_boost transitioned
from=HOT
to=WARM

poll_boost expired
phase=IDLE
```

Không log:

- raw client IP nếu không cần;
- full account number;
- QR secret/token;
- ACB credentials.

---

# 32. Task 1 — PollBoostState

**Create**

```text
internal/monitor/poll_boost.go
internal/monitor/poll_boost_test.go
```

Tests:

```go
func TestPollBoostFreshActivationHasFiveSecondGrace(t *testing.T)

func TestPollBoostHotProfile(t *testing.T)

func TestPollBoostWarmProfile(t *testing.T)

func TestPollBoostCoolProfile(t *testing.T)

func TestPollBoostExpiresToIdle(t *testing.T)

func TestPollBoostActivationDuringHotExtendsWithoutGrace(t *testing.T)

func TestPollBoostActivationDuringWarmReturnsToHot(t *testing.T)
```

Sử dụng fake `now`.

Không `time.Sleep()` trong unit tests.

Acceptance:

```text
T+0       GRACE
T+4.999   GRACE
T+5       HOT 2–4
T+59      HOT
T+60      WARM 3–6
T+120     COOL 6–10
T+180     IDLE
```

---

# 33. Task 2 — Monitor integration

**Modify**

```text
internal/monitor/monitor.go
```

Add:

```text
boostMu
boost
boostCh
ActivatePaymentWindow()
```

Modify Run loop để chọn effective schedule.

Tests cần chứng minh:

```text
base next poll = ~25s
↓ activation
timer wakes
↓
first boosted poll around +5s
```

Không đợi timer idle cũ.

---

# 34. Task 3 — Priority tests

Test:

### Explicit pause

```text
PAUSED + QR activation
→ no ACB request
```

### Keepalive

```text
KEEPALIVE_ONLY + QR
→ temporary REALTIME burst
```

### Backoff

```text
HOT = 2–4s
network backoff = 20s

effective wait >= 20s
```

### Shutdown

```text
context cancelled
→ boost cannot keep monitor alive
```

### Race

Run:

```bash
go test -race ./internal/monitor
```

---

# 35. Task 4 — Worker RPC

**Modify**

```text
internal/workerrpc/rpc.go
internal/workerrpc/rpc_test.go
cmd/worker/main.go
```

Add:

```go
ActivatePaymentWindow(context.Context) error
```

Tests:

```text
gateway RPC
↓
worker handler called
↓
monitor activation called
```

Update existing RPC mocks so test suite vẫn compile.

---

# 36. Task 5 — Public activation API

**Create**

```text
internal/httpapi/payment_activation.go
internal/httpapi/payment_activation_test.go
```

**Modify**

```text
internal/httpapi/server.go
```

Route:

```http
POST /api/public/v1/payment-qr/activate
```

Tests:

- POST valid → success;
- GET → method not allowed/not routed;
- worker unavailable → safe degraded result;
- no ACB/internal metadata leaked;
- `Cache-Control: no-store`;
- repeated activation safe;
- malformed identifier rejected;
- endpoint never contacts VietQR provider directly.

---

# 37. Task 6 — Payment UI

**Modify**

```text
web/src/shared/api/queries.ts
```

Add:

```ts
export const activatePaymentPolling = async () =>
  publicApi('/payment-qr/activate', {
    method: 'POST',
  });
```

**Create payment page**

Responsibilities:

1. activate tracking;
2. display cached VietQR;
3. direct-bank button/deeplink;
4. subscribe existing realtime events;
5. display:

```text
Đang chờ thanh toán
```

then:

```text
Đã nhận tiền
```

Không cần polling transaction API từ browser.

Backend ACB worker đã poll.

Browser dùng existing realtime SSE pipeline.

---

# 38. Task 7 — Edge anti-abuse

Extend existing public edge protection cho activation endpoint.

Requirements:

```text
Cloudflare Tunnel only
↓
Traefik
↓
activation-specific rate limit
↓
gateway
```

Không expose host port.

Không bypass existing `public-deny-private`.

Tests tại:

```bash
go test -v ./platform/edge/...
```

phải tiếp tục pass.

---

# 39. Task 8 — Integration test

Scenario:

```text
base poll 20–30
      ↓
activation request
      ↓
worker RPC
      ↓
5s grace
      ↓
RealtimeTask
      ↓
fake ACB response
      ↓
transaction storage
      ↓
journal
      ↓
SSE
      ↓
frontend
```

Dùng fake clock/fake BankClient nếu có thể.

Không viết integration test phải chờ thật 3 phút.

---

# 40. Acceptance criteria cuối

Feature chỉ được coi là hoàn thành khi:

- [ ] Không scan → mỗi poll random 20–30s.
- [ ] Fresh scan → timer idle đang chạy bị interrupt ngay.
- [ ] Fresh scan → không poll trước 5s.
- [ ] Sau 5s → interval random 2–4s.
- [ ] Sau 60s từ activation → 3–6s.
- [ ] Sau 120s → 6–10s.
- [ ] Sau 180s → trở về 20–30s.
- [ ] Scan mới trong HOT không thêm grace 5s.
- [ ] Scan mới trong WARM/COOL quay lại HOT.
- [ ] 100 scan đồng thời vẫn chỉ có một polling stream.
- [ ] Scheduler không tích tụ duplicate realtime tasks.
- [ ] PAUSED không bị anonymous scan override.
- [ ] KEEPALIVE có thể tạm boost.
- [ ] Network/circuit-breaker backoff thắng QR profile.
- [ ] Không thêm Redis.
- [ ] Không thêm DB migration cho boost.
- [ ] Không gọi VietQR API mỗi lần scan.
- [ ] Cached QR vẫn thanh toán được nếu worker tạm unavailable.
- [ ] Cached QR vẫn dùng được nếu VietQR provider tạm down.
- [ ] Existing webhook/Bark/SSE pipeline không bị thêm delay.
- [ ] `go test -race ./internal/monitor ./internal/scheduler ./internal/workerrpc` pass.
- [ ] HTTP API tests pass.
- [ ] Edge tests pass.
- [ ] Frontend tests/build pass.

---

# 41. Kiến trúc cuối

```text
                  NORMAL
                20–30 sec
                    │
                    │
          ┌─────────▼─────────┐
          │ Activation QR     │
          │ URL của hệ thống  │
          └─────────┬─────────┘
                    │ scan
                    ▼
              PAYMENT PAGE
                    │
                    │ POST activate
                    ▼
              PUBLIC GATEWAY
                    │
               rate limit
               debounce
                    │
                    ▼
                 RPC
                    │
                    ▼
             SINGLE WORKER
                    │
             PollBoostState
                    │
          ┌─────────┴─────────┐
          │                   │
        GRACE                QR/UI
         5 sec             cached VietQR
          │                   │
          ▼                   ▼
        HOT                bank app
        2–4s                  │
          │                   │
          ▼                   │
        WARM                  │
        3–6s                  │
          │                   │
          ▼                   │
        COOL                  │
        6–10s                 │
          │                   │
          └──────┐     ┌──────┘
                 ▼     ▼
                    ACB
                     │
                transaction
                     │
                     ▼
                  STORE
                     │
                     ▼
             DURABLE JOURNAL
                     │
                     ▼
                  GATEWAY
               ┌─────┼─────┐
               ▼     ▼     ▼
              SSE  Webhook Bark
               │
               ▼
         PAYMENT SUCCESS
```

# 42. Những thứ cố ý KHÔNG làm

Để tránh overengineering:

```text
❌ Redis
❌ Kafka
❌ NATS
❌ payment-session database
❌ goroutine per scan
❌ cron riêng
❌ second polling worker
❌ gọi VietQR provider mỗi scan
❌ thay toàn bộ scheduler
❌ hạ global realtime minimum xuống 2s
❌ correlate scan với transaction trong V1
```

Chỉ thêm:

```text
1 PollBoostState
1 wake channel
1 worker RPC method
1 public activation endpoint
1 payer landing flow
một ít anti-abuse + telemetry
```

Đó là đủ.
