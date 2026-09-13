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

# PLAN V3 — ACB Transaction Webhook
## Transaction Viewer hoàn chỉnh · Filter đẩy xuống ACB đúng phạm vi · Polling theo khung giờ · Keepalive Session · QR tĩnh nhận tiền

> Repository: `TheDemonTuan/acb-transaction-webhook`  
> Baseline đã review: `main` tại commit `15a30aa1436c9512a5c310c6666bf0f82fde4309`  
> Ngày lập plan: 12/09/2026  
> Timezone mặc định: `Asia/Ho_Chi_Minh`  
> Mục tiêu: **bảo mật, nhanh, nhẹ, ổn định, realtime tối đa trong khung giờ cần realtime, giảm tải ACB ngoài giờ, không kéo dư dữ liệu, không mất giao dịch, UX dễ hiểu.**

---

# 0. Executive Summary

Bản plan này gom toàn bộ yêu cầu mới nhất vào một kiến trúc thống nhất, thay vì tiếp tục vá riêng từng phần.

Hệ thống mục tiêu có 5 nguyên tắc cốt lõi:

1. **Realtime background monitoring độc lập với filter UI.**
   - Trong khung giờ cần realtime: poll ACB ngẫu nhiên trong khoảng ví dụ `5–15 giây`.
   - Ngoài khung giờ: không cần tải History liên tục; chỉ `KEEPALIVE_ONLY` trong khoảng ví dụ `3–5 phút` để giữ phiên đăng nhập.
   - Khi bước từ keepalive → realtime, chạy **catch-up** một lần trước khi trở lại poll nhanh.

2. **Filter trên Transaction Viewer phải được xử lý server-side.**
   - `Hôm nay`, `7 ngày`, ngày tùy chọn, tiền vào/ra, search, pagination đều query SQLite.
   - Những filter ACB thực sự hỗ trợ, trước hết là **date range**, có thể được đẩy xuống ACB trong một **one-shot history sync** để không tải thừa.
   - Không đổi global realtime poll chỉ vì một user đang bấm filter.

3. **Không gọi ACB vô ích.**
   - Cache coverage theo date range.
   - Debounce filter.
   - Single-flight/coalescing các request cùng range.
   - Rate-limit guard.
   - Pagination guard.
   - Historical date đã sync thì TTL dài hơn.
   - `Tất cả` không tự động query ACB vô hạn.

4. **Transaction Viewer phải đúng dữ liệu.**
   - Fix triệt để format ngày ACB `DD/MM/YYYY`.
   - 3 card KPI không được cộng trên 100 rows gần nhất nữa.
   - Summary phải tính trực tiếp từ DB trên toàn bộ range.
   - Detail phải fetch được transaction cũ hơn 100 record.

5. **QR nhận tiền chỉ là QR tĩnh.**
   - Cho phép upload ảnh QR có sẵn.
   - Hoặc nhập ACB + số tài khoản + tên tài khoản để hệ thống tự tạo VietQR tĩnh một lần rồi cache local.
   - Viewer có nút sticky/floating `Nhận tiền`.
   - Click mở QR lớn cho khách scan.
   - Không có QR động theo amount, không correlation payment session.

---

# 1. Tình trạng source hiện tại

## 1.1 Polling hiện tại

Backend hiện có:

```go
PollMinInterval time.Duration
PollMaxInterval time.Duration
```

được load từ:

```env
POLL_MIN_INTERVAL_SEC=5
POLL_MAX_INTERVAL_SEC=15
```

Và monitor dùng random jitter trong khoảng đó.

Nhược điểm:

- Chỉ config qua ENV.
- Muốn đổi phải sửa env/restart container.
- Không có scheduler theo giờ.
- Không có mode `KEEPALIVE_ONLY`.
- Không có nhiều profile.
- Không có runtime update qua Admin UI.
- Giới hạn config hiện tại tối đa 300 giây.
- Một cấu hình dùng chung 24/7.

## 1.2 ACB history request hiện tại

`PrepareHistoryFields()` đang mặc định:

```text
FromDate = hôm qua
ToDate   = hôm nay
```

cho mỗi cycle.

Điều đó làm background polling 5–15 giây có thể liên tục kéo lại cả hôm qua + hôm nay, dù realtime thường chỉ cần một window nhỏ hơn.

## 1.3 Transaction Viewer hiện tại

Frontend đang:

```ts
fetchTransactions({ limit: 100 })
```

rồi:

- filter client-side,
- search client-side,
- tính KPI client-side,
- parse date trực tiếp bằng `new Date(transactionDate)`.

Có 3 vấn đề lớn:

1. ACB có thể trả `12/09/2026`; browser không được phép parse trực tiếp dạng này.
2. KPI chỉ tính tối đa 100 rows.
3. Filter không đại diện cho toàn DB.

## 1.4 Detail hiện tại

Detail vẫn lấy list `limit=100` rồi `.find(id)`.

Nếu transaction nằm ngoài 100 record gần nhất thì detail có thể báo không tìm thấy.

## 1.5 QR

Hiện repo chưa có payment QR subsystem.

Connection chỉ có thông tin account dạng masked phục vụ monitor; không nên tái sử dụng trực tiếp làm QR nhận tiền.

---

# 2. Target Architecture

```text
                         ┌──────────────────────┐
                         │       React UI       │
                         │ Transaction Viewer   │
                         │ Admin Settings       │
                         │ Static QR Modal      │
                         └──────────┬───────────┘
                                    │ REST + SSE
                                    ▼
┌────────────────────────────────────────────────────────────┐
│                       Go Gateway                           │
│                                                            │
│  ┌────────────────┐    ┌─────────────────────────────┐    │
│  │ Transaction API│    │ Runtime Monitor Settings    │    │
│  │ + DB filters   │    │ + Poll Schedule Resolver   │    │
│  │ + summary      │    └──────────────┬──────────────┘    │
│  └───────┬────────┘                   │                   │
│          │                            ▼                   │
│          │               ┌────────────────────────────┐   │
│          │               │ Monitor Scheduler          │   │
│          │               │ REALTIME                   │   │
│          │               │ KEEPALIVE_ONLY             │   │
│          │               │ CATCH_UP                   │   │
│          │               │ MANUAL_RANGE_SYNC          │   │
│          │               └──────────────┬─────────────┘   │
│          ▼                              ▼                 │
│       SQLite <──────────────────── ACB Client              │
│          │                                                │
│          ├── transactions                                 │
│          ├── poll_runs                                    │
│          ├── monitor_settings                             │
│          ├── monitor_schedule_windows                     │
│          ├── history_coverage                             │
│          └── payment_qr_settings                          │
│          │                                                │
│          ▼                                                │
│    SSE Event Journal ───────────────► React               │
│    Webhook Dispatcher ──────────────► External endpoints  │
└────────────────────────────────────────────────────────────┘
```

---

# 3. Quy tắc kiến trúc bắt buộc

## 3.1 Frontend không trực tiếp poll ACB

Client chỉ đọc API backend, cập nhật settings backend và có thể yêu cầu `history refresh` có kiểm soát.

Luôn:

```text
React → Go Gateway → ACB
```

## 3.2 Filter UI không được thay đổi global monitoring mode

Ví dụ user A chọn `7 ngày`, user B chọn `Hôm nay`.

Không được:

```text
User A chọn 7 ngày
→ global poll chuyển thành 7 ngày mỗi 5 giây
```

Global monitor phải độc lập với tab đang mở.

## 3.3 Realtime poll và history refresh là hai intent khác nhau

### Realtime poll

Mục tiêu: phát hiện giao dịch mới nhanh nhất có thể.

### History refresh

Mục tiêu: bảo đảm DB có đủ dữ liệu cho range user đang xem.

Hai flow dùng chung ACB client/upstream gate nhưng không dùng chung scheduler logic.

---

# 4. Fix P0 — Chuẩn hóa ngày ACB

## 4.1 Nguyên nhân

ACB có thể trả:

```text
12/09/2026
12/09/2026 10:32
12/09/2026 10:32:15
```

Frontend hiện parse kiểu:

```ts
new Date(raw)
```

không an toàn.

## 4.2 Canonical representation

Toàn domain dùng:

```text
2026-09-12
```

và nếu có giờ:

```text
2026-09-12T10:32:15+07:00
```

## 4.3 Normalize ở backend boundary

Tạo:

```text
internal/acb/date.go
```

API:

```go
func ParseACBTransactionDate(raw string, loc *time.Location) (time.Time, bool, error)
```

Support:

```text
02/01/2006
02/01/2006 15:04
02/01/2006 15:04:05
```

## 4.4 Storage

Khuyến nghị thêm:

```sql
ALTER TABLE transactions ADD COLUMN transaction_at_iso TEXT;
ALTER TABLE transactions ADD COLUMN transaction_day TEXT;
```

Giữ raw value nếu cần forensic.

## 4.5 API

```json
{
  "transactionDate": "2026-09-12T10:32:15+07:00",
  "transactionDay": "2026-09-12"
}
```

## 4.6 Backfill

1. Scan rows chưa có `transaction_day`.
2. Parse format cũ.
3. Fill canonical fields.
4. Log rows success/fail.
5. Không xóa raw.

## 4.7 Test bắt buộc

```text
12/09/2026           → 2026-09-12
12/09/2026 09:15     → 2026-09-12
01/02/2026           → 2026-02-01
31/12/2026           → 2026-12-31
invalid              → error rõ ràng
```

---

# 5. Transaction Query API mới

```http
GET /api/v1/transactions
```

Params:

```text
from=2026-09-12
to=2026-09-12
direction=all|credit|debit
q=search text
limit=50
cursor=...
```

Validation:

- date ISO `YYYY-MM-DD`,
- `from <= to`,
- direction whitelist,
- q max length,
- limit 1–100.

SQL filter:

```sql
WHERE transaction_day >= ?
  AND transaction_day <= ?
```

Direction:

```sql
credit > 0
```

hoặc:

```sql
debit > 0
```

Search chạy SQLite, không kéo 100 rows rồi filter browser.

---

# 6. KPI / Summary

Không còn:

```text
KPI = sum(current page)
```

Mà:

```text
KPI = aggregate SQL toàn range
```

Khuyến nghị response:

```json
{
  "items": [],
  "nextCursor": "...",
  "summary": {
    "count": 123,
    "incoming": 25400000,
    "outgoing": 6200000
  }
}
```

SQL:

```sql
SELECT
  COUNT(*) AS total_count,
  COALESCE(SUM(CASE WHEN credit > 0 THEN credit ELSE 0 END), 0) AS incoming,
  COALESCE(SUM(CASE WHEN debit > 0 THEN debit ELSE 0 END), 0) AS outgoing
FROM transactions
WHERE transaction_day BETWEEN ? AND ?;
```

Card label phải theo range:

```text
Hôm nay → Tiền vào hôm nay
7 ngày  → Tiền vào 7 ngày
```

---

# 7. Transaction Detail API

```http
GET /api/v1/transactions/:id
```

Không lấy 100 records rồi `.find()`.

Frontend:

```ts
queryKey: ['transaction', id]
```

---

# 8. Capability-aware Filter Pushdown xuống ACB

Yêu cầu: khi client áp filter, những filter ACB hỗ trợ nên truyền tới ACB để giảm dữ liệu trả về.

Tạo:

```go
type ACBHistoryCapabilities struct {
    DateRange bool
    Direction bool
    TextQuery bool
    Amount    bool
}
```

V1:

```go
DateRange: true
Direction: false
TextQuery: false
Amount: false
```

Mapping:

| UI filter | SQLite | Push ACB |
|---|---:|---:|
| Hôm nay | ✅ | ✅ |
| 7 ngày | ✅ | ✅ |
| Ngày tùy chọn | ✅ | ✅ |
| Tất cả | ✅ | ❌ auto |
| Tiền vào | ✅ | ❌ V1 |
| Tiền ra | ✅ | ❌ V1 |
| Search description | ✅ | ❌ |
| Search amount | ✅ | ❌ |
| Search ID | ✅ | ❌ |

---

# 9. Flow filter range

Ví dụ user bấm `7 ngày`.

## Step 1

DB trả ngay:

```text
GET /transactions?from=...&to=...
```

## Step 2

Kiểm tra coverage.

## Step 3

Nếu stale/missing:

```text
ACB FromDate=06/09/2026
ACB ToDate=12/09/2026
```

## Step 4

Ingest DB.

## Step 5

SSE `history.sync.completed`.

## Step 6

React Query refetch.

Không đổi global realtime poll thành 7 ngày.

---

# 10. Không gọi ACB mỗi lần click

Frontend date filter debounce khoảng:

```text
300–500ms
```

Search DB debounce:

```text
200–350ms
```

Tạo coverage table day-granularity:

```sql
CREATE TABLE history_coverage_days (
  connection_id TEXT NOT NULL,
  day TEXT NOT NULL,
  synced_at TEXT NOT NULL,
  status TEXT NOT NULL,
  PRIMARY KEY(connection_id, day)
);
```

TTL gợi ý:

```text
Hôm nay:
background realtime lo.

Hôm qua:
5–15 phút.

2–7 ngày trước:
30–60 phút.

Lịch sử cũ:
vài giờ hoặc manual.
```

`Tất cả` không được auto query ACB không giới hạn.

---

# 11. Single-flight / coalescing

Nếu 5 tab cùng request:

```text
06/09→12/09
```

chỉ chạy 1 upstream sync.

Key:

```go
type HistorySyncKey struct {
    ConnectionID string
    FromDay string
    ToDay string
}
```

Các request sau join operation đang chạy.

---

# 12. Poll Scheduler runtime configurable từ Admin UI

Đây là yêu cầu trọng tâm.

Admin phải chỉnh được:

- timezone,
- bật/tắt monitor,
- nhiều khung giờ,
- mode từng khung,
- min/max interval,
- ngày trong tuần,
- behavior ngoài khung.

Ví dụ:

```text
07:00–23:00
Realtime
5–15 giây

23:00–07:00
Chỉ giữ phiên
180–300 giây
```

Không restart container.

---

# 13. ENV chỉ là fallback

Giữ:

```env
POLL_MIN_INTERVAL_SEC
POLL_MAX_INTERVAL_SEC
```

nhưng chỉ là default nếu DB chưa có runtime settings.

Priority:

```text
DB runtime settings
>
ENV fallback
>
hardcoded safe defaults
```

---

# 14. DB runtime settings

```sql
CREATE TABLE monitor_settings (
  id TEXT PRIMARY KEY,
  enabled INTEGER NOT NULL DEFAULT 1,
  timezone TEXT NOT NULL DEFAULT 'Asia/Ho_Chi_Minh',
  default_mode TEXT NOT NULL DEFAULT 'KEEPALIVE_ONLY',
  default_min_interval_sec INTEGER NOT NULL DEFAULT 180,
  default_max_interval_sec INTEGER NOT NULL DEFAULT 300,
  revision INTEGER NOT NULL DEFAULT 1,
  updated_at TEXT NOT NULL,
  updated_by TEXT
);
```

Schedule windows:

```sql
CREATE TABLE monitor_schedule_windows (
  id TEXT PRIMARY KEY,
  settings_id TEXT NOT NULL REFERENCES monitor_settings(id),
  name TEXT NOT NULL,
  mode TEXT NOT NULL,
  days_mask INTEGER NOT NULL,
  start_minute INTEGER NOT NULL,
  end_minute INTEGER NOT NULL,
  min_interval_sec INTEGER NOT NULL,
  max_interval_sec INTEGER NOT NULL,
  priority INTEGER NOT NULL DEFAULT 0,
  enabled INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
```

Modes:

```text
REALTIME
KEEPALIVE_ONLY
PAUSED
```

Internal:

```text
CATCH_UP
MANUAL_SYNC
BACKOFF
AUTH_REQUIRED
```

---

# 15. Default settings đề xuất

```json
{
  "timezone": "Asia/Ho_Chi_Minh",
  "defaultMode": "KEEPALIVE_ONLY",
  "defaultInterval": {
    "minSeconds": 180,
    "maxSeconds": 300
  },
  "windows": [
    {
      "name": "Giờ hoạt động",
      "days": [1,2,3,4,5,6,7],
      "start": "07:00",
      "end": "23:00",
      "mode": "REALTIME",
      "minSeconds": 5,
      "maxSeconds": 15
    }
  ]
}
```

Các giờ trên chỉ là default minh họa; UI cho phép đổi.

---

# 16. Multiple schedule windows

Có thể cấu hình:

```text
06:30–11:30  realtime 5–10s
11:30–13:00  realtime 20–45s
13:00–23:00  realtime 5–15s
23:00–06:30  keepalive 180–300s
```

Support window qua đêm:

```text
22:00 → 07:00
```

---

# 17. Schedule Resolver

Tạo:

```text
internal/monitor/schedule.go
```

```go
type EffectiveSchedule struct {
    Mode PollMode
    MinInterval time.Duration
    MaxInterval time.Duration
    WindowID string
}
```

```go
func ResolveSchedule(
    now time.Time,
    settings MonitorSettings,
    windows []ScheduleWindow,
) EffectiveSchedule
```

Mỗi cycle xong resolve lại schedule.

Không giữ profile cũ nếu vừa qua mốc chuyển khung.

---

# 18. Admin UX — Lịch theo dõi ACB

Đặt:

```text
Admin
└── Kết nối ACB
    └── Lịch theo dõi
```

UI:

```text
Theo dõi giao dịch                      [Bật]

Múi giờ
[ Asia/Ho_Chi_Minh ▼ ]

┌──────────────────────────────────────────────┐
│ Giờ hoạt động                               │
│ 07:00 → 23:00                              │
│ Cập nhật realtime                           │
│ Mỗi 5–15 giây                               │
│ T2 T3 T4 T5 T6 T7 CN          [Sửa] [Xóa] │
└──────────────────────────────────────────────┘

Ngoài các khung giờ:
[ Chỉ giữ phiên đăng nhập ▼ ]
Mỗi [180] – [300] giây

[ + Thêm khung giờ ]              [Lưu thay đổi]
```

Preview:

```text
Đang áp dụng:
Realtime · 5–15 giây

23:00:
Chuyển sang giữ phiên · 3–5 phút
```

---

# 19. API Monitor Settings

Read:

```http
GET /api/v1/monitor/settings
```

Update:

```http
PUT /api/v1/monitor/settings
```

Request gồm:

```json
{
  "revision": 3,
  "enabled": true,
  "timezone": "Asia/Ho_Chi_Minh",
  "defaultProfile": {
    "mode": "KEEPALIVE_ONLY",
    "minSeconds": 180,
    "maxSeconds": 300
  },
  "windows": []
}
```

Security:

- CSRF,
- owner/operator role,
- optimistic revision,
- audit.

---

# 20. Hot reload scheduler

Sau save:

```text
DB commit
↓
audit
↓
settingsChanged channel
↓
monitor wake timer
↓
resolve config mới
```

Không restart backend.

Không đọc DB mỗi cycle; dùng in-memory snapshot + wake signal.

---

# 21. Run loop mới

Pseudo:

```go
for {
    schedule := resolver.Resolve(now)
    wait := jitter(schedule.Min, schedule.Max)
    timer.Reset(wait)

    select {
    case <-ctx.Done():
        return

    case <-settingsChanged:
        stopAndDrain(timer)
        continue

    case <-manualSync:
        runManualSync()

    case <-timer.C:
        switch schedule.Mode {
        case REALTIME:
            runRealtimePoll()

        case KEEPALIVE_ONLY:
            runKeepalive()

        case PAUSED:
            // no upstream request
        }
    }
}
```

---

# 22. REALTIME mode

Mục tiêu:

- detect giao dịch mới nhanh,
- ingest ngay,
- SSE ngay,
- webhook ngay,
- voice từ authoritative event.

Date range thường:

```text
FromDate=today
ToDate=today
```

Không kéo hôm qua cả ngày.

---

# 23. Midnight overlap

Khoảng đầu ngày, ví dụ 10 phút:

```text
00:00–00:10
```

cho phép:

```text
FromDate=yesterday
ToDate=today
```

để chống race giao dịch sát 00:00.

Backend config internal:

```text
midnightOverlapMinutes=10
```

---

# 24. KEEPALIVE_ONLY mode

Ngoài giờ realtime:

```text
3–5 phút
```

không nên tải History.

Flow:

```text
Restore session
↓
Bootstrap authenticated page
↓
Classify response
↓
Persist refreshed cookies/session
↓
lastKeepaliveAt
```

Không:

```text
ParseHistory
IngestTransactionsBatch
bank.transaction.credit
```

Nếu test thực tế cho thấy Bootstrap không đủ refresh session thì chọn authenticated endpoint nhẹ nhất được xác minh.

---

# 25. Kiểm chứng keepalive interval

Không được giả định 3–5 phút chắc chắn giữ session ACB.

Test:

1. Login.
2. Keepalive 5 phút.
3. Chạy 6–12 giờ.
4. Verify không AUTH_REQUIRED.
5. Nếu session vẫn expire, giảm xuống 90–150 giây hoặc giá trị phù hợp.

UI có warning nếu user đặt quá dài.

---

# 26. Transition KEEPALIVE → REALTIME: Catch-up

Khi bước vào giờ realtime:

```text
KEEPALIVE_ONLY
→ CATCH_UP
→ REALTIME
```

Catch-up dựa trên checkpoint:

```text
lastSuccessfulRealtimePollAt
```

Nếu cùng ngày:

```text
today→today
```

Nếu qua đêm:

```text
yesterday→today
```

Nếu downtime dài:

```text
date(lastRealtimePoll)→today
```

có cap:

```text
MAX_AUTOMATIC_CATCHUP_DAYS=7
```

Không mất transaction ngoài giờ.

---

# 27. Catch-up event behavior

Transactions mới chưa có DB vẫn ingest.

Webhook vẫn dispatch một lần theo idempotency.

Voice không được đọc hàng loạt giao dịch cũ.

Event nên thêm:

```json
{
  "source": "catchup",
  "announceEligible": false
}
```

Realtime:

```json
{
  "source": "realtime",
  "announceEligible": true
}
```

---

# 28. Backoff / rate limit ưu tiên cao hơn schedule

Priority:

```text
Circuit breaker / 429 / maintenance
>
Auth state
>
Schedule
```

Nếu ACB trả 429, dù schedule 5–15s vẫn phải backoff.

Có thể:

```text
60s
120s
300s
```

rồi reset khi success.

---

# 29. Poll Run metadata

Thêm:

```sql
ALTER TABLE poll_runs ADD COLUMN mode TEXT;
ALTER TABLE poll_runs ADD COLUMN range_from TEXT;
ALTER TABLE poll_runs ADD COLUMN range_to TEXT;
ALTER TABLE poll_runs ADD COLUMN schedule_window_id TEXT;
```

Mode:

```text
REALTIME
KEEPALIVE
CATCH_UP
MANUAL_RANGE
```

---

# 30. History Range Sync API

```http
POST /api/v1/transactions/refresh
```

```json
{
  "from": "2026-09-06",
  "to": "2026-09-12"
}
```

Nên queue + operation ID:

```json
{
  "status": "QUEUED",
  "operationId": "..."
}
```

SSE:

```text
history.sync.started
history.sync.completed
history.sync.failed
```

---

# 31. ACB pagination

History nhiều ngày cần hỗ trợ pagination nếu ACB phân trang.

Cần nghiên cứu form/state cụ thể:

- next page action,
- processor state,
- page index,
- hidden state.

Guard:

```text
MAX_HISTORY_PAGES_PER_SYNC=20
```

Stop khi:

- không next page,
- không rows,
- page signature lặp,
- max pages,
- context cancel,
- rate limit.

---

# 32. Upstream concurrency

Tất cả request ACB cùng session phải serialize:

```text
REALTIME
KEEPALIVE
CATCH_UP
MANUAL_RANGE
```

Nhưng range sync không được block realtime quá lâu.

Nếu pagination nhiều page, có thể yield giữa pages để realtime due poll được ưu tiên.

---

# 33. Viewer Filter UX

Date:

```text
Hôm nay
7 ngày
Tùy chọn
Tất cả
```

Type:

```text
Tất cả
Tiền vào
Tiền ra
```

Search:

```text
Nội dung, số tiền, mã giao dịch...
```

Nên sync filter vào URL:

```text
/transactions?range=today&type=credit&q=abc
```

---

# 34. Hôm nay khi REALTIME active

```text
User click Hôm nay
→ DB query
→ không ACB request thêm
```

Background monitor đã giữ today fresh.

---

# 35. Hôm nay khi KEEPALIVE_ONLY

Không tự phá schedule.

Render DB.

Có thể hiện:

```text
Ngoài giờ realtime
```

và button:

```text
Kiểm tra ACB ngay
```

nếu user muốn one-shot sync.

---

# 36. `7 ngày`

```text
DB trả ngay
↓
coverage stale?
↓ yes
one-shot ACB 7-day sync
↓
SSE completion
↓
UI refresh
```

Background realtime scheduler không đổi.

---

# 37. `Tất cả`

Query DB.

Không auto gọi ACB unbounded.

Nếu muốn history rộng:

```text
Chọn khoảng ngày để đồng bộ từ ACB
```

---

# 38. Manual “Kiểm tra ACB ngay”

Button hiện tại `Làm mới` nên semantic rõ.

Nếu action thực sự hit ACB:

```text
Kiểm tra ACB ngay
```

Nếu chỉ React refetch DB thì không cần vì SSE đã realtime.

Manual sync vẫn respect:

- gate,
- rate limit,
- auth state.

---

# 39. Static QR — scope

Chỉ QR tĩnh.

Không:

- QR theo amount,
- QR reference,
- payment session,
- auto payment correlation.

Hai cách:

1. Upload ảnh QR có sẵn.
2. Tự generate VietQR static rồi cache local.

ACB BIN theo VietQR hiện là:

```text
970416
```

---

# 40. QR Upload

Support:

```text
PNG
JPEG
WebP
```

Limit đề xuất:

```text
5MB
```

Backend:

- validate magic bytes,
- decode image,
- reject malformed,
- reject SVG,
- normalize PNG,
- resize nếu cực lớn,
- save local.

Path:

```text
/data/payment-qr/<uuid>.png
```

---

# 41. QR Generate

Input:

```text
Bank: ACB
Account number
Account name
Template
```

Có thể dùng VietQR Quick Link hoặc Generate API.

Tạo một lần → validate ảnh → cache local.

Không hotlink mỗi lần modal mở.

Lợi ích:

- mở nhanh,
- không phụ thuộc network lúc quét,
- giảm external calls,
- dễ backup.

---

# 42. QR DB

```sql
CREATE TABLE payment_qr_settings (
  id TEXT PRIMARY KEY,
  enabled INTEGER NOT NULL DEFAULT 0,
  source TEXT NOT NULL,
  bank_code TEXT NOT NULL,
  bank_bin TEXT NOT NULL,
  account_number TEXT NOT NULL,
  account_name TEXT NOT NULL,
  image_path TEXT NOT NULL,
  image_sha256 TEXT NOT NULL,
  revision INTEGER NOT NULL DEFAULT 1,
  updated_at TEXT NOT NULL,
  updated_by TEXT
);
```

---

# 43. QR API

Viewer:

```http
GET /api/v1/payment-qr
GET /api/v1/payment-qr/image
```

Admin upload:

```http
PUT /api/v1/payment-qr/upload
```

Admin generate:

```http
POST /api/v1/payment-qr/generate
```

Disable:

```http
PATCH /api/v1/payment-qr
```

---

# 44. QR Viewer UX

Desktop floating button:

```text
[ ▦ Nhận tiền ]
```

bottom-right.

Mobile sticky pill:

```css
bottom: calc(16px + env(safe-area-inset-bottom));
```

Modal:

```text
Quét mã để chuyển khoản

[        QR 420–480px        ]

ACB
NGUYEN VAN A
123456789

[ Sao chép số tài khoản ]
[ Phóng to ]
```

Không gọi button là “QR”; dùng intent “Nhận tiền”.

---

# 45. QR Admin UX

```text
Kết nối ACB
└── Mã QR nhận tiền
```

```text
Mã QR nhận tiền                         [Bật]

Nguồn:
(•) Upload ảnh QR có sẵn
( ) Tạo VietQR tự động

Preview:
[QR]

[ Lưu ]
```

Viewer không có quyền sửa.

---

# 46. Atomic QR replace

```text
write temp
↓
validate
↓
close/fsync
↓
atomic rename
↓
DB update
```

Nếu generate/upload fail:

```text
giữ QR cũ
```

---

# 47. QR security

- không tin browser MIME,
- validate decoder,
- reject SVG,
- random server filename,
- không path traversal,
- owner/operator write,
- viewer read-only,
- CSRF write APIs,
- audit changes.

---

# 48. Status API mở rộng

```json
{
  "monitor": {
    "mode": "KEEPALIVE_ONLY",
    "lastPollAt": "...",
    "lastRealtimePollAt": "...",
    "lastKeepaliveAt": "...",
    "lastCatchupAt": "...",
    "nextRunAt": "...",
    "nextTransitionAt": "...",
    "backoffUntil": null
  }
}
```

---

# 49. SSE events mới

Giữ SSE, không WebSocket.

```text
bank.transaction.credit
poll.completed
history.sync.started
history.sync.completed
history.sync.failed
monitor.settings.changed
monitor.mode.changed
payment_qr.changed
```

---

# 50. Scheduler safety bounds

Backend hard bound đề xuất:

Realtime:

```text
min >= 5s
max <= 300s
```

Keepalive:

```text
min >= 60s
max <= 1800s
```

Không cho client set 0–1 giây.

---

# 51. UX presets

Realtime:

```text
Nhanh       5–15s
Cân bằng   15–30s
Tiết kiệm  30–60s
Tùy chỉnh
```

Keepalive:

```text
3–5 phút
5–10 phút
Tùy chỉnh
```

Vẫn lưu exact min/max.

---

# 52. Observability

Metrics:

```text
acb_monitor_mode
acb_poll_interval_seconds
acb_last_realtime_poll_timestamp
acb_last_keepalive_timestamp
acb_last_catchup_timestamp
acb_history_sync_inflight
acb_history_rows_seen
acb_history_pages_seen
acb_rate_limit_count
acb_session_expired_count
```

Log fields:

```text
mode
range_from
range_to
rows_seen
pages
inserted_count
duration_ms
schedule_window
request_reason
```

---

# 53. Error codes

```text
INVALID_DATE_RANGE
RANGE_TOO_LARGE
ACB_RATE_LIMITED
ACB_SESSION_EXPIRED
ACB_MAINTENANCE
HISTORY_SYNC_BUSY
HISTORY_SYNC_FAILED
MONITOR_SETTINGS_CONFLICT
INVALID_POLL_SCHEDULE
QR_INVALID_IMAGE
QR_GENERATION_FAILED
```

Frontend map semantic Vietnamese.

---

# 54. Tests — Date/KPI

Seed:

```text
150 transactions today
```

API limit 50.

Expected:

```text
items=50
summary.count=150
```

Test `12/09/2026` phải lọc đúng ngày 12/09.

---

# 55. Tests — Schedule

```text
06:59 -> KEEPALIVE
07:00 -> REALTIME
22:59 -> REALTIME
23:00 -> KEEPALIVE
```

Cross midnight.

Days-of-week.

Multiple windows.

Overlap validation.

---

# 56. Tests — Hot reload

1. Scheduler đang wait 4 phút.
2. Admin đổi sang realtime 5–15s.
3. settings changed wake timer.
4. config mới apply ngay.

---

# 57. Tests — Keepalive

Mock:

```text
Bootstrap success
History must NOT be called
```

Expected:

```text
Bootstrap=1
History=0
```

Persist refreshed session nếu có.

---

# 58. Tests — Catch-up

```text
23:00 keepalive
02:00 transaction A
06:00 transaction B
07:00 realtime resumes
```

Expected:

- A/B ingest đúng một lần,
- webhook đúng một lần,
- voice không đọc transaction stale,
- regular realtime bắt đầu sau catch-up.

---

# 59. Tests — Coalescing

10 client cùng ensure:

```text
06/09→12/09
```

Expected:

```text
1 upstream ACB sync
```

---

# 60. Tests — Rate limit

ACB mock 429.

Expected:

- breaker active,
- 5s schedule không bypass backoff,
- no request storm.

---

# 61. Tests — QR

Upload:

- valid PNG/JPEG/WebP,
- fake image reject,
- SVG reject,
- oversized reject.

Generate:

- success cache local,
- provider failure giữ QR cũ.

Viewer:

- sticky button,
- modal,
- copy account,
- mobile safe-area.

---

# 62. Proposed files

Backend:

```text
internal/acb/date.go
internal/acb/date_test.go

internal/monitor/mode.go
internal/monitor/schedule.go
internal/monitor/schedule_test.go
internal/monitor/keepalive.go
internal/monitor/history_sync.go

internal/storage/monitor_settings.go
internal/storage/history_coverage.go
internal/storage/payment_qr.go

internal/httpapi/transactions.go
internal/httpapi/monitor_settings.go
internal/httpapi/payment_qr.go

internal/paymentqr/service.go
```

Frontend:

```text
web/src/features/transactions/
web/src/features/monitor-settings/
web/src/features/payment-qr/

web/src/pages/viewer/TransactionsPage.tsx
web/src/pages/viewer/TransactionDetailPage.tsx
web/src/pages/admin/BankConnectionPage.tsx
```

---

# 63. Migration

```sql
BEGIN;

ALTER TABLE transactions ADD COLUMN transaction_at_iso TEXT;
ALTER TABLE transactions ADD COLUMN transaction_day TEXT;

CREATE INDEX IF NOT EXISTS idx_transactions_transaction_day
ON transactions(transaction_day);

CREATE TABLE monitor_settings (...);
CREATE TABLE monitor_schedule_windows (...);
CREATE TABLE history_coverage_days (...);
CREATE TABLE payment_qr_settings (...);

ALTER TABLE poll_runs ADD COLUMN mode TEXT;
ALTER TABLE poll_runs ADD COLUMN range_from TEXT;
ALTER TABLE poll_runs ADD COLUMN range_to TEXT;
ALTER TABLE poll_runs ADD COLUMN schedule_window_id TEXT;

COMMIT;
```

Backup SQLite trước migration.

---

# 64. Deployment phases

## Phase 1 — Correctness P0

1. Normalize date.
2. Backfill.
3. Server filters.
4. Server summary.
5. Detail endpoint.
6. Frontend query refactor.

## Phase 2 — Runtime polling settings

1. DB schema.
2. repository.
3. API.
4. schedule resolver.
5. Admin UI.
6. hot reload.

## Phase 3 — Realtime / Keepalive split

1. modes.
2. realtime poll.
3. keepalive.
4. transition.
5. catch-up.
6. metrics.

## Phase 4 — Range-aware history sync

1. coverage.
2. one-shot sync.
3. single-flight.
4. pagination.
5. filter integration.

## Phase 5 — Static QR

1. storage.
2. upload.
3. generate.
4. Viewer button/modal.
5. mobile QA.

---

# 65. PR breakdown

```text
PR1 fix: normalize ACB transaction dates and today filtering
PR2 feat: server-side transaction filters summary and detail endpoint
PR3 feat: persisted runtime ACB monitoring schedule
PR4 feat: realtime vs lightweight keepalive scheduler modes
PR5 feat: catch-up after keepalive/off-hours
PR6 feat: coverage-aware ACB history range synchronization
PR7 feat: static receive-money QR upload and generation
PR8 test: harden monitoring filter sync QR and realtime E2E
```

---

# 66. CI

CI phải chạy:

```text
go test ./...
tsc
vite build
vitest
Playwright critical E2E
```

Main push cũng nên có CI hoặc branch protection bắt buộc PR.

---

# 67. Rollout

1. Deploy date/filter/KPI trước.
2. Deploy persisted settings nhưng chưa bật multi-mode.
3. Enable schedule.
4. Theo dõi keepalive session vài giờ/ngày.
5. Enable history range sync.
6. Enable QR.

Feature flag fallback nếu cần:

```env
ENABLE_RUNTIME_MONITOR_SCHEDULE=true
```

Tắt → fallback ENV.

---

# 68. Acceptance — Transaction

- [ ] ACB `12/09/2026` parse đúng.
- [ ] Hôm nay có dữ liệu đúng.
- [ ] 7 ngày đúng.
- [ ] Custom range đúng.
- [ ] Direction đúng.
- [ ] Search đúng.
- [ ] KPI không phụ thuộc page 100.
- [ ] Detail transaction cũ hoạt động.
- [ ] URL giữ filter.

---

# 69. Acceptance — Scheduler

- [ ] Config được từ client Admin.
- [ ] Không restart.
- [ ] Timezone đúng.
- [ ] Multiple windows.
- [ ] Cross-midnight.
- [ ] Realtime 5–15s.
- [ ] Keepalive 3–5m.
- [ ] Hot reload.
- [ ] ENV fallback.
- [ ] Backoff ưu tiên schedule.
- [ ] Không concurrent upstream requests.

---

# 70. Acceptance — Keepalive/Catch-up

- [ ] Keepalive không gọi History.
- [ ] Session được duy trì nếu ACB behavior cho phép.
- [ ] Session expiration phát hiện đúng.
- [ ] Resume realtime có catch-up.
- [ ] Không mất overnight transaction.
- [ ] Không voice spam transaction cũ.

---

# 71. Acceptance — Filter Pushdown

- [ ] Today maps đúng ACB today.
- [ ] 7 days maps đúng range.
- [ ] Unsupported filters chỉ xử lý SQLite.
- [ ] Coverage TTL.
- [ ] Same range coalesced.
- [ ] All không unbounded fetch.
- [ ] Pagination guard.

---

# 72. Acceptance — QR

- [ ] Upload QR tĩnh.
- [ ] Generate ACB VietQR tĩnh.
- [ ] Cache local.
- [ ] `Nhận tiền` sticky.
- [ ] QR modal lớn.
- [ ] Mobile safe-area.
- [ ] Copy account.
- [ ] Failed regenerate không làm mất QR cũ.
- [ ] Viewer read-only.

---

# 73. Recommended production default

Nếu chưa có dữ liệu thực tế về business hours:

```text
Timezone: Asia/Ho_Chi_Minh

Realtime:
07:00–23:00
5–15s

Off-hours:
KEEPALIVE_ONLY
180–300s

Midnight overlap:
10 phút

Automatic catch-up:
max 7 ngày

Auto history range:
max 31 ngày

History max pages:
20
```

Tất cả business-hour parameters phải chỉnh được từ UI.

---

# 74. Final runtime behavior

```text
07:00–23:00
ACB History today
ngẫu nhiên 5–15s
↓
new transaction
↓
SQLite
↓
SSE
↓
Viewer + voice + webhook

23:00–07:00
không History
↓
lightweight keepalive
3–5 phút

07:00
catch-up
↓
ingest missed transactions
↓
realtime resumes
```

Filter:

```text
Client filter
↓
SQLite query ngay
↓
range coverage stale?
↓
one-shot ACB range sync
```

QR:

```text
[ Nhận tiền ]
↓
static ACB QR
↓
khách scan
```

---

# 75. Kết luận

Ba luồng phải tách rõ:

```text
1. Realtime monitoring
2. History synchronization
3. Client filtering
```

Kiến trúc cuối:

```text
Realtime scheduler
├── REALTIME 5–15s
└── KEEPALIVE 3–5m

Client filters
└── SQLite query

Date coverage stale
└── one-shot ACB range sync

SSE
└── push giao dịch mới

Static QR
└── local cached image
```

Đây là hướng phù hợp nhất để vừa giữ realtime trong giờ cần thiết, vừa giảm request lên ACB ngoài giờ, giữ session, tránh kéo dư dữ liệu, không phụ thuộc tab frontend, không mất transaction và cho phép cấu hình hoàn toàn từ Admin UI.
