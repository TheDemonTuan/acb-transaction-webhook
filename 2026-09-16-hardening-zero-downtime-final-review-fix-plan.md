# ACB Zero-Downtime Hardening Branch — Final Review & Convergence Fix Plan

**Branch:** `hardening/zero-downtime-final`  
**Reviewed HEAD:** `5993132fd234c625b6ca0b81fbabca2522d84925`  
**Base:** `main@b2433caf57476697595da528c262eada90e4e714`  
**Latest CI reviewed:** run #65 / `35086680375`  
**Goal:** close the remaining correctness gaps in one pass, make tests model the real production contracts, then run one final CI + production acceptance sequence.

---

## 0. Final verdict before fixes

The branch already implements most of the intended architecture:

- canonical runtime layout;
- canonical release state schema v2;
- signed deployment-engine bootstrap/recovery path;
- split Compose bundles by component;
- fail-closed gateway slot resolution;
- frontend legacy recovery support;
- exact previous-image + previous-compose rollback test;
- release dirty-start recovery;
- failover-controller bundle deployment with stale registry deletion;
- frontend/gateway route drift checks;
- systemd drift checks;
- schema backward-compatibility tests;
- component-map coverage;
- incident #247/#248 simulators;
- crash-recovery/chaos matrix;
- backup/restore drills in CI.

However, **do not merge this branch or deploy it to production yet**. Current test coverage contains contract mismatches that let important recovery defects pass.

### Current CI blocker

The latest CI run fails at ShellCheck:

```text
SC2168: 'local' is only valid in functions
file: deploy/dispatch-rollout.sh
line: ~893
code: local r_failures=0
```

This is easy to fix, but fixing only this line is not enough.

---

# P0 — Must fix before merge

## Task 1 — Unblock CI and make static validation fail fast

### Root cause

`dispatch-rollout.sh` declares `local r_failures=0` at top-level script scope during interrupted-rollout startup recovery.

### Files

- Modify: `deploy/dispatch-rollout.sh`
- Modify: `.github/workflows/ci.yml`
- Add/modify: `deploy/tests/test_ci_workflow.sh`

### Fix

Replace:

```bash
local r_failures=0
```

with:

```bash
r_failures=0
```

Do not suppress `SC2168`.

Move these checks near the beginning of `verify`, before frontend/Go/Python/full failure drills:

1. `bash -n` on every tracked shell script.
2. ShellCheck on every production shell script.
3. architecture/schema static contract tests.

Do not maintain hand-written glob lists that can drift. Generate the shell-script set from tracked files, for example by combining:

- `git ls-files '*.sh'`
- executable/text scripts with a bash/sh shebang if needed.

At minimum include:

- `deploy/*.sh`
- `deploy/lib/*.sh`
- `deploy/tests/*.sh`
- `scripts/*.sh`
- `scripts/ops/*.sh`
- `platform/**/*.sh`

### Regression tests

Add assertions that:

- no top-level `local` exists in production scripts;
- CI ShellCheck covers `deploy/tests` and `platform` scripts too;
- lint/static checks execute before expensive build/test phases.

### Acceptance

```bash
bash -n deploy/dispatch-rollout.sh
shellcheck --severity=error deploy/dispatch-rollout.sh
```

must pass.

---

## Task 2 — Create one canonical release/manifest naming schema

### Root cause A — TTS key mismatch

The signed release manifest writes:

```json
"images": {
  "tts_gateway": "..."
}
```

while `release-state.py` builds canonical state using:

```python
manifest_images.get("tts")
```

This can produce:

```text
runtime TTS = candidate digest
candidate canonical state TTS = previous digest
```

and trigger false production drift after a successful TTS deployment.

### Root cause B — docs-only path still assumes release-state schema v1

`dispatch-rollout.sh` docs-only code checks:

```python
state.get("schema_version") != 1
```

and mutates `previous_release_id`, while the hardening branch declares schema v2 with a `previous` object.

### Root cause C — frontend topology uses two meanings

Some paths encode legacy frontend as:

```text
legacy
```

while final release commit can encode it as `null`/empty because the active slot file is absent.

That weakens drift verification because frontend checks are skipped for an empty expected topology.

### Decision

Use these canonical names everywhere:

```text
frontend
gateway
worker
dbtool
auth_browser
tts
bark
failover_controller
platform
```

The manifest writer should emit `images.tts`, not `images.tts_gateway`.

For a short compatibility window, readers may accept:

```text
tts_gateway -> tts
```

but writers must emit only `tts`.

Frontend topology must always be explicit:

```text
blue | green | legacy
```

Never use empty/null to mean legacy.

### Files

- Modify: `deploy/generate-release-manifest.sh`
- Modify: `deploy/verify-manifest.sh`
- Modify: `deploy/release-state.py`
- Modify: `deploy/dispatch-rollout.sh`
- Modify: workflow consumers of `IMAGE_TTS_GATEWAY`
- Modify tests/fixtures that manually construct manifests.

### Refactor docs-only release state

Delete hand-written state mutation from `dispatch-rollout.sh`.

Add an operation to `release-state.py`, e.g.:

```bash
release-state.py advance-doc-only \
  --previous current-release.json \
  --release-dir <signed-release-dir> \
  --manifest release-manifest.json \
  --output candidate-doc-state.json
```

This operation must:

- increment generation;
- preserve active slots/images/config/failover state;
- record previous release object correctly;
- update `release_id`, `git_sha`, manifest hash, release_dir;
- validate schema v2 before atomic commit.

### Regression tests

Add tests generated with **the real manifest generator**, not hand-written JSON:

1. TTS-only release changes `state.images.tts` to candidate digest.
2. TTS-only runtime drift passes with the candidate digest.
3. Old `tts_gateway` manifest can be read only in migration compatibility mode.
4. New manifests never contain `tts_gateway`.
5. docs-only update from schema v2 succeeds.
6. docs-only update preserves all runtime image/slot fields.
7. frontend legacy remains explicit `active_slots.frontend = "legacy"`.

---

## Task 3 — Unify rollout journal schema and recovery readers

### Root cause

The real dispatcher writes:

```json
{
  "completed_steps": ["schema", "auth_browser", "worker"]
}
```

But both `reconcile-release.sh` and `rollback-release.sh` test:

```python
data.get("steps", {}).get(component) == "STEP_COMPLETED"
```

The rollback unit test currently creates the **wrong synthetic schema** (`steps: {...}`), so it passes even though it does not model production.

### Risk

After a dirty/interrupted real rollout, recovery can restore the routes but silently skip already-promoted:

- worker;
- Bark;
- TTS;
- auth-browser;
- failover controller.

The runtime then remains mixed while recovery logic believes component rollback was not required.

### Design

Create a single journal library/API.

Suggested file:

```text
deploy/lib/rollout-journal.sh
```

Canonical journal schema v2:

```json
{
  "schema_version": 2,
  "rollout_id": "...",
  "git_sha": "...",
  "candidate_release_dir": "...",
  "previous_release_dir": "...",
  "status": "RUNNING",
  "current_step": "worker",
  "steps": {
    "schema": "STEP_COMPLETED",
    "auth_browser": "STEP_COMPLETED",
    "tts": "NOT_STARTED",
    "bark": "NOT_STARTED",
    "frontend": "NOT_STARTED",
    "worker": "STEP_COMPLETED",
    "gateway": "NOT_STARTED",
    "failover_controller": "NOT_STARTED",
    "platform": "NOT_STARTED",
    "release_commit": "NOT_STARTED"
  }
}
```

Do not maintain both `completed_steps` and `steps` as independent authorities.

If migration support is required, implement exactly one reader that can normalize legacy `completed_steps` into v2 in memory. New writes are v2 only.

### Files

- Add: `deploy/lib/rollout-journal.sh`
- Modify: `deploy/dispatch-rollout.sh`
- Modify: `deploy/reconcile-release.sh`
- Modify: `deploy/rollback-release.sh`
- Modify all rollout simulator/chaos tests.

### Tests

Do not hand-write a rollback journal separately from production format.

Generate the journal through:

```bash
init_rollout_journal
update_rollout_step schema STEP_COMPLETED
update_rollout_step worker STEP_COMPLETED
```

Then feed the exact resulting file to:

- `reconcile-release.sh`
- `rollback-release.sh`

Assertions:

- completed worker is actually rolled back;
- uncompleted worker is not touched;
- every completed auxiliary component is restored;
- failures are accumulated, not short-circuited;
- journal is archived only after final drift verification succeeds.

---

## Task 4 — Make every route/stop safety prerequisite explicitly fail-closed

### Root cause A — Bash `set -e` is not a safety contract

`atomic_switch_route()` and `atomic_switch_frontend_route()` currently do:

```bash
verify_traefik_prerequisites
# then continue
```

When a function is evaluated in conditional command lists such as:

```bash
if atomic_switch_route ... && ack_route_identity ...; then
```

Bash `errexit` behavior cannot be relied upon to abort at the nested failed command.

The chaos logs already demonstrated this: a Traefik dynamic-dir mismatch logged “Aborting before route switch”, followed by a successful route write.

### Root cause B — standby can be stopped without an intentional-stop marker

`stop_standby_container()` currently does:

```bash
mark_intentional_stop "$slot"
compose_prod stop ...
```

without checking marker success explicitly.

If `/var/lib/vps-failover/apps/acb` is unwritable/unavailable, the marker can fail but the gateway is still stopped. The failover controller may then interpret the stop as an unexpected failure and restart it.

### Required style

Never rely on `set -e` for safety-critical nested calls.

Use explicit guards:

```bash
if ! verify_traefik_prerequisites; then
  log_error "Route prerequisites failed; no route mutation performed."
  return 1
fi
```

and:

```bash
if ! mark_intentional_stop "$slot"; then
  log_error "Refusing to stop $slot without durable intentional-stop marker."
  return 1
fi
```

Also make rollback route logic explicit:

```text
old route switch
  -> route ACK
  -> only after ACK may candidate be stopped
```

If old route cannot be ACKed, preserve both healthy slots and evidence rather than making a second destructive mutation.

### Files

- Modify: `deploy/lib/traefik.sh`
- Modify: `deploy/lib/state.sh`
- Modify: `deploy/deploy-gateway.sh`
- Modify: frontend/reconcile rollback paths as necessary.

### Regression tests

Add tests where:

1. Traefik mount path is wrong.
   - route file bytes must remain unchanged;
   - active slot file must remain unchanged.

2. failover state dir is unwritable.
   - `stop_standby_container` must return nonzero;
   - Docker stop must not be invoked.

3. rollback route ACK fails.
   - candidate must remain running;
   - pending evidence must remain;
   - recovery returns failure.

4. run safety functions from inside `if`, `&&`, `!` contexts to reproduce Bash `errexit` edge behavior.

---

## Task 5 — Put the runtime contract check BEFORE the 900-second soak

### Current problem

The branch builds a planned candidate state before mutation, but the real runtime drift check against the final candidate state still occurs after `deploy-gateway.sh` has performed its long soak.

Therefore a mismatch such as the TTS naming bug can still waste the full 900 seconds before failing.

The incident simulator says stale candidate state is detected pre-soak, but the actual dispatcher sequencing must enforce the same contract.

### Target flow

```text
verify signed release
        ↓
build planned candidate state
        ↓
schema/aux/frontend/worker candidate promotions
        ↓
gateway candidate + route ACK
        ↓
failover-controller candidate install
        ↓
BUILD ACTUAL CANDIDATE STATE
        ↓
PRE-SOAK RUNTIME CONTRACT CHECK
        ↓
if mismatch -> rollback immediately (< seconds)
        ↓
900s soak
        ↓
FINAL RUNTIME CONTRACT CHECK
        ↓
atomic canonical state commit
        ↓
retire old slots
```

### Implementation options

Preferred: split gateway cutover from release-level soak.

Add to `deploy-gateway.sh` either:

```text
--defer-soak
```

or a dedicated transaction mode returning after route ACK while retaining the old slot.

The dispatcher owns the one authoritative release soak.

Do not use `--soak-seconds 0` unless the gateway script explicitly defines zero as “defer to parent”; avoid ambiguous semantics.

### Tests

Add an integration test with a fake long soak command:

- mutate TTS runtime to digest B;
- candidate state incorrectly expects A;
- assert pre-soak check fails;
- assert soak command was **never invoked**.

Also test:

- pre-soak passes -> soak runs;
- final check fails after soak -> rollback;
- final check passes -> canonical commit.

---

# P1 — Must finish in the same convergence PR

## Task 6 — Fix post-commit frontend cleanup for JSON evidence

### Root cause

`deploy-frontend.sh` now writes `PENDING_FRONTEND_RETIRE_FILE` as JSON containing:

```json
{
  "previous_topology": "legacy|blue|green",
  "previous_container": "...",
  "candidate_container": "..."
}
```

But post-commit cleanup still does:

```bash
old_slot="$(sed -n 's/^old_slot=//p' "$frontend_cleanup")"
docker stop "acb-frontend-${old_slot}"
```

That parser only understands the old key-value format.

### Fix

Create one evidence parser/helper shared by:

- normal cleanup;
- rollback;
- recovery-only reconciliation.

For frontend, stop `previous_container`, not a reconstructed name.

This is mandatory for legacy topology because the old container is:

```text
acb-frontend
```

not `acb-frontend-legacy`.

### Cleanup contract

Post-commit cleanup is non-authoritative and idempotent:

- release stays committed if cleanup fails;
- cleanup evidence remains;
- next deploy/preflight attempts cleanup again safely;
- cleanup never alters the active route.

### Tests

- legacy -> blue cleanup stops `acb-frontend`;
- blue -> green cleanup stops `acb-frontend-blue`;
- malformed JSON fails closed and preserves cleanup file;
- rerun cleanup is idempotent.

---

## Task 7 — Make drift verification genuinely complete

### 7.1 Fix journal-path check

Current code derives `expected_journal` from `TX_JOURNAL_FILE` itself when it is set, then compares `TX_JOURNAL_FILE` to that value. This cannot detect an overridden wrong path.

Expected path must come only from canonical runtime layout, e.g.:

```text
$RUNTIME_DATA_DIR/deploy-journal.json
```

Then compare `realpath`/normalized absolute path of the actual configured path.

### 7.2 Verify split-Compose bundle, not legacy compose only

`release-state.py` currently looks for artifact hash `compose.prod.yaml` for `compose_bundle_sha256`, while runtime now uses split files:

```text
compose/base.yaml
compose/gateway.yaml
compose/frontend.yaml
compose/worker.yaml
compose/auth-browser.yaml
compose/tts.yaml
compose/bark.yaml
compose/dbtool.yaml
```

Create a deterministic bundle hash:

```text
for each relative path sorted:
  hash.update(path + NUL + bytes + NUL)
```

Manifest generator records this bundle hash explicitly.

Canonical state stores it.

Drift verifier validates the signed current release directory bundle against this hash.

### 7.3 Verify platform/Traefik template hashes too

Consume the schema-v2 `config` object rather than merely validating its syntax.

At minimum verify:

- compose bundle hash;
- `lib/traefik.sh`/route template bundle hash;
- runtime-layout/platform bundle hash;
- failover controller bundle hash;
- exact failover registry set;
- required systemd service/timer active/enabled.

### Tests

- wrong TX journal path -> drift failure;
- mutate one split Compose file -> drift failure;
- mutate Traefik template -> drift failure;
- extra failover registry file -> drift failure;
- systemd stopped/disabled -> drift failure.

---

## Task 8 — Verify previous release bundle before rollback

Rollback currently selects previous release config through `RELEASE_CONTEXT_DIR` / `COMPOSE_ROOT`, which is the correct direction, but recovery must not trust a retained release directory merely because it exists.

Before using a previous release bundle:

1. validate path is under canonical release root;
2. reject symlink release directory;
3. validate manifest SHA against canonical state/journal;
4. verify release artifact checksums;
5. where available, verify the retained Cosign manifest bundle against the expected workflow identity;
6. only then load previous split Compose config.

Use the **current trusted recovery engine** to execute the rollback using the verified previous runtime bundle. This is preferable to blindly executing older deploy scripts.

Clarify comments/docs accordingly: “exact previous runtime bundle” means previous images + previous configuration + previous route/platform assets, controlled by the trusted recovery engine.

### Tests

- previous compose untouched -> rollback allowed;
- previous compose tampered -> rollback fails before Docker mutation;
- previous manifest tampered -> rollback fails;
- release_dir symlink/path escape -> rollback fails.

---

## Task 9 — Make tests consume production artifacts instead of look-alike fixtures

This is the main lesson from the current branch: many tests are good, but a few synthetic fixtures use a different schema from production and therefore prove the wrong contract.

### Mandatory integration fixture pipeline

Build test input exactly like production:

```text
compute-promotion-scope.sh
        ↓
stage-platform-assets.sh
        ↓
generate-release-manifest.sh
        ↓
verify-manifest.sh (signature may be test-bypassed, content validation may not)
        ↓
release-state.py build
        ↓
init_rollout_journal/update_rollout_step
        ↓
dispatch / reconcile / rollback
```

Tests may mock Docker/systemd/network side effects, but **must not manually reinvent**:

- image key names;
- journal shape;
- canonical state shape;
- pending evidence shape.

### Add contract tests

One test should assert the set of component/image names is identical across:

- component map;
- promotion scope;
- manifest generator;
- manifest verifier;
- dispatcher exported vars;
- release-state builder;
- `.release.env` projection;
- drift verifier.

Fail CI on any naming divergence.

One test should assert journal schema consumers all use the same parser/helper.

---

## Task 10 — CI execution order and complete Production Contract

### New verify order

1. checkout
2. static syntax/ShellCheck/YAML/JSON validation
3. naming/schema contract tests
4. component classifier tests
5. Go/frontend/Python unit tests
6. deployment unit tests
7. real-artifact release integration simulator
8. chaos matrix
9. backup/restore drill
10. supply-chain validation
11. Docker smoke tests
12. Production Contract

This saves time when a 1-line shell error exists.

### Production Contract must require

- verify;
- gateway smoke;
- auth-browser smoke;
- TTS smoke;
- release integration simulator;
- chaos/recovery contract;
- no skipped mandatory job.

Do not interpret a skipped smoke job caused by upstream failure as success.

---

# P2 — Final production/governance gate

## Task 11 — Protect `main` before merging

At review time `main` is still unprotected and required status checks are not enforced.

Before merging this branch:

- enable branch protection/ruleset for `main`;
- require PR flow;
- require `Production Contract`;
- block force push;
- block branch deletion;
- require branch up-to-date before merge if compatible with workflow;
- optionally require signed commits if desired, but release artifact signing remains the important production trust boundary.

Do this before the final PR is merged so the final architecture cannot immediately be bypassed by direct push.

---

## Task 12 — Production dirty-state recovery before candidate rollout

Production has history from failed runs #247/#248, so final deployment must explicitly handle existing dirty evidence.

Do not start by blindly deleting journals/pending evidence.

### Sequence

```text
capture current canonical state + journal + pending evidence
        ↓
verify current live gateway/frontend/worker/aux state
        ↓
install signed recovery engine bootstrap only
        ↓
run reconcile-release.sh --recovery-only
        ↓
verify runtime exactly matches previous canonical state
        ↓
archive recovery evidence
        ↓
only then authorize candidate rollout
```

Recovery success requires:

- active gateway route matches canonical slot;
- frontend route/topology matches canonical state;
- worker/sidecar images match canonical digests;
- failover controller state is consistent;
- no pending mutation journal remains;
- worker polling is live before candidate rollout.

---

## Task 13 — Final acceptance run

Do not call the work complete until all gates below are met on one unchanged commit SHA.

### CI gate

```text
ShellCheck                         PASS
frontend TypeScript/build/tests    PASS
Go race + vet + tests              PASS
TTS tests                          PASS
failover tests                     PASS
promotion classifier               PASS
journal schema contracts           PASS
manifest/state naming contract     PASS
runtime layout                     PASS
bootstrap recovery                 PASS
legacy frontend recovery           PASS
exact runtime-bundle rollback      PASS
route fail-closed tests            PASS
intentional-stop fail-closed       PASS
full drift verifier                PASS
schema compatibility               PASS
incident #247/#248 simulations     PASS
crash/chaos matrix                 PASS
backup/restore drill               PASS
supply-chain manifest              PASS
Docker gateway smoke               PASS
Docker auth-browser smoke          PASS
Docker TTS smoke                   PASS
Production Contract                PASS
```

### Production acceptance

Run one rollout using the final commit:

1. preflight/recovery clean;
2. signed manifest verified;
3. schema compatibility gate passes;
4. component promotions pass;
5. route ACK passes;
6. **pre-soak candidate runtime contract passes**;
7. 900-second soak passes;
8. final runtime contract passes;
9. canonical schema-v2 state committed atomically;
10. `.release.env` regenerated from canonical state;
11. old slots cleanup succeeds or is durably queued without corrupting committed state;
12. `verify-runtime-drift.sh` passes against committed canonical state;
13. next read-production-state workflow reads the new generation/SHA/digests successfully.

### Rollback acceptance

In simulator/staging, prove on the exact final commit:

```text
Release A image/config/state
      ↓
Release B mutates every component
      ↓
inject failure at release-commit boundary
      ↓
recovery
      ↓
100% Release A runtime + route + images + config
```

No mixed component may remain.

---

# Required implementation order

Do these in this order to avoid fixing tests around a broken contract:

1. **Task 1** — ShellCheck blocker + fail-fast static CI.
2. **Task 2** — canonical naming/schema (`tts`, docs-only v2, explicit legacy).
3. **Task 3** — one rollout journal schema.
4. **Task 4** — explicit route/intentional-stop fail-closed.
5. **Task 6** — JSON cleanup evidence.
6. **Task 7** — real drift completeness + config bundle hashes.
7. **Task 8** — verify previous bundle before rollback.
8. **Task 5** — move runtime contract gate before long soak.
9. **Task 9** — rebuild tests around real production artifacts/contracts.
10. **Task 10** — final CI ordering/Production Contract.
11. **Task 11** — protect main.
12. **Task 12** — recover dirty production state.
13. **Task 13** — one final acceptance rollout.

Do **not** push intermediate commits to `main`. Keep all fixes on `hardening/zero-downtime-final`, let CI iterate there, then merge one reviewed final SHA.

---

# Definition of Done

The branch can be called final within the single-VPS scope only when:

- no safety function relies on implicit `set -e` propagation;
- manifest/state/component names have one schema;
- rollout journal has one schema and all readers consume it;
- tests generate real manifest/journal/state artifacts through production writers;
- pre-soak drift check executes before any 900-second wait;
- canonical state is the sole source of truth;
- recovery restores every completed component, not just routes;
- previous runtime bundle is verified before use;
- drift checks route, images, frontend topology, systemd, failover registry, journal path and config bundle;
- cleanup evidence is idempotent and format-consistent;
- all CI + Docker smoke + Production Contract gates pass on the same SHA;
- `main` is protected;
- one production acceptance rollout succeeds and the following production-state read confirms it.

Physical VPS/host/provider failure remains outside single-VPS zero-downtime guarantees.
