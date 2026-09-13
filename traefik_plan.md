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

Đổi sang **Traefik** thì mình thấy còn hợp với mục tiêu “1 VPS, nhiều repo Docker Compose” hơn Caddy.

Điểm khác biệt lớn nhất là:

```text
Caddy:
repo mới
→ sửa config Caddy
→ validate
→ reload

Traefik:
repo mới
→ thêm Docker labels
→ docker compose up -d
→ Traefik tự phát hiện
→ route xuất hiện
```

Traefik Docker provider theo dõi Docker API và tự sinh router/service từ container labels. Nên càng nhiều repo/container thì lợi thế càng rõ. ([Traefik Docs][1])

---

# Kiến trúc mình sẽ đổi thành

```text
                         INTERNET
                            │
                      Cloudflare
                            │
                    Cloudflare Tunnel
                            │
                            ▼
                   ┌─────────────────┐
                   │     TRAEFIK     │
                   │ shared edge     │
                   └───────┬─────────┘
                           │
               Docker service discovery
                           │
      ┌────────────────────┼────────────────────┐
      │                    │                    │
      ▼                    ▼                    ▼
 ACB Gateway          Messenger API        Portfolio
 labels               labels               labels
      │
      ├── Blue/Green
      │
      ▼
   ACB Core
      │
  ACB Worker
      │
     ACB
```

Không còn:

```text
/opt/edge/Caddyfile
```

chứa hàng chục route cho từng project nữa.

Mỗi repo tự khai báo route của chính nó.

---

# Ví dụ repo ACB

Trong Compose:

```yaml
services:
  gateway:
    image: ${IMAGE_REF}

    networks:
      - proxy
      - acb_core

    labels:
      - traefik.enable=true

      - traefik.http.routers.acb.rule=Host(`bank.tuannguyenviet.site`)
      - traefik.http.routers.acb.entrypoints=web

      - traefik.http.services.acb.loadbalancer.server.port=8090
```

Traefik thấy container:

```text
acb-gateway
```

và labels:

```text
Host(bank.tuannguyenviet.site)
port 8090
```

nó tự tạo:

```text
Router:
bank.tuannguyenviet.site

        ↓

Service:
acb

        ↓

container:8090
```

Không cần biết IP container.

Container recreate:

```text
172.x.x.5
→ chết

172.x.x.18
→ container mới
```

Traefik tự cập nhật.

---

# Repo thứ hai

Ví dụ Messenger:

```yaml
labels:
  - traefik.enable=true
  - traefik.http.routers.messenger.rule=Host(`messenger.example.com`)
  - traefik.http.services.messenger.loadbalancer.server.port=8080
```

xong.

Traefik tự có:

```text
bank.domain
    ↓
ACB

messenger.domain
    ↓
Messenger

portfolio.domain
    ↓
Portfolio
```

Đây chính là lý do mình thích Traefik hơn Caddy cho VPS của bạn.

---

# Nhưng phải bật `exposedByDefault=false`

Đây gần như bắt buộc.

Traefik config:

```yaml
providers:
  docker:
    exposedByDefault: false
```

Sau đó chỉ container nào có:

```yaml
labels:
  - traefik.enable=true
```

mới được public.

Traefik docs cũng khuyến nghị cơ chế này khi muốn giới hạn service discovery. ([Traefik Docs][2])

Nếu quên `traefik.enable=true`:

```text
container
→ private
```

rất tốt cho security.

---

# DB / Redis / Worker tuyệt đối không có label

Ví dụ:

```yaml
services:

  gateway:
    labels:
      - traefik.enable=true

  acb-worker:
    labels:
      - traefik.enable=false

  redis:
    labels:
      - traefik.enable=false

  database:
    labels:
      - traefik.enable=false
```

Thành:

```text
Internet
  ↓
Traefik
  ↓
Gateway

Worker    X
Redis     X
Database  X
```

---

# Nhưng Traefik có một vấn đề bảo mật rất quan trọng

Để tự detect container, Traefik cần đọc Docker API.

Cách thường thấy:

```yaml
volumes:
  - /var/run/docker.sock:/var/run/docker.sock:ro
```

Mình **không khuyến nghị** kiến trúc production của bạn làm trực tiếp như vậy.

Traefik chính thức cũng cảnh báo Docker API access là vấn đề bảo mật: nếu Traefik bị compromise, attacker có thể tiếp cận Docker daemon/host. ([Traefik Docs][1])

Thay vào đó:

```text
Docker Socket
     │
     ▼
restricted socket proxy
     │
 private management network
     │
     ▼
Traefik
```

Ví dụ:

```text
/var/run/docker.sock
      ↓
docker-api-proxy
      ↓
GET containers
GET networks
GET events

      X POST container
      X create
      X exec
      X delete
```

Traefik docs cũng liệt kê filtered Docker socket proxy/authorization layer như một giải pháp phù hợp. ([Traefik Docs][1])

---

# Kiến trúc security mình chọn

```text
                         Docker Engine
                              │
                       /var/run/docker.sock
                              │
                              ▼
                    docker-socket-proxy
                    READ-ONLY FILTERED
                              │
                    management network
                              │
                              ▼
                         Traefik
```

Traefik:

```text
không mount docker.sock
```

trực tiếp.

---

# Network thì sao?

Có 2 lựa chọn.

## Cách đơn giản

Một network chung:

```text
proxy
```

gồm:

```text
Traefik
ACB gateway
Messenger gateway
Portfolio
OmniRoute frontend
...
```

Mỗi app còn có private network riêng:

```text
proxy

Traefik
├─ acb-gateway
├─ messenger-api
└─ portfolio


acb-core
├─ acb-gateway
├─ acb-worker
└─ ...


messenger-core
├─ messenger-api
├─ redis
└─ worker
```

Ưu điểm:

```text
repo mới
→ join proxy
→ labels
→ done
```

Đây là phương án mình chọn cho bạn nếu ưu tiên automation.

---

# Có giảm isolation không?

Có một chút.

Vì:

```text
ACB Gateway
Messenger Gateway
Portfolio
```

đều chung `proxy`.

Nhưng chỉ các **public-facing containers** nằm đó.

DB/Redis/worker vẫn không nằm chung.

```text
proxy:
Traefik + public frontend/API only
```

vẫn là mức cân bằng rất tốt.

Nếu muốn isolation cực mạnh, mỗi app một network:

```text
proxy-acb
proxy-messenger
proxy-portfolio
```

nhưng lúc đó Traefik phải được connect thêm network mỗi khi onboarding app:

```bash
docker network connect proxy-acb traefik
```

Routing vẫn tự động labels, nhưng onboarding không còn 100% tự động.

Với bạn mình nghiêng về:

> **shared proxy network + private core network per app**

vì đơn giản, dễ chuẩn hóa cho hàng chục repo.

---

# Blue/Green với Traefik

Phần này thú vị hơn Caddy.

Ví dụ:

```text
ACB Blue v10
ACB Green v11
```

cả hai Traefik đều có thể tự discover.

```yaml
blue:
  labels:
    - traefik.enable=true
    - traefik.http.services.acb-blue.loadbalancer.server.port=8090

green:
  labels:
    - traefik.enable=true
    - traefik.http.services.acb-green.loadbalancer.server.port=8090
```

Traefik biết:

```text
acb-blue@docker
acb-green@docker
```

---

# Nhưng có một nuance quan trọng

Traefik có native **Failover Service**, kiểu:

```text
primary
   ↓ failure
fallback
```

nhưng hiện tại failover service này **không định nghĩa hoàn toàn bằng Docker labels**; tài liệu hiện tại ghi nó hỗ trợ File provider và Kubernetes CRD. ([Traefik Docs][3])

Nên kiến trúc đẹp nhất là hybrid:

```text
95% Docker Labels
+
5% Traefik File Provider
```

Không phải quay lại kiểu Caddy config mỗi app.

File provider chỉ chứa những policy platform-level như:

```text
Blue/Green failover
security middleware
default headers
TLS policy
common rate limit
```

Còn:

```text
hostname
container
port
service discovery
health
```

đều từ Docker labels.

Traefik hỗ trợ reference chéo provider bằng:

```text
service-name@docker
middleware-name@file
```

chính thức. ([Traefik Docs][2])

---

# Ví dụ Blue/Green failover đẹp

Docker tự discover:

```text
acb-blue@docker
acb-green@docker
```

Traefik File Provider chỉ có:

```yaml
http:
  services:

    acb-production:
      failover:

        service: acb-blue@docker

        fallback:
          service: acb-green@docker
```

Concept:

```text
                       Traefik

                          │
                    acb-production
                          │
                 ┌────────┴────────┐
                 ↓                 ↓
          acb-blue@docker    acb-green@docker
             PRIMARY           FALLBACK
```

Nếu Blue healthcheck fail:

```text
Blue ❌
```

Traefik:

```text
→ Green
```

Failover service phụ thuộc healthcheck của primary. ([Traefik Docs][3])

---

# Nhưng với Warm Standby của bạn

Bình thường:

```text
BLUE ✅ primary

GREEN ⏹
```

Traefik Docker provider mặc định chỉ dùng container đang running. ([Traefik Docs][4])

Khi deploy:

```text
GREEN v11 START
```

Docker event:

```text
container start
```

Traefik tự discover:

```text
acb-green@docker ✅
```

Smoke test.

OK:

```text
production policy:
Green primary
Blue fallback
```

Sau soak:

```text
Blue stop
```

Traefik tự thấy:

```text
Blue disappeared
```

Không phải xóa IP/config thủ công.

---

# Healthcheck cũng có thể labels

Ví dụ:

```yaml
labels:
  - traefik.http.services.acb-green.loadbalancer.healthcheck.path=/readyz
  - traefik.http.services.acb-green.loadbalancer.healthcheck.interval=3s
  - traefik.http.services.acb-green.loadbalancer.healthcheck.timeout=1s
```

Nếu `/readyz` lỗi:

```text
Traefik
→ backend unhealthy
```

và không route vào đó. Health checks là một phần native của Traefik load-balancer. ([Traefik Docs][5])

---

# Recreate container cực tiện

Đây là chỗ hơn Caddy rõ nhất.

Với Caddy:

```text
container mới
IP thay
```

thường dựa DNS alias hoặc config upstream cố định.

Traefik:

```text
docker event:
container die

docker event:
container start
```

→ Docker provider cập nhật dynamic config.

Không cần:

```text
generate Caddyfile
validate
reload
```

cho case thông thường.

---

# Flow deploy mới

Giả sử:

```text
BLUE v10 = primary
GREEN = off
```

Push v11:

```text
GitHub
   │
   ↓
test
   │
   ↓
build
   │
   ↓
security scan
   │
   ↓
image@sha256
   │
   ↓
VPS
```

Bật Green:

```text
docker compose
→ GREEN v11
```

Traefik tự:

```text
Docker API event
     ↓
discover Green
     ↓
register acb-green
```

Green test:

```text
/healthz ✅
/readyz ✅
smoke ✅
```

Sau đó deployment controller đổi:

```text
PRIMARY:
Blue → Green
```

Traefik reload dynamic config tự động.

Không restart Traefik.

---

# Nếu Green lỗi lúc khởi động

```text
Green ❌
```

Traefik không cho nó nhận production traffic.

Blue:

```text
Blue ✅
```

vẫn chạy.

---

# Nếu Green đang production rồi crash

Nếu Blue còn hot standby:

```text
Traefik
    │
Green ❌
    │
    └────→ Blue ✅
```

Nếu Blue đã stop theo warm standby:

```text
Traefik
→ không còn fallback
```

Docker sẽ thử restart Green.

Watchdog host có thể:

```text
Green fail repeatedly
        ↓
docker start Blue
        ↓
Traefik automatically detects Blue
        ↓
route recovered
```

Đây cũng đẹp hơn vì watchdog **không cần sửa proxy config**.

Nó chỉ cần:

```bash
docker start acb-blue
```

Traefik lo phần còn lại.

---

# Với rất nhiều repo thì workflow cực đẹp

Ví dụ repo mới:

```yaml
services:
  api:
    networks:
      - proxy
      - internal

    labels:
      - traefik.enable=true

      - traefik.http.routers.myapp.rule=Host(`myapp.example.com`)
      - traefik.http.routers.myapp.entrypoints=web

      - traefik.http.services.myapp.loadbalancer.server.port=8080

networks:

  proxy:
    external: true

  internal:
    internal: true
```

Deployment:

```bash
docker compose up -d
```

xong.

Traefik tự có route.

---

# Standard repo sau này sẽ chỉ cần

```text
deploy/
├── compose.prod.yml
├── labels.yml (optional anchors)
├── health/
└── scripts/
```

Mỗi public service theo convention:

```text
traefik.enable=true

traefik.http.routers.<app>.rule
traefik.http.routers.<app>.entrypoints

traefik.http.services.<app>.loadbalancer.server.port
traefik.http.services.<app>.loadbalancer.healthcheck.path
```

Rất dễ convert repo khác.

---

# Dashboard Traefik cũng tiện

Bạn sẽ nhìn được:

```text
Routers
Services
Middlewares
EntryPoints
health/backend
```

nhưng:

```text
Traefik dashboard
```

không được public thẳng.

Cho:

```text
Cloudflare Access
        ↓
traefik-admin.domain
        ↓
dashboard
```

hoặc chỉ local/VPN.

---

# Version

Traefik release hiện tại mình kiểm tra là **v3.7.1**, phát hành ngày 11/05/2026, và bản này có security fix CVE-2026-44774. Nếu triển khai mới mình sẽ target nhánh **3.7.x**, pin image digest thay vì dùng `latest`. ([GitHub][6])

---

# Mình sẽ đổi kiến trúc tổng thể thành

```text
                        Cloudflare
                            │
                         Tunnel
                            │
                            ▼
                       TRAEFIK
                  shared edge gateway
                            │
                     proxy network
          ┌─────────────────┼─────────────────┐
          │                 │                 │
      ACB gateway       Messenger API      Portfolio
       B / G              B / G
          │                 │
      acb-core        messenger-core
          │                 │
      ACB worker         workers
          │
         ACB
```

Docker discovery:

```text
                    Docker Socket
                         │
                         ▼
                restricted API proxy
                         │
                         ▼
                      Traefik
```

---

## So với Caddy plan vừa rồi

|                       | Caddy                  | Traefik                     |
| --------------------- | ---------------------- | --------------------------- |
| Repo mới              | sửa edge config        | **labels**                  |
| Container recreate    | DNS alias              | **auto discover**           |
| Scaling               | phải thiết kế upstream | **auto discover instances** |
| Docker integration    | không native           | **native**                  |
| Health routing        | tốt                    | **rất tốt**                 |
| Nhiều Compose repo    | tốt                    | **rất phù hợp**             |
| Config đơn giản       | **thắng**              | nhiều concept hơn           |
| Docker socket risk    | không cần              | **phải xử lý kỹ**           |
| Blue/Green automation | script/config          | **dễ automate hơn**         |
| Future Swarm          | được                   | **rất hợp**                 |

### Với trường hợp của bạn

Mình sẽ **đổi sang Traefik**.

Không phải vì Traefik nhanh hơn Caddy đáng kể, mà vì architecture của bạn là:

```text
1 VPS
+
nhiều repo
+
nhiều Docker Compose
+
container thay đổi thường xuyên
+
Blue/Green
+
muốn chuẩn chung cho repo mới
```

Đây chính là môi trường mà **Docker provider + labels của Traefik** đem lại lợi ích lớn.

Và mình sẽ sửa standard trước thành:

> **Cloudflare Tunnel → Traefik → Docker label discovery → shared proxy network → private network per app → singleton stateful workers.**

Chỉ giữ File Provider cho các policy nâng cao như **Blue/Green failover và middleware chung**, còn routing hằng ngày để Docker labels tự quản.

[1]: https://doc.traefik.io/traefik/master/reference/install-configuration/providers/docker/?utm_source=chatgpt.com "Traefik Docker Documentation - Traefik"
[2]: https://doc.traefik.io/traefik/reference/install-configuration/providers/overview/?utm_source=chatgpt.com "Traefik Providers Documentation - Traefik"
[3]: https://doc.traefik.io/traefik/master/reference/routing-configuration/http/load-balancing/service/?utm_source=chatgpt.com "Traefik HTTP Services Documentation - Traefik"
[4]: https://doc.traefik.io/traefik/routing/providers/docker/?utm_source=chatgpt.com "Traefik Docker Routing Documentation - Traefik"
[5]: https://doc.traefik.io/traefik/reference/routing-configuration/http/load-balancing/service/?utm_source=chatgpt.com "Traefik HTTP Services Documentation - Traefik"
[6]: https://github.com/traefik/traefik/releases?utm_source=chatgpt.com "Releases · traefik/traefik · GitHub"
