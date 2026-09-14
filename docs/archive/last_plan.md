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

Mình đã đọc trực tiếp **run `34772159689`**, full log job fail và đối chiếu với **HEAD mới nhất `414cfa3`**.

## Action `34772159689` bị gì?

**Không phải code app bị fail.** Run này build/test gần như toàn bộ đều qua:

| Phần                       | Kết quả    |
| -------------------------- | ---------- |
| Frontend build/test        | ✅          |
| Go `go test -race ./...`   | ✅          |
| Go vet                     | ✅          |
| TTS tests                  | ✅          |
| Failover controller tests  | ✅          |
| Shell syntax/lint          | ✅          |
| Gateway image              | ✅          |
| Worker image               | ✅          |
| DBTool image               | ✅          |
| Auth-browser image + smoke | ✅          |
| TTS image + smoke          | ✅          |
| Security/signing stage     | ❌          |
| Deploy VPS                 | ⏭️ skipped |

Run chết duy nhất tại:

```text
Scan, attest, and sign release artifacts
    ↓
Install Cosign
    ↓
FAIL
```

Sau đó toàn bộ Trivy/signature/manifest bị skip, nên `Deploy to VPS` cũng bị skip.

### Lỗi gốc trong log

Installer đang chạy:

```text
sigstore/cosign-installer@053f9b...
cosign-release: v2.4.3
```

rồi tải:

```text
https://github.com/sigstore/cosign/releases/download/v2.4.3/cosign-linux-amd64
```

bằng:

```bash
curl -fsL ...
```

và chết ngay:

```text
Process completed with exit code 22
```

`curl` code `22` nghĩa là server trả về HTTP error khi dùng `--fail`.

Điểm quan trọng: **không thể khẳng định là 404**, vì log installer không in HTTP status. Và mình đã kiểm tra official Cosign release: **`v2.4.3` vẫn tồn tại**, release asset cũng tồn tại.

Vì vậy evidence phù hợp nhất là:

```text
GitHub runner
   ↓
cosign-installer
   ↓
GitHub Release/CDN
   ↓
HTTP error tạm thời
   ↓
curl exit 22
```

Có thể là CDN/network/rate/transient GitHub error. Nó **không chứng minh binary/path Cosign bị sai**.

---

# Nhưng Action hiện vẫn chưa được harden

HEAD mới nhất vẫn đang dùng:

```yaml
- name: Install Cosign
  uses: sigstore/cosign-installer@053f9b74638557590800a301da1ba82351507e2c # v3.8.1
```

Trong khi `cosign-installer` mới nhất hiện là **v4.1.2**, phát hành tháng 5/2026, dùng Cosign 3.0.6.

Tag `v4.1.2` hiện trỏ đến commit:

```text
6f9f17788090df1f26f669e9d70d6ae9567deba6
```

Mình sẽ sửa Action theo hướng:

```yaml
- name: Install Cosign
  uses: sigstore/cosign-installer@6f9f17788090df1f26f669e9d70d6ae9567deba6
```

và vẫn **pin SHA**, không dùng floating:

```yaml
@v4
```

Ngoài ra nên có retry cho dependency download. Trivy của bạn đã làm khá đúng:

```bash
curl \
  --fail \
  --location \
  --retry 3
```

nhưng Cosign installer hiện fail một request là cả release chết.

**Không nên bỏ security gate** để chữa lỗi này.

---

# Code mới đã hoàn hảo chưa?

HEAD hiện tại là:

```text
414cfa3  Create codacy.yml
75ec27b  Add Dependabot configuration
c46c724  fix(deploy): ...
```

Hai commit trên cùng chỉ liên quan config/tooling, nên application logic thực tế gần như vẫn là code tại `c46c724`.

Tin tốt là những lỗi lớn mình tìm ở vòng audit trước **đã được sửa thật**.

### Session worker — ✅ đã sửa P0

Trước đây:

```text
handoff
 ↓
restore vào acbClient A
 ↓
verify bằng acbClient B
 ↓
FAIL
```

Commit `d0831e4` đã tách đúng:

```text
pollSessionLoader
  → acbClient

verifierSessionLoader
  → verifierClient
```

Worker hiện dùng đúng `verifierSessionLoader + verifierClient` cùng một client/session state.

Đây là fix rất quan trọng.

### Realtime partial → catch-up — ✅ đã sửa

Commit đó cũng sửa việc realtime chạm page budget rồi bỏ quên các page sau bằng cách đảm bảo partial poll kích hoạt catch-up. Đây là phần rất quan trọng đối với mục tiêu **không bỏ sót tiền vào**.

### Pagination — ✅ đã sửa

Logic hiện tại đã thành:

```go
neededPages := (maxTotalRowsSeen + 9) / 10

if neededPages > 50 {
    neededPages = 50
}

if neededPages > maxPages {
    maxPages = neededPages
}
```

Thay vì lỗi cũ khiến `neededPages > 50` không tăng page budget.

### EventHub race — ✅ đã sửa

`dropped` đã đổi sang `atomic.Uint64`, không còn write dưới `RLock` như trước.

### Realtime SSE — ✅ tốt hơn

Journal watcher đã từ:

```text
1000 ms
```

xuống:

```text
200 ms
```

nên sau khi worker lấy được giao dịch từ ACB, backend → UI chỉ thêm khoảng tối đa ~200 ms do journal watcher trong điều kiện bình thường.

---

# Còn gì chưa hoàn hảo?

| Mức       | Vấn đề                                              | Đánh giá               |
| --------- | --------------------------------------------------- | ---------------------- |
| P0        | Session verifier sai client                         | ✅ FIXED                |
| P0        | PARTIAL poll có thể bỏ page sau                     | ✅ FIXED                |
| P1        | Pagination clamp                                    | ✅ FIXED                |
| P1        | EventHub data race                                  | ✅ FIXED                |
| P2        | SSE watcher 1 giây                                  | ✅ FIXED → 200ms        |
| P2        | Liveness/readiness code                             | ✅ đã tách              |
| P2        | Compose vẫn gọi legacy `--healthcheck`              | ⚠️ còn                 |
| P2        | History RPC synchronous 25s                         | ⚠️ còn                 |
| Security  | Transaction description chưa encrypt thật           | ⚠️ còn                 |
| Hardening | VerifySession generation fence chưa strict equality | ⚠️ còn                 |
| CI        | Cosign download single-point failure                | ❌ đang gây Action fail |

Có ba điểm đáng sửa tiếp.

**Worker healthcheck:** code đã có `--liveness-check` và `--readiness-check`, nhưng production Compose vẫn:

```yaml
healthcheck:
  test: ["CMD", "/worker", "--healthcheck"]
```

Trong khi legacy `--healthcheck` vẫn có logic:

```text
readyz fail
 ↓
thử healthz
 ↓
healthz OK
 ↓
exit 0
```

Mình sẽ đổi Docker thành:

```yaml
healthcheck:
  test: ["CMD", "/worker", "--liveness-check"]
```

và các deployment/readiness probe dùng:

```bash
/worker --readiness-check
```

như vậy semantic sạch hơn.

**History sync:** worker RPC server timeout hiện vẫn `25s`, và `EnsureHistory()` client cũng giới hạn 25s.

Với 31 ngày lịch sử + nhiều page + ACB chậm, 25s có thể hết trước job. Về lâu dài nên:

```text
POST ensure-history
       ↓
202 + jobId
       ↓
worker background job
       ↓
history_sync_jobs
       ↓
SSE progress
```

thay vì giữ RPC lâu.

**Transaction description:** code hiện vẫn search bằng:

```sql
CAST(description_envelope AS TEXT)
```

Nghĩa là tên `description_envelope` hơi misleading: nội dung giao dịch vẫn có thể đọc như plaintext trong SQLite. Với hệ thống ngân hàng, mình ưu tiên encrypt-at-rest hoặc ít nhất phải xác định rõ threat model + encrypt backup/disk.

---

# Kết luận

Nếu **chỉ xét application core**, bỏ phần GitHub Action/deploy ra:

```text
ACB
 ↓
singleton worker
 ↓
session persistence
 ↓
poll / pagination / catch-up
 ↓
generation fence
 ↓
atomic SQLite ingest
 ↓
event journal
 ├─ webhook / Bark
 └─ gateway SSE
       ↓
      UI
```

thì hiện tại mình đánh giá khoảng **9.5–9.7/10**.

Quan trọng nhất: **mình chưa thấy lại P0 rõ ràng nào có khả năng trực tiếp làm mất session hoặc bỏ sót giao dịch như những vòng trước**. Run `34772159689` cũng cho thấy `go test -race`, frontend, TTS, failover tests và tất cả image build đều pass trước khi chết ở bước tải Cosign.

Còn **Action `34772159689` thì nguyên nhân trực tiếp là Cosign installer download bị HTTP failure**, không phải code app. Việc nên làm ngay là **upgrade/pin `cosign-installer` v4.1.2 + làm download resilient**, rồi chạy lại pipeline. Sau đó xử lý 3 phần hardening còn lại: worker health semantics, async history sync, và encryption transaction description.
