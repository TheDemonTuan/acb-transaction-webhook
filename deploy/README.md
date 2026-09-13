# ACB Production Deployment & Operation Standard

This directory contains the production deployment baseline for the ACB Transaction Webhook platform on Single-VPS infrastructure using Traefik 3.x edge ingress, unified Docker Compose (`compose.prod.yaml`), and transactional Warm Standby Blue/Green cutover.

---

## 1. Architecture Overview

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

## 2. Operational Modes

### Mode A: Fresh Initial Deployment
For a brand new VPS with no pre-existing data:
1. Initialize data volumes with explicit confirmation:
   ```bash
   ./deploy/init-fresh-data.sh --confirm-fresh-init
   ```
2. Start core services and initial Blue gateway:
   ```bash
   ./deploy/deploy-warm.sh --upgrade-core ghcr.io/thedemontuan/acb-transaction-webhook@sha256:<gateway-digest>
   ```

### Mode B: Migration from Legacy Monolith
When transitioning from the legacy monolithic `acb-transaction-gateway`:
1. Safely stop the legacy monolith container to release SQLite WAL locks:
   ```bash
   docker stop acb-transaction-gateway || true
   ```
2. Run database migration and verification via immutable dbtool:
   ```bash
   ./deploy/backup.sh
   docker run --rm --user 1000:1000 \
     -v bank-event-gateway_gateway_data:/data:rw \
     ghcr.io/thedemontuan/acb-transaction-webhook-dbtool@sha256:<dbtool-digest> \
     -path /data/gateway.db -migrate
   ```
3. Deploy stack with core services enabled:
   ```bash
   ./deploy/deploy.sh --upgrade-core \
     ghcr.io/thedemontuan/acb-transaction-webhook@sha256:<gateway-digest> \
     ghcr.io/thedemontuan/acb-transaction-webhook-auth-browser@sha256:<browser-digest> \
     ghcr.io/thedemontuan/acb-transaction-webhook-tts-gateway@sha256:<tts-digest>
   ```

### Mode C: Already-Bluegreen Warm Standby Releases (Standard)
Normal continuous releases deploy **web-only** and MUST NOT restart or pull worker/core:
```bash
./deploy/deploy-warm.sh ghcr.io/thedemontuan/acb-transaction-webhook@sha256:<new-digest>
```
Or via primary entrypoint:
```bash
./deploy/deploy.sh ghcr.io/thedemontuan/acb-transaction-webhook@sha256:<new-digest>
```

---

## 3. Canonical Environment Variables & Image References

All production images must be specified as **explicit immutable digests** (`image@sha256:<64-hex>`). Tags such as `:latest` are rejected.

| Component | Canonical Env Var | Compose Variable Aliases |
|-----------|-------------------|--------------------------|
| Gateway | `IMAGE_REF` | `GATEWAY_IMAGE_REF`, `IMAGE_REF_BLUE`, `IMAGE_REF_GREEN` |
| Worker | `WORKER_IMAGE_REF` | `WORKER_IMAGE_REF` |
| DB Tool | `DBTOOL_IMAGE_REF` | `DBTOOL_IMAGE_REF` |
| Auth Browser | `AUTH_BROWSER_IMAGE_REF` | `BROWSER_IMAGE_REF` |
| TTS Gateway | `TTS_GATEWAY_IMAGE_REF` | `TTS_IMAGE_REF` |
| Bark Server | `BARK_IMAGE_REF` | `BARK_IMAGE_REF` |
| Gateway Data | `DATA_VOLUME_NAME` | `bank-event-gateway_gateway_data` |
| Bark Data | `BARK_VOLUME_NAME` | `bank-event-gateway_bark_data` |

---

## 4. Data Safety & Preflight Guarantees

1. **Volume Existence Requirement**:
   `DATA_VOLUME_NAME` must exist prior to deployment. If the volume is missing or ambiguous, deployment aborts immediately. The deployment tooling never creates, deletes, or renames production data except through the explicit `init-fresh-data.sh --confirm-fresh-init` command.
2. **Online SQLite Backup & Integrity Verification**:
   Before schema migrations or core modification, an online SQLite backup is executed (`.backup` with `PRAGMA wal_checkpoint(TRUNCATE)`), followed by `PRAGMA integrity_check;` and schema migration verification.
3. **Backup Manifest**:
   A cryptographic manifest (`manifest-<timestamp>.json`) records SHA-256 digests and file sizes for the database backup, secrets (`app_master_key`, internal tokens, basic auth credentials), and state.
4. **Encrypted Offhost Hook**:
   If `ENCRYPTED_BACKUP_HOOK` or `OFFHOST_BACKUP_HOOK` is configured, its executable status is verified before execution.
5. **No Auto Restore**:
   If a database migration fails, the deployment aborts and active traffic remains on the existing active slot. Automated restoration of production databases is forbidden to prevent overwriting live transactions.
6. **Active-Auth Gate**:
   Before modifying core services or running migrations, the active-auth gate verifies that zero authentication attempts are in-flight (`STARTING`, `IN_PROGRESS`, `EXPORTING`, `VERIFYING`). If an active login session exists, deployment aborts to protect customer authentication.

---

## 5. Central Controller CLI Contract & Intentional Stop Handshake

The VPS failover controller (`/opt/platform/failover/vps-failover-controller.py`) manages automatic local failover across containers on the host.

### Controller CLI Contract:
- The controller switches routes by executing:
  ```bash
  /bin/bash deploy/switch-slot.sh <blue|green>
  ```
- **No Nested Locks**: Route switching primitives check lock reentrancy (`DEPLOY_LOCK_HELD=1` or `SKIP_LOCK=1`) to avoid deadlock when invoked from deployment scripts or the failover daemon.
- **Intentional Stop Handshake**:
  When deployment stops the old slot into warm standby after soak completion, it marks an intentional stop marker (`/tmp/vps-failover/acb.intentional-stop` and sets failover cooldown) **BEFORE** issuing the `docker stop` command. This ensures the controller does not mistake scheduled container stopping for an unexpected failure.

---

## 6. Warm Cutover State Machine & Rollback

1. **Candidate Startup**: Candidate slot starts with target image while active slot handles live traffic.
2. **Readiness Probe**: Candidate must pass health checks (`/gateway --healthcheck` and `/readyz`).
3. **Atomic Route Switch**: Traefik dynamic configuration is atomically updated (`mv -f`).
4. **Route Identity ACK**: Active route resolution is verified. If acknowledgement fails, automatic route rollback to the previous slot executes immediately.
5. **15-Minute Resumable Soak**: Old slot remains running during the soak period. If candidate fails during soak, automatic rollback restores the previous slot.
6. **Emergency Rollback**:
   ```bash
   ./deploy/rollback-warm.sh
   ```

---

## 7. High Availability Scope: No Host-Loss HA

**Important Architecture Notice**:
The Single-VPS deployment architecture provides high availability against **software-level failures** on a single host (container crash, memory exhaustion, hung process, zero-downtime release).

It **DOES NOT** provide host-loss HA:
- If the virtual machine or physical host suffers hardware failure, network disconnection, or host destruction, the system will experience downtime.
- Disaster recovery requires provisioning a new VPS and restoring the encrypted offsite backup using the backup manifest and master key.
