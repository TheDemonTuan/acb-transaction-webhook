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
2. Run database backup and migration via age and immutable dbtool:
   ```bash
   ./deploy/backup-db.sh
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

All production images must be specified as **explicit immutable digests** (`image@sha256:<64-hex>`). Tags such as `:latest` or `:-` fallback values are strictly forbidden and rejected at preflight.

Production state is separated into two canonical files:
1. **Runtime Host Configuration**: `/opt/acb-transaction-webhook/deploy/.env.production` (chmod 600, host-managed, no secret/image churn)
2. **Release Component State**: `/opt/acb-transaction-webhook/deploy/.release.env` (chmod 600, atomic read/write via `deploy/release-env.sh`)

| Component | Canonical Release Key | Compose Variable Requirement |
|-----------|-----------------------|------------------------------|
| Gateway Active (Blue) | `IMAGE_REF_BLUE` | `${IMAGE_REF_BLUE:?IMAGE_REF_BLUE is required}` |
| Gateway Standby (Green) | `IMAGE_REF_GREEN` | `${IMAGE_REF_GREEN:?IMAGE_REF_GREEN is required}` |
| Worker Singleton | `WORKER_IMAGE_REF` | `${WORKER_IMAGE_REF:?WORKER_IMAGE_REF is required}` |
| Database Migration Tool | `DBTOOL_IMAGE_REF` | `${DBTOOL_IMAGE_REF:?DBTOOL_IMAGE_REF is required}` |
| Auth Browser Headless | `BROWSER_IMAGE_REF` | `${BROWSER_IMAGE_REF:?BROWSER_IMAGE_REF is required}` |
| TTS Gateway | `TTS_IMAGE_REF` | `${TTS_IMAGE_REF:?TTS_IMAGE_REF is required}` |
| Bark Server (Push) | `BARK_IMAGE_REF` | `${BARK_IMAGE_REF:?BARK_IMAGE_REF is required}` |
| Gateway Data Volume | `DATA_VOLUME_NAME` | `bank-event-gateway_gateway_data` |
| Bark Data Volume | `BARK_VOLUME_NAME` | `bank-event-gateway_bark_data` |

---

## 4. Runtime Hardening & Isolation Policy

Production Docker Compose enforces the following security and isolation invariants:
- **Read-Only Root Filesystems**: All first-party services (`worker`, `tts-gateway`, `bark`, `gateway-blue`, `gateway-green`, `dbtool`) enforce `read_only: true`.
- **Non-Root Execution**: First-party services execute under non-root UID `1000:1000`.
- **Zero Linux Capabilities**: All containers drop all capabilities (`cap_drop: [ALL]`).
- **No Privilege Escalation**: Every container enforces `no-new-privileges:true`.
- **Dedicated Seccomp**: `auth-browser` attaches `seccomp-auth-browser.json`.
- **Network Segmentation**:
  - `edge-acb`: Internal network joined only by gateway slots and Bark. `worker` is strictly prohibited from `edge-acb`.
  - `acb-core`: Internal mesh for inter-service communication (`worker`, `auth-browser`, `tts-gateway`, `bark`, `gateway-blue`, `gateway-green`).
  - `acb-egress`: Outbound-capable external network for upstream bank and API calls.
  - `none`: Isolated zero-network namespace for `dbtool`.
- **Resource Constraints**: Strict limits on CPUs, memory, and PIDs enforced at both service-level (`cpus`, `mem_limit`, `pids_limit`) and `deploy.resources.limits`.
- **No Published Ports & No Source Mounts**: Containers expose internal ports only to internal networks; no development source trees or Docker socket mounts are permitted.
- **Audit Verification**: Preflight validation via `deploy/verify-compose-runtime.sh`.

---

## 4. Data Safety & Preflight Guarantees

1. **Volume Existence Requirement**:
   `DATA_VOLUME_NAME` must exist prior to deployment. If the volume is missing or ambiguous, deployment aborts immediately. The deployment tooling never creates, deletes, or renames production data except through the explicit `init-fresh-data.sh --confirm-fresh-init` command.
2. **Online SQLite Backup & Integrity Verification**:
   Before schema migrations or core modification, an online SQLite backup is executed (`.backup` with `PRAGMA wal_checkpoint(TRUNCATE)`), followed by `PRAGMA integrity_check;` and schema migration verification.
3. **Backup Encryption at Rest**:
   Backups are automatically encrypted with OpenSSL AES-256-CBC using PBKDF2 with the host's `app_master_key` (`.db.enc`). A cryptographic manifest (`manifest-<timestamp>.json`) records SHA-256 digests and file sizes for both the SQLite database backup, encrypted backup, and secrets.
4. **Encrypted Offhost Hook**:
   If `ENCRYPTED_BACKUP_HOOK` or `OFFHOST_BACKUP_HOOK` is configured, its executable status is verified before execution.
5. **No Auto Restore**:
   If a database migration fails, the deployment aborts and active traffic remains on the existing active slot. Automated restoration of production databases is forbidden to prevent overwriting live transactions.
6. **Active-Auth Gate**:
   Before modifying core services or running migrations, the active-auth gate verifies that zero authentication attempts are in-flight (`STARTING`, `IN_PROGRESS`, `EXPORTING`, `VERIFYING`). If an active login session exists, deployment aborts to protect customer authentication.
7. **Threat Model & Storage Security**:
   - **Credentials & Session Secrets**: ACB passwords, OAuth credentials, session cookies, and internal service tokens are encrypted at rest using AES-GCM 256 via the cryptographic Keyring (`internal/crypto`) derived from `app_master_key`.
   - **Transaction Indexing & Metadata**: Transaction descriptions and metadata are indexed in SQLite for high-performance substring searching, pagination, and deduplication (`semantic_key`, `canonical_hash`).
   - **OS & Volume Isolation**: The SQLite database volume is secured with Linux file mode `0600`, confined to non-root UID `1000:1000`, protected by host full-disk encryption (LUKS), and backed up with AES-256 encryption.

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

1. **Candidate Startup**: Candidate slot starts with target immutable digest while active slot handles live traffic.
2. **Readiness Probe**: Candidate must pass health probes (`/internal/deployz`) twice consecutively with slot/schema/worker verification.
3. **Atomic Route Switch**: Traefik dynamic configuration is rendered, YAML-validated, and atomically updated (`mv -f`).
4. **Route Identity ACK**: Active edge route identity is verified via `X-Platform-Slot` and `X-Release-Commit` response headers. If acknowledgement fails, automatic route rollback to previous slot executes immediately.
5. **15-Minute Resumable Soak**: Old slot remains running during the soak period. If candidate fails during soak, automatic rollback restores the previous slot.
6. **Emergency Rollback**:
   ```bash
   ./deploy/rollback-warm.sh
   ```

---

## 7. Edge Ownership Boundaries & Route Derivation

The ACB repository owns strictly its dynamic route configuration at `/opt/edge/dynamic/acb.yml`.
- Shared Traefik 3.x and Cloudflare Tunnel infrastructure belong to the host platform and must NEVER be restarted by application deployments.
- Host routing rule is dynamically derived from `PUBLIC_ORIGIN` (or canonical `PUBLIC_HOST`).
- Dynamic routing enforces `/internal` denial via `deny-internal` middleware.
- Internal promotion gates (`/internal/deployz`) require internal worker tokens and cannot be reached through public edge routes.
- Transaction journal at `deploy/data/deploy-journal.json` tracks deploy lifecycle with automatic trap cleanup and crash recovery.

---

## 8. High Availability Scope: No Host-Loss HA

**Important Architecture Notice**:
The Single-VPS deployment architecture provides high availability against **software-level failures** on a single host (container crash, memory exhaustion, hung process, zero-downtime release).

It **DOES NOT** provide host-loss HA:
- If the virtual machine or physical host suffers hardware failure, network disconnection, or host destruction, the system will experience downtime.
- Disaster recovery requires provisioning a new VPS and restoring the encrypted offsite backup using the backup manifest and master key.
