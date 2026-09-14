# ACB Production Disaster Recovery & Cold-Iron Restoration Runbook

This runbook defines the operational protocol for restoring the ACB Transaction Webhook platform from catastrophic failure, total VPS loss, hardware destruction, or unrecoverable database corruption.

---

## 1. Disaster Recovery Prerequisites

Before initiating restoration, ensure you have:
1. **Private Recovery Identity:** Secure access to `acb-recovery-identity.txt` (stored off-host in password vault).
2. **Encrypted Backup Artifacts:** Access to the off-host backup repository containing:
   - Database snapshot: `gateway-YYYYMMDDHHMMSS.db.age`
   - Secrets archive: `secrets-YYYYMMDDHHMMSS.tar.age`
3. **Fresh Host / Clean VPS:** Debian 12 / Ubuntu 24.04 LTS instance with Docker Engine, Docker Compose v2, and Git.
4. **Cloudflare Access & DNS Control:** Access to Cloudflare Zero Trust dashboard to point tunnels or DNS records to the new host.

---

## 2. Order of Operations

Disaster recovery must follow this strict sequence:

$$\text{Host Prep} \longrightarrow \text{Decrypt Secrets} \longrightarrow \text{Decrypt Database} \longrightarrow \text{Volume Setup} \longrightarrow \text{Launch Stack} \longrightarrow \text{Validate Invariants}$$

---

## 3. Step-by-Step Recovery Procedure

### Step 1: Clone Repository & Prepare Directory Layout
On the new host:
```bash
sudo mkdir -p /opt/acb-transaction-webhook/{deploy,data,secrets}
sudo chown -R deploy:deploy /opt/acb-transaction-webhook
git clone https://github.com/TheDemonTuan/acb-transaction-webhook.git /opt/acb-transaction-webhook
cd /opt/acb-transaction-webhook
```

### Step 2: Retrieve Recovery Key to Secure Staging Area
```bash
# Place the off-host private recovery key in a temporary secure file (NOT under repo or backups)
mkdir -p /root/recovery
chmod 700 /root/recovery
# Copy acb-recovery-identity.txt securely (e.g. via scp or terminal paste)
chmod 600 /root/recovery/acb-recovery-identity.txt
```

### Step 3: Decrypt Secrets Archive
```bash
# Decrypt the secrets archive using age
age -d -i /root/recovery/acb-recovery-identity.txt \
  /path/to/backups/secrets-latest.tar.age | tar -x -C /opt/acb-transaction-webhook/deploy/secrets/

# Enforce secure permissions immediately
chmod 700 /opt/acb-transaction-webhook/deploy/secrets
chmod 600 /opt/acb-transaction-webhook/deploy/secrets/*

# Verify app_master_key is present and valid
python3 -c "import base64; k = open('/opt/acb-transaction-webhook/deploy/secrets/app_master_key', 'rb').read().strip(); raw = base64.b64decode(k); assert len(raw) == 32; print('Master key verified: 32 bytes')"
```

### Step 4: Decrypt and Validate SQLite Database (`deploy/restore-db.sh`)
Execute the restore utility in a temporary staging area to guarantee SQLite integrity before moving data into the live volume:

```bash
mkdir -p /tmp/dr-staging
deploy/restore-db.sh \
  --identity /root/recovery/acb-recovery-identity.txt \
  --backup /path/to/backups/gateway-latest.db.age \
  --output /tmp/dr-staging/gateway.db
```

**Output Must Show:**
```text
[RESTORE] Decrypting backup file using recovery key...
[RESTORE] Verifying SQLite integrity of decrypted database...
[RESTORE] SUCCESS: Database restored and verified at /tmp/dr-staging/gateway.db
```

### Step 5: Provision Production Volumes and Mount Database
```bash
# Initialize data volumes
deploy/init-fresh-data.sh --confirm-fresh-init

# Copy verified database into volume mount point
docker volume create bank-event-gateway_gateway_data
docker volume create bank-event-gateway_bark_data

docker run --rm \
  -v bank-event-gateway_gateway_data:/data \
  -v /tmp/dr-staging:/staging \
  alpine sh -c "cp /staging/gateway.db /data/gateway.db && chown -R 1000:1000 /data"

# Clean up staging area and private key from host
rm -rf /tmp/dr-staging
rm -rf /root/recovery
```

### Step 6: Configure Environment and Release State
```bash
# Configure runtime environment
cp .env.example deploy/.env.production
chmod 600 deploy/.env.production
# Populate deploy/.env.production with host domain and public keys

# Initialize release environment with known-good digests
deploy/release-env.sh init \
  --gateway-blue ghcr.io/thedemontuan/acb-transaction-webhook@sha256:<gateway-digest> \
  --gateway-green ghcr.io/thedemontuan/acb-transaction-webhook@sha256:<gateway-digest> \
  --worker-image ghcr.io/thedemontuan/acb-transaction-webhook-worker@sha256:<worker-digest> \
  --dbtool-image ghcr.io/thedemontuan/acb-transaction-webhook-dbtool@sha256:<dbtool-digest> \
  --browser-image ghcr.io/thedemontuan/acb-transaction-webhook-auth-browser@sha256:<browser-digest> \
  --tts-image ghcr.io/thedemontuan/acb-transaction-webhook-tts-gateway@sha256:<tts-digest> \
  --bark-image ghcr.io/finb/bark-server@sha256:32d65b07fa835c99b31a396b77727a04ed058377fc2482da3e9dc7397167ffc4
```

### Step 7: Launch Stack in Dependency Order
```bash
# 1. Run schema verification
deploy/deploy-schema.sh ghcr.io/thedemontuan/acb-transaction-webhook-dbtool@sha256:<dbtool-digest>

# 2. Launch auxiliary sidecars
deploy/deploy-auth-browser.sh ghcr.io/thedemontuan/acb-transaction-webhook-auth-browser@sha256:<browser-digest>
deploy/deploy-tts.sh ghcr.io/thedemontuan/acb-transaction-webhook-tts-gateway@sha256:<tts-digest>
deploy/deploy-bark.sh ghcr.io/finb/bark-server@sha256:32d65b07fa835c99b31a396b77727a04ed058377fc2482da3e9dc7397167ffc4

# 3. Launch singleton worker
deploy/deploy-worker.sh ghcr.io/thedemontuan/acb-transaction-webhook-worker@sha256:<worker-digest>

# 4. Launch active Gateway slot
deploy/deploy-gateway.sh ghcr.io/thedemontuan/acb-transaction-webhook@sha256:<gateway-digest>
```

---

## 4. Post-Restoration Verification Checklist

- [ ] Private recovery key file `/root/recovery` deleted from host (`ls /root/recovery` returns No such file).
- [ ] Database contains expected transaction history (`sqlite3 gateway.db "SELECT count(*) FROM transactions"`).
- [ ] Worker logs confirm scheduler active (`docker logs --tail 30 acb-worker`).
- [ ] Edge route responds HTTP 200 with matching `X-Platform-Slot` header.
- [ ] Re-establish ACB banking session if authentication credentials/cookies expired during outage.
