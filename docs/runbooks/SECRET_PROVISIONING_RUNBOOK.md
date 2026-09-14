# ACB Production Secret Provisioning & Key Management Runbook

This runbook specifies procedures for provisioning, configuring, isolating, and rotating cryptographic keys and secrets for the ACB Transaction Webhook platform.

---

## 1. Secrets Storage Architecture & Invariants

```text
/opt/acb-transaction-webhook/deploy/
├── .env.production           (chmod 600, deploy:deploy - Runtime config & public keys)
└── secrets/                  (chmod 700, deploy:deploy)
    ├── app_master_key        (chmod 600, 32 bytes base64 - Encrypts banking sessions in SQLite)
    ├── webhook_secret        (chmod 600 - Signs outgoing webhooks with HMAC-SHA256)
    ├── cf_access_client_id   (chmod 600 - Cloudflare Access service token ID)
    ├── cf_access_client_secret (chmod 600 - Cloudflare Access service token secret)
    └── acb_password          (chmod 600 - Encrypted or raw bank login credentials)
```

### Critical Security Invariants:
1. **Directory Isolation:** Production secrets reside exclusively in `${DEPLOY_PATH}/deploy/secrets/` with strict POSIX permissions `0600` (files) and `0700` (directory).
2. **Asymmetric Backup Key Separation:** The `age` private recovery identity is **STRICTLY FORBIDDEN** on the production VPS. Only the public recipient key (`BACKUP_AGE_RECIPIENT`) resides in `.env.production`.
3. **No Plaintext Backups:** Plaintext secret dumps and unencrypted databases must never be written to durable storage directories (`/var/backups/acb` or `/opt/acb/data`).
4. **Preflight Validation:** All deployment and startup scripts validate that required secret files exist, are non-empty, and adhere to `0600` permissions. Missing or insecure secrets cause deployments to abort fail-closed.

---

## 2. Generating the Master Encryption Key (`app_master_key`)

The Application Master Key encrypts sensitive ACB banking credentials, cookies, and tokens inside SQLite:

```bash
# Generate exactly 32 bytes of cryptographically secure random data (base64 encoded)
openssl rand -base64 32 > /opt/acb-transaction-webhook/deploy/secrets/app_master_key
chmod 600 /opt/acb-transaction-webhook/deploy/secrets/app_master_key

# Verify decoded byte length is exactly 32 bytes
python3 -c "import base64; k = open('/opt/acb-transaction-webhook/deploy/secrets/app_master_key', 'rb').read().strip(); raw = base64.b64decode(k); assert len(raw) == 32, f'Expected 32 bytes, got {len(raw)}'; print('Key valid: 32 bytes raw')"
```

---

## 3. Generating the Asymmetric `age` Backup Keypair

Backup encryption uses modern asymmetric `age` cryptography:

### On Secure Operator Workstation (OFF-HOST ONLY):
```bash
# Generate age keypair on workstation
age-keygen -o acb-recovery-identity.txt
chmod 600 acb-recovery-identity.txt

# Display public recipient
cat acb-recovery-identity.txt | grep "public key:"
# Example output: # public key: age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p
```

### Store Keys Securely:
- **Private Key (`acb-recovery-identity.txt`):** Store in company password manager / hardware security module. **NEVER COPY TO VPS.**
- **Public Recipient Key:** Configure in `/opt/acb-transaction-webhook/deploy/.env.production`:
  ```bash
  BACKUP_AGE_RECIPIENT=age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p
  ```

---

## 4. Automated Secret Provisioning Script (`deploy/provision-secrets.sh`)

For initial bootstrap or non-interactive host setup:

```bash
# Run automated secret provisioning with explicit confirmation
/opt/acb-transaction-webhook/deploy/provision-secrets.sh --confirm-fresh-provision
```

**Verification Performed:**
- Verifies directory `deploy/secrets/` is created with permissions `0700`.
- Generates `app_master_key` if missing.
- Sets file permissions to `0600` for all contained secrets.
- Validates that no private keys or forbidden credentials are leaked.

---

## 5. Secret Validation Harness

Execute the automated secrets test suite to verify enforcement across all boundary conditions:

```bash
bash deploy/tests/test_secrets.sh
```

**Verifies:**
- Deployment fails when secrets directory is missing or permissions are lax (`0777`).
- Deployment fails when `app_master_key` is missing, empty, or corrupt.
- Backup encryption fails closed if `BACKUP_AGE_RECIPIENT` is invalid or missing.
- Temporary files created during secret operations are securely shredded.

---

## 6. Secret Rotation Procedures

### 6.1 Webhook Secret Rotation
1. Generate new webhook secret: `openssl rand -hex 32 > deploy/secrets/webhook_secret.new`.
2. Configure downstream consumer systems to accept both old and new signatures.
3. Promote new secret: `mv -f deploy/secrets/webhook_secret.new deploy/secrets/webhook_secret`.
4. Deploy gateway: `deploy/deploy-gateway.sh <current-gateway-digest>`.

### 6.2 Cloudflare Access Service Token Rotation
1. Generate new Service Token in Cloudflare Zero Trust dashboard.
2. Update `deploy/secrets/cf_access_client_id` and `deploy/secrets/cf_access_client_secret`.
3. Verify permissions: `chmod 600 deploy/secrets/cf_access_*`.
4. Restart gateway slot to pick up new tokens.
