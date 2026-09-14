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

# Public Transaction Viewer / Cloudflare Access Split — Implementation Plan

> **Ngày:** 2026-09-14
> **Repo:** `TheDemonTuan/acb-transaction-webhook`
> **Mục tiêu:** tách giao diện xem giao dịch thành một public viewer không bị Cloudflare Access chặn, trong khi toàn bộ admin tiếp tục được bảo vệ bởi Cloudflare Access.

---

## 1. Kết luận kiến trúc

Không triển khai:

```text
bank.tuannguyenviet.site/admin
    -> Cloudflare Access

bank.tuannguyenviet.site/transactions
    -> Access Bypass
```

Mặc dù Cloudflare hỗ trợ policy theo path, đây không phải boundary tốt cho trường hợp này.

Triển khai:

```text
ADMIN
https://bank.tuannguyenviet.site
        |
        v
Cloudflare Access
        |
        v
Cloudflare Tunnel
        |
        v
Traefik
        |
        v
ACB Gateway
        |
        +-- Admin SPA
        +-- /api/v1/*
              Cloudflare JWT required


PUBLIC VIEWER
https://transactions.tuannguyenviet.site
        |
        v
Cloudflare
(no Access)
        |
        v
same Cloudflare Tunnel
        |
        v
same Traefik
        |
        v
same active ACB Gateway slot
        |
        +-- Viewer SPA only
        +-- /api/public/v1/*
              GET/SSE only
```

Cloudflare mô tả Published Application của Tunnel là endpoint có thể được đưa thẳng ra Internet; Access chỉ cần thêm vào nếu muốn hạn chế người truy cập.

### Quyết định chính

- `bank.tuannguyenviet.site`
  - giữ Cloudflare Access.
  - chứa toàn bộ Admin.
  - chứa API quản trị hiện tại `/api/v1/*`.
  - JWT Access + role Owner/Operator/Viewer tiếp tục giữ nguyên.

- `transactions.tuannguyenviet.site`
  - không có Cloudflare Access.
  - không sử dụng Access Bypass.
  - chỉ chứa Transaction Viewer.
  - chỉ có API read-only riêng `/api/public/v1/*`.
  - không có endpoint cấu hình, login ACB, webhook, polling, notification, audit, CSRF hoặc các mutation khác.

- Không thêm container mới.
- Không thêm database mới.
- Không thêm tunnel mới.
- Không cần CORS vì viewer gọi API cùng origin.
- Không ảnh hưởng worker polling ACB.
- Không ảnh hưởng session ACB.
- Không làm thay đổi blue/green deployment hiện tại.

---

# 2. Vì sao source hiện tại chưa đủ an toàn để chỉ bỏ Access

Frontend hiện đã có bước tách tương đối tốt:

```text
/admin/*
    -> AdminLayout

/transactions
    -> ViewerLayout
```

Router hiện tại xác nhận `/transactions` dùng `ViewerLayout`, trong khi `/admin/*` dùng `AdminLayout`.

Nhưng đây mới chỉ là **route-level separation**, chưa phải **security boundary**.

## 2.1 Admin và Viewer vẫn cùng bundle

`router.tsx` import cả:

```text
AdminLayout
OverviewPage
BankConnectionPage
NotificationChannelsPage
ActivityPage
SystemPage

+

TransactionsPage
TransactionDetailPage
ViewerLayout
```

Do đó nếu chỉ public hostname hiện tại, browser vẫn tải bundle có logic Admin.

Mục tiêu mới phải là:

```text
ADMIN BUILD
  chỉ chứa admin code

VIEWER BUILD
  chỉ chứa transaction/QR/realtime/voice code
```

---

# 3. Một vấn đề quan trọng hơn: AppProviders hiện đang kéo Admin logic vào Viewer

`AppProviders` hiện mount:

```tsx
<RealtimeProvider>
  <VoiceAnnouncementProvider>
    <BankConnectionProvider>
      <RealtimeDomainBridge />
      {children}
    </BankConnectionProvider>
  </VoiceAnnouncementProvider>
</RealtimeProvider>
```



`BankConnectionProvider` khi mount còn tự gọi API kiểm tra phiên đăng nhập ACB và chứa logic:

```text
startAuthSession
cancelAuthSession
checkAuthStatus
sendConnectionAction
```



Điều này tuyệt đối không nên tồn tại trong public viewer bundle.

Public Viewer sẽ có provider tree riêng:

```text
PublicViewerProviders
    |
    +-- QueryClientProvider
    +-- PublicRealtimeProvider
    +-- VoiceAnnouncementProvider
    +-- PublicRealtimeDomainBridge
```

Không có:

```text
BankConnectionProvider
Admin Realtime Domain Bridge
Auth Session state
Webhook state
Notification state
Polling state
Audit state
```

---

# 4. Phạm vi tính năng Public Viewer

Viewer vẫn giữ UX chính giống page hiện nay.

## Được phép

```text
✓ Xem danh sách giao dịch
✓ KPI giao dịch
✓ Tổng tiền vào
✓ Tổng tiền ra
✓ Filter hôm nay
✓ Filter 7 ngày
✓ Khoảng ngày tùy chọn
✓ Filter tiền vào / tiền ra
✓ Search giao dịch
✓ Pagination
✓ Xem chi tiết giao dịch
✓ Tải lại data đã lưu
✓ QR nhận tiền
✓ Xem lịch sử tiền vào trong QR modal
✓ Nhận giao dịch mới realtime
✓ Hiển thị trạng thái realtime
✓ Giọng đọc giao dịch bằng browser
```

Các tính năng viewer hiện tại như filter, KPI, search và QR đã có test Playwright riêng.

## Không được phép

```text
✗ Login ACB
✗ Mở auth-browser
✗ Pause monitoring
✗ Resume monitoring
✗ Force sync ACB
✗ Sync lịch sử ACB
✗ Thay đổi polling
✗ Thay đổi schedule
✗ Upload QR
✗ Generate QR
✗ Delete QR
✗ Webhook management
✗ Notification management
✗ Bark management
✗ Delivery replay
✗ Audit logs
✗ System diagnostic
✗ Server-side TTS mutation
✗ CSRF endpoint
```

---

# 5. Loại bỏ "Đồng bộ từ ACB" khỏi Public Viewer

Đây là thay đổi bắt buộc.

`TransactionsPage` hiện có:

```tsx
ensureHistory({
    from: queryParams.from,
    to: queryParams.to
})
```

và button:

```text
Đồng bộ từ ACB
```



Đây không phải read operation.

Backend hiện cũng đăng ký:

```go
api.Post("/transactions/ensure-history", s.ensureHistory)
```

trong API group chung.

Public page chỉ còn:

```text
[Tải lại]
```

với ý nghĩa:

```text
Browser
   |
   | GET
   v
SQLite/cache hiện có
```

không được:

```text
Browser
   |
   | POST
   v
Worker / ACB API
```

### Backend hardening thêm

Đổi route admin:

```go
api.With(
    s.auth.Require(auth.Owner, auth.Operator),
).Post(
    "/transactions/ensure-history",
    s.ensureHistory,
)
```

Không cho Viewer role kích hoạt fetch lịch sử nữa.

---

# 6. Public API riêng

Tạo namespace:

```text
/api/public/v1
```

## Endpoint allowlist

```http
GET /api/public/v1/transactions
GET /api/public/v1/transactions/{id}

GET /api/public/v1/payment-qr
GET /api/public/v1/payment-qr/image

GET /api/public/v1/events
```

Chỉ từng đó.

Không public:

```text
/api/v1/*
```

## Quy tắc quan trọng

Public router **không được** xây theo kiểu:

```go
/api/public/v1/* -> reuse entire authenticated router
```

mà phải đăng ký từng route một.

Ví dụ:

```go
public.Route("/api/public/v1", func(api chi.Router) {
    api.Get("/transactions", s.publicTransactions)
    api.Get("/transactions/{id}", s.publicTransactionDetail)

    api.Get("/payment-qr", s.publicPaymentQR)
    api.Get("/payment-qr/image", s.publicPaymentQRImage)

    api.Get("/events", s.publicEventsStream)
})
```

Không có `POST`.

Không có `PUT`.

Không có `PATCH`.

Không có `DELETE`.

Không có CSRF vì public API không có mutation.

---

# 7. Không reuse raw Transaction response một cách mù quáng

Tạo:

```go
type PublicTransaction struct {
    ID              string  `json:"id"`
    SemanticKey     string  `json:"semanticKey"`
    TransactionDate string  `json:"transactionDate"`
    TransactionDay  string  `json:"transactionDay"`
    Debit           float64 `json:"debit"`
    Credit          float64 `json:"credit"`
    Description     string  `json:"description"`
    FirstSeenAt     string  `json:"firstSeenAt"`
}
```

Không trả:

```text
balance
internal connection ID
session data
worker state
raw source metadata không cần thiết
```

### Đặc biệt: `balance`

Viewer hiện đang render:

```text
Số dư: ...
```



Public Internet không nên nhận số dư tài khoản.

Vì vậy public version bỏ dòng:

```text
Số dư: 123.456.789 ₫
```

Admin vẫn có thể xem khi cần.

---

# 8. Payment QR cũng cần Public DTO

Storage model hiện chứa:

```go
ID
ConnectionID
AccountNumber
AccountName
Bin
BankName
ImagePath
ImageHash
ImageContentType
Provider
Revision
CreatedAt
UpdatedAt
```



Handler hiện tại trả gần như trực tiếp object `qr` cùng `imageURL`.

Không reuse response này cho Internet.

Tạo:

```go
type PublicPaymentQR struct {
    Configured    bool   `json:"configured"`
    HasImage      bool   `json:"hasImage"`
    AccountNumber string `json:"accountNumber"`
    AccountName   string `json:"accountName"`
    BankName      string `json:"bankName"`
    Bin           string `json:"bin"`
    ImageURL      string `json:"imageURL"`
}
```

Không expose:

```text
ConnectionID
ImagePath
ImageHash
revision nội bộ
createdAt
updatedAt
provider metadata
```

QR nhận tiền vẫn hoạt động bình thường.

---

# 9. Tách realtime stream

Đây là một trong những phần quan trọng nhất.

Realtime client hiện kết nối:

```text
/api/v1/events
```



Nhưng hệ thống realtime hiện định nghĩa:

```text
bank.transaction.credit
connection.changed
auth.changed
webhook.changed
notification.changed
delivery.changed
poll.completed
audit.created
stream_error
```



Public browser không được nhận:

```text
connection.changed
auth.changed
webhook.changed
notification.changed
delivery.changed
poll.completed
audit.created
```

## Public SSE

Tạo:

```text
GET /api/public/v1/events
```

Allowlist duy nhất:

```text
bank.transaction.credit
```

Ngoài ra giữ protocol events:

```text
initial_state
reset_state
stream_error
heartbeat
```

## Reuse journal nhưng filter trước khi emit

Không cần một event system khác.

Luồng:

```text
worker
   |
   v
journal
   |
   +------------------------+
   |                        |
   v                        v
Admin SSE               Public SSE
all events              transaction credit only
```

Existing SSE đã sanitize riêng `bank.transaction.credit` bằng cách xóa:

```text
balance
accountNumber
sessionToken
```

trước khi gửi.

Giữ behavior này và thêm event allowlist cho public stream.

---

# 10. Chống leak event khi reconnect SSE

Public SSE vẫn dùng journal sequence ID:

```text
ep1:12345
```

Khi gặp event không được public:

```text
ep1:12346 auth.changed
ep1:12347 audit.created
```

server:

```text
không emit event
nhưng vẫn advance watermark
```

Sau đó:

```text
ep1:12348 bank.transaction.credit
```

mới emit.

Như vậy reconnect không scan lại vô hạn những admin event đã bỏ qua.

---

# 11. Giới hạn SSE abuse

Public SSE là long-lived connection nên cần protection riêng.

Thêm:

```text
GLOBAL_PUBLIC_SSE_LIMIT = 200
PUBLIC_SSE_PER_IP_LIMIT = 4
```

Khi quá giới hạn:

```http
HTTP 429 Too Many Requests
Retry-After: 15
```

State:

```go
type publicSSELimiter struct {
    mu       sync.Mutex
    total    int
    perIP    map[string]int
}
```

`defer` luôn release slot khi:

```text
client disconnect
server error
context cancellation
```

Phải chạy test với `go test -race`.

---

# 12. Public frontend bundle riêng

Hiện Vite chỉ build một bundle vào:

```text
internal/httpui/dist
```



Docker cũng chỉ copy một frontend build vào Go image.

Thay thành:

```text
internal/httpui/dist
    -> Admin bundle

internal/httpviewer/dist
    -> Public Viewer bundle
```

## Files mới

```text
web/
├── index.html
├── viewer/
│   └── index.html
│
├── src/
│   ├── app/
│   │   ├── App.tsx
│   │   ├── router.tsx
│   │   └── providers.tsx
│   │
│   └── viewer-app/
│       ├── main.tsx
│       ├── App.tsx
│       ├── router.tsx
│       ├── providers.tsx
│       └── api/
│           └── public-queries.ts
│
├── vite.config.ts
└── vite.viewer.config.ts
```

Backend:

```text
internal/
├── httpui/
│   └── embed.go
│
└── httpviewer/
    └── embed.go
```

`internal/httpui` chỉ chứa admin.

`internal/httpviewer` chỉ chứa viewer.

---

# 13. Admin router không import Viewer nữa

`web/src/app/router.tsx` sau refactor:

```text
/
    -> redirect /admin

/admin
/admin/overview
/admin/connection
/admin/notifications
/admin/activity
/admin/system
```

Không còn import:

```text
ViewerLayout
TransactionsPage
TransactionDetailPage
```

Điều này cho phép Vite tree-shake toàn bộ viewer khỏi admin bundle và ngược lại.

---

# 14. Public Viewer router

Viewer bundle:

```text
/
    -> TransactionsPage

/transactions
    -> TransactionsPage

/transactions/:id
    -> TransactionDetailPage

*
    -> /
```

Không định nghĩa bất kỳ `/admin/*` route nào.

---

# 15. Public providers

Tạo:

```tsx
<PublicViewerProviders>
  <RouterProvider router={viewerRouter} />
</PublicViewerProviders>
```

Bên trong:

```tsx
<QueryClientProvider>
    <RealtimeProvider url="/api/public/v1/events">
        <VoiceAnnouncementProvider engine={browserSpeechEngine}>
            <PublicRealtimeDomainBridge />
            {children}
        </VoiceAnnouncementProvider>
    </RealtimeProvider>
</QueryClientProvider>
```

Không mount:

```text
BankConnectionProvider
Admin RealtimeDomainBridge
```

---

# 16. Public RealtimeDomainBridge

Current bridge invalidate:

```text
status
adminOverview
pollRuns
connection
notificationChannels
webhooks
deliveries
auditLogs
```



Public bridge chỉ làm:

```text
bank.transaction.credit
        |
        +-> prepend vào transaction React Query cache
        |
        +-> update KPI incoming
        |
        +-> VoiceAnnouncementProvider
```

Không biết gì về:

```text
auth
polling
connection
webhooks
audit
notifications
```

---

# 17. Giọng đọc trên Public Viewer

Hiện online voice engine gọi các POST API như:

```text
POST /voice/test
POST /voice/transactions/{id}
POST /voice/transactions/{id}/replay
POST /voice/transactions/summary
```



Không public các endpoint này.

Nếu public anonymous, attacker có thể dùng chúng để tiêu tốn TTS resource/API.

Public viewer sử dụng:

```text
BrowserSpeechEngine
```

tức:

```text
SSE transaction
   |
   v
browser
   |
   v
Web Speech API / local browser voice
```

Admin vẫn được phép sử dụng server-side TTS hiện tại.

---

# 18. Viewer Header

Hiện Viewer Header có:

```text
Realtime Status
Mã QR nhận tiền
Voice Toggle
Quản trị
```



Giữ UX đó.

Nhưng nút:

```text
Quản trị
```

không còn:

```tsx
navigate('/admin')
```

mà thành external link:

```text
https://bank.tuannguyenviet.site/admin
```

Khi click:

```text
Public Viewer
     |
     v
Admin hostname
     |
     v
Cloudflare Access login
```

Đúng mục đích.

---

# 19. Viewer Footer

Hiện footer Public Viewer còn link tới:

```text
Tổng quan
Kết nối ACB
Webhooks
Polling
Phân phối
Chẩn đoán
Audit
```



Loại bỏ toàn bộ.

Public footer chỉ còn:

```text
ACB Transaction Monitor
Realtime transaction updates

[Quản trị]
```

Hoặc thậm chí chỉ copyright.

Không quảng bá cấu trúc admin endpoint ra public UI.

---

# 20. API client riêng

Không cho Public Viewer import:

```text
web/src/shared/api/queries.ts
```

vì file đó chứa cả:

```text
configureConnection()
startAuthSession()
cancelAuthSession()
updateMonitorSettings()
uploadPaymentQR()
generatePaymentQR()
deletePaymentQR()
createWebhookEndpoint()
notification mutations
...
```



Tạo:

```text
web/src/viewer-app/api/public-api.ts
web/src/viewer-app/api/public-queries.ts
```

Chỉ export:

```ts
fetchPublicTransactions()
fetchPublicTransaction()
fetchPublicPaymentQR()
```

Base:

```text
/api/public/v1
```

Không CSRF.

Không credential requirement.

---

# 21. Backend Host Isolation

Đây là lớp defense-in-depth.

Thêm config:

```text
ADMIN_ORIGIN=https://bank.tuannguyenviet.site
PUBLIC_VIEWER_ORIGIN=https://transactions.tuannguyenviet.site
```

Giữ compatibility:

```text
PUBLIC_ORIGIN
```

tạm alias cho `ADMIN_ORIGIN` trong migration đầu tiên.

Gateway phân luồng dựa trên Host:

```text
Host: bank.tuannguyenviet.site
    -> adminRouter

Host: transactions.tuannguyenviet.site
    -> publicRouter
```

Pseudo structure:

```go
root:
    /healthz
    /readyz
    /internal/deployz

    host == ADMIN_HOST:
        adminHandler

    host == PUBLIC_VIEWER_HOST:
        publicHandler

    otherwise:
        404
```

Điều này cực kỳ quan trọng.

Ngay cả khi Traefik cấu hình nhầm:

```http
GET https://transactions.../api/v1/connection
```

backend vẫn không route request đó vào admin API.

---

# 22. Public router phải chặn `/api/*` trước SPA fallback

Không để:

```text
/api/v1/connection
```

bị SPA fallback thành `index.html`.

Public router:

```text
/api/public/v1/*
    -> public API

/api/*
    -> 404

/internal/*
    -> 404

/admin/*
    -> 404

/*
    -> viewer SPA
```

---

# 23. Cloudflare Tunnel

Không tạo tunnel mới.

Repo hiện đã theo mô hình shared `cloudflared` + Traefik, và edge config chỉ expose traffic qua Tunnel.

Cloudflare cũng hỗ trợ nhiều DNS records cùng trỏ tới một tunnel target.

Thêm Published Application:

```text
Hostname:
transactions.tuannguyenviet.site

Tunnel:
tunnel hiện tại

Service:
cùng Traefik service hiện tại
```

---

# 24. Cloudflare Access

## Admin

Giữ:

```text
bank.tuannguyenviet.site/*
        |
        v
Cloudflare Access
```

Không thay đổi Access JWT validation hiện tại.

Production backend hiện verify:

```text
Cf-Access-Jwt-Assertion
CF_Authorization
Authorization Bearer
```

sau đó verify issuer/audience/JWKS và role.

Giữ nguyên.

## Public

Không tạo Access application cho:

```text
transactions.tuannguyenviet.site
```

### Kiểm tra đặc biệt

Nếu hiện tại Access Application đang là:

```text
*.tuannguyenviet.site
```

phải thu hẹp nó.

Mục tiêu:

```text
Access protected:
bank.tuannguyenviet.site

Not Access protected:
transactions.tuannguyenviet.site
```

Cloudflare hỗ trợ Access application theo hostname/subdomain/path.

Không giải quyết bằng Bypass `Everyone` trừ trường hợp emergency migration ngắn hạn.

---

# 25. Traefik

Current router chỉ nhận:

```yaml
Host(`bank.tuannguyenviet.site`)
```



Thêm public host.

Ví dụ:

```yaml
http:
  routers:
    acb-admin-router:
      rule: "Host(`bank.tuannguyenviet.site`) && !PathPrefix(`/internal`)"
      entryPoints:
        - web
      middlewares:
        - tunnel-only
        - security-headers
      service: acb-service

    acb-viewer-router:
      rule: "Host(`transactions.tuannguyenviet.site`) && !PathPrefix(`/internal`)"
      entryPoints:
        - web
      middlewares:
        - tunnel-only
        - security-headers
        - public-rate-limit
      service: acb-service
```

Cả hai dùng:

```text
acb-service
```

Do đó:

```text
blue active
   -> admin + viewer đều blue

switch green
   -> admin + viewer đều green
```

Không làm phức tạp zero-downtime deployment.

---

# 26. Rate limiting Public API

Thêm Traefik middleware riêng:

```yaml
public-rate-limit:
  rateLimit:
    average: 120
    period: 1m
    burst: 60
```

Source IP sử dụng Cloudflare forwarded client IP trong trusted Tunnel flow.

Không áp middleware này cho Admin vì Admin đã có Access.

Ngoài edge limit, backend vẫn clamp:

```text
limit <= 100
search length <= 128
page cursor validation
date validation
```

Không tin query từ client.

---

# 27. Search-engine protection

Viewer public về network không có nghĩa muốn Google index.

Public responses thêm:

```http
X-Robots-Tag: noindex, nofollow, noarchive
```

Viewer HTML:

```html
<meta
  name="robots"
  content="noindex,nofollow,noarchive"
/>
```

`robots.txt`:

```text
User-agent: *
Disallow: /
```

Đây không phải security mechanism, nhưng giảm accidental discovery.

---

# 28. Cache policy

Transaction API:

```http
Cache-Control: no-store
```

SSE:

```http
Cache-Control: no-cache, no-transform
```

QR metadata:

```http
Cache-Control: no-store
```

QR image có revision URL nên có thể:

```text
/api/public/v1/payment-qr/image?v=<revision>
```

và cache ngắn hoặc immutable theo revision.

Không cache transaction JSON ở Cloudflare.

---

# 29. Security headers

Tiếp tục sử dụng current server-side CSP/security headers.

Repo đã có Content Security Policy dựa trên `'self'`, phù hợp với việc public viewer API/SSE cùng origin.

Viewer nên dùng:

```text
default-src 'self'
connect-src 'self'
object-src 'none'
base-uri 'none'
frame-ancestors 'none'
form-action 'none'
```

Không có third-party JS nếu không thật sự cần.

---

# 30. Không dùng CORS

Không thiết kế:

```text
transactions.tuannguyenviet.site
        |
        | CORS
        v
bank.tuannguyenviet.site/api
```

Thay vào đó:

```text
transactions.tuannguyenviet.site
        |
        v
transactions.tuannguyenviet.site/api/public/v1
```

Cùng origin.

Lợi ích:

```text
không CORS
không credential cross-origin
không CF_Authorization cookie problem
không expose admin origin
CSP đơn giản hơn
```

---

# 31. Docker build

Current Dockerfile chỉ build frontend một lần rồi copy:

```text
internal/httpui/dist
```



Đổi web build thành:

```text
bun run typecheck
bun run build:admin
bun run build:viewer
```

`package.json`:

```json
{
  "scripts": {
    "typecheck": "tsc --noEmit",
    "build:admin": "vite build",
    "build:viewer": "vite build --config vite.viewer.config.ts",
    "build": "bun run typecheck && bun run build:admin && bun run build:viewer"
  }
}
```

Go builder copy:

```text
internal/httpui/dist
internal/httpviewer/dist
```

Không làm tăng số container.

---

# 32. Bundle isolation test

Sau build phải có automated guard để chống regression.

Viewer dist không được chứa những chuỗi như:

```text
/api/v1/connection/auth
/api/v1/monitor/settings
/api/v1/webhooks
/api/v1/notification-channels
Kết nối ACB
Chẩn đoán hệ thống
Audit
```

Có thể viết:

```text
scripts/check-viewer-bundle.sh
```

fail CI nếu admin capability vô tình được import vào viewer.

Đây là defense rất đáng có vì trong tương lai dev có thể vô tình import lại `shared/api/queries.ts`.

---

# 33. Backend tests

Tạo:

```text
internal/httpapi/public_api_test.go
internal/httpapi/public_sse_test.go
internal/httpapi/host_routing_test.go
```

## Test 1 — public works without Access JWT

```text
Host: transactions.tuannguyenviet.site

GET /api/public/v1/transactions

Expected:
200
```

không có:

```text
Cf-Access-Jwt-Assertion
CF_Authorization
```

---

## Test 2 — admin API inaccessible through public hostname

```text
Host: transactions.tuannguyenviet.site

GET /api/v1/connection
```

Expected:

```text
404
```

Không phải `200`.

---

## Test 3 — public write impossible

Test:

```text
POST   /api/public/v1/transactions
POST   /api/public/v1/payment-qr
PUT    /api/public/v1/payment-qr
DELETE /api/public/v1/payment-qr
```

Expected:

```text
404 / 405
```

---

## Test 4 — QR response sanitization

Assert JSON không chứa:

```text
connectionId
imagePath
imageHash
provider
createdAt
updatedAt
```

---

## Test 5 — transaction sanitization

Assert không chứa:

```text
balance
```

---

## Test 6 — SSE allowlist

Insert journal events:

```text
auth.changed
audit.created
connection.changed
bank.transaction.credit
```

Public stream chỉ được nhận:

```text
bank.transaction.credit
```

---

## Test 7 — admin Access vẫn bắt buộc

Production-style request:

```text
Host: bank.tuannguyenviet.site
GET /api/v1/transactions
```

không JWT:

```text
401
```

JWT hợp lệ:

```text
200
```

---

## Test 8 — host boundary

```text
Host: attacker.example.com
```

Expected:

```text
404
```

Không fallback admin SPA.

---

# 34. Frontend tests

Tạo:

```text
web/tests/public-api.spec.ts
web/tests/public-router.spec.tsx

web/e2e/public-viewer.spec.ts
web/e2e/admin-public-boundary.spec.ts
```

Public e2e phải verify:

```text
✓ transactions load
✓ filter works
✓ search works
✓ detail works
✓ QR opens
✓ realtime event arrives
✓ voice toggle works client-side
✓ admin button points to bank.tuannguyenviet.site
```

Và:

```text
✗ không có "Đồng bộ từ ACB"
✗ không có "Kết nối ACB"
✗ không có "Webhooks"
✗ không có "Polling"
✗ không có "Audit"
✗ không request /api/v1/*
```

---

# 35. Edge tests

Update:

```text
platform/edge/edge_test.go
```

Assert:

```text
bank.tuannguyenviet.site
    -> acb-service

transactions.tuannguyenviet.site
    -> acb-service
```

cả hai đều bắt buộc:

```text
tunnel-only
security-headers
```

Public thêm:

```text
public-rate-limit
```

`/internal` vẫn bị deny.

---

# 36. CI verification

Trước merge:

```bash
go test ./...
go test -race ./internal/httpapi/...

cd web
bun install --frozen-lockfile
bun run test
bun run build
bun run e2e

docker build --target gateway .
```

Sau build:

```text
verify admin dist exists
verify viewer dist exists
run viewer bundle isolation check
```

---

# 37. Deployment order — không downtime

## Phase 1 — deploy application capability trước

Deploy version mới có:

```text
adminHost router
publicHost router
public API
viewer bundle
```

nhưng chưa public DNS.

Blue/green deployment vẫn chạy như hiện tại.

Verify candidate slot:

```text
Host bank...
    -> admin OK

Host transactions...
    -> viewer OK
```

---

## Phase 2 — Traefik

Thêm:

```text
transactions.tuannguyenviet.site
```

vào shared edge configuration.

Không đổi active slot logic.

---

## Phase 3 — Cloudflare Tunnel

Thêm Published Application:

```text
transactions.tuannguyenviet.site
        ->
existing tunnel
        ->
existing Traefik
```

Cloudflare hỗ trợ nhiều hostname cùng tunnel.

---

## Phase 4 — Cloudflare Access

Xác minh:

```text
bank.tuannguyenviet.site
    Access ON

transactions.tuannguyenviet.site
    Access OFF
```

Không tạo Bypass policy cho viewer.

---

# 38. Production smoke test

Test bằng Incognito / browser chưa login Cloudflare.

## Public Viewer

```text
https://transactions.tuannguyenviet.site
```

Expected:

```text
không Cloudflare login
viewer load ngay
transactions load
QR load
SSE connected
```

Network tab phải chỉ có:

```text
/api/public/v1/*
```

Không được xuất hiện:

```text
/api/v1/*
```

---

## Admin

Mở:

```text
https://bank.tuannguyenviet.site/admin
```

Expected:

```text
Cloudflare Access login
```

Sau auth mới vào admin.

---

## Direct attack checks

```text
https://transactions.../admin
https://transactions.../api/v1/status
https://transactions.../api/v1/connection
https://transactions.../api/v1/webhooks
https://transactions.../internal/deployz
```

Expected:

```text
404/403
```

Không redirect sang admin.

Không render admin SPA.

---

# 39. Rollback

Không có DB migration bắt buộc nên rollback đơn giản.

Nếu viewer có vấn đề:

```text
1. Remove/disable Cloudflare Published Application của transactions host
2. Remove public Traefik router
3. Admin bank.tuannguyenviet.site tiếp tục hoạt động
4. Worker ACB không restart
5. Session ACB không bị ảnh hưởng
```

Đây là ưu điểm lớn của kiến trúc này.

---

# 40. Files dự kiến thay đổi

## Backend

```text
internal/config/config.go
internal/httpapi/server.go
internal/httpapi/public_api.go
internal/httpapi/public_sse.go
internal/httpapi/public_types.go
internal/httpapi/public_api_test.go
internal/httpapi/public_sse_test.go
internal/httpapi/host_routing_test.go

internal/httpviewer/embed.go
internal/httpviewer/dist/*
```

## Frontend

```text
web/package.json
web/vite.config.ts
web/vite.viewer.config.ts
web/viewer/index.html

web/src/app/router.tsx
web/src/layouts/admin/AdminTopbar.tsx
web/src/layouts/admin/AdminMobileNav.tsx

web/src/viewer-app/main.tsx
web/src/viewer-app/App.tsx
web/src/viewer-app/router.tsx
web/src/viewer-app/providers.tsx
web/src/viewer-app/api/public-api.ts
web/src/viewer-app/api/public-queries.ts

web/src/pages/viewer/TransactionsPage.tsx
web/src/pages/viewer/TransactionDetailPage.tsx
web/src/layouts/viewer/ViewerLayout.tsx
web/src/layouts/viewer/ViewerHeader.tsx
web/src/features/payment-qr/ReceivingQRModal.tsx

web/src/realtime/realtime.client.ts
web/src/realtime/PublicRealtimeDomainBridge.tsx
```

## Docker/deployment

```text
Dockerfile
deploy/compose.prod.yaml

platform/edge/dynamic/acb.yml
platform/edge/dynamic/middlewares.yml
platform/edge/edge_test.go
```

## Tests

```text
web/tests/public-api.spec.ts
web/tests/public-router.spec.tsx
web/e2e/public-viewer.spec.ts
web/e2e/admin-public-boundary.spec.ts

scripts/check-viewer-bundle.sh
```

---

# 41. Task/commit breakdown

## Task 1 — backend host boundary

Implement:

```text
ADMIN_ORIGIN
PUBLIC_VIEWER_ORIGIN
adminRouter
publicRouter
unknown host reject
```

Test first.

Commit:

```text
feat(security): split admin and public viewer host boundaries
```

---

## Task 2 — public read-only API

Implement:

```text
transactions
transaction detail
QR metadata
QR image
public DTOs
```

Test all response fields.

Commit:

```text
feat(viewer): add read-only public transaction API
```

---

## Task 3 — SSE isolation

Implement:

```text
public event allowlist
credit-only stream
SSE connection limits
```

Run race tests.

Commit:

```text
feat(realtime): add isolated public transaction event stream
```

---

## Task 4 — frontend bundle separation

Implement:

```text
admin Vite build
viewer Vite build
httpviewer embed
```

Commit:

```text
refactor(web): split admin and viewer frontend bundles
```

---

## Task 5 — Public Viewer providers/API

Implement:

```text
PublicViewerProviders
public queries
public realtime client
PublicRealtimeDomainBridge
browser-only voice
```

Commit:

```text
feat(viewer): isolate public viewer runtime dependencies
```

---

## Task 6 — remove viewer admin capabilities

Remove:

```text
ensureHistory
admin footer links
same-origin /admin navigation
server TTS access
balance rendering
```

Commit:

```text
fix(viewer): enforce read-only public capabilities
```

---

## Task 7 — edge/Tunnel-ready routing

Update:

```text
Traefik routers
rate limits
host tests
compose env
```

Commit:

```text
feat(edge): route public transaction viewer separately
```

---

## Task 8 — complete tests

Add:

```text
backend isolation tests
frontend isolation tests
Playwright tests
bundle leakage test
```

Commit:

```text
test(security): verify admin viewer isolation
```

---

# 42. Acceptance criteria

Implementation chỉ được coi là hoàn tất khi thỏa cả 12 điều:

1. `transactions.tuannguyenviet.site` mở được trong Incognito không cần Cloudflare Access.
2. `bank.tuannguyenviet.site` vẫn bắt buộc Cloudflare Access.
3. Public Viewer không chứa Admin routes.
4. Public bundle không chứa Admin API clients.
5. Public browser không gửi request nào đến `/api/v1/*`.
6. Public API không có write endpoint.
7. Public API không trả `balance`.
8. QR API không leak `imagePath`, `connectionId`, `imageHash`.
9. Public SSE chỉ trả transaction event.
10. `/admin`, `/api/v1` và `/internal` không reachable qua public hostname.
11. Blue/green switching vẫn chuyển cả Admin + Viewer sang cùng active slot.
12. Worker/polling/session ACB không phải restart chỉ vì public viewer traffic.

---

# 43. Kiến trúc cuối cùng

```text
                         Internet
                            |
                    Cloudflare Edge
                            |
             +--------------+---------------+
             |                              |
             |                              |
 bank.tuannguyenviet.site       transactions.tuannguyenviet.site
             |                              |
       Cloudflare Access                 NO ACCESS
             |                              |
             +--------------+---------------+
                            |
                    Cloudflare Tunnel
                            |
                         Traefik
                            |
                       acb-service
                            |
                  active Blue / Green
                            |
                       Go Gateway
                  +---------+---------+
                  |                   |
             Admin Host          Viewer Host
                  |                   |
             Admin SPA            Viewer SPA
                  |                   |
             /api/v1/*        /api/public/v1/*
                  |                   |
             CF JWT/RBAC          GET/SSE only
                  |                   |
                  +---------+---------+
                            |
                         SQLite
                            |
                          Worker
                            |
                           ACB
```

## Kết quả

Ta vẫn chỉ vận hành:

```text
1 shared Cloudflare Tunnel
1 shared Traefik edge
1 active Gateway slot
1 Worker
1 SQLite database
```

nhưng từ góc nhìn security:

```text
Admin != Public Viewer
```

thật sự ở cả:

```text
hostname
Cloudflare policy
Traefik routing
backend route tree
API namespace
frontend bundle
React providers
realtime events
HTTP methods
data contract
```

chứ không chỉ khác URL.
