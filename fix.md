Mình đã audit lại **HEAD mới nhất** của `TheDemonTuan/acb-transaction-webhook`. Hiện `main` đã lên:

`d6f8f11ba169e31e6c865798f7073cf96790bac7` — `feat: implement Plan V5 with Edge TTS, gTTS fallback, and resilient voice engine`. Trước đó còn hai commit redesign QR `f531992` và `67e399f`.

**Kết luận: chưa thể gọi là hoàn hảo/production-complete.** V5 đã triển khai được phần lớn kiến trúc mình yêu cầu, đặc biệt TTS đã tiến bộ rất nhiều, nhưng mình vẫn thấy vài lỗi **P0/P1** cần sửa trước khi tin tưởng chạy 24/7. Mình đánh giá hiện tại khoảng **7.5–8/10**.

| Mức        | Vấn đề mình tìm thấy                                              | Ảnh hưởng                                                                             |
| ---------- | ----------------------------------------------------------------- | ------------------------------------------------------------------------------------- |
| 🔴 P0      | `RealtimeProvider.subscribe` vẫn thay identity theo mỗi SSE event | Có thể miss `bank.transaction.credit`, đúng kiểu “giao dịch có nhưng voice không đọc” |
| 🔴 P0      | Cancel voice có thể kích hoạt Browser fallback                    | Tắt voice nhưng vẫn có khả năng đọc                                                   |
| 🔴 P0      | QR “VỪA NHẬN TRONG PHIÊN” không kiểm tra `source=REALTIME`        | Catch-up/replay có thể bị hiển thị như tiền vừa nhận                                  |
| 🔴 P0      | Catch-up chỉ cố định hôm qua→hôm nay                              | Downtime >1 ngày có thể bỏ sót                                                        |
| 🔴 P0/P1   | History sync chưa pagination nhưng vẫn mark coverage COMPLETE     | Range nhiều giao dịch có nguy cơ được đánh dấu đầy đủ dù chưa lấy hết                 |
| 🟠 P1      | Normal realtime poll vẫn lấy hôm qua→hôm nay                      | Tốn request/data, tăng nguy cơ rate limit                                             |
| 🟠 P1      | `ProviderMode` / `OnlineFallback` lưu DB nhưng chưa thực thi      | UI/config nói một kiểu, runtime chạy một kiểu                                         |
| 🟠 P1      | Voice được chọn khi “Nghe thử” ≠ voice dùng cho transaction thật  | Test Nam Minh nhưng giao dịch vẫn có thể dùng Hoài My                                 |
| 🟠 P1      | Settings voice không sync giữa tabs                               | Bật voice ở tab không leader có thể không có tiếng                                    |
| 🟠 P1      | QR recent history ghi “hôm nay” nhưng API không filter hôm nay    | Nội dung UI sai                                                                       |
| 🟠 P1      | Schedule cross-midnight + weekday có bug                          | Ví dụ T2 23:00–02:00 sai ở 01:00 T3                                                   |
| 🟠 P1      | Backend không enforce lower safety bounds cho polling             | Có thể gửi API cấu hình 1s                                                            |
| 🟠 P1      | TTS internal token chưa được cấu hình trong prod compose          | Defense-in-depth chưa đúng plan                                                       |
| 🟡 P2      | `requirements.lock` không thực sự lock dependencies               | Build chưa reproducible                                                               |
| 🟡 P2      | QR `os.Rename()` bỏ qua error                                     | Có thể DB ghi QR thành công nhưng file thực tế không rename được                      |
| ⚠️ Release | HEAD không có CI status/workflow run mình quan sát được           | Chưa có bằng chứng HEAD đã pass CI                                                    |

## 1. Lỗi đáng sửa nhất: race SSE vẫn còn

Đây là lỗi mình đặc biệt lưu ý vì **nó liên quan trực tiếp tới lỗi voice trước của bạn**.

`RealtimeProvider` hiện vẫn tạo:

```ts
subscribe: (type, listener) =>
  client.subscribe(type, listener)
```

ngay trong `useMemo`, mà `useMemo` phụ thuộc:

```ts
status
lastEventAt
watermark
client
```

Mỗi SSE event lại gọi:

```ts
setLastEventAt(new Date())
```

nên context value đổi và `subscribe` trở thành function mới.

Trong khi `RealtimeDomainBridge` lại:

```ts
useEffect(() => {
   subscribe(...)
   ...
}, [subscribe, queryClient, handleCreditEvent])
```

Tức flow có thể thành:

```text
poll.completed
↓
lastEventAt thay đổi
↓
React render
↓
DomainBridge cleanup subscriptions
↓
resubscribe
↓
bank.transaction.credit tới đúng cửa sổ đó
↓
MISS
```

Backend lại thực sự finish poll trước rồi mới publish `NewEvents`, nên hai event có thể đến sát nhau.

Đây là **P0**.

Fix đúng:

```ts
const subscribe = useCallback(
  <T,>(
    type: RealtimeEventType,
    listener: RealtimeListener<T>
  ) => client.subscribe(type, listener),
  [client]
);

const onAny = useCallback(
  (listener: RealtimeListener<any>) =>
    client.onAny(listener),
  [client]
);
```

Sau đó context dùng hai callback ổn định này.

---

# 2. Cancel voice hiện chưa an toàn

`TransactionAudioEngine.speak()` hiện:

```ts
try {
  await this.speakOnline(message);
  return;
} catch {
  await this.browserFallback.speak(message);
}
```

Nó fallback Browser cho **mọi lỗi** online.

Trong khi `cancel()`:

```ts
this.abortController.abort();
```

Nhưng `apiAudio()` khi fetch bị abort lại catch chung:

```ts
catch {
  throw new Error(
    'Không thể kết nối máy chủ...'
  );
}
```

nên mất luôn thông tin `AbortError`.

Có thể xảy ra:

```text
voice đang request Edge
↓
user tắt voice
↓
AbortController.abort()
↓
apiAudio biến AbortError thành generic network error
↓
TransactionAudioEngine catch
↓
thử BrowserSpeech fallback
↓
vẫn nói
```

Đây là bug logic nghiêm trọng.

Phải phân loại:

```ts
if (isAbortError(error) ||
    error.message === 'VOICE_CANCELLED') {
  throw error;
}

if (isRetryableOnlineTTSError(error)) {
  return browserFallback.speak(...);
}

throw error;
```

Và `apiAudio()` phải preserve:

```ts
if (err instanceof DOMException &&
    err.name === 'AbortError') {
  throw err;
}
```

---

# 3. Queue còn một cancellation race nữa

`VoiceQueue.processNext()` hiện:

```ts
await this.engine.speak(message);
message.onSuccess?.();

if (this.operationId !== currentOp) {
  return;
}
```

Tức nó gọi `onSuccess()` **trước khi kiểm tra operation đã bị cancel chưa**.

Nếu đang playback:

```text
cancel()
↓
source.stop()
↓
onended
↓
playAudioBuffer resolve
↓
engine.speak resolve
↓
onSuccess()
↓
dedupe.commit()
```

Transaction bị đánh dấu “đã đọc” dù thực tế user vừa cancel.

Đổi thứ tự:

```ts
await engine.speak(message);

if (this.operationId !== currentOp) {
  message.onError?.(
    new Error('VOICE_CANCELLED')
  );
  return;
}

message.onSuccess?.();
```

---

# 4. Dedupe mới thì làm đúng hơn nhiều

Phần này mình đánh giá tốt.

V5 đã có:

```text
reserve
commit
release
```

và:

```text
MAX_REPLAY_AGE = 120s
FUTURE_CLOCK_SKEW = 30s
```

Provider cũng đã truyền:

```ts
envelope.receivedAt
```

vào freshness check và chỉ cho:

```ts
data.source === 'REALTIME'
```

Đây là phần **đã triển khai đúng Plan V5**.

---

# 5. Edge TTS → gTTS đã triển khai khá chuẩn

TTS Gateway hiện thực sự có:

```text
Edge TTS
↓ lỗi
gTTS
```

Edge:

```python
edge_tts.Communicate(...)
```

collect audio chunks và timeout 4s.

gTTS:

```python
gTTS(
    text=text,
    lang="vi",
    ...
)
```

và ghi vào `BytesIO`, không tạo file MP3 tạm.

TTS Gateway còn có:

* cache;
* single-flight;
* circuit breaker;
* voice whitelist;
* max text length.

Đây là improvement lớn.

---

# 6. Nhưng `ProviderMode` hiện gần như chỉ được lưu cho đẹp

DB có:

```go
ProviderMode:
  ONLINE_AUTO
  EDGE_ONLY
  BROWSER_ONLY

OnlineFallback bool
EdgeVoice
```

Nhưng runtime transaction audio vẫn:

```go
res, err :=
  s.ttsClient.Synthesize(...{
      Voice: settings.EdgeVoice,
  })
```

TTS Gateway lại luôn:

```text
try Edge
↓
try gTTS
```

Nên:

```text
EDGE_ONLY
```

vẫn có khả năng fallback gTTS.

```text
OnlineFallback=false
```

vẫn fallback.

```text
BROWSER_ONLY
```

frontend vẫn thử online trước.

Đây là **split-brain config**.

Cần implement thật:

```text
ONLINE_AUTO
Edge → gTTS → Browser

EDGE_ONLY
Edge → fail
không gTTS

BROWSER_ONLY
không request server TTS
→ BrowserSpeechEngine

OnlineFallback=false
Edge only
```

---

# 7. Chọn Nam Minh trên UI chưa chắc giao dịch thật dùng Nam Minh

Frontend lưu:

```ts
settings.voiceURI
```

và dropdown có Hoài My/Nam Minh.

`Nghe thử` gửi:

```json
{
  "voiceId": "..."
}
```

nên test voice đúng selection.

Nhưng transaction thật gửi body chỉ:

```json
{
  "includeDescription": true/false
}
```

không gửi `voiceURI`.

Backend transaction lại lấy:

```go
settings.EdgeVoice
```

từ DB.

Nên user có thể:

```text
UI chọn Nam Minh
↓
Nghe thử = Nam Minh

giao dịch thật
↓
Hoài My
```

Cần chọn một source of truth.

Mình khuyên **global Edge voice từ backend settings**, còn frontend không lưu Edge voice local nữa.

---

# 8. Voice multi-tab vẫn chưa hoàn tất

`isLeader=false` ban đầu đã sửa đúng.

Nhưng settings local vẫn dùng:

```text
acb.voice.settings.v1
```

và chỉ đọc localStorage lúc mount.

Không có:

```ts
window.addEventListener('storage', ...)
```

Ví dụ:

```text
Tab A = leader
voice disabled

Tab B = không leader
user bật voice
↓
localStorage đổi

Tab A React state vẫn disabled
↓
transaction tới
↓
leader A return ngay
↓
không voice
```

Đây hoàn toàn có thể xảy ra.

---

# 9. Persistent `voiceschanged` vẫn chưa hoàn tất

Browser fallback đã sửa rất tốt:

```text
chỉ vi / vi-VN
không giọng Mỹ
```

Nhưng `getVoices()` vẫn chỉ listen `voiceschanged` cho đến lần resolve đầu tiên hoặc timeout 500ms, sau đó remove listener.

Provider lại load voice một lần lúc mount.

Nên case browser load Vietnamese voice muộn vẫn có thể không cập nhật dropdown.

Không phải P0 vì online TTS là primary nữa, nhưng vẫn chưa đúng V5 hoàn toàn.

---

# 10. QR “anti-fraud” đang có vấn đề semantic nghiêm trọng

QR modal subscribe:

```ts
subscribe(
  'bank.transaction.credit',
  ...
)
```

và chỉ check:

```ts
creditVal > 0
```

Nó **không check**:

```text
source == REALTIME
detectedAt freshness
transaction dedupe
```

Backend thì CATCH_UP vẫn emit `bank.transaction.credit` với:

```json
"source": "CATCH_UP"
```

Do đó:

```text
mở QR modal
↓
morning catch-up chạy
↓
phát hiện transaction đêm qua
↓
bank.transaction.credit source=CATCH_UP
↓
QR modal nhận
↓
hiển thị
"ĐÃ NHẬN TIỀN"
"VỪA NHẬN TRONG PHIÊN NÀY"
```

trong khi nó **không vừa nhận**.

Với UI đang dùng các câu như:

```text
Xác thực tự động
Đang trực tiếp theo dõi biến động số dư
VỪA NHẬN TRONG PHIÊN NÀY
```

thì đây là **P0 product correctness**.

QR modal ít nhất phải:

```ts
if (d.source !== 'REALTIME') return;

if (!fresh(d.detectedAt)) return;

if (seenTransactionIds.has(d.transactionId))
    return;
```

---

# 11. QR tĩnh không thể gọi là “xác thực khoản thanh toán QR”

Điểm này quan trọng về logic sản phẩm.

QR của bạn là:

```text
QR tĩnh
không amount
không payment reference
không correlation ID
```

Vậy nếu modal QR đang mở và một người khác bất kỳ chuyển tiền vào tài khoản:

```text
100.000đ
```

UI sẽ báo nhận tiền.

Nó không thể chứng minh:

> “người vừa scan QR này đã thanh toán”.

Vì vậy UI nên dùng:

```text
Có tiền vào tài khoản
```

hoặc:

```text
Phát hiện giao dịch tiền vào mới
```

Không nên gọi:

```text
anti-fraud verification
QR payment verified
```

nếu chưa có dynamic reference/session correlation.

---

# 12. QR “Lịch sử hôm nay” thực ra chưa filter hôm nay

Query hiện là:

```ts
fetchTransactions({
  direction: 'credit',
  limit: 20
})
```

Nhưng UI ghi:

```text
Lịch sử nhận tiền gần nhất hôm nay
```

Không có:

```text
from=today
to=today
```

nên 20 giao dịch credit có thể gồm ngày cũ.

Cần sửa query hoặc sửa label.

---

# 13. Realtime cache transaction hiện có bug filter date

`RealtimeDomainBridge` hiện:

```ts
queryClient.setQueriesData(
  {
    queryKey: ['transactions'],
    exact: false
  },
  ...
)
```

Tức **mọi transaction cache** đều được prepend giao dịch realtime.

Ví dụ user đang xem:

```text
01/08/2026 → 07/08/2026
```

hôm nay 12/09 có 500k mới:

```text
500k hôm nay
↓
bị prepend vào cache 01/08–07/08
↓
summary count/incoming cũng bị +1
```

Đây là bug data correctness.

Phải kiểm tra:

```text
transactionDay nằm trong query from/to không?
direction có match không?
search có match không?
```

Hoặc đơn giản và an toàn hơn:

```text
invalidate queries
```

thay vì optimistic patch toàn bộ cache.

---

# 14. Transaction DB/KPI thì phần nền đã làm tốt

Server-side filter hiện đã có:

```text
from
to
direction
query
cursor
limit
```

Summary SQL tính trên toàn range trước pagination.

Detail cũng đã:

```text
GetTransactionByID
```

không còn find trong first 100 rows.

Phần này mình đánh giá **đúng kiến trúc V3**.

---

# 15. Catch-up chưa bảo đảm “không mất giao dịch”

`catchUp()` hiện hard-code:

```go
yesterday
today
```

Ví dụ:

```text
VPS down 3 ngày

last realtime:
09/09

start lại:
12/09
```

Catch-up:

```text
11/09 → 12/09
```

Ngày:

```text
10/09
```

có thể bị bỏ qua.

Plan trước yêu cầu:

```text
from =
date(lastSuccessfulRealtimePollAt)

to =
today

maxAutoCatchup =
7 days
```

Phần này chưa implement.

---

# 16. Startup trong REALTIME cũng chưa force catch-up

Catch-up chỉ set khi:

```go
lastMode == KEEPALIVE_ONLY
||
lastMode == PAUSED
```

rồi mode mới là REALTIME.

Process restart lúc 10:00:

```text
lastMode = zero value
schedule = REALTIME
```

không trigger catch-up.

Hiện normal polling lấy yesterday→today nên phần nào che lỗi này, nhưng khi sửa realtime poll thành today-only thì startup gap sẽ lộ ra.

Nên có startup recovery riêng.

---

# 17. History pagination vẫn chưa có

`EnsureHistory()`:

```text
Bootstrap
↓
History một lần
↓
ParseHistory
↓
RecordCoverage(all days)
```

Không thấy loop:

```text
page1
page2
page3...
```

Nhưng sau một response nó record:

```text
COMPLETE
```

cho toàn bộ range.

Nếu ACB phân trang history thì:

```text
API trả page 1
↓
repo ingest page 1
↓
coverage = COMPLETE
↓
những trang còn lại không được sync lại
```

Đây là một blocker quan trọng trước khi coi history sync đáng tin cậy.

---

# 18. Normal realtime polling vẫn kéo hôm qua → hôm nay

`PrepareHistoryFields()` default vẫn:

```go
fromDate :=
    today.AddDate(0, 0, -1)

toDate :=
    today
```

`Client.historyRange()` cũng vẫn yesterday→today.

Nghĩa là trong khung:

```text
07:00–23:00
mỗi 5–15s
```

backend vẫn liên tục request cả:

```text
hôm qua + hôm nay
```

Thay vì target:

```text
today → today
```

Đây là waste đáng kể và tăng rủi ro ACB rate-limit.

---

# 19. Keepalive thì đã làm đúng

`pollKeepalive()` hiện chỉ:

```text
Restore
Bootstrap
Persist session
```

và comment rõ:

```text
never calls History
```

Đây đúng mục tiêu.

---

# 20. Schedule có bug ở cross-midnight + weekday

Resolver kiểm tra:

```go
dayMatch :=
    current weekday ∈ DaysOfWeek
```

trước khi tính overnight.

Ví dụ:

```text
Monday:
23:00 → 02:00
```

Days:

```text
[Monday]
```

01:00 Tuesday về logic phải vẫn thuộc “Monday night”.

Nhưng resolver:

```text
weekday = Tuesday
Tuesday không trong list
→ skip
```

Test overnight hiện dùng:

```text
DaysOfWeek = tất cả 7 ngày
```

nên không phát hiện bug này.

---

# 21. Poll safety bounds chưa được enforce

Backend validation hiện chỉ yêu cầu:

```text
minSeconds > 0
maxSeconds >= minSeconds
```

Có thể POST trực tiếp:

```json
{
  "mode": "REALTIME",
  "minSeconds": 1,
  "maxSeconds": 1
}
```

UI min=2 không phải security/safety control.

Mình sẽ enforce backend:

```text
REALTIME:
min >= 5
max <= 300

KEEPALIVE:
min >= 60
max <= 1800
```

---

# 22. TTS internal token chưa thực sự triển khai production

Code support:

```text
TTS_INTERNAL_TOKEN
TTS_INTERNAL_TOKEN_FILE
```

Nhưng nếu expected token rỗng:

```python
if not expected:
    return
```

tức authentication bypass.

Prod compose hiện set:

```text
TTS_GATEWAY_URL
```

nhưng không thấy token secret được mount vào gateway/tts-gateway.

Service không public host port nên không phải remote Internet vulnerability trực tiếp, nhưng defense-in-depth trong Plan V5 **chưa hoàn tất**.

---

# 23. Dependency lock chưa thật sự lock

`requirements.lock` có:

```text
fastapi>=
uvicorn>=
pydantic>=
requests>=
httpx>=
```

và Dockerfile lại cài:

```dockerfile
pip install -r requirements.txt
```

Tức build hôm nay và tháng sau có thể lấy versions khác.

Cần:

```text
exact pins
+
hashes
```

hoặc uv/pip-tools lock thực sự.

---

# 24. TTS cache giới hạn theo số item, không theo bytes

Cache:

```text
max_items = 256
```

Container:

```text
mem_limit: 256m
```

Nếu 256 MP3 đều tương đối lớn, cache có thể ăn phần đáng kể RAM.

Nên giới hạn:

```text
maxBytes
```

ví dụ 32–64MB RAM cache.

---

# 25. gTTS timeout chưa phải hard deadline

gTTS chạy trong:

```python
asyncio.to_thread(_run_gtts)
```

Nó truyền timeout vào gTTS/request nhưng không có:

```python
asyncio.wait_for(...)
```

bọc toàn bộ worker.

Không nghiêm trọng bằng các lỗi trên, nhưng nếu network/library treo ngoài dự kiến, coroutine có thể giữ lâu hơn target.

---

# 26. QR atomic rename đang ignore error

Upload:

```go
_ = os.Rename(tempPath, filePath)
```

Generate cũng vậy.

Nếu rename fail:

```text
DB vẫn SavePaymentQR(filePath)
↓
response OK
↓
file thực tế không tồn tại
```

Nên:

```go
if err := os.Rename(...); err != nil {
    os.Remove(tempPath)
    return 500
}
```

---

## Những phần hiện đã làm khá tốt

So với các bản trước, repo đã tiến rất xa. TTS primary đã chuyển sang Edge + gTTS fallback, strict Vietnamese browser fallback đã loại bỏ giọng Mỹ, transaction audio API đã dùng `transactionId` authoritative thay vì public arbitrary text, CSRF được middleware enforce cho POST, canonical dates/server KPI/detail API đã đúng kiến trúc, keepalive không gọi History, history sync đã có coverage + single-flight, Docker TTS đã `read_only`, non-root, drop capabilities, resource limit, và QR backend đã bổ sung MIME validation/path containment.

## Thứ tự mình sẽ fix trước production

**P0 trước:** ổn định `RealtimeProvider.subscribe`; fix cancel/fallback voice; fix QR chỉ nhận REALTIME+fresh+dedupe và bỏ wording “verified” không có correlation; catch-up theo checkpoint thực tế; pagination history trước khi mark coverage COMPLETE.

**Sau đó P1:** realtime ACB today-only, thực thi thật `ProviderMode/OnlineFallback`, thống nhất voice selection frontend/backend, sync local settings giữa tabs, sửa filtered React cache, fix overnight weekday schedule + backend interval bounds, TTS internal token, QR rename error.

**Cuối cùng hardening:** dependency lock thật, cache max bytes, hard timeout gTTS, persistent `voiceschanged`, metrics và real-provider smoke tests.

Vì **HEAD hiện không có CI status/workflow run nào mình quan sát được**, mình cũng chưa coi commit `d6f8f11` là release candidate cho tới khi các P0 trên được sửa và chạy full `go test -race ./...`, frontend unit/build, Python TTS tests, Docker smoke và một giao dịch ACB thật end-to-end.
