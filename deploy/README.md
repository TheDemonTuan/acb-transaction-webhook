# ACB Production Deployment & Operations Standard

This directory contains the production deployment standard and transactional deployment scripts for the ACB Transaction Webhook platform on Single-VPS infrastructure using Traefik 3.x edge ingress, unified Docker Compose (`compose.prod.yaml`), and warm Blue/Green slot switching.

> **CRITICAL ARCHITECTURAL POLICY:**
> - **Monolithic Deployments Retired:** The legacy monolithic `deploy-warm.sh` and `--upgrade-core` flags have been **RETIRED AND HARD-FAIL SAFELY**.
> - **Zero-Impact Gateway Cutover:** Gateway deployments never restart the ACB worker, auth browser, TTS, or Bark containers.
> - **Singleton Worker Invariant:** Under no circumstances may two `acb-worker` instances run simultaneously.
> - **Immutable Digests Only:** All production images must be specified as immutable digests (`image@sha256:<64-hex>`). Tags such as `:latest` are strictly forbidden.
> - **Authoritative Runbook:** See [`docs/runbooks/DEPLOYMENT_RUNBOOK.md`](../docs/runbooks/DEPLOYMENT_RUNBOOK.md) for full operational procedures.

---

## 1. Architecture Topology

```text
Cloudflare Tunnel (172.31.250.2)
         │
         ▼
 Traefik 3.x (172.31.250.4:8080)
         │
 ┌───────┴────────────────────────────────────────┐
 │ File Provider Dynamic Route (/opt/edge/dynamic)│
 │ (Atomically points to active slot upstream)    │
 └───────┬────────────────────────────────────────┘
         │
         ▼
 ┌────────────────────────────────┐
 │ Gateway PRIMARY (Blue/Green)   │ (Running: HTTP/API/UI/SSE on :8090)
 └───────┬────────────────────────┘
         │
 ┌───────┴────────────────────────┐
 │ Gateway STANDBY (Green/Blue)   │ (Stopped: image pulled, config ready)
 └────────────────────────────────┘
         │
    SQLite WAL (Volume: bank-event-gateway_gateway_data)
         ▲
         │
 ┌────────────────────────────────┐
 │ Worker Singleton (:8190)       │ (Running continuously: Polling & Dispatcher)
 └────────────────────────────────┘
         │
 ┌───────┴────────────────────────┐
 │ Sidecars (Auth-Browser, TTS,   │
 │ Bark Push Notification Server) │
 └────────────────────────────────┘
```

---

## 2. Operational Procedures & Transition Modes

### Mode A: Fresh Host Provisioning & Bootstrap Transition
For a newly provisioned VPS with no existing data:
1. **Initialize Data Volumes:**
   ```bash
   ./deploy/init-fresh-data.sh --confirm-fresh-init
   ```
2. **Provision Production Secrets:**
   ```bash
   ./deploy/provision-secrets.sh --confirm-fresh-provision
   ```
3. **Configure Environment:**
   ```bash
   cp .env.example deploy/.env.production
   chmod 600 deploy/.env.production
   ```
4. **Initialize Release Environment Digests:**
   ```bash
   ./deploy/release-env.sh init \
     --gateway-blue ghcr.io/thedemontuan/acb-transaction-webhook@sha256:<gateway-digest> \
     --gateway-green ghcr.io/thedemontuan/acb-transaction-webhook@sha256:<gateway-digest> \
     --worker-image ghcr.io/thedemontuan/acb-transaction-webhook-worker@sha256:<worker-digest> \
     --dbtool-image ghcr.io/thedemontuan/acb-transaction-webhook-dbtool@sha256:<dbtool-digest> \
     --browser-image ghcr.io/thedemontuan/acb-transaction-webhook-auth-browser@sha256:<browser-digest> \
     --tts-image ghcr.io/thedemontuan/acb-transaction-webhook-tts-gateway@sha256:<tts-digest> \
     --bark-image ghcr.io/finb/bark-server@sha256:32d65b07fa835c99b31a396b77727a04ed058377fc2482da3e9dc7397167ffc4
   ```
5. **Initial Stack Launch in Dependency Order:**
   ```bash
   ./deploy/deploy-schema.sh ghcr.io/thedemontuan/acb-transaction-webhook-dbtool@sha256:<dbtool-digest>
   ./deploy/deploy-worker.sh ghcr.io/thedemontuan/acb-transaction-webhook-worker@sha256:<worker-digest>
   ./deploy/deploy-gateway.sh ghcr.io/thedemontuan/acb-transaction-webhook@sha256:<gateway-digest>
   ```

### Mode B: Legacy Monolith Migration
When upgrading from the legacy monolithic container:
1. Stop the monolith container to release SQLite database locks:
   ```bash
   docker stop acb-transaction-gateway || true
   ```
2. Back up database and execute schema migration:
   ```bash
   ./deploy/backup-db.sh
   ./deploy/deploy-schema.sh ghcr.io/thedemontuan/acb-transaction-webhook-dbtool@sha256:<dbtool-digest>
   ```
3. Launch worker and gateway components:
   ```bash
   ./deploy/deploy-worker.sh ghcr.io/thedemontuan/acb-transaction-webhook-worker@sha256:<worker-digest>
   ./deploy/deploy-gateway.sh ghcr.io/thedemontuan/acb-transaction-webhook@sha256:<gateway-digest>
   ```

### Mode C: Standard Gateway-Only Promotions (Continuous Deployment)
Normal continuous releases deploy **gateway-only** with zero impact on worker polling or active authentication sessions:
```bash
./deploy/deploy-gateway.sh ghcr.io/thedemontuan/acb-transaction-webhook@sha256:<new-digest>
```
Or via primary entrypoint (which delegates to the promotion dispatcher):
```bash
./deploy/deploy.sh ghcr.io/thedemontuan/acb-transaction-webhook@sha256:<new-digest>
```

---

## 3. Directory of Transactional Deployment Scripts

| Script | Subsystem Scope | Purpose | Safety Guarantees |
|---|---|---|---|
| `deploy/deploy-gateway.sh` | Gateway | Blue/Green isolated cutover | Preflight readiness, atomic Traefik switch, route identity ACK, automatic rollback |
| `deploy/deploy-worker.sh` | Worker | Singleton worker upgrade | Preflight active-auth gate, `/rpc/quiesce` handoff, candidate rollback |
| `deploy/deploy-schema.sh` | Schema | Database migrations | Read-only WAL probe, preflight encrypted backup, zero route changes |
| `deploy/deploy-auth-browser.sh`| Aux / Browser | Auth browser sidecar | Active-auth gate blocks replacement if login/OTP is active |
| `deploy/deploy-tts.sh` | Aux / Audio | TTS gateway sidecar | Isolated sidecar upgrade on internal `acb-core` network |
| `deploy/deploy-bark.sh` | Aux / Push | Bark notification sidecar | Isolated sidecar upgrade on internal `acb-core` network |
| `deploy/dispatch-rollout.sh` | Orchestration | Bounded rollout dispatcher | Verifies signed release manifest, executes in dependency order |
| `deploy/switch-slot.sh` | Ingress | Traefik route cutover | Validates YAML syntax, atomically replaces route file, verifies header ACK |
| `deploy/rollback.sh` | Gateway | Emergency route rollback | Reverts route pointer to warm standby slot; reactivates container |
| `deploy/backup-db.sh` | Storage | SQLite online backup | Uses `VACUUM INTO`, verifies integrity, encrypts with `age` public key |
| `deploy/restore-db.sh` | Storage | Database restore | Isolated staging restore using off-host `age` private key |

---

## 4. Rollback & State Recovery

### Emergency Gateway Rollback
```bash
/opt/acb-transaction-webhook/deploy/rollback.sh
```

### Worker Rollback
```bash
/opt/acb-transaction-webhook/deploy/deploy-worker.sh <previous-worker-digest>
```

### Transaction Journal Recovery
If a deployment was killed or interrupted unexpectedly, the next script execution automatically triggers `recover_tx_journal`, which reverts uncommitted route changes, stops dangling candidate containers, and cleans state.

---

## 5. Operational Runbook Directory

- **Deployment Runbook:** [`docs/runbooks/DEPLOYMENT_RUNBOOK.md`](../docs/runbooks/DEPLOYMENT_RUNBOOK.md)
- **Failover & Standby Runbook:** [`docs/runbooks/FAILOVER_RUNBOOK.md`](../docs/runbooks/FAILOVER_RUNBOOK.md)
- **Secret Provisioning Runbook:** [`docs/runbooks/SECRET_PROVISIONING_RUNBOOK.md`](../docs/runbooks/SECRET_PROVISIONING_RUNBOOK.md)
- **Backup Runbook:** [`docs/runbooks/BACKUP_RUNBOOK.md`](../docs/runbooks/BACKUP_RUNBOOK.md)
- **Restore Runbook:** [`docs/runbooks/RESTORE_RUNBOOK.md`](../docs/runbooks/RESTORE_RUNBOOK.md)
- **Disaster Recovery Runbook:** [`docs/runbooks/DISASTER_RECOVERY_RUNBOOK.md`](../docs/runbooks/DISASTER_RECOVERY_RUNBOOK.md)
- **Observability Runbook:** [`docs/runbooks/OBSERVABILITY.md`](../docs/runbooks/OBSERVABILITY.md)
- **Handoff Checklist:** [`docs/runbooks/HANDOFF_RELEASE_CHECKLIST.md`](../docs/runbooks/HANDOFF_RELEASE_CHECKLIST.md)
- **Operator Drills Template:** [`docs/runbooks/OPERATOR_DRILLS_TEMPLATE.md`](../docs/runbooks/OPERATOR_DRILLS_TEMPLATE.md)
