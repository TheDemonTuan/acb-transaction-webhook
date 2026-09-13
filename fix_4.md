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

Mình vừa kiểm tra lại **HEAD mới nhất**. Hiện repo ở:

`27abcd6d15866da1413406d4c95fb14a878388a3`

Commit này đã sửa đúng những lỗi mình chỉ ra ở vòng trước: catch-up retry, pagination cumulative, realtime `PARTIAL` và permission secret.

**Tình trạng hiện tại tốt hơn hẳn: application core mình đánh giá khoảng 9.4–9.5/10. Tuy nhiên vẫn chưa nên gọi là hoàn hảo 100%, chủ yếu vì deployment và một edge case pagination.**

### Những phần bây giờ đã ổn

Catch-up incomplete đã được sửa đúng. Trước đây `catchUpPending=true` bị `Run()` ghi đè thành false; giờ incomplete trả `ErrCatchUpIncomplete`, vòng monitor `continue` và giữ pending để retry.

Pagination global-total cũng đã sửa đúng hướng: không còn lấy số row của riêng page cuối so với total, mà monitor gom cumulative rows rồi mới quyết định range complete. Test mới cũng cover đúng case page 1 + page 2 đều hiển thị cùng global total.

Realtime poll giờ không còn giả vờ `SUCCEEDED` nếu page sau lỗi. Nó cho tối đa 5 pages và đánh:

```text
PARTIAL
```

khi fetch/parse page sau lỗi hoặc chạm page budget. Storage đã chấp nhận `PARTIAL`, frontend cũng hiển thị `"Đồng bộ một phần"`, nên chuỗi này hiện consistent.

CI/deploy của chính commit `27abcd6d` cũng đã **completed / success**. Verify frontend + Go + Python TTS đều pass, gateway/TTS/auth-browser images đều build, TTS/auth-browser smoke pass và deploy VPS cũng success.

## Nhưng blocker lớn nhất vẫn còn: deploy downtime

`deploy.sh` vẫn đang làm:

```text
STOP gateway + auth-browser
↓
backup
↓
PULL 3 Docker images
↓
migration
↓
DB check
↓
recreate containers
↓
healthcheck
```

Tức **stop production trước khi tải image**.

Và run production vừa rồi chứng minh vấn đề này rất rõ. Trong log thực tế:

```text
~16:14:04  gateway/auth-browser bị stop
...
pull auth-browser ~375 MB
...
~16:15:14  gateway mới bắt đầu
~16:15:20  gateway healthy
```

Tức khoảng **hơn 1 phút ACB monitor không chạy**.

Đây chính xác là thứ bạn từng muốn tránh:

```text
deploy
→ ACB polling không được chết lâu
→ session càng không nên bị mất
```

Quan trọng hơn, hiện tại nếu:

```text
docker stop old
↓
docker compose pull
↓
GHCR/network lỗi
```

script `exit 1`, nhưng **production cũ đã bị stop**.

Đây vẫn là P0 đối với kiến trúc deployment của repo này.

### Deploy chuẩn nên là

```text
PRODUCTION CŨ VẪN CHẠY

pull gateway image
pull auth-browser image
pull TTS image
validate digests
validate compose
smoke/preflight
prepare secrets
↓
mọi thứ OK

=========== short critical section ===========

pause/stop gateway writer
SQLite checkpoint
backup
migration
switch containers
health check

=========== vài giây ===========

resume
```

Nếu preflight/pull fail:

```text
old production
→ vẫn chạy nguyên
```

Đây là thay đổi mình ưu tiên số 1 bây giờ.

---

## Còn một vấn đề pagination dài hạn

`fetchHistoryRange()` vẫn có:

```text
maxPages = 10
```

Catch-up gọi nó với 10 pages.

Nếu thực tế range có **11+ pages**:

```text
fetch page 1 → 10
↓
page 10 vẫn HasNext
↓
Complete=false
↓
không advance checkpoint
↓
catch-up retry
↓
lại bắt đầu page 1 → 10
↓
repeat forever
```

Vì continuation cursor không được persist giữa các cycle.

Đây là edge case nhưng đối với mục tiêu **“không mất giao dịch”** thì cần sửa.

Mình khuyên không đơn giản tăng từ 10 lên 50, mà chuyển catch-up thành:

```text
checkpoint date
↓
sync từng ngày

09/09:
 page 1 → ... → complete
 → save coverage 09/09
 → advance

10/09:
 page 1 → ...
 → complete
 → advance
```

Và trong một ngày nếu có nhiều page thì có dynamic bound dựa trên `TotalRows`.

Như vậy crash giữa chừng cũng tiếp tục được từ ngày đã hoàn tất gần nhất.

---

## Còn một edge case realtime nhỏ

Realtime hiện có logic:

```go
if pageResult.HasNext &&
   (NextAction != "" || len(NextFields) > 0) {
    // pagination
}
```

Nếu parser biết:

```text
HasNext=true
```

nhưng ACB thay markup khiến:

```text
NextAction=""
NextFields=nil
```

thì nhánh pagination không chạy và hiện có khả năng poll vẫn được coi là success.

`fetchHistoryRange()` đã bảo vệ case này, nhưng normal realtime poll chưa hoàn toàn giống vậy.

Nên thêm ngay:

```go
if pageResult.HasNext &&
   pageResult.NextAction == "" &&
   len(pageResult.NextFields) == 0 {

    isPartial = true
    pollErr = ErrPaginationUnavailable
}
```

Không lớn, nhưng rất rẻ để harden.

---

## Secret security cũng tốt hơn, nhưng helper fallback có vấn đề kỹ thuật nhỏ

Bạn đã bỏ `chmod 644` và ép `0600`, rất đúng.

Nhưng fallback này:

```bash
docker run \
  --entrypoint /bin/sh \
  "$image_ref" \
  -c "chown ..."
```

dùng chính gateway image.

Gateway Dockerfile là:

```dockerfile
FROM gcr.io/distroless/static-debian12:nonroot
USER 1000:1000
ENTRYPOINT ["/gateway"]
```

Distroless image **không có `/bin/sh`**, đồng thời mặc định chạy UID 1000. Vì vậy fallback container này về cơ bản không đáng tin.

Hiện production deploy vừa rồi vẫn thành công vì host-side cleanup/chown đã làm được, nhưng fallback nên đổi thành một root helper image có shell hoặc thực hiện sau khi pull TTS image rồi:

```bash
docker run --rm \
  --user 0:0 \
  -v "$script_dir/secrets:/sec" \
  --entrypoint /bin/sh \
  "$tts_image_ref" \
  -c 'chown 1000:1000 /sec/...'
```

Tốt hơn nữa: VPS deploy user/sudo được cấu hình rõ để không cần trick container này.

---

### Đánh giá hiện tại

| Hạng mục              |       Điểm |
| --------------------- | ---------: |
| ACB realtime logic    |     9.5/10 |
| Catch-up              |       9/10 |
| Pagination            |       9/10 |
| Voice/TTS             |     9.5/10 |
| QR/SSE                |     9.5/10 |
| Auth/VNC              |       9/10 |
| Security              |       9/10 |
| CI/testing            | **9.5/10** |
| Deploy correctness    |       8/10 |
| Zero/minimal downtime |   **5/10** |

**Tóm lại: code app bây giờ mình khá yên tâm. Phần còn chưa “hoàn hảo” không còn nằm nhiều ở React/Go/TTS nữa, mà chủ yếu là deployment architecture.**

Nếu sửa tiếp, mình sẽ **ngưng vá tính năng** và tập trung hẳn một commit vào: **pre-pull/preflight trước khi stop → short critical section → automatic rollback → progressive catch-up >10 pages**. Sau commit đó repo mới thực sự gần mức production hoàn thiện mà bạn đang nhắm tới.
