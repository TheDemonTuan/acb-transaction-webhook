# ACB Zero-Downtime Final Convergence Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Hoàn tất kiến trúc deploy production fail-closed/zero-downtime trên single VPS theo một đợt duy nhất: mọi mutable runtime state dùng chung một layout, rollback phục hồi đúng *toàn bộ previous release bundle* chứ không chỉ image, worker chỉ dừng sau quiesce v2, gateway/frontend B/G rollback an toàn, failover controller không đánh nhau với deploy, runtime drift bị phát hiện đầy đủ, migration luôn backward-compatible, và production chỉ chạy sau full contract/chaos gates.

**Architecture:** Tách rõ `immutable release bundle` và `shared mutable runtime state`. Mỗi release được stage vào `releases/<release_id>` và được ký/hashing; `state/current-release.json` schema v2 là nguồn chân lý duy nhất, trỏ tới exact previous release directory để rollback sử dụng đúng compose/scripts/config của release trước. Tất cả path state/lock/journal/slot/backup được resolve từ một runtime layout duy nhất; CI dùng release simulator + chaos matrix để chứng minh candidate failure luôn đưa runtime trở về canonical release trước khi được phép deploy production.

**Tech Stack:** Bash, Docker Compose, Go, Python 3, SQLite/WAL, Traefik, systemd, GitHub Actions, Cosign/Sigstore, GHCR.

**Spec:** `zero_downtime_lastfix.md` (plan này supersede phần implementation còn thiếu và là checklist cuối để đóng hẳn zero-downtime).

## Global Constraints

- Single VPS; không tuyên bố HA khi VPS/disk/provider chết.
- ACB polling worker phải chạy liên tục trong mọi deploy không liên quan tới worker; chỉ chấp nhận bounded gap khi *explicit worker promotion*.
- Không dùng Redis chỉ để giải quyết deploy/realtime.
- Không dùng `docker compose down`, không recreate whole stack, không `up` service ngoài promotion scope.
- Worker protocol v2 là bắt buộc; không có legacy bypass.
- Mọi image production phải immutable `@sha256`.
- Component deploy scripts chỉ được gọi qua release orchestrator.
- Candidate release không được ghi trực tiếp canonical release state.
- `state/current-release.json` là nguồn chân lý duy nhất sau bootstrap; `.release.env` chỉ là compatibility projection và không được dùng để tự cứu khi canonical state hỏng/mất.
- Mọi ambiguity (slot, route, state, release dir, config hash, rollback target) phải fail closed.
- Schema migration là forward-only nhưng phải backward-compatible với previous gateway/worker trong suốt rollout/rollback window.
- Mọi rollback phải phục hồi previous release **image + compose config + route config + platform/failover assets**, không chỉ image digest.
- Production GitHub Actions phải serialized (`cancel-in-progress: false`) và chỉ chạy sau required checks/ruleset.

---


## Incident Addendum — Runs #247 and #248 must be solved by this plan, not patched separately

This plan now explicitly includes the two latest production failures:

```text
run #247 / 373bf3d32f31fec3c484b5130d316689a0de89d7
  schema/auth-browser/TTS/Bark/frontend/worker       PASS
  gateway candidate + edge ACK                      PASS
  gateway 900s soak                                 PASS
  failover-controller install                       PASS
  candidate canonical-state/runtime drift check     FAIL
  gateway route rollback                            PASS
  frontend rollback evidence                        FAIL (old_slot=legacy)
  final release rollback                            INCOMPLETE

run #248 / b2433caf57476697595da528c262eada90e4e714
  manifest/build/sign                               PASS
  rollout start                                     BLOCKED
  previous interrupted rollout reconciliation       FAIL
  error                                              frontend rollback evidence malformed
```

### Root cause A — post-soak candidate state is built through a stale compatibility projection

The failed runtime was correct: the new blue gateway was healthy and had the new candidate digest. The expected state used by `verify-runtime-drift.sh` still contained the old blue digest. Therefore the fault boundary is **candidate release-state construction**, not gateway rollout or soak.

The final architecture must obey:

```text
signed manifest + previous canonical state + deterministic planned slots
                         |
                         v
                candidate-release.json
                         |
               pre-soak runtime verify
                         |
                     900s soak
                         |
                final runtime verify
                         |
                  atomic canonical commit
                         |
                         v
              regenerate .release.env projection
```

`.release.env` MUST NOT be an input to candidate canonical-state construction. It is a projection generated **after** canonical commit only.

### Root cause B — frontend legacy-to-B/G transition produces valid evidence that rollback rejects

`deploy-frontend.sh` intentionally supports `ACTIVE_FRONTEND_SLOT=legacy` and, with deferred retirement, persists that value as `old_slot=legacy`. The release-level `rollback_pending_routes()` currently accepts only `blue|green`; therefore a valid first B/G migration is later classified as malformed.

The rollback schema must explicitly support:

```text
frontend previous topology = legacy | blue | green
```

and for `legacy` must restore the legacy route/container, remove the B/G active-slot marker, verify edge ACK/availability, then stop the candidate B/G container.

### Root cause C — failed rollout creates a trusted-deployer bootstrap deadlock

The host executes the already-installed trusted `deploy/stable-deployer.sh`. Candidate `stable-deployer.sh` is copied into the trusted location only **after** `dispatch-rollout.sh` succeeds. Therefore a fix that exists only in candidate `stable-deployer.sh` cannot repair a production rollout that fails before the copy step.

This is exactly why `b2433caf` can contain the frontend shared-path fix while run #248 still starts under the old trusted deployer semantics.

Long-term rule:

```text
stable-deployer = tiny version-stable verifier/launcher only
candidate dispatch/runtime-layout = owns ALL runtime path semantics
```

A candidate must never require its new `stable-deployer.sh` to already be installed in order to deploy itself safely.

### Root cause D — rollback short-circuits and can leave a mixed runtime

Current cleanup logically behaves like:

```bash
rollback_pending_routes && rollback_completed_components
```

If frontend route rollback fails, component rollback is not attempted. After #247 this can leave candidate worker/auth-browser/TTS/Bark/failover assets alive while gateway route has already returned to the previous release.

Final rollback must be **best-effort exhaustive + final authoritative verification**, never short-circuit on the first failed restoration step.

---

## Final File/Module Map

### New files

- `deploy/runtime-layout.sh` — duy nhất nơi định nghĩa shared mutable paths.
- `deploy/bootstrap-deployment-engine.sh` — one-time/signed deployment-engine bootstrap that mutates no application runtime.
- `deploy/reconcile-release.sh` — deterministic recovery-only entrypoint for interrupted/dirty releases.
- `deploy/release-state.py` — validate/read/write canonical release state schema v2.
- `deploy/rollback-release.sh` — whole-release rollback dùng exact previous release bundle.
- `deploy/compose/base.yaml`
- `deploy/compose/gateway.yaml`
- `deploy/compose/frontend.yaml`
- `deploy/compose/worker.yaml`
- `deploy/compose/auth-browser.yaml`
- `deploy/compose/tts.yaml`
- `deploy/compose/bark.yaml`
- `deploy/compose/dbtool.yaml`
- `deploy/tests/test_runtime_layout.sh`
- `deploy/tests/test_bootstrap_recovery.sh`
- `deploy/tests/test_legacy_frontend_recovery.sh`
- `deploy/tests/test_release_state.sh`
- `deploy/tests/fixtures/release-state-v2.json`
- `deploy/tests/test_release_rollback.sh`
- `deploy/tests/test_drift_complete.sh`
- `deploy/tests/test_failover_bundle.sh`
- `deploy/tests/test_slot_resolution.sh`
- `deploy/tests/test_schema_compatibility.sh`
- `deploy/tests/test_release_crash_recovery.sh`
- `deploy/tests/test_component_map_coverage.sh`
- `deploy/tests/test_release_simulator.sh`
- `docs/runbooks/FINAL_RELEASE_CONTRACT.md`

### Modify

- `deploy/stable-deployer.sh`
- `deploy/lib/common.sh`
- `deploy/lib/state.sh`
- `deploy/lib/traefik.sh`
- `deploy/lib/database.sh`
- `deploy/dispatch-rollout.sh`
- `deploy/deploy-worker.sh`
- `deploy/deploy-gateway.sh`
- `deploy/deploy-frontend.sh`
- `deploy/deploy-auth-browser.sh`
- `deploy/deploy-tts.sh`
- `deploy/deploy-bark.sh`
- `deploy/deploy-schema.sh`
- `deploy/deploy-failover-controller.sh`
- `deploy/generate-release-manifest.sh`
- `deploy/verify-manifest.sh`
- `deploy/verify-runtime-drift.sh`
- `deploy/component-map.json`
- `deploy/stage-platform-assets.sh`
- `platform/failover/vps-failover-controller.py`
- `platform/failover/apps.d/acb.json`
- `platform/failover/vps-failover-controller.service`
- `scripts/compute-promotion-scope.sh`
- `.github/workflows/ci.yml`
- `.github/workflows/deploy.yml`
- `docs/runbooks/DEPLOYMENT_RUNBOOK.md`

---


# Task 0: Break the current production recovery deadlock before any new application rollout

**Files:**
- Create: `deploy/bootstrap-deployment-engine.sh`
- Create: `deploy/reconcile-release.sh`
- Create: `deploy/tests/test_bootstrap_recovery.sh`
- Create: `deploy/tests/test_legacy_frontend_recovery.sh`
- Modify: `deploy/stable-deployer.sh`
- Modify: `deploy/dispatch-rollout.sh`
- Modify: `deploy/deploy-frontend.sh`
- Modify: `.github/workflows/deploy.yml`

**Interfaces:**
- `bootstrap-deployment-engine.sh --release-dir <signed-release> --runtime-deploy-dir <dir>` upgrades only trusted deployment-engine files after verifying their signed artifact hashes; it never starts/stops containers, changes routes, migrates DB, or writes canonical release state.
- `reconcile-release.sh --runtime-root <root> --recovery-only` consumes the existing rollout journal/pending evidence and converges runtime back to the last successful canonical/legacy release.
- Production workflow exposes a **recovery-only phase before builds** whenever production state is dirty.

- [ ] **Step 1: Write regression fixture for run #247**

Create a fixture with:

```text
last successful gateway = green
failed candidate gateway = blue
frontend previous topology = legacy (acb-frontend)
frontend candidate = blue (acb-frontend-blue)
rollout journal = INTERRUPTED
pending gateway = old_slot=green,candidate_slot=blue
pending frontend = old_slot=legacy,candidate_slot=blue
completed components = schema,auth_browser,tts,bark,frontend,worker,gateway,failover_controller
canonical release = previous release
```

The test must fail with current code because `legacy` is rejected.

- [ ] **Step 2: Replace pending frontend env evidence with schema-validated JSON**

Required shape:

```json
{
  "schema_version": 1,
  "previous_topology": "legacy",
  "previous_container": "acb-frontend",
  "candidate_topology": "blue",
  "candidate_container": "acb-frontend-blue",
  "gateway_slot_at_switch": "green",
  "route_switched": true
}
```

For normal B/G previous topology, `previous_topology` is `blue` or `green`. Reject unknown values; do not overload `old_slot` with incompatible semantics.

- [ ] **Step 3: Implement exact legacy frontend rollback**

For `previous_topology=legacy`:

```text
verify acb-frontend exists + healthy/running
render route -> legacy frontend backend while keeping canonical gateway slot
atomic rename Traefik config
edge/availability ACK
remove FRONTEND_ACTIVE_SLOT_FILE
record previous topology = legacy
stop/remove candidate acb-frontend-blue only after ACK
remove pending evidence
```

If the legacy container is missing/unhealthy, fail closed and preserve evidence.

- [ ] **Step 4: Make release rollback non-short-circuiting**

Do not use:

```bash
rollback_pending_routes && rollback_completed_components
```

Use an accumulator:

```bash
rollback_failures=0
rollback_gateway_route || rollback_failures=$((rollback_failures + 1))
rollback_frontend_route || rollback_failures=$((rollback_failures + 1))
rollback_completed_components || rollback_failures=$((rollback_failures + 1))
verify_previous_runtime || rollback_failures=$((rollback_failures + 1))
(( rollback_failures == 0 )) || exit 1
```

Every restoration step must be attempted when it is independently safe. The journal becomes `ROLLED_BACK` only after final runtime verification against the previous authoritative state.

- [ ] **Step 5: Add a recovery-only production gate before builds**

`Read authoritative production state` must also report:

```text
rollout_status
pending_gateway_retire
pending_frontend_retire
runtime_dirty
canonical_generation
```

If `runtime_dirty=true`, run `reconcile-release.sh --recovery-only` before expensive image builds. If reconciliation fails, stop the workflow immediately; do not build/sign/deploy another candidate on top of mixed runtime.

- [ ] **Step 6: Implement a signed deployment-engine bootstrap path**

Because the currently installed trusted deployer cannot self-promote after a failed rollout, add a one-time/bootstrap-capable path:

```text
stage signed candidate release
old trusted verify-manifest verifies candidate manifest + artifact hashes
verify candidate deployment-engine file hashes against signed manifest
bash -n + --self-test candidate engine
install candidate files to *.next
fsync files + parent dir
atomic rename into runtime deploy dir
retain *.previous copy
run recovery-only reconcile
if engine installation/self-test fails before recovery begins -> restore previous engine files
once the verified fixed engine starts recovery, keep that engine installed even if runtime reconciliation fails so recovery capability is not lost; preserve journal/evidence and block new mutation
```

Allowed bootstrap files are an explicit allowlist only:

```text
stable-deployer.sh
verify-manifest.sh
runtime-layout.sh
reconcile-release.sh
release-state.py
```

This bootstrap MUST NOT touch Docker, Traefik, SQLite, secrets, or application release state.

- [ ] **Step 7: Make `stable-deployer.sh` version-stable**

Move all mutable/runtime path decisions out of trusted stable deployer. Its long-term responsibilities become only:

```text
validate arguments
verify signed candidate bundle using trusted verifier
pass --runtime-root / --release-dir to signed candidate dispatcher
atomically promote verifier/launcher after a successful release
```

`dispatch-rollout.sh` must source the candidate's signed `runtime-layout.sh` and resolve every shared path itself. Therefore a future runtime-layout fix no longer depends on first successfully updating stable-deployer.

- [ ] **Step 8: Prove failed run #247 converges before accepting a new candidate**

Test must assert after recovery:

```text
gateway route = previous green
frontend route/topology = previous legacy
candidate frontend B/G stopped
worker/auth-browser/TTS/Bark = previous release versions/config
failover-controller = previous release bundle
canonical/legacy last-success release unchanged
no pending retire evidence
rollout journal archived as ROLLED_BACK/RECONCILED
verify-runtime-drift(previous) = PASS
```

Schema is not downgraded; backward compatibility is verified by Task 10.

- [ ] **Step 9: Add bootstrap/recovery tests to CI**

Run:

```bash
bash deploy/tests/test_bootstrap_recovery.sh
bash deploy/tests/test_legacy_frontend_recovery.sh
bash deploy/tests/test_promotion_dispatcher.sh
```

Expected: PASS, including the exact `legacy -> blue -> failed release -> legacy` scenario.

- [ ] **Step 10: Commit**

```bash
git add deploy .github/workflows/deploy.yml
git commit -m "fix(deploy): recover interrupted releases before candidate rollout"
```

---

# Task 1: Freeze baseline and create a final hardening branch

**Files:**
- No production code change yet.
- Branch: `hardening/zero-downtime-final`

**Interfaces:**
- Consumes: current `main` SHA at implementation start.
- Produces: one branch containing *all* tasks below; production deploy remains main-only.

- [ ] **Step 1: Record baseline**

```bash
git fetch origin
git checkout main
git pull --ff-only
BASE_SHA="$(git rev-parse HEAD)"
printf '%s\n' "$BASE_SHA"
```

Expected: exact 40-character SHA; at time this plan was written the observed HEAD was `b2433caf57476697595da528c262eada90e4e714`.

- [ ] **Step 2: Create isolated branch/worktree**

```bash
git switch -c hardening/zero-downtime-final
```

- [ ] **Step 3: Verify deploy workflow does not deploy this branch**

Run:

```bash
grep -n -A4 '^  push:' .github/workflows/deploy.yml
```

Expected: deployment push trigger remains `branches: [main]` only.

- [ ] **Step 4: Commit branch marker only if repository policy requires it**

No production mutation is allowed from this branch.

---

# Task 2: Canonicalize every shared runtime path in one module

**Files:**
- Create: `deploy/runtime-layout.sh`
- Modify: `deploy/stable-deployer.sh`
- Modify: `deploy/lib/common.sh`
- Modify: `deploy/lib/state.sh`
- Modify: `deploy/lib/database.sh`
- Modify: `deploy/deploy-schema.sh`
- Modify: `platform/failover/vps-failover-controller.py`
- Modify: `platform/failover/vps-failover-controller.service`
- Create: `deploy/tests/test_runtime_layout.sh`

**Interfaces:**
- Produces exported variables:
- Critical invariant: production mutable paths are derived from `RUNTIME_ROOT` only; `SCRIPT_DIR`/`RELEASE_DIR` may locate immutable code/config but can never select journal/state/slot/backup locations.
  - `RUNTIME_ROOT`
  - `RUNTIME_DEPLOY_DIR`
  - `RUNTIME_STATE_DIR`
  - `RUNTIME_DATA_DIR`
  - `RUNTIME_RELEASES_DIR`
  - `ENV_FILE`
  - `RELEASE_ENV_FILE`
  - `SECRETS_DIR`
  - `BACKUP_DIR`
  - `CURRENT_RELEASE_FILE`
  - `TX_JOURNAL_FILE`
  - `ROLLOUT_JOURNAL_FILE`
  - `ACTIVE_SLOT_FILE`
  - `PREVIOUS_SLOT_FILE`
  - `FRONTEND_ACTIVE_SLOT_FILE`
  - `FRONTEND_PREVIOUS_SLOT_FILE`
  - `DEPLOY_STATE_FILE`
  - `SOAK_STATE_FILE`
  - `MIGRATION_RECORD`
  - `PENDING_GATEWAY_RETIRE_FILE`
  - `PENDING_FRONTEND_RETIRE_FILE`

- [ ] **Step 1: Write failing runtime-path test**

`deploy/tests/test_runtime_layout.sh` must source the layout with a fixture root and assert **no mutable path lives below `RELEASE_DIR`**:

```bash
#!/usr/bin/env bash
set -euo pipefail
root="$(mktemp -d)"
trap 'rm -rf "$root"' EXIT
export RUNTIME_ROOT="$root/runtime"
export RELEASE_DIR="$root/releases/rel-test"
mkdir -p "$RUNTIME_ROOT" "$RELEASE_DIR"
source deploy/runtime-layout.sh

expected=(
  "$RUNTIME_ROOT/data/deploy-journal.json"
  "$RUNTIME_ROOT/data/rollout-journal.json"
  "$RUNTIME_ROOT/state/current-release.json"
  "$RUNTIME_ROOT/state/gateway-active-slot"
  "$RUNTIME_ROOT/state/frontend-active-slot"
)
for path in "${expected[@]}"; do
  [[ "$path" != "$RELEASE_DIR"/* ]]
done
[[ "$TX_JOURNAL_FILE" == "$RUNTIME_ROOT/data/deploy-journal.json" ]]
[[ "$ROLLOUT_JOURNAL_FILE" == "$RUNTIME_ROOT/data/rollout-journal.json" ]]
```

- [ ] **Step 2: Run test and confirm it fails before implementation**

```bash
bash deploy/tests/test_runtime_layout.sh
```

Expected: FAIL because `runtime-layout.sh` does not exist yet / current defaults derive state from `SCRIPT_DIR`.

- [ ] **Step 3: Implement `runtime-layout.sh`**

Required contract:

```bash
RUNTIME_ROOT="${RUNTIME_ROOT:-${DEPLOY_PATH:-/opt/acb-transaction-webhook}}"
RUNTIME_DEPLOY_DIR="$RUNTIME_ROOT/deploy"
RUNTIME_STATE_DIR="$RUNTIME_ROOT/state"
RUNTIME_DATA_DIR="$RUNTIME_ROOT/data"
RUNTIME_RELEASES_DIR="$RUNTIME_ROOT/releases"

ENV_FILE="$RUNTIME_DEPLOY_DIR/.env.production"
RELEASE_ENV_FILE="$RUNTIME_DEPLOY_DIR/.release.env"
SECRETS_DIR="$RUNTIME_DEPLOY_DIR/secrets"
BACKUP_DIR="${BACKUP_DIR:-/var/backups/acb}"
CURRENT_RELEASE_FILE="$RUNTIME_STATE_DIR/current-release.json"
TX_JOURNAL_FILE="$RUNTIME_DATA_DIR/deploy-journal.json"
ROLLOUT_JOURNAL_FILE="$RUNTIME_DATA_DIR/rollout-journal.json"
ACTIVE_SLOT_FILE="$RUNTIME_STATE_DIR/gateway-active-slot"
PREVIOUS_SLOT_FILE="$RUNTIME_STATE_DIR/gateway-previous-slot"
FRONTEND_ACTIVE_SLOT_FILE="$RUNTIME_STATE_DIR/frontend-active-slot"
FRONTEND_PREVIOUS_SLOT_FILE="$RUNTIME_STATE_DIR/frontend-previous-slot"
DEPLOY_STATE_FILE="$RUNTIME_STATE_DIR/deploy-state.json"
SOAK_STATE_FILE="$RUNTIME_STATE_DIR/soak-state.env"
MIGRATION_RECORD="$RUNTIME_DATA_DIR/migration-record.json"
PENDING_GATEWAY_RETIRE_FILE="$RUNTIME_DATA_DIR/pending-gateway-retire.env"
PENDING_FRONTEND_RETIRE_FILE="$RUNTIME_DATA_DIR/pending-frontend-retire.env"

export RUNTIME_ROOT RUNTIME_DEPLOY_DIR RUNTIME_STATE_DIR RUNTIME_DATA_DIR RUNTIME_RELEASES_DIR
export ENV_FILE RELEASE_ENV_FILE SECRETS_DIR BACKUP_DIR CURRENT_RELEASE_FILE TX_JOURNAL_FILE ROLLOUT_JOURNAL_FILE
export ACTIVE_SLOT_FILE PREVIOUS_SLOT_FILE FRONTEND_ACTIVE_SLOT_FILE FRONTEND_PREVIOUS_SLOT_FILE
export DEPLOY_STATE_FILE SOAK_STATE_FILE MIGRATION_RECORD PENDING_GATEWAY_RETIRE_FILE PENDING_FRONTEND_RETIRE_FILE
```

- [ ] **Step 4: Source runtime layout before every deploy library**

`stable-deployer.sh` must export `RUNTIME_ROOT="$DEPLOY_PATH"`; `lib.sh` must source `runtime-layout.sh` before `common.sh/state.sh`; delete path defaults in `common.sh/state.sh` that derive mutable data from `SCRIPT_DIR`.

- [ ] **Step 5: Add one-time path migration**

Stable deployer must atomically migrate existing files:

```text
deploy/.active-slot              -> state/gateway-active-slot
deploy/.previous-slot            -> state/gateway-previous-slot
deploy/.active-frontend-slot     -> state/frontend-active-slot
deploy/.previous-frontend-slot   -> state/frontend-previous-slot
deploy/.deploy-state             -> state/deploy-state.json
deploy/.soak-state               -> state/soak-state.env
```

Rules:
- if new exists and old exists with different content: fail closed;
- if only old exists: copy to temp, fsync, rename, fsync parent, then remove old;
- never silently choose one of two conflicting states.

- [ ] **Step 6: Pass transaction journal path to failover controller explicitly**

Systemd unit:

```ini
Environment=ACB_TX_JOURNAL_FILE=/opt/acb-transaction-webhook/data/deploy-journal.json
```

Controller:

```python
def get_deployment_journal_path() -> Path:
    env_path = os.environ.get("ACB_TX_JOURNAL_FILE")
    if not env_path:
        raise RuntimeError("ACB_TX_JOURNAL_FILE is required")
    return Path(env_path)
```

Do **not** keep an install-path guess such as `/opt/acb/...` or `Path(__file__)...` in production mode.

- [ ] **Step 7: Run tests**

```bash
bash deploy/tests/test_runtime_layout.sh
bash deploy/tests/test_promotion_dispatcher.sh
python3 -m pytest -q platform/failover/test_failover.py
```

Expected: PASS; test explicitly proves `TX_JOURNAL_FILE` used by dispatcher and controller is byte-for-byte the same absolute path.

- [ ] **Step 8: Commit**

```bash
git add deploy platform/failover
git commit -m "fix(deploy): centralize shared runtime state layout"
```

---

# Task 3: Upgrade canonical release state to schema v2 with exact previous release bundle

**Files:**
- Create: `deploy/release-state.py`
- Modify: `deploy/dispatch-rollout.sh`
- Modify: `deploy/release-env.sh`
- Modify: `deploy/stable-deployer.sh`
- Modify: `deploy/verify-manifest.sh`
- Create: `deploy/tests/test_release_state.sh`

**Interfaces:**
- `release-state.py validate <state>`
- `release-state.py get <state> <json.path>`
- `release-state.py build --previous ... --release-dir ... --manifest ...`
- Canonical schema v2 includes exact release directories/config fingerprints.

- [ ] **Step 1: Write failing schema-v2 tests**

Required state shape:

```json
{
  "schema_version": 2,
  "generation": 42,
  "release_id": "rel-...",
  "release_dir": "/opt/acb-transaction-webhook/releases/rel-...",
  "git_sha": "40hex",
  "manifest_sha256": "64hex",
  "status": "COMPLETED",
  "previous": {
    "release_id": "rel-prev",
    "release_dir": "/opt/acb-transaction-webhook/releases/rel-prev",
    "generation": 41
  },
  "active_slots": {"gateway": "green", "frontend": "blue"},
  "images": {},
  "config": {
    "compose_bundle_sha256": "64hex",
    "traefik_template_sha256": "64hex",
    "platform_bundle_sha256": "64hex"
  },
  "failover_controller": {
    "sha256": "64hex",
    "bundle_sha256": "64hex"
  }
}
```

Tests must reject:
- release dir outside `$RUNTIME_RELEASES_DIR`;
- release dir symlink;
- state pointing to missing release manifest;
- manifest SHA mismatch;
- previous generation >= current;
- non-immutable image refs;
- missing active gateway/frontend slot after migration complete.

- [ ] **Step 2: Implement `release-state.py` validation and atomic writer input**

Keep JSON serialization deterministic: `sort_keys=True`, UTF-8, newline at EOF.

- [ ] **Step 3: Make dispatcher the only schema-v2 writer**

Before commit, dispatcher must build candidate state from:
- existing canonical state;
- current signed manifest;
- actual active slots after ACK/soak;
- exact candidate `RELEASE_DIR`;
- exact artifact/config hashes.


Additionally, fix the #247 stale-digest class by forbidding `.release.env` as candidate-state input. Build the candidate document directly:

```text
candidate = deep-copy(previous canonical state)
candidate.release_id/release_dir/git_sha/manifest = signed manifest
candidate.images[component] = manifest.images[component] for each promoted component
candidate.active_slots.gateway = deterministic candidate gateway slot
candidate.active_slots.frontend = deterministic candidate frontend topology/slot
candidate.failover_controller = signed candidate bundle hashes when promoted
```

Before any runtime mutation, write this as `state/candidate/<release_id>.json` and validate it. After route ACK but **before the 900s soak**, run a phase-aware drift check against this planned candidate state. Old standby containers are allowed in pre-commit mode, but the active route/container/image must already match. A deterministic state mismatch therefore fails in seconds instead of after 15 minutes.

- [ ] **Step 4: Remove production fallback to legacy state after one successful v2 bootstrap**

Introduce:

```bash
REQUIRE_CANONICAL_RELEASE_STATE=1
```

After canonical commit, regenerate `.release.env` **from the committed schema-v2 document**. Never merge candidate state through `.release.env`, and never use `PREVIOUS_*` keys as an authoritative rollback source.

Production behavior:

```text
current-release.json missing/corrupt
=> FAIL
=> never fallback automatically to last-release.json + .release.env
```

Legacy fallback remains available only with explicit one-time bootstrap flag:

```bash
ALLOW_CANONICAL_STATE_BOOTSTRAP=1
```

and is removed after production has committed schema v2 once.

- [ ] **Step 5: Run tests**

```bash
python3 deploy/release-state.py validate deploy/tests/fixtures/release-state-v2.json
bash deploy/tests/test_release_state.sh
bash deploy/tests/test_promotion_dispatcher.sh
```

- [ ] **Step 6: Commit**

```bash
git add deploy
 git commit -m "feat(deploy): make release bundle identity canonical"
```

---

# Task 4: Split Compose ownership so config changes do not restart worker unnecessarily

**Files:**
- Create: `deploy/compose/*.yaml` listed in file map
- Modify: `deploy/lib/common.sh`
- Modify: `deploy/component-map.json`
- Modify: `deploy/generate-release-manifest.sh`
- Modify: `deploy/verify-manifest.sh`
- Modify: `deploy/tests/test_runtime_policy.sh`
- Modify: `scripts/test-promotion-scope.sh`

**Interfaces:**
- `compose_prod()` always loads a deterministic ordered Compose file list.
- Each service fragment has explicit component ownership.

- [ ] **Step 1: Add tests proving service ownership**

Examples:

```text
deploy/compose/worker.yaml   -> worker only
deploy/compose/bark.yaml     -> bark only
deploy/compose/frontend.yaml -> frontend only
deploy/compose/gateway.yaml  -> gateway only
deploy/compose/base.yaml     -> all runtime components
```

Worker must remain `false` when only `frontend.yaml` changes.

- [ ] **Step 2: Move current monolithic services into fragments without semantic changes**

No resource/security setting may be changed in this task; first produce equivalent config.

- [ ] **Step 3: Make `compose_prod()` deterministic**

```bash
COMPOSE_FILES=(
  "$RELEASE_DIR/compose/base.yaml"
  "$RELEASE_DIR/compose/gateway.yaml"
  "$RELEASE_DIR/compose/frontend.yaml"
  "$RELEASE_DIR/compose/worker.yaml"
  "$RELEASE_DIR/compose/auth-browser.yaml"
  "$RELEASE_DIR/compose/tts.yaml"
  "$RELEASE_DIR/compose/bark.yaml"
  "$RELEASE_DIR/compose/dbtool.yaml"
)
for file in "${COMPOSE_FILES[@]}"; do
  compose_flags+=(-f "$file")
done
```

- [ ] **Step 4: Prove generated config is equivalent before deleting monolithic authority**

```bash
docker compose --env-file deploy/.env.production --env-file deploy/.release.env -f deploy/compose.prod.yaml config --format json > /tmp/old.json
docker compose --env-file deploy/.env.production --env-file deploy/.release.env \
  -f deploy/compose/base.yaml -f deploy/compose/gateway.yaml -f deploy/compose/frontend.yaml \
  -f deploy/compose/worker.yaml -f deploy/compose/auth-browser.yaml -f deploy/compose/tts.yaml \
  -f deploy/compose/bark.yaml -f deploy/compose/dbtool.yaml config --format json > /tmp/new.json
python3 -m json.tool --sort-keys /tmp/old.json > /tmp/old.sorted.json
python3 -m json.tool --sort-keys /tmp/new.json > /tmp/new.sorted.json
diff -u /tmp/old.sorted.json /tmp/new.sorted.json
```

Expected: no semantic diff.

- [ ] **Step 5: Sign/hash every Compose fragment in release manifest**

Manifest verifier must fail if an expected fragment is missing, symlinked, added unexpectedly, or hash-mismatched.

- [ ] **Step 6: Commit**

```bash
git add deploy scripts
 git commit -m "refactor(deploy): split compose ownership by runtime component"
```

---

# Task 5: Implement exact previous-release rollback, not previous-image rollback

**Files:**
- Create: `deploy/rollback-release.sh`
- Modify: `deploy/dispatch-rollout.sh`
- Modify: all component deploy scripts to accept `RELEASE_CONTEXT_DIR`
- Create: `deploy/tests/test_release_rollback.sh`

**Interfaces:**
- `rollback-release.sh --state <current-v2-state> --journal <rollout-journal>`
- Component scripts consume config from `RELEASE_CONTEXT_DIR`, mutable state from `RUNTIME_ROOT`.

- [ ] **Step 1: Write the critical regression test**

Create fixture release A and candidate B:

```text
Release A:
  worker image = A
  worker config env = GOOD_A
Release B:
  worker image = B
  worker config env = BAD_B
```

Force B to fail after worker promotion and assert rollback starts:

```text
image A + config GOOD_A
```

The test must fail if rollback produces:

```text
image A + config BAD_B
```

- [ ] **Step 2: Store previous release dir in rollout journal before any mutation**

Journal initialization must include:

```json
{
  "candidate_release_dir": "/.../releases/B",
  "previous_release_dir": "/.../releases/A",
  "previous_release_id": "A",
  "previous_generation": 41
}
```

- [ ] **Step 3: Implement rollback context validation**

Before rollback:

```bash
python3 deploy/release-state.py validate "$CURRENT_RELEASE_FILE"
realpath "$PREVIOUS_RELEASE_DIR" must be under "$RUNTIME_RELEASES_DIR"
verify-manifest.sh must validate previous release manifest/artifact hashes
```

- [ ] **Step 4: Roll back completed components in reverse dependency order**

Required order:

```text
platform/failover
-> gateway route/candidate
-> worker
-> frontend route/candidate
-> bark
-> tts
-> auth-browser
```

Schema is not automatically downgraded; Task 9 guarantees old runtime remains compatible with new schema.

- [ ] **Step 5: Use previous release compose/config/scripts for restoration**

Every rollback invocation must set:

```bash
RELEASE_CONTEXT_DIR="$PREVIOUS_RELEASE_DIR"
COMPOSE_ROOT="$PREVIOUS_RELEASE_DIR/compose"
```

and must never source candidate Compose when restoring previous runtime.

- [ ] **Step 6: Verify rollback result against canonical previous state**

After rollback:

```bash
bash "$CURRENT_ENGINE_DIR/verify-runtime-drift.sh" --state "$CURRENT_RELEASE_FILE"
```

Only after this passes may rollout journal become `ROLLED_BACK`.


- [ ] **Step 7: Recover every completed component even when one route rollback fails**

A failed restoration must not short-circuit unrelated restoration attempts. Record per-component results:

```json
{
  "rollback": {
    "gateway_route": "RESTORED",
    "frontend_route": "FAILED",
    "worker": "RESTORED",
    "bark": "RESTORED",
    "tts": "RESTORED",
    "auth_browser": "RESTORED",
    "failover_controller": "RESTORED"
  }
}
```

Final status is `ROLLED_BACK` only when every required item and the final previous-state drift check pass; otherwise `ROLLBACK_FAILED`/`INTERRUPTED` is retained with exact failed items.

- [ ] **Step 8: Add previous-release config regression for #247/#248**

The test must prove a failed candidate cannot leave candidate worker/sidecars/controller active just because frontend rollback failed. Each completed component is restored using the exact previous release bundle even when another rollback step returns non-zero.

- [ ] **Step 9: Commit**

```bash
git add deploy
 git commit -m "fix(deploy): rollback exact previous release bundle"
```

---

# Task 6: Make slot resolution and route state fully fail-closed

**Files:**
- Modify: `deploy/lib/state.sh`
- Modify: `deploy/lib/traefik.sh`
- Modify: `deploy/deploy-frontend.sh`
- Modify: `deploy/deploy-gateway.sh`
- Create: `deploy/tests/test_slot_resolution.sh`

**Interfaces:**
- `resolve_gateway_slot_strict()` returns `blue|green` or non-zero.
- `resolve_frontend_slot_strict()` returns `blue|green|legacy` only during migration bootstrap.

- [ ] **Step 1: Add table-driven tests**

Gateway cases:

```text
state=blue, route=blue                 -> blue
state=green, route=green               -> green
state missing, route=blue              -> blue
state missing, route missing, only blue running  -> blue
state missing, route missing, only green running -> green
both running + no route/state          -> FAIL
neither running + no route/state       -> FAIL
state=blue + route=green               -> FAIL
```

- [ ] **Step 2: Remove default-to-blue behavior**

Never use:

```bash
else slot=blue
```

for ambiguous runtime.

- [ ] **Step 3: Finish legacy frontend migration semantics**

`legacy` is accepted only while canonical state has `frontend: null` and container `acb-frontend` exists. First successful B/G promotion must atomically set frontend slot and canonical state. Any later `legacy` observation is drift/failure.

- [ ] **Step 4: Add route ACK after rollback as well as cutover**

Rollback is not successful until edge probe proves expected slot identity.

- [ ] **Step 5: Commit**

```bash
git add deploy
 git commit -m "fix(deploy): fail closed on ambiguous route and slot state"
```

---

# Task 7: Make failover-controller deployment an exact bundle transaction

**Files:**
- Modify: `deploy/deploy-failover-controller.sh`
- Modify: `platform/failover/vps-failover-controller.py`
- Modify: `platform/failover/apps.d/*.json`
- Modify: `deploy/verify-runtime-drift.sh`
- Create: `deploy/tests/test_failover_bundle.sh`

**Interfaces:**
- Installed registry must exactly equal signed candidate registry; no stale files.
- Drift includes controller bytes + unit bytes + registry exact set + service active/enabled status.

- [ ] **Step 1: Add stale-registry regression test**

Fixture installed registry:

```text
acb.json
auth-browser.json
worker.json
obsolete.json
```

Candidate:

```text
acb.json
auth-browser.json
worker.json
```

After install, `obsolete.json` must not exist.

- [ ] **Step 2: Replace per-file overwrite with atomic directory swap**

Algorithm:

```text
validate candidate dir
copy to registry.next
fsync contents
audit exact file set
rename current -> backup
rename next -> current
daemon-reload/restart/verify
rollback directory + units if health fails
```

Do not use unbounded `rm -rf` on computed paths; validate allowed absolute roots first.

- [ ] **Step 3: Drift-check exact registry set**

Any extra or missing `*.json` is `PRODUCTION_DRIFT`.

- [ ] **Step 4: Drift-check systemd state**

Required:

```bash
systemctl is-active --quiet vps-failover-controller.service
systemctl is-enabled --quiet vps-failover-controller.service
systemctl is-active --quiet vps-failover-reconcile.timer
systemctl is-enabled --quiet vps-failover-reconcile.timer
```

- [ ] **Step 5: Commit**

```bash
git add deploy platform/failover
 git commit -m "fix(failover): install and verify exact signed controller bundle"
```

---

# Task 8: Complete runtime drift detection for every authoritative surface

**Files:**
- Modify: `deploy/verify-runtime-drift.sh`
- Create: `deploy/tests/test_drift_complete.sh`

**Interfaces:**
- One verifier must compare state -> containers -> slots -> Traefik routes -> failover bundle -> systemd -> shared journal path.

- [ ] **Step 1: Add frontend drift tests**

Must fail when canonical frontend slot is blue but:
- `.frontend-active-slot` says green;
- Traefik points green;
- blue image correct but route wrong.

- [ ] **Step 2: Add gateway route strictness**

Fail if both blue and green strings are simultaneously discoverable in the active service definition or route cannot be uniquely determined.

- [ ] **Step 3: Add failover service checks from Task 7**

- [ ] **Step 4: Add journal-path identity check**

Read controller effective environment and assert:

```text
ACB_TX_JOURNAL_FILE == TX_JOURNAL_FILE == $RUNTIME_DATA_DIR/deploy-journal.json
```

- [ ] **Step 5: Add config fingerprints**

For every active service, compare Docker Compose `com.docker.compose.config-hash` with expected hash stored in canonical release state where available.

- [ ] **Step 6: Run**

```bash
bash deploy/tests/test_drift_complete.sh
bash deploy/verify-runtime-drift.sh --state /path/to/fixture-state.json
```

- [ ] **Step 7: Commit**

```bash
git add deploy
 git commit -m "fix(deploy): verify complete production runtime drift"
```

---

# Task 9: Make component ownership truly fail-closed

**Files:**
- Modify: `deploy/component-map.json`
- Modify: `scripts/compute-promotion-scope.sh`
- Create: `deploy/tests/test_component_map_coverage.sh`

**Interfaces:**
- Unknown runtime path => CI failure.
- No broad regex may silently classify a new runtime file as only `platform`.

- [ ] **Step 1: Remove broad catch-all ownership**

Delete rules equivalent to:

```regex
^deploy/
^platform/
^\.github/workflows/
^scripts/
```

unless they map to an explicit `orchestrator` pseudo-component that deterministically expands to all affected components.

- [ ] **Step 2: Add `orchestrator` component**

Examples that legitimately affect deployment engine globally:

```text
deploy/dispatch-rollout.sh
deploy/runtime-layout.sh
deploy/release-state.py
deploy/lib/state.sh
deploy/lib/common.sh
.github/workflows/deploy.yml
```

`orchestrator` expands to:

```text
frontend,gateway,worker,schema,auth_browser,tts,bark,failover_controller,platform
```

- [ ] **Step 3: Coverage-test every tracked runtime path**

Test command:

```bash
git ls-files | while read -r path; do
  scripts/compute-promotion-scope.sh --files-from <(printf 'M\t%s\n' "$path") --format list >/dev/null
done
```

Exclude docs/assets explicitly. Any unowned executable/runtime/config path fails CI.

- [ ] **Step 4: Commit**

```bash
git add deploy/component-map.json scripts deploy/tests
 git commit -m "fix(ci): make runtime ownership classification exhaustive"
```

---

# Task 10: Enforce forward-only migration compatibility with rollback-safe old binaries

**Files:**
- Modify: `deploy/deploy-schema.sh`
- Modify: `deploy/lib/database.sh`
- Create: `deploy/tests/test_schema_compatibility.sh`
- Modify: `.github/workflows/ci.yml`

**Interfaces:**
- Migration may remain applied when later release step fails.
- Therefore previous worker/gateway images must be proven compatible with migrated DB before migration is allowed in production.

- [ ] **Step 1: Add four compatibility gates**

CI fixture must prove:

```text
previous gateway + new schema -> ready/read-only checks pass
previous worker  + new schema -> startup/readiness/quiesce protocol pass
candidate gateway + new schema -> pass
candidate worker  + new schema -> pass
```

- [ ] **Step 2: Reject destructive migration patterns in same release**

Schema change that removes/renames a column/table required by previous runtime cannot ship in the same release. Use expand/contract:

```text
Release N: add new structure, dual-read/write if needed
Release N+1: move readers/writers
Release N+2: remove old structure only after previous runtime window is gone
```

- [ ] **Step 3: Persist migration backup path in rollout journal**

If operator intervention is required, journal identifies exact encrypted backup and schema generation.

- [ ] **Step 4: Run restore drill in CI for migration releases**

```bash
bash deploy/tests/test_schema_compatibility.sh
bash deploy/tests/test_restore_drill.sh
```

- [ ] **Step 5: Commit**

```bash
git add deploy .github/workflows/ci.yml
 git commit -m "test(schema): prove rollback-safe rolling compatibility"
```

---

# Task 11: Build a release simulator and chaos matrix before production

**Files:**
- Create: `deploy/tests/test_release_simulator.sh`
- Create: `deploy/tests/test_release_crash_recovery.sh`
- Modify: `.github/workflows/ci.yml`
- Modify: `.github/workflows/deploy.yml`

**Interfaces:**
- PR/branch CI can execute the release state machine without touching production VPS.
- Production job depends on one aggregate `Production Contract` check.

- [ ] **Step 1: Create release A/B simulator**

Simulator lifecycle:

```text
bootstrap release A
assert canonical A
stage release B
run B
assert canonical B
rollback B failure scenarios
assert runtime/canonical A exactly
```

Use throwaway Docker project/network/volumes and fixture secrets; do not call ACB production endpoints.

- [ ] **Step 2: Inject failure after every orchestration boundary**

Required matrix:

```text
after schema
after auth-browser
after tts
after bark
after frontend route ACK
after worker quiesce but before stop
after worker stop but before candidate ready
after gateway candidate ready before switch
after gateway route switch before ACK
during gateway soak
after failover-controller install before release commit
during canonical state commit
SIGTERM/SSH disconnect equivalent after each completed step
```

For every row assert:

```text
canonical state is previous release OR fully committed candidate
never a mixed undocumented state
worker resumes/runs exactly once
route points to a healthy slot
runtime drift verifier passes
```

- [ ] **Step 3: Add failover race chaos**

Simulate stale Docker event arriving while deploy lock is held and immediately after lock release. Controller must not restart/flip a healthy current container.

- [ ] **Step 4: Add outage durability chaos**

Webhook/Bark unavailable for 2 hours (simulated time/backoff): delivery rows remain pending and become deliverable later; no transaction loss.


- [ ] **Step 5: Reproduce both production incidents exactly in simulator**

Required named cases:

```text
INCIDENT_247_STALE_CANDIDATE_STATE
  active gateway blue actually candidate digest
  candidate state accidentally contains previous blue digest
  expected: pre-soak candidate-state verification catches it BEFORE 900s soak

INCIDENT_247_LEGACY_FRONTEND_ROLLBACK
  previous frontend = legacy
  candidate frontend = blue
  later release step fails
  expected: route returns to legacy and candidate blue retires cleanly

INCIDENT_248_DIRTY_START
  previous rollout journal INTERRUPTED + pending frontend recovery evidence
  expected: recovery-only phase executes before builds/new mutation and converges previous release

INCIDENT_STABLE_DEPLOYER_BOOTSTRAP_DEADLOCK
  installed stable deployer = N
  candidate contains stable deployer N+1 path fix
  candidate app rollout fails
  expected: future recovery does not depend on N+1 having been promoted; signed recovery/bootstrap path remains operable
```

- [ ] **Step 6: Add a post-ACK pre-soak contract gate**

Immediately after frontend/gateway route ACK and before 900s soak, verify planned candidate state for all already-promoted active components. The 900s soak is entered only when deterministic image/slot/route/config expectations already match runtime.

- [ ] **Step 7: Add one aggregate CI job**

`Production Contract` depends on:

```text
unit/race/vet
frontend tests
TTS tests
failover tests
component ownership coverage
runtime layout tests
release state tests
rollback tests
schema compatibility
restore drill
Docker image smoke tests
release simulator
chaos matrix
supply-chain verification
```

- [ ] **Step 8: Prevent deploy job from running unless aggregate contract is success**

Deploy workflow should consume the same contract or re-run required gates; do not maintain a weaker parallel set.

- [ ] **Step 9: Commit**

```bash
git add deploy/tests .github/workflows
git commit -m "test(deploy): gate production on release chaos contract"
```

---

# Task 12: Lock GitHub governance before merging the final branch

**Files:**
- GitHub repository settings/ruleset (not source only)
- Optional: `.github/CODEOWNERS`
- Modify: `docs/runbooks/FINAL_RELEASE_CONTRACT.md`

**Interfaces:**
- `main` cannot accept direct unverified production changes.

- [ ] **Step 1: Create/enable ruleset for `main`**

Required settings:

```text
Require pull request before merge: ON
Required approvals: 0 or 1 (solo maintainer choice)
Require status checks: ON
Required check: Production Contract
Require branch up to date: ON
Block force pushes: ON
Block deletions: ON
Require linear history: ON
Bypass: disabled for normal pushes
```

- [ ] **Step 2: Keep production environment secrets only in production jobs**

PR CI gets no VPS SSH key, no production app secrets, no production signing capability beyond what is needed to test local verification.

- [ ] **Step 3: Require manual production environment approval if desired**

Recommended for a single critical payment/transaction VPS.

- [ ] **Step 4: Document emergency procedure**

Emergency bypass must require explicit GitHub admin action and a follow-up PR; do not embed permanent bypass environment variables in workflow YAML.

---

# Task 13: Final production cutover and acceptance — one run, one checklist

**Files:**
- Modify: `docs/runbooks/FINAL_RELEASE_CONTRACT.md`
- No ad-hoc code changes during this step.

**Interfaces:**
- This is the only gate that marks zero-downtime plan complete.

- [ ] **Step 1: Merge only after all branch CI gates pass**

No “merge then fix workflow on prod”.

- [ ] **Step 2: Let production workflow serialize naturally**

Keep:

```yaml
concurrency:
  group: acb-transaction-webhook-production
  cancel-in-progress: false
```

Do not cancel a release while it owns worker/gateway transaction state unless testing recovery intentionally.

- [ ] **Step 3: Observe full production rollout**

Must pass:

```text
production-state read
dirty rollout/pending-evidence check
recovery-only reconcile if dirty; verify previous state before any candidate build
scope classification
build affected images only
scan/SBOM/sign
manifest verify
candidate stage
build deterministic candidate-release state directly from previous state + signed manifest
schema compatibility/preflight if needed
auth-browser/TTS/Bark transaction if scoped
frontend B/G if scoped
worker v2 quiesce/drain/checkpoint if scoped
gateway B/G + route ACK if scoped
pre-soak candidate-state/runtime contract audit
gateway 900s soak if scoped
failover controller exact bundle if scoped
final candidate runtime drift audit
canonical release state v2 atomic commit
regenerate .release.env projection from committed canonical state
old slot retirement
post-commit drift audit
```

- [ ] **Step 4: Verify worker latency invariant**

Collect timestamps:

```text
T0 poll start
T1 ACB response
T2 DB commit
T3 realtime publish
T4 notification dispatch
```

Acceptance:
- unrelated deploy does not change worker container ID;
- explicit worker deploy shows only one bounded stop/start gap;
- after T1, T1->T2, T2->T3 and T2->T4 remain near-instant relative to ACB polling latency.

- [ ] **Step 5: Verify exact runtime state**

Run on VPS:

```bash
bash /opt/acb-transaction-webhook/deploy/verify-runtime-drift.sh \
  --state /opt/acb-transaction-webhook/state/current-release.json
```

Expected: `Runtime matches canonical release state.`

- [ ] **Step 6: Verify failover services**

```bash
systemctl is-active vps-failover-controller.service
systemctl is-active vps-failover-reconcile.timer
systemctl is-enabled vps-failover-controller.service
systemctl is-enabled vps-failover-reconcile.timer
```

Expected: active/active/enabled/enabled.

- [ ] **Step 7: Verify no mutable state remains inside release directories**

```bash
find /opt/acb-transaction-webhook/releases -type f \
  \( -name 'deploy-journal.json' -o -name 'rollout-journal.json' -o -name '.active-slot' -o -name '.active-frontend-slot' -o -name '.soak-state' -o -name '.deploy-state' \) -print
```

Expected: no output.

- [ ] **Step 8: Remove bootstrap/legacy fallbacks**

After canonical schema v2 is confirmed in production:
- remove `ALLOW_CANONICAL_STATE_BOOTSTRAP` use;
- production-state job fails if `current-release.json` missing;
- remove legacy slot files after migration;
- keep `.release.env` only as regenerated compatibility projection until all tooling stops consuming it.

- [ ] **Step 9: Final commit for documentation/cleanup only**

```bash
git add docs deploy .github
 git commit -m "docs(deploy): close final zero-downtime production contract"
```

---

# Final Acceptance Matrix — do not call the plan complete until every row is green

| Invariant | Required result |
|---|---|
| Runtime layout | Every mutable state path resolves under shared runtime root, never candidate release dir |
| Dirty-start recovery | INTERRUPTED/pending evidence is reconciled before builds/new mutation; new candidate cannot layer on mixed runtime |
| Deployment-engine bootstrap | A failed app rollout cannot prevent a signed recovery/runtime-layout fix from being executed safely |
| Candidate state | Built directly from previous canonical state + signed manifest; `.release.env` is output-only projection |
| Pre-soak gate | deterministic image/slot/route/config mismatch fails before the 900s soak |
| Legacy frontend recovery | `legacy -> blue/green` rollback is explicitly supported and verified |
| Worker isolation | Frontend/gateway/Bark/TTS/platform deploy does not change `acb-worker` ID |
| Worker handoff | protocol v2 mandatory; quiesce/drain/session/journal checkpoint verified before stop |
| Worker rollback | previous image **and previous config bundle** restored |
| Gateway | candidate ready twice, route ACK, 900s soak, old slot retained until release commit |
| Frontend | B/G ACK + soak, previous slot retained until release commit |
| Bark | unreadable secret fails before old Bark is touched; rollback health is verified |
| Schema | previous and candidate gateway/worker both work with migrated schema |
| Canonical state | schema v2, one atomic writer, exact release dir and previous release dir |
| Crash recovery | SIGTERM/SSH loss at every boundary converges to previous or committed candidate, never mixed |
| Failover controller | exact signed bundle; no stale registry files; service/timer active + enabled |
| Failover/deploy race | shared lock and shared journal path; stale Docker events cannot fight rollout |
| Drift | images, config hashes, gateway route, frontend route, slots, controller, registry, systemd all checked |
| Scope classifier | any unknown runtime file fails CI; no broad silent platform catch-all |
| Supply chain | immutable digest + SBOM + scan + Cosign + signed artifact hashes |
| Backups | migration backup encrypted; restore drill passes |
| Notifications | transient outage retained/retried; no silent delivery loss |
| Realtime | journal gap causes reset/resync, never silent event skip |
| GitHub governance | main protected; Production Contract required; direct bypass disabled |
| Production workflow | serialized; failed candidate does not mutate canonical release state |
| Single-VPS limitation | documented: host/provider failure is outside zero-downtime guarantee |

---

# Implementation Order — must be followed exactly

0. Task 0 — reproduce #247/#248, install the signed recovery/bootstrap path, and prove dirty production can converge **without a new application rollout**.
1. Task 1 — freeze branch.
2. Task 2 — runtime layout first, because every later rollback/recovery depends on correct shared paths.
3. Task 3 — canonical state v2 / deterministic candidate state; `.release.env` becomes output-only.
4. Task 4 — Compose ownership split.
5. Task 5 — exact previous-release rollback + non-short-circuit recovery.
6. Task 6 — strict slot/route resolution, including explicit legacy frontend topology.
7. Task 7 — exact failover bundle transaction.
8. Task 8 — complete drift verifier.
9. Task 9 — exhaustive component ownership.
10. Task 10 — schema backward compatibility.
11. Task 11 — simulator + chaos contract, including exact #247/#248 reproductions and pre-soak contract gate.
12. Task 12 — GitHub ruleset.
13. Task 13 — one final production acceptance run and remove bootstrap fallbacks.

Do not deploy intermediate tasks to production merely to “see what breaks”. Intermediate commits are tested only on the hardening branch/CI. Production receives the merged convergence set after the full contract is green.

---

# Self-review against `zero_downtime_lastfix.md`

Covered explicitly:
- stable signed releases;
- canonical current release state;
- exact component ownership;
- first-class failover-controller deployment;
- mandatory worker protocol v2;
- quiesce/drain/checkpoints;
- Bark preflight/rollback/no-op;
- frontend/gateway B/G;
- release crash recovery;
- runtime drift;
- schema migration compatibility;
- restore drill;
- chaos matrix;
- branch protection;
- production contract;
- worker latency invariant;
- single-VPS limitation.

The main extra correction added by this plan is **runtime-layout canonicalization** and **exact previous-release bundle rollback**, because the latest `b2433caf` path fix shows that fixing state paths one-by-one is not sufficient.

The #247/#248 incident addendum further makes **dirty-start recovery, legacy frontend rollback, deterministic candidate-state construction, pre-soak drift verification, non-short-circuit whole-release rollback, and deployment-engine bootstrap independence** mandatory acceptance criteria rather than follow-up fixes.
