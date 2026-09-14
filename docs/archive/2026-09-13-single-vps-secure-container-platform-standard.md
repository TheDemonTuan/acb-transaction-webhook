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

# Single-VPS Secure Container Platform Standard v1

> **Purpose:** Chuẩn kiến trúc dùng lại cho nhiều repository chạy Docker Compose trên **một VPS**, ưu tiên bảo mật, blast-radius nhỏ, zero/near-zero downtime ở cấp ứng dụng, rollback nhanh, tài nguyên thấp và vận hành rõ ràng.
>
> **Status:** Proposed standard, baseline 2026-09-13.
>
> **First adopter:** `TheDemonTuan/acb-transaction-webhook`.

---

## 1. Mục tiêu thiết kế

Platform phải bảo đảm các tính chất sau:

1. Một ứng dụng bị compromise không mặc định nhìn thấy DB/Redis/worker của ứng dụng khác.
2. Chỉ có một ingress chung trên VPS: Cloudflare Tunnel -> shared Caddy.
3. Production app không publish host port nếu traffic đã đi qua Caddy.
4. Service public không được quyền điều khiển Docker daemon.
5. Stateful singleton worker không bị nhân đôi chỉ để đạt zero-downtime cho HTTP.
6. Stateless HTTP/UI có thể deploy theo Active/Standby Blue-Green.
7. Build/test/image lỗi không được tác động release đang active.
8. Runtime instance lỗi phải có lớp tự phục hồi phù hợp với loại workload.
9. Image production dùng immutable digest, có vulnerability gate, SBOM và chữ ký provenance/signature.
10. Secret không nằm trong source, image layer hoặc log; cấp quyền theo từng service.
11. Một container runaway không được ăn hết RAM, CPU, PID hoặc disk của VPS.
12. Backup phải có off-host copy và restore drill; “có file backup” chưa được tính là hoàn thành.
13. Host hardening và app hardening là hai lớp độc lập; không dựa vào một lớp duy nhất.
14. Mọi repo mới phải tuân cùng contract để có thể onboarding bằng checklist thay vì thiết kế lại từ đầu.

---

## 2. Non-goals

Standard v1 **không** cố biến một VPS thành hạ tầng HA cấp máy.

Nếu VPS mất điện, kernel panic, provider mất host hoặc disk chết thì toàn bộ workload trên host đó vẫn có thể down. Muốn HA cấp infrastructure cần tối thiểu node thứ hai, external datastore/replication phù hợp, hoặc chuyển sang orchestrator/multi-node architecture.

Standard v1 cũng không yêu cầu:

- Docker Swarm;
- Kubernetes/K3s;
- service mesh;
- Redis chỉ để phục vụ deployment;
- Caddy Docker socket discovery;
- container `autoheal` có quyền Docker socket;
- một monitoring stack nặng nếu VPS chưa cần.

---

## 3. Workload classes bắt buộc

Mỗi service phải được gán đúng một class trước khi thiết kế deployment.

### W1 — Stateless HTTP/UI/API

Ví dụ:

- frontend;
- REST API;
- SSE/WebSocket gateway;
- read-mostly admin UI.

Mặc định:

```text
Active/Standby Blue-Green
+ Caddy health/failover
+ Docker restart policy
```

Có thể chạy 2 slot đồng thời nếu data layer hỗ trợ multi-process.

### W2 — Singleton stateful worker

Ví dụ:

- bank poller;
- payment watcher;
- scheduler có side effect;
- queue consumer không partition;
- webhook dispatcher singleton.

Mặc định:

```text
exactly-one logical owner
+ exclusive lock/lease
+ preflight
+ graceful drain
+ explicit handoff
```

Không dùng dual-active chỉ vì Blue-Green HTTP đang dùng hai slot.

### W3 — Stateful datastore

Ví dụ:

- SQLite;
- PostgreSQL/MySQL;
- Redis khi được dùng như source/state store;
- persistent Bark data.

Mặc định:

```text
one canonical state
+ backup
+ migration contract
+ restore drill
```

Không Blue-Green database bằng cách mount cùng data volume vào hai server engine nếu engine không hỗ trợ điều đó.

### W4 — Stateful sidecar/browser/runtime session

Ví dụ:

- Chromium login sidecar;
- browser automation host;
- session holder;
- VNC/noVNC sidecar.

Mặc định:

```text
singleton
+ private network
+ persistent session handoff where supported
+ no blind restart during interactive session
```

### W5 — Public stateful auxiliary service

Ví dụ:

- Bark;
- webhook receiver có local state;
- object/file service.

Mặc định:

```text
Caddy ingress
+ singleton unless app supports replication
+ component-specific deploy
+ persistent volume
```

### W6 — One-shot maintenance/job

Ví dụ:

- DB migration;
- backup;
- cleanup;
- reconciliation.

Mặc định:

```text
one-shot container
+ explicit lock
+ bounded timeout
+ no public network
```

---

## 4. Platform topology chuẩn

```text
                               INTERNET
                                  |
                          Cloudflare Edge
                     WAF / Access / DDoS layer
                                  |
                         Cloudflare Tunnel
                                  |
                         shared cloudflared
                                  |
                            shared Caddy
                                  |
              +-------------------+-------------------+
              |                   |                   |
          edge-app-a          edge-app-b          edge-app-c
          internal            internal            internal
              |                   |                   |
       active/standby         app frontend         app frontend
        HTTP slots
              |
        app-a-core
        internal
      +-------+--------+
      |       |        |
    worker   DB      sidecars
      |
 app-a-egress
 non-internal
      |
   Internet destinations actually required by app

                    management plane
                          |
                   Cloudflare Access
                          |
            deploy/metrics/admin endpoints
```

---

## 5. Shared edge rule

VPS chỉ có **một edge stack**, mặc định tại:

```text
/opt/edge
```

Edge stack sở hữu:

```text
cloudflared
Caddy
shared route configuration
edge health checks
```

App repository **không** tự chạy Caddy hoặc cloudflared trong production.

Một app chỉ khai báo:

- hostname;
- edge network;
- stable aliases;
- port nội bộ;
- health path;
- auth classification;
- stream requirements;
- Blue/Green upstream order nếu dùng Active/Standby.

### 5.1 Caddy version policy

Baseline kiểm chứng ngày 2026-09-13:

```text
Caddy v2.11.4
```

Production phải pin image bằng digest sau khi validate trên VPS.

Không chạy `latest` cho shared edge.

### 5.2 Caddy admin API

Caddy admin API:

- không publish ra host/public network;
- không expose qua hostname;
- chỉ dùng nội bộ cho `caddy validate`/`caddy reload` hoặc Unix/container-local endpoint;
- app container không được gọi admin API trực tiếp.

### 5.3 Config reload

Mọi route change production phải:

```text
stage config
-> caddy validate
-> caddy reload
-> probe route
```

Không restart Caddy chỉ để đổi route.

---

## 6. Network micro-segmentation contract

### 6.1 Mỗi app có edge network riêng

Tên chuẩn:

```text
edge-<app-id>
```

Ví dụ:

```text
edge-acb
edge-messenger
edge-omniroute
```

Bootstrap:

```bash
docker network create \
  --internal \
  --label io.tuan.edge.managed=true \
  edge-acb
```

Edge network chỉ chứa:

```text
shared Caddy
public frontend/upstream của app
```

Không chứa DB, Redis, worker, browser RPC, queue hoặc management service.

### 6.2 Mỗi app có core network

Tên chuẩn:

```text
<app-id>-core
```

Phải:

```yaml
internal: true
```

Core network dùng cho app-to-app nội bộ:

```text
gateway -> worker
gateway -> tts
gateway -> auth-browser
worker  -> local auxiliary service
```

### 6.3 Egress network

Tên chuẩn:

```text
<app-id>-egress
```

Đây là non-internal bridge chỉ cấp cho service thực sự cần outbound Internet.

Không attach DB/cache chỉ để “cho tiện”.

Nếu app cần egress mạnh hơn, Phase 2 chuyển sang:

```text
service
 -> explicit egress proxy
 -> allowlist/audit
 -> Internet
```

Không dùng edge network để cấp outbound Internet.

### 6.4 Data network

Nếu app dùng DB server riêng:

```text
<app-id>-data
```

phải internal và chỉ gồm đúng client/server cần thiết.

Nếu app dùng SQLite shared volume thì data plane là volume, không cần network data riêng.

### 6.5 Không dùng default bridge cho production app

Mọi production service phải khai báo network rõ ràng.

---

## 7. No-published-port policy

Mặc định production:

```yaml
ports: []
```

hoặc không khai báo `ports:`.

Dùng:

```yaml
expose:
  - "8090"
```

chỉ để document port nội bộ.

Debug host mapping nếu thật sự cần phải nằm trong override không deploy production:

```yaml
ports:
  - "127.0.0.1:18091:8090"
```

Không bind app/debug port lên:

```text
0.0.0.0
::
```

nếu không có lý do đã review.

---

## 8. Active/Standby Blue-Green contract cho W1

Tên slot:

```text
blue
green
```

Cả hai có thể chạy 24/7 nếu VPS đủ tài nguyên.

Ví dụ:

```text
Caddy
  |- gateway-green   PRIMARY
  `- gateway-blue    STANDBY
```

Caddy route pattern:

```caddyfile
reverse_proxy acb-web-green:8090 acb-web-blue:8090 {
    lb_policy first

    health_uri /readyz
    health_interval 2s
    health_timeout 1s
    health_fails 2
    health_passes 2

    fail_duration 30s
    max_fails 2
    lb_try_duration 3s
    lb_try_interval 250ms

    stream_close_delay 5m
}
```

`first` + health check được dùng như primary/secondary failover.

### 8.1 Release N -> N+1

Nếu Green active và Blue standby:

```text
1. build/test image N+1 outside production
2. verify signature
3. pull image N+1
4. update Blue only
5. Blue health/readiness/deploy probe
6. smoke Blue directly
7. stage Caddy order: Blue first, Green second
8. validate Caddy
9. reload Caddy
10. public smoke
11. soak
12. keep Green as hot standby
```

Không stop Green sau soak nếu app được xếp `hot-standby-required=true`.

### 8.2 Failure behavior

Nếu primary crash:

```text
Docker restart policy attempts restart
+
Caddy removes unhealthy primary
+
new traffic goes to standby
```

Đây là HA cấp process/application, không phải HA cấp VPS.

---

## 9. Singleton worker contract cho W2

Worker phải có một cơ chế chứng minh “chỉ một active owner”.

Trên one-VPS local volume, một lựa chọn nhẹ:

```text
flock exclusive lock file
```

Ví dụ:

```text
/data/acb-worker.lock
```

Nếu lock không lấy được:

```text
worker startup fails closed
```

Không tự chạy secondary active.

### 9.1 Worker update

```text
pull candidate
-> offline/preflight candidate
-> drain old worker
-> persist state/session
-> stop old
-> start new
-> new obtains lock
-> readiness
-> verify first successful cycle
```

Nếu failure:

```text
restart previous digest
```

### 9.2 Side-effect fencing

Worker có external side effects nên phải có ít nhất một trong:

- generation fence;
- idempotency key;
- DB uniqueness;
- lease token;
- event dedupe;
- cursor/checkpoint.

---

## 10. Health contract

### `/healthz`

Chỉ phản ánh process liveness.

Không fail vì upstream business state như:

```text
AUTH_REQUIRED
customer not logged in
no transactions
external provider maintenance
```

### `/readyz`

Phản ánh instance có thể nhận traffic hay chưa.

Ví dụ W1:

```text
HTTP server ready
DB readable/writable as required
critical config loaded
worker control reachable if endpoint depends on worker
```

### `/internal/deployz`

Chỉ private/debug path.

Phản ánh candidate có đủ điều kiện promote:

```text
release SHA/digest known
schema compatible
worker dependency ready
mandatory sidecars ready
```

Shared Caddy phải chặn `/internal/*` từ public ingress.

---

## 11. Container hardening baseline

Mọi service phải bắt đầu từ baseline sau và chỉ nới quyền khi có test chứng minh cần thiết.

```yaml
restart: unless-stopped
init: true
read_only: true
user: "1000:1000"
cap_drop:
  - ALL
security_opt:
  - no-new-privileges:true
pids_limit: 100
mem_limit: 512m
cpus: 1.0
stop_grace_period: 30s
```

### 11.1 Exceptions

`read_only: false` chỉ dùng khi image/runtime thật sự cần ghi rootfs và phải document lý do.

Chromium/browser thường cần writable `/tmp`; dùng tmpfs thay vì mở cả root filesystem nếu có thể.

### 11.2 Seccomp

- Giữ Docker default seccomp cho service thường.
- Custom seccomp chỉ khi đã test đầy đủ.
- Không dùng `seccomp=unconfined` production nếu không có review riêng.

### 11.3 AppArmor/SELinux

- Giữ distro/default Docker profile.
- Service nhạy cảm có thể có custom profile Phase 2.
- Không disable host LSM cho tiện debug.

### 11.4 Forbidden by default

```text
privileged: true
network_mode: host
pid: host
ipc: host
/dev/* device passthrough
/var/run/docker.sock mount
host root filesystem bind mount
```

Mỗi exception phải có threat review.

---

## 12. Docker socket policy

Docker socket là management-plane credential có quyền gần tương đương root host.

Không mount:

```text
/var/run/docker.sock
```

vào:

- Caddy;
- frontend;
- API;
- worker;
- monitoring agent không cần thiết;
- autoheal container.

Nếu Docker management UI buộc cần socket, UI đó phải được xếp vào **management plane**, Cloudflare Access protected, không chung network với public app và không được coi là app bình thường.

---

## 13. Resource containment

Mỗi service phải có:

- memory hard limit;
- CPU limit hoặc shares phù hợp;
- PID limit;
- bounded tmpfs;
- bounded log storage.

Không disable OOM killer cho app container.

Resource values phải dựa trên đo đạc production hoặc load test, không copy mù giữa repo.

Host cần giữ headroom cho:

```text
kernel
Docker daemon
Caddy/cloudflared
backup/migration one-shot
Blue+Green overlap
```

Minimum operational target:

```text
>= 20% RAM headroom before deployment
>= 20% disk free or app-specific stricter threshold
```

Nếu không đủ headroom, deploy phải abort trước khi start candidate.

---

## 14. Log policy

Ưu tiên Docker `local` logging driver cho workload bình thường:

```yaml
logging:
  driver: local
  options:
    max-size: "10m"
    max-file: "3"
```

Không để unbounded logs làm đầy disk.

App log:

- stdout/stderr structured;
- không log secret/token/cookie/password/full bank session;
- request ID/correlation ID;
- error code ổn định;
- log rotation do Docker driver quản lý.

---

## 15. Secrets contract

Dùng Compose secrets/file mounts thay vì plaintext environment khi app hỗ trợ.

Pattern:

```yaml
services:
  api:
    secrets:
      - worker_internal_token

secrets:
  worker_internal_token:
    file: ./secrets/worker_internal_token
```

App đọc:

```text
/run/secrets/worker_internal_token
```

Host secret directory:

```text
0700 directory
0600 file
```

Không commit:

```text
.env.production
secrets/*
Tunnel token
API tokens
private keys
session cookies
```

Lưu ý Docker Compose file-backed secrets là bind mount; ownership/mode thực tế phải verify trên exact host/image, không giả định `uid/gid/mode` luôn remap như Swarm secrets.

---

## 16. Supply-chain contract

### 16.1 Build outside production

VPS không chạy:

```text
git pull
npm install
bun install
go build
docker compose build
```

trong release flow production.

CI build image và push registry trước.

### 16.2 Immutable deployment

Production chỉ nhận:

```text
registry/repo@sha256:<digest>
```

Không nhận mutable tag làm deployment identity.

### 16.3 Vulnerability scan

Baseline scanner:

```text
Trivy 0.74.0
```

Gate tối thiểu:

- image vulnerability scan;
- misconfiguration scan cho Dockerfile/Compose;
- secret scan source;
- fail release với CRITICAL chưa được allowlist có lý do.

### 16.4 SBOM

Build pipeline sinh SBOM CycloneDX hoặc SPDX cho image/release.

### 16.5 Signing

Dùng Sigstore/Cosign keyless GitHub OIDC.

CI:

```text
build -> push digest -> cosign sign -> verify
```

VPS deploy script phải verify identity/issuer trước pull/promote.

Không deploy image “đúng digest nhưng không đúng signer”.

---

## 17. GitHub Actions hardening

Workflow production phải có:

```yaml
permissions:
  contents: read
  packages: write
  id-token: write
```

chỉ ở job thật sự cần.

Rules:

- production environment protection;
- concurrency group;
- `cancel-in-progress: false` cho deployment;
- không dùng PR từ fork để deploy production;
- third-party actions pin full commit SHA;
- Dependabot hoặc scheduled maintenance cập nhật action pins;
- deploy bằng digest output từ chính build job.

---

## 18. Database migration contract

Blue/Green yêu cầu old/new binary có thể cùng tồn tại trong rollback window.

Dùng:

```text
EXPAND
-> BACKFILL/MIGRATE
-> SWITCH READ/WRITE
-> CONTRACT ở release sau
```

Trong release bình thường cho phép:

- create table;
- add nullable/default-compatible column;
- add index;
- additive enum/state;
- backfill tương thích.

Trong rollback window không được:

- drop column/table old binary còn dùng;
- incompatible rename;
- incompatible type change;
- mandatory constraint khiến old writer fail.

Migration phải chạy qua W6 one-shot và exclusive migration lock.

---

## 19. Backup/restore standard

Mỗi stateful component phải có:

```text
RPO target
RTO target
backup command
restore command
retention
restore drill
```

Tối thiểu:

```text
pre-deploy consistent backup
nightly encrypted off-host backup
weekly restore verification
```

Backup sensitive state phải đi kèm key cần để giải mã.

Ví dụ ACB:

```text
gateway.db
+
APP_MASTER_KEY
```

nhưng lưu hai thành phần theo cơ chế an toàn, không nhét chung public artifact.

---

## 20. Host Docker baseline

### 20.1 `live-restore`

`/etc/docker/daemon.json` baseline:

```json
{
  "live-restore": true,
  "log-driver": "local",
  "log-opts": {
    "max-size": "10m",
    "max-file": "3"
  }
}
```

Không thay toàn file daemon config mù; merge với config hiện có và validate trước reload.

### 20.2 Docker API

Không expose TCP Docker API public.

Preferred:

```text
Unix socket only
```

### 20.3 Rootless/userns

Advanced hardening track:

```text
rootless Docker
OR
userns-remap
```

không bật cả hai ngẫu nhiên và không bật trong cùng migration với app Blue/Green.

Với host đã có nhiều bind mount/Chromium, triển khai như project riêng sau khi test ownership, networking, cgroup và browser behavior.

### 20.4 Host firewall

Vì Cloudflare Tunnel là outbound-only, public application ports không cần mở inbound.

Host firewall vẫn phải chặn unexpected inbound.

Nếu có Docker-published port exception, review Docker firewall/NAT behavior thay vì giả định UFW alone luôn đủ.

### 20.5 OS

Baseline:

- supported Linux distro;
- automatic security patch policy;
- NTP/time synchronization;
- no password SSH;
- no root SSH login;
- audit `sudo`/deploy users;
- disk monitoring;
- filesystem permissions review.

---

## 21. SSH/management plane

Preferred interactive admin path:

```text
Cloudflare Access for Infrastructure
-> short-lived SSH certificates
-> private Tunnel path
```

Nếu GitHub Actions vẫn dùng direct SSH trong giai đoạn chuyển đổi:

- key-only;
- dedicated deploy user;
- pinned known_hosts;
- no root login;
- command/permission scope càng nhỏ càng tốt;
- migration sang private deployment path là phase riêng.

Management UI phải đi qua hostname riêng + Cloudflare Access/MFA.

---

## 22. Runtime self-healing hierarchy

Không áp một cơ chế restart cho mọi service.

### W1 stateless HTTP

```text
Caddy failover
-> Docker restart
-> host watchdog after sustained unhealthy
```

### W2 stateful worker

```text
alert / controlled restart
-> lock/state recovery
```

Không blind restart loop nếu external session nhạy cảm.

### W3 datastore

```text
restart policy + integrity/recovery rules specific to datastore
```

Không auto-delete/recreate volume.

### Host watchdog

Nếu cần, dùng host-level `systemd` timer với allowlist service names.

Không chạy generic autoheal container có Docker socket.

---

## 23. Observability minimum

Mọi repo phải có:

- `/healthz`;
- `/readyz`;
- structured logs;
- release SHA/digest visible trong internal status;
- container restart count;
- health state;
- CPU/RAM usage;
- disk usage;
- backup freshness;
- deployment result.

Stateful external integration thêm:

- last successful cycle;
- external 429/rate-limit count;
- session/auth state;
- queue/backlog/dead-letter count.

Không cần Prometheus để pass v1 nếu health/status scripts đã đủ; metrics stack có thể bổ sung sau.

---

## 24. Repository contract v1

Repo production-compatible nên có tối thiểu:

```text
deploy/
  README.md
  compose.core.yaml
  compose.slot.yaml        # nếu có W1 Blue/Green
  compose.ops.yaml         # backup/migration/check
  deploy.sh
  rollback.sh
  verify-deployment.sh
  preflight.sh
  smoke-slot.sh
  secrets/
    .gitkeep

docs/
  PRODUCTION_SETUP.md
```

Nếu không cần Blue/Green, `compose.slot.yaml` có thể bỏ nhưng phải ghi lý do workload class.

### Required environment metadata

Mọi app cần định nghĩa:

```text
APP_ID
PUBLIC_HOSTNAME
ACTIVE_SLOT
IMAGE_DIGEST(S)
HEALTH_PATH
READY_PATH
EDGE_NETWORK
CORE_NETWORK
EGRESS_REQUIRED_SERVICES
```

Không bắt buộc phải tạo custom YAML parser; metadata có thể nằm trong Compose + deploy state files miễn contract được giữ.

---

## 25. Standard deployment state machine

```text
SOURCE PUSH
    |
    v
VERIFY / TEST
    |
    v
BUILD IMAGE
    |
    v
SCAN + SBOM
    |
    v
PUSH DIGEST
    |
    v
SIGN + VERIFY
    |
    v
PRODUCTION PRECHECK
  - disk
  - RAM headroom
  - networks
  - secrets
  - current active slot
    |
    v
BACKUP / MIGRATION GATE
    |
    v
START INACTIVE SLOT
    |
    v
HEALTH / READY / DEPLOY PROBE
    |
    v
DIRECT SMOKE
    |
    v
STAGE CADDY ROUTE ORDER
    |
    v
CADDY VALIDATE
    |
    v
CADDY RELOAD
    |
    v
PUBLIC SMOKE
    |
    v
SOAK
   / \
FAIL OK
 |    |
route old primary first
      |
      v
new primary retained
old version hot standby
```

---

## 26. Rollback standard

### During hot-standby window

Rollback là route order flip:

```text
new primary -> standby
old standby -> primary
```

Không rebuild.
Không re-run destructive migration.
Không `docker compose down`.

### Historical rollback

Nếu old slot đã được overwritten:

```text
restore old digest into inactive slot
-> health/smoke
-> Caddy switch
```

---

## 27. Security review checklist cho repo mới

### Ingress

- [ ] Không có public host port không cần thiết.
- [ ] Chỉ frontend/public service join edge network.
- [ ] Caddy shared, không proxy riêng per-repo.
- [ ] Unknown host trả 404.
- [ ] Cloudflare Access classification rõ.

### Network

- [ ] Edge network internal.
- [ ] Core network internal.
- [ ] DB/cache không join edge.
- [ ] Chỉ service cần Internet join egress.

### Runtime

- [ ] non-root nếu hỗ trợ.
- [ ] `cap_drop: ALL`.
- [ ] `no-new-privileges`.
- [ ] read-only rootfs nếu hỗ trợ.
- [ ] tmpfs bounded.
- [ ] no privileged.
- [ ] no Docker socket.
- [ ] resource/PID limits.
- [ ] healthcheck.
- [ ] graceful shutdown.

### Secrets

- [ ] Secret file-backed.
- [ ] Per-service grant.
- [ ] No secret logs.
- [ ] No secret in image layers.

### Supply chain

- [ ] Build in CI.
- [ ] Immutable digest.
- [ ] Scan.
- [ ] SBOM.
- [ ] Signed image.
- [ ] Deploy signature verified.

### State

- [ ] Migration compatible with rollback.
- [ ] Backup consistent.
- [ ] Off-host copy.
- [ ] Restore tested.

### Operations

- [ ] Deployment serialized.
- [ ] Rollback tested.
- [ ] Failure drill completed.
- [ ] Logs bounded.
- [ ] Disk headroom gate.
- [ ] RAM headroom gate.

---

## 28. Migration sequence cho repo kế tiếp

Không copy ACB-specific worker design mù.

Đối với repo mới:

```text
1. classify every service W1-W6
2. inventory public ports and secrets
3. create edge/core/egress topology
4. apply container hardening
5. add health/readiness
6. build immutable CI pipeline
7. choose deploy strategy by workload class
8. add backup/migration gates
9. add Caddy route
10. test rollback/failure
11. cut over one app only
12. remove legacy ingress/ports after rollback window
```

---

## 29. Official references used by this standard

- Docker Engine security: `https://docs.docker.com/engine/security/`
- Docker rootless mode: `https://docs.docker.com/engine/security/rootless/`
- Docker user namespace remap: `https://docs.docker.com/engine/security/userns-remap/`
- Docker seccomp: `https://docs.docker.com/engine/security/seccomp/`
- Docker AppArmor: `https://docs.docker.com/engine/security/apparmor/`
- Docker bridge networks: `https://docs.docker.com/engine/network/drivers/bridge/`
- Docker internal network option: `https://docs.docker.com/reference/cli/docker/network/create/`
- Docker port publishing: `https://docs.docker.com/engine/network/port-publishing/`
- Docker firewall behavior: `https://docs.docker.com/engine/network/firewall-iptables/`
- Docker resource constraints: `https://docs.docker.com/engine/containers/resource_constraints/`
- Docker local log driver: `https://docs.docker.com/engine/logging/drivers/local/`
- Docker live restore: `https://docs.docker.com/engine/daemon/live-restore/`
- Docker Compose secrets: `https://docs.docker.com/compose/how-tos/use-secrets/`
- Caddy zero-downtime reload: `https://caddyserver.com/docs/getting-started`
- Caddy API rollback behavior: `https://caddyserver.com/docs/api`
- Caddy reverse proxy/failover: `https://caddyserver.com/docs/caddyfile/directives/reverse_proxy`
- Cloudflare Tunnel: `https://developers.cloudflare.com/tunnel/`
- Cloudflare SSH Access for Infrastructure: `https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/use-cases/ssh/ssh-infrastructure-access/`
- Sigstore CI quickstart: `https://docs.sigstore.dev/quickstart/quickstart-ci/`
- Trivy: `https://www.trivy.dev/docs/latest/`

---

## 30. Versioning rule

Standard này được gọi là:

```text
Single-VPS Secure Container Platform Standard v1
```

Khi thay đổi contract ảnh hưởng mọi repo, tạo version mới thay vì âm thầm sửa behavior.

Ví dụ:

```text
v1 -> Docker Compose single-host
v2 -> optional multi-node/orchestrator contract
```

Repo phải ghi version platform standard mà nó đang tuân thủ trong `deploy/README.md`.
