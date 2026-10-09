#!/usr/bin/env bash
# Disaster Recovery Restore Drill and Canary Decrypt Validation
# Proves age encrypted database and secrets restore into an isolated environment without touching production.
set -euo pipefail
umask 077

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "$SCRIPT_DIR/../.." && pwd)"
DEPLOY_DIR="$REPO_ROOT/deploy"

DRILL_DIR=""
EVIDENCE_FILE=""

usage() {
  cat <<EOF
Usage: $0 --drill-dir <path> [options]

Required:
  -d, --drill-dir <path>    Explicit isolated working directory for restore drill

Optional:
  -e, --evidence <path>     Path to output evidence JSON record
  -h, --help                Show this help message

SAFETY NOTICE: This script strictly refuses to operate inside live production data volumes.
EOF
  exit 1
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    -d|--drill-dir)
      DRILL_DIR="$2"; shift 2 ;;
    -e|--evidence)
      EVIDENCE_FILE="$2"; shift 2 ;;
    -h|--help)
      usage ;;
    *)
      printf 'Unknown option: %s\n' "$1" >&2
      usage ;;
  esac
done

if [[ -z "$DRILL_DIR" ]]; then
  printf 'ERROR: Missing required --drill-dir argument.\n' >&2
  usage
fi

# 1. Fail closed if drill dir matches or is inside live production volume/path
if [[ "$DRILL_DIR" == "/data" || "$DRILL_DIR" == "/data/"* ]]; then
  printf 'RESTORE DRILL REFUSED: Target drill directory (%s) is inside the production data root.\n' "$DRILL_DIR" >&2
  exit 1
fi

abs_drill="$(realpath -m -- "$DRILL_DIR")"
live_data_dir="${DATA_DIR:-$REPO_ROOT/deploy/data}"
abs_live="$(realpath -m -- "$live_data_dir")"

if [[ "$abs_drill" == "$abs_live" || "$abs_drill" == "$abs_live/"* || "$abs_drill" == "/data" || "$abs_drill" == "/data/"* || "$abs_drill" == *gateway_data* ]]; then
  printf 'RESTORE DRILL REFUSED: Target drill directory (%s) overlaps with live production data directory (%s).\n' "$abs_drill" "$abs_live" >&2
  exit 1
fi

mkdir -p "$abs_drill"
[[ ! -L "$abs_drill" ]] || { printf 'RESTORE DRILL REFUSED: Target is a symlink.\n' >&2; exit 1; }
chmod 700 "$abs_drill"
ts="$(date -u +'%Y%m%d%H%M%S')"
drill_id="drill-${ts}"

log() {
  printf '[%s] [RESTORE-DRILL] %s\n' "$(date -u +'%Y-%m-%dT%H:%M:%SZ')" "$*"
}

log "Starting disaster recovery restore drill in isolated directory: ${abs_drill}"

# Locate age, age-keygen, and dbtool
AGE_BIN="age"
AGE_KEYGEN_BIN="age-keygen"
DBTOOL_BIN="${DBTOOL_BIN:-dbtool}"
if ! command -v "$AGE_BIN" >/dev/null 2>&1; then
  if [[ -x "${HOME}/go/bin/age" ]]; then
    AGE_BIN="${HOME}/go/bin/age"
  elif [[ -x "/usr/local/bin/age" ]]; then
    AGE_BIN="/usr/local/bin/age"
  fi
fi
if ! command -v "$AGE_KEYGEN_BIN" >/dev/null 2>&1; then
  if [[ -x "${HOME}/go/bin/age-keygen" ]]; then
    AGE_KEYGEN_BIN="${HOME}/go/bin/age-keygen"
  elif [[ -x "/usr/local/bin/age-keygen" ]]; then
    AGE_KEYGEN_BIN="/usr/local/bin/age-keygen"
  fi
fi
if ! command -v "$DBTOOL_BIN" >/dev/null 2>&1; then
  if [[ -x "${HOME}/go/bin/dbtool" ]]; then
    DBTOOL_BIN="${HOME}/go/bin/dbtool"
  elif [[ -x "/usr/local/bin/dbtool" ]]; then
    DBTOOL_BIN="/usr/local/bin/dbtool"
  fi
fi

if ! command -v "$AGE_BIN" >/dev/null 2>&1 && [[ ! -x "$AGE_BIN" ]]; then
  printf 'ERROR: age binary is missing. Restore drill cannot proceed.\n' >&2
  exit 1
fi
if ! command -v "$AGE_KEYGEN_BIN" >/dev/null 2>&1 && [[ ! -x "$AGE_KEYGEN_BIN" ]]; then
  printf 'ERROR: age-keygen binary is missing. Restore drill cannot proceed.\n' >&2
  exit 1
fi

# 2. Generate ephemeral test recovery identity and recipient
identity_file="$abs_drill/drill_identity.txt"
"$AGE_KEYGEN_BIN" -o "$identity_file" 2>/dev/null
chmod 600 "$identity_file" 2>/dev/null || true

recipient="$(grep "^# public key: " "$identity_file" | cut -d' ' -f4 | tr -d ' \r\n')"
if [[ -z "$recipient" ]]; then
  printf 'ERROR: Failed to extract recipient public key from generated identity file.\n' >&2
  exit 1
fi
log "Generated ephemeral recovery keypair. Recipient: ${recipient}"

# 3. Create isolated source fixture environment
fixture_dir="$abs_drill/fixture"
mkdir -p "$fixture_dir/data" "$fixture_dir/secrets" "$fixture_dir/backups"
chmod 700 "$fixture_dir" "$fixture_dir/secrets" "$fixture_dir/backups" 2>/dev/null || true

fixture_db="$fixture_dir/data/gateway.db"

# Use the real candidate schema, not a reduced substitute for financial tables.
command -v sqlite3 >/dev/null || { printf 'ERROR: sqlite3 required for financial restore drill\n' >&2; exit 1; }
if ! command -v "$DBTOOL_BIN" >/dev/null 2>&1 && [[ ! -x "$DBTOOL_BIN" ]]; then
  printf 'ERROR: candidate dbtool required; set DBTOOL_BIN to the compiled binary\n' >&2
  exit 1
fi
"$DBTOOL_BIN" -path "$fixture_db" -migrate >/dev/null
sqlite3 "$fixture_db" <<'SQL'
PRAGMA foreign_keys=ON;
BEGIN;
INSERT INTO connections(id,bank_code,state,created_at,updated_at) VALUES('restore-acb','ACB','PAUSED','2026-10-09T00:00:00Z','2026-10-09T00:00:00Z');
INSERT INTO transactions(id,connection_id,semantic_key,canonical_hash,transaction_date,effective_date,credit,parser_version,first_seen_at)
VALUES('tx_001','restore-acb','restore:1','hash:1','2026-10-09T00:00:00Z','2026-10-09T00:00:00Z',150000,'v1','2026-10-09T00:00:00Z'),
('tx_002','restore-acb','restore:2','hash:2','2026-10-09T00:00:00Z','2026-10-09T00:00:00Z',300000,'v1','2026-10-09T00:00:00Z'),
('tx_003','restore-acb','restore:3','hash:3','2026-10-09T00:00:00Z','2026-10-09T00:00:00Z',450000,'v1','2026-10-09T00:00:00Z'),
('tx_payos','payos-klb','PAYOS:restore:ref','payos:hash','2026-10-09T00:00:00Z','2026-10-09T00:00:00Z',2000,'payos-v1','2026-10-09T00:00:00Z');
INSERT INTO payment_orders(id,order_code,channel_id,idempotency_key,request_hash,amount_vnd,description,origin,status,payment_link_id,transaction_id,created_at,updated_at,expires_at,paid_at)
VALUES('restore-order',100000000001,'restore-channel','00000000-0000-4000-8000-000000000001','restore-hash',2000,'DH100000000001','STATIC_URL','PAID','restore-link','tx_payos','2026-10-09T00:00:00Z','2026-10-09T00:00:00Z','2026-10-09T00:30:00Z','2026-10-09T00:00:00Z');
INSERT INTO payment_receipts(channel_id,reference,order_id,payment_link_id,amount_vnd,transaction_at,canonical_hash,transaction_id,received_at)
VALUES('restore-channel','restore-reference','restore-order','restore-link',2000,'2026-10-09T00:00:00Z','payos:hash','tx_payos','2026-10-09T00:00:00Z');
COMMIT;
SQL
fixture_schema="$(sqlite3 "$fixture_db" 'SELECT count(*) FROM schema_migrations;')"
chmod 600 "$fixture_db" 2>/dev/null || true

# Create fixture secrets
printf 'test_master_key_32_bytes_len_01234567890123456789012345678912\n' > "$fixture_dir/secrets/app_master_key"
printf 'test_worker_token_canary_0123456789\n' > "$fixture_dir/secrets/worker_internal_token"
printf 'test_tts_token_canary_0123456789\n' > "$fixture_dir/secrets/tts_internal_token"
printf 'test_bark_admin\n' > "$fixture_dir/secrets/bark_basic_auth_user"
printf 'test_bark_pass_canary_0123456789\n' > "$fixture_dir/secrets/bark_basic_auth_password"
for f in "$fixture_dir/secrets"/*; do
  chmod 600 "$f" 2>/dev/null || true
done

# 4. Run automated backup-db.sh against fixture
log "Running backup-db.sh using test recipient..."
export BACKUP_AGE_RECIPIENT="$recipient"
export BACKUP_DIR="$fixture_dir/backups"
export SECRETS_DIR="$fixture_dir/secrets"
export DATA_DIR="$fixture_dir/data"
export DATABASE_PATH="$fixture_db"

backup_artifact="$("$DEPLOY_DIR/backup-db.sh" | tail -n 1 | tr -d '\r\n')"
log "Created backup artifact: ${backup_artifact}"

# 5. Run automated backup-secrets.sh against fixture
log "Running backup-secrets.sh using test recipient..."
secret_artifact="$("$DEPLOY_DIR/backup-secrets.sh" | tail -n 1 | tr -d '\r\n')"
log "Created secrets bundle: ${secret_artifact}"

# 6. Execute restore drill into clean canary target directory
canary_target="$abs_drill/canary_restored"
mkdir -p "$canary_target"
chmod 700 "$canary_target" 2>/dev/null || true

restored_db_out="$canary_target/gateway-canary.db"

log "Restoring and decrypting database using restore-db.sh..."
"$DEPLOY_DIR/restore-db.sh" \
  --identity "$identity_file" \
  --backup "$backup_artifact" \
  --output "$restored_db_out"

if [[ ! -s "$restored_db_out" ]]; then
  printf 'FAIL: Decrypted database is missing or empty\n' >&2
  exit 1
fi

# 7. Verify restored database integrity and row counts
log "Verifying restored database integrity and row counts..."
integrity_status="$(sqlite3 "$restored_db_out" 'PRAGMA integrity_check;')"
[[ "$integrity_status" == ok ]] || { printf 'FAIL: Restored database integrity\n' >&2; exit 1; }
migrations_count="$(sqlite3 "$restored_db_out" 'SELECT count(*) FROM schema_migrations;')"
tx_count="$(sqlite3 "$restored_db_out" 'SELECT count(*) FROM transactions;')"
order_count="$(sqlite3 "$restored_db_out" 'SELECT count(*) FROM payment_orders;')"
receipt_count="$(sqlite3 "$restored_db_out" 'SELECT count(*) FROM payment_receipts;')"
[[ "$migrations_count" == "$fixture_schema" && "$tx_count" == 4 && "$order_count" == 1 && "$receipt_count" == 1 ]] || { printf 'FAIL: Restored financial row counts mismatch\n' >&2; exit 1; }
cmp -s <(sqlite3 "$fixture_db" .dump) <(sqlite3 "$restored_db_out" .dump) || { printf 'FAIL: Restored financial database rows differ\n' >&2; exit 1; }
[[ "$(sqlite3 "$restored_db_out" 'SELECT amount_vnd FROM payment_receipts WHERE reference="restore-reference";')" == 2000 ]] || { printf 'FAIL: payOS receipt amount changed\n' >&2; exit 1; }
log "Integrity: ${integrity_status} (migrations: ${migrations_count}, transactions: ${tx_count})"

# 8. Restore and verify secret bundle
restored_secrets_dir="$canary_target/secrets"
mkdir -p "$restored_secrets_dir"
chmod 700 "$restored_secrets_dir" 2>/dev/null || true

log "Decrypting secret recovery bundle into isolated directory..."
"$AGE_BIN" -d -i "$identity_file" "$secret_artifact" | tar -C "$restored_secrets_dir" -xf -

for s in app_master_key worker_internal_token tts_internal_token bark_basic_auth_user bark_basic_auth_password; do
  if [[ ! -s "$restored_secrets_dir/$s" ]]; then
    printf 'FAIL: Restored secret is missing: %s\n' "$s" >&2
    exit 1
  fi
  orig_sha="$(sha256sum "$fixture_dir/secrets/$s" | cut -d' ' -f1)"
  rest_sha="$(sha256sum "$restored_secrets_dir/$s" | cut -d' ' -f1)"
  if [[ "$orig_sha" != "$rest_sha" ]]; then
    printf 'FAIL: Restored secret checksum mismatch for %s\n' "$s" >&2
    exit 1
  fi
done
log "All 5 required production secrets restored and verified with exact checksum match; payOS configuration is preserved in the restored database."

# 9. Record Evidence JSON (No secret values!)
release_commit="$(git -C "$REPO_ROOT" rev-parse HEAD 2>/dev/null || echo "unknown")"
db_sha="$(sha256sum "$backup_artifact" | cut -d' ' -f1)"
sec_sha="$(sha256sum "$secret_artifact" | cut -d' ' -f1)"

if [[ -z "$EVIDENCE_FILE" ]]; then
  EVIDENCE_FILE="$abs_drill/restore-drill-evidence-${ts}.json"
fi
mkdir -p "$(dirname "$EVIDENCE_FILE")"

cat <<EOF > "$EVIDENCE_FILE"
{
  "drill_id": "${drill_id}",
  "timestamp": "$(date -u +'%Y-%m-%dT%H:%M:%SZ')",
  "release_commit": "${release_commit}",
  "db_artifact": {
    "file": "$(basename "$backup_artifact")",
    "sha256": "${db_sha}"
  },
  "secrets_artifact": {
    "file": "$(basename "$secret_artifact")",
    "sha256": "${sec_sha}"
  },
  "schema_version": ${migrations_count},
  "integrity_check": "${integrity_status}",
  "table_counts": {
    "schema_migrations": ${migrations_count},
    "transactions": ${tx_count},
    "payment_orders": ${order_count},
    "payment_receipts": ${receipt_count}
  },
  "recipient_fingerprint": "$(printf '%s' "$recipient" | sha256sum | cut -d' ' -f1)",
  "drill_status": "SUCCESS"
}
EOF
chmod 600 "$EVIDENCE_FILE" 2>/dev/null || true

log "================================================================="
log "RESTORE DRILL COMPLETED SUCCESSFULLY: ${drill_id}"
log "Drill Evidence Record: ${EVIDENCE_FILE}"
log "Database Artifact:     ${backup_artifact} (${db_sha})"
log "Secrets Bundle:        ${secret_artifact} (${sec_sha})"
log "Integrity:             ${integrity_status}"
log "================================================================="

printf '%s\n' "$EVIDENCE_FILE"
exit 0
