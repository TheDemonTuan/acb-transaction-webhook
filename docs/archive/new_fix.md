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

Mình vừa kiểm tra lại **HEAD hiện tại** và **GitHub Actions run mới nhất**. HEAD vẫn là `d6f3137` — `fix(ci): use published Trivy release`; run mới nhất là **#139** và vẫn fail.

## Nguyên nhân Action đang fail

Không phải Docker build hỏng.

Run #139 cho thấy:

| Job                       | Kết quả |
| ------------------------- | ------- |
| Verify frontend/Go/Python | ✅       |
| Failover tests            | ✅       |
| ShellCheck/lint           | ✅       |
| Gateway image             | ✅       |
| Worker image              | ✅       |
| DBTool image              | ✅       |
| Auth-browser image        | ✅       |
| TTS image                 | ✅       |
| **Scan, attest, sign**    | ❌       |
| Deploy VPS                | ⏭️      |

Job fail chính xác ở:

```text
Scan images and generate CycloneDX SBOMs with Trivy
```

Workflow hiện gom cả image do bạn build và image third-party Bark vào cùng một policy:

```bash
trivy image \
  --exit-code 1 \
  --severity HIGH,CRITICAL \
  --ignorefile .trivyignore \
  "$img"
```

và array có cả:

```bash
["bark"]="$BARK_IMAGE_REF"
```

Bark hiện đang pin đúng immutable digest:

```text
ghcr.io/finb/bark-server@
sha256:32d65b07fa835c99b31a396b77727a04ed058377fc2482da3e9dc7397167ffc4
```

và digest đó cũng được third-party allowlist chấp nhận tới `2027-01-01`.

Nhưng Trivy tìm được:

```text
Alpine packages: 4 HIGH
Go binary:      16 HIGH
CRITICAL:        0
--------------------
Total:          20 HIGH
```

nên `--exit-code 1` làm workflow chết ngay tại Bark.

Điểm đáng chú ý nữa: Bash associative array không đảm bảo thứ tự; run này Bark bị scan đầu tiên. Vì vậy **5 first-party images thậm chí chưa được security scan xong**, mặc dù chúng build thành công.

---

# Cách fix Action mình khuyên dùng

Không nên làm:

```yaml
continue-on-error: true
```

cho toàn bộ security scan, cũng không nên tắt Trivy.

Tách rõ:

```text
FIRST PARTY
gateway
worker
dbtool
auth-browser
tts
      ↓
HIGH hoặc CRITICAL
      ↓
BLOCK RELEASE ❌


THIRD PARTY
Bark exact pinned digest
      ↓
CRITICAL
      ↓
BLOCK RELEASE ❌

HIGH
      ↓
report + SBOM + tracked risk
      ↓
không block tạm thời
```

Đây là cách hợp lý nhất để unblock hiện tại mà không làm security gate của code do bạn kiểm soát yếu đi.

### Thay step scan hiện tại bằng

```yaml
      - name: Scan first-party images with Trivy
        env:
          GATEWAY_IMAGE: ${{ needs.build-gateway-image.outputs.image }}@${{ needs.build-gateway-image.outputs.digest }}
          WORKER_IMAGE: ${{ needs.build-worker-image.outputs.image }}@${{ needs.build-worker-image.outputs.digest }}
          DBTOOL_IMAGE: ${{ needs.build-dbtool-image.outputs.image }}@${{ needs.build-dbtool-image.outputs.digest }}
          BROWSER_IMAGE: ${{ needs.build-auth-browser-image.outputs.image }}@${{ needs.build-auth-browser-image.outputs.digest }}
          TTS_IMAGE: ${{ needs.build-tts-gateway-image.outputs.image }}@${{ needs.build-tts-gateway-image.outputs.digest }}
        run: |
          set -euo pipefail

          mkdir -p sbom-artifacts security-reports

          names=(
            gateway
            worker
            dbtool
            auth-browser
            tts-gateway
          )

          images=(
            "$GATEWAY_IMAGE"
            "$WORKER_IMAGE"
            "$DBTOOL_IMAGE"
            "$BROWSER_IMAGE"
            "$TTS_IMAGE"
          )

          failed=0

          for i in "${!names[@]}"; do
            name="${names[$i]}"
            img="${images[$i]}"

            echo "=== First-party scan: $name ==="

            # Always keep a machine-readable report.
            trivy image \
              --scanners vuln \
              --format json \
              --output "security-reports/trivy-${name}.json" \
              "$img"

            # Always generate SBOM.
            trivy image \
              --format cyclonedx \
              --output "sbom-artifacts/sbom-${name}.cdx.json" \
              "$img"

            # First-party policy: any HIGH/CRITICAL blocks release.
            if ! trivy image \
              --scanners vuln \
              --exit-code 1 \
              --severity HIGH,CRITICAL \
              --ignorefile .trivyignore \
              "$img"; then
              failed=1
            fi
          done

          exit "$failed"

      - name: Scan approved third-party Bark image
        env:
          BARK_IMAGE_REF: ${{ vars.BARK_IMAGE_REF || 'ghcr.io/finb/bark-server@sha256:32d65b07fa835c99b31a396b77727a04ed058377fc2482da3e9dc7397167ffc4' }}
        run: |
          set -euo pipefail

          mkdir -p sbom-artifacts security-reports

          echo "=== Third-party scan: Bark ==="

          # Preserve all HIGH/CRITICAL findings for auditing.
          trivy image \
            --scanners vuln \
            --severity HIGH,CRITICAL \
            --format json \
            --output security-reports/trivy-bark.json \
            "$BARK_IMAGE_REF"

          trivy image \
            --format cyclonedx \
            --output sbom-artifacts/sbom-bark.cdx.json \
            "$BARK_IMAGE_REF"

          # Third-party pinned dependency:
          # CRITICAL remains a hard release blocker.
          trivy image \
            --scanners vuln \
            --exit-code 1 \
            --severity CRITICAL \
            "$BARK_IMAGE_REF"

      - name: Upload security artifacts
        if: always()
        uses: actions/upload-artifact@4cec3d8aa04e39d1a68397de0c4cd6fb9dce8ec1
        with:
          name: security-artifacts
          path: |
            sbom-artifacts/
            security-reports/
```

Sau thay đổi này, Bark hiện tại có:

```text
HIGH     → report
CRITICAL → 0
```

nên không chặn pipeline.

Nhưng nếu ngày mai Bark xuất hiện:

```text
CRITICAL → 1
```

release vẫn bị block.

### Bản hardened hơn về lâu dài

Tốt nhất cuối cùng là:

```text
fork/pin Bark source
      ↓
update Go + x/net + x/text
      ↓
updated Alpine/OpenSSL
      ↓
build image của chính bạn
      ↓
Cosign sign
      ↓
HIGH/CRITICAL = hard fail
```

Lúc đó Bark cũng quay lại policy strict giống gateway.

---

# Phần Blue/Green mới đã tiến bộ rất nhiều

Điểm này cần sửa lại so với audit trước: **CI/CD cũ đã được thay rồi**.

Workflow hiện đã truyền đầy đủ:

```text
gateway image
worker image
dbtool image
auth-browser image
tts image
Bark image
```

rồi verify signed release manifest và cuối cùng gọi:

```bash
timeout 900 ./deploy/deploy-warm.sh --upgrade-core "$IMAGE"
```

Đây là đúng.

`deploy-warm.sh` mới cũng đã có transaction khá hoàn chỉnh:

```text
validate
  ↓
lock
  ↓
active/candidate
  ↓
active-auth gate
  ↓
DB backup
  ↓
dbtool migration
  ↓
pull
  ↓
start candidate
  ↓
readiness
  ↓
Traefik switch
  ↓
route ACK
  ↓
SOAK 15 phút
  ↓
auto rollback nếu fail
  ↓
stop old slot
```

Phần migration false-success trước đây cũng đã sửa. Gateway bây giờ **từ chối** `--migrate-only` và yêu cầu dùng DBTool.

Worker RPC token cũng được sửa: production có `WORKER_RPC_URL` mà thiếu token thì application refuse startup.

`/internal/deployz` cũng đã implement thật, kiểm tra DB/schema/worker/auth-browser/TTS/release/slot.

Những phần này mình đánh giá tốt.

---

# Nhưng chưa hoàn hảo: còn một P0 failover

File:

```text
platform/failover/apps.d/acb.json
```

đang khai báo:

```json
"blue": {
  "container_name": "acb-web-blue"
},
"green": {
  "container_name": "acb-web-green"
}
```

Nhưng Compose thật lại là:

```yaml
gateway-blue:
  container_name: acb-gateway-blue

gateway-green:
  container_name: acb-gateway-green
```

`acb-web-blue` / `acb-web-green` chỉ là **Docker network alias dành cho Traefik**.

Trong khi failover controller match event bằng **exact container name**:

```python
if slot_cfg.container_name == container_name:
    ...
```

Do đó hiện tại:

```text
Docker event:
acb-gateway-blue died
        ↓
controller registry expects:
acb-web-blue
        ↓
NO MATCH
        ↓
❌ event-driven failover có thể không chạy
```

### Sửa `acb.json`

```json
{
  "app": "acb",
  "workload_class": "blue_green",
  "cooldown_seconds": 300,
  "max_restarts": 3,
  "health_timeout": 20,
  "switch_cmd": [
    "/usr/local/bin/platform-switch",
    "{app}",
    "{slot}"
  ],
  "slots": {
    "blue": {
      "container_name": "acb-gateway-blue",
      "service": "gateway-blue"
    },
    "green": {
      "container_name": "acb-gateway-green",
      "service": "gateway-green"
    }
  }
}
```

Traefik vẫn giữ:

```text
acb-web-blue
acb-web-green
```

vì đó là DNS alias. Chỉ watchdog phải dùng container name thật.

---

# Auth-browser cũng đang cùng lỗi

Registry hiện:

```json
"container_name": "auth-browser"
```

nhưng Compose:

```yaml
container_name: acb-auth-browser
```

Nên sửa thành:

```json
{
  "app": "auth-browser",
  "workload_class": "singleton",
  "container_name": "acb-auth-browser",
  "max_restarts": 3,
  "cooldown_seconds": 60
}
```

Đây cũng là P0 vì watchdog có thể không thấy auth-browser chết.

Test hiện còn mock `acb-web-blue`, nghĩa là test đang xác nhận chính cái tên sai thay vì xác nhận Compose thực tế.

Mình rất khuyên thêm một CI integration test:

```text
apps.d/*.json
      ↓
extract container_name
      ↓
deploy/compose.prod.yaml
      ↓
every configured container must exist
```

để lỗi kiểu này không quay lại.

---

# Active-auth gate vẫn còn một bug

Bạn đã có DBTool:

```bash
dbtool -active-auth-count
```

và DBTool đọc đúng DB trong named volume.

Nhưng `check_active_auth_gate()` hiện vẫn mặc định query:

```bash
$SCRIPT_DIR/data/gateway.db
```

bằng host `sqlite3`.

Production DB thật lại nằm trong:

```text
bank-event-gateway_gateway_data
```

Vậy gate có khả năng đọc nhầm/nonexistent DB → thấy `0` → cho core deploy dù đang có phiên login ACB.

Nên đổi logic thành:

```bash
auth_json="$(
  docker run --rm \
    --user 1000:1000 \
    -e DATABASE_PATH=/data/gateway.db \
    -v "${DATA_VOLUME_NAME}:/data:ro" \
    "$DBTOOL_IMAGE_REF" \
    -path /data/gateway.db \
    -active-auth-count
)"

active_count="$(
  printf '%s' "$auth_json" |
    jq -r '.activeCount // .count // 0'
)"

if (( active_count > 0 )); then
  log_error "Active authentication session exists: ${active_count}"
  return 1
fi
```

Report DB đã có cả:

```json
{
  "activeCount": ...,
  "count": ...,
  "attempts": [...]
}
```

---

# Candidate promotion chưa dùng hết `/internal/deployz`

Bạn đã viết endpoint rất tốt, nhưng deployment hiện candidate readiness vẫn chủ yếu:

```bash
docker exec acb-gateway-$slot /gateway --healthcheck
```

và `smoke-slot.sh` cũng chỉ dùng `--healthcheck`.

Trong khi `/internal/deployz` mới là thứ biết:

```text
SQLite OK?
schema OK?
worker OK?
auth-browser OK?
TTS?
release đúng?
slot đúng?
```

Do đó flow tốt nhất phải là:

```text
/healthz
   ↓
/readyz
   ↓
/internal/deployz
   ↓
verify:
  status != not_ready
  slot == candidate
  release == expected SHA
   ↓
PROMOTE
```

`/healthz` chỉ nên quyết định process alive.

---

# Worker nên có Docker healthcheck

Worker mới có readiness logic khá tốt, nhưng Compose hiện chưa khai báo Docker `healthcheck:` cho worker.

Thêm:

```yaml
worker:
  # ...

  healthcheck:
    test: ["CMD", "/worker", "--healthcheck"]
    interval: 10s
    timeout: 3s
    retries: 3
    start_period: 15s
```

Như vậy Docker/ops/watchdog biết rõ trạng thái singleton worker.

---

# Trạng thái hiện tại

| Phần                      | Đánh giá                    |
| ------------------------- | --------------------------- |
| Build/test CI             | 🟢 Tốt                      |
| Immutable digest          | 🟢                          |
| DBTool migration          | 🟢                          |
| Gateway/Worker separation | 🟢                          |
| Worker RPC security       | 🟢                          |
| `/internal/deployz`       | 🟢                          |
| Blue/Green deploy         | 🟢 Khá hoàn chỉnh           |
| Auto rollback             | 🟢                          |
| 15m soak                  | 🟢                          |
| Release manifest          | 🟢                          |
| Cosign                    | 🟢                          |
| SBOM                      | 🟢                          |
| Trivy policy              | 🔴 **đang làm Action fail** |
| ACB failover registry     | 🔴 **container name sai**   |
| Auth-browser registry     | 🔴 **container name sai**   |
| Active-auth gate          | 🟠 đọc sai storage path     |
| Candidate deep smoke      | 🟠 chưa dùng deployz        |
| Worker Docker health      | 🟠 thiếu                    |

### Kết luận

So với lần audit trước, code đã tiến từ khoảng **5–6/10 production readiness lên khoảng 8/10**. Rất nhiều P0 trước đã thực sự được sửa.

Nhưng **chưa gọi là hoàn hảo**. Trước khi deploy production, mình sẽ bắt buộc sửa 3 thứ này trước:

```text
1. Trivy Bark policy        ← đang làm Action fail ngay bây giờ
2. Failover container names ← watchdog có thể bỏ lỡ crash thật
3. Active-auth named volume ← bảo vệ session ACB khi core deploy
```

Sau đó mới nâng candidate gate sang `/internal/deployz` và thêm worker healthcheck.

Quan trọng: run #139 bạn đang thấy **không hề deploy VPS**. Nó chết ở security gate; hơn nữa đây là event `push`, trong khi workflow hiện cố tình chỉ cho job deploy chạy khi `workflow_dispatch` với `deploy=true`.

Mình đã thử tạo branch để commit các fix này trực tiếp nhưng GitHub integration hiện không có quyền tạo ref (`403`), nên mình không thể push thay đổi vào repo từ phiên này. Root cause ở trên đã xác định được từ run thật; patch workflow phía trên là phần cần áp dụng đầu tiên để Action đi tiếp.
