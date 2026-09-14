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

Mình đã audit **source mới nhất trên GitHub**, gồm phần Traefik, Blue/Green, worker split, deployment scripts, CI/CD, unified failover controller và các commit mới nhất. Kết luận là:

> **Kiến trúc mới đi đúng hướng và tốt hơn rất nhiều, nhưng hiện tại CHƯA thể gọi là hoàn hảo và mình chưa khuyên dùng workflow deploy hiện tại cho production.**

Phần thiết kế mình đánh giá khoảng **8/10**, nhưng implementation end-to-end hiện tại khoảng **6/10** vì còn vài lỗi P0 có thể làm deploy fail hoặc failover sai.

## Những phần đã làm đúng

Worker separation đã được triển khai thật chứ không còn nằm trên plan. Khi `WORKER_RPC_URL` tồn tại, gateway chạy HTTP-only, không giữ `gateway.lock`, còn ACB monitor/dispatcher/session verification chuyển sang `acb-worker`. Worker giữ exclusive lock nên tránh hai ACB poller cùng hoạt động. Đây là thay đổi quan trọng nhất và đang đi đúng kiến trúc.

Blue và Green cũng đã xuất hiện thật trong `compose.prod.yaml`, có network alias riêng, Traefik labels, healthcheck metadata và các label `platform.failover.*`. Worker được đánh `singleton`, tức là về ý tưởng đã tách đúng stateless HTTP khỏi stateful poller.

Traefik edge cũng khá tốt về security. Traefik không mount Docker socket trực tiếp mà đi qua `docker-socket-proxy`; Docker provider có `exposedByDefault: false`; access log loại `Authorization`, `Cookie`, Cloudflare Access JWT và worker token. Cloudflared và Traefik cũng được tách network khá rõ.

Unified failover controller cũng đã chuyển đúng từ kiểu “mỗi app một timer” sang event-driven Docker events + 60-second reconciliation. Đây là đúng hướng để sau này quản nhiều app trên một VPS.

Nhưng các phần trên hiện chưa nối với nhau hoàn chỉnh.

---

# P0 — CI/CD hiện tại đang gọi sai deployment path

Đây là vấn đề lớn nhất.

Workflow mới build:

```text
gateway
worker
dbtool
auth-browser
tts-gateway
```

nhưng job deploy cuối cùng **không gọi `deploy-warm.sh`**. Nó vẫn chạy:

```bash
./deploy.sh "$IMAGE" "$BROWSER_IMAGE" "$TTS_IMAGE" "$staged_compose"
```

và thậm chí không truyền worker image hoặc dbtool image xuống VPS.

Trong khi `deploy.sh` cũ vẫn tìm service:

```bash
pull_targets=(gateway auth-browser tts-gateway)
```

Nhưng Compose mới **không còn service `gateway`**. Nó có:

```text
gateway-blue
gateway-green
worker
...
```

Nghĩa là hiện tại:

```text
GitHub Actions
      ↓
new Compose
      ↓
old deploy.sh
      ↓
expects "gateway"
      ↓
❌
```

Và `deploy.sh` cũ vẫn còn:

```bash
docker stop acb-transaction-gateway acb-auth-browser
```

Đây chính xác là hành vi mình muốn loại bỏ khi chuyển Blue/Green.

### Phải đổi thành

```text
GitHub Actions
      ↓
component change detection
      ↓
Gateway changed?
      ↓
deploy-warm.sh

Worker changed?
      ↓
deploy-worker.sh

Auth Browser changed?
      ↓
safe browser deploy

TTS changed?
      ↓
independent TTS deploy
```

Không còn một `deploy.sh` monolithic restart mọi thứ.

---

# P0 — migration path hiện đang nguy hiểm

Gateway mới mở database bằng:

```go
storage.OpenRuntime(...)
```

tức là **không chạy migration**.

Nhưng `cmd/gateway/main.go` sau đó vẫn có:

```go
if *migrateOnly {
    logger.Info("database migrations applied successfully")
    return
}
```

Tức là nếu gọi:

```bash
/gateway --migrate-only
```

nó có thể báo:

```text
database migrations applied successfully
```

trong khi `OpenRuntime()` vừa được định nghĩa là:

```text
RunMigrations: false
```

Đây là **false-success rất nguy hiểm**.

Trong khi đó repo đã có `dbtool` mới đúng cách:

```go
storage.OpenWithOptions(
    ...,
    OpenOptions{RunMigrations: true},
)
```

Nhưng production workflow chưa dùng nó.

Cần bỏ migration khỏi `/gateway` hoàn toàn và chỉ:

```text
dbtool backup
dbtool migrate
dbtool check
```

được phép đụng schema.

---

# P0 — Failover Controller chưa hiểu “active” và “standby”

Đây là lỗi logic rất quan trọng.

Cả Blue:

```text
platform.failover.enabled=true
platform.failover.slot=blue
```

và Green:

```text
platform.failover.enabled=true
platform.failover.slot=green
```

đều được controller monitor.

Nhưng controller chỉ thấy:

```python
if action in ("die", "oom") or
   action.startswith("health_status: unhealthy"):
    handle_failover(...)
```

Nó **không kiểm tra container vừa chết có phải active production slot hay chỉ là standby đang được stop có chủ ý**.

Ví dụ bình thường:

```text
BLUE v12 PRIMARY ✅

GREEN v11 standby
↓
soak xong
↓
docker stop GREEN
```

Docker phát:

```text
die acb-gateway-green
```

Controller có thể nghĩ:

```text
"Green chết!"
```

rồi:

```text
restart Green
↓
không ổn?
↓
start Blue
↓
switch route
```

Trong khi Green **được stop có chủ ý**.

Đây phải sửa trước production.

---

# Cần có một desired-state registry

Mình sẽ không chỉ dựa vào labels.

Ví dụ:

```text
/var/lib/vps-platform/apps/acb.json
```

chứa:

```json
{
  "activeSlot": "blue",
  "standbySlot": "green",
  "standbyDesired": "stopped",
  "deploymentInProgress": false,
  "activeRelease": "sha256:...",
  "previousRelease": "sha256:..."
}
```

Controller nhận:

```text
GREEN die
```

thì đọc state:

```text
Green = standby
standbyDesired = stopped
```

→

```text
IGNORE ✅
```

Nhưng:

```text
BLUE unhealthy
```

và:

```text
Blue = active
```

→ mới kích hoạt failover.

---

# P0/P1 — Deploy và watchdog có thể đua nhau

Hiện có:

```text
deploy-warm.sh
```

và:

```text
vps-failover-controller.py
```

và:

```text
vps-failover-reconcile.timer
```

đều có thể thao tác container/routing cùng lúc.

Nhưng `deploy-warm.sh` không có shared per-app lock với controller.

Có thể xảy ra:

```text
Deploy process:
start Green
↓
stop/recreate Green

đúng lúc đó

Controller:
nhận die event
↓
thấy Green "failed"
↓
restart/switch
```

Đây là race condition.

Mình muốn:

```text
/run/lock/platform-failover/acb.lock
```

và tất cả:

```text
deploy
rollback
controller
reconcile
```

phải acquire cùng lock trước khi thay đổi state.

---

# Mình còn muốn bỏ reconcile thành process riêng

Hiện architecture là:

```text
vps-failover-controller.service
+
vps-failover-reconcile.timer
```

Tốt hơn nữa là:

```text
ONE process

VPS Failover Controller
├─ Docker event stream
└─ every 60s internal reconcile
```

Như vậy không có:

```text
event controller
      ↕ race
reconcile process
```

và chỉ cần per-app mutex trong cùng controller.

---

# P1 — controller chưa generic thật sự cho nhiều app

Tên là:

```text
VPS Unified Failover Controller
```

nhưng fallback routing code lại hardcode:

```python
old_str = f"acb-web-{other_slot}"
new_str = f"acb-web-{target_slot}"
```

Nếu sau này:

```text
messenger
omniroute
portfolio
```

thì controller vẫn tìm:

```text
acb-web-blue
acb-web-green
```

Đây không phải reusable platform controller.

Nó phải dựa vào labels/config:

```text
platform.failover.app=messenger
platform.failover.service=messenger-web
platform.failover.slot=blue
```

→ tạo:

```text
messenger-web-blue
```

hoặc tốt hơn, đừng cho controller tự string-replace YAML.

Dùng một function duy nhất:

```text
platform-switch app slot
```

để deploy, rollback và controller đều gọi chung.

---

# P1 — failover quá aggressive

Controller hiện làm:

```text
unhealthy
↓
sleep 2s
↓
nếu chưa healthy
↓
docker restart primary
```

Nếu container đang:

```text
health = starting
```

sau một restart tự nhiên của Docker, controller có thể restart nó lần nữa.

Mình muốn state machine:

```text
UNHEALTHY
   ↓
failure #1

wait 3s
   ↓
failure #2

wait 3s
   ↓
failure #3
   ↓
RECOVERY ACTION
```

Và nếu:

```text
container Status=running
health=starting
```

thì:

```text
wait startup grace
```

không restart ngay.

Đúng với logic bạn đề cập trước đó là:

> lỗi 3 lần liên tiếp.

Code hiện tại **chưa thực hiện threshold=3**.

---

# P1 — reconcile cũng chưa đủ thông minh

Nếu reconcile thấy:

```text
0 running containers
```

nó làm:

```python
docker start containers[0]
```

Nhưng nó không biết:

```text
containers[0]
```

là active hay standby.

Và sau khi start nó cũng chưa chắc update Traefik route.

Có thể thành:

```text
Traefik → Blue ❌

Reconcile → starts Green ✅

nhưng
Traefik vẫn → Blue ❌
```

→ public vẫn down.

Reconcile phải kiểm tra **desired state + actual route + actual containers**.

---

# P1 — `deploy-warm.sh` mới chỉ là phiên bản tối thiểu

Nó hiện làm:

```text
start core
start candidate
sleep 3
smoke
switch
```

Và sau cutover chỉ **in ra**:

```text
Old slot remains active during soak period (15 minutes)
```

nhưng không thực sự:

```text
monitor 15 minutes
auto rollback
stop old slot
```

Tức là hiện chưa có:

```text
automatic soak controller
```

Mình muốn:

```text
switch → candidate
      ↓
15-minute soak
      ↓
check every 5s
      │
      ├─ 5xx
      ├─ /readyz
      ├─ restart count
      ├─ worker availability
      └─ SSE health
      ↓
3 consecutive failures?
   /            \
 yes             no
 ↓                ↓
rollback        success
                  ↓
             stop old slot
```

---

# P1 — candidate smoke test hiện quá yếu

`smoke-slot.sh` chỉ:

```text
container running?
↓
docker exec /gateway --healthcheck
```

Mà `/gateway --healthcheck` kiểm tra `/healthz`.

Trong server:

```go
/healthz
→ luôn status ok
```

còn `/readyz` chỉ kiểm tra:

```go
s.store.Health()
```

Nghĩa là candidate có thể:

```text
Gateway process ✅
SQLite ✅

Worker ❌
Auth-browser ❌
worker token sai ❌
RPC ❌
```

vẫn được promote.

Nên có riêng:

```text
/internal/deployz
```

kiểm tra:

```text
Gateway process       ✅
SQLite                ✅
schema compatible     ✅
Worker RPC            ✅
Auth browser           ✅
release SHA           ✅
TTS                    healthy/degraded
```

Smoke deploy dùng `/internal/deployz`.

Traefik health routing vẫn nên dùng `/readyz` nhẹ hơn để không làm cả dashboard biến mất chỉ vì ACB worker đang degraded.

---

# P0 security/functional — worker token có bug

Gateway config đọc token file bằng:

```go
strings.TrimSpace(...)
```

Nhưng worker đọc:

```go
workerToken = string(b)
```

không trim.

Nếu file:

```text
abc123\n
```

thì:

```text
Gateway sends:
abc123

Worker expects:
abc123\n
```

→

```text
401
```

cho mọi RPC.

Ngoài ra RPC middleware hiện là:

```go
if s.token != "" {
    verify token
}
```

Nếu token rỗng:

```text
NO AUTH
```

Production config cũng chưa bắt buộc worker token khi `WORKER_RPC_URL` tồn tại.

Cần sửa thành:

```text
production
+
worker RPC enabled
+
token missing
=
PROCESS REFUSES TO START
```

Worker cũng phải:

```go
strings.TrimSpace()
```

---

# Worker separation đúng, nhưng health chưa đủ

`worker` hiện có:

```text
restart: unless-stopped
```

nhưng Compose không có `healthcheck:` cho worker.

Worker binary đã hỗ trợ:

```text
--healthcheck
```

nên Compose nên gọi nó.

Quan trọng hơn, worker health không nên đồng nghĩa:

```text
ACB AUTH_REQUIRED
=
worker dead
```

Health nên kiểm tra:

```text
process
DB
monitor goroutine heartbeat
dispatcher alive
RPC
```

Còn:

```text
ACB session expired
ACB maintenance
429
```

phải là:

```text
degraded business state
```

không phải container failure.

---

# Realtime đã đúng hướng nhưng vẫn có thêm ~1 giây

Gateway HTTP-only đã có `RunJournalWatcher`, rất tốt.

Nhưng hiện được gọi với:

```go
1*time.Second
```

Watcher poll journal đúng 1 giây một lần.

Với mục tiêu realtime của hệ thống ACB, mình sẽ giảm xuống:

```text
200–250 ms
```

SQLite query ở đây rất nhẹ:

```text
MAX(seq)
```

và chỉ đọc journal mới.

Khi đó:

```text
ACB response
↓
worker ingest
↓
journal
↓
~0–250ms
↓
SSE
↓
browser
```

đẹp hơn đáng kể.

---

# P1 security — systemd controller hiện là root-equivalent

Controller chạy:

```ini
User=opc
Group=docker
```

Thành viên Docker group về thực tế có quyền kiểm soát Docker daemon, tức quyền cực mạnh trên VPS.

Điều này cần thiết nếu controller phải:

```text
docker start
docker restart
```

nhưng phải coi file controller như privileged code.

Mình muốn:

```text
/opt/platform/failover/
owner root:root
mode 0755

controller.py
owner root:root
mode 0755
```

Deploy user/app không được sửa nó.

State không nên nằm:

```text
/tmp/vps-failover
```

như hiện tại.

Nên nằm:

```text
/run/vps-failover
```

cho transient locks hoặc:

```text
/var/lib/vps-platform
```

cho desired state persistent.

---

# Traefik security đang khá tốt nhưng vẫn tối ưu thêm được

Hiện Traefik:

```text
Docker API
    ↓
socket-proxy
    ↓
Traefik
```

thay vì mount socket trực tiếp. Đây là quyết định đúng.

Nhưng middleware:

```yaml
sourceRange:
  - 172.31.250.0/28
```

đang cho phép cả subnet edge, trong khi cloudflared được pin:

```text
172.31.250.2
```

Tốt hơn:

```yaml
sourceRange:
  - 172.31.250.2/32
```

nếu Cloudflared là origin ingress duy nhất.

Access log hiện:

```yaml
headers:
  defaultMode: keep
```

rồi drop vài header.

Security tối đa hơn là:

```text
defaultMode: drop
```

rồi allow những header thật sự cần log.

---

# Production routing hiện không phải 100% Docker labels

Blue và Green có Docker labels, nhưng production hostname:

```text
bank.tuannguyenviet.site
```

vẫn nằm trong:

```text
platform/edge/dynamic/acb.yml
```

và trỏ trực tiếp:

```text
acb-web-blue:8090
```

Mình **không coi đây là lỗi**.

Thực tế hybrid này khá hợp:

```text
Docker Provider
→ discover containers
→ health/service metadata

File Provider
→ production traffic policy
→ active slot
→ middleware
```

Nó tốt hơn cố nhét toàn bộ Blue/Green switching vào labels.

Nhưng nên mô tả đúng là:

> **Traefik Docker discovery + File Provider traffic policy**

chứ chưa phải:

> “mọi thứ tự động hoàn toàn bằng labels”.

---

# Một lỗi bootstrap nữa

`check-host.sh` nói:

```text
network missing
→ will be created automatically on deploy
```

Nhưng search source không thấy code runtime tạo:

```text
acb-core
acb-egress
```

ngoài các tài liệu plan.

Trong Compose chúng lại là:

```yaml
external: true
```

External network **phải tồn tại trước**.

Bootstrap script phải explicit create + verify labels/internal/egress policy.

---

# CI hiện cũng chưa test platform layer

`ci.yml` hiện test:

```text
frontend
Go
TTS
bash syntax
Docker smoke gateway/browser/TTS
```

Nhưng chưa thấy test:

```text
python failover controller
Traefik config validation
Compose production config
Blue/Green state machine
intentional standby stop
controller/deploy race
reconcile recovery
worker image smoke
dbtool restore/migrate
```

Đây là phần nên bổ sung trước khi gọi platform này “production hardened”.

---

# Đánh giá tổng thể

| Khu vực                        |              Hiện tại | Đánh giá                             |
| ------------------------------ | --------------------: | ------------------------------------ |
| Worker separation              |          Đã implement | 🟢 Tốt                               |
| Singleton ACB ownership        |              Có flock | 🟢 Tốt                               |
| SQLite runtime/migration split |                    Có | 🟡 Đúng hướng nhưng deploy chưa dùng |
| Blue/Green containers          |                    Có | 🟢                                   |
| Warm standby scripts           |                    Có | 🟡 Chưa complete                     |
| Traefik Docker discovery       |                    Có | 🟢                                   |
| Socket proxy                   |                    Có | 🟢                                   |
| Cloudflare → Traefik isolation |                   Tốt | 🟢                                   |
| Automatic failover             |                    Có | 🟡 Logic còn lỗi active/standby      |
| Unified multi-app watchdog     |                    Có | 🟡 Chưa generic hoàn toàn            |
| Reconcile safety net           |                    Có | 🟡 Còn race/state issues             |
| Automatic soak rollback        |                  Chưa | 🔴                                   |
| CI → new Blue/Green path       |          **Chưa nối** | 🔴 P0                                |
| Migration production path      | **Sai path hiện tại** | 🔴 P0                                |
| Worker RPC auth                |                Có bug | 🔴 P0                                |
| Component-independent deploy   |       Chưa hoàn chỉnh | 🔴                                   |
| Supply-chain signing/SBOM      |             Chưa thấy | 🟡                                   |
| Egress allowlist thật          |                  Chưa | 🟡                                   |
| Full platform CI tests         |                  Chưa | 🟡                                   |

### Chấm hiện tại

```text
Architecture/design:       ~8/10
Code structure:            ~7.5/10
Security direction:        ~8/10
Blue/Green implementation: ~6.5/10
Failover/watchdog:         ~5.5/10
CI/CD integration:         ~4/10
Production readiness:      ~5–6/10
```

Không phải source tệ. Ngược lại, **10 commit gần đây đã chuyển kiến trúc rất mạnh về đúng hướng**, và repo bây giờ tốt hơn baseline trước nhiều. Nhưng đang ở trạng thái:

```text
major architecture implemented
        ↓
pieces exist
        ↓
integration not finished
```

chứ chưa phải:

```text
production platform completed
```

## Thứ tự mình sẽ sửa

1. **Bỏ legacy deploy path khỏi GitHub Actions**, chuyển hoàn toàn sang component-aware Blue/Green deploy.
2. **Dùng `dbtool` thật sự**, xóa `gateway --migrate-only`.
3. **Fix worker token**: trim, mandatory in production, RPC không bao giờ được chạy auth-less.
4. **Tạo canonical desired-state registry** cho active/standby.
5. **Controller chỉ failover active slot**, ignore intentionally stopped standby.
6. **Gộp event + reconcile thành một controller**, hoặc ít nhất có per-app `flock`.
7. **Thêm failure threshold + startup grace**, không restart khi health=`starting`.
8. **Thêm worker health + `/internal/deployz`**, nâng smoke test.
9. **Implement real automatic soak + rollback + old-slot stop.**
10. **Thêm platform integration tests**, immutable digest enforcement, Traefik validation, Trivy/SBOM/Cosign sau đó.

Sau khi sửa 1–9, mình mới xem kiến trúc này là **production-ready thật sự cho ACB**, và lúc đó nó cũng đủ sạch để lấy làm chuẩn migrate Messenger, OmniRoute, Portfolio và các Compose repo khác.
