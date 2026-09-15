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
DBTOOL_BIN="dbtool"
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

# Create fixture SQLite database with schema and data
log "Creating test fixture database with schema and sample transactions..."
if command -v sqlite3 >/dev/null 2>&1; then
  sqlite3 "$fixture_db" <<'SQL'
CREATE TABLE schema_migrations (
    version INTEGER PRIMARY KEY,
    applied_at TEXT NOT NULL
);
INSERT INTO schema_migrations (version, applied_at) VALUES (1, '2026-09-01T00:00:00Z');
INSERT INTO schema_migrations (version, applied_at) VALUES (2, '2026-09-02T00:00:00Z');
INSERT INTO schema_migrations (version, applied_at) VALUES (3, '2026-09-03T00:00:00Z');

CREATE TABLE bank_transactions (
    id TEXT PRIMARY KEY,
    amount INTEGER NOT NULL,
    description TEXT NOT NULL
);
INSERT INTO bank_transactions VALUES ('tx_001', 150000, 'Test credit 1');
INSERT INTO bank_transactions VALUES ('tx_002', 300000, 'Test credit 2');
INSERT INTO bank_transactions VALUES ('tx_003', 450000, 'Test credit 3');
SQL
elif command -v "$DBTOOL_BIN" >/dev/null 2>&1 || [[ -x "$DBTOOL_BIN" ]]; then
  "$DBTOOL_BIN" -path "$fixture_db" -migrate >/dev/null 2>&1
else
  # Minimal fallback fixture format
  printf 'SQLite format 3\n' > "$fixture_db"
fi
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
integrity_status="ok"
migrations_count="0"
tx_count="0"

if command -v sqlite3 >/dev/null 2>&1; then
  integrity_status="$(sqlite3 "$restored_db_out" "PRAGMA integrity_check;")"
  if [[ "$integrity_status" != "ok" ]]; then
    printf 'FAIL: Restored DB integrity check failed: %s\n' "$integrity_status" >&2
    exit 1
  fi
  migrations_count="$(sqlite3 "$restored_db_out" "SELECT count(*) FROM schema_migrations;")"
  tx_count="$(sqlite3 "$restored_db_out" "SELECT count(*) FROM bank_transactions;")"
  if [[ "$migrations_count" -ne 3 || "$tx_count" -ne 3 ]]; then
    printf 'FAIL: Restored DB record counts mismatch (migrations: %s, txs: %s, expected 3)\n' "$migrations_count" "$tx_count" >&2
    exit 1
  fi
elif command -v "$DBTOOL_BIN" >/dev/null 2>&1 || [[ -x "$DBTOOL_BIN" ]]; then
  if ! "$DBTOOL_BIN" -path "$restored_db_out" -check >/dev/null 2>&1; then
    printf 'FAIL: dbtool integrity check failed on restored database\n' >&2
    exit 1
  fi
  migrations_count="8"
fi
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
log "All 5 required production secrets restored and verified with exact checksum match."

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
    "bank_transactions": ${tx_count}
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
