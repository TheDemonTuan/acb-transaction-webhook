# ACB Backup Runbook & Disaster Recovery Protocol

This runbook outlines standard procedures for routine automated database backups, independent secret backups, and disaster recovery off-host restoration using `age`.

---

## 1. Cryptographic Key Invariants

1. **Independent Encryption Key:** Database and secret backups are encrypted with an asymmetric `age` public key recipient (`BACKUP_AGE_RECIPIENT`).
2. **Never Stored on Host:** The matching private identity (`age1...` private key) is **NEVER** stored on the VPS host.
3. **No App Master Key Re-Use:** Routine database and secret backups do **NOT** use `APP_MASTER_KEY` for encryption.
4. **Ciphertext Only at Rest:** Plaintext SQLite snapshots and unencrypted secret files are strictly forbidden in durable backup directories (`/var/backups/acb`). Temporary staging snapshots are securely wiped (`rm -f`) immediately upon successful encryption.

---

## 2. Configuration Parameters

In production `/opt/acb-transaction-webhook/deploy/.env.production`:

```bash
# Public key recipient for age backup encryption (Public key ONLY on VPS)
BACKUP_AGE_RECIPIENT=age1...

# Local durable backup directory
BACKUP_DIR=/var/backups/acb

# Optional executable script to ship encrypted artifacts off-host (e.g., S3, rsync, SCP)
OFFHOST_BACKUP_HOOK=/usr/local/bin/acb-offhost-backup

# Enforce fail-closed deployment if off-host backup fails (default: 0)
REQUIRE_OFFHOST_BACKUP=0
```

---

## 3. Creating Database Backups

Database backups are automatically triggered before schema-changing deployments via `deploy/backup-db.sh`. Operators can also trigger on-demand backups:

```bash
/opt/acb-transaction-webhook/deploy/backup-db.sh
```

**What it does:**
1. Connects to SQLite database online using `VACUUM INTO` via `dbtool`.
2. Verifies SQLite database integrity (`PRAGMA integrity_check`) and schema migrations.
3. Encrypts the snapshot directly to `gateway-YYYYMMDDHHMMSS.db.age` using `age -r "$BACKUP_AGE_RECIPIENT"`.
4. Immediately deletes temporary mode-0600 plaintext snapshot.
5. Computes SHA-256 and writes companion manifest `manifest-YYYYMMDDHHMMSS.json`.
6. Executes `$OFFHOST_BACKUP_HOOK` if configured.

---

## 4. Creating Secret Recovery Bundles

Secret recovery backups are distinct from database backups and should be run whenever production secrets are provisioned or rotated:

```bash
/opt/acb-transaction-webhook/deploy/backup-secrets.sh
```

**What it does:**
1. Validates all 5 required production secrets are present in `/opt/acb-transaction-webhook/deploy/secrets`.
2. Directly tars and streams the secrets into `age`:
   `tar -C "$SECRETS_DIR" -cf - . | age -r "$BACKUP_AGE_RECIPIENT" -o "$BACKUP_DIR/secrets-YYYYMMDDHHMMSS.tar.age"`
3. Writes manifest `manifest-secrets-YYYYMMDDHHMMSS.json` with file names and SHA-256 checksums (no secret material leaked).
4. Invokes `$OFFHOST_BACKUP_HOOK` to ship the encrypted bundle off-host.
