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

# PLAN V2 — REDESIGN & REFACTOR TOÀN BỘ REACT CLIENT + REALTIME VOICE ANNOUNCEMENT
## ACB Transaction Webhook

> Repository: `TheDemonTuan/acb-transaction-webhook`  
> Phạm vi chính: `web/` và các contract realtime liên quan giữa backend → SSE → React client  
> Baseline đã đối chiếu lại: `main` tại commit `b4b195e27fa09d7b0039e0a72728223c4a63aa15`  
> Ngày baseline: 12/09/2026  
> Đây là bản plan mới thay thế plan frontend trước đó vì code hiện tại đã bổ sung kiến trúc realtime SSE.

---

# 0. Executive Summary

Bản refactor mới phải giải quyết đồng thời 4 mục tiêu:

1. **Refactor React client từ monolith sang kiến trúc theo domain/feature**
2. **Redesign UI hoàn toàn thành 2 trải nghiệm**
   - Admin Console
   - Transaction Viewer
3. **Giữ SSE hiện tại làm realtime transport chính**
4. **Thêm tính năng đọc giao dịch mới bằng giọng nói tiếng Việt**

Điểm quan trọng nhất sau khi đọc lại source mới:

- Backend hiện đã có event-driven pipeline.
- Frontend hiện đã kết nối:
  ```ts
  new EventSource('/api/v1/events')
  ```
- SSE hiện phát:
  ```text
  bank.transaction.credit
  connection.changed
  auth.changed
  webhook.changed
  delivery.changed
  poll.completed
  audit.created
  stream_error
  ```
- Backend chỉ emit `bank.transaction.credit` khi:
  - transaction vừa được insert lần đầu;
  - không phải baseline;
  - `credit > 0`.

Do đó:

> **Không cần so sánh toàn bộ danh sách giao dịch để biết có giao dịch mới.**

Trigger chuẩn cho voice là:

```text
bank.transaction.credit
```

Và voice phải được xây như một **subscriber độc lập của realtime domain event**, không gắn chặt vào component bảng giao dịch.

---

# 1. Snapshot source hiện tại đã kiểm tra

## 1.1. Latest commit

Baseline:

```text
b4b195e27fa09d7b0039e0a72728223c4a63aa15
fix: publish poll.completed events to SSE and refresh transactions in realtime
```

Ngay trước đó có:

```text
53c2e6b15ff262d78cab890000472048584044c2
feat: implement realtime event-driven transaction pipeline
```

Điều này thay đổi cách nên thiết kế frontend so với plan cũ.

---

## 1.2. `web/src` hiện tại

Hiện vẫn chủ yếu gồm:

```text
web/src/
├── App.tsx
├── api.ts
├── index.css
├── main.tsx
├── realtime-types.ts
├── useRealtime.ts
└── vite-env.d.ts
```

`App.tsx` hiện đã tăng lên khoảng:

```text
62 KB
```

Tức là frontend vẫn còn vấn đề monolithic rất rõ.

---

# 2. Những điểm source hiện tại đang làm tốt

Không nên rewrite bỏ những thứ vừa được làm đúng.

## 2.1. SSE thay polling UI liên tục

Đây là hướng đúng.

Client:

```ts
const source = new EventSource('/api/v1/events');
```

Server:

```text
Content-Type: text/event-stream
Cache-Control: no-cache, no-transform
X-Accel-Buffering: no
```

Có heartbeat:

```text
15s
```

Đây phù hợp cho:

- transaction event;
- connection state;
- webhook status;
- delivery update;
- poll result;
- audit.

---

## 2.2. SSE có journal/replay

Server hỗ trợ:

```text
Last-Event-ID
```

và event ID dạng:

```text
ep1:<sequence>
```

Có:

```text
initial_state
reset_state
```

và xử lý:

```text
invalid_cursor
retention_expired
storage_error
```

Đây là nền tốt để giữ UI realtime mà không cần gọi API mỗi 5 giây.

---

## 2.3. Event transaction có tính idempotent tốt

Trong batch ingest:

```text
semanticKey = ACB:<transaction number>
```

Transaction cũ:

```text
SkippedCount++
```

Transaction tiền vào mới:

```text
bank.transaction.credit
```

Mỗi event có:

```text
transactionId
transactionNumber
credit
debit
currency
transactionDate
description
detectedAt
```

Đây là payload đủ để:

- thêm row mới vào UI;
- phát notification;
- đọc voice.

---

## 2.4. Baseline không phát voice/event

Backend hiện:

```go
if !isBaseline && item.Credit > 0 {
    // emit bank.transaction.credit
}
```

Đây cực kỳ quan trọng.

Khi user vừa connect ACB lần đầu và hệ thống import lịch sử:

```text
KHÔNG được đọc hàng loạt các giao dịch lịch sử.
```

Backend hiện đã giúp đảm bảo điều này.

---

## 2.5. SSE đã lọc dữ liệu nhạy cảm

Khi gửi `bank.transaction.credit` ra client, server loại:

```text
balance
accountNumber
sessionToken
```

Voice feature nên tiếp tục dựa trên safe event DTO này.

---

# 3. Các vấn đề còn lại trong frontend hiện tại

## 3.1. `App.tsx` vẫn là God Component

Hiện App đang làm cùng lúc:

- navigation;
- page routing;
- API request;
- server state;
- client state;
- realtime handler;
- auth lifecycle;
- CSRF retry;
- transaction filtering;
- pagination;
- webhook actions;
- poll history;
- delivery history;
- audit history;
- diagnostics;
- UI copy;
- status mapping;
- layout;
- cards;
- form;
- modal/session UI.

Đây là điểm cần refactor lớn nhất.

---

# 4. Realtime hiện tại vẫn coupling quá nhiều với App

Đoạn realtime hiện nay có dạng:

```ts
const realtime = useRealtime({
  onEvent: (type, raw) => {
    if (type === 'bank.transaction.credit') {
      ...
      setTransactions(...)
      void load()
    } else if (type === 'poll.completed') {
      ...
      api('/transactions?limit=50')
      ...
      void load()
    } else {
      void load()
    }
  }
})
```

Vấn đề:

```text
SSE event
    ↓
App.tsx
    ↓
manual setState
    ↓
load()
    ↓
nhiều API request
```

SSE đang được dùng như:

```text
"có gì đổi thì refresh lại gần như cả app"
```

thay vì:

```text
"event nào đổi domain nào thì update/invalidate domain đó"
```

---

# 5. `void load()` sau realtime event cần loại bỏ

Hiện:

```text
bank.transaction.credit
    ↓
insert transaction local
    ↓
load()
```

và:

```text
poll.completed
    ↓
fetch transactions
    ↓
load()
```

Điều này:

- request dư;
- khó predict;
- domain coupling;
- gây race;
- khó test;
- làm SSE mất lợi ích.

Kiến trúc mới:

```text
SSE Event
   ↓
Typed Event Bus
   ↓
Domain Realtime Bridge
   ├── patch query cache
   ├── invalidate đúng query
   ├── update health state
   └── Voice Announcement subscriber
```

---

# 6. Vấn đề UI/UX hiện tại vẫn còn

Source mới vẫn hiện trực tiếp các thuật ngữ:

```text
AUTH_REQUIRED
MONITORING
UNREACHABLE
UNKNOWN
DISABLED
dead-letter
Webhooks
Polling
Audit
Pause
Resume
Sync
Generation
```

Ví dụ UI hiện có:

```text
ACB đang hoạt động (MONITORING)
```

và:

```text
Đã tạo endpoint ở trạng thái DISABLED.
```

Đây chính xác là thứ cần loại khỏi product UI.

---

# 7. Product Architecture mới

Frontend sẽ được xem như 2 sản phẩm dùng chung core.

```text
                    React Application
                           │
                   Realtime Provider
                           │
                ┌──────────┴──────────┐
                │                     │
          Admin Console       Transaction Viewer
```

---

# 8. Product A — Admin Console

URL:

```text
/admin
/admin/connection
/admin/notifications
/admin/activity
/admin/system
```

Đối tượng:

- chủ hệ thống;
- người vận hành;
- kỹ thuật/admin.

Mục tiêu:

```text
Hệ thống có đang chạy không?
ACB còn kết nối không?
Giao dịch có cập nhật không?
Kênh thông báo có gửi được không?
Có lỗi gì cần xử lý không?
```

---

# 9. Product B — Transaction Viewer

URL:

```text
/transactions
/transactions/:transactionId
```

Đối tượng:

- người theo dõi giao dịch;
- nhân viên cửa hàng;
- kế toán;
- màn hình POS;
- tablet;
- điện thoại.

Mục tiêu:

```text
Có tiền vào mới không?
Bao nhiêu tiền?
Lúc nào?
Nội dung gì?
Tổng tiền vào hôm nay?
Tìm giao dịch nào đó?
```

Transaction Viewer cũng là nơi chính để bật:

```text
Đọc giao dịch mới
```

---

# 10. Navigation Admin mới

Chỉ giữ 5 mục.

```text
Tổng quan
Kết nối ngân hàng
Kênh thông báo
Hoạt động
Hệ thống
```

Không giữ 8 menu:

```text
Tổng quan
Kết nối ACB
Giao dịch
Webhooks
Phân phối
Polling
Chẩn đoán
Audit
```

Transaction Viewer là một surface riêng.

---

# 11. Mapping menu cũ → mới

| Cũ | Mới |
|---|---|
| Tổng quan | Tổng quan |
| Kết nối ACB | Kết nối ngân hàng |
| Giao dịch | Transaction Viewer riêng |
| Webhooks | Kênh thông báo |
| Phân phối | Hoạt động → Gửi thông báo |
| Polling | Hoạt động → Cập nhật giao dịch |
| Audit | Hoạt động → Thay đổi hệ thống |
| Chẩn đoán | Hệ thống → Chẩn đoán nâng cao |

---

# 12. Frontend Architecture đề xuất

```text
web/src/
├── app/
│   ├── App.tsx
│   ├── router.tsx
│   ├── providers.tsx
│   ├── routes.ts
│   └── query-client.ts
│
├── layouts/
│   ├── admin/
│   │   ├── AdminLayout.tsx
│   │   ├── AdminSidebar.tsx
│   │   ├── AdminTopbar.tsx
│   │   └── AdminMobileNav.tsx
│   │
│   └── viewer/
│       ├── ViewerLayout.tsx
│       ├── ViewerHeader.tsx
│       └── ViewerRealtimeStatus.tsx
│
├── pages/
│   ├── admin/
│   │   ├── OverviewPage.tsx
│   │   ├── BankConnectionPage.tsx
│   │   ├── NotificationChannelsPage.tsx
│   │   ├── ActivityPage.tsx
│   │   └── SystemPage.tsx
│   │
│   └── viewer/
│       ├── TransactionsPage.tsx
│       └── TransactionDetailPage.tsx
│
├── realtime/
│   ├── RealtimeProvider.tsx
│   ├── realtime.client.ts
│   ├── realtime.types.ts
│   ├── realtime.events.ts
│   ├── realtime.reducer.ts
│   └── useRealtimeStatus.ts
│
├── features/
│   ├── transactions/
│   ├── bank-connection/
│   ├── notification-channels/
│   ├── activity/
│   ├── system-health/
│   └── voice-announcements/
│
├── entities/
│   ├── transaction/
│   ├── bank-connection/
│   ├── notification-channel/
│   ├── delivery/
│   └── poll-run/
│
├── shared/
│   ├── api/
│   ├── ui/
│   ├── hooks/
│   ├── formatters/
│   ├── storage/
│   └── lib/
│
├── content/
│   ├── status-copy.ts
│   ├── error-copy.ts
│   └── labels.ts
│
├── styles/
│   ├── tokens.css
│   ├── globals.css
│   └── utilities.css
│
└── main.tsx
```

---

# 13. Dependency direction

```text
app
 ↓
pages/layouts
 ↓
features
 ↓
entities
 ↓
shared
```

Realtime là infrastructure ngang:

```text
realtime
 ↓
typed domain events
 ↓
feature subscribers
```

Không để:

```text
realtime → App.tsx → mọi thứ
```

---

# 14. `App.tsx` target

Sau refactor:

```tsx
export default function App() {
  return (
    <AppProviders>
      <AppRouter />
    </AppProviders>
  );
}
```

Target:

```text
< 50–80 lines
```

---

# 15. Router

Thêm:

```text
react-router-dom
```

Không còn:

```ts
const [active, setActive] = useState('Tổng quan');
```

Route tree:

```text
/
├── /transactions
│   └── /transactions/:id
└── /admin
    ├── /
    ├── /connection
    ├── /notifications
    ├── /activity
    └── /system
```

---

# 16. Server State

Khuyến nghị:

```text
@tanstack/react-query
```

Không dùng React Query để thay SSE.

Dùng:

```text
HTTP = snapshot/query/mutation
SSE  = realtime invalidation + realtime inserts
```

Hai thứ bổ sung cho nhau.

---

# 17. Query keys

```ts
['admin-overview']

['bank-connection']

['transactions', filters]
['transaction', id]

['notification-channels']

['activity', 'sync', filters]
['activity', 'delivery', filters]
['activity', 'audit', filters]

['system-health']
```

---

# 18. Realtime Layer mới

Thay:

```ts
useRealtime({ onEvent(type, raw) {} })
```

bằng event envelope typed.

```ts
type RealtimeEvent<T = unknown> = {
  id: string | null;
  type: RealtimeEventType;
  data: T;
  receivedAt: number;
};
```

Quan trọng:

```text
id phải lấy từ MessageEvent.lastEventId
```

Hiện `useRealtime.ts` đang bỏ mất metadata này.

Voice dedupe cần nó.

---

# 19. Typed event map

```ts
type RealtimeEventMap = {
  'bank.transaction.credit': BankTransactionCreditEvent;
  'connection.changed': ConnectionChangedEvent;
  'auth.changed': AuthChangedEvent;
  'webhook.changed': WebhookChangedEvent;
  'delivery.changed': DeliveryChangedEvent;
  'poll.completed': PollCompletedEvent;
  'audit.created': AuditCreatedEvent;
  'stream_error': StreamErrorEvent;
};
```

Không dùng:

```ts
Record<string, unknown>
```

ở mỗi page.

---

# 20. Bank transaction SSE DTO

Theo backend hiện tại:

```ts
type BankTransactionCreditEvent = {
  bank: 'ACB';
  accountMasked?: string;
  transactionId: string;
  transactionNumber: string;
  credit: string;
  debit: string;
  currency: 'VND';
  transactionDate: string;
  description: string;
  detectedAt: string;
};
```

Lưu ý:

```text
credit/debit trong event hiện là string.
```

Không nên lập tức convert thành `Number` ở transport layer.

---

# 21. Money type

Đối với dữ liệu tài chính, ưu tiên:

```ts
type VndAmount = string;
```

hoặc boundary:

```ts
BigInt(dto.credit)
```

Không để presentation tự gọi:

```ts
Number(data.credit)
```

rải rác.

Tạo:

```text
entities/money/
```

với:

```ts
parseVnd()
formatVnd()
speakVnd()
```

---

# 22. Realtime Bridge

Ví dụ:

```ts
function onTransactionCredit(event) {
  queryClient.setQueryData(
    ['transactions', activeFilter],
    old => prependIfRelevant(old, event.data)
  );

  queryClient.invalidateQueries({
    queryKey: ['admin-overview']
  });
}
```

Không gọi:

```text
loadEverything()
```

---

# 23. Poll completed bridge

`poll.completed` dùng để cập nhật:

```text
Activity → Cập nhật giao dịch
System health
Admin overview last sync
```

Chỉ invalidate transactions nếu:

```text
insertedCount > 0
```

Không refresh toàn app.

---

# 24. Realtime status UX

Không hiện:

```text
CONNECTED
RECONNECTING
DISCONNECTED
```

Mapping:

| Technical | UI |
|---|---|
| CONNECTED | Đang cập nhật trực tiếp |
| CONNECTING | Đang kết nối |
| RECONNECTING | Đang kết nối lại |
| DISCONNECTED | Mất kết nối trực tiếp |

Viewer header:

```text
● Đang cập nhật trực tiếp
```

Nếu reconnect:

```text
○ Đang kết nối lại...
```

Không làm banner đỏ ngay vì reconnect là trạng thái có thể tự phục hồi.

---

# 25. TÍNH NĂNG MỚI — Đọc giao dịch bằng giọng nói

Đây là feature mới quan trọng.

Tên feature cho user:

```text
Đọc giao dịch mới
```

Không dùng:

```text
TTS
Speech Synthesis
Voice Event
Audio Engine
```

trong UI thông thường.

---

# 26. Mục tiêu voice

Khi backend phát:

```text
bank.transaction.credit
```

ví dụ:

```json
{
  "transactionId": "txn_...",
  "credit": "500000",
  "currency": "VND",
  "description": "NGUYEN VAN A CHUYEN TIEN",
  "detectedAt": "..."
}
```

client phát:

```text
"Bạn vừa nhận được năm trăm nghìn đồng."
```

Có thể optional:

```text
"Bạn vừa nhận được năm trăm nghìn đồng. Nội dung: Nguyễn Văn A chuyển tiền."
```

---

# 27. Không nên nói “đã chuyển khoản”

Đối với event:

```text
bank.transaction.credit
```

đây là **tiền vào**.

Copy mặc định nên là:

```text
Bạn vừa nhận được 500.000 đồng.
```

hoặc voice text:

```text
Bạn vừa nhận được năm trăm nghìn đồng.
```

Không dùng:

```text
Đã chuyển khoản 500.000 đồng
```

vì dễ hiểu là user vừa chuyển tiền đi.

---

# 28. Trigger voice chuẩn

```text
ACB upstream
   ↓
poll
   ↓
transaction INSERT mới
   ↓
event journal
   ↓
bank.transaction.credit
   ↓
SSE
   ↓
RealtimeProvider
   ├── Transaction cache
   ├── Admin metrics
   └── VoiceAnnouncementService
          ↓
       speech queue
          ↓
      Browser TTS
```

Voice **không** trigger từ:

```text
transaction list changed
```

Voice **không** trigger từ:

```text
poll.completed insertedCount > 0
```

Voice **chỉ** trigger từ:

```text
bank.transaction.credit
```

---

# 29. Vì sao không so sánh list trước/sau?

Sai:

```text
fetch 50 transactions
↓
compare previous array
↓
thấy row mới
↓
speak
```

Vấn đề:

- pagination;
- filter;
- race;
- reconnect;
- refresh;
- duplicate;
- thay sort;
- tab inactive;
- baseline.

Backend đã có authoritative domain event.

Do đó source of truth:

```text
bank.transaction.credit
```

---

# 30. Voice module structure

```text
features/voice-announcements/
├── VoiceAnnouncementProvider.tsx
├── useVoiceAnnouncements.ts
├── voice-engine.ts
├── browser-speech-engine.ts
├── voice-queue.ts
├── voice-settings.ts
├── voice-storage.ts
├── voice-dedupe.ts
├── voice-copy.ts
├── money-to-vietnamese.ts
├── transaction-to-announcement.ts
├── multi-tab-leader.ts
└── components/
    ├── VoiceToggle.tsx
    ├── VoiceSettingsSheet.tsx
    ├── VoiceTestButton.tsx
    └── VoiceStatus.tsx
```

---

# 31. Voice Engine Interface

Không gọi trực tiếp:

```ts
window.speechSynthesis
```

trong transaction component.

Tạo abstraction:

```ts
export interface VoiceEngine {
  isSupported(): boolean;
  getVoices(): Promise<VoiceInfo[]>;
  speak(message: VoiceMessage): Promise<void>;
  cancel(): void;
}
```

Implementation:

```text
BrowserSpeechEngine
```

Lợi ích:

- test được;
- sau này thay TTS engine;
- không coupling browser API vào feature domain.

---

# 32. Web Speech API — lựa chọn V1

V1 nên dùng:

```text
window.speechSynthesis
SpeechSynthesisUtterance
```

Lý do:

- không cần backend TTS;
- không cần tạo MP3;
- không thêm request latency;
- không tốn token/API;
- voice có sẵn theo thiết bị/browser;
- đủ tốt cho transaction announcement.

Các setting browser API hỗ trợ:

```text
lang
voice
rate
pitch
volume
```

---

# 33. Vietnamese voice selection

Logic:

```text
1. voice lang === vi-VN
2. voice lang startsWith vi
3. device default voice
```

Pseudo:

```ts
const vietnamese =
  voices.find(v => v.lang.toLowerCase() === 'vi-vn')
  ?? voices.find(v => v.lang.toLowerCase().startsWith('vi'))
  ?? voices.find(v => v.default)
  ?? voices[0];
```

Set:

```ts
utterance.lang = 'vi-VN';
```

---

# 34. `voiceschanged`

Một số browser load danh sách voice bất đồng bộ.

Do đó:

```text
getVoices()
+
voiceschanged
```

Phải xử lý cả hai.

Không assume:

```ts
speechSynthesis.getVoices()
```

luôn có kết quả ngay lúc app mount.

---

# 35. Voice settings

Settings mặc định đề xuất:

```ts
type VoiceSettings = {
  enabled: boolean;
  voiceURI?: string;
  volume: number;
  rate: number;
  pitch: number;
  includeDescription: boolean;
  announceWhenHidden: boolean;
  burstMode: 'individual' | 'summary';
};
```

Default:

```text
enabled = false
volume = 1
rate = 1
pitch = 1
includeDescription = false
announceWhenHidden = true
burstMode = summary
```

---

# 36. Vì sao default voice nên OFF

Âm thanh phát tự động có thể:

- gây bất ngờ;
- lộ thông tin giao dịch;
- gây ồn nơi công cộng.

Do đó user phải:

```text
Bật đọc giao dịch mới
```

một lần.

Sau đó save preference trên thiết bị.

---

# 37. UI bật voice

Viewer header:

```text
🔊 Đọc giao dịch mới
```

Khi off:

```text
🔇 Đọc giao dịch mới: Tắt
```

Click mở sheet:

```text
Đọc giao dịch mới

[ ON ]

Giọng đọc
Tiếng Việt — <device voice>

Âm lượng
────────●─

Tốc độ
────●────

[ ] Đọc cả nội dung chuyển khoản

[ Nghe thử ]
```

---

# 38. Voice unlock

Khi user bật lần đầu:

```text
user gesture
   ↓
initialize speech engine
   ↓
load voices
   ↓
optional test
```

Nên có button:

```text
Nghe thử
```

Phrase:

```text
Đã bật đọc giao dịch mới.
```

Điều này vừa kiểm tra:

- speaker;
- volume;
- voice;
- browser permission/behavior.

---

# 39. Browser limitation phải hiểu đúng

Voice browser chỉ đáng tin cậy khi:

```text
web page đang mở
```

Browser/mobile OS có thể:

- suspend tab;
- pause JS;
- hạn chế audio khi browser background sâu;
- dừng hoàn toàn khi browser bị kill.

Do đó V1 không được quảng cáo là:

```text
"luôn đọc kể cả khi app đóng"
```

Nếu sau này cần đó:

```text
PWA/native app
hoặc
local always-on agent
```

là phase khác.

---

# 40. Voice privacy

Default chỉ đọc:

```text
amount
```

Không đọc mặc định:

```text
số tài khoản
mã transaction
toàn bộ description
balance
```

Default phrase:

```text
Bạn vừa nhận được một triệu đồng.
```

Optional:

```text
Đọc nội dung chuyển khoản
```

---

# 41. Tạo câu voice

Tạo pure function:

```ts
buildTransactionAnnouncement(event, settings)
```

Ví dụ:

```ts
buildTransactionAnnouncement({
  credit: '500000',
  description: 'NGUYEN VAN A CHUYEN TIEN'
})
```

→

```text
Bạn vừa nhận được năm trăm nghìn đồng.
```

Nếu include description:

```text
Bạn vừa nhận được năm trăm nghìn đồng. Nội dung: Nguyễn Văn A chuyển tiền.
```

---

# 42. Không đọc trực tiếp `500000`

Nên có:

```text
money-to-vietnamese.ts
```

Ví dụ:

```text
500000
→ năm trăm nghìn đồng

1250000
→ một triệu hai trăm năm mươi nghìn đồng

10000000
→ mười triệu đồng

10500000
→ mười triệu năm trăm nghìn đồng
```

TTS nghe tự nhiên hơn.

---

# 43. Money speech formatter

API:

```ts
speakVnd('1250000')
```

→

```text
một triệu hai trăm năm mươi nghìn đồng
```

Unit test đầy đủ:

```text
0
1
10
15
21
105
1.000
1.005
10.000
100.000
1.000.000
1.250.000
10.500.000
1.000.000.000
```

---

# 44. Description sanitation trước khi đọc

Nếu bật đọc description:

- normalize whitespace;
- bỏ URL dài;
- giới hạn length;
- không đọc mã kỹ thuật dài;
- không đọc transaction number;
- strip control chars.

Ví dụ:

```ts
sanitizeSpeechDescription(text)
```

Max:

```text
~100–140 ký tự
```

Nếu description quá dài:

```text
chỉ đọc amount
```

---

# 45. Voice Queue

Không gọi:

```ts
speechSynthesis.speak()
```

rải rác.

Tạo queue:

```text
VoiceQueue
```

State:

```text
idle
speaking
paused
```

Flow:

```text
enqueue
 ↓
if idle → speak next
 ↓
onend
 ↓
speak next
```

---

# 46. Không để voice overlap

Hai transaction tới nhanh:

```text
TX A
TX B
```

Không phát hai utterance cùng lúc.

Queue:

```text
A → xong → B
```

---

# 47. Burst handling

Case:

```text
poll ACB trả 8 transaction mới cùng lúc
```

Nếu đọc từng cái:

```text
8 câu liên tục
```

rất khó chịu.

Khuyến nghị:

```text
<= 3 giao dịch / 2 giây
→ đọc từng giao dịch

> 3 giao dịch / 2 giây
→ đọc summary
```

Ví dụ:

```text
Bạn vừa nhận được 6 giao dịch mới, tổng cộng ba triệu hai trăm nghìn đồng.
```

User có thể chọn:

```text
Đọc từng giao dịch
Đọc tổng khi có nhiều giao dịch
```

---

# 48. DEDUPE — phần bắt buộc

Đây là phần quan trọng nhất của voice feature.

Nếu không có dedupe:

```text
SSE reconnect
→ journal replay
→ giao dịch cũ đọc lại
```

Không chấp nhận.

---

# 49. Dedupe key priority

Ưu tiên:

```text
1. SSE event id
2. transactionId
3. semantic key / transactionNumber
```

Event ID hiện dạng:

```text
ep1:12345
```

---

# 50. `useRealtime` phải truyền `lastEventId`

Hiện handler:

```ts
onEvent(type, data)
```

Target:

```ts
onEvent({
  id: event.lastEventId || null,
  type,
  data,
  receivedAt: Date.now()
})
```

Voice service nhận event metadata đầy đủ.

---

# 51. Dedupe storage

Tạo:

```text
sessionStorage
```

key:

```text
acb.voice.processed-events.v1
```

Lưu:

```text
last ~200–500 event IDs
```

Có TTL.

Không cần lưu transaction data.

---

# 52. Vì sao sessionStorage phù hợp

- không chứa secret;
- chống duplicate trong session/reconnect;
- tab isolation tốt;
- page reload fresh SSE không replay historical snapshot;
- data tự mất khi session kết thúc.

Cross-tab coordination xử lý riêng.

---

# 53. Fresh page không đọc transaction cũ

Server hiện khi kết nối không có cursor:

```text
initial_state
```

với watermark hiện tại.

Client:

```text
load snapshot
```

Voice rule:

```text
initial snapshot = NEVER SPEAK
```

Dù bảng có 50 transaction:

```text
không đọc.
```

Chỉ event sau watermark mới có quyền trigger voice.

---

# 54. `reset_state` không được đọc voice

Nếu:

```text
reset_state
```

client:

```text
refetch snapshot
```

Voice:

```text
suppress
```

Không được:

```text
compare list rồi đọc các row chưa thấy.
```

---

# 55. Reconnect replay policy

SSE replay là hữu ích vì transaction bị lỡ lúc network ngắt có thể được nhận lại.

Nhưng không nên đọc giao dịch quá cũ.

Đề xuất:

```text
VOICE_REPLAY_MAX_AGE = 120s
```

Nếu:

```text
now - detectedAt <= 120s
```

→ có thể đọc.

Nếu lớn hơn:

```text
UI vẫn update
voice suppress
```

Optional sau này:

```text
Có 4 giao dịch mới trong lúc mất kết nối.
```

---

# 56. Multi-tab duplicate

Nếu user mở:

```text
Tab 1: /transactions
Tab 2: /admin
```

cả hai cùng SSE.

Nếu cả hai cùng voice:

```text
âm thanh bị đọc 2 lần.
```

Phải có multi-tab leader.

---

# 57. Voice Leader Election

Khuyến nghị:

```text
Web Locks API
```

nếu support:

```text
acb-transaction-voice-leader
```

Fallback:

```text
BroadcastChannel
```

Logic:

```text
1 tab làm announcer leader
các tab khác nghe event nhưng không phát voice
```

---

# 58. Leader preference

Ưu tiên:

```text
visible tab
```

Nếu leader đóng:

```text
tab khác takeover
```

Không để silence vĩnh viễn.

---

# 59. Voice provider mount ở đâu?

Không mount trong:

```text
TransactionsPage
```

Nên mount trong:

```text
AppProviders
```

Lý do:

Nếu user đang xem:

```text
/admin
```

mà đã bật voice:

```text
vẫn đọc giao dịch mới
```

miễn app còn mở.

---

# 60. Voice feature flow hoàn chỉnh

```text
bank.transaction.credit
        │
        ▼
RealtimeProvider
        │
        ▼
parse + validate typed DTO
        │
        ├──────────────► Transaction cache update
        │
        └──────────────► VoiceAnnouncementProvider
                              │
                              ▼
                         enabled?
                              │
                              ▼
                         tab leader?
                              │
                              ▼
                         duplicate?
                              │
                              ▼
                         fresh enough?
                              │
                              ▼
                        burst aggregator
                              │
                              ▼
                      build Vietnamese text
                              │
                              ▼
                         speech queue
                              │
                              ▼
                    SpeechSynthesisUtterance
```

---

# 61. Voice status UI

Không hiện technical state.

User-friendly:

```text
Đọc giao dịch: Đang bật
```

Nếu no voice support:

```text
Thiết bị này chưa hỗ trợ đọc bằng giọng nói.
```

Nếu no Vietnamese voice:

```text
Không tìm thấy giọng tiếng Việt trên thiết bị. Hệ thống sẽ dùng giọng mặc định.
```

---

# 62. Voice error handling

Không show raw:

```text
SpeechSynthesisErrorEvent.error
```

User:

```text
Không thể phát giọng đọc trên thiết bị này.
```

Advanced debug:

```text
technical error
voice URI
browser info
```

---

# 63. Voice preferences storage

Có thể dùng:

```text
localStorage
```

vì đây là preference không nhạy cảm.

Key:

```text
acb.voice.settings.v1
```

Không lưu:

```text
transaction description
amount history
account data
```

---

# 64. Voice V1 không cần backend TTS

Không cần endpoint:

```text
POST /tts
```

Không cần:

```text
MP3
ElevenLabs
Google TTS
OpenAI TTS
```

ở V1.

Mục tiêu:

```text
nhẹ + realtime + không thêm latency
```

---

# 65. Nếu muốn voice chất lượng cao sau này

Interface đã có:

```text
VoiceEngine
```

nên phase 2 có thể thêm:

```text
RemoteTtsEngine
```

Nhưng phải cân nhắc:

- latency;
- privacy;
- cost;
- network;
- cache;
- API key.

Không đưa vào core refactor đầu tiên.

---

# 66. Transaction Viewer redesign

Đây là trang user-facing chính.

Desktop:

```text
┌──────────────────────────────────────────────────────────────┐
│ Giao dịch                         ● Đang cập nhật trực tiếp │
│                              🔊 Đọc giao dịch: Bật           │
├──────────────────────────────────────────────────────────────┤
│ +12.500.000 ₫     28 giao dịch      -1.250.000 ₫            │
│ Tiền vào hôm nay  Hôm nay           Tiền ra hôm nay         │
├──────────────────────────────────────────────────────────────┤
│ 🔎 Tìm giao dịch...    Hôm nay ▼    Tiền vào ▼              │
├──────────────────────────────────────────────────────────────┤
│ 14:05   Tiền vào    +500.000 ₫    NGUYEN VAN A...          │
│ 13:48   Tiền vào  +1.000.000 ₫    CHUYEN TIEN...           │
└──────────────────────────────────────────────────────────────┘
```

---

# 67. Viewer mobile

```text
Giao dịch
● Trực tiếp            🔊

+12.500.000 ₫
Tiền vào hôm nay

[ Tìm giao dịch... ]

[Hôm nay] [Tiền vào] [Bộ lọc]

+500.000 ₫
Tiền vào
NGUYEN VAN A CHUYEN TIEN
14:05

+1.000.000 ₫
Tiền vào
...
```

Không render desktop table thu nhỏ.

---

# 68. New transaction animation

Khi SSE event tới:

- prepend row;
- highlight nhẹ 1–2 giây;
- badge:
  ```text
  Mới
  ```
- không flash toàn table;
- không scroll user lên đầu nếu họ đang đọc lịch sử.

Nếu user đang top list:

```text
insert trực tiếp
```

Nếu user scroll xa:

```text
"2 giao dịch mới"
```

button:

```text
[Xem]
```

---

# 69. Transaction Viewer summary

KPI:

```text
Tiền vào hôm nay
Tiền ra hôm nay
Số giao dịch
```

Không dùng realtime local list 50 row để tính tổng.

Cần server summary hoặc endpoint aggregate.

---

# 70. Server-side filters

Hiện filter vẫn chủ yếu client-side trên `transactions` đã load.

Đây không chuẩn khi pagination.

Backend nên hỗ trợ:

```text
GET /transactions
  ?from=
  &to=
  &direction=
  &q=
  &limit=
  &cursor=
```

Response:

```json
{
  "items": [],
  "nextCursor": "...",
  "summary": {
    "count": 28,
    "incoming": "12500000",
    "outgoing": "1250000"
  }
}
```

---

# 71. Viewer filter URL state

```text
/transactions?range=today&type=incoming&q=nguyen
```

Không giữ filter chỉ trong `useState`.

Lợi ích:

- reload;
- back;
- share;
- bookmark.

---

# 72. Transaction detail

```text
Giao dịch

+500.000 ₫
Tiền vào

14:05 · 12/09/2026

Nội dung
NGUYEN VAN A CHUYEN TIEN

Mã giao dịch
...

[ Sao chép thông tin ]
```

Technical:

```text
semantic key
firstSeenAt
parser version
```

chỉ admin/advanced.

---

# 73. Admin Overview redesign

Primary status:

```text
● Hệ thống đang hoạt động bình thường

ACB đang kết nối
Cập nhật gần nhất: 14:05
Giao dịch gần nhất: 14:05

[Xem giao dịch]
```

Nếu auth hết:

```text
⚠ Cần đăng nhập lại ACB

Giao dịch mới sẽ chưa được cập nhật cho tới khi kết nối lại.

[Đăng nhập lại]
```

---

# 74. Overview KPIs

Chỉ giữ:

```text
Tiền vào hôm nay
Giao dịch hôm nay
Kênh thông báo đang hoạt động
Vấn đề cần xử lý
```

Không KPI hóa:

```text
SQLite WAL
service version
generation
```

---

# 75. Bank Connection page

Workflow:

```text
Chưa kết nối
    ↓
Bắt đầu kết nối
    ↓
Đăng nhập ACB
    ↓
Xác thực
    ↓
Đang cập nhật giao dịch
```

Không render raw state.

---

# 76. Kết nối đang tốt

```text
✓ ACB đang được kết nối

Tài khoản: •••• 1234
Cập nhật gần nhất: 14:05

[Cập nhật ngay] [Kết nối lại]
```

Không:

```text
MONITORING
Generation 12
Resume
Pause
Sync
```

---

# 77. Advanced controls

Nếu thực sự cần:

```text
Pause
Resume
Generation
```

đưa vào:

```text
Cài đặt nâng cao
```

và đổi wording:

```text
Tạm dừng cập nhật
Tiếp tục cập nhật
```

`Generation` không nên user-facing.

---

# 78. Kênh thông báo

Đổi:

```text
Webhook
Endpoint
```

thành:

```text
Kênh thông báo
```

Card:

```text
Messenger Bot
● Đang hoạt động

Lần gửi gần nhất: 14:05
Tình trạng: Bình thường

[Gửi thử] [Cài đặt]
```

---

# 79. Activity page

Tabs:

```text
Cập nhật giao dịch
Gửi thông báo
Thay đổi hệ thống
```

Mapping:

```text
Polling  → Cập nhật giao dịch
Delivery → Gửi thông báo
Audit    → Thay đổi hệ thống
```

---

# 80. Status Copy Layer

Bắt buộc:

```text
content/status-copy.ts
```

Ví dụ:

```ts
const bankStatusCopy = {
  MONITORING: {
    label: 'Đang cập nhật giao dịch',
    tone: 'success'
  },

  AUTH_REQUIRED: {
    label: 'Cần đăng nhập lại ACB',
    tone: 'warning'
  },

  UNCONFIGURED: {
    label: 'Chưa kết nối ngân hàng',
    tone: 'neutral'
  },

  PAUSED: {
    label: 'Đang tạm dừng',
    tone: 'neutral'
  }
};
```

---

# 81. Error Copy Layer

Không:

```ts
setNotice({ text: error.message })
```

Target:

```text
backend error code
    ↓
error translator
    ↓
friendly UI copy
```

Ví dụ:

```text
AUTH_SESSION_SUPERSEDED
→ Phiên đăng nhập đã được thay thế.

SESSION_EXPIRED
→ Phiên đăng nhập đã hết hạn.

ACB_RATE_LIMITED
→ ACB đang giới hạn tần suất truy cập. Hệ thống sẽ tự thử lại.
```

---

# 82. Technical code chỉ xuất hiện ở advanced diagnostics

Admin:

```text
Xem chi tiết kỹ thuật
```

Có thể thấy:

```text
AUTH_REQUIRED
SESSION_EXPIRED
HTTP 429
classifier
generation
event id
journal seq
```

User viewer:

```text
never.
```

---

# 83. Design Direction

Khác hoàn toàn bản hiện tại.

Default:

```text
light theme
```

Dark optional.

Style:

- clean financial dashboard;
- neutral background;
- white surface;
- subtle border;
- ít shadow;
- typography rõ;
- amount nổi bật;
- state dùng dot + label;
- khoảng thở nhiều.

Không:

- developer dashboard;
- neon;
- quá nhiều card;
- inline dark-blue boxes;
- status enum khổng lồ.

---

# 84. Design tokens

```css
:root {
  --bg-app: ...;
  --bg-surface: ...;
  --bg-subtle: ...;

  --text-primary: ...;
  --text-secondary: ...;
  --text-muted: ...;

  --border-default: ...;

  --status-success: ...;
  --status-warning: ...;
  --status-danger: ...;
  --status-info: ...;

  --radius-sm: 8px;
  --radius-md: 12px;
  --radius-lg: 16px;
}
```

Không hardcode màu trong JSX.

---

# 85. CSS refactor

Hiện JSX vẫn có nhiều:

```tsx
style={{
  padding: ...,
  background: ...,
  border: ...,
  color: ...
}}
```

Target:

```text
Tailwind 4
+
CSS semantic tokens
+
component variants
```

---

# 86. Shared UI

```text
Button
IconButton
Badge
StatusDot
Card
Stat
Input
Select
Tabs
Table
MobileList
Drawer
Dialog
Toast
Tooltip
Skeleton
EmptyState
ErrorState
PageHeader
SectionHeader
Switch
Slider
```

Voice dùng:

```text
Switch
Slider
Select
```

cùng design system.

---

# 87. Toast hierarchy

Action result:

```text
Đã bật đọc giao dịch mới.
Đã cập nhật kết nối.
Kênh thông báo đã được bật.
```

Persistent state:

```text
Cần đăng nhập lại ACB
```

phải là:

```text
banner/card
```

không phải toast.

---

# 88. CSRF handling

Hiện CSRF retry logic đã được cải thiện.

Refactor nó về:

```text
shared/api/client.ts
```

Không để mutation feature phải biết:

```text
CSRF_CODE_TOKEN_INVALID
invalidateCsrfToken
getCsrfToken(true)
```

API client tự xử lý một lần retry.

---

# 89. API client target

```ts
apiClient.get()
apiClient.post()
apiClient.delete()
```

Responsibilities:

- JSON;
- CSRF;
- timeout;
- AbortSignal;
- normalized error;
- request ID;
- retry policy.

---

# 90. Mutation invalidation

Không:

```text
mutation
↓
load everything
```

Ví dụ:

## Sync now

```text
invalidate:
bank-connection
admin-overview
```

Transaction sẽ được update bằng:

```text
bank.transaction.credit
```

khi thật sự có row mới.

---

# 91. Realtime + Query contract

```text
HTTP query = current truth snapshot
SSE event = incremental change signal
```

Không coi SSE local state là database thay thế.

Nếu app nghi ngờ mất consistency:

```text
invalidate/refetch
```

---

# 92. SSE reset handling

```text
reset_state
```

→

```text
invalidate all relevant queries
clear realtime transient state
do not clear user preferences
do not voice snapshot
```

---

# 93. SSE stream error UX

`stream_error`:

Không show raw:

```text
storage_error
```

Viewer:

```text
Đang tạm mất cập nhật trực tiếp. Hệ thống sẽ thử kết nối lại.
```

Admin advanced:

```text
storage_error
```

---

# 94. SSE status health

Topbar indicator:

```text
● Trực tiếp
```

Click tooltip:

```text
Giao dịch mới sẽ tự xuất hiện mà không cần tải lại trang.
```

This is user-friendly explanation of realtime.

---

# 95. Testing — Voice

Không test actual speaker trong CI.

Inject:

```text
FakeVoiceEngine
```

Test:

```text
event → expected phrase
event duplicate → no speak
old replay → no speak
voice disabled → no speak
burst → summary
description disabled → amount only
description enabled → sanitized
multi-tab follower → no speak
```

---

# 96. Unit tests cần thêm

```text
money-to-vietnamese.test.ts
voice-copy.test.ts
voice-dedupe.test.ts
voice-queue.test.ts
voice-settings.test.ts
realtime-parser.test.ts
status-copy.test.ts
error-copy.test.ts
transaction-mapper.test.ts
```

---

# 97. Realtime tests

Test:

```text
initial_state
bank.transaction.credit
poll.completed
reset_state
reconnect
duplicate SSE id
invalid JSON
unknown event
```

`useRealtime` hiện catch parse error rỗng.

Target:

- log safe diagnostics;
- malformed event không crash app;
- malformed event không trigger voice.

---

# 98. E2E Voice tests

Playwright không cần nghe audio.

Mock:

```text
window.speechSynthesis
```

Scenario:

1. user mở `/transactions`;
2. bật voice;
3. SSE transaction mới;
4. assert `speak()` được gọi một lần;
5. cùng event ID phát lại;
6. assert vẫn một lần;
7. SSE khác;
8. assert lần 2.

---

# 99. E2E fresh load

Scenario:

```text
API snapshot có 20 transactions
initial_state
```

Expected:

```text
speech count = 0
```

Sau đó:

```text
bank.transaction.credit
```

Expected:

```text
speech count = 1
```

Đây là test quan trọng nhất.

---

# 100. E2E reconnect

```text
connected
↓
disconnect
↓
reconnect
↓
journal replay same event
```

Expected:

```text
không đọc duplicate
```

Nếu missed event mới:

```text
đọc nếu còn trong freshness window
```

---

# 101. E2E burst

Emit 5 credits gần nhau.

Mode:

```text
summary
```

Expected:

```text
1 utterance
```

Ví dụ:

```text
Bạn vừa nhận được 5 giao dịch mới, tổng cộng hai triệu đồng.
```

---

# 102. Existing test coverage gap

Hiện `web/tests` rất nhỏ, chủ yếu có:

```text
status.test.ts
```

E2E hiện có:

```text
auth-resilience.spec.ts
foundation.spec.ts
```

Refactor lớn phải tăng coverage trước khi xóa legacy App.

---

# 103. Migration plan

Không rewrite tất cả trong một commit.

---

# 104. Phase 0 — Lock baseline

- record current latest commit;
- build;
- unit tests;
- E2E;
- screenshot current pages;
- record current API contracts;
- record SSE event contracts.

---

# 105. Phase 1 — Realtime transport refactor

Trước UI redesign.

Tạo:

```text
RealtimeProvider
RealtimeEventEnvelope
typed event parser
event metadata
lastEventId propagation
```

Không đổi behavior lớn.

---

# 106. Phase 2 — API + Query foundation

Add:

```text
react-router-dom
@tanstack/react-query
```

Tạo:

```text
ApiClient
QueryClient
AppProviders
```

Move:

```text
CSRF
error normalization
formatters
```

---

# 107. Phase 3 — Copy layer

Trước redesign.

Tạo:

```text
status-copy
error-copy
labels
```

Sau phase này:

```text
raw enum không được render trong normal UI
```

---

# 108. Phase 4 — Voice engine core

Tạo:

```text
VoiceEngine
BrowserSpeechEngine
VoiceQueue
MoneySpeechFormatter
Dedupe
Settings
```

Chưa cần UI đẹp.

Test bằng fake engine.

---

# 109. Phase 5 — Voice realtime integration

Subscribe:

```text
bank.transaction.credit
```

Implement:

- event ID dedupe;
- transaction ID dedupe;
- freshness;
- burst;
- multi-tab leader.

Không gắn vào list diff.

---

# 110. Phase 6 — Transaction Viewer mới

Tạo:

```text
/transactions
```

Features:

- summary;
- realtime list;
- filter;
- search;
- detail;
- voice control;
- responsive;
- mobile.

---

# 111. Phase 7 — Admin shell mới

Tạo:

```text
/admin
```

- sidebar;
- topbar;
- overview;
- realtime status.

---

# 112. Phase 8 — Bank connection refactor

Extract:

- connection query;
- auth session;
- auth lifecycle;
- start/cancel;
- friendly states.

Giữ E2E auth resilience.

---

# 113. Phase 9 — Notification channels

Rename conceptual UI:

```text
Webhook Endpoint
→ Kênh thông báo
```

---

# 114. Phase 10 — Activity consolidation

Gộp:

```text
poll runs
deliveries
audit
```

1 page.

---

# 115. Phase 11 — Diagnostics

Move technical info:

```text
/admin/system
```

Advanced only.

---

# 116. Phase 12 — Remove legacy monolith

Xóa:

```text
active string navigation
content useMemo giant switch
manual global load()
duplicate type declarations
inline style blocks
legacy CSS
```

---

# 117. PR breakdown đề xuất

```text
PR 01 refactor(web): introduce typed realtime provider and event envelopes
PR 02 refactor(web): introduce router query client and normalized api layer
PR 03 refactor(web): add semantic status and error copy
PR 04 feat(web): add browser voice announcement engine
PR 05 feat(web): add SSE-driven transaction voice announcements with dedupe
PR 06 feat(web): add redesigned transaction viewer
PR 07 feat(web): add redesigned admin shell and overview
PR 08 refactor(web): redesign bank connection flow
PR 09 feat(web): redesign notification channels
PR 10 refactor(web): consolidate polling delivery and audit activity
PR 11 refactor(web): move diagnostics into system page
PR 12 test(web): expand unit realtime voice and Playwright coverage
PR 13 chore(web): remove legacy App monolith and unused styles
```

---

# 118. Voice PR acceptance criteria

Phải đạt:

- [ ] Voice default off.
- [ ] User bật bằng thao tác rõ ràng.
- [ ] Có `Nghe thử`.
- [ ] Ưu tiên `vi-VN`.
- [ ] Amount đọc tiếng Việt tự nhiên.
- [ ] Event duplicate không đọc lại.
- [ ] Snapshot không đọc.
- [ ] Baseline không đọc.
- [ ] `reset_state` không đọc snapshot.
- [ ] Old replay ngoài freshness window không đọc.
- [ ] Multi-tab chỉ 1 tab phát.
- [ ] Event burst không overlap.
- [ ] Disable voice gọi `cancel()`.
- [ ] Không lưu transaction content vào localStorage.
- [ ] Voice setting tồn tại theo browser.
- [ ] Viewer vẫn hoạt động nếu browser không support speech.

---

# 119. Realtime acceptance criteria

- [ ] Không polling UI cố định 5 giây.
- [ ] `useRealtime` truyền `lastEventId`.
- [ ] Không `void load()` cho mọi SSE event.
- [ ] Event invalidate đúng domain.
- [ ] Unknown event không crash.
- [ ] Reset làm snapshot refresh.
- [ ] Reconnect không duplicate.
- [ ] Transaction event update list gần như lập tức.

---

# 120. UX acceptance criteria

- [ ] Không raw `AUTH_REQUIRED`.
- [ ] Không raw `MONITORING`.
- [ ] Không raw `DISABLED`.
- [ ] Không `dead-letter` trong normal UI.
- [ ] Không `Generation` trong normal UI.
- [ ] Main nav không có Polling/Audit/Delivery.
- [ ] Viewer không có admin controls.
- [ ] Transaction page usable mobile.
- [ ] Voice wording dễ hiểu.
- [ ] Realtime wording dễ hiểu.

---

# 121. Code quality acceptance criteria

- [ ] `App.tsx` nhỏ.
- [ ] Không file frontend > 1.000 lines.
- [ ] Page component chủ yếu composition.
- [ ] API call không nằm trong pure presentational component.
- [ ] Typed realtime events.
- [ ] Raw backend enum không đi trực tiếp vào JSX.
- [ ] No giant inline styles.
- [ ] Build pass.
- [ ] Unit pass.
- [ ] E2E pass.
- [ ] Voice tests pass.

---

# 122. Security acceptance criteria

- [ ] Voice không đọc account number.
- [ ] Voice không đọc balance mặc định.
- [ ] Description opt-in.
- [ ] Secret không localStorage.
- [ ] CSRF nằm trong API layer.
- [ ] Raw backend errors không leak viewer.
- [ ] SSE safe DTO giữ nguyên.
- [ ] RBAC backend vẫn là authority.

---

# 123. Performance acceptance criteria

- [ ] SSE là primary realtime path.
- [ ] Không refetch cả app sau mỗi event.
- [ ] No duplicated transaction fetch on same poll event.
- [ ] Route code split.
- [ ] Viewer không load diagnostics bundle.
- [ ] Admin System không load vào Viewer.
- [ ] Voice queue không block React render.
- [ ] Large transaction list vẫn cursor paginate.

---

# 124. Browser speech implementation sketch

```ts
class BrowserSpeechEngine implements VoiceEngine {
  async speak(message: VoiceMessage) {
    const utterance = new SpeechSynthesisUtterance(message.text);

    utterance.lang = 'vi-VN';
    utterance.volume = message.volume;
    utterance.rate = message.rate;
    utterance.pitch = message.pitch;

    const voice = resolveVietnameseVoice(
      window.speechSynthesis.getVoices(),
      message.voiceURI
    );

    if (voice) {
      utterance.voice = voice;
    }

    await speakUtterance(utterance);
  }

  cancel() {
    window.speechSynthesis.cancel();
  }
}
```

---

# 125. Event integration sketch

```ts
realtime.subscribe('bank.transaction.credit', event => {
  transactionRealtimeBridge.apply(event);

  voiceAnnouncementService.handle({
    eventId: event.id,
    transactionId: event.data.transactionId,
    amount: event.data.credit,
    description: event.data.description,
    detectedAt: event.data.detectedAt,
  });
});
```

---

# 126. Voice service sketch

```ts
async handle(event: TransactionVoiceEvent) {
  if (!settings.enabled) return;

  if (!leader.isLeader()) return;

  if (dedupe.has(event.eventId, event.transactionId)) return;

  if (!isFreshEnough(event.detectedAt)) {
    dedupe.mark(event);
    return;
  }

  dedupe.mark(event);

  burstAggregator.push(event);
}
```

---

# 127. Burst aggregator sketch

```text
first event
↓
start 500–1000ms collection window
↓
collect related events
↓
1–3 events → individual
4+ events → summary
```

Window không được quá dài vì mục tiêu realtime.

Khuyến nghị:

```text
750ms
```

---

# 128. Announcement phrases

## 1 transaction

```text
Bạn vừa nhận được năm trăm nghìn đồng.
```

## Với description

```text
Bạn vừa nhận được năm trăm nghìn đồng. Nội dung: Nguyễn Văn A chuyển tiền.
```

## Burst

```text
Bạn vừa nhận được năm giao dịch mới, tổng cộng ba triệu đồng.
```

---

# 129. Không đọc sender nếu parser chưa chắc chắn

Không tự parse:

```text
description → sender name
```

rồi nói:

```text
Nguyễn Văn A vừa chuyển...
```

trừ khi backend có field structured:

```text
senderName
```

đáng tin cậy.

V1:

```text
amount
+
optional raw description sanitized
```

an toàn hơn.

---

# 130. Future structured event

Sau này backend có thể nâng payload:

```json
{
  "transactionId": "...",
  "amount": "500000",
  "direction": "incoming",
  "currency": "VND",
  "counterparty": {
    "name": "..."
  },
  "description": "..."
}
```

Khi đó voice có thể:

```text
Bạn vừa nhận được năm trăm nghìn đồng từ Nguyễn Văn A.
```

---

# 131. Có nên dùng sound effect trước voice?

Optional.

Ví dụ:

```text
ding
→ 100ms
→ voice
```

Không default nếu gây khó chịu.

Setting:

```text
Âm báo trước khi đọc
```

P2, không P0.

---

# 132. Quiet hours

Optional phase sau:

```text
Không đọc từ 22:00 đến 07:00
```

Không cần P0 trừ khi viewer chạy 24/7 ở nhà.

---

# 133. Voice global vs per-account

Hiện hệ thống chủ yếu 1 ACB connection.

Preference V1:

```text
per-browser
```

Nếu multi-account sau này:

```text
per-account voice policy
```

---

# 134. Persist preference schema version

```json
{
  "version": 1,
  "enabled": true,
  "volume": 1,
  "rate": 1,
  "pitch": 1,
  "includeDescription": false,
  "burstMode": "summary"
}
```

Invalid schema:

```text
fallback default
```

---

# 135. Accessibility

Voice feature không thay thế visual accessibility.

Viewer vẫn cần:

- semantic transaction list;
- screen-reader labels;
- keyboard controls;
- focus;
- contrast;
- live region cho transaction new indicator phù hợp.

Không để screen reader và transaction voice đọc chồng nhau nếu có thể tránh.

---

# 136. Mobile UX voice

Toolbar mobile:

```text
🔊
```

tap:

```text
Bottom Sheet
```

Status:

```text
Đang bật
```

Không nhét slider trực tiếp vào header.

---

# 137. Desktop UX voice

Header:

```text
🔊 Đọc giao dịch
```

Popover:

```text
Bật/Tắt
Voice
Volume
Speed
Description
Test
```

---

# 138. Viewer realtime indicator và voice indicator khác nhau

Không gộp.

```text
● Trực tiếp
🔊 Đọc giao dịch: Bật
```

Realtime connection có thể alive trong khi voice off.

---

# 139. Nếu SSE disconnect

Voice status vẫn:

```text
Đang bật
```

nhưng phụ:

```text
Chờ kết nối lại để nhận giao dịch mới.
```

Không tự tắt user preference.

---

# 140. Nếu browser speech unavailable

Realtime vẫn chạy bình thường.

Feature graceful degradation:

```text
Voice unavailable
Transaction UI unaffected
```

---

# 141. Logging voice

Development:

```text
voice.enqueued
voice.spoken
voice.deduped
voice.suppressed_stale
voice.suppressed_follower_tab
```

Production không log:

```text
description
full amount history
```

Có thể log counts.

---

# 142. Metrics optional

Client telemetry sau này:

```text
voice_enabled_clients
voice_announcement_count
voice_failure_count
```

Không cần lưu nội dung.

---

# 143. Backend có cần thay đổi cho voice V1 không?

**Không bắt buộc.**

Event hiện tại đã đủ:

```text
transactionId
credit
description
detectedAt
```

Frontend chỉ cần refactor realtime metadata.

---

# 144. Backend thay đổi optional nên làm

## 1. Typed SSE schema docs

Document:

```text
bank.transaction.credit v1
```

## 2. Event schema version

Có thể thêm:

```json
"schemaVersion": 1
```

## 3. Structured direction

```json
"direction": "credit"
```

## 4. Optional counterparty structured

Nếu parse đáng tin.

---

# 145. Không cần WebSocket cho voice

SSE hiện tại đã phù hợp.

Voice chỉ cần:

```text
server → client
```

Không cần bidirectional transport.

Do đó:

```text
SSE > WebSocket
```

cho use case này vì hệ thống hiện đã có SSE journal/replay đúng hướng.

---

# 146. Flow latency

Target:

```text
ACB API response
   ↓
parse + DB
   ↓
journal publish
   ↓
SSE
   ↓
browser
   ↓
speech queue
```

Voice latency thêm ở client chỉ nên khoảng:

```text
vài chục ms + TTS engine startup
```

Không thêm remote TTS network ở V1.

---

# 147. Mục tiêu realtime

Delay lớn nhất lý tưởng vẫn chỉ là:

```text
backend chờ ACB trả dữ liệu
```

Sau khi backend detect transaction:

```text
UI + voice phải gần như lập tức
```

Đúng với mục tiêu kiến trúc hiện tại.

---

# 148. Realtime architecture final

```text
                    ACB
                     │
                     ▼
                Poll Worker
                     │
                     ▼
            Atomic Transaction Ingest
                     │
       ┌─────────────┼──────────────┐
       │             │              │
       ▼             ▼              ▼
 Transactions     Events        Deliveries
       │             │
       │        Event Journal
       │             │
       │         Event Hub
       │             │
       │             ▼
       │            SSE
       │             │
       └──────┬──────┘
              ▼
       RealtimeProvider
              │
   ┌──────────┼───────────┐
   │          │           │
   ▼          ▼           ▼
Queries      UI         Voice
Cache                   Queue
```

---

# 149. Frontend final architecture

```text
AppProviders
├── QueryClientProvider
├── RealtimeProvider
├── VoiceAnnouncementProvider
└── RouterProvider
    ├── AdminLayout
    │   ├── Overview
    │   ├── Connection
    │   ├── Notifications
    │   ├── Activity
    │   └── System
    │
    └── ViewerLayout
        ├── Transactions
        └── Transaction Detail
```

---

# 150. Definition of Done

Refactor chỉ được xem là hoàn thành khi:

## Architecture

- App không còn monolith.
- Router thật.
- Query cache.
- Typed realtime.
- Event metadata.
- No global load-everything.

## UX

- Admin và Viewer tách rõ.
- Modern redesign.
- Friendly Vietnamese copy.
- Không technical enum ở normal UI.

## Realtime

- SSE là primary realtime.
- Reconnect safe.
- Reset safe.
- Transaction insert realtime.

## Voice

- New credit event đọc gần như lập tức.
- Không duplicate.
- Không đọc baseline.
- Không đọc snapshot.
- Multi-tab safe.
- Burst safe.
- Vietnamese amount natural.

## Quality

- Unit tests.
- Realtime tests.
- Voice tests.
- E2E.
- Mobile.
- Accessibility.
- Build pass.

---

# 151. Ưu tiên triển khai thực tế

Nếu làm ngay, thứ tự tốt nhất là:

```text
1. Refactor useRealtime để expose event id
2. Typed realtime provider
3. API + Query foundation
4. Status/error copy
5. Voice core + dedupe + test
6. Transaction Viewer mới
7. Admin shell mới
8. Bank connection
9. Notification channels
10. Activity
11. Diagnostics
12. Remove legacy App
```

Không nên redesign UI trước rồi mới sửa realtime architecture vì sẽ phải sửa lại lần hai.

---

# 152. Kết luận

Sau khi đọc lại source mới, kiến trúc mục tiêu khác plan trước ở một điểm rất quan trọng:

> **Repo hiện đã có một event-driven realtime pipeline khá tốt ở backend, vì vậy frontend mới nên xoay quanh SSE domain events thay vì tiếp tục tư duy refresh/poll UI.**

Đối với voice:

> **`bank.transaction.credit` chính là nguồn trigger chuẩn.**

Không cần compare list.

Không cần thêm WebSocket.

Không cần remote TTS ở V1.

Luồng nên là:

```text
transaction mới thật sự
→ event journal
→ SSE
→ typed realtime event
→ transaction UI update
→ voice dedupe
→ voice queue
→ đọc: "Bạn vừa nhận được ... đồng"
```

Và requirement quan trọng nhất:

```text
"mỗi giao dịch thật chỉ được đọc đúng một lần trong điều kiện realtime bình thường"
```

đồng thời:

```text
fresh load / baseline / reset / duplicate replay
```

không được làm hệ thống đọc nhầm hàng loạt giao dịch cũ.
