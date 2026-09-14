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

Mình đã xem đúng commit mới. GitHub hiện resolve commit đầy đủ là **`64cbe2a08400258c5816b272b549384ab0496f72`**; tức short SHA thực tế là `64cbe2a0` — `64cbe2aa` bạn gửi có vẻ lệch ký tự cuối một chút. Đây đúng commit lớn có message fix auth/VNC + toàn bộ audit issues.

## Kết luận

**Bản này tốt hơn rất nhiều. Phần application logic mình đánh giá khoảng 9/10. Nhưng toàn hệ thống vẫn chưa thể gọi là “hoàn hảo production” vì còn 3 vấn đề đáng sửa trước: deploy TTS artifact, provisioning TTS secret, và pagination/history completeness.**

| Hạng mục                                            | Trạng thái       |
| --------------------------------------------------- | ---------------- |
| SSE subscription race                               | ✅ Đã fix         |
| Voice clock skew/dedupe                             | ✅                |
| Cancel voice → browser fallback                     | ✅                |
| Queue cancel trước commit success                   | ✅                |
| Cross-tab voice settings                            | ✅                |
| Persistent `voiceschanged`                          | ✅                |
| Selected Hoài My/Nam Minh dùng cho transaction thật | ✅                |
| ProviderMode/gTTS fallback backend                  | ✅ phần lớn       |
| QR chỉ nhận REALTIME + fresh + dedupe               | ✅                |
| QR history “hôm nay”                                | ✅                |
| Realtime cache date range                           | ✅                |
| Poll realtime today-only                            | ✅                |
| Catch-up startup/checkpoint 7 ngày                  | ✅                |
| Cross-midnight weekday                              | ✅                |
| Poll interval safety bounds                         | ✅                |
| ACB auth 2067 / resume VNC                          | ✅ cải thiện mạnh |
| TTS internal token code                             | ✅                |
| TTS secret production provisioning                  | 🔴 Chưa          |
| Immutable TTS image deploy                          | 🔴 Chưa          |
| History pagination/completeness                     | 🟠 Chưa          |
| Minimal/zero downtime deploy                        | 🟠 Chưa          |

### 1. SSE race trước đây đã fix đúng

`subscribe` và `onAny` giờ được bọc `useCallback([client])`, nên `lastEventAt/status/watermark` thay đổi sẽ không còn đổi identity subscription nữa. Đây chính là fix mình yêu cầu trước đó.

`RealtimeDomainBridge` vẫn subscribe một lần theo stable callback và giờ optimistic transaction cache còn kiểm tra cả `direction`, `from`, `to` và bỏ qua cache đang có search filter.

=> Case:

```text
poll.completed
→ React rerender
→ unsubscribe
→ credit tới đúng gap
→ mất event
```

về cơ bản đã được loại bỏ.

---

## 2. Voice đã được sửa khá hoàn chỉnh

Provider giờ có:

```text
isLeader = false
storage event cross-tab
persistent voiceschanged
REALTIME only
freshness ± clock skew
reserve → play → commit
```

`TransactionAudioEngine` cũng đã preserve cancellation:

```text
AbortError
VOICE_CANCELLED
→ không Browser fallback
```

và transaction thật đã gửi `voiceId` lên backend.

Queue cũng đã đảo đúng thứ tự:

```ts
await engine.speak()

if cancelled:
    onError
    return

onSuccess()
```

nên không còn trường hợp cancel giữa chừng mà vẫn `dedupe.commit()`.

`apiAudio()` cũng không còn nuốt `AbortError`.

Phần này mình đánh giá **đã giải quyết đúng các bug lớn trước đây**.

---

# 3. ProviderMode đã được implement thật ở backend

Backend giờ đọc:

```text
ProviderMode
EdgeVoice
OnlineFallback
```

và transaction request có thể override bằng hai voice whitelist:

```text
vi-VN-HoaiMyNeural
vi-VN-NamMinhNeural
```

`BROWSER_ONLY` không gọi online TTS, `EDGE_ONLY` tắt gTTS fallback, `ONLINE_AUTO` mới cho phép fallback tùy `OnlineFallback`.

TTS Gateway cũng thực thi:

```text
Edge
↓ fail

if ONLINE_AUTO && allow_fallback
    gTTS

EDGE_ONLY
    error
```

Đây là một improvement lớn so với commit trước.

Có một semantic nhỏ còn lại: phía browser `TransactionAudioEngine` vẫn fallback BrowserSpeech cho hầu hết online errors. Vì vậy `EDGE_ONLY` hiện thực tế có thể là:

```text
Edge only ở server
↓ Edge fail
Browser vi-VN fallback ở client
```

Nếu ý nghĩa của `EDGE_ONLY` là **tuyệt đối chỉ Edge**, thì vẫn cần thêm rule frontend. Nếu nó chỉ có nghĩa “không dùng gTTS”, implementation hiện tại chấp nhận được.

---

# 4. Polling realtime đã chuyển đúng thành today-only

`historyRange()` và `PrepareHistoryFields()` giờ mặc định:

```text
today → today
```

thay vì:

```text
yesterday → today
```

Đây đúng mục tiêu:

```text
5–15s realtime
→ chỉ request hôm nay
→ giảm payload
→ giảm áp lực ACB
```

---

# 5. Catch-up đã tốt hơn rất nhiều

`catchUp()` giờ:

```text
checkpoint CoverageTo
↓
today
```

và clamp tối đa:

```text
7 days
```

Đặc biệt startup cũng đã được xử lý:

```go
lastMode == ""
&& schedule == REALTIME
→ catchUpPending = true
```

nên restart VPS lúc 10h sáng cũng chạy catch-up trước realtime poll.

Sau catch-up còn lưu checkpoint:

```text
CoverageFrom
CoverageTo=today
```

Đây sửa đúng vấn đề mình nêu ở lần audit trước.

---

# 6. Scheduler overnight đã fix đúng

Case:

```text
Monday
23:00 → 02:00
```

tại:

```text
Tuesday 01:00
```

giờ resolver kiểm tra weekday của **ngày hôm qua** cho phần sau midnight.

Backend cũng đã enforce:

```text
REALTIME min >= 5s
REALTIME max <= 300s

KEEPALIVE min >= 60s
KEEPALIVE max <= 1800s
```

Tức không còn phụ thuộc HTML `min=` ở UI để bảo vệ ACB.

---

# 7. QR realtime đã sửa đúng lỗi lớn

QR Modal giờ yêu cầu:

```text
source == REALTIME
```

rồi:

```text
detectedAt <= 120s
```

và:

```text
detectedAt >= sessionOpenedAt - 5s
```

cộng thêm:

```text
transactionId dedupe
```

History cũng query đúng:

```text
from=today
to=today
direction=credit
```

=> Bug:

```text
morning CATCH_UP
→ QR modal báo "vừa nhận tiền"
```

đã được xử lý.

---

# 8. ACB auth/VNC 2067 được fix khá tốt

Backend mới có:

```http
GET /connection/auth/current
```

để recover phiên đang hoạt động.

`startAuth()` nếu tìm thấy existing attempt và browser session còn sống thì **resume** thay vì tạo attempt mới.

Storage cũng:

* expire stale attempt;
* bắt unique constraint và map thành `ErrAuthAttemptActive`;
* cleanup stale auth attempts;
* background reaper 30s.

Frontend khi reload trang sẽ gọi current session và mount lại VNC.

Đây là fix đúng hướng cho lỗi:

```text
UNIQUE constraint 2067
```

và:

```text
reload page
→ mất iframe VNC
→ tưởng phải tạo session mới
```

### Nhưng còn một edge case

Trong `startAuth()`:

```go
session, err := s.browser.Status(...)

if err == nil && session alive:
    resume
else:
    FinishAuthAttempt(..., "FAILED")
```

Nghĩa là **một lỗi mạng tạm thời tới auth-browser**, không phải 404, cũng có thể làm DB đánh attempt hiện tại `FAILED`.

Mình sẽ sửa thành:

```text
Status 404
→ mark FAILED

terminal session
→ finish

transient network/5xx
→ KHÔNG finish attempt
→ trả 503/retry
```

Đây là **P1**, không còn là blocker chính như 2067 trước.

---

# 9. Blocker còn lại #1 — TTS image mới vẫn chưa được deploy bằng digest

Commit mới **không sửa `deploy.yml`**.

Workflow hiện tạo:

```bash
tts_image="...@sha256:..."
```

và truyền:

```bash
TTS_IMAGE="$tts_image"
```

vào remote shell.

Nhưng remote chỉ:

```bash
export IMAGE_REF="$IMAGE"
export AUTH_BROWSER_IMAGE_REF="$BROWSER_IMAGE"

./deploy.sh \
  "$IMAGE" \
  "$BROWSER_IMAGE" \
  "$staged_compose"
```

Nó vẫn **không export**:

```bash
TTS_GATEWAY_IMAGE_REF="$TTS_IMAGE"
```

và cũng không truyền TTS digest làm argument.

`deploy.sh` thì chỉ nhận TTS image từ `$3/$4` hoặc `TTS_GATEWAY_IMAGE_REF`.

Vậy hiện vẫn có thể:

```text
build TTS image mới ✅
push GHCR ✅

deploy
↓
tts_image_ref = ""
↓
không pull TTS digest mới
↓
container TTS cũ tiếp tục chạy
```

**Đây vẫn là P0.**

Fix:

```bash
export TTS_GATEWAY_IMAGE_REF="$TTS_IMAGE"

timeout 300 ./deploy.sh \
  "$IMAGE" \
  "$BROWSER_IMAGE" \
  "$TTS_IMAGE" \
  "$staged_compose"
```

Tốt hơn nữa là bỏ positional parsing mơ hồ và bắt đủ 3 digest.

---

# 10. Blocker #2 — secret TTS mới chưa được deploy script tạo

Compose mới bắt cả gateway và TTS mount:

```text
./secrets/tts_internal_token
→ /run/secrets/tts_internal_token
```

và TTS đặt:

```text
TTS_REQUIRE_AUTH=true
```

Nhưng `deploy.sh` hiện chỉ tự tạo:

```text
secrets/app_master_key
```

Không có đoạn nào tạo:

```text
secrets/tts_internal_token
```

Trong TTS Gateway:

```python
if require_auth and no token:
    HTTP 500
```

Gateway thì nếu file không đọc được sẽ để token rỗng.

Do đó production deploy phải bổ sung:

```bash
tts_token_file="$script_dir/secrets/tts_internal_token"

if [[ ! -f "$tts_token_file" ]]; then
    umask 077
    openssl rand -hex 32 > "$tts_token_file"
fi

chmod 600 "$tts_token_file"
```

Và nên production config **fail fast** nếu TTS token thiếu.

---

# 11. History pagination vẫn chưa được giải quyết

Đây là lỗi application lớn nhất còn sót.

`EnsureHistory()` hiện vẫn:

```text
Bootstrap
↓
History() đúng 1 lần
↓
ParseHistory()
↓
Ingest
↓
RecordCoverage toàn range
```

`catchUp()` cũng:

```text
History() đúng một lần
↓
ParseHistory()
↓
SaveCheckpoint CoverageTo=today
```

Normal polling source vẫn cho thấy logic:

```text
poll.Pages = 1
```

Nếu ACB History endpoint có pagination hoặc giới hạn row:

```text
range 7 ngày
↓
ACB page 1 có 50/100 rows
↓
repo parse 50/100
↓
RecordCoverage COMPLETE 7 ngày
↓
SaveCheckpoint to today
↓
50/100 còn lại bị coi như đã sync
```

Đây là **P1/P0 tùy ACB thực tế có phân trang hay không**.

Trước khi gọi hệ thống “không mất giao dịch”, phải hoặc:

* implement pagination thật;
* hoặc chứng minh qua network/response ACB rằng endpoint trả **toàn bộ rows của range**.

Hiện source chưa có safeguard.

---

# 12. Deploy vẫn chưa minimal/zero downtime

`deploy.sh` vẫn:

```bash
docker stop \
  acb-transaction-gateway \
  acb-auth-browser
```

**trước khi**:

```text
backup
pull images
migration
DB check
compose up
```

Tức mục tiêu zero/minimal downtime bạn nói ở các trao đổi trước **chưa được xử lý bởi commit này**.

Thứ tự nên là:

```text
production cũ vẫn chạy
↓
pull tất cả image
↓
validate/smoke

sau đó mới vào critical section:
pause writer
→ checkpoint
→ backup
→ migration
→ switch
→ health
```

Không nên download hàng trăm MB trong lúc ACB polling đã bị stop.

---

# 13. Một vài lỗi nhỏ còn lại

`cancelVoice()` hiện clear `burstBufferRef` nhưng không `dedupe.release()` các item trong burst như handler khi disable.

Case hiếm:

```text
credit event
↓
reserve
↓
đang trong 750ms burst
↓
cancelVoice()
↓
buffer clear
↓
reservation còn
```

Nên release trước khi clear.

Ngoài ra config có:

```text
TTS_CACHE_MAX_BYTES
```

nhưng `main.py` khởi tạo `TTSCache` mà không truyền `config.cache_max_bytes`; hiện vẫn có default 32MB nên **không mất safety**, chỉ là env setting không có tác dụng.

---

# CI của commit này

Workflow run cho chính `64cbe2a0…` đang chạy khi mình kiểm tra; job **Verify test and build** đã success, gateway image và TTS image đã build success, auth-browser smoke cũng đã đi qua ở lần kiểm tra sau, còn deployment job đang tiếp tục.

Nhưng deploy workflow trên `main` vẫn chỉ verify Go/frontend/shell; Python `pytest` + Docker TTS smoke nằm trong workflow CI khác không chạy cho push trực tiếp main. Vì vậy mình vẫn khuyên đưa TTS tests vào deploy gate trước production.

---

## Chốt mức độ hiện tại

Mình sẽ đánh giá:

```text
Application logic:       9.0 / 10
Voice/TTS logic:         9.0 / 10
ACB auth/VNC:            8.5 / 10
Polling/scheduler:       9.0 / 10
Catch-up:                8.5 / 10
QR realtime:             9.0 / 10

History completeness:    7.0 / 10
Deploy correctness:      6.5 / 10
Minimal downtime:        5.0 / 10

Overall production:      ~8.3 / 10
```

**Commit `64cbe2a0` đã sửa gần hết những lỗi code mình chỉ ra trước đó.** Nếu bạn muốn đạt mức mình sẵn sàng gọi là “production hoàn thiện”, mình chỉ còn ưu tiên 4 việc: **fix TTS digest wiring → provision/verify TTS secret → giải quyết/verify ACB history pagination → redesign deploy để pull/build trước khi stop gateway**. Sau đó mới xử lý vài P2 như transient auth-browser status và burst reservation cleanup.
