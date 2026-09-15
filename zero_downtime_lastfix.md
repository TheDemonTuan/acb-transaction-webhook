Được. Nếu mục tiêu là **làm một lần cho hết**, tôi sẽ coi đây là bản spec cuối để đưa repo lên mức **10/10 trong phạm vi kiến trúc single-VPS**.

Có một giới hạn cần nói rõ: một VPS vẫn là **single point of failure vật lý**. Nếu VPS chết, disk chết hoặc provider mất mạng thì code không thể tạo HA thật sự. Còn xét các mục tiêu của repo — deploy an toàn, polling ACB không bị ảnh hưởng bởi thành phần khác, realtime, rollback, security, recovery — thì có thể làm rất sát 10/10.

Hiện HEAD `79def32` đã deploy thành công, baseline production, worker fencing, gateway B/G, selective build và soak đều đã tiến bộ rất nhiều.  Phần ingest tiền cũng đã làm đúng một điểm rất quan trọng: transaction, event, delivery và journal được ghi **atomically trong cùng SQLite transaction**. Phần này nên giữ nguyên.

# Kiến trúc cuối tôi muốn đạt tới

```text
                         GITHUB
                           │
                    protected main
                           │
               ┌───────────▼────────────┐
               │ CI / Test / Security   │
               │ Go + Bun + integration│
               │ CodeQL + Trivy        │
               └───────────┬────────────┘
                           │
                    impact analysis
                           │
               chỉ build component đổi
                           │
                    immutable images
                           │
                       Cosign
                           │
                 signed release bundle
                           │
                           ▼
               ┌──────────────────────┐
               │ VPS stable deployer  │
               │ KHÔNG bị overwrite  │
               └───────────┬──────────┘
                           │
                   verify candidate
                           │
                   stage release dir
                           │
        ┌──────────────────┼──────────────────┐
        ▼                  ▼                  ▼
     frontend BG       gateway BG        worker singleton
                                             │
                                        quiesce/drain
                                             │
                                        replace/rollback
                                             │
                                             ▼
                                            ACB
                                             │
                                             ▼
                                      atomic SQLite TX
                             ┌───────────────┼──────────────┐
                             ▼               ▼              ▼
                         transaction       journal        outbox
                                             │              │
                                             ▼              ▼
                                         realtime       dispatcher
                                             │          webhook/Bark
                                             ▼
                                           client
```

---

# Master plan 10/10

|  # | Hạng mục                           | Mức    | Kết quả cuối                                     |
| -: | ---------------------------------- | ------ | ------------------------------------------------ |
|  1 | Stable deploy bootstrap            | **P0** | Candidate không thể overwrite deployer đang chạy |
|  2 | Canonical release state atomic     | **P0** | Chỉ có một source-of-truth                       |
|  3 | Service-aware impact analysis      | **P0** | File đổi nào → đúng service đó                   |
|  4 | Deploy failover-controller thật sự | **P0** | Source mới chắc chắn chạy trên VPS               |
|  5 | Worker deploy protocol v2          | **P0** | Không còn 404 legacy bypass                      |
|  6 | Notification durability 24–72h     | **P0** | Downstream chết lâu không mất notification       |
|  7 | Realtime gap/recovery correctness  | **P1** | Không silent-skip journal                        |
|  8 | Frontend zero-downtime             | **P1** | UI cũng B/G                                      |
|  9 | Release crash recovery             | **P1** | Mất SSH/GHA giữa deploy vẫn tự reconcile         |
| 10 | Secret permissions                 | **P1** | Không còn secret `0644`                          |
| 11 | Branch/ruleset security            | **P1** | Không direct-push phá production                 |
| 12 | Runtime drift detection            | **P1** | Production thực tế luôn khớp release state       |
| 13 | Observability/SLO                  | **P1** | Biết chính xác delay nằm ở đâu                   |
| 14 | Chaos/DR testing                   | **P1** | Test crash thực sự thay vì chỉ happy-path        |
| 15 | Backup/restore drill               | **P1** | Backup không chỉ “có”, mà restore được           |
| 16 | Final production contract          | **P2** | CI khóa toàn bộ invariant                        |

---

# 1. Không được sync candidate trực tiếp lên deploy đang chạy

Đây là thay đổi đầu tiên.

Workflow hiện đang:

```bash
scp deploy/* VPS:$DEPLOY_PATH/deploy/
```

rồi mới chạy dispatcher. Nghĩa là candidate script đã overwrite code deploy production trước khi candidate được verify đầy đủ.

Đổi layout VPS thành:

```text
/opt/acb/
├── deployer/
│   ├── stable/
│   │   └── deploy-release.sh
│   └── cosign
│
├── releases/
│   ├── rel-abc123/
│   │   ├── manifest.json
│   │   ├── manifest.bundle
│   │   ├── compose/
│   │   ├── deploy/
│   │   └── platform/
│   │
│   └── rel-def456/
│
├── current -> releases/rel-abc123
├── data/
├── secrets/
└── state/
```

Luồng mới:

```text
GitHub
  │
  ├─ upload -> releases/<release-id>.candidate/
  │
  ├─ stable deployer verify Cosign
  │
  ├─ verify checksums
  │
  ├─ verify compose
  │
  ├─ verify component scope
  │
  ├─ execute transactions
  │
  └─ atomic symlink current -> release
```

`stable deployer` không nằm trong release bundle nên candidate commit không thể tự thay validator của chính nó.

---

# 2. Gộp release state thành đúng một canonical state

Hiện có nhiều nơi:

```text
last-release.json
.release.env
.active-slot
.previous-slot
deploy journal
GitHub Variables
```

Dù logic hiện tốt hơn trước, càng nhiều source-of-truth càng dễ drift.

Tạo:

```text
/opt/acb/state/current-release.json
```

dạng:

```json
{
  "schema": 2,
  "release_id": "rel-79def32",
  "git_sha": "79def32...",
  "status": "SUCCESS",

  "components": {
    "frontend": {
      "image": "ghcr.io/...@sha256:..."
    },
    "gateway": {
      "active_slot": "green",
      "image": "ghcr.io/...@sha256:..."
    },
    "worker": {
      "image": "ghcr.io/...@sha256:..."
    },
    "auth_browser": {
      "image": "ghcr.io/...@sha256:..."
    },
    "tts": {
      "image": "ghcr.io/...@sha256:..."
    },
    "bark": {
      "image": "ghcr.io/...@sha256:..."
    },
    "failover_controller": {
      "sha256": "..."
    }
  },

  "compose_hash": "...",
  "promoted_at": "...",
  "previous_release": "..."
}
```

Quan trọng nhất:

```text
component deploy scripts
        │
        X không được tự commit canonical state
        │
        ▼
chỉ trả SUCCESS/FAIL
        │
        ▼
dispatch-rollout
        │
        ▼
tất cả transaction + soak OK
        │
        ▼
write temp
fsync
rename()
fsync directory
        │
        ▼
current-release.json SUCCESS
```

Hiện component scripts đang tự `set_release_env`, ví dụ worker commit image trước khi dispatcher kết thúc toàn release.

Nên đổi thành:

```text
deploy-worker.sh
→ deploy thôi

dispatch-rollout.sh
→ duy nhất nơi commit release state
```

Đây sẽ xử lý luôn lỗi consistency kiểu:

```text
worker deploy OK
gateway deploy fail
release state dở dang
```

---

# 3. Bỏ một `compose.prod.yaml` khổng lồ

Hiện worker, browser, TTS, Bark, frontend, gateway B/G cùng nằm một compose.

Tách:

```text
deploy/compose/
├── base.yaml
├── worker.yaml
├── frontend.yaml
├── gateway.yaml
├── auth-browser.yaml
├── tts.yaml
├── bark.yaml
└── dbtool.yaml
```

Khi đó classifier cực rõ:

```text
compose/worker.yaml
       ↓
worker=true

compose/frontend.yaml
       ↓
frontend=true

platform/failover/**
       ↓
failover_controller=true
```

Không còn tình trạng:

```text
compose.prod.yaml changed
→ platform=true
→ không biết worker/gateway nào cần recreate
```

Classifier hiện vẫn gom `deploy/**` và `platform/**` về platform khá rộng.

---

# 4. Có một `component-map.yaml` duy nhất

Không hard-code impact rules rải trong shell.

Ví dụ:

```yaml
components:

  worker:
    paths:
      - cmd/worker/**
      - internal/monitor/**
      - internal/acb/**
      - internal/notification/**
      - internal/webhook/**
      - internal/bark/**
      - internal/maintenance/**
      - internal/workerstate/**
      - compose/worker.yaml

  gateway:
    paths:
      - cmd/gateway/**
      - internal/httpapi/**
      - internal/eventhub/**
      - compose/gateway.yaml

  frontend:
    paths:
      - web/**
      - deploy/frontend-nginx.conf
      - compose/frontend.yaml

  failover_controller:
    paths:
      - platform/failover/**

  platform:
    paths:
      - platform/edge/**
```

Shared package:

```yaml
shared:
  internal/storage/**:
    - gateway
    - worker
    - dbtool
```

Nếu file không match:

```text
UNCLASSIFIED
      ↓
CI FAIL
```

Không nên fallback âm thầm rộng.

Developer thêm file mới phải khai ownership ngay.

---

# 5. Failover controller thành first-class deploy component

Hiện source mới:

```text
platform/failover/vps-failover-controller.py
```

có stale-event fencing rất tốt.

Nhưng production workflow hiện chỉ sync `deploy/*` và `platform/edge/probe.sh`, chưa thấy deploy controller sang `/opt/platform/failover`.

Tạo transaction riêng:

```text
candidate controller
       ↓
python3 -m py_compile
       ↓
run test_failover.py
       ↓
sha256 verify
       ↓
backup current binary
       ↓
atomic rename
       ↓
systemctl daemon-reload
       ↓
systemctl restart
       ↓
systemctl is-active
       ↓
reconcile dry-run
       ↓
SUCCESS
```

Fail:

```text
restore previous
→ restart
→ verify
```

Manifest phải chứa hash của:

```text
vps-failover-controller.py
.service
.timer
apps.d/*
```

---

# 6. Worker deploy protocol phải chính thức version hóa

Bỏ logic:

```text
/rpc/quiesce → 404
→ "legacy"
→ cứ stop worker
```

Worker hiện đã support `quiesce`, `drain`, `resume`, readiness/liveness.

Thêm:

```bash
/worker --deploy-capabilities
```

trả:

```json
{
  "protocol": 2,
  "quiesce": true,
  "drain": true,
  "resume": true,
  "readiness": true,
  "checkpoint": true
}
```

Deploy yêu cầu:

```text
protocol >= 2
```

Không đáp ứng:

```text
FAIL CLOSED
```

Không đoán capability bằng HTTP error nữa.

---

# 7. Quiesce worker phải trở thành barrier thật sự

Quiesce response cuối nên chứa:

```json
{
  "quiesced": true,

  "poller": "IDLE",
  "dispatcher": "IDLE",
  "history": "IDLE",
  "maintenance": "IDLE",

  "active_deliveries": 0,
  "active_poll": false,

  "journal_seq": 187261,
  "session_checkpointed": true,

  "generation": 133
}
```

Chỉ stop worker nếu tất cả invariant trên đạt.

Flow:

```text
prepull candidate
prepull rollback
       ↓
acquire deploy lock
       ↓
acquire mutation gate
       ↓
quiesce
       ↓
wait in-flight = 0
       ↓
persist ACB session
       ↓
record journal checkpoint
       ↓
stop old
       ↓
start candidate --no-deps
       ↓
candidate readiness
       ↓
verify generation/checkpoint
       ↓
release mutation gate
```

Nếu candidate fail:

```text
stop candidate
→ old digest
→ readiness
→ resume
```

---

# 8. Worker readiness cần chia thành liveness / readiness / progress

Hiện readiness đã tốt hơn trước: storage, schema, singleton lock, scheduler running và phát hiện realtime poll/keepalive treo quá 2 phút.

Tách rõ:

```text
/healthz
process sống

/readyz
DB + lock + scheduler + RPC + realtime server ready

/progressz
poll scheduler vẫn thực sự tiến lên
```

`progressz` theo:

```text
last_poll_started
last_poll_finished
last_successful_acb_response
last_journal_commit
```

Không nên chỉ dựa heartbeat 5 giây của process, vì process có thể sống nhưng poll loop chết.

---

# 9. Notification phải durable tới 24–72 giờ

Đây là phần cần nâng mạnh nếu hệ thống liên quan giao dịch tiền.

Hiện dispatcher có:

```text
2s
5s
15s
30s
1m
2m
5m
15m
```

nhưng với `maxRetries=len(backoffs)` thì delivery thực tế có thể terminal sau khoảng dưới 10 phút đối với lỗi retryable.

Đổi policy thành:

```text
2s
5s
15s
30s
1m
2m
5m
10m
15m
30m
30m
30m
...
```

Cho đến:

```text
delivery_age >= 72h
```

Mới:

```text
DEAD_LETTER
```

Phân loại:

| Lỗi                       | Xử lý    |
| ------------------------- | -------- |
| timeout                   | retry    |
| DNS/network               | retry    |
| HTTP 408                  | retry    |
| HTTP 429                  | retry    |
| HTTP 5xx                  | retry    |
| HTTP 400/401/403/404      | terminal |
| malformed endpoint config | terminal |

Webhook sender hiện đã phân biệt phần lớn nhóm này đúng.

---

# 10. Bỏ ticker 2 giây của notification dispatcher

Fast path hiện đã gọi:

```go
dispatcher.Wake()
```

ngay khi transaction mới được commit.

Nên tận dụng triệt để.

Thay:

```go
ticker := time.NewTicker(2 * time.Second)
```

bằng:

```text
Wake channel
     +
dynamic retry timer
```

Pseudo:

```text
query NextDeliveryDue()

timer = due-now

select:
  new event wake
  retry timer
  context cancellation
```

Khi ACB trả transaction:

```text
ACB
 ↓
DB commit
 ↓
Wake
 ↓
dispatch ngay
```

Không phải chờ internal polling.

Điều này đúng mục tiêu của bạn:

> delay đáng kể duy nhất phải là ACB polling.

---

# 11. Delivery phải thiết kế theo at-least-once + idempotency

Không thể đảm bảo exactly-once qua HTTP:

```text
POST webhook thành công
       ↓
network response về
       ↓
process crash trước CompleteDelivery
       ↓
delivery retry
```

=> duplicate.

Do đó mỗi notification phải có:

```text
event_id
delivery_id
attempt
idempotency_key
```

Webhook header:

```http
X-Event-ID: bevt_xxx
X-Delivery-ID: deliv_xxx
X-Idempotency-Key: deliv_xxx
X-Signature: ...
```

Receiver dedupe theo `delivery_id`.

Với Bark không hỗ trợ transactional dedupe, chấp nhận semantics:

```text
at-least-once
```

nhưng đảm bảo:

```text
never silently lost
```

---

# 12. Realtime journal phải xử lý retention gap

RealtimeCoordinator hiện có bounded queue + journal recovery rất tốt.

Nhưng cần đóng thêm gap này.

Store đã có:

```go
GetMinJournalSeq()
GetMaxJournalSeq()
```

Nếu:

```text
client/coordinator lastSeq = 100
journal minimum = 500
```

thì không được giả vờ recover `101→499`.

Phải:

```text
lastSeq < minSeq - 1
       ↓
JOURNAL_GAP
       ↓
snapshot/resync
       ↓
cursor=minSeq-1
       ↓
replay
```

SSE có thể emit:

```text
event: reset
data: {"reason":"journal_retention_gap"}
```

Client fetch snapshot mới rồi tiếp tục realtime.

---

# 13. Sửa `rows.Err()` trong journal

`ReadJournalEvents` hiện loop:

```go
for rows.Next() {
   ...
}
return entries, nil
```

nhưng chưa check `rows.Err()`.

Phải:

```go
if err := rows.Err(); err != nil {
    return nil, fmt.Errorf("iterate journal events: %w", err)
}
```

Nhỏ, nhưng 10/10 thì không để query iteration lỗi bị coi là success.

---

# 14. Frontend cũng blue/green

Hiện frontend chỉ có một container `acb-frontend`.

Đổi:

```text
frontend-blue
frontend-green
```

Luồng giống gateway nhưng nhẹ hơn:

```text
pull candidate
→ start inactive
→ /readyz x2
→ switch Traefik
→ ACK
→ short soak
→ stop previous
```

Lúc đó:

```text
frontend deploy = zero visible downtime
```

Không còn vài giây nginx recreate.

---

# 15. Compose/environment config phải là release artifact

Không để `.env` hoặc compose config thay ngoài release mà CI không biết.

Manifest chứa:

```text
compose worker hash
compose gateway hash
compose frontend hash
env-schema hash
Traefik route template hash
failover config hash
```

Không chứa secret value.

Khi deploy:

```text
expected config hash
          =
actual config hash
```

không khớp:

```text
FAIL
```

---

# 16. Production state phải lấy 100% từ VPS

Phần baseline hiện đã làm tốt: đọc successful release SHA, image digest và active gateway slot từ VPS, validate SHA và ancestry.

Nhưng ở scan/manifest vẫn còn fallback dùng:

```text
vars.FRONTEND_IMAGE_REF
vars.GATEWAY_IMAGE_REF
...
```

Bỏ hẳn.

Logic:

```text
component changed?
    │
    ├─ yes → candidate digest
    │
    └─ no  → VPS actual digest
```

GitHub Variables không được là production source-of-truth.

---

# 17. Thêm runtime drift detector

Mỗi vài phút:

```text
current-release.json
        │
        ▼
docker inspect
        │
        ▼
actual running image digest
```

So sánh:

```text
worker expected vs actual
gateway expected vs actual
frontend expected vs actual
browser expected vs actual
tts expected vs actual
bark expected vs actual
```

Sai:

```text
PRODUCTION_DRIFT
```

Không tự blindly sửa.

Alert trước.

Ví dụ operator chạy tay:

```bash
docker compose up worker
```

với image khác thì hệ thống phát hiện ngay.

---

# 18. Release recovery state machine

Deploy phải survive:

```text
SSH disconnect
GitHub Action cancel
VPS reboot
shell crash
```

Journal:

```text
PREPARED
COMPONENTS_RUNNING
WORKER_PROMOTED
GATEWAY_SWITCHED
SOAKING
COMMITTING
COMPLETED
```

Khi deployer chạy lại:

```text
journal != COMPLETED
        ↓
inspect current runtime
        ↓
compare candidate/previous
        ↓
resume OR rollback deterministically
```

Không được đơn giản archive `INTERRUPTED` rồi tiếp tục release mới nếu runtime state chưa reconcile.

---

# 19. Atomic release-state commit

State write phải dùng:

```text
write current-release.json.tmp
↓
fsync(file)
↓
rename()
↓
fsync(parent directory)
```

Tương tự deploy journals.

Điện mất đúng lúc write vẫn không tạo JSON half-written.

---

# 20. Secret Bark không được `0644`

Workflow hiện:

```bash
chmod 0644 bark_basic_auth_user bark_basic_auth_password
```

`0644` nghĩa là tất cả local user trên VPS có quyền read.

Đổi sang một trong hai giải pháp:

```text
0600 + container chạy đúng UID sở hữu file
```

hoặc:

```text
root:bark-secret
0640
container supplemental group
```

Nếu cần Docker Compose bind secret:

```text
chown <bark_uid>:<group>
chmod 0400/0440
```

Không dùng world-readable để giải quyết UID mismatch.

---

# 21. Main branch phải protected

Hiện GitHub báo:

```json
"protected": false
```

và commit HEAD cũng unsigned.

Đây là một trong các điểm còn xa “10/10”.

Ruleset:

```text
main
 ├─ require pull request
 ├─ require CI success
 ├─ require CodeQL
 ├─ require security scan
 ├─ require deploy-policy tests
 ├─ block force push
 ├─ block branch deletion
 ├─ require linear history
 └─ optionally require signed commits
```

Nếu bạn làm repo một mình:

```text
PR approval count = 0 hoặc 1
```

nhưng vẫn không direct push vào main.

---

# 22. Production environment protection

GitHub Environment `production` đang được dùng rồi — tốt.

Nâng thêm:

```text
environment: production

deployment branch:
main only

prevent self-review nếu team
```

Và chỉ `deploy` job có:

```text
VPS SSH key
```

Build/test jobs không được nhìn thấy production secrets.

---

# 23. Workflow permissions tối thiểu

Từng job có permission riêng.

Ví dụ:

```text
verify:
 contents: read

build:
 contents: read
 packages: write

scan:
 contents: read
 packages: read/write
 id-token: write

deploy:
 contents: read
```

Không để default token quá quyền.

Actions hiện đã được pin full SHA, đây là điểm tốt và CI cũng kiểm tra nó.

---

# 24. Release artifact phải immutable + signed

Giữ hiện tại:

```text
Trivy
SBOM
Cosign
manifest signature
immutable @sha256
```

Workflow mới nhất đã scan/sign/verify các first-party image và manifest. Đây là một phần rất tốt hiện tại.

Nâng thêm:

```text
manifest
   ├── git SHA
   ├── all image digests
   ├── all deploy artifact hashes
   ├── compose hashes
   ├── platform hashes
   ├── migration version
   └── component scope
```

Remote stable deployer verify tất cả trước khi execute.

---

# 25. Migration contract

Schema migration phải luôn:

```text
backup
↓
integrity_check
↓
migration
↓
schema compatibility test
↓
continue deploy
```

Latest production log hiện đã làm khá chuẩn: backup SQLite, integrity verification, encrypted backup và compatibility check. Đây nên giữ nguyên.

Bổ sung:

```text
migration compatibility:
old gateway + new schema
old worker + new schema
new gateway + new schema
new worker + new schema
```

Vì trong rolling deploy có lúc binary cũ và schema mới tồn tại cùng lúc.

---

# 26. SQLite critical durability

Vì đây là money-event gateway, acceptance contract phải là:

```text
ACB transaction phát hiện
       ↓
SQLite transaction COMMIT
       ↓
mới được coi là accepted
```

Điều này hiện đã đúng với batch ingest: transaction + event + delivery + journal nằm trong cùng SQL transaction.

Ngoài ra production test phải kiểm:

```text
WAL enabled
foreign_keys enabled
busy_timeout configured
integrity_check OK
disk free threshold OK
```

Nếu disk gần full:

```text
alert trước
```

Không đợi SQLite write fail.

---

# 27. Backup phải có restore drill

Không chỉ tạo backup.

Tạo CI/offline job:

```text
production backup copy
       ↓
temporary volume
       ↓
decrypt
       ↓
restore
       ↓
PRAGMA integrity_check
       ↓
run dbtool schema check
       ↓
boot worker/gateway read-only test
```

Backup chưa restore thử thì chưa được coi là backup đáng tin cậy.

---

# 28. ACB session cũng phải có recovery contract

Worker deploy log mới nhất cho thấy gap hiện khá nhỏ:

```text
quiesce ~07:59:03
new worker ready ~07:59:08
```

khoảng vài giây, rất tốt.

Nhưng cần metric:

```text
worker_upgrade_gap_seconds
```

và:

```text
session_persist_success
session_restore_success
generation
last_successful_poll
```

Nếu candidate start nhưng session không restore được:

```text
readiness FAIL
→ rollback
```

Không mark ready chỉ vì HTTP server đã mở.

---

# 29. Polling progress watchdog

Mục tiêu của bạn quan trọng nhất là:

```text
ACB polling không được silently chết
```

Nên controller theo dõi:

```text
worker process healthy
AND
scheduler running
AND
poll progress advancing
```

Ví dụ:

```text
last poll finished > expected threshold
AND no poll currently legitimately running
    ↓
worker unhealthy
```

Phân biệt:

```text
ACB request chậm
```

và:

```text
scheduler deadlock
```

---

# 30. Realtime SLO metric

Đo 4 mốc:

```text
T0 = bắt đầu poll ACB
T1 = ACB response
T2 = SQLite commit
T3 = gateway realtime publish
T4 = webhook/Bark dispatch
```

Metrics:

```text
acb_poll_duration
acb_response_to_commit
commit_to_gateway
commit_to_dispatch
delivery_http_latency
```

Mục tiêu:

```text
T1 → T2: vài ms
T2 → T3: gần tức thì
T2 → dispatch start: gần tức thì
```

Khi user thấy notification chậm, bạn biết chính xác:

```text
ACB chậm?
SQLite chậm?
worker chậm?
network downstream chậm?
```

---

# 31. Alert theo backlog thay vì chỉ process health

Theo dõi:

```text
pending_delivery_count
oldest_pending_delivery_age
journal_lag
realtime_queue_full_count
failed_deliveries
worker_restart_count
ACB last success age
disk free
DB WAL size
```

Alert đáng sợ nhất:

```text
ACB last successful poll > threshold
```

và:

```text
oldest pending notification > threshold
```

---

# 32. Failover stale event đã sửa — giữ test bắt buộc

Logic hiện đã:

```text
inspect actual container
compare ID
check running+healthy
check StartedAt
ignore stale event
only consume restart count after successful restart
```

Đưa test đó thành **production invariant test**, không chỉ unit test bình thường.

Sau này ai sửa controller làm regression thì CI phải đỏ.

---

# 33. Gateway B/G giữ nguyên nhưng nâng state ownership

Gateway hiện:

```text
candidate --no-deps
ready x2
route switch
route identity ACK
15 min soak
stop previous
```

và production run mới nhất đã thực sự chạy hết soak thành công.

Đây gần như đạt chuẩn rồi.

Chỉ thay:

```text
deploy-gateway.sh
```

không tự `set_release_env`.

Canonical state commit bởi release coordinator ở cuối.

---

# 34. Failover controller không được fight với deploy

Giữ cơ chế hiện tại:

```text
host lock
deployment journal
intentional-stop marker
operation lease
```

Các cơ chế này đã được thiết kế trong failover layer.

Nhưng phải có chaos test race:

```text
gateway deploy đang switch
        +
gateway active chết
```

controller phải:

```text
detect deploy lock
→ không tự failover song song
```

---

# 35. Container unchanged audit áp dụng cho mọi transaction

Hiện gateway có audit container ID của worker/browser/TTS/Bark — rất tốt.

Tổng quát hóa:

```text
before transaction:
snapshot all component IDs

after:
assert only authorized components changed
```

Ví dụ frontend release:

```text
frontend allowed change
gateway SAME ID
worker SAME ID
auth-browser SAME ID
tts SAME ID
bark SAME ID
```

Nếu service không nằm promotion scope nhưng ID đổi:

```text
RELEASE FAIL
```

---

# 36. Chaos test matrix bắt buộc

Đây là phần để từ “code nhìn có vẻ đúng” thành “10/10”.

| Scenario                                   | Expected                             |
| ------------------------------------------ | ------------------------------------ |
| kill worker                                | auto recover                         |
| Docker auto-restart trước controller event | controller không restart lần 2       |
| worker unhealthy                           | bounded restart                      |
| candidate worker fail                      | rollback old                         |
| quiesce fail                               | old worker tiếp tục                  |
| deploy SSH mất                             | journal recover                      |
| GitHub Action cancel giữa soak             | runtime reconcile                    |
| gateway candidate bad                      | active untouched                     |
| route ACK fail                             | revert                               |
| Traefik reload chậm                        | retry ACK                            |
| webhook down 2h                            | queue giữ, tự retry                  |
| Bark down 2h                               | queue giữ                            |
| SQLite temporarily locked                  | retry/fail safely                    |
| disk nearly full                           | alert/fail closed                    |
| frontend bad                               | route không switch                   |
| failover-controller update bad             | rollback controller                  |
| VPS reboot                                 | worker/session/release state recover |
| journal retention gap                      | snapshot reset                       |
| old release state corrupted                | fail closed                          |
| active slot file missing                   | infer only if unambiguous            |
| both gateway slots bad                     | degraded, không random switch        |

Không release nếu matrix quan trọng không pass.

---

# 37. CI cuối cùng

Pipeline cuối:

```text
production-state
      │
      ▼
resolve baseline
      │
      ▼
impact analysis
      │
      ├─ static checks
      ├─ unit
      ├─ integration
      ├─ failover
      ├─ deploy transaction tests
      ├─ realtime pipeline tests
      ├─ migration compatibility
      └─ security
      │
      ▼
build affected only
      │
      ▼
scan + SBOM + Cosign
      │
      ▼
generate signed manifest
      │
      ▼
stage candidate release
      │
      ▼
stable remote verifier
      │
      ▼
transaction rollout
      │
      ▼
soak
      │
      ▼
runtime identity/drift audit
      │
      ▼
atomic current-release commit
```

---

# 38. Điều kiện để tôi gọi nó “10/10”

Không phải “CI xanh là xong”.

Release chỉ được coi hoàn thiện khi cùng lúc thỏa:

```text
ACB polling survives unrelated deploys
worker stale events cannot produce false restarts
worker deploy has bounded unavoidable gap only
no candidate can overwrite trusted deployer
all deployment state has one canonical authority
compose changes always map to correct service
production controller code equals committed version
notifications survive long downstream outages
realtime cannot silently skip retained gaps
frontend and gateway deploy without client downtime
every release immutable and signed
secrets are least-privilege
main cannot bypass CI
runtime drift is detected
backup restore is tested
deploy crash is recoverable
chaos matrix passes
```

## Thứ tự triển khai tôi khuyên

Làm đúng thứ tự này để không phải quay lại đập kiến trúc lần nữa:

1. **Stable deployer + staged release directory**
2. **Single canonical `current-release.json`**
3. **Split compose + `component-map.yaml`**
4. **Failover-controller first-class deployment**
5. **Worker deploy protocol v2 + bỏ legacy 404**
6. **72h durable notification + dynamic timer**
7. **Realtime retention-gap + `rows.Err()`**
8. **Frontend blue/green**
9. **Atomic crash recovery/reconciliation**
10. **Secret permission hardening**
11. **Runtime drift detector**
12. **Branch protection/ruleset**
13. **Observability/SLO**
14. **Chaos matrix + restore drill**
15. **Final CI production-contract gate**

Sau bộ này, phần mềm của bạn không còn kiểu “thêm một fix nhỏ rồi lần sau tôi lại chỉ ra thêm một lỗ cùng loại”. Những thay đổi còn lại sau đó chủ yếu sẽ là **feature/business logic**, không phải tiếp tục vá nền tảng deploy/realtime/reliability.

Với single VPS, đây là kiến trúc tôi sẽ lấy làm **baseline cuối** cho repo ACB và sau đó dùng cùng framework này làm chuẩn cho các repo Docker Compose khác của bạn.

