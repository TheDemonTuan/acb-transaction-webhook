# GitHub Actions Pre-Deploy Gates & Production Contract Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` hoặc `superpowers:executing-plans` để triển khai task-by-task. Mỗi task phải có regression test trước khi thay đổi production logic.

**Goal:** Đưa phần lớn lỗi deployment về GitHub Actions hoặc preflight trước rollout; đồng thời xử lý an toàn lỗi worker legacy đang chặn production hiện tại.

**Architecture:** Windows developer machine chỉ code và push. GitHub Actions kiểm tra code, Compose, Docker runtime contract và deployment protocol. VPS có một preflight không thay đổi production workload trước khi `dispatch-rollout.sh` được phép chạy. Worker singleton có protocol capability rõ ràng và một cơ chế one-time legacy handoff an toàn để nâng worker cũ chưa có `quiesce`.

**Tech Stack:** GitHub Actions, Docker/Compose, Buildx, Bash, Go, Bun, pytest, ShellCheck, Trivy, Cosign, SSH, SQLite, existing blue/green + transactional deployment.

**Spec:** Tài liệu này là implementation contract.

---

# 1. Các lỗi thực tế đã thấy

## Deploy failure classes

| Run | Loại lỗi | Vì sao CI không bắt |
|---|---|---|
| trước đây | Compose `uid/gid/mode` không hợp lệ | CI chưa dùng parser Compose production thật |
| #190 | Bark runtime/health contract | CI không start Bark theo production config |
| #191 | Bark không ghi được `/data/bark.db` | Không test volume ownership thực tế |
| #192 | Bark không đọc được secret | Không test secret mount/runtime UID |
| #193 | Worker cũ không có `-quiesce` | Worker deploy test giả định old worker đã hỗ trợ protocol mới |

Current Compose đã bỏ `uid/gid/mode` khỏi top-level secrets, tức lỗi Compose trước đây đã được sửa trong main. 

Nhưng nguyên nhân hệ thống vẫn còn:

```text
CI kiểm syntax / unit
        ↓
Build image
        ↓
Scan / Sign
        ↓
VPS
        ↓
Mới chạy production contract
        ↓
FAIL
```

Mục tiêu mới:

```text
GitHub CI
   ↓
Production contract simulation
   ↓
Build / Scan / Sign
   ↓
VPS preflight
   ↓
PASS?
 ┌─┴─┐
NO  YES
│    │
STOP rollout
```

---

# 2. Root cause lỗi hiện tại #193

Current `deploy-worker.sh` quy định:

```text
old worker
   ↓
quiesce
   ↓
chỉ khi quiesce thành công
   ↓
stop old worker
   ↓
start candidate
```

Đây là invariant tốt và **không nên bỏ**. Current source ghi rõ old worker chỉ được stop sau khi quiesce thành công. 

Nhưng production worker đang chạy là phiên bản cũ:

```text
/worker

supports:
-healthcheck
-liveness-check
-readiness-check

không có:
-quiesce
-drain
```

Trong khi current source mới đã thêm:

```text
-quiesce
-drain
```

và `workerService.Quiesce()` thực sự pause scheduler/history/dispatcher/maintenance và persist session. 

Đây là **protocol bootstrap problem**:

```text
deploy protocol v2
       ↓
đòi current worker support v2
       ↓
current worker là v1
       ↓
không thể tự nâng cấp
```

Không thể sửa bằng retry Action.

---

# 3. Một vấn đề khác: promotion scope hiện quá dễ thành full-stack

`scripts/compute-promotion-scope.sh` hiện làm:

```text
LAST_RELEASE_COMMIT?
    │
    ├─ có → dùng
    │
    └─ không
         ↓
      HEAD~1?
         │
         ├─ có
         │
         └─ không
              ↓
           empty tree
```



GitHub checkout mặc định của workflow hiện không yêu cầu full history.

Khi parent commit không có trong checkout:

```text
HEAD~1 lookup fail
      ↓
empty tree
      ↓
git diff entire repository
      ↓
schema = true
worker = true
gateway = true
platform = true
...
```

Điều đó giải thích vì sao nhiều Bark fix vẫn có:

```text
Authorized Promotion Scope:
[schema,auth_browser,tts,bark,worker,gateway,platform]
```

Đây là fail-safe, nên không phải lỗi nguy hiểm kiểu bỏ sót deploy, nhưng quá rộng và làm các component không liên quan bị rollout.

**Không sửa bằng cách đơn giản lấy commit trước đó.**

Nếu deploy trước đó fail ở giữa:

```text
commit A
↓
Bark pass
Worker fail

commit B
↓
chỉ diff A..B
```

thì có thể bỏ sót worker chưa promote thành công.

Baseline đúng phải là:

> commit của **last successful production release**, hoặc state deployment component đã được xác nhận.

---

# PHASE 0 — FIX PRODUCTION BLOCKER HIỆN TẠI

# Task 1 — Thêm worker deploy protocol capability

## Files

Modify:

```text
cmd/worker/main.go
deploy/deploy-worker.sh
deploy/tests/test_worker_deploy.sh
```

## Interface mới

Worker binary có:

```text
-deploy-capabilities
```

Output JSON:

```json
{
  "protocol": 2,
  "quiesce": true,
  "drain": true,
  "readiness": true
}
```

Không initialize DB.

Không start polling.

Không cần secret.

Không gọi ACB.

## Test trước

Trong `deploy/tests/test_worker_deploy.sh`, mock Docker phải hỗ trợ ba worker class:

```text
CURRENT
LEGACY
BROKEN
```

Thêm:

```bash
export MOCK_WORKER_PROTOCOL=current
```

Khi current:

```text
docker exec acb-worker /worker -deploy-capabilities
→ protocol 2
```

Khi legacy:

```text
docker exec ...
→ flag provided but not defined
```

## deploy-worker.sh

Thêm:

```bash
detect_running_worker_protocol()
```

Return:

```text
2       current protocol
legacy  old worker
none    no existing worker
error   unexpected failure
```

Flow:

```text
running worker?
      │
      ├─ no
      │    → candidate startup
      │
      └─ yes
           ↓
      detect protocol
         /      \
       v2      legacy
       │         │
   quiesce    special handoff
```

Không dựa vào parse generic `-h` lâu dài.

Dùng machine-readable capability command.

## Candidate contract

Trước khi đụng old worker:

```bash
docker run --rm \
  --network none \
  --entrypoint /worker \
  "$CANDIDATE_WORKER_IMAGE" \
  -deploy-capabilities
```

Candidate phải trả:

```json
protocol >= 2
quiesce = true
readiness = true
```

Nếu không:

```text
DEPLOY_E_CANDIDATE_WORKER_PROTOCOL
```

và abort khi production chưa bị thay đổi.

## Acceptance

Future regression như xóa `-quiesce` khỏi worker sẽ fail trước deployment.

Commit:

```text
feat(worker): expose deployment protocol capabilities
```

---

# Task 2 — One-time legacy worker handoff

Không được sửa lỗi hiện tại bằng:

```text
quiesce fail
→ vẫn docker stop worker
```

một cách tự động.

Đó sẽ phá invariant hiện tại.

Thay vào đó tạo **explicit one-time migration mode**.

## Workflow input

Modify:

```text
.github/workflows/deploy.yml
```

Thêm:

```yaml
workflow_dispatch:
  inputs:
    deploy:
      type: boolean
      default: false

    allow_legacy_worker_handoff:
      description: Allow one-time upgrade from a worker without deploy protocol v2
      type: boolean
      default: false
```

Push thông thường:

```text
allow legacy = false
```

Chỉ manual production migration mới bật:

```text
true
```

## deploy-worker.sh

Environment:

```text
ALLOW_LEGACY_WORKER_HANDOFF=0|1
```

Flow legacy:

```text
running worker = legacy
       ↓
ALLOW_LEGACY_WORKER_HANDOFF?
       │
  ┌────┴────┐
 NO        YES
 │           │
abort     active-auth=0
           ↓
       mutation gate
           ↓
      verify previous
      immutable digest
           ↓
     graceful SIGTERM
     docker stop -t 30
           ↓
      start candidate
           ↓
      readiness PASS?
       /          \
     NO           YES
     │             │
 rollback old     commit
 image digest
```

## Error khi flag chưa bật

Phải rõ:

```text
DEPLOY_E_LEGACY_WORKER_HANDOFF_REQUIRED

Running worker does not support deployment protocol v2.
No container was stopped.
Run the manually-approved deployment with
allow_legacy_worker_handoff=true to perform the one-time migration.
```

Không generic:

```text
Failed to quiesce worker
```

## Tại sao cần one-time mode

Không có cách nào yêu cầu binary cũ chạy RPC mà binary đó chưa implement.

Vì vậy transition:

```text
legacy worker
      ↓
one controlled SIGTERM handoff
      ↓
protocol-v2 worker
      ↓
mọi deploy tương lai:
quiesce → stop → replace
```

Chỉ deployment đầu tiên có khoảng gián đoạn polling rất ngắn.

Sau đó không dùng legacy path nữa.

## Rollback

Nếu candidate fail:

```text
new candidate
   ↓ fail
remove
   ↓
PREV_WORKER_REF
   ↓
start
   ↓
readiness
```

Không update `.release.env` cho tới khi candidate ready.

## Regression tests

Thêm test:

```text
legacy + allow=0
→ fail
→ old worker chưa stop

legacy + allow=1
→ old worker graceful stop
→ candidate ready
→ success

legacy + allow=1
→ candidate fail
→ previous worker restored

protocol v2
→ quiesce path
→ legacy handoff KHÔNG chạy
```

Commit:

```text
fix(deploy): support explicit legacy worker handoff
```

---

# Task 3 — Bỏ misleading localhost worker fallback

Current deploy script default:

```bash
WORKER_RPC_URL=http://127.0.0.1:8190
```



Nhưng production Compose chỉ:

```yaml
expose:
  - "8190"
```

không:

```yaml
ports:
  - "8190:8190"
```



Vì vậy:

```text
VPS host
127.0.0.1:8190
```

không phải endpoint mặc định hợp lệ.

## New policy

Primary:

```text
docker exec acb-worker /worker -quiesce
```

Nếu worker protocol v2.

Fallback HTTP chỉ chạy khi:

```text
WORKER_RPC_URL explicitly configured
```

Không tự default `127.0.0.1`.

Pseudo:

```bash
if docker_exec_quiesce; then
    return 0
fi

if [[ -n "${WORKER_RPC_URL:-}" ]]; then
    explicit_rpc_quiesce
fi

return 1
```

Không publish RPC port ra host chỉ để phục vụ deploy.

Giữ network attack surface nhỏ.

Commit:

```text
fix(deploy): remove unreachable worker host rpc fallback
```

---

# PHASE 1 — FIX PROMOTION SCOPE

# Task 4 — Không cho compute-promotion-scope tự đoán empty tree

## Modify

```text
scripts/compute-promotion-scope.sh
scripts/test-promotion-scope.sh
```

Current implicit fallback:

```text
HEAD~1 unavailable
→ empty tree
```

phải bỏ.

Thêm explicit:

```text
--initial-release
```

Chỉ khi caller chủ động truyền flag này mới được diff empty tree.

Nếu:

```text
base missing
HEAD~1 unavailable
initial-release=false
```

thì:

```text
exit 2
DEPLOY_E_PROMOTION_BASE_UNKNOWN
```

## Test

```text
base available
→ calculate normal

base missing
→ fail

base missing + --initial-release
→ intentional full scope
```

Điều này biến:

```text
silent full deploy
```

thành:

```text
explicit full deploy
```

Commit:

```text
fix(ci): fail closed on unknown promotion baseline
```

---

# Task 5 — Resolve baseline từ last successful production release

## Create

```text
scripts/resolve-promotion-base.sh
scripts/test-resolve-promotion-base.sh
```

Không dùng blindly:

```text
HEAD~1
```

Baseline:

```text
latest successful Build and deploy gateway run
before current commit
```

Workflow cần:

```yaml
permissions:
  contents: read
  actions: read
```

`resolve-promotion-base.sh` dùng GitHub API:

```bash
gh api \
  "/repos/${GITHUB_REPOSITORY}/actions/workflows/deploy.yml/runs?branch=main&status=success&per_page=20"
```

Lấy:

```text
head_sha
```

của successful deploy gần nhất.

Output:

```text
BASE_SHA=<40-char-sha>
BASE_SOURCE=last_successful_deploy
INITIAL_RELEASE=false
```

Nếu chưa từng success:

```text
INITIAL_RELEASE=true
BASE_SOURCE=initial_release
```

Không silently chọn empty tree.

## Why last successful deploy

Ví dụ:

```text
Release A → SUCCESS

B → Bark fail
C → Worker fail
D → current candidate
```

Base phải là:

```text
A
```

không phải:

```text
C
```

để các thay đổi chưa deploy thành công ở B/C vẫn được retry.

Đây là fail-safe đúng.

---

# Task 6 — Tạo `release-plan` job trước build

Modify:

```text
.github/workflows/deploy.yml
```

Flow hiện tại:

```text
verify
↓
build everything
↓
scan/sign
↓
compute promotion scope
```

Đổi:

```text
verify
↓
release-plan
↓
build
↓
scan/sign
↓
deploy
```

`release-plan`:

```yaml
release-plan:
  needs: verify
  runs-on: ubuntu-latest
  permissions:
    contents: read
    actions: read
```

Checkout:

```yaml
with:
  fetch-depth: 0
```

Steps:

```text
resolve last successful deploy SHA
↓
compute promotion scope
↓
validate result
↓
upload promotion-scope.json
```

Output phải log:

```text
Candidate: cc714e...
Baseline:  abcdef...
Source:    last_successful_deploy

Promotion:
schema=false
auth_browser=false
tts=false
bark=true
worker=true/false
gateway=...
platform=...
```

Không đợi đến VPS mới biết cái gì sắp bị thay.

---

# 4. Lưu ý về full promotion hiện tại

Không được mặc định coi:

```text
Bark commit
→ worker=false
```

vì worker có thể vẫn đang pending từ release fail trước.

Do đó:

```text
source change classification
+
last successful production baseline
```

mới là nguồn đúng.

Future phase có thể làm tốt hơn bằng per-component deployed-state, nhưng chưa cần cho fix hiện tại.

---

# PHASE 2 — BẮT CÁC LỖI #187–#192 NGAY TRÊN GITHUB

# Task 7 — Real Docker Compose parser gate

## Create

```text
scripts/ci/validate-production-compose.sh
```

Script:

```text
dummy immutable image refs
synthetic .env.production
synthetic non-secret secret files
↓
docker compose config --quiet
```

Không:

```text
up
pull
stop
restart
```

Phải dùng chính:

```text
deploy/compose.prod.yaml
```

và Docker Compose thật.

## Bắt

```text
invalid YAML
invalid secret field
invalid service field
missing variable
invalid volume syntax
invalid network syntax
bad interpolation
```

Lỗi Compose `uid/gid/mode` trước đây phải fail ở đây.

## Regression

Inject invalid field:

```yaml
secrets:
  app_master_key:
    file: ./secrets/app_master_key
    impossible_property: foo
```

Expected:

```text
GitHub CI red
```

trước build image.

---

# Task 8 — Production Bark runtime contract test

Đây là missing gate lớn nhất hiện nay.

CI hiện chỉ có Docker smoke:

```text
gateway
auth-browser
tts-gateway
```

không có Bark. 

Trong khi `smoke-test-bark.sh` hiện chỉ probe một Bark đã chạy sẵn; bản thân nó không chứng minh production Compose có thể start Bark. 

## Create

```text
deploy/tests/test_bark_runtime_contract.sh
```

## Test bằng actual production configuration

Setup ephemeral:

```text
Docker networks
- edge-acb
- acb-core
- acb-egress

Docker volumes
- gateway_data test
- bark_data test

synthetic secrets
- bark_basic_auth_user
- bark_basic_auth_password
```

Sau đó:

```bash
docker compose \
  --env-file "$TEST_ENV" \
  -f deploy/compose.prod.yaml \
  up -d --no-deps bark
```

Với actual pinned:

```text
BARK_IMAGE_REF
```

## Verify

1. container không exit
2. `/data/bark.db` tạo được
3. basic auth secret đọc được
4. health status `healthy`
5. `/ping` = 200
6. authenticated request hoạt động
7. rootfs vẫn read-only
8. persistent data volume writable đúng chỗ

Cleanup:

```text
container
test volume
test networks
test secrets
```

không để state runner.

## Lỗi sẽ bắt được

Run #190 class:

```text
invalid Bark startup flags
```

Run #191 class:

```text
/data/bark.db permission denied
```

Run #192 class:

```text
/run/secrets/... permission denied
```

Commit:

```text
test(ci): add production bark runtime contract
```

---

# Task 9 — Worker image deploy-contract smoke

## Add CI job

```text
docker-contract-worker
```

Build:

```yaml
target: worker
load: true
push: false
```

Run:

```bash
docker run --rm \
  --network none \
  --entrypoint /worker \
  worker:contract \
  -deploy-capabilities
```

Validate JSON:

```text
protocol >= 2
quiesce == true
readiness == true
```

Sau đó:

```bash
docker run ... /worker -h
```

phải chứa:

```text
-deploy-capabilities
-quiesce
-readiness-check
-liveness-check
```

Nếu developer vô tình xóa deploy control flags:

```text
PR CI fail
```

chứ không đợi worker production mới fail.

---

# Task 10 — Legacy worker deployment regression test

Current `test_worker_deploy.sh` mock `docker exec ... -quiesce` mặc định luôn trả thành công. 

Đó là lý do current worker incompatibility không được test.

Thêm mock:

```bash
if [[ "$MOCK_WORKER_PROTOCOL" == "legacy" ]] &&
   [[ "$*" =~ -deploy-capabilities|-quiesce ]]
then
    echo "flag provided but not defined"
    exit 2
fi
```

Required test matrix:

```text
v2 / normal
v2 / quiesce fail
legacy / handoff disabled
legacy / handoff enabled
legacy / candidate fail + rollback
no current worker
candidate protocol invalid
```

Acceptance:

```text
mọi branch của deploy-worker.sh
có regression test
```

---

# PHASE 3 — SHARED GITHUB PREFLIGHT

# Task 11 — `scripts/ci/preflight.sh`

## Create

```text
scripts/ci/preflight.sh
```

Đây là single source cho non-image validations:

```text
frontend typecheck/build/tests
Go tests
Go vet
pytest
Python compile
bash -n
shellcheck
promotion scope unit tests
dispatcher tests
deployment failure tests
production Compose parser
supply chain validators
git diff --check
```

Then:

```text
ci.yml
     \
      → preflight.sh
     /
deploy.yml
```

Không copy validation quan trọng thành hai phiên bản khác nhau.

---

# Task 12 — Required CI Gate

Add final job:

```text
Required CI Gate
```

Depends on:

```text
verify
docker-smoke-gateway
docker-smoke-auth-browser
docker-smoke-tts-gateway
docker-contract-worker
docker-contract-bark
```

Pseudo:

```yaml
required-ci:
  if: always()
  needs:
    - verify
    - docker-smoke-gateway
    - docker-smoke-auth-browser
    - docker-smoke-tts-gateway
    - docker-contract-worker
    - docker-contract-bark
```

Bất kỳ job nào không `success`:

```text
Required CI Gate = FAIL
```

Ruleset main chỉ cần require check này.

---

# PHASE 4 — VPS PREFLIGHT TRƯỚC KHI MUTATE COMPONENT ĐẦU TIÊN

# Task 13 — Production preflight

## Create

```text
deploy/preflight-production.sh
deploy/tests/test_production_preflight.sh
```

Điểm quan trọng nhất:

> preflight phải chạy trước **Schema Transaction 1**.

Current worker failure chỉ được phát hiện sau:

```text
schema
auth-browser
TTS
Bark
```

đã chạy xong.

Đó là quá muộn.

New flow:

```text
manifest verified
       ↓
GLOBAL PREFLIGHT
       ↓
mọi component prerequisites PASS?
       │
     NO│YES
       │
 STOP  ↓
      schema
      browser
      tts
      bark
      worker
      gateway
```

## General checks

```text
Docker daemon reachable
Docker Compose version
canonical .env.production
secrets directory
secret permissions
release manifest
manifest signatures
immutable image refs
external volumes
external networks
disk capacity
seccomp profile
docker compose config
```

---

# Task 14 — Promotion-aware component preflight

Preflight đọc:

```json
promotion_scope
```

từ signed release manifest.

Không test component không promotion.

Ví dụ:

```text
promotion_scope = bark
```

thì:

```text
general checks
Bark checks
```

không:

```text
worker handoff
schema migration prerequisite
gateway switch prerequisite
```

Nếu:

```text
worker=true
```

thì bắt buộc chạy worker compatibility handshake.

---

# Task 15 — Worker remote capability handshake

Trước Transaction 1:

```text
candidate worker capabilities
       ↓
running worker capabilities
```

Candidate:

```bash
docker run --rm \
  --network none \
  --entrypoint /worker \
  "$WORKER_IMAGE" \
  -deploy-capabilities
```

Current worker:

```bash
docker exec acb-worker \
  /worker \
  -deploy-capabilities
```

Results:

### Current supports v2

```text
PASS
normal quiesce deployment allowed
```

### Current is legacy

If:

```text
ALLOW_LEGACY_WORKER_HANDOFF=0
```

then preflight returns:

```text
DEPLOY_E_LEGACY_WORKER_HANDOFF_REQUIRED
```

**before schema/Bark/etc are touched.**

If manual rollout has:

```text
ALLOW_LEGACY_WORKER_HANDOFF=1
```

preflight says:

```text
PASS WITH LEGACY HANDOFF
```

and records warning.

This single check would have caught #193 near the start of deployment.

---

# Task 16 — Bark candidate sandbox on VPS

GitHub CI already tests Bark, nhưng VPS architecture/runtime có thể khác.

Before production rollout:

```text
candidate Bark
+
ephemeral test volume
+
ephemeral secret files
```

Start:

```text
acb-preflight-bark-<release-id>
```

Không sử dụng:

```text
production bark_data
production container name
production ports
```

Verify:

```text
startup
database creation
secrets
health
```

Cleanup immediately.

Điều này không phải strictly read-only với Docker daemon vì có ephemeral resources, nhưng:

```text
production runtime mutation = 0
```

Không stop/restart production.

---

# PHASE 5 — WIRE PREFLIGHT VÀ ROLLOUT

# Task 17 — Tách rõ deployment stages

Current conceptual flow:

```text
Sync deployment files
↓
Deploy immutable image
```

Đổi thành:

```text
Stage release files
↓
Verify staged release
↓
Production preflight
↓
Candidate sandbox checks
↓
Production rollout
↓
Soak
```

`.github/workflows/deploy.yml`:

```text
Verify
Release Plan
Build
Security
Stage
Preflight
Rollout
```

Không dùng một step khổng lồ tên:

```text
Deploy immutable image
```

cho tất cả lỗi.

Như vậy Actions UI sẽ nói ngay:

```text
Production preflight ❌
```

hay:

```text
Worker promotion ❌
```

---

# Task 18 — Không sửa canonical Compose trước rollout success

Giữ staging:

```text
compose.prod.yaml.next
```

Preflight chạy trên:

```text
.next
```

Rollout cũng sử dụng staged version.

Chỉ sau success:

```text
cp compose.prod.yaml.next compose.prod.yaml
```

Nếu failure:

```text
canonical compose
production containers
```

giữ nguyên.

---

# PHASE 6 — DEPLOY DIAGNOSTICS

# Task 19 — Structured deploy error codes

Mỗi script deployment phải có error classes.

Ví dụ:

```text
DEPLOY_E_COMPOSE_SCHEMA
DEPLOY_E_BARK_VOLUME
DEPLOY_E_BARK_SECRET
DEPLOY_E_WORKER_PROTOCOL
DEPLOY_E_LEGACY_WORKER_HANDOFF_REQUIRED
DEPLOY_E_WORKER_QUIESCE
DEPLOY_E_WORKER_READINESS
DEPLOY_E_SCHEMA_BACKUP
DEPLOY_E_ACTIVE_AUTH
DEPLOY_E_TRAEFIK_SWITCH
```

Log:

```text
[ERROR] code=DEPLOY_E_WORKER_PROTOCOL
component=worker
stage=preflight
mutation_started=false
```

GitHub Actions nhìn vào log là biết ngay loại lỗi.

---

# Task 20 — Safe diagnostic artifact on failure

Create:

```text
deploy/collect-deploy-diagnostics.sh
```

Whitelisted only:

```text
release ID
git SHA
promotion scope
transaction state
container name
image digest
running/exited
health status
restart count
last 100 logs của failed service
```

Không dump:

```text
docker inspect Env
secret values
.env.production contents
tokens
passwords
cookies
```

GitHub step:

```yaml
- name: Collect deploy diagnostics
  if: failure()
```

Upload:

```text
deploy-diagnostics-<run-id>
```

Khi lỗi lần sau không phải SSH thủ công tìm log.

---

# PHASE 7 — MAIN BRANCH GATE

# Task 21 — Main branch Ruleset

Sau khi `Required CI Gate` tồn tại:

```text
Settings
→ Rules
→ Rulesets
→ main
```

Require:

```text
Pull Request
Required CI Gate
branch up to date
block force push
block deletion
```

Bạn làm một mình có thể:

```text
required approvals = 0
```

Mục tiêu là CI gate, không phải ép code review.

---

# PHASE 8 — ACTION RESOURCE OPTIMIZATION

Chỉ làm sau correctness.

Hiện workflow build tất cả image cho mỗi release.

Future:

```text
release-plan
↓
promotion scope
↓
build only affected image
```

Nhưng unchanged component digest phải lấy từ trusted previous release state.

Không implement trước khi production state tracking ổn định.

Correctness quan trọng hơn tiết kiệm vài phút Actions.

---

# 5. Thứ tự implementation chính xác

## P0 — Unblock production

```text
1. worker -deploy-capabilities
2. legacy worker detection
3. explicit legacy handoff
4. worker regression tests
5. remove implicit localhost RPC fallback
```

Sau đó chạy manual deployment:

```text
workflow_dispatch
deploy=true
allow_legacy_worker_handoff=true
```

Chỉ cần cho lần transition này.

Sau khi worker v2 chạy:

```text
mọi automatic deploy sau
→ normal quiesce path
```

---

## P1 — Ngăn current class tái diễn

```text
6. production Compose parser
7. worker contract Docker test
8. Bark production runtime contract
9. Required CI Gate
```

---

## P2 — Fix promotion planning

```text
10. remove implicit empty-tree fallback
11. resolve last successful production baseline
12. add release-plan job
13. sign promotion scope into manifest
```

---

## P3 — Catch production incompatibility trước mutation

```text
14. production preflight
15. worker capability handshake
16. Bark sandbox
17. wire before dispatch-rollout
```

---

## P4 — Operations

```text
18. structured error codes
19. safe failure diagnostics
20. main branch ruleset
21. runbook
```

---

# 6. GitHub CI target architecture

```text
Developer Windows
       │
       │ git push
       ▼
┌──────────────────────────────┐
│ GitHub PR CI                 │
│                              │
│ Go / Bun / Python            │
│ Shell tests                  │
│ Deploy tests                 │
│ Compose parser               │
│ Gateway contract             │
│ Auth-browser contract        │
│ TTS contract                 │
│ Worker deploy protocol       │
│ Bark production contract     │
└─────────────┬────────────────┘
              │
              ▼
       Required CI Gate
              │
              ▼
           Merge main
              │
              ▼
       Production workflow
              │
              ▼
         Release Plan
              │
              ▼
       Build / Scan / Sign
              │
              ▼
          Stage on VPS
              │
              ▼
┌──────────────────────────────┐
│ Production Preflight         │
│                              │
│ Compose                      │
│ Host                         │
│ Volume/network               │
│ Secrets                      │
│ Candidate protocols          │
│ Running-worker compatibility │
│ Candidate sandbox            │
└─────────────┬────────────────┘
              │
         ┌────┴────┐
       FAIL       PASS
         │          │
        STOP        ▼
              dispatch-rollout
                    │
              transactional deploy
                    │
              rollback / soak
```

---

# 7. Definition of Done

Không coi plan hoàn tất cho tới khi prove được:

### Compose

```text
invalid Compose property
→ PR CI FAIL
```

### Bark

```text
cannot write bark.db
→ PR CI FAIL

cannot read Bark secret
→ PR CI FAIL

invalid Bark command
→ PR CI FAIL
```

### Worker candidate

```text
missing -quiesce
→ PR CI FAIL
```

### Legacy running worker

```text
legacy current worker
+
normal automatic deploy
→ production preflight FAIL
→ zero production mutation
```

### One-time migration

```text
legacy current worker
+
manual allow flag
→ controlled SIGTERM handoff
→ candidate starts
→ ready
→ release committed
```

### Candidate worker failure

```text
legacy/current worker
→ candidate fail
→ previous immutable image restored
```

### Future worker upgrades

```text
protocol v2
→ quiesce
→ stop
→ start candidate
→ readiness
→ commit
```

### Promotion scope

```text
unknown baseline
→ never silently empty-tree

last successful deployment known
→ diff against that baseline
```

### Deployment UI

Không còn chỉ:

```text
Deploy immutable image failed
```

mà phải biết:

```text
Production Preflight
  ↳ Worker compatibility
  ↳ DEPLOY_E_LEGACY_WORKER_HANDOFF_REQUIRED
```

---

# 8. Việc không nên làm

Không:

```text
publish worker RPC 8190 ra host chỉ để deploy
```

Không:

```text
quiesce fail → stop worker anyway
```

Không:

```text
remove quiesce safety gate
```

Không:

```text
retry Action #193 y nguyên
```

Không:

```text
assume unit tests = production contract tests
```

Không:

```text
HEAD~1 missing → silently diff empty tree
```

Không:

```text
dùng production secrets thật trong GitHub CI
```

---

# 9. Fix trực tiếp cho trạng thái hiện tại

Trạng thái hiện tại:

```text
Bark issue đã vượt qua
↓
Worker legacy compatibility đang chặn
```

Việc cần làm trước:

```text
add deploy protocol capability
↓
add legacy migration path
↓
add regression tests
↓
manual one-time handoff
↓
worker v2 production
```

Sau đó triển khai toàn bộ preflight/contract gates để lần sau không phải sửa theo kiểu:

```text
push
↓
đợi build
↓
VPS fail
↓
fix
↓
push lại
```

Mục tiêu cuối:

```text
85–95% deployment mistakes
→ chết trước rollout

production-specific mismatch còn lại
→ chết tại VPS preflight

chỉ runtime failure thật sự
→ được phép đi vào transactional rollout/rollback
```