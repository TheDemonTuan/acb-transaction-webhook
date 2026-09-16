# ACB Production Deployment Runbook

> **OPERATIONAL STATUS & NON-EXECUTION NOTICE:**
> This runbook is the authoritative operational specification for deploying, upgrading, inspecting, and recovering components of the ACB Transaction Webhook platform on production infrastructure.
> **Documenting these operational procedures does NOT claim or imply that they have been executed on the live VPS during this phase.** All commands and procedures herein are strictly procedural instructions for human operators and automated CI workflows.
>
> **CRITICAL PRODUCTION OPERATING INVARIANTS:**
> 1. **NO MONOLITHIC DOWN:** Under NO circumstance may an operator or script run `docker compose down` or perform a whole-stack recreation in production. Every mutation must be scoped, bounded, and component-isolated.
> 2. **WORKER CONTINUITY IS SACROSANCT:** The single ACB polling worker (`acb-worker`) MUST NEVER be stopped or restarted for frontend, gateway, Bark, TTS, auth-browser, platform, or documentation releases. Worker restart is permitted ONLY during an explicit worker promotion or verified recovery of an actually mutated worker transaction. Bank polling sessions must remain uninterrupted.
> 3. **SINGLE VPS REALITY:** The production platform operates on a single VPS host. Operational instructions must never claim or assume host-level high availability or multi-host automatic failover.

---

## 1. Release Architecture & Scope Model

The ACB platform strictly forbids monolithic whole-stack restarts in production. Changes are classified by promotion scope (`scripts/compute-promotion-scope.sh`) and signed into an immutable release manifest (`deploy/release-manifest.json`).

### 1.1 Promotion Scope Classification
| Scope Name | Triggering Changes | Impacted Component Transactions |
|---|---|---|
| `docs_only` | Changes exclusively within `docs/**`, `*.md`, or repo metadata | Zero container mutations. Records release receipt only. |
| `schema` | Changes to `migrations/**`, database tooling, or storage schemas | `deploy/deploy-schema.sh` |
| `worker` | Changes to `internal/worker/**`, scheduler, monitor, storage, or worker cmd | `deploy/deploy-worker.sh` |
| `gateway` | Changes to `cmd/gateway/**`, `internal/httpapi/**`, `web/**` | `deploy/deploy-gateway.sh` |
| `auth_browser` | Changes to `cmd/auth-browser/**` or browser automation | `deploy/deploy-auth-browser.sh` |
| `tts` | Changes to `tts-gateway/**` | `deploy/deploy-tts.sh` |
| `bark` | Upstream Bark image or configuration updates | `deploy/deploy-bark.sh` |
| `failover_controller` | Controller code, systemd units, or app registry | `deploy/deploy-failover-controller.sh` |
| `mixed` / `full_stack` | Combined cross-component changes | Executed in strict dependency order via `deploy/dispatch-rollout.sh` |

---

## 2. Bounded Rollout Orchestrator (`deploy/dispatch-rollout.sh`)

In CI/CD (and for operator-driven multi-component upgrades), `deploy/dispatch-rollout.sh` orchestrates component updates in strict dependency order:

$$\text{schema} \longrightarrow \text{auxiliaries (auth-browser, tts, bark)} \longrightarrow \text{worker} \longrightarrow \text{gateway} \longrightarrow \text{failover controller} \longrightarrow \text{platform}$$

The dispatcher is the only release-state writer. After every authorized transaction, route acknowledgement, and soak succeeds, it atomically commits `$DEPLOY_PATH/state/current-release.json`. Component scripts never commit `.release.env`; that file is a compatibility projection only. A running worker must expose verified deployment protocol v2 capabilities. Missing or malformed capability/quiesce responses stop the rollout before the worker is stopped.

### Rollout Command
```bash
/opt/acb-transaction-webhook/deploy/dispatch-rollout.sh \
  --manifest /opt/acb-transaction-webhook/deploy/release-manifest.json \
  --bundle /opt/acb-transaction-webhook/deploy/release-manifest.bundle \
  --gateway-image ghcr.io/thedemontuan/acb-transaction-webhook@sha256:<digest>
```

**Guarantees Enforced:**
1. **Cosign Attestation:** Verifies signed manifest against trusted GitHub Actions OIDC identity before container interaction.
2. **VPS Preflight Checks (`deploy/preflight-vps.sh`):** Fails closed immediately if current-release.json, active gateway slot, Traefik route, container active images, or pending/journal files diverge from canonical state.
3. **Execution Lock:** Acquires exclusive host release lock `/run/lock/vps-failover/acb.lock` coordinated with failover controller.
4. **Transaction Journaling:** Records state transitions in `data/rollout-journal.json`; catches `INT`/`TERM` signals and marks state `INTERRUPTED`.
5. **Exact Previous Release Rollback:** Candidate failure rolls back using previous release compose configuration and images, never unverified candidate compose files.
6. **Compose Path Correctness:** Docker Compose always resolves relative file paths from the release root (`--project-directory "$RELEASE_DIR"`) ensuring secrets, `.env.production`, `bark-entrypoint.sh`, and `seccomp-auth-browser.json` resolve accurately.
7. **Evidence Archival:** Persists execution receipts, logs, and digests in `data/releases/<release_id>/`.

---

## 3. Immutable Staging & Release Bundle Architecture

Production release reliability requires a strict physical separation between signed, immutable release artifacts and shared mutable runtime state.

### 3.1 Immutable Release Directory vs. Shared Mutable State
All components operate under `$DEPLOY_PATH` (`/opt/acb-transaction-webhook`):

```text
/opt/acb-transaction-webhook/
├── releases/                                # IMMUTABLE RELEASE DIRECTORIES
│   └── <release_id>/                        # Signed release bundle root (read-only after verification)
│       ├── release-manifest.json            # Signed manifest with SHA-256 artifact map
│       ├── release-manifest.bundle          # Cosign Sigstore verification bundle
│       ├── compose/                         # Compose fragments (base, gateway, worker, etc.)
│       │   ├── base.yaml
│       │   ├── gateway.yaml
│       │   ├── worker.yaml
│       │   └── ...
│       └── <staged-engine-scripts>          # Exact verified deploy scripts for this release
│
├── state/                                   # SHARED MUTABLE RUNTIME STATE
│   ├── current-release.json                 # Authoritative canonical release state schema v2
│   ├── gateway-active-slot                  # "blue" or "green" pointer
│   ├── gateway-previous-slot                # Standby slot pointer
│   └── deploy-state.json                    # Transactional deployer state
│
├── data/                                    # OPERATIONAL DATA & AUDIT JOURNALS
│   ├── deploy-journal.json                  # Active component transaction journal
│   ├── rollout-journal.json                 # Rollout orchestrator journal
│   ├── pending-gateway-retire.env           # Explicit retire marker (if cleanup pending)
│   ├── evidence/                            # Archived run evidence & recovery receipts
│   └── backups/                             # Preflight SQLite snapshots
│
└── deploy/                                  # INSTALLED TRUSTED DEPLOYMENT ENGINE
    ├── .env.production                      # Active production environment (chmod 0600)
    ├── .release.env                         # Backward-compatible projection of canonical state
    ├── secrets/                             # Active production secrets (chmod 0700 dir, 0600 files)
    └── <engine-scripts>                     # Verified trusted deployer tools
```

**Golden Rules of Release Staging:**
- NO mutable state, runtime logs, Unix sockets, SQLite databases, or lock files may ever be written inside `releases/<release_id>/`.
- Candidate releases are staged in isolated directories (`releases/<candidate_release_id>/`) prior to executing any preflights or container commands.
- If a candidate rollout fails, its staged release directory is preserved for forensic inspection, but `$DEPLOY_PATH/state/current-release.json` remains pointed strictly to the canonical release.

### 3.2 Docker Compose `--project-directory` Path Resolution Invariant
Docker Compose commands executed across the platform must **never** rely on the process working directory (`$PWD`). Docker Compose resolves relative paths (such as `../secrets/app_master_key`, `./.env.production`, `./bark-entrypoint.sh`, and `./seccomp-auth-browser.json`) relative to the project directory.

**Mandatory Compose Command Form:**
```bash
docker compose \
  --project-directory "$RELEASE_DIR" \
  --env-file "$DEPLOY_PATH/deploy/.env.production" \
  -f "$RELEASE_DIR/compose/base.yaml" \
  -f "$RELEASE_DIR/compose/<component>.yaml" \
  <command>
```
This guarantees that whether invoked via CI SSH session, cron job, systemd service, or manual operator shell from any directory, relative path resolution remains 100% deterministic.

### 3.3 Staging Idempotency & Collision Protection
Staging is strictly idempotent:
- If a release directory `releases/<release_id>` already exists on the VPS, the staging engine verifies that its `release-manifest.json` and `release-manifest.bundle` are byte-for-byte identical to the incoming candidate.
- If existing files match the incoming signed bundle, the stage operation succeeds without error (idempotent re-run).
- If any byte mismatch or unverified file is detected within an existing directory, the operation fails closed immediately with `RELEASE_ID_COLLISION`. Re-using a release ID for differing content is strictly prohibited.

---

## 4. Cryptographic Verification & Signed Deployment Engine Bootstrapping

Production VPS mutation permits **zero execution of untrusted candidate code**. Candidate scripts cannot be executed to deploy themselves.

### 4.1 Cosign Attestation & GitHub Actions OIDC Trust
Every release manifest is cryptographically verified before any file is staged or read:
```bash
cosign verify-blob \
  --bundle "$RELEASE_DIR/release-manifest.bundle" \
  --certificate-identity "https://github.com/thedemontuan/acb-transaction-webhook/.github/workflows/deploy.yml@refs/heads/main" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
  "$RELEASE_DIR/release-manifest.json"
```

**Security Invariants:**
- The signing certificate subject is strictly pinned to `.github/workflows/deploy.yml@refs/heads/main`.
- Manifests signed from pull request workflows, feature branches, or local developer workstations fail verification and are rejected immediately.
- The OIDC issuer must strictly match GitHub Actions (`https://token.actions.githubusercontent.com`).

### 4.2 Manifest Artifact Hashing & Integrity Enforcement
The release manifest (`release-manifest.json`) includes an `artifacts` object containing the hex-encoded SHA-256 digests of every script, compose fragment, configuration file, and binary asset.

Before the release bundle is accepted:
1. Every file referenced in `artifacts` is checked for directory traversal attempts (`..` or leading `/` rejected).
2. The SHA-256 digest of each staged file is calculated and compared to the signed manifest.
3. If any file is missing, altered, or unlisted, the staging operation aborts fail-closed.

### 4.3 Safe Engine Bootstrapping (`deploy/bootstrap-deployment-engine.sh`)
When deployment engine tooling itself is upgraded (e.g. enhancements to `preflight-runtime.sh`, `dispatch-rollout.sh`, or `reconcile-release.sh`):
1. The candidate release bundle is staged and verified cryptographically.
2. The trusted bootstrap script upgrades only verified engine tooling into `$DEPLOY_PATH/deploy/`:
   ```bash
   bash /opt/acb-transaction-webhook/deploy/bootstrap-deployment-engine.sh \
     --release-dir "/opt/acb-transaction-webhook/releases/<verified_release_id>" \
     --runtime-deploy-dir "/opt/acb-transaction-webhook/deploy"
   ```
3. **Execution Guardrail:** `bootstrap-deployment-engine.sh` never starts/stops application containers, never changes Traefik routes, never runs database migrations, and never alters canonical release state. It provides a pure, non-destructive upgrade of deployment engine capabilities.

---

## 5. Gateway Canary Blue/Green & 900-Second Soak Verification Protocol

The HTTP Gateway is promoted using a zero-downtime canary progression backed by a mandatory 900-second (15-minute) traffic observation soak.

### 5.1 Canary Progression & Isolated Health Probing
1. **Slot Identification:** Determine active slot (`blue` or `green`) from `$DEPLOY_PATH/state/gateway-active-slot`. The alternate slot becomes the candidate.
2. **Isolated Candidate Startup:** Launch candidate container on its isolated internal host port (e.g. `green` on `127.0.0.1:8082` while `blue` serves live traffic on `127.0.0.1:8081`).
3. **Internal Readiness Probing:** Probe candidate endpoint `/internal/deployz`. Require 2 consecutive HTTP 200 responses with valid JSON status.
4. **Atomic Route Cutover:** Atomically render Traefik dynamic provider configuration (`/opt/platform/edge/dynamic/acb.yml`) pointing to the candidate slot.
5. **Edge Ingress Identity ACK:** Probe the public edge route through Traefik ingress. Positively verify that:
   - Response header `X-Platform-Slot` equals the candidate slot name.
   - Response header `X-Release-Commit` equals the candidate commit SHA.
   - HTTP status returns 200 OK.
   If header verification fails, the orchestrator reverts Traefik immediately to the previous slot.

### 5.2 Pre-Soak Contract Audit
Immediately following successful edge route cutover, and **before** commencing the 900-second soak timer:
```bash
# Automated audit of planned candidate state
bash /opt/acb-transaction-webhook/deploy/preflight-runtime.sh \
  --state /opt/acb-transaction-webhook/state/candidate/candidate-release.json \
  --check-only
```
The audit confirms that the candidate container is running the exact planned image digest, Traefik edge routing points to the candidate slot, and no orphan or uncommitted journals exist. If this pre-soak audit fails, the deployment aborts and triggers automatic rollback before traffic soak commitment.

### 5.3 900-Second Traffic Soak & Warm Standby Holding
Once pre-soak audit passes, the 900-second (15-minute) soak phase begins:
- The candidate gateway receives 100% of live incoming production webhooks and API calls.
- **Warm Standby Invariant:** The old gateway container remains running in warm standby throughout the entire 900-second soak window.
- The soak monitor periodically checks candidate container health, crash/restart count (`RestartCount == 0`), memory stability, and error logs.
- If the candidate container crashes or exhibits errors during the 900 seconds, an instantaneous route rollback is executed back to the warm standby container without service downtime.

### 5.4 Post-Soak Runtime Contract Audit & Atomic Canonical Commit
Upon completion of the 900 seconds:
1. **Post-Soak Contract Audit:** The orchestrator verifies that the candidate container maintained 100% uptime with zero restarts and zero 5xx error spikes during the soak.
2. **Single Atomic Commit:** The orchestrator atomically writes `$DEPLOY_PATH/state/current-release.json` with generation incremented, committing the release canonically.
3. **Standby Slot Decommissioning:** The previous gateway slot container is gracefully stopped into warm standby (`.intentional-stop-<old_slot>`), preserving its container image for instant manual rollback if needed later.

### 5.5 CI/CD Timeout Budgeting & Network Keepalive Architecture
To support the 15-minute (900s) soak without risk of pipeline truncation:
- **Deploy Step Timeout:** Must be configured to at least **25 to 30 minutes** in `.github/workflows/deploy.yml`. This provides a minimum of 10 minutes non-soak operational headroom for image pulls, health checks, preflights, and post-soak audits.
- **Deploy Job Timeout:** Must be configured to at least **30 to 35 minutes**, ensuring a minimum of 5 minutes buffer over the step timeout to allow clean step failure reporting.
- **Pre-Mutation Tool Bootstrap:** External tool downloads (such as Cosign) must occur and verify before any container is touched or route is modified. If host Cosign matches the pinned version and checksum, it is reused directly.
- **SSH Keepalive Configuration:** SSH client configs must include:
  ```text
  Host *
    ServerAliveInterval 15
    ServerAliveCountMax 10
    TCPKeepAlive yes
    ConnectTimeout 15
  ```
  This prevents stateful NAT/firewall drops during long quiet periods of the 900s soak.

---

## 6. Operator Procedure: Pre-Candidate VPS Inspection & Repair (Task 8 Runbook)

### 6.1 Purpose & Non-Destructive Principles (Run #249 Convergence)
This procedure is the mandatory operator runbook for inspecting and reconciling a dirty, drifted, or interrupted production VPS state (such as the interrupted run #249 state) **BEFORE** any new candidate release workflow is triggered.

**OPERATOR GOLDEN INVARIANTS:**
- **DO NOT USE `docker compose down`:** Monolithic stack tear-down will destroy live banking webhooks and kill active sessions.
- **DO NOT RESTART THE WORKER:** The ACB polling worker maintains stateful bank session cookies and scheduled sync quanta. Do NOT restart `acb-worker` unless worker image/container drift is separately and conclusively proven.
- **DO NOT BLINDLY RERUN RUN #249:** Retrying a failed workflow on a dirty host compounds state corruption. Follow this convergence procedure first.

---

### 6.2 Step 1: Freeze Automatic Deployment Retries
Before touching the VPS:
1. Ensure no GitHub Actions workflow runs are executing or queued for deployment.
2. Cancel any pending runs in the repository Actions tab.
3. Coordinate with team members to ensure no manual pushes to `main` occur during inspection.

---

### 6.3 Step 2: Non-Destructive Baseline Evidence Capture
Log in to the production VPS via SSH and create a timestamped evidence directory outside mutable release folders:

```bash
EVIDENCE_DIR="/opt/acb-transaction-webhook/data/evidence/pre-repair-$(date +%Y%m%d_%H%M%S)"
mkdir -p "$EVIDENCE_DIR"
cd /opt/acb-transaction-webhook

# 1. Capture authoritative state files
cp state/current-release.json "$EVIDENCE_DIR/current-release.json" 2>/dev/null || true
cp data/last-release.json "$EVIDENCE_DIR/last-release.json" 2>/dev/null || true
cp data/rollout-journal.json "$EVIDENCE_DIR/rollout-journal.json" 2>/dev/null || true
cp data/deploy-journal.json "$EVIDENCE_DIR/deploy-journal.json" 2>/dev/null || true
cp state/gateway-active-slot "$EVIDENCE_DIR/gateway-active-slot" 2>/dev/null || true
cp /opt/platform/edge/dynamic/acb.yml "$EVIDENCE_DIR/acb.yml" 2>/dev/null || true

# 2. Capture live container runtime states
docker inspect acb-gateway-blue acb-gateway-green acb-worker acb-bark acb-auth-browser acb-tts-gateway \
  > "$EVIDENCE_DIR/docker-inspect-containers.json" 2>&1 || true

# 3. Capture process table and locks
ps aux | grep -E "deploy|rollout|reconcile|acb" > "$EVIDENCE_DIR/ps-aux.txt"
ls -la /run/lock/vps-failover/ > "$EVIDENCE_DIR/locks.txt" 2>&1 || true
```

Review the captured state:
```bash
cat state/current-release.json
cat data/rollout-journal.json || true
cat /opt/platform/edge/dynamic/acb.yml
```

---

### 6.4 Step 3: Dynamic Verification of Canonical Target
Read the authoritative canonical target directly from `state/current-release.json`. Do **not** hardcode values, as state may have evolved:

```bash
python3 -c '
import json, sys
with open("state/current-release.json") as f:
    st = json.load(f)
print("Canonical Release ID:  ", st.get("release_id"))
print("Canonical Commit SHA:  ", st.get("commit_sha"))
print("Canonical Generation:  ", st.get("generation"))
print("Canonical Active Slot: ", st.get("active_slot", st.get("gateway_active_slot")))
print("Canonical Release Dir: ", st.get("release_dir"))
'
```
*(In the run #249 incident, canonical state reported generation `1`, commit SHA `373bf3d32f31fec3c484b5130d316689a0de89d7`, and active slot `blue`).*

Define variables based on the live canonical inspection:
```bash
CANONICAL_RELEASE_DIR="$(python3 -c 'import json; print(json.load(open("state/current-release.json"))["release_dir"])')"
CANONICAL_SLOT="$(python3 -c 'import json; st=json.load(open("state/current-release.json")); print(st.get("active_slot", st.get("gateway_active_slot")))')"
CANONICAL_COMMIT="$(python3 -c 'import json; print(json.load(open("state/current-release.json"))["commit_sha"])')"
```

---

### 6.5 Step 4: Recreate Canonical Active Gateway Slot (If Diverged)
Inspect whether the canonical gateway container (`acb-gateway-$CANONICAL_SLOT`) is running and healthy:

```bash
docker inspect "acb-gateway-$CANONICAL_SLOT" --format '{{.State.Status}} {{.Config.Image}}'
```

If `acb-gateway-$CANONICAL_SLOT` is stopped, missing, or running a mismatched image digest:
1. Recreate the container using the canonical release compose file and canonical digest:
   ```bash
   docker compose \
     --project-directory "$CANONICAL_RELEASE_DIR" \
     --env-file deploy/.env.production \
     -f "$CANONICAL_RELEASE_DIR/compose/base.yaml" \
     -f "$CANONICAL_RELEASE_DIR/compose/gateway.yaml" \
     up -d --no-recreate "acb-gateway-$CANONICAL_SLOT"
   ```
2. Proactively probe candidate readiness on its internal port (e.g. 8081 for blue, 8082 for green):
   ```bash
   PORT="$([[ "$CANONICAL_SLOT" == "blue" ]] && echo 8081 || echo 8082)"
   for i in {1..10}; do
     if curl -fsS "http://127.0.0.1:$PORT/internal/deployz"; then
       echo "Canonical gateway is ready!"
       break
     fi
     sleep 2
   done
   ```
   **DO NOT ROUTE TRAFFIC TO IT UNTIL IT REPORTS READY.**

---

### 6.6 Step 5: Switch Route Only After Health and Identity ACK
Once canonical gateway health is verified:
1. Render Traefik dynamic configuration to point to `$CANONICAL_SLOT`:
   ```bash
   bash deploy/render-traefik-route.sh \
     --slot "$CANONICAL_SLOT" \
     --output /opt/platform/edge/dynamic/acb.yml
   ```
2. Verify edge ingress identity response headers:
   ```bash
   curl -sS -I https://acb-api.yourdomain.com/healthz | grep -E "X-Platform-Slot|X-Release-Commit"
   ```
   Ensure `X-Platform-Slot: $CANONICAL_SLOT` and `X-Release-Commit: $CANONICAL_COMMIT` are returned.
3. Atomically update slot state pointer:
   ```bash
   printf '%s\n' "$CANONICAL_SLOT" > state/gateway-active-slot
   ```

---

### 6.7 Step 6: Verify Worker Continuity & Polling Intactness
Check the running `acb-worker` container:
```bash
docker inspect acb-worker --format 'ID: {{.Id}} | Status: {{.State.Status}} | StartedAt: {{.State.StartedAt}} | Image: {{.Config.Image}}'
curl -fsS http://127.0.0.1:8190/healthz
```
**Verification Requirement:**
- Container ID and `StartedAt` must reflect that the worker was **not** restarted during gateway/route repair.
- HTTP `/healthz` on port 8190 returns 200 OK.
- Bank polling continuity is preserved without interruption.

---

### 6.8 Step 7: Run Full Runtime Drift Verification
Execute the strict preflight runtime verification against the authoritative canonical state:
```bash
bash deploy/preflight-runtime.sh \
  --state /opt/acb-transaction-webhook/state/current-release.json \
  --check-only
```
**Acceptance Criterion:** The preflight script must return exit code 0 and report:
- `current-release.json` is valid and completed.
- Active slot matches Traefik dynamic route.
- Running container image digests match canonical state exactly.
- Zero drift and zero uncommitted transactions.

If preflight fails, resolve remaining discrepancies before continuing.

---

### 6.9 Step 8: Post-Convergence Evidence Archival
Only **after** Step 7 reports zero drift:
```bash
# Archive lingering dirty journals into the saved evidence directory
if [[ -f data/rollout-journal.json ]]; then
  mv data/rollout-journal.json "$EVIDENCE_DIR/rollout-journal.json.dirty"
fi

if [[ -f data/deploy-journal.json ]]; then
  mv data/deploy-journal.json "$EVIDENCE_DIR/deploy-journal.json.dirty"
fi

rm -f data/pending-gateway-retire.env 2>/dev/null || true
rm -f data/pending-frontend-retire.env 2>/dev/null || true

# Re-run preflight to confirm absolute clean status
bash deploy/preflight-runtime.sh \
  --state /opt/acb-transaction-webhook/state/current-release.json \
  --check-only
```
The VPS runtime is now fully converged, clean, and ready for future candidate releases.

---

## 7. Production Failure Drills & Recovery Playbooks

Standardized operational recovery playbooks for handling production deployment faults.

### 7.1 Drill 1: Candidate Gateway Health Check Failure (Pre-Cutover)
- **Fault Scenario:** Candidate container `acb-gateway-green` launches, but fails `/internal/deployz` readiness probes due to application crash or bad configuration.
- **System Behavior:**
  1. The deployer aborts the transaction before touching Traefik dynamic routes.
  2. Traefik dynamic routing remains 100% pointed to active slot `blue`.
  3. Candidate container `acb-gateway-green` is stopped.
  4. Transaction journal logs failure state `PROBE_FAILED`.
- **Operator Verification:**
  - Verify edge route is unaffected: `curl -fsS https://acb-api.yourdomain.com/healthz` returns `X-Platform-Slot: blue`.
  - Zero dropped webhook requests.
  - Review candidate container logs: `docker logs acb-gateway-green`.

### 7.2 Drill 2: Route Switch / Identity Header ACK Failure (Pre-Commit)
- **Fault Scenario:** Traefik dynamic route file is rewritten, but the post-switch identity probe fails (e.g. edge ingress returns 502, wrong commit SHA, or times out).
- **System Behavior:**
  1. The deployment engine detects identity ACK failure.
  2. Automatic rollback triggers immediately: Traefik dynamic file `/opt/platform/edge/dynamic/acb.yml` is rewritten back to the previous slot (`blue`).
  3. Warm standby container is maintained running.
  4. Transaction state marked `ROUTE_ACK_FAILED` -> `TX_ROLLED_BACK`.
- **Operator Verification:**
  - Ingress curl confirms immediate return to `X-Platform-Slot: blue`.
  - Canonical release state `state/current-release.json` remains untouched.

### 7.3 Drill 3: Abrupt Process Interruption (`SIGINT`, `SIGTERM`, OOM, Network Drop)
- **Fault Scenario:** An operator terminates SSH connection, the CI runner runner drops connection, or the deployment process receives `SIGKILL`/`SIGTERM` mid-flight.
- **System Behavior:**
  1. Process signal trap logs `INTERRUPTED` into `data/rollout-journal.json`.
  2. Host lock `/run/lock/vps-failover/acb.lock` remains present to prevent concurrent conflicting deployments.
  3. Live application traffic continues serving from whatever slot was active prior to the interruption.
- **Recovery Procedure:**
  1. Log into VPS via SSH.
  2. Check if the locking PID is alive:
     ```bash
     cat /run/lock/vps-failover/acb.lock
     ps aux | grep "<pid>"
     ```
  3. If process is dead, execute safe recovery:
     ```bash
     bash /opt/acb-transaction-webhook/deploy/preflight-runtime.sh \
       --state /opt/acb-transaction-webhook/state/current-release.json \
       --reconcile
     ```
  4. Reconciler stops uncommitted candidate containers, restores Traefik routing to canonical active slot, archives interrupted journal, and releases the execution lock.

### 7.4 Drill 4: Worker Upgrade / Quiesce Timeout Failure (Bank Polling Preservation)
- **Fault Scenario:** Worker upgrade is triggered, but running worker is executing an active bank login session (`-active-auth-count > 0`), or worker fails to acknowledge HTTP POST `/rpc/quiesce` within 30 seconds.
- **System Behavior:**
  1. Preflight auth check or quiesce timeout fails closed immediately.
  2. The running worker container `acb-worker` is **NEVER stopped or killed**.
  3. Deployment aborts cleanly with exit code 1.
  4. Scheduler quanta and live bank polling continue uninterrupted.
- **Operator Action:**
  - Inspect worker logs: `docker logs --tail 100 acb-worker`.
  - Check active auth status: `curl -fsS http://127.0.0.1:8190/status`.
  - Retry worker deployment only after active authentication finishes.

### 7.5 Drill 5: Canonical Runtime Recovery Drill
- **Fault Scenario:** A multi-component candidate deployment experiences failure during auxiliary or gateway rollout, leaving candidate containers running alongside canonical containers.
- **Recovery Requirement:** Recovery must reconcile strictly to the current canonical release (`state/current-release.json.release_dir`), never to unverified candidate compose files or older previous releases.
- **Operator Action:**
  ```bash
  CANONICAL_RELEASE="$(python3 -c 'import json; print(json.load(open("state/current-release.json"))["release_dir"])')"
  bash "$CANONICAL_RELEASE/reconcile-release.sh" --runtime-root /opt/acb-transaction-webhook --recovery-only
  ```
  Verify all containers, edge routes, and journals converge 100% to canonical state.

---

## 8. Production Governance & Manual Branch Protection Operations (Task 10)

Production stability depends on cryptographic alignment between source code governance and container deployment attestations.

### 8.1 GitHub Repository Ruleset Configuration for `refs/heads/main`
Direct unreviewed pushes to `main` violate production supply chain integrity and must be blocked via GitHub branch protection rulesets.

**Mandatory Repository Ruleset Settings:**
1. Navigate to: **GitHub Repo -> Settings -> Rules -> Rulesets -> New ruleset**.
2. **Ruleset Name:** `production-main-protection`
3. **Enforcement Status:** `Active`
4. **Target Branches:**
   - Include: `refs/heads/main` (Default branch)
5. **Branch Rules Configuration:**
   - [x] **Restrict deletions:** Prevent branch deletion.
   - [x] **Block force pushes:** Prevent `git push --force` or history rewriting.
   - [x] **Require a pull request before merging:**
     - Required approvals: `0` or `1` (Solo maintainer can use 0 approvals, but PR creation is strictly required).
     - [x] Dismiss stale pull request approvals when new commits are pushed.
     - [x] Require linear history (Merge commits or rebase/squash enforced).
   - [x] **Require status checks to pass before merging:**
     - [x] Require branches to be up to date before merging.
     - **Required Status Checks:**
       - `verify` (Production Contract / verification test suite)
       - `production-state` (VPS preflight drift check)
   - [x] **Do not allow bypassing the above settings:** Enforce rules on administrators.

### 8.2 Supply Chain & Cryptographic Trust Alignment
The production deployment workflow (`deploy.yml`) uses GitHub Actions OIDC keyless signing via Sigstore/Cosign:
- **OIDC Subject Claim:** `https://github.com/thedemontuan/acb-transaction-webhook/.github/workflows/deploy.yml@refs/heads/main`
- If an unverified direct push to `main` occurs, or if code is pushed bypassing pull request validation, supply chain provenance is broken.
- By enforcing branch protection, every production release is guaranteed to have passed automated contract tests and VPS drift verification before reaching production.

### 8.3 Governed Emergency Break-Glass Hotfix Procedure
In the rare event of a severe production outage where automated CI/CD is blocked:
1. **Audited Break-Glass Authorization:** Repository administrator temporarily enables break-glass emergency ruleset bypass. GitHub Audit Log records the authorization event.
2. **Manual Hotfix Application:** The hotfix is applied strictly using the signed release workflow or operator emergency script.
3. **Mandatory Post-Incident Reconciliation:**
   - Execute full runtime drift verification on the VPS:
     ```bash
     bash /opt/acb-transaction-webhook/deploy/preflight-runtime.sh \
       --state /opt/acb-transaction-webhook/state/current-release.json \
       --check-only
     ```
   - Immediately re-enable branch protection on `refs/heads/main`.
   - Submit a retroactive Pull Request with the hotfix commits to ensure the Git repository history matches the VPS canonical state.

---

## 9. Bootstrap Transition: First-Time Host Provisioning

When provisioning a fresh VPS from scratch:

### Step 1: Preflight Host Preparation
```bash
# Ensure deploy user and directories exist
sudo mkdir -p /opt/acb-transaction-webhook/{deploy,data,secrets}
sudo chown -R deploy:deploy /opt/acb-transaction-webhook
cd /opt/acb-transaction-webhook
```

### Step 2: Initialize Fresh Data Volume
```bash
# Initialize data volumes with explicit safety confirmation
deploy/init-fresh-data.sh --confirm-fresh-init
```

### Step 3: Provision Secrets
```bash
# Generate app_master_key, verify age recipient, restrict permissions to 0600
deploy/provision-secrets.sh --confirm-fresh-provision
```

### Bark Secret Runtime Permissions
If the Bark preflight reports a permissions error, inspect metadata without printing secret contents:

```bash
stat -c '%U:%G %a %n' deploy/secrets/bark_basic_auth_user deploy/secrets/bark_basic_auth_password
docker inspect acb-bark --format '{{json .Config.User}} {{json .HostConfig.GroupAdd}}'
```

The trusted deployer normalizes these two existing files without changing their owner/group and opens them from one-off Bark and worker identities before any component transaction. If the two files do not share a group, an operator must align them with the worker runtime group; do not use `chmod 644`, `chmod 777`, or rerun fresh provisioning. A `TX_ROLLBACK_FAILED` journal must be reconciled by the existing rollout recovery after permissions are repaired; do not delete the journal manually.

### Step 4: Configure Production Environment
```bash
cp .env.example deploy/.env.production
chmod 600 deploy/.env.production
# Edit deploy/.env.production with host domain, Cloudflare credentials, and ACB config
```

### Step 5: Initialize Release State
```bash
deploy/release-env.sh init \
  --gateway-blue ghcr.io/thedemontuan/acb-transaction-webhook@sha256:<gateway-digest> \
  --gateway-green ghcr.io/thedemontuan/acb-transaction-webhook@sha256:<gateway-digest> \
  --worker-image ghcr.io/thedemontuan/acb-transaction-webhook-worker@sha256:<worker-digest> \
  --dbtool-image ghcr.io/thedemontuan/acb-transaction-webhook-dbtool@sha256:<dbtool-digest> \
  --browser-image ghcr.io/thedemontuan/acb-transaction-webhook-auth-browser@sha256:<browser-digest> \
  --tts-image ghcr.io/thedemontuan/acb-transaction-webhook-tts-gateway@sha256:<tts-digest> \
  --bark-image ghcr.io/finb/bark-server@sha256:32d65b07fa835c99b31a396b77727a04ed058377fc2482da3e9dc7397167ffc4
```

### Step 6: Initial Component Launch
```bash
# Launch database schema first
deploy/deploy-schema.sh ghcr.io/thedemontuan/acb-transaction-webhook-dbtool@sha256:<dbtool-digest>

# Launch auxiliaries
deploy/deploy-auth-browser.sh ghcr.io/thedemontuan/acb-transaction-webhook-auth-browser@sha256:<browser-digest>
deploy/deploy-tts.sh ghcr.io/thedemontuan/acb-transaction-webhook-tts-gateway@sha256:<tts-digest>
deploy/deploy-bark.sh ghcr.io/finb/bark-server@sha256:32d65b07fa835c99b31a396b77727a04ed058377fc2482da3e9dc7397167ffc4

# Launch worker singleton
deploy/deploy-worker.sh ghcr.io/thedemontuan/acb-transaction-webhook-worker@sha256:<worker-digest>

# Launch initial Blue gateway slot
deploy/deploy-gateway.sh ghcr.io/thedemontuan/acb-transaction-webhook@sha256:<gateway-digest>
```

---

## 10. Component Transactions

### 10.1 Gateway-Only Blue/Green Deployment (`deploy/deploy-gateway.sh`)
Gateway deployment runs with **zero impact** on the ACB worker, auth session, browser, or sidecars:

```bash
/opt/acb-transaction-webhook/deploy/deploy-gateway.sh <gateway-image-digest>
```

**Lifecycle Steps:**
1. Identifies active slot (e.g. `blue`) and candidate slot (e.g. `green`).
2. Starts candidate container `acb-gateway-green` on internal port.
3. Validates candidate readiness via `/internal/deployz` (requires 2 consecutive HTTP 200 ACKs).
4. Atomically switches Traefik dynamic file provider (`/opt/edge/dynamic/acb.yml`).
5. Positively acknowledges edge route identity: verifies `X-Platform-Slot: green` and `X-Release-Commit` response headers from edge ingress.
6. If ACK fails: automatically reverts Traefik route back to previous slot and preserves old container.
7. If ACK succeeds: stops old candidate into warm standby (`.intentional-stop-blue`). Old container remains ready for instantaneous rollback.

### 10.2 Database Schema Migration (`deploy/deploy-schema.sh`)
```bash
/opt/acb-transaction-webhook/deploy/deploy-schema.sh <dbtool-image-digest>
```

**Lifecycle Steps:**
1. Verifies no active authentication session is in flight (`-active-auth-count == 0`); aborts fail-closed if check fails.
2. Triggers preflight online SQLite backup (`deploy/backup-db.sh`).
3. Executes schema migration via isolated `dbtool` container against SQLite WAL.
4. Validates schema migration count and table integrity.
5. **Never changes active edge route pointer.** If migration fails, Live DB is preserved and deployment halts cleanly.

### 10.3 Worker Singleton Upgrade (`deploy/deploy-worker.sh`)
```bash
/opt/acb-transaction-webhook/deploy/deploy-worker.sh <worker-image-digest>
```

**Lifecycle Steps:**
1. Preflight check: queries active auth status; aborts if bank authentication is actively running.
2. Graceful Quiesce RPC: issues HTTP POST `/rpc/quiesce` to running worker (:8190) with 30s timeout.
3. Running worker flushes pending scheduler quanta, writes session checkpoint to SQLite, requeues in-flight jobs, and closes listener.
4. Old worker container is stopped only after successful quiesce acknowledgment.
5. Candidate worker starts with new image digest.
6. Health probe validates `/healthz` and singleton lock ownership.
7. **Automatic Rollback:** If candidate worker fails health probes, the old worker image digest is automatically restarted to maintain bank monitoring continuity.

### 10.4 Auxiliary Services Deployment
- **Auth Browser:** `deploy/deploy-auth-browser.sh <browser-digest>` (fails closed if active auth session is present)
- **TTS Gateway:** `deploy/deploy-tts.sh <tts-digest>`
- **Bark Push Server:** `deploy/deploy-bark.sh <bark-digest>`

---

## 11. Rollback Procedures

### 11.1 Gateway Slot Rollback
To immediately revert Traefik route pointer to the warm standby slot:

```bash
# Reverts edge route to previous healthy slot and starts standby container
/opt/acb-transaction-webhook/deploy/rollback.sh
```

**What it executes:**
1. Starts stopped standby container (`acb-gateway-<prev>`).
2. Probes candidate health until ready.
3. Atomically switches `/opt/edge/dynamic/acb.yml` back to previous slot.
4. Acknowledges `X-Platform-Slot: <prev>` header.
5. Stops faulty current slot into warm standby.

### 11.2 Worker Rollback
If a newly promoted worker exhibits runtime issues:
```bash
# Re-deploy previous known-good worker digest
deploy/deploy-worker.sh <previous-worker-digest>
```
Or via release-env helper:
```bash
deploy/release-env.sh rollback WORKER_IMAGE_REF
```

### 11.3 Release Environment Rollback
To revert any component's pinned digest in `.release.env`:
```bash
deploy/release-env.sh rollback <KEY>
# E.g.: deploy/release-env.sh rollback GATEWAY_BLUE_IMAGE_REF
```

---

## 12. State Recovery & Dangling Transactions

### 12.1 Transaction Journal Crash Recovery
If a deployment process was terminated abruptly (e.g. OOM, host reboot, network drop), a dangling journal may remain:

```bash
# Built into all deploy scripts via lib/state.sh:
recover_tx_journal
```

**Recovery Behavior:**
- Scans `deploy-journal.json` for uncommitted candidate states (`PREFLIGHT`, `CONTAINER_START`, `PROBE`).
- Reverts Traefik route to active slot recorded in `.active-slot`.
- Stops any uncommitted candidate container.
- Moves corrupted/interrupted journal to `deploy-journal.json.recovered.<timestamp>`.
- Releases deployment lock if expired.

### 12.2 Manual Lock Release
If a process died without releasing `/run/lock/vps-failover/acb.lock`:
```bash
# Check if locking PID is alive
cat /run/lock/vps-failover/acb.lock
# If process is dead, safely clear lock:
rm -f /run/lock/vps-failover/acb.lock
```
