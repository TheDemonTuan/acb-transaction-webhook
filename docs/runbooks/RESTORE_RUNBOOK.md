# ACB Disaster Recovery Restore Runbook

This runbook specifies the step-by-step procedure for restoring an ACB deployment from encrypted `age` backups during catastrophic hardware failure, volume loss, or database corruption.

---

## 1. Disaster Recovery Order of Operations

In a total recovery scenario, execute steps in this exact sequence:

1. **Provision Clean Host & Infrastructure:** Ensure Docker Compose, Traefik edge, and Cloudflare tunnel are available.
2. **Restore Secrets:** Decrypt `secrets-YYYYMMDDHHMMSS.tar.age` into `/opt/acb-transaction-webhook/deploy/secrets/` with mode `0600`.
3. **Decrypt & Validate Database:** Run `deploy/restore-db.sh` using the private `age` recovery identity to verify SQLite integrity.
4. **Create Fresh Docker Volumes:** Recreate `gateway_data` and `bark_data`.
5. **Install Production Database:** Place validated database into the volume (`/data/gateway.db`).
6. **Install Production Environment & Release State:** Copy `.env.production` and release config.
7. **Start Core Singleton Workload:** Launch worker, auth-browser, tts-gateway, and bark.
8. **Start Active Gateway Slot:** Launch gateway (e.g. blue) and verify edge health probes.
9. **Validate Invariants & Sessions:** Confirm bank connection state or perform interactive re-authentication if session expired.

---

## 2. Decrypting Database Backups

The `restore-db.sh` utility decrypts backups in an isolated staging environment and runs SQLite integrity checks. **It never overwrites the live production database automatically.**

```bash
# Decrypt database using private recovery identity
./deploy/restore-db.sh \
  --identity /path/to/offhost/recovery-identity.txt \
  --backup /var/backups/acb/gateway-YYYYMMDDHHMMSS.db.age \
  --output /tmp/restored/gateway.db
```

Output:
```text
[RESTORE] Decrypting backup file using recovery key...
[RESTORE] Verifying SQLite integrity of decrypted database...
[RESTORE] Verifying backup against manifest...
[RESTORE] SUCCESS: Database restored and verified successfully.
Restored Database: /tmp/restored/gateway.db
SQLite Integrity:  ok
Schema Migrations: 8
```

---

## 3. Decrypting Secret Recovery Bundles

Secret bundles are tar archives encrypted with `age`:

```bash
mkdir -p /opt/acb-transaction-webhook/deploy/secrets
chmod 700 /opt/acb-transaction-webhook/deploy/secrets

age -d -i /path/to/offhost/recovery-identity.txt \
  /var/backups/acb/secrets-YYYYMMDDHHMMSS.tar.age | \
  tar -C /opt/acb-transaction-webhook/deploy/secrets -xf -

chmod 600 /opt/acb-transaction-webhook/deploy/secrets/*
```

Verify permissions and secret integrity:
```bash
./deploy/lib.sh # or ./deploy/tests/test_secrets.sh
```

---

## 4. Disaster Recovery Restore Drills

To verify disaster readiness without disrupting production services or accessing real production secrets, run the automated drill:

```bash
./scripts/ops/restore-drill.sh --drill-dir /tmp/canary-drill
```

The script:
- Generates ephemeral test keypairs and a fresh fixture database.
- Runs full backup encryption and manifests.
- Decrypts database and secrets in isolation.
- Asserts row counts, schema version, and SHA-256 checksums match.
- Records non-sensitive audit evidence to JSON.
