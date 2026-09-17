# Public Transaction Viewer — Simplified Implementation Plan

> Bản này **thay thế plan cũ** `docs/archive/Public Transaction Viewer - Cloudflare Access Split — Implementation Plan.md`.

**Goal:** Tách `transactions.tuannguyenviet.site` thành trang xem giao dịch anonymous/read-only, không bị Cloudflare Access chặn; giữ `bank.tuannguyenviet.site` làm admin và vẫn bắt buộc Cloudflare Access.

**Architecture:** Giữ nguyên toàn bộ worker, database, gateway, frontend image và cơ chế blue/green hiện tại. Chỉ thêm một public API namespace rất nhỏ, public SSE filter, chế độ frontend public/read-only và thêm hostname thứ hai vào Traefik.

**Tech Stack:** Go + chi, React/Vite, TanStack Query, SSE, Nginx frontend container, Traefik, Cloudflare Tunnel/Access.

**Spec:** Current-source revision of `docs/archive/Public Transaction Viewer - Cloudflare Access Split — Implementation Plan.md`.

---

# 1. Kiến trúc cuối cùng

```text
                         Cloudflare
                            |
              +-------------+-------------+
              |                           |
 bank.tuannguyenviet.site       transactions.tuannguyenviet.site
              |                           |
      Cloudflare Access                 PUBLIC
              |                           |
              +-------------+-------------+
                            |
                    same Cloudflare Tunnel
                            |
                         Traefik
                    /                 \
          active frontend         active gateway
           blue / green            blue / green
                |                       |
            React SPA             Go HTTP API
                                      |
                         +------------+------------+
                         |                         |
                     /api/v1/*            /api/public/v1/*
                       AUTH                  ANONYMOUS
                                              GET/SSE
                                                |
                                             SQLite
                                                |
                                             Worker
                                                |
                                               ACB
```

Source hiện tại đã có frontend Nginx container blue/green riêng và gateway blue/green riêng, nên dùng lại chính topology này là hợp lý nhất.  

## Không làm những thứ sau

Không tạo:

```text
public-gateway container
viewer container riêng
Redis
database riêng
public database replica
internal/httpviewer
Vite build thứ hai
public event hub riêng
public worker
public polling worker
token service
PIN service
CORS giữa 2 domain
SSE connection manager mới
```

Các thứ này không mang lại lợi ích tương xứng với source hiện tại.

---

# 2. Boundary cần giữ

## ADMIN

```text
https://bank.tuannguyenviet.site
```

Giữ nguyên:

```text
Cloudflare Access
    ↓
React admin
    ↓
/api/v1/*
    ↓
Cloudflare JWT / RBAC
```

Không sửa cơ chế auth hiện tại.

Backend hiện đặt toàn bộ `/api/v1` sau:

```go
api.Use(s.auth.Require(auth.Owner, auth.Operator, auth.Viewer))
```

và mutation tiếp tục yêu cầu `Owner/Operator`. Đó là boundary tốt, giữ nguyên. 

## PUBLIC VIEWER

```text
https://transactions.tuannguyenviet.site
```

Chỉ có:

```text
GET transaction list
GET transaction detail
GET QR metadata
GET QR image
GET SSE transaction stream
```

Không có:

```text
login ACB
auth-browser
sync ACB
history sync
monitor settings
poll settings
webhook settings
notification settings
Bark settings
audit
diagnostics
connection state
CSRF
server-side TTS API
POST
PUT
PATCH
DELETE
```

---

# 3. Task 1 — Tạo public API read-only nhỏ

## Files

```text
Modify:
internal/httpapi/server.go
internal/httpapi/sse.go

Create:
internal/httpapi/public.go
internal/httpapi/public_test.go
```

Không tạo package mới.

## Route

Trong `server.go`, đặt public API **ngoài** `/api/v1` authenticated group:

```go
r.Route("/api/public/v1", func(api chi.Router) {
    api.Get("/transactions", s.publicTransactions)
    api.Get("/transactions/{id}", s.publicTransactionDetail)

    api.Get("/payment-qr", s.publicPaymentQR)
    api.Get("/payment-qr/image", s.getPaymentQRImage)

    api.Get("/events", s.publicEventsStream)
})
```

Không đăng ký method mutation.

Do đó:

```text
GET    /api/public/v1/transactions        -> 200
GET    /api/public/v1/payment-qr          -> 200
GET    /api/public/v1/events              -> SSE

POST   /api/public/v1/...                 -> 405
PUT    /api/public/v1/...                 -> 405
DELETE /api/public/v1/...                 -> 405
```

## Transaction DTO

Storage hiện có:

```go
type TransactionView struct {
    ID             string
    SemanticKey    string
    TransactionAt  string
    TransactionDay string
    DatePrecision  string
    EffectiveAt    string
    Debit          int64
    Credit         int64
    Balance        *int64
    Description    string
    FirstSeenAt    string
    Source         string
}
```



Public response giữ lại:

```go
type publicTransaction struct {
    ID              string `json:"id"`
    SemanticKey     string `json:"semanticKey"`
    TransactionDate string `json:"transactionDate"`
    TransactionDay  string `json:"transactionDay,omitempty"`
    DatePrecision   string `json:"datePrecision,omitempty"`
    EffectiveDate   string `json:"effectiveDate"`
    Debit           int64  `json:"debit"`
    Credit          int64  `json:"credit"`
    Description     string `json:"description"`
    FirstSeenAt     string `json:"firstSeenAt"`
    Source          string `json:"source,omitempty"`
}
```

Đặc biệt:

```text
KHÔNG trả balance
```

UI hiện tại render `tx.balance` khi field tồn tại, nên chỉ cần public DTO không gửi field đó thì public UI tự nhiên không hiển thị số dư. 

Không cần sửa schema database.

Không cần query database mới.

Vẫn gọi:

```go
s.store.ListTransactionsFiltered(...)
s.store.GetTransactionByID(...)
```

Các hàm này đã tồn tại.  

## QR DTO

Storage QR hiện chứa cả:

```text
ConnectionID
ImagePath
ImageHash
Provider
Revision
CreatedAt
UpdatedAt
```



Public không cần chúng.

Chỉ trả:

```json
{
  "configured": true,
  "hasImage": true,
  "imageURL": "/api/public/v1/payment-qr/image",
  "qr": {
    "accountNumber": "...",
    "accountName": "...",
    "bin": "970416",
    "bankName": "ACB"
  }
}
```

Không trả:

```text
connectionId
imagePath
imageHash
createdAt
updatedAt
provider
revision
```

QR image handler hiện có thể reuse trực tiếp.

## HTTP headers

Cho JSON public:

```http
Cache-Control: no-store
```

Không cần CORS vì:

```text
transactions.tuannguyenviet.site
        ↓
same-origin
        ↓
/api/public/v1/*
```

---

# 4. Task 2 — Public SSE nhưng reuse event journal hiện tại

Không tạo event bus thứ hai.

SSE hiện tại đọc journal, replay cursor, heartbeat, gap recovery và live event khá đầy đủ rồi. 

Chỉ refactor:

```go
eventsStream()
```

thành core dùng chung:

```go
eventsStreamFiltered(
    w,
    r,
    allowEvent func(string) bool,
)
```

Admin:

```go
func (s *Server) eventsStream(w http.ResponseWriter, r *http.Request) {
    s.eventsStreamFiltered(w, r, nil)
}
```

Public:

```go
func (s *Server) publicEventsStream(w http.ResponseWriter, r *http.Request) {
    s.eventsStreamFiltered(w, r, func(eventType string) bool {
        return eventType == "bank.transaction.credit"
    })
}
```

Khi gặp event không được public:

```go
watermark = entry.Seq
continue
```

Điều này rất quan trọng để cursor không bị kẹt.

Public SSE vẫn dùng:

```text
initial_state
reset_state
heartbeat
stream_error
bank.transaction.credit
```

Nhưng không gửi:

```text
connection.changed
auth.changed
webhook.changed
notification.changed
delivery.changed
poll.completed
audit.created
```

Code hiện đã sanitize riêng `bank.transaction.credit` bằng cách bỏ:

```text
balance
accountNumber
sessionToken
```

nên giữ nguyên lớp bảo vệ đó. 

### Không thêm

```text
SSE server riêng
Redis Pub/Sub
NATS
WebSocket
connection broker
custom SSE limiter
```

Không cần.

---

# 5. Task 3 — Chuyển React sang public runtime mode

Source hiện tại vẫn có Admin và Viewer trong cùng router. 

Điều này không cần giải quyết bằng frontend build thứ hai.

Repo còn public trên GitHub, nên việc cố giấu JavaScript Admin trong bundle không tạo security boundary thực tế. **API authorization mới là boundary quan trọng.**

## Create

```text
web/src/app/runtime-mode.ts
```

Ví dụ:

```ts
export const ADMIN_ORIGIN = 'https://bank.tuannguyenviet.site';
export const PUBLIC_VIEWER_HOST = 'transactions.tuannguyenviet.site';

export function isPublicViewerHost(
  hostname = typeof window !== 'undefined'
    ? window.location.hostname
    : '',
): boolean {
  return hostname === PUBLIC_VIEWER_HOST;
}
```

Không cần runtime config service.

---

# 6. Task 4 — Public host chỉ có viewer routes

Modify:

```text
web/src/app/router.tsx
```

Nếu:

```ts
isPublicViewerHost() === true
```

router chỉ đăng ký:

```text
/
    -> /transactions

/transactions
    -> ViewerLayout
    -> TransactionsPage

/transactions/:id
    -> TransactionDetailPage

*
    -> /transactions
```

Không đăng ký:

```text
/admin
/admin/overview
/admin/connection
/admin/notifications
/admin/activity
/admin/system
```

Admin hostname vẫn dùng router hiện tại y nguyên.

---

# 7. Task 5 — API client tự chuyển namespace

Modify:

```text
web/src/api.ts
```

Hiện tại mọi request dùng:

```ts
/api/v1
```



Đổi thành:

```ts
const basePath = isPublicViewerHost()
  ? '/api/public/v1'
  : '/api/v1';
```

và:

```ts
if (isPublicViewerHost() && isMutation(method)) {
    throw new ApiError(
        'Trang xem giao dịch chỉ hỗ trợ đọc dữ liệu.',
        405,
        'PUBLIC_READ_ONLY',
    );
}
```

Như vậy các hàm đang có:

```ts
fetchTransactions()
fetchTransactionDetail()
fetchPaymentQR()
```

không cần duplicate.

Trên admin:

```text
fetchTransactions()
    ↓
/api/v1/transactions
```

Trên public:

```text
fetchTransactions()
    ↓
/api/public/v1/transactions
```

Đây đơn giản hơn việc tạo nguyên:

```text
public-api.ts
public-queries.ts
public-types.ts
```

---

# 8. Task 6 — Public providers

Global `AppProviders` hiện mount:

```text
RealtimeProvider
VoiceAnnouncementProvider
BankConnectionProvider
RealtimeDomainBridge
```



`BankConnectionProvider` không được chạy trên public host.

Modify:

```text
web/src/app/providers.tsx
```

Logic:

```text
ADMIN
QueryClient
  Realtime /api/v1/events
    Voice TransactionAudioEngine
      BankConnectionProvider
        RealtimeDomainBridge

PUBLIC
QueryClient
  Realtime /api/public/v1/events
    Voice BrowserSpeechEngine
      RealtimeDomainBridge
```

Không cần `PublicRealtimeDomainBridge`.

`RealtimeProvider` đã hỗ trợ truyền custom `url`, nên có thể dùng ngay:

```tsx
<RealtimeProvider url="/api/public/v1/events">
```



`VoiceAnnouncementProvider` cũng đã cho inject `VoiceEngine`. 

Source còn có sẵn:

```text
BrowserSpeechEngine
```



Vậy public viewer vẫn đọc giao dịch bằng giọng nói nhưng:

```text
KHÔNG gọi server TTS
KHÔNG POST /api/v1/voice/*
```

Admin giữ Edge/server TTS như hiện tại.

---

# 9. Task 7 — Làm TransactionsPage thực sự read-only trên public host

Modify:

```text
web/src/pages/viewer/TransactionsPage.tsx
```

Trang hiện có history synchronization và button:

```text
Đồng bộ từ ACB
```



Public mode phải bỏ toàn bộ UI và side effect liên quan:

```text
ensureHistory()
fetchLatestHistorySyncJob()
fetchHistorySyncJob()
cancelHistorySyncJob()
localStorage acb_active_history_sync_job_id
Đồng bộ từ ACB
Hủy đồng bộ
sync progress
```

nhưng **chỉ khi public mode**.

Admin version vẫn giữ như cũ.

Public vẫn có:

```text
Tải lại dữ liệu đã lưu
search
today
7 days
custom date
credit/debit filter
pagination
KPI
transaction detail
QR
realtime
voice
```

Không thay đổi UX chính.

---

# 10. Task 8 — Dọn Viewer Header/Footer cho public

## ViewerHeader

Hiện button Quản trị:

```ts
navigate('/admin')
```



Trên public đổi thành external navigation:

```ts
window.location.assign(
  'https://bank.tuannguyenviet.site/admin'
);
```

Kết quả:

```text
transactions... -> bấm Quản trị
                       ↓
bank...
                       ↓
Cloudflare Access login
```

Đây chính xác là flow mong muốn.

## Viewer footer

Hiện footer public viewer vẫn chứa:

```text
Tổng quan
Kết nối ACB
Webhooks
Polling
Phân phối
Chẩn đoán
Audit
```



Trên public mode bỏ toàn bộ các link này.

Chỉ giữ:

```text
Giao dịch
Realtime status
QR
copyright
```

Admin-host `/transactions` có thể giữ footer cũ.

---

# 11. Task 9 — Traefik route hostname thứ hai

Đây là chỗ cần đặc biệt lưu ý vì source mới đã thay đổi.

**Không chỉ sửa:**

```text
platform/edge/dynamic/acb.yml
```

Deployment hiện runtime-generate file này bằng:

```text
deploy/lib/traefik.sh
    ↓
render_traefik_config()
```



Nếu chỉ sửa YAML checked-in thì lần deploy tiếp theo có thể ghi đè mất.

## Modify

```text
deploy/lib/traefik.sh
platform/edge/dynamic/acb.yml
.env.example
platform/edge/edge_test.go
```

Thêm:

```text
PUBLIC_VIEWER_HOST=transactions.tuannguyenviet.site
```

### Public API router

```yaml
acb-public-api-router:
  rule: "Host(`transactions.tuannguyenviet.site`) && PathPrefix(`/api/public/v1`)"
  priority: 300
  middlewares:
    - tunnel-only
    - security-headers
  service: acb-service
```

### Chặn private/admin surface qua public hostname

```yaml
acb-public-deny-private:
  rule: >
    Host(`transactions.tuannguyenviet.site`) &&
    (
      PathPrefix(`/api/v1`) ||
      PathPrefix(`/internal`) ||
      PathPrefix(`/admin`)
    )
  priority: 1000
  middlewares:
    - deny-internal
  service: acb-service
```

Có thể reuse middleware `deny-internal` hiện tại; không cần middleware mới. Middleware này chỉ allow loopback nên request từ tunnel sẽ nhận 403. 

### Public frontend

```yaml
acb-public-frontend-router:
  rule: "Host(`transactions.tuannguyenviet.site`)"
  priority: 100
  middlewares:
    - tunnel-only
    - security-headers
  service: acb-frontend-service
```

Cả hai hostname dùng cùng:

```text
acb-frontend-service
acb-service
```

nên frontend/gateway blue-green hiện tại tiếp tục hoạt động nguyên trạng.

`deploy-frontend.sh` hiện cũng đã đảm bảo deploy frontend không restart worker/gateway/auth-browser/TTS/Bark ngoài phạm vi. 

---

# 12. Cloudflare

Tạo hostname:

```text
transactions.tuannguyenviet.site
```

và publish nó qua **Cloudflare Tunnel hiện tại**, trỏ về cùng Traefik.

Cloudflare Tunnel hỗ trợ nhiều published applications/hostname trên cùng một tunnel nên không cần tunnel hay `cloudflared` container thứ hai.

## Access

Giữ Access application:

```text
bank.tuannguyenviet.site
```

Không áp Access lên:

```text
transactions.tuannguyenviet.site
```

### Cần kiểm tra

Nếu hiện tại có Access wildcard:

```text
*.tuannguyenviet.site
```

thì nó có thể bắt luôn public viewer.

Cloudflare cho phép hostname/path-specific Access applications và rule cụ thể hơn có precedence riêng.

Nếu account đang bật:

```text
Require Access protection
```

thì hostname không có Access application sẽ bị block.

Trong trường hợp đó mới tạo một Access application cụ thể cho:

```text
transactions.tuannguyenviet.site
```

với:

```text
Bypass
Everyone
```

Cloudflare lưu ý Bypass bỏ Access enforcement và Access request logging, nên chỉ dùng khi account-wide protection bắt buộc; bình thường public hostname không cần Access application là sạch hơn.

---

# 13. Tests bắt buộc

## Backend

Create:

```text
internal/httpapi/public_test.go
```

Test:

```text
GET /api/public/v1/transactions
    -> 200 without CF JWT

response
    -> has amount/description
    -> DOES NOT contain balance
```

```text
GET /api/public/v1/transactions/{id}
    -> no balance
```

```text
GET /api/public/v1/payment-qr
    -> accountNumber
    -> accountName
    -> bankName

    -> no connectionId
    -> no imagePath
    -> no imageHash
```

```text
POST /api/public/v1/transactions
    -> 405
```

Và đặc biệt regression:

```text
GET /api/v1/status without CF JWT
    -> vẫn unauthorized
```

để chắc chắn không vô tình weaken admin auth.

## SSE

Đưa vào journal:

```text
audit.created
connection.changed
bank.transaction.credit
notification.changed
```

Public stream phải chỉ thấy:

```text
bank.transaction.credit
```

và credit payload không có:

```text
balance
accountNumber
sessionToken
```

## Frontend

Add Vitest cho:

```text
isPublicViewerHost()
public API base
mutation blocking
viewer-only router selection
```

Update `transactions-sync.spec.ts` để xác nhận admin host vẫn còn:

```text
Đồng bộ từ ACB
```

Source hiện đã có E2E riêng cho transaction sync và viewer functionality nên không cần xây suite mới từ đầu.  

## Edge

Update:

```text
platform/edge/edge_test.go
```

Repo đã test structure của:

```text
acb-api-router
acb-frontend-router
```

nên chỉ mở rộng chính test này cho public router. 

Thêm một shell regression test nhỏ cho:

```text
render_traefik_config()
```

assert cả:

```text
bank.tuannguyenviet.site
transactions.tuannguyenviet.site
```

và assert public API + frontend cùng trỏ active gateway/frontend slot.

---

# 14. Verification

Trước deploy:

```bash
go test ./...
```

```bash
cd web
bun run test
bun run build
```

```bash
go test ./platform/edge/...
```

và chạy deploy shell regression suite hiện tại.

CI hiện đã chạy khá nhiều deployment/preflight tests, nên chỉ bổ sung test mới vào suite hiện có, không tạo workflow mới. 

---

# 15. Deployment order

## Bước 1

Deploy code trước:

```text
public API
public SSE
frontend public mode
Traefik route support
```

Lúc này chưa có Cloudflare hostname public nên chưa expose ra Internet.

## Bước 2

Verify admin:

```text
https://bank.tuannguyenviet.site
```

phải hoạt động bình thường.

Cloudflare Access vẫn xuất hiện.

Worker không restart ngoài deployment scope bình thường.

## Bước 3

Add Tunnel published hostname:

```text
transactions.tuannguyenviet.site
```

vào tunnel hiện tại.

## Bước 4

Incognito test.

### Public

```text
https://transactions.tuannguyenviet.site
```

phải mở thẳng, không Access login.

### Public API

```text
/api/public/v1/transactions
```

→ `200`

### Private API từ public host

```text
/api/v1/status
```

→ `403/401`, tuyệt đối không data.

### Admin route từ public host

```text
/admin
```

→ `403` hoặc redirect viewer, không render Admin.

### Admin

```text
https://bank.tuannguyenviet.site/admin
```

→ Cloudflare Access.

---

# 16. Acceptance criteria

Hoàn thành khi đủ tất cả:

```text
[ ] transactions.tuannguyenviet.site không cần Cloudflare Access
[ ] bank.tuannguyenviet.site vẫn bắt Cloudflare Access

[ ] Public xem được transaction list
[ ] Public search/filter/pagination hoạt động
[ ] Public xem transaction detail
[ ] Public mở QR được
[ ] Public thấy giao dịch mới realtime qua SSE
[ ] Public voice announcement hoạt động bằng browser speech

[ ] Public không thấy balance
[ ] Public không thấy QR internal metadata

[ ] Public không có nút Đồng bộ từ ACB
[ ] Public không gọi history-sync API
[ ] Public không gọi connection API
[ ] Public không gọi monitor settings
[ ] Public không gọi webhook/notification API
[ ] Public không gọi server-side TTS

[ ] Public SSE chỉ phát bank.transaction.credit

[ ] /api/v1 vẫn yêu cầu Cloudflare JWT
[ ] /admin không tồn tại trên public hostname

[ ] Không tạo container mới
[ ] Không tạo database mới
[ ] Không tạo frontend image thứ hai
[ ] Không tạo gateway thứ hai
[ ] Không thay đổi worker polling
[ ] Không ảnh hưởng ACB session

[ ] Frontend blue/green vẫn hoạt động
[ ] Gateway blue/green vẫn hoạt động
```

---

# 17. Commit breakdown

Không cần chia 8–10 commit như plan cũ.

Chỉ cần khoảng 4 commit:

```text
1. feat(api): add read-only public transaction viewer API

2. feat(web): add anonymous read-only viewer mode

3. feat(edge): route public viewer hostname through existing edge

4. test(security): cover public and admin isolation
```

---

# Kết luận kiến trúc

Bản cần triển khai thực tế chỉ là:

```text
1 hostname mới
+
1 public API namespace
+
1 SSE filter
+
1 runtime frontend mode
+
3 Traefik rules
```

Toàn bộ:

```text
worker
polling
ACB session
SQLite
gateway blue/green
frontend blue/green
deployment engine
Cloudflare Tunnel container
Traefik container
```

**giữ nguyên.**

Đây là mức tách vừa đủ: public surface được cô lập ở HTTP boundary nhưng không nhân đôi infrastructure.