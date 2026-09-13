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

Cách đó **ổn cho ACB nếu chỉ có một app**, nhưng nếu VPS có 10–20 app thì **không nên mỗi app một `systemd timer` riêng**.

Mình sẽ đổi thành **một Failover Controller chung cho cả VPS**.

```text
Docker Engine
    │
    │ events:
    │ die / oom / unhealthy / restart
    ▼
vps-failover-controller.service
    │
    ├── ACB
    ├── Messenger
    ├── OmniRoute
    ├── Portfolio
    └── app khác...
```

Docker có event stream realtime cho `die`, `oom`, `restart`, `health_status`... nên controller không nhất thiết phải polling mỗi 10 giây. ([Docker Documentation][1])

### Mỗi app chỉ cần khai báo labels

Ví dụ ACB Blue:

```yaml
labels:
  - platform.failover.enabled=true
  - platform.failover.app=acb
  - platform.failover.slot=blue
  - platform.failover.peer=acb-gateway-green
  - platform.failover.health=/readyz
  - platform.failover.failure-threshold=3
  - platform.failover.mode=warm
```

Green:

```yaml
labels:
  - platform.failover.enabled=true
  - platform.failover.app=acb
  - platform.failover.slot=green
  - platform.failover.peer=acb-gateway-blue
  - platform.failover.health=/readyz
  - platform.failover.failure-threshold=3
  - platform.failover.mode=warm
```

Repo Messenger sau này chỉ đổi:

```text
platform.failover.app=messenger
```

Không tạo:

```text
acb-health-watchdog.timer
messenger-health-watchdog.timer
omniroute-health-watchdog.timer
portfolio-health-watchdog.timer
...
```

---

## Workflow chuẩn sẽ là 3 tầng

### Tầng 1 — Traefik

Traefik liên tục health-check backend và loại backend unhealthy khỏi routing. Đây là việc Traefik đã hỗ trợ native. ([Traefik Labs Documentation][2])

```text
request
   ↓
Traefik
   ↓
PRIMARY healthy?
 ├─ yes → primary
 └─ no  → không route vào primary
```

### Tầng 2 — Docker restart

Nếu process/container **thoát thật sự**:

```text
container dies
↓
restart: unless-stopped
↓
Docker tự restart
```

Docker restart policy áp dụng khi container exit/terminate. ([Docker Documentation][3])

Nhưng có điểm quan trọng:

```text
container running
nhưng /readyz = unhealthy
```

thì restart policy không phải cơ chế tự chữa chính, vì container chưa exit.

Đây là lúc controller phát huy tác dụng.

### Tầng 3 — VPS Failover Controller

```text
PRIMARY unhealthy
       ↓
Docker health event
       ↓
Failover Controller
       ↓
xác nhận lỗi liên tục
       ↓
thử restart primary
       ↓
không hồi phục?
       ↓
start standby
       ↓
wait /readyz
       ↓
Traefik discover standby
       ↓
traffic phục hồi
```

Traefik Docker provider vốn đã watch Docker changes theo thời gian thực. ([Traefik Labs Documentation][4])

---

# Mình còn muốn đổi `timer 10s` thành event-driven

Cái hiện tại:

```text
every 10 sec
↓
check
↓
fail #1

10 sec
↓
fail #2

10 sec
↓
fail #3
```

chưa tính startup standby:

```text
30s detection
+
container startup
+
healthcheck
+
Traefik update
```

nên đặt mục tiêu:

```text
≤90 giây
```

là an toàn về mặt SLA nhưng **khá chậm** cho một web/API.

Mình thích:

```text
docker events
      ↓
immediate signal
```

hơn.

Ví dụ:

```text
t=0s
container health_status: unhealthy

t≈0s
controller nhận event

t=0–5s
xác minh lỗi / retry

t=5s
start standby

t=7–15s
standby ready

t≈10–20s
Traefik route traffic
```

Mục tiêu thực tế tốt hơn là:

> **Warm standby failover ≤20–30 giây**, không phải 90 giây.

90 giây chỉ nên là hard timeout.

---

# Nhưng vẫn nên có một timer reconciliation

Event-driven cũng có thể bỏ lỡ event, chẳng hạn controller vừa restart.

Cho nên kiến trúc chuẩn:

```text
vps-failover-controller.service
→ chạy liên tục
→ docker events
→ phản ứng nhanh
```

và thêm:

```text
vps-failover-reconcile.timer
→ mỗi 60 giây
```

Timer không trực tiếp failover từng app.

Nó chỉ hỏi:

```text
"trạng thái thực tế có đúng trạng thái mong muốn không?"
```

Ví dụ:

```text
ACB primary expected BLUE
nhưng BLUE gone
GREEN stopped

→ reconcile phát hiện
→ repair
```

Đây là pattern tốt hơn rất nhiều:

```text
Events = fast path
Timer  = safety net
```

---

# Kiến trúc cuối cho nhiều app

```text
                       Docker Engine
                            │
                        events API
                            │
                            ▼
                ┌────────────────────────┐
                │ VPS Failover Controller│
                │      systemd service   │
                └───────────┬────────────┘
                            │
       ┌────────────────────┼────────────────────┐
       │                    │                    │
       ▼                    ▼                    ▼
      ACB                Messenger           OmniRoute
 Blue / Green           Blue / Green        Blue / Green

                            │
                            ▼
                         Traefik
                    Docker provider
                            │
                          Users
```

Một controller quản lý tất cả.

---

## Controller không nên quản lý mọi container giống nhau

Cần phân loại workload.

### Stateless web/API

Có thể tự failover:

```text
API
Frontend
SSE gateway
Portfolio
```

```text
restart
→ standby
→ Traefik
```

### Stateful singleton

Không được tự bật duplicate:

```text
ACB worker
DB
Redis
notification dispatcher
cron worker đặc biệt
```

Ví dụ:

```yaml
labels:
  - platform.failover.class=singleton
```

Controller biết:

```text
không bao giờ start worker #2
```

Đây cực kỳ quan trọng với ACB.

---

# Mình sẽ dùng các class chung

```text
platform.workload.class=http
```

→ có Blue/Green tự động.

```text
platform.workload.class=singleton
```

→ restart cẩn thận, không duplicate.

```text
platform.workload.class=database
```

→ alert, không tự failover bừa.

```text
platform.workload.class=worker
```

→ policy tùy worker.

```text
platform.workload.class=stateless-worker
```

→ có thể restart tự động.

---

# Ví dụ cả VPS sau này

```text
ACB
├─ gateway-blue        HTTP
├─ gateway-green       HTTP
├─ acb-worker          SINGLETON
├─ auth-browser        SINGLETON
└─ SQLite              DATA

Messenger
├─ api-blue            HTTP
├─ api-green           HTTP
├─ worker              WORKER
└─ Redis               DATA

OmniRoute
├─ api-blue            HTTP
├─ api-green           HTTP
└─ browser-sidecar     SINGLETON

Portfolio
├─ blue                HTTP
└─ green               HTTP
```

Nhưng chỉ có:

```text
vps-failover-controller.service

vps-failover-reconcile.timer
```

**2 systemd units cho toàn VPS**, chứ không phải 2 × số app.

---

# Một cải tiến nữa: controller không cần "switch Traefik" nhiều

Vì Traefik đã watch Docker automatically.

Nếu:

```text
Green stopped
```

controller:

```text
docker start Green
```

Traefik nhận Docker event và cập nhật backend tự động. ([Traefik Labs Documentation][4])

Controller chủ yếu quản:

```text
container lifecycle
desired primary slot
failure counters
cooldown
standby startup
rollback state
```

Để Traefik quản:

```text
service discovery
health routing
HTTP routing
load balancing
```

Đúng separation of concerns hơn.

---

## Mình sẽ sửa câu bạn đưa thành

Thay vì:

> Systemd timer `acb-health-watchdog.timer` chạy mỗi 10 giây...

Mình sẽ thiết kế chuẩn thành:

```text
vps-failover-controller.service
```

chạy event-driven cho **toàn bộ VPS**.

Và:

```text
vps-failover-reconcile.timer
```

chạy mỗi khoảng **60 giây** làm safety net.

Mục tiêu:

```text
container exit
→ Docker restart immediately

HTTP unhealthy
→ Traefik ngừng route

primary không hồi phục
→ controller bật warm standby

standby healthy
→ Traefik tự discover

target recovery:
~10–30 giây

hard timeout:
≤90 giây
```

Cách này **gọn hơn, scale tốt hơn và chuẩn hóa được cho mọi repo** so với mỗi app tự có watchdog riêng.

[1]: https://docs.docker.com/reference/cli/docker/system/events/?utm_source=chatgpt.com "docker system events | Docker Docs"
[2]: https://doc.traefik.io/traefik/routing/providers/docker/?utm_source=chatgpt.com "Traefik Docker Routing Documentation - Traefik"
[3]: https://docs.docker.com/reference/cli/docker/container/run?utm_source=chatgpt.com "docker container run | Docker Docs"
[4]: https://doc.traefik.io/traefik/v3.2/reference/install-configuration/providers/docker/?utm_source=chatgpt.com "Traefik Docker Documentation | Traefik | v3.2"
