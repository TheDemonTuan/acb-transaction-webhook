# ACB Production Failover & Standby Runbook

This runbook outlines operational procedures for host-level failover between Primary and Standby VPS nodes, edge ingress switching, split-brain fencing, and state recovery.

---

## 1. Failover Topology & Invariants

```text
               +----------------------------------+
               |     Cloudflare Access / DNS      |
               +----------------+-----------------+
                                |
             Active Ingress     |     Standby Ingress
                   v            |            v
+-----------------------+       |    +-----------------------+
|      Primary VPS      |       |    |      Standby VPS      |
|  (/opt/edge: Traefik) | <-----+--> |  (/opt/edge: Traefik) |
|                       |            |                       |
| acb-gateway (Active)  |            | acb-gateway (Standby) |
| acb-worker (Active)   |            | acb-worker (Stopped)  |
| SQLite (Primary WAL)  |            | SQLite (Replica)      |
+-----------------------+            +-----------------------+
            ^                                    ^
            +---- Failover Heartbeat / Locking --+
```

### Key Invariants:
1. **Single Worker Invariant:** Under NO circumstances may `acb-worker` run simultaneously on both Primary and Standby nodes. Concurrent workers polling ACB with identical session tokens trigger anti-fraud lockouts and account freezes.
2. **Shared Release Lock:** Both deployment scripts and the failover controller coordinate through `/run/lock/vps-failover/acb.lock`. Deployment cannot proceed during active failover; failover cannot switch nodes mid-deployment.
3. **Fenced SQLite Access:** Only the active host's containers mount the production SQLite database volume in read-write mode.

---

## 2. Failover Controller (`platform/failover/vps-failover-controller.py`)

The failover controller daemon monitors node health and controls host transitions:

### Controller Verification & Configuration
```bash
# Verify controller script syntax and unit tests
python3 -m py_compile platform/failover/vps-failover-controller.py
python3 -m pytest platform/failover/test_failover.py
```

### Controller Parameters (`/etc/vps-failover/acb.conf`):
- `PRIMARY_HEARTBEAT_URL`: Gateway health probe URL (`http://<primary_ip>:8090/healthz`)
- `CHECK_INTERVAL_SEC`: Interval between health probes (default: 5s)
- `FAILURE_THRESHOLD`: Consecutive probe failures before declaring host down (default: 3)
- `LOCK_FILE`: `/run/lock/vps-failover/acb.lock`
- `STATE_DIR`: `/var/lib/vps-failover/apps/acb`

---

## 3. Automated Failover Detection & Execution

When the Primary VPS experiences unrecoverable hardware failure, network partition, or OS crash:

1. **Heartbeat Failure:** Failover controller on Standby detects 3 consecutive failed health probes (15 seconds total).
2. **Quorum / Fencing Check:** Controller verifies Primary is unreachable before initiating promotion.
3. **Lock Acquisition:** Controller acquires `/run/lock/vps-failover/acb.lock` on Standby node.
4. **Data Sync Check:** Checks latest replicated SQLite snapshot from off-host backup repository (`/var/backups/acb`).
5. **Database Initialization:** If primary volume is unshared, restores latest encrypted WAL snapshot via `deploy/restore-db.sh`.
6. **Container Launch:** Starts `acb-worker`, `acb-auth-browser`, `acb-tts-gateway`, `acb-bark`, and `acb-gateway-blue`.
7. **Route ACK Probe:** Verifies Standby gateway responds HTTP 200 on `/healthz`.
8. **Edge Ingress Switch:** Updates Cloudflare Tunnel route or DNS record to direct incoming traffic to Standby node.

---

## 4. Manual Failover Procedure (Planned Maintenance)

For planned VPS maintenance or migration:

### Step 1: Quiesce Primary Node
On Primary VPS:
```bash
# Acquire deployment lock to pause rollouts
exec 200>/run/lock/vps-failover/acb.lock
flock -x 200

# Quiesce worker via RPC (finishes current quantum, serializes session)
curl -s -X POST http://127.0.0.1:8190/rpc/quiesce

# Take final consistent database backup
/opt/acb-transaction-webhook/deploy/backup-db.sh

# Stop primary stack
cd /opt/acb-transaction-webhook/deploy
docker compose -f compose.prod.yaml stop
```

### Step 2: Sync Latest Data to Standby
```bash
# Transfer latest encrypted backup to Standby host
rsync -avz /var/backups/acb/ deploy@<standby_ip>:/var/backups/acb/
```

### Step 3: Promote Standby Node
On Standby VPS:
```bash
cd /opt/acb-transaction-webhook

# Restore latest snapshot if needed
deploy/restore-db.sh \
  --identity /root/.ssh/acb-recovery.key \
  --backup /var/backups/acb/gateway-<timestamp>.db.age \
  --output /var/lib/docker/volumes/bank-event-gateway_gateway_data/_data/gateway.db

# Start core services and active gateway
deploy/deploy-worker.sh <worker-digest>
deploy/deploy-gateway.sh <gateway-digest>
```

### Step 4: Retarget Edge Ingress
Update Cloudflare Tunnel credentials or edge routing:
```bash
# Verify edge ingress responds through Cloudflare Access
curl -fsSI "https://<public_domain>/healthz"
```

---

## 5. Post-Failover Verification & Reconciliation

1. **Verify Single Worker:** Check `docker ps` on Primary; ensure `acb-worker` is stopped.
2. **Verify Bank Polling:** Check worker logs on promoted node:
   ```bash
   docker logs --tail 50 -f acb-worker
   ```
   Confirm scheduler prints regular quantum cycles: `quanta_executed` increasing, 0 session conflicts.
3. **Verify Edge Headers:**
   ```bash
   curl -sI -H "Host: <public_host>" http://127.0.0.1:8090/healthz | grep -E "X-Platform-Slot|X-Release-Commit"
   ```
4. **Demotion of Old Primary:** When old host recovers, mark it explicitly as Standby (`docker compose stop`); never restart containers without re-synchronizing database state from the new Primary.
