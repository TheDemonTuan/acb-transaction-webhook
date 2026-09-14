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

Chưa. Mình bỏ hẳn phần **GitHub Actions/deploy workflow** ra khỏi đánh giá và xem riêng application/runtime logic.

HEAD mới nhất hiện là `13092e0`, nhưng commit này chỉ sửa CI Trivy `arm64`, không thay application code; vì vậy core logic mới nhất vẫn tương ứng với `3b3f7e0`.

Mình đánh giá kiến trúc hiện tại đã lên khoảng **8.7–9/10**, nhưng còn **2 vấn đề P0 phải sửa trước khi gọi là production-complete**.

## P0 #1 — Worker verify session đang dùng sai ACB client

Đây là lỗi quan trọng nhất mình tìm được.

Trong worker bạn tạo:

```go
sessionLoader = monitor.NewSessionLoader(store, keyring, acbClient)
bankMonitor.WithSessionLoader(sessionLoader)

verifierClient, err = acb.NewClient(...)
```

Tức `sessionLoader` gắn với **polling `acbClient`**, còn `verifierClient` là instance khác.

Sau đó verify lại làm:

```go
verifier := monitor.NewSessionVerifier(
    w.sessionLoader,
    w.verifierClient,
    w.bankMonitor.UpstreamGate(),
)
```

Trong `SessionVerifier`:

```go
v.sessions.RestoreEnvelope(...)
response, err := v.client.Bootstrap(ctx)
```

Vấn đề là mỗi lần `acb.NewClient()` tạo **cookie jar riêng**:

```go
jar, err := cookiejar.New(nil)
```

và `bootstrap/bootstrapFields` cũng nằm riêng trong từng `Client`.

Thành ra thực tế:

```text
Browser login handoff
        ↓
SessionLoader.RestoreEnvelope()
        ↓
restore cookies + form
vào acbClient #1
        ↓
verifierClient #2.Bootstrap()
        ↓
client #2 không có cookie/form state
        ↓
❌ verification fail
```

Đặc biệt flow gateway thực sự gọi verifier **trước** `CompleteAuthSession()`:

```go
authVerifier.VerifySession(...)
...
store.CompleteAuthSession(...)
```

Điều thú vị là **monolith cũ làm đúng**: nó tạo `verifierLoader` từ chính `verifierClient`:

```go
verifierLoader :=
    monitor.NewSessionLoader(store, keyring, verifierClient)

server.WithAuthVerifier(
    monitor.NewSessionVerifier(
        verifierLoader,
        verifierClient,
        bankMonitor.UpstreamGate(),
    ),
)
```

Worker split đã làm mất logic này.

### Fix chuẩn

Worker nên có 2 loader:

```go
pollSessionLoader :=
    monitor.NewSessionLoader(store, keyring, acbClient)

bankMonitor.WithSessionLoader(pollSessionLoader)

verifierClient, err :=
    acb.NewClient("https://online.acb.com.vn", nil)

if err != nil {
    ...
}

verifierSessionLoader :=
    monitor.NewSessionLoader(
        store,
        keyring,
        verifierClient,
    )
```

Sau đó:

```go
ws := &workerService{
    bankMonitor:    bankMonitor,
    dispatcher:     dispatcher,
    sessionLoader:  verifierSessionLoader,
    verifierClient: verifierClient,
    store:          store,
}
```

Tốt hơn nữa đổi field thành:

```go
verifierSessionLoader *monitor.SessionLoader
```

để sau này không nhầm lại.

Mình xem đây là **P0** vì production architecture hiện dùng dedicated worker.

---

# P0 #2 — Realtime `PARTIAL` có thể bỏ lại giao dịch ở page 6+

Realtime poll cố tình giới hạn tối đa 5 page:

```go
for pagesCount < 5 {
    ...
}
```

Nếu page 5 vẫn:

```text
HasNext = true
```

thì code chỉ:

```go
isPartial = true
```

sau đó vẫn ingest các transaction đã lấy và đánh:

```go
poll.Status = "PARTIAL"
```

Ý tưởng này đúng để một realtime poll không đập ACB quá nhiều request.

**Nhưng thiếu bước quan trọng:**

```go
m.catchUpPending = true
```

Kết quả có thể là:

```text
ACB có 80 giao dịch mới
       ↓
Realtime poll
       ↓
page 1
page 2
page 3
page 4
page 5
       ↓
50 giao dịch được ingest
       ↓
page 6+ vẫn còn
       ↓
poll = PARTIAL
       ↓
KHÔNG schedule catch-up
       ↓
5–10 giây sau poll lại
       ↓
lại page 1 → page 5
       ↓
50 giao dịch cũ bị dedupe
       ↓
page 6+ vẫn không bao giờ tới
```

Catch-up hiện chủ yếu chạy khi chuyển:

```text
PAUSED/KEEPALIVE/startup → REALTIME
```

hoặc khi **chính catch-up** trước đó bị lỗi/incomplete.

Các chỗ `m.catchUpPending = true` hiện cũng chủ yếu nằm trong catch-up error path, không phải realtime PARTIAL.

Đối với hệ thống detect tiền vào, đây là P0 vì có thể khiến một transaction nằm ngoài page budget **không được phát webhook/Bark cho tới khi restart, đổi schedule hoặc manual history sync**.

### Fix

Ngay khi realtime partial:

```go
if isPartial {
    poll.Status = "PARTIAL"

    if pollErr != nil {
        poll.Error = pollErr.Error()
    } else {
        poll.Error = "PARTIAL_PAGE_BUDGET_REACHED"
    }

    m.catchUpPending = true
}
```

Flow chuẩn thành:

```text
Realtime
   ↓
<=5 pages
   ↓
complete ───────→ normal
   │
   └─ incomplete
          ↓
       PARTIAL
          ↓
  catchUpPending=true
          ↓
 next monitor cycle
          ↓
 day-by-day catch-up
          ↓
       realtime
```

Cách này vừa giữ realtime nhanh vừa đảm bảo **không bỏ transaction**.

---

# P1 — Dynamic pagination có bug ở range cực lớn

Hiện:

```go
neededPages := (maxTotalRowsSeen / 10) + 2

if neededPages > maxPages &&
   neededPages <= 50 {
    maxPages = neededPages
}
```

Ví dụ:

```text
TotalRows = 600

neededPages = 62
```

Do:

```text
62 <= 50
```

là false nên **không clamp thành 50**.

Nếu ban đầu:

```text
EnsureHistory maxPages = 10
```

nó vẫn chỉ chạy 10.

Catch-up:

```text
maxPages = 20
```

thì vẫn chỉ chạy 20.

Đúng ra:

```go
neededPages := (maxTotalRowsSeen + 9) / 10

if neededPages > 50 {
    neededPages = 50
}

if neededPages > maxPages {
    maxPages = neededPages
}
```

Hoặc tốt hơn nữa, vì đã có `HasNext`, chỉ cần:

```text
follow HasNext
until no next
or hardSafetyCap
```

Không nên phụ thuộc quá nhiều vào `TotalRows / 10`.

---

# P1 — Có real Go data race trong `eventhub`

Hiện `Publish()`:

```go
h.mu.RLock()
defer h.mu.RUnlock()
```

nhưng khi subscriber đầy:

```go
h.dropped++
```

`RLock` cho phép **nhiều reader đồng thời**, nhưng ở đây lại đang WRITE `dropped`.

Trong code hiện có nhiều nguồn gọi `Publish`: journal watcher, HTTP realtime events và monolith callbacks.

Nên đổi:

```go
type Hub struct {
    ...
    dropped atomic.Uint64
}
```

và:

```go
h.dropped.Add(1)
```

```go
func (h *Hub) DroppedNotifications() uint64 {
    return h.dropped.Load()
}
```

Không dùng full mutex chỉ để tăng counter.

---

# P1 security — `description_envelope` thực ra đang là plaintext

Schema đặt tên:

```text
description_envelope BLOB
```

nhưng insert:

```go
[]byte(item.Description)
```

và query còn:

```sql
CAST(description_envelope AS TEXT)
```

để search.

Nghĩa là database bị copy thì description giao dịch đọc được trực tiếp.

Đây không phải bug làm hệ thống chết, nhưng với dữ liệu ngân hàng thì mình không gọi đây là thiết kế security hoàn chỉnh.

Có hai lựa chọn:

```text
A. Chấp nhận plaintext
→ rename rõ description_plaintext
→ threat model ghi rõ
→ disk/backup encryption bắt buộc
```

hoặc mạnh hơn:

```text
B. AES-GCM envelope encrypt description
+
search index riêng
```

Sessions hiện đã mã hóa khá tốt; transaction description nên có policy tương xứng nếu mục tiêu là hardening cao.

---

# P2 — Docker worker healthcheck đang che lỗi readiness

`/worker --healthcheck` hiện:

```text
GET /readyz
  ↓ fail
GET /healthz
  ↓ 200
return success
```

Nghĩa là:

```text
SQLite/schema/worker readiness fail
but process alive
        ↓
Docker = healthy
```

Nếu đây là chủ đích **liveness** thì không sai, nhưng tên gọi đang gây hiểu nhầm.

Nên tách:

```bash
/worker --liveness-check
/worker --readiness-check
```

Docker restart policy dùng liveness.

Traffic/deploy/failover decision dùng readiness.

---

# P2 — Realtime worker → browser đang tự thêm tối đa ~1 giây delay

Gateway đang chạy:

```go
go server.RunJournalWatcher(ctx, 1*time.Second)
```

Do worker ghi event vào SQLite rồi gateway mới poll journal nên:

```text
ACB API response
 ↓
worker ingest
 ↓
SQLite journal
 ↓
0–1000 ms
 ↓
gateway notices
 ↓
SSE
 ↓
browser
```

Với mục tiêu trước đây của bạn là:

> delay chỉ nên nằm ở ACB → backend

thì 1 giây này vẫn hơi nhiều.

Mình chọn:

```go
150 * time.Millisecond
```

hoặc `200ms`.

SQLite WAL chịu mức này rất nhẹ trên một VPS.

Kiến trúc vẫn:

```text
SQLite journal = source of truth
eventHub       = wake-up hint
```

không cần Redis.

---

# P2 — History RPC timeout 25 giây hơi ngắn

Worker RPC toàn server đang có timeout khoảng:

```go
25 * time.Second
```

và `EnsureHistory()` client cũng khoảng 25s.

Nhưng một sync:

```text
31 days
× nhiều ACB pages
× network latency
```

hoàn toàn có thể >25s.

Tốt nhất `EnsureHistory` nên thành async:

```text
POST ensure-history
       ↓
202 Accepted
{
  jobId
}
       ↓
worker chạy job
       ↓
SQLite history_sync_jobs
       ↓
SSE progress
```

Thay vì giữ một HTTP RPC 25–60 giây.

---

## Những phần mình đánh giá đã rất ổn

Phần quan trọng là repo không phải “toàn lỗi”. Khung core bây giờ khá tốt:

| Logic                             |   Đánh giá |
| --------------------------------- | ---------: |
| Singleton ACB worker              | ✅ rất đúng |
| Upstream ACB mutex                |          ✅ |
| SQLite WAL + FULL synchronous     |          ✅ |
| Gateway/worker separation         |          ✅ |
| Generation fence                  |          ✅ |
| Atomic transaction ingest         |          ✅ |
| Durable event/outbox              |          ✅ |
| Delivery retry + lease            |          ✅ |
| Webhook timeout 15s < lease 30s   |          ✅ |
| SSRF protection webhook           |          ✅ |
| Session encrypted at rest         |          ✅ |
| Catch-up từng ngày                |          ✅ |
| Progressive checkpoint            |          ✅ |
| Incomplete catch-up không advance |          ✅ |
| Startup catch-up                  |          ✅ |
| SSE cursor/replay                 |          ✅ |
| Worker internal RPC token         |          ✅ |
| Failover container-name mismatch  |   ✅ đã sửa |
| Auth-browser failover name        |   ✅ đã sửa |

Catch-up mới đặc biệt tốt: mỗi ngày chỉ được mark coverage và advance checkpoint **sau khi `fetchRes.Complete`**, nếu incomplete thì giữ `catchUpPending` và retry.

---

## Thứ tự mình sẽ fix

```text
P0
1. verifierSessionLoader phải gắn verifierClient
2. realtime PARTIAL → catchUpPending=true

P1
3. pagination clamp >50
4. eventHub dropped counter atomic
5. xác định policy encryption transaction description

P2
6. split liveness/readiness
7. JournalWatcher 1s → ~150–250ms
8. EnsureHistory → async job
9. worker VerifySession require generation equality
```

Sau **hai P0 đầu tiên**, mình mới yên tâm cho hệ thống chạy thực tế để detect tiền ACB. Đặc biệt lỗi số 1 có thể trực tiếp giải thích các trường hợp kiểu **“login browser thành công nhưng worker không nhận session / xác thực session thất bại”**; lỗi số 2 liên quan trực tiếp tới mục tiêu **không bỏ sót giao dịch khi có burst nhiều giao dịch**.

Nếu chỉ xét **application core, bỏ CI/deploy Actions**, hiện tại mình chấm khoảng **8.8/10**. Sửa 2 P0 + pagination + race thì có thể lên khoảng **9.6/10**; phần còn lại chủ yếu là hardening và latency optimization.
