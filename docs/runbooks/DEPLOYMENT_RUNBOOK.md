# ACB Production Deployment Runbook

This runbook is the authoritative operational guide for deploying, upgrading, and recovering components of the ACB Transaction Webhook platform on production infrastructure.

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
2. **Execution Lock:** Acquires exclusive host release lock `/run/lock/vps-failover/acb.lock` coordinated with failover controller.
3. **Transaction Journaling:** Records state transitions in `data/rollout-journal.json`; catches `INT`/`TERM` signals and marks state `INTERRUPTED`.
4. **Evidence Archival:** Persists execution receipts, logs, and digests in `data/releases/<release_id>/`.

---

## 3. Bootstrap Transition: First-Time Host Provisioning

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

Bark and the worker share only `bark_basic_auth_user` and `bark_basic_auth_password`. The deployment keeps both files at mode `0640`, preserves their existing common numeric group, and gives Bark that group as a supplementary group. The worker continues to consume the same Compose secret mounts with its existing runtime configuration. Never make these files world-readable and never regenerate them while repairing permissions.

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

## 4. Component Transactions

### 4.1 Gateway-Only Blue/Green Deployment (`deploy/deploy-gateway.sh`)
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

### 4.2 Database Schema Migration (`deploy/deploy-schema.sh`)
```bash
/opt/acb-transaction-webhook/deploy/deploy-schema.sh <dbtool-image-digest>
```

**Lifecycle Steps:**
1. Verifies no active authentication session is in flight (`-active-auth-count == 0`); aborts fail-closed if check fails.
2. Triggers preflight online SQLite backup (`deploy/backup-db.sh`).
3. Executes schema migration via isolated `dbtool` container against SQLite WAL.
4. Validates schema migration count and table integrity.
5. **Never changes active edge route pointer.** If migration fails, Live DB is preserved and deployment halts cleanly.

### 4.3 Worker Singleton Upgrade (`deploy/deploy-worker.sh`)
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

### 4.4 Auxiliary Services Deployment
- **Auth Browser:** `deploy/deploy-auth-browser.sh <browser-digest>` (fails closed if active auth session is present)
- **TTS Gateway:** `deploy/deploy-tts.sh <tts-digest>`
- **Bark Push Server:** `deploy/deploy-bark.sh <bark-digest>`

---

## 5. Rollback Procedures

### 5.1 Gateway Slot Rollback
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

### 5.2 Worker Rollback
If a newly promoted worker exhibits runtime issues:
```bash
# Re-deploy previous known-good worker digest
deploy/deploy-worker.sh <previous-worker-digest>
```
Or via release-env helper:
```bash
deploy/release-env.sh rollback WORKER_IMAGE_REF
```

### 5.3 Release Environment Rollback
To revert any component's pinned digest in `.release.env`:
```bash
deploy/release-env.sh rollback <KEY>
# E.g.: deploy/release-env.sh rollback GATEWAY_BLUE_IMAGE_REF
```

---

## 6. State Recovery & Dangling Transactions

### 6.1 Transaction Journal Crash Recovery
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

### 6.2 Manual Lock Release
If a process died without releasing `/run/lock/vps-failover/acb.lock`:
```bash
# Check if locking PID is alive
cat /run/lock/vps-failover/acb.lock
# If process is dead, safely clear lock:
rm -f /run/lock/vps-failover/acb.lock
```
