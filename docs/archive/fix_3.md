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

Mình đã audit lại **HEAD mới nhất hiện tại**. Repo đang ở commit:

`acfc47bf67d08a7740e8021a7c16708500245b53`
`fix: resolve TTS deployment digest wiring, secret provisioning, transient auth errors, and history pagination safeguards`

Đây là commit mới duy nhất sau `64cbe2a0`, và nó **thực sự sửa gần hết các vấn đề mình nêu lần trước**, không chỉ sửa commit message.

### Kết luận hiện tại

**Chưa hoàn hảo 100%.** Phần application logic đã khá sát production, khoảng **9–9.3/10**, nhưng mình vẫn chưa khuyên chốt kiến trúc vì còn **3 lỗi logic đáng sửa và 1 vấn đề deployment lớn**.

| Phần                  |                     Hiện tại |
| --------------------- | ---------------------------: |
| SSE realtime          |                     ✅ Rất ổn |
| Voice/TTS             |                    ✅ ~9.5/10 |
| ACB auth/VNC          |                      ✅ ~9/10 |
| Schedule/keepalive    |                      ✅ ~9/10 |
| Catch-up              |          ⚠️ còn 1 race logic |
| History pagination    | ⚠️ đã có nhưng còn edge case |
| QR realtime           |                            ✅ |
| TTS security          |             ✅ cải thiện mạnh |
| CI                    |         ✅ phần chính đã xanh |
| Deploy reliability    |                           ⚠️ |
| Zero/minimal downtime |                   ❌ chưa đạt |

Commit mới đã sửa đúng TTS digest: workflow giờ truyền cả ba immutable image digest vào `deploy.sh`, script bắt buộc TTS phải là digest và `verify-deployment.sh` kiểm tra image TTS đang chạy có đúng digest hay không.

TTS secret cũng đã tốt hơn rõ rệt. `deploy.sh` tự provision `tts_internal_token`, Go production config fail ngay nếu file thiếu/rỗng, còn Python TTS sidecar cũng fail startup khi `TTS_REQUIRE_AUTH=true` mà không đọc được token. Cache byte limit cũng đã được wire vào runtime thật.

Auth/VNC transient error cũng sửa đúng: nếu `browser.Status()` lỗi mạng/5xx thì attempt hiện tại **không còn bị mark FAILED**, chỉ trả 503 và cho thử lại. Chỉ 404 hoặc session terminal mới kết thúc attempt.

Voice burst cancellation cũng đã đúng hơn: các reservation chưa phát được release khi cancel, disable hoặc unmount, nên không còn tình trạng transaction bị “kẹt dedupe” sau khi người dùng hủy voice.

## Nhưng còn lỗi quan trọng #1: catch-up incomplete không retry đúng

Đây là lỗi mình nghĩ nên sửa ngay.

Trong `catchUp()`:

```go
if !fetchRes.Complete {
    m.catchUpPending = true
    ...
    return nil
}
```

Nhưng vòng `Run()` lại:

```go
if m.catchUpPending {
    if err := m.catchUp(ctx); err != nil {
        continue
    }

    m.catchUpPending = false
}
```

Tức thực tế:

```text
history chưa lấy đủ
↓
catchUpPending = true
↓
catchUp return nil
↓
Run nhận nil
↓
catchUpPending = false   ← ghi đè
↓
REALTIME tiếp tục
```

Comment trong code nói:

> force resync on next cycle

nhưng hiện tại **không force resync next cycle**.

Checkpoint may mắn là chưa advance, nên data không bị đánh dấu complete sai; nhưng nó có thể chỉ được retry khi restart hoặc chuyển mode lần sau.

### Fix đẹp nhất

Cho:

```go
func (m *Monitor) catchUp(...) (complete bool, err error)
```

rồi:

```go
complete, err := m.catchUp(ctx)

if err != nil {
    continue
}

if complete {
    m.catchUpPending = false
}
```

Hoặc đơn giản incomplete trả sentinel:

```go
return ErrCatchUpIncomplete
```

---

## Lỗi #2: pagination `Truncated` có thể false-positive

Parser hiện xác định:

```go
if totalRows > 0 &&
   rowCount > 0 &&
   rowCount < totalRows &&
   !hasNext {
    truncated = true
}
```

Điều này đúng với:

```text
Total = 100
page hiện tại = 20 rows
không thấy next
→ có thể truncated
```

Nhưng giả sử ACB hiển thị **global total trên mọi page**:

```text
page 1: 50/100 + next
page 2: 50/100 + no next
```

thì page cuối có:

```text
rowCount = 50
totalRows = 100
hasNext = false
```

Parser sẽ kết luận:

```text
Truncated = true
```

dù thực tế đã lấy đủ:

```text
50 + 50 = 100
```

`fetchHistoryRange()` lại lập tức xem page đó incomplete.

Test pagination hiện có case 2 page, nhưng final page mock **không chứa global total count**, nên edge case này chưa bị test bắt.

### Cách chuẩn hơn

`ParseHistoryPage()` chỉ nên trả:

```text
HasNext
TotalRows
RowsThisPage
```

Còn quyết định complete phải nằm ở `fetchHistoryRange()`:

```go
if !page.HasNext {
    if page.TotalRows > 0 &&
       len(allTransactions) < page.TotalRows {
        incomplete
    } else {
        complete
    }
}
```

Tức so:

```text
cumulative rows
```

chứ không phải:

```text
current page rows
```

---

## Lỗi #3: realtime poll chỉ fetch tối đa 3 pages rồi vẫn báo success

Normal polling hiện:

```go
for pagesCount < 3
```

và nếu:

```go
nextErr != nil
```

hoặc parse next page lỗi thì code chỉ:

```go
break
```

rồi tiếp tục ingest data đã có và cuối cùng:

```text
poll.Status = SUCCEEDED
```

Nếu một ngày ACB có:

```text
page 1
page 2
page 3
page 4
page 5
```

realtime chỉ lấy:

```text
1 → 3
```

Nếu ACB newest-first thì thường transaction mới vẫn nằm đầu, nên rủi ro thấp hơn. Nhưng với mục tiêu của bạn là **không mất giao dịch**, đây vẫn là assumption không nên có.

Mình sẽ đổi normal realtime sang một trong hai kiểu:

```text
A. fetch cho đến khi gặp transaction đã biết
```

tốt nhất, hoặc:

```text
B. dùng fetchHistoryRange()
   nhưng giới hạn theo time/context
   và mark poll PARTIAL nếu hết page budget
```

Không được:

```text
page 2 lỗi
→ break
→ SUCCEEDED
```

---

# Vấn đề lớn nhất còn lại: deploy vẫn không minimal downtime

Commit mới đã sửa image/secret deployment, nhưng `deploy.sh` vẫn làm thứ tự:

```text
move compose mới
↓
docker stop gateway auth-browser
↓
tạo/check secrets
↓
backup SQLite
↓
docker compose pull 3 images
↓
migration
↓
DB check
↓
docker compose up
```

Đây chưa đúng mục tiêu zero/minimal downtime trước đó của bạn.

Đặc biệt:

```bash
docker stop ...
```

xảy ra **trước**:

```bash
docker compose pull
```

Nếu GHCR chậm 60 giây:

```text
ACB monitor down 60s+
```

Tệ hơn, nếu:

```text
pull image FAILED
```

script:

```bash
exit 1
```

nhưng container cũ đã stop.

Tức:

```text
old production đang chạy
↓
STOP
↓
pull image
↓
registry lỗi
↓
deploy exit
↓
production vẫn DOWN
```

Đây hiện là blocker lớn nhất của deploy architecture.

### Nên đổi thành

```text
OLD PRODUCTION VẪN CHẠY

pull gateway
pull auth-browser
pull tts
verify digests
image smoke test
prepare migration image
validate compose
prepare secrets
↓
tất cả OK

=== critical section ===

pause writer
checkpoint SQLite
backup
migration
switch container
healthcheck
↓
SUCCESS
```

Nếu critical section fail:

```text
automatic restart/rollback old version
```

`deploy.sh` hiện chưa có error trap tự phục hồi old production.

---

## Một security detail mình cũng sẽ sửa

Nếu `chown 1000:1000` secret thất bại, script fallback:

```bash
chmod 644
```

cho cả master key/TTS token.

Với secret thì mình **không thích fallback world-readable**.

Nên:

```text
chown/ACL đúng
→ chmod 600

không làm được
→ DEPLOY FAIL
```

chứ không:

```text
chmod 644
→ tiếp tục
```

Đây là P1 security hardening.

---

## CI hiện tại thì tốt hơn nhiều

Đối với `acfc47bf`, job `Verify test and build` đã **success**, bao gồm frontend build/test, `go test`, `go vet`, Python TTS tests và shell verification. Gateway image đã build success; TTS image đã build và **published-image smoke test cũng success**. Auth-browser smoke test cũng đã chạy success, dù lúc mình truy vấn workflow vẫn đang ở phần post-job cleanup nên toàn workflow chưa kết thúc hẳn.

TTS smoke hiện verify rất đúng phần security:

```text
/health → OK
no token → 401
invalid token → 401
correct token → 200
```

Nhưng smoke đó chưa synthesize audio thật qua Edge/gTTS, nên production smoke bằng một câu Việt vẫn nên giữ riêng.

### Chốt lại

So với lần audit trước:

```text
64cbe2a0: ~8.3/10
acfc47bf: ~9.0/10
```

**App core hiện rất ổn.** Các lỗi SSE/voice/QR/auth/TTS/scheduler lớn mình nêu trước gần như đã được giải quyết. Nhưng mình chưa gọi là “hoàn hảo” cho tới khi sửa **catch-up pending overwrite**, chuyển pagination completeness sang cumulative logic, xử lý realtime pagination partial đúng nghĩa, và quan trọng nhất là **đổi deployment flow để không stop ACB monitor trước khi pull/validate image**.

Nếu bốn điểm đó được sửa đúng, mình nghĩ repo này mới bắt đầu đạt mức **production release candidate ~9.5+/10** thay vì cứ tiếp tục vá nhỏ từng commit.
