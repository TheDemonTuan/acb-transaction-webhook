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

# PLAN V5 — FINAL IMPLEMENTATION
## ACB Transaction Webhook
### Edge TTS primary · gTTS fallback · Browser vi-VN fallback · Optional Piper offline
### Realtime voice hardening · Polling schedule · Keepalive · Catch-up · Filter ACB · KPI/date · QR tĩnh · Security · Testing · Rollout

> Repository: `TheDemonTuan/acb-transaction-webhook`  
> Baseline mới nhất đã kiểm tra: `f0fae614058099cba655eb9ad2af7e746df6c966`  
> Commit: `feat: implement Plan V3 with schedule polling, keepalive, server-side filter, KPI aggregate, and static QR`  
> Ngày cập nhật: 12/09/2026  
> Timezone vận hành: `Asia/Ho_Chi_Minh`

---

# 0. Mục tiêu cuối cùng

Hệ thống sau V5 phải đạt:

1. **Backend phát hiện giao dịch ACB mới nhanh nhất có thể trong giờ cần realtime.**
2. **Frontend không HTTP-poll liên tục; SSE là kênh realtime chính.**
3. **Giao dịch tiền vào REALTIME được đọc bằng giọng Việt tự nhiên.**
4. **Không còn phụ thuộc việc máy Windows/browser có cài voice Việt.**
5. **Không fallback tiếng Việt sang giọng Mỹ.**
6. **Edge TTS là primary vì nhẹ VPS, giọng Việt tốt và không cần API key.**
7. **gTTS là fallback online thứ hai.**
8. **Browser Web Speech chỉ fallback nếu thiết bị thực sự có `vi-VN`.**
9. **Piper chỉ là tùy chọn offline/self-host sau này, không cần chạy mặc định.**
10. **Không lưu MP3 lâu dài; ưu tiên stream/in-memory, cache có kiểm soát.**
11. **Ngoài giờ realtime chỉ keepalive 3–5 phút để giữ session.**
12. **Khi quay lại giờ realtime phải catch-up một lần để không mất giao dịch.**
13. **Filter UI query DB ngay; chỉ sync ACB đúng date range nếu coverage thiếu.**
14. **KPI tính toàn bộ range trong DB, không dựa 100 rows.**
15. **Ngày ACB `DD/MM/YYYY` được canonical hóa backend.**
16. **QR nhận tiền chỉ là QR tĩnh: upload hoặc generate.**
17. **TTS lỗi không được ảnh hưởng monitor ACB, webhook, SSE hoặc transaction storage.**
18. **Có diagnostics đủ để biết chính xác vì sao một giao dịch không được đọc.**

---

# 1. Quyết định kiến trúc quan trọng nhất

## V4 cũ

```text
Primary = Piper local
```

## V5 mới

```text
Primary
  Edge TTS
  vi-VN-HoaiMyNeural

Fallback 1
  gTTS
  lang=vi

Fallback 2
  Browser Web Speech
  ONLY vi-VN

Optional offline fallback
  Piper
```

## Vì sao đổi

Case hiện tại chỉ cần đọc câu ngắn:

```text
Bạn vừa nhận được năm trăm nghìn đồng.
```

Không cần một neural model chạy thường trực trên VPS nếu:

- VPS cần nhẹ;
- có Internet;
- chấp nhận dùng online TTS không chính thức;
- muốn giọng Việt tự nhiên hơn.

Edge TTS phù hợp hơn về:

- RAM VPS;
- CPU VPS;
- chất lượng giọng;
- setup;
- không cần GPU;
- không cần API key.

Piper vẫn là lựa chọn tốt nếu sau này yêu cầu:

```text
offline
không gửi phrase ra ngoài
không phụ thuộc Edge/Google
```

nhưng không cần deploy ngay.

---

# 2. Research snapshot dùng cho V5

## 2.1 edge-tts

Project:

```text
rany2/edge-tts
```

Cho phép dùng Microsoft Edge online text-to-speech:

- Python module;
- CLI;
- không cần Microsoft Edge;
- không cần Windows;
- không cần API key;
- hỗ trợ streaming;
- hỗ trợ rate;
- hỗ trợ volume;
- hỗ trợ pitch.

Release mới nhất đã xác minh trong lúc lập plan:

```text
7.2.8
22/03/2026
```

Nên pin version khi deploy.

## 2.2 Voice tiếng Việt Microsoft

Microsoft hiện liệt kê:

```text
vi-VN-HoaiMyNeural
Female

vi-VN-NamMinhNeural
Male
```

Default khuyến nghị:

```text
vi-VN-HoaiMyNeural
```

Admin cho phép đổi Nam Minh.

## 2.3 gTTS

`gTTS`:

- dùng Google Translate speech;
- không cần Google Cloud API key;
- output MP3;
- có thể write vào file-like object / bytes;
- hỗ trợ `lang="vi"`.

Nhưng đây là undocumented Google Translate speech functionality.

Dùng fallback, không dùng primary production path.

## 2.4 Browser Web Speech

Giữ emergency fallback.

Rule tuyệt đối:

```text
Vietnamese phrase
→ only Vietnamese voice
```

Nếu browser chỉ có:

```text
en-US
```

thì không đọc.

## 2.5 Piper

Không chạy mặc định.

Chỉ optional:

```text
TTS_OFFLINE_ENABLED=true
```

khi muốn self-host hoàn toàn.

---

# 3. Architecture cuối cùng

```text
                         ┌────────────────────┐
                         │        ACB         │
                         └─────────┬──────────┘
                                   │
                          poll / keepalive
                                   │
                         ┌─────────▼──────────┐
                         │     Go Gateway     │
                         │                    │
                         │ Monitor Scheduler  │
                         │ SQLite             │
                         │ Event Journal      │
                         │ SSE                │
                         │ Voice API          │
                         └──────┬───────┬─────┘
                                │       │
                      SSE credit│       │ internal HTTP
                                │       │
                    ┌───────────▼──┐ ┌──▼──────────────┐
                    │   React UI   │ │  TTS Gateway    │
                    │              │ │  Python         │
                    │ leader       │ │                 │
                    │ freshness    │ │ Edge TTS        │
                    │ dedupe       │ │ ↓ fallback      │
                    │ AudioPlayer  │ │ gTTS            │
                    └──────┬───────┘ └─────────────────┘
                           │
                           ▼
                          🔊
```

Optional:

```text
TTS Gateway
├── Edge
├── gTTS
└── Piper adapter later
```

---

# 4. Docker architecture

Target Compose:

```text
gateway
auth-browser
tts-gateway
```

Không cần:

```text
tts-piper
```

mặc định.

`tts-gateway` cực nhẹ:

```text
Python 3.12 slim
FastAPI
Uvicorn
edge-tts
gTTS
```

No GPU.

No CUDA.

No AI model volume.

---

# 5. Network topology

```text
Internet
   │
Cloudflare Tunnel
   │
gateway/web
   │
Docker internal network
   │
tts-gateway
```

`tts-gateway`:

```text
NO public port
NO Cloudflare route
NO direct browser access
```

Gateway gọi:

```text
http://tts-gateway:8081
```

---

# 6. Vì sao vẫn cần TTS Gateway thay vì React gọi Edge trực tiếp

Không phải để chạy model.

Sidecar chỉ làm adapter rất nhẹ.

Lợi ích:

1. Edge protocol thay đổi → sửa một service.
2. gTTS fallback nằm một chỗ.
3. Không phụ thuộc CORS từ browser.
4. Không để từng tab mở connection Edge riêng.
5. Có timeout/retry/circuit breaker.
6. Có metrics.
7. Có cache.
8. Có privacy policy.
9. Có voice whitelist.
10. Không expose unofficial protocol tới React.
11. Có thể đổi Edge → Azure/Piper sau này mà React không đổi.

---

# 7. Không tạo file MP3 tạm cho từng transaction

Primary flow:

```text
Edge TTS
↓
audio chunks / MP3 bytes
↓
TTS Gateway
↓
Go Gateway
↓
Browser
```

Không:

```text
/tmp/transaction_123.mp3
```

trừ cache có chủ đích.

gTTS fallback dùng:

```text
BytesIO
```

không cần ghi file.

---

# 8. TTS Gateway API nội bộ

## Health

```http
GET /health
```

Response:

```json
{
  "status": "ok",
  "primary": "edge",
  "edge": {
    "available": true
  },
  "gtts": {
    "available": true
  }
}
```

Không check external network quá nặng mỗi health probe.

## Voices

```http
GET /voices
```

Response whitelist:

```json
[
  {
    "id": "vi-VN-HoaiMyNeural",
    "name": "Hoài My",
    "gender": "female",
    "provider": "edge"
  },
  {
    "id": "vi-VN-NamMinhNeural",
    "name": "Nam Minh",
    "gender": "male",
    "provider": "edge"
  }
]
```

## Synthesize

Internal only:

```http
POST /synthesize
```

Request:

```json
{
  "text": "Bạn vừa nhận được năm trăm nghìn đồng.",
  "voice": "vi-VN-HoaiMyNeural",
  "rate": "+0%",
  "pitch": "+0Hz"
}
```

Response:

```text
audio/mpeg
```

Headers:

```text
X-TTS-Provider: edge
X-TTS-Voice: vi-VN-HoaiMyNeural
```

Nếu Edge fail và gTTS success:

```text
X-TTS-Provider: gtts
```

---

# 9. Không expose arbitrary internal TTS

Browser không gọi:

```text
tts-gateway:8081
```

Chỉ:

```text
/api/v1/voice/...
```

trên Go Gateway.

---

# 10. Go Voice API — thiết kế mạnh hơn arbitrary text

Không khuyến nghị public endpoint:

```http
POST /api/v1/voice/synthesize
{ "text": "anything" }
```

Thay vào đó transaction voice nên server-authoritative.

## Transaction audio

```http
POST /api/v1/voice/transactions/{transactionId}
```

Body:

```json
{
  "includeDescription": false
}
```

Gateway:

1. auth user;
2. lookup transaction;
3. verify `credit > 0`;
4. verify transaction source/policy;
5. build Vietnamese phrase;
6. call internal TTS Gateway;
7. stream MP3 về client.

## Test voice

Endpoint riêng:

```http
POST /api/v1/voice/test
```

Server dùng fixed safe phrase.

Như vậy app không trở thành free public arbitrary TTS proxy.

---

# 11. Tại sao dùng transactionId tốt hơn gửi text

SSE đã có:

```text
transactionId
```

Frontend chỉ gửi lại ID.

Backend authoritative:

```text
transaction
credit
description
ingest_source
first_seen_at
```

Ưu điểm:

- không synth arbitrary text;
- không trust amount frontend;
- không trust description frontend;
- không bị abuse thành public TTS;
- backend quyết định phrase;
- privacy rule tập trung;
- dễ audit.

---

# 12. Phrase builder chuyển backend

Tạo package:

```text
internal/voicecopy/
```

API:

```go
func BuildCreditAnnouncement(
    amount int64,
    description string,
    includeDescription bool,
) string
```

Ví dụ:

```text
500000
→ Bạn vừa nhận được năm trăm nghìn đồng.
```

---

# 13. Vietnamese money-to-words

Bắt buộc unit test:

```text
1
5
10
15
20
21
24
25
100
101
105
110
115
1,000
10,000
100,000
500,000
1,000,000
1,005,000
1,234,567
10,000,000
100,000,000
1,000,000,000
```

Natural forms:

```text
mốt
tư
lăm
lẻ
triệu
tỷ
```

No float money.

Input:

```text
int64
```

---

# 14. Description privacy

Default:

```text
includeDescription=false
```

Edge/gTTS chỉ thấy:

```text
Bạn vừa nhận được năm trăm nghìn đồng.
```

Không gửi:

- account number;
- balance;
- transaction number;
- sender;
- bank reference;
- raw ACB HTML.

Nếu bật description:

UI phải cảnh báo:

```text
Nội dung giao dịch sẽ được gửi tới dịch vụ TTS trực tuyến.
```

---

# 15. Description sanitize

Nếu bật:

- Unicode normalize;
- trim;
- collapse spaces;
- strip controls;
- max ~80 chars;
- remove URLs;
- limit long numeric references.

Phrase:

```text
Bạn vừa nhận được năm trăm nghìn đồng.
Nội dung: thanh toán đơn hàng.
```

---

# 16. TTS provider policy

```text
EDGE
↓
gTTS
↓
Browser vi-VN
↓
error / optional chime
```

Piper không tự động bật.

---

# 17. Edge retry policy

Không retry nhiều.

Proposed:

```text
attempt 1
↓ fail
100ms jitter
↓
attempt 2
↓ fail
fallback gTTS
```

Không:

```text
10 retries
```

vì voice notification phải fresh.

---

# 18. gTTS fallback policy

gTTS chỉ dùng khi Edge:

- timeout;
- no audio;
- transient network;
- provider protocol error.

Không dùng nếu request invalid.

---

# 19. Circuit breaker

Edge failures:

```text
N=3 consecutive failures
→ OPEN
```

Trong OPEN:

```text
skip Edge
→ gTTS directly
```

Probe lại:

```text
30–60s
```

Success:

```text
HALF_OPEN → CLOSED
```

Tương tự có circuit breaker gTTS nếu cần.

---

# 20. Timeouts

Internal TTS call:

```text
connect timeout ~1s
overall Edge synth ~3–5s
gTTS fallback ~3–5s
```

Một voice notification không được giữ HTTP quá lâu.

Recommended whole request hard deadline:

```text
6–8s
```

Sau đó fail/chime.

---

# 21. Streaming vs buffer

## Edge

Có thể stream audio chunks.

Khuyến nghị V1:

```text
collect short MP3 fully in memory
→ return
```

vì phrase cực ngắn, implementation dễ và retry/fallback sạch hơn.

Sau khi ổn định có thể optimize:

```text
StreamingResponse
```

nếu benchmark cho thấy cần.

## gTTS

Use memory buffer.

Không disk file.

---

# 22. Cache

Amount phrases lặp rất nhiều.

Cache key:

```text
SHA256(
  providerClass +
  voice +
  rate +
  phrase
)
```

Cache:

```text
LRU memory
```

và optional disk small cache.

Recommended:

```text
memory 32–64 MB
disk max 100–200 MB
TTL 7 days
```

---

# 23. Cache privacy policy

If:

```text
includeDescription=false
```

cache allowed.

If:

```text
includeDescription=true
```

default:

```text
no disk cache
```

memory short TTL only.

---

# 24. Browser VoiceEngine mới

Primary không còn BrowserSpeechEngine.

Tạo:

```text
TransactionAudioEngine
```

Responsibilities:

- request transaction audio;
- decode/play MP3;
- cancel;
- volume;
- runtime diagnostics.

BrowserSpeechEngine chỉ fallback.

---

# 25. Browser audio playback

Prefer:

```text
AudioContext
```

hoặc `HTMLAudioElement` object URL.

Khuyến nghị:

- V1 dùng `Audio`/Blob URL nếu ổn định nhất;
- AudioContext nếu cần fine control/queue/unlock.

Dù chọn gì phải có explicit autoplay state.

---

# 26. Autoplay unlock

User click:

```text
Bật đọc giao dịch
```

phải unlock playback.

State:

```text
AUDIO_LOCKED
READY
```

Sau reload browser có thể lock lại.

UI:

```text
Giọng đọc đã bật.
Nhấn để kích hoạt âm thanh.
```

Không silent fail.

---

# 27. Voice runtime state

```text
DISABLED
INITIALIZING
READY
AUDIO_LOCKED
SPEAKING
DEGRADED
ERROR
```

Normal viewer nhìn label tiếng Việt.

Developer/Admin có diagnostic detail.

---

# 28. Fix P0 — freshness clock skew

Current logic có nguy cơ:

```text
age < 0
→ drop
```

Sửa:

```text
MAX_REPLAY_AGE = 120s
FUTURE_CLOCK_SKEW = 30s
```

Use:

```text
envelope.receivedAt
```

làm client reference time.

Rule:

```text
-30s <= age <= 120s
```

---

# 29. Không dùng transactionDate cho voice freshness

Use:

```text
detectedAt
receivedAt
```

Không:

```text
transactionDate
```

vì ACB có thể chỉ trả ngày.

---

# 30. Fix P0 — dedupe reservation

Không mark permanently trước audio success.

Flow:

```text
event
↓
reserve
↓
TTS request
↓
play
↓
mark PLAYED
```

Retryable failure:

```text
release reservation
```

Permanent policy skip:

```text
mark SKIPPED
```

---

# 31. Dedupe keys

Giữ nhiều lớp:

```text
eventId
transactionId
semanticKey
```

Session storage persisted:

```text
PLAYED
SKIPPED
```

In-memory:

```text
RESERVED
PLAYING
```

---

# 32. Source eligibility

Backend V3 đã có:

```text
REALTIME
CATCH_UP
FILTER_SYNC
BOOTSTRAP
```

Voice:

```text
REALTIME only
```

Recommended event field:

```json
{
  "source": "REALTIME",
  "announceEligible": true
}
```

Client verify cả hai.

---

# 33. Không đọc giao dịch tiền ra

Voice event:

```text
credit > 0
```

UI wording:

```text
Đọc khi có tiền vào
```

Không gọi generic:

```text
Đọc giao dịch
```

nếu thực tế debit không đọc.

---

# 34. Fix P0 — Browser fallback strict Vietnamese

Nếu fallback tới Web Speech:

```text
vi-VN
vi-*
```

only.

Không:

```text
default voice
first voice
en-US
```

Nếu không có:

```text
BROWSER_VIETNAMESE_VOICE_UNAVAILABLE
```

---

# 35. Browser voiceschanged

Persistent listener:

```text
voiceschanged
```

UI cập nhật nếu voice Việt xuất hiện sau mount.

Button:

```text
Kiểm tra lại giọng trên thiết bị
```

---

# 36. Fix realtime subscription identity

Use stable `useCallback` for:

```text
subscribe
onAny
```

Không resubscribe DomainBridge mỗi SSE state update.

---

# 37. Multi-tab leader

Initial:

```text
false
```

Không optimistic `true`.

Only elected leader:

```text
requests audio
plays audio
```

3 tabs:

```text
1 synth request
1 speaker
```

---

# 38. Cross-tab settings

Sync:

```text
storage event
```

Local preferences:

- enabled;
- volume;
- includeDescription;
- announceWhenHidden;
- burst mode.

---

# 39. Burst mode

Window:

```text
750ms
```

Individual:

```text
mỗi transaction
```

Summary:

```text
Bạn vừa nhận ba giao dịch,
tổng cộng một triệu hai trăm nghìn đồng.
```

---

# 40. Queue limits

```text
MAX_PENDING = 5
```

Nếu burst lớn:

```text
aggregate
```

Không để audio queue vài phút.

---

# 41. Queue freshness

Nếu message chờ:

```text
>30s
```

aggregate/drop.

Transaction UI vẫn authoritative.

---

# 42. Optional chime

Nếu Edge + gTTS + browser vi đều fail:

Optional setting:

```text
Phát âm báo nếu không thể đọc giọng
```

Use local bundled short chime.

Không silent completely.

---

# 43. Voice diagnostics

Expose:

```text
lastSSEEventAt
lastEligibleEventAt
lastAudioRequestAt
lastAudioStartedAt
lastAudioFinishedAt
lastProvider
lastDecision
lastError
runtimeState
isLeader
```

---

# 44. Admin diagnostics example

```text
Giọng đọc
Provider chính: Edge TTS
Voice: Hoài My
Fallback: gTTS
Browser vi-VN: Không có

SSE credit gần nhất: 18:52:11
Quyết định: PLAYED
Provider sử dụng: Edge
TTS latency: 210 ms
Audio start delay: 65 ms
```

---

# 45. Error codes

```text
VOICE_DISABLED
VOICE_NOT_LEADER
VOICE_SOURCE_NOT_REALTIME
VOICE_EVENT_STALE
VOICE_DUPLICATE
VOICE_AUDIO_LOCKED

EDGE_TTS_TIMEOUT
EDGE_TTS_NO_AUDIO
EDGE_TTS_UNAVAILABLE

GTTS_TIMEOUT
GTTS_UNAVAILABLE

BROWSER_VIETNAMESE_VOICE_UNAVAILABLE
BROWSER_SPEECH_ERROR

AUDIO_DECODE_FAILED
AUDIO_PLAYBACK_BLOCKED
```

---

# 46. User-facing errors

No raw enum in normal UI.

Examples:

```text
Giọng đọc trực tuyến đang tạm thời không khả dụng.
```

```text
Trình duyệt đang chặn âm thanh. Nhấn để kích hoạt.
```

```text
Thiết bị này chưa có giọng Tiếng Việt.
```

---

# 47. TTS Gateway implementation stack

Recommended:

```text
Python 3.12
FastAPI
Uvicorn
edge-tts==7.2.8
gTTS pinned version
httpx/aiohttp only if needed
```

Pin exact versions in requirements/lock.

---

# 48. TTS Gateway project structure

```text
tts-gateway/
  app/
    main.py
    config.py
    models.py
    providers/
      base.py
      edge.py
      gtts.py
    circuit_breaker.py
    cache.py
    metrics.py
  tests/
  Dockerfile
  requirements.lock
```

---

# 49. Provider interface Python

Conceptual:

```python
class TTSProvider(Protocol):
    name: str

    async def synthesize(
        self,
        text: str,
        voice: str | None,
        rate: str | None,
        pitch: str | None,
    ) -> bytes:
        ...
```

---

# 50. Edge implementation

Use:

```python
edge_tts.Communicate(
    text,
    voice,
    rate=...,
    pitch=...
)
```

Collect audio chunks:

```text
chunk["type"] == "audio"
```

No audio chunks:

```text
EDGE_TTS_NO_AUDIO
```

---

# 51. gTTS implementation

```python
gTTS(
    text=text,
    lang="vi"
)
```

write to:

```text
BytesIO
```

No filesystem.

---

# 52. Provider selection

```python
try Edge if circuit closed
except retryable:
    try gTTS
```

Return metadata:

```text
provider=edge|gtts
```

---

# 53. TTS internal authentication

Even Docker-internal endpoint should support optional shared token:

```text
X-Internal-TTS-Token
```

Gateway sends token.

TTS sidecar rejects missing/wrong.

Defense-in-depth.

Token from Docker secret/env.

---

# 54. Docker hardening TTS Gateway

```text
non-root
cap_drop ALL
no-new-privileges
read_only
tmpfs /tmp
no public ports
memory limit
CPU limit
```

No persistent storage required except optional cache volume.

---

# 55. Healthcheck

Compose health:

```text
GET /health
```

Should not require successful Microsoft synthesis every 10s.

Health means process healthy.

Separate provider probe for diagnostics with low frequency.

---

# 56. Go TTS client

Create:

```text
internal/ttsclient/
```

`Client`:

- BaseURL;
- internal token;
- HTTP transport;
- request timeout;
- structured errors.

No `http.DefaultClient`.

---

# 57. Go Voice API files

```text
internal/httpapi/voice.go
internal/httpapi/voice_test.go
internal/voicecopy/money_vi.go
internal/voicecopy/money_vi_test.go
internal/voicecopy/announcement.go
```

---

# 58. Voice API security

`POST /api/v1/voice/transactions/{id}`:

- authenticated;
- authorization;
- CSRF if required by app's write-request model;
- transaction exists;
- credit >0;
- rate-limit per user/session;
- max concurrent synth;
- no arbitrary text.

---

# 59. Test endpoint

```http
POST /api/v1/voice/test
```

Server fixed phrase:

```text
Đã bật đọc giao dịch mới.
Bạn vừa nhận được năm trăm nghìn đồng.
```

Can accept selected allowed voice:

```json
{
  "voiceId": "vi-VN-HoaiMyNeural"
}
```

No arbitrary text.

---

# 60. Voice settings model

Admin/global:

```text
providerMode = ONLINE_AUTO | EDGE_ONLY | BROWSER_ONLY | OFFLINE_PIPER
edgeVoice = HoaiMy | NamMinh
onlineFallback = true
```

Viewer/local:

```text
enabled
volume
includeDescription
announceWhenHidden
burstMode
```

---

# 61. Default voice config

```text
providerMode=ONLINE_AUTO
edgeVoice=vi-VN-HoaiMyNeural
onlineFallback=true
includeDescription=false
```

Flow:

```text
Edge → gTTS → Browser vi-VN
```

---

# 62. Admin UI

Section:

```text
Admin
└── Giọng đọc giao dịch
```

Provider:

```text
(•) Tự động — khuyến nghị
    Microsoft Edge → Google fallback

( ) Chỉ Microsoft Edge

( ) Chỉ giọng trên thiết bị

( ) Offline/self-host
    Piper — chưa cấu hình
```

---

# 63. Voice choice UI

When Edge:

```text
Hoài My — Nữ
Nam Minh — Nam
```

When browser:

only detected Vietnamese voices.

---

# 64. Privacy UI

Show:

```text
Mặc định chỉ số tiền được gửi tới dịch vụ giọng đọc.
```

Checkbox:

```text
Đọc kèm nội dung giao dịch
```

Under checkbox:

```text
Khi bật, nội dung chuyển khoản sẽ được gửi tới nhà cung cấp TTS trực tuyến.
```

---

# 65. Viewer quick status

Header:

```text
🔊 Sẵn sàng
```

States:

```text
Tắt
Sẵn sàng
Đang đọc
Cần kích hoạt
TTS dự phòng
Lỗi
```

---

# 66. Nghe thử

Must use:

```text
Go /voice/test
→ TTS Gateway
→ Edge/gTTS
→ browser player
```

Không dùng BrowserSpeech direct nếu provider auto.

---

# 67. TTS latency metrics

TTS Gateway:

```text
tts_requests_total
tts_provider_success_total
tts_provider_failure_total
tts_synthesis_duration_seconds
tts_cache_hit_total
tts_circuit_state
```

Go Gateway:

```text
voice_audio_request_total
voice_audio_request_duration
```

---

# 68. Privacy logs

Do not log:

```text
text
description
account number
transaction content
```

Log:

```text
chars
provider
voice
duration
fallbackUsed
errorCode
```

---

# 69. Source baseline V3 — what is already implemented

Commit `f0fae614` added/modified:

- `internal/acb/date.go`;
- monitor scheduler logic;
- runtime settings;
- keepalive/catch-up tests;
- history coverage;
- singleflight;
- server-side queries;
- canonical date;
- payment QR storage/API/UI;
- ScheduleSettingsSection;
- PaymentQRSettingsSection;
- ReceivingQRModal;
- Transaction Viewer refactor;
- source-aware voice suppression tests.

V5 should **build on this**, not rewrite everything.

---

# 70. V3 areas that must be verified, not blindly redone

Before implementing TTS:

```text
go test ./...
frontend tests
frontend build
E2E
```

Then smoke:

- schedule config save;
- realtime/keepalive transition;
- catch-up;
- today filter;
- 7-day filter;
- KPI;
- QR upload/generate.

---

# 71. Poll schedule requirement

Default example:

```text
07:00–23:00
REALTIME
5–15 seconds

23:00–07:00
KEEPALIVE_ONLY
180–300 seconds
```

Admin can change.

---

# 72. Scheduler rules

Priority:

```text
rate-limit/backoff
>
auth/session state
>
configured schedule
```

Schedule cannot override ACB safety.

---

# 73. Keepalive

Outside realtime window:

```text
NO History query
```

Only lightweight authenticated request sufficient to maintain session.

Must empirically test ACB behavior.

---

# 74. Keepalive interval configuration

Admin supports:

```text
min
max
```

Example:

```text
180
300
```

Random jitter.

No fixed robotic interval.

---

# 75. Catch-up

At transition:

```text
KEEPALIVE_ONLY → REALTIME
```

Run:

```text
CATCH_UP
```

then:

```text
REALTIME
```

No voice from catch-up.

---

# 76. Realtime range

During normal realtime:

```text
today → today
```

Around midnight configurable overlap:

```text
yesterday → today
```

for first N minutes.

---

# 77. Long downtime

Catch-up from last known checkpoint.

Hard max auto range:

```text
7 days
```

Larger history requires explicit sync.

---

# 78. Filter architecture

Client always:

```text
DB first
```

No filter directly reconfigures global poll loop.

---

# 79. Date filter pushdown

ACB supports by-date form.

Push:

```text
from
to
```

only.

Other filters stay DB-side unless verified later.

---

# 80. Coverage

For requested range:

```text
fresh?
→ no ACB call

stale?
→ one-shot sync
```

---

# 81. Single-flight

Same connection + same date range:

```text
one upstream sync
```

All clients join result.

---

# 82. `Tất cả`

Never:

```text
auto fetch all ACB history
```

Only DB.

Provide explicit date sync UI if needed.

---

# 83. Search

Server-side DB.

Potential future SQLite FTS5 if data becomes large.

Not upstream ACB.

---

# 84. KPI

SQL aggregate whole filter range.

Never current page only.

---

# 85. Date correctness

Raw ACB:

```text
12/09/2026
```

Backend canonical:

```text
2026-09-12
```

Frontend never `new Date(rawAcbDate)`.

---

# 86. Transaction detail

Dedicated endpoint.

Never fetch 100 then `.find`.

---

# 87. SSE

Keep SSE.

No need WebSocket.

Events:

```text
bank.transaction.credit
poll.completed
history.sync.completed
monitor.mode.changed
...
```

Voice only uses authoritative credit event.

---

# 88. Do not trigger voice from poll.completed

Wrong:

```text
insertedCount > 0
→ voice
```

Correct:

```text
bank.transaction.credit
source=REALTIME
→ voice
```

---

# 89. Static QR requirement

QR stays static only.

Sources:

```text
upload
generate VietQR once
```

No dynamic amount/reference.

---

# 90. QR storage

Local persistent data volume.

Atomic update.

Previous good QR survives failed regenerate.

---

# 91. QR Viewer UX

Sticky:

```text
Nhận tiền
```

Modal:

- large QR;
- ACB;
- account holder;
- account number;
- copy;
- fullscreen;
- mobile safe area.

---

# 92. QR security

- image magic validation;
- reject SVG;
- random filename;
- no path traversal;
- admin writes only;
- viewer read-only.

---

# 93. Independent failure domains

TTS down:

```text
monitor works
DB works
SSE works
webhook works
UI works
```

ACB down:

TTS service health irrelevant.

QR provider down after local QR cached:

QR still displays.

---

# 94. Browser lifecycle

Handle:

```text
visibilitychange
pageshow
pagehide
```

On resume:

- leader re-evaluate;
- audio unlock state;
- stale event suppression.

---

# 95. Background tabs

`announceWhenHidden=true` is best-effort.

Browser/OS may throttle background playback.

Do not promise universal background audio on iOS/Safari.

---

# 96. Supported browser priority

Primary production QA:

```text
Chrome Windows
Edge Windows
```

Then:

```text
Chrome macOS
Safari macOS
Chrome Android
Safari iOS
```

---

# 97. Current Browser voice behavior becomes fallback only

This also means if user's Windows has only US voices:

```text
does not matter
```

because default audio comes from Edge TTS MP3.

This directly fixes the original complaint.

---

# 98. Version pinning

TTS Gateway requirements lock.

Example:

```text
edge-tts==7.2.8
gTTS==<verified pinned version>
fastapi==<pin>
uvicorn==<pin>
```

Do not auto-upgrade `edge-tts` in production.

---

# 99. Upstream break contingency

Since Edge/gTTS use unofficial mechanisms:

1. dependency pinned;
2. circuit breaker;
3. two providers;
4. browser vi fallback;
5. optional Piper adapter;
6. monitoring alert;
7. upgrade library deliberately.

---

# 100. Optional Piper later

If both online services become unacceptable:

```text
providerMode=OFFLINE_PIPER
```

Then deploy Piper sidecar.

No frontend redesign required because Go Voice API stays stable.

---

# 101. Optional Azure later

If official SLA needed:

```text
AzureProvider
```

Go/TTS Gateway API unchanged.

Secrets backend only.

---

# 102. Security headers for audio

Response:

```text
Content-Type: audio/mpeg
X-Content-Type-Options: nosniff
Cache-Control: private, no-store
```

Client can still play blob.

Server internal cache independent of browser cache.

---

# 103. Rate limiting

Voice transaction endpoint:

```text
per authenticated subject
global
```

Prevent repeated manual calls on same transaction.

Can key:

```text
user + transactionId
```

short TTL.

---

# 104. TTS concurrency

No need high concurrency.

Recommended:

```text
2–4 concurrent synth max
```

Voice playback itself serial per client.

---

# 105. Transaction voice eligibility backend

`POST /voice/transactions/{id}` can enforce:

```text
credit > 0
ingest_source = REALTIME
transaction recent enough
```

But allow configurable short grace period so reconnect/retry still works.

Example:

```text
first_seen <= 5 minutes
```

Client stricter at 120s.

---

# 106. Why server grace > client freshness

Client freshness:

```text
notification UX
```

Server grace:

```text
security / abuse protection
```

Client may retry because playback failed.

So server can allow 5m while UI normally speaks <=120s.

---

# 107. Audio request cancellation

Frontend uses `AbortController`.

Disable voice:

```text
abort HTTP
stop current audio
clear queue
clear burst
```

Provider switch:

same.

---

# 108. Provider metadata

Go returns headers:

```text
X-Voice-Provider
X-Voice-Voice
X-Voice-Fallback
```

Admin diagnostics can capture them.

Do not expose internal URLs.

---

# 109. E2E voice test matrix

## Edge success

```text
REALTIME credit
→ Edge
→ play once
```

## Edge fail, gTTS success

```text
fallback
→ play once
```

## Edge + gTTS fail, browser vi present

```text
browser vi fallback
```

## Edge + gTTS fail, browser only en-US

```text
no English speech
→ error/chime
```

---

# 110. Clock skew E2E

Server `detectedAt`:

```text
client +10s
```

must play.

Old:

```text
client -150s
```

must not.

---

# 111. Multi-tab E2E

Two tabs.

One incoming credit.

Assert:

```text
1 /voice transaction request
1 playback
```

---

# 112. Catch-up E2E

Overnight transaction discovered in CATCH_UP.

Assert:

```text
DB yes
UI yes
voice no
```

---

# 113. Filter-sync E2E

Historical range sync finds old credits.

Assert:

```text
voice no
```

---

# 114. Voice retry E2E

First audio request fails transient.

Retry/fallback succeeds.

Assert:

```text
one final playback
not permanently deduped before success
```

---

# 115. Audio lock E2E

Saved voice enabled.

Fresh reload.

If playback policy locked:

```text
AUDIO_LOCKED
```

User click:

```text
READY
```

---

# 116. Amount words tests

Human review actual generated speech for:

```text
15k
21k
25k
105k
115k
1.005m
1.234.567
1 billion
```

Vietnamese number pronunciation matters more than generic TTS benchmark.

---

# 117. Edge voice A/B

Test both:

```text
HoaiMy
NamMinh
```

Choose default after listening specifically to money phrases.

Current recommendation:

```text
HoaiMy
```

but make configurable.

---

# 118. TTS latency benchmark

Measure:

```text
Edge first request
Edge warm/repeated
gTTS fallback
cache hit
```

Target after backend detects transaction:

```text
SSE + audio start ideally well under a few seconds
```

Do not state fixed guarantee before VPS/network benchmark.

---

# 119. Monitoring timestamps

Capture:

```text
transactionDetectedAt
sseReceivedAt
audioRequestAt
ttsCompletedAt
audioStartedAt
audioFinishedAt
```

This tells exact delay source.

---

# 120. Main latency objective

The only unavoidable major delay should remain:

```text
backend waiting for ACB polling response / next polling interval
```

Once gateway knows transaction:

```text
SSE
→ TTS
→ speaker
```

must be short and measurable.

---

# 121. CI pipeline

Backend:

```text
go test ./...
go vet ./...
```

Frontend:

```text
npm ci
npm run typecheck
npm test
npm run build
```

TTS Gateway:

```text
pytest
ruff/lint
type checks if configured
```

E2E:

```text
Playwright
```

---

# 122. CI should not depend on real Edge/Google for every test

Unit/PR tests:

```text
mock provider
```

Optional scheduled integration:

```text
real Edge smoke
real gTTS smoke
```

Avoid flaky CI from external services.

---

# 123. Production provider probe

Low frequency background diagnostics:

```text
every 5–15 minutes
```

or on-demand.

Do not continuously synthesize audio just for health.

Circuit breaker uses actual requests.

---

# 124. Logging TTS Gateway

Example:

```text
tts completed
provider=edge
voice=vi-VN-HoaiMyNeural
chars=42
duration_ms=280
fallback=false
```

Never phrase.

---

# 125. Auditing settings

Audit:

```text
voice.provider.updated
voice.settings.updated
voice.test.requested
```

Do not create DB audit row every automatic playback.

---

# 126. Deployment order

## Phase 0 — baseline verification

1. checkout `f0fae614`;
2. run Go tests;
3. frontend tests/build;
4. V3 E2E;
5. backup SQLite;
6. verify deployed migration.

## Phase 1 — current voice P0 fixes

1. clock skew;
2. stable subscription;
3. multi-tab leader;
4. strict browser vi fallback;
5. voiceschanged;
6. dedupe reservation;
7. diagnostics.

## Phase 2 — TTS Gateway

1. Python service;
2. Edge provider;
3. gTTS provider;
4. fallback;
5. circuit breaker;
6. health;
7. tests.

## Phase 3 — Go Voice API

1. transactionId audio API;
2. test API;
3. Vietnamese phrase builder;
4. TTS client;
5. rate limit;
6. auth.

## Phase 4 — frontend audio engine

1. request audio;
2. playback;
3. queue;
4. unlock;
5. cancellation;
6. status.

## Phase 5 — production hardening

1. cache;
2. metrics;
3. privacy UI;
4. fallback diagnostics;
5. real transfer smoke.

## Phase 6 — optional

1. Piper offline;
2. Azure official.

---

# 127. PR breakdown

## PR 1

```text
fix(voice): harden realtime eligibility dedupe leadership and clock skew
```

## PR 2

```text
feat(tts): add lightweight Edge TTS gateway with gTTS fallback
```

## PR 3

```text
feat(voice): add transaction-scoped backend audio API and Vietnamese money phrases
```

## PR 4

```text
feat(web): replace primary browser speech with streamed transaction audio
```

## PR 5

```text
feat(voice): add provider settings privacy controls and diagnostics
```

## PR 6

```text
perf(tts): add cache circuit breaker limits and telemetry
```

## PR 7

```text
test: cover realtime credit to Vietnamese audio end-to-end
```

## PR 8 optional

```text
feat(tts): add Piper offline provider
```

---

# 128. Proposed new files — TTS Gateway

```text
tts-gateway/
├── app/
│   ├── main.py
│   ├── config.py
│   ├── schemas.py
│   ├── cache.py
│   ├── circuit.py
│   └── providers/
│       ├── base.py
│       ├── edge.py
│       └── gtts.py
├── tests/
├── Dockerfile
└── requirements.lock
```

---

# 129. Proposed Go files

```text
internal/ttsclient/
  client.go
  errors.go
  client_test.go

internal/voicecopy/
  money_vi.go
  announcement.go
  money_vi_test.go

internal/httpapi/
  voice.go
  voice_test.go
```

---

# 130. Proposed frontend files

```text
web/src/features/voice-announcements/
  VoiceAnnouncementProvider.tsx
  voice-orchestrator.ts
  transaction-audio-engine.ts
  browser-speech-engine.ts
  audio-player.ts
  voice-dedupe.ts
  voice-queue.ts
  voice-settings.ts
  voice-diagnostics.ts
```

---

# 131. Compose plan

Conceptual:

```yaml
services:
  gateway:
    environment:
      TTS_GATEWAY_URL: http://tts-gateway:8081
      TTS_INTERNAL_TOKEN_FILE: /run/secrets/tts_internal_token
    depends_on:
      tts-gateway:
        condition: service_healthy

  tts-gateway:
    build:
      context: ./tts-gateway
    restart: unless-stopped
    expose:
      - "8081"
    environment:
      EDGE_TTS_VOICE: vi-VN-HoaiMyNeural
      EDGE_TTS_ENABLED: "true"
      GTTS_ENABLED: "true"
    secrets:
      - tts_internal_token
```

No public `ports:`.

---

# 132. TTS Gateway Dockerfile target

Use:

```text
python:3.12-slim
```

- install pinned requirements;
- create non-root user;
- no build tools left;
- no cache pip;
- health endpoint;
- `uvicorn`.

---

# 133. Resource limits

Start conservative:

```text
CPU: small fraction / 1 core cap
RAM: measured after deploy
```

Since no model inference local, expected footprint should be modest, but **measure instead of assuming**.

---

# 134. Cloudflare

No new Tunnel hostname needed.

Browser only talks existing app origin:

```text
/api/v1/voice/...
```

---

# 135. CSP

Audio same origin.

Recommended:

```text
media-src 'self' blob:
```

No need Edge domain in frontend CSP because backend calls Edge.

---

# 136. CORS

No public CORS needed for TTS Gateway.

Go Voice API same-origin.

---

# 137. Backpressure

If many clients request same amount:

cache helps.

Single-flight TTS cache key:

```text
same phrase
→ one synth
→ multiple waiters
```

Good optimization.

---

# 138. TTS single-flight

Implement inside TTS Gateway:

```text
cache miss key K
request A starts synth
request B same K joins
```

Avoid duplicate Edge calls.

---

# 139. Cache key excludes transaction identity

Cache based phrase/voice/settings.

Never transaction ID.

Thus:

```text
500k phrase
```

reusable.

---

# 140. No persistent transaction audio

Do not store:

```text
transactionId.mp3
```

No long-term audio archive.

---

# 141. Polling settings acceptance

- [ ] Editable from Admin.
- [ ] Hot reload.
- [ ] Timezone correct.
- [ ] Multiple windows.
- [ ] Cross-midnight.
- [ ] Weekdays.
- [ ] 5–15s realtime.
- [ ] 3–5m keepalive.
- [ ] Backoff overrides.
- [ ] No restart.

---

# 142. Keepalive acceptance

- [ ] No full History.
- [ ] Session refreshed.
- [ ] Session expiry detected.
- [ ] No retry storm.
- [ ] Actual ACB multi-hour test.

---

# 143. Catch-up acceptance

- [ ] Missed overnight credits inserted.
- [ ] No duplicate.
- [ ] No voice.
- [ ] Webhook policy correct.
- [ ] Realtime resumes after completion.

---

# 144. Filter acceptance

- [ ] Today correct.
- [ ] 7 days correct.
- [ ] custom correct.
- [ ] DB render first.
- [ ] coverage sync only if needed.
- [ ] no global poll mutation.
- [ ] same range coalesced.
- [ ] all not unbounded.

---

# 145. KPI/date acceptance

- [ ] `12/09/2026` canonical correct.
- [ ] KPI sums full range.
- [ ] >100 records still correct.
- [ ] transaction detail old rows works.

---

# 146. QR acceptance

- [ ] Upload static QR.
- [ ] Generate static QR.
- [ ] Persist local.
- [ ] Sticky `Nhận tiền`.
- [ ] Large scan modal.
- [ ] Copy account.
- [ ] Mobile safe-area.
- [ ] Atomic update.

---

# 147. Voice acceptance

- [ ] New REALTIME credit reaches orchestrator.
- [ ] ±30s clock skew does not drop.
- [ ] Old replay does not speak.
- [ ] Catch-up does not speak.
- [ ] Filter sync does not speak.
- [ ] Multi-tab only one speaks.
- [ ] Edge voice Hoài My works.
- [ ] Edge failure falls back gTTS.
- [ ] Browser only used with real vi-VN.
- [ ] Never English fallback.
- [ ] TTS error visible.
- [ ] Audio lock explicit.
- [ ] Disable cancels current/pending.
- [ ] Real transfer verified.

---

# 148. Security acceptance

- [ ] TTS Gateway internal only.
- [ ] Internal token.
- [ ] Non-root container.
- [ ] No arbitrary public text synth.
- [ ] Transaction ID authoritative.
- [ ] Rate limit.
- [ ] Timeouts.
- [ ] Provider whitelist.
- [ ] No TTS secrets in React.
- [ ] No transaction text logs.
- [ ] Description cloud opt-in.
- [ ] CSRF/auth where required.

---

# 149. Failure behavior

## Edge down

```text
gTTS
```

## Edge + gTTS down

```text
Browser vi-VN
```

## Browser vi unavailable

```text
optional chime + clear warning
```

## TTS Gateway down

```text
transactions still realtime visually
```

## ACB down

```text
voice subsystem remains independent
```

---

# 150. Rollback

Feature flags:

```text
VOICE_ONLINE_TTS_ENABLED=true
VOICE_EDGE_ENABLED=true
VOICE_GTTS_ENABLED=true
VOICE_BROWSER_FALLBACK_ENABLED=true
```

If new TTS problematic:

```text
VOICE_ONLINE_TTS_ENABLED=false
```

Fallback current browser behavior, but strict vi only.

Monitoring remains unaffected.

---

# 151. Production defaults

```text
Edge voice:
vi-VN-HoaiMyNeural

Edge retries:
1 retry

Fallback:
gTTS

Browser fallback:
vi-VN only

Description:
OFF

Replay max:
120s

Future clock skew:
30s

Burst:
750ms

Queue max:
5
```

Polling:

```text
07:00–23:00 REALTIME 5–15s
23:00–07:00 KEEPALIVE 180–300s
```

---

# 152. Real-world smoke procedure

1. Deploy V5.
2. Verify TTS Gateway `/health`.
3. Open Transaction Viewer.
4. Open voice settings.
5. Select Hoài My.
6. Click `Nghe thử`.
7. Confirm Vietnamese natural speech.
8. Enable incoming voice.
9. Send a small real ACB transfer.
10. Record time ACB received.
11. Record gateway detection.
12. Confirm transaction UI appears immediately after detection.
13. Confirm only one voice plays.
14. Confirm phrase amount correct.
15. Open second tab and repeat.
16. Confirm still only one voice.
17. Force Edge provider failure.
18. Confirm gTTS fallback.
19. Force both online providers fail.
20. Confirm no US voice is used.
21. Verify error/chime behavior.

---

# 153. Metrics to inspect after rollout

For first 24–48h:

```text
ACB poll success/error
429 count
session expiry
keepalive success
catch-up duration
SSE reconnects
voice eligible count
voice played count
voice skipped count
Edge success %
gTTS fallback %
TTS p50/p95 latency
audio errors
multi-tab duplicate attempts
```

---

# 154. Success target

The target is not:

```text
100% dependency uptime from unofficial Edge/gTTS
```

because those services are external/unofficial.

The target is:

```text
high availability through layered fallback
+
clear diagnostics
+
no silent failure
+
no impact on ACB monitoring
```

---

# 155. Final target flow

```text
REALTIME WINDOW
ACB poll every 5–15s
↓
new credit
↓
SQLite
↓
event_journal
↓
SSE
↓
leader tab
↓
freshness/dedupe/source check
↓
POST /voice/transactions/:id
↓
Go builds Vietnamese phrase
↓
TTS Gateway
├── Edge Hoài My
└── gTTS fallback
↓
MP3 bytes
↓
browser audio
↓
"Bạn vừa nhận được năm trăm nghìn đồng."
```

Outside hours:

```text
KEEPALIVE 3–5m
↓
no history
↓
no voice
```

Morning:

```text
CATCH_UP
↓
DB update
↓
no voice
↓
REALTIME
```

---

# 156. Checklist triển khai theo thứ tự

```text
BASELINE
[ ] checkout f0fae614
[ ] backup DB
[ ] go test ./...
[ ] frontend tests
[ ] build
[ ] V3 E2E

VOICE P0
[ ] clock skew tolerance
[ ] stable realtime subscribe
[ ] multi-tab leader initial false
[ ] dedupe reservation
[ ] strict browser vi
[ ] voiceschanged
[ ] diagnostics

TTS GATEWAY
[ ] Python service
[ ] pin edge-tts 7.2.8
[ ] pin gTTS
[ ] Edge provider
[ ] gTTS provider
[ ] timeout
[ ] fallback
[ ] circuit breaker
[ ] cache
[ ] single-flight
[ ] internal token
[ ] health

GO VOICE API
[ ] TTS client
[ ] transaction audio endpoint
[ ] fixed test endpoint
[ ] Vietnamese amount words
[ ] phrase builder
[ ] auth
[ ] rate limit
[ ] privacy rule

FRONTEND AUDIO
[ ] transaction audio engine
[ ] audio playback
[ ] autoplay unlock
[ ] cancel
[ ] queue
[ ] burst
[ ] statuses
[ ] test button
[ ] privacy warning

TEST
[ ] Edge success mock
[ ] gTTS fallback
[ ] browser vi fallback
[ ] no English fallback
[ ] skew
[ ] replay
[ ] multi-tab
[ ] catch-up
[ ] filter sync
[ ] audio lock
[ ] real SSE
[ ] real ACB transfer

V3 VERIFY
[ ] schedule
[ ] keepalive
[ ] catch-up
[ ] filters
[ ] KPI
[ ] date
[ ] detail
[ ] QR
```

---

# 157. Definition of Done

V5 chỉ được coi là hoàn tất khi:

```text
1. Một giao dịch tiền vào thật được backend phát hiện.
2. UI cập nhật qua SSE.
3. Chỉ một tab yêu cầu audio.
4. Backend tạo phrase từ dữ liệu DB authoritative.
5. Edge TTS đọc đúng giọng Việt.
6. Nếu Edge fail, gTTS hoạt động.
7. Không bao giờ đọc tiếng Việt bằng en-US.
8. Clock skew không làm mất voice mới.
9. Catch-up/history sync không spam voice.
10. TTS failure không ảnh hưởng monitor ACB.
11. Admin chỉnh được polling schedule.
12. Off-hours keepalive hoạt động.
13. Morning catch-up không mất giao dịch.
14. KPI/filter/date/detail đúng.
15. QR tĩnh hoạt động.
16. Toàn bộ critical flow có automated tests + real smoke test.
```

---

# 158. Nguồn research kỹ thuật chính

- `rany2/edge-tts`: Microsoft Edge online TTS từ Python, không cần Edge/Windows/API key; hỗ trợ stream và prosody controls.
- edge-tts release được xác minh mới nhất: `7.2.8` ngày 22/03/2026.
- Microsoft Speech language support: `vi-VN-HoaiMyNeural`, `vi-VN-NamMinhNeural`.
- `pndurette/gTTS`: Google Translate TTS, output MP3/file-like; project cảnh báo undocumented upstream có thể breaking.
- Browser Web Speech: chỉ giữ fallback khi `vi-VN` thực sự có.
- Piper/OHF-Voice: giữ optional offline fallback, không chạy mặc định V5.

---

# 159. Kết luận

V5 nên tối ưu theo đúng nhu cầu hiện tại thay vì over-engineer:

```text
Không cần model TTS chạy trên VPS.
```

Dùng:

```text
Edge TTS primary
→ gTTS fallback
→ Browser vi-VN fallback
```

với một `tts-gateway` Python rất nhẹ làm adapter nội bộ.

Phần quan trọng hơn bản thân thư viện TTS là:

- transaction-authoritative voice API;
- không public arbitrary TTS;
- clock-skew fix;
- dedupe đúng lifecycle;
- leader-only synthesis;
- audio autoplay state;
- circuit breaker;
- fallback;
- diagnostics;
- privacy;
- không silent failure.

Kiến trúc này giữ VPS nhẹ, giọng Việt tự nhiên, không cần API key cho primary/fallback online, đồng thời vẫn có đường nâng cấp lên Piper offline hoặc Azure chính thức về sau mà không phải redesign frontend.
