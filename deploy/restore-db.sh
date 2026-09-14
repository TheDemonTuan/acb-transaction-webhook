#!/usr/bin/env bash
# Isolated decryption and integrity verification for age SQLite backups
# Fails closed on integrity/manifest errors; NEVER overwrites live database.
set -euo pipefail
umask 077

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deploy/lib.sh
source "$SCRIPT_DIR/lib.sh"

IDENTITY_FILE=""
BACKUP_FILE=""
MANIFEST_FILE=""
TARGET_DIR=""
OUTPUT_FILE=""

usage() {
  cat <<EOF
Usage: $0 --identity <private_key_file> --backup <gateway-*.db.age> [options]

Required:
  -i, --identity <path>     Path to age private identity recovery key
  -b, --backup <path>       Path to encrypted .db.age backup file

Optional:
  -m, --manifest <path>     Path to manifest-*.json (defaults to companion manifest)
  -t, --target-dir <path>   Directory to place restored database (default: isolated staging)
  -o, --output <path>       Explicit output file path for decrypted database
  -h, --help                Show this help message

SAFETY NOTICE: This tool will never overwrite the live production database automatically.
EOF
  exit 1
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    -i|--identity)
      IDENTITY_FILE="$2"; shift 2 ;;
    -b|--backup)
      BACKUP_FILE="$2"; shift 2 ;;
    -m|--manifest)
      MANIFEST_FILE="$2"; shift 2 ;;
    -t|--target-dir)
      TARGET_DIR="$2"; shift 2 ;;
    -o|--output)
      OUTPUT_FILE="$2"; shift 2 ;;
    -h|--help)
      usage ;;
    *)
      log_error "Unknown argument: $1"
      usage ;;
  esac
done

if [[ -z "$IDENTITY_FILE" || -z "$BACKUP_FILE" ]]; then
  log_error "Both --identity and --backup arguments are required."
  usage
fi

if [[ ! -f "$IDENTITY_FILE" ]]; then
  log_error "Age private identity file not found: '$IDENTITY_FILE'"
  exit 1
fi
check_secret_permissions "$IDENTITY_FILE"

if [[ ! -f "$BACKUP_FILE" ]]; then
  log_error "Encrypted backup file not found: '$BACKUP_FILE'"
  exit 1
fi

if [[ "$BACKUP_FILE" != *.age ]]; then
  log_error "Backup file must have .age extension: '$BACKUP_FILE'"
  exit 1
fi

AGE_BIN="age"
if ! command -v "$AGE_BIN" >/dev/null 2>&1; then
  if [[ -x "${HOME}/go/bin/age" ]]; then
    AGE_BIN="${HOME}/go/bin/age"
  elif [[ -x "/usr/local/bin/age" ]]; then
    AGE_BIN="/usr/local/bin/age"
  else
    log_error "age utility not found in PATH"
    exit 1
  fi
fi

# Locate companion manifest if not specified
if [[ -z "$MANIFEST_FILE" ]]; then
  candidate_manifest="$(dirname "$BACKUP_FILE")/manifest-$(basename "$BACKUP_FILE" .db.age | sed 's/^gateway-//').json"
  if [[ -f "$candidate_manifest" ]]; then
    MANIFEST_FILE="$candidate_manifest"
  fi
fi

# Refuse production live database paths
live_db_path="$SCRIPT_DIR/data/gateway.db"
if [[ "$OUTPUT_FILE" == "$live_db_path" || "$OUTPUT_FILE" == "/data/gateway.db" ]]; then
  log_error "RESTORE REFUSED: Output path points to live production database ($OUTPUT_FILE). Automatic overwrite is strictly forbidden."
  exit 1
fi

ts="$(date -u +'%Y%m%d%H%M%S')"
if [[ -z "$TARGET_DIR" ]]; then
  RESTORE_DIR="$(mktemp -d "${TMPDIR:-/tmp}/acb-restore.${ts}.XXXXXX")"
else
  mkdir -p "$TARGET_DIR"
  RESTORE_DIR="$TARGET_DIR"
fi
chmod 700 "$RESTORE_DIR" 2>/dev/null || true

if [[ -n "$OUTPUT_FILE" ]]; then
  restored_db="$OUTPUT_FILE"
  mkdir -p "$(dirname "$restored_db")"
else
  restored_db="$RESTORE_DIR/gateway-restored-${ts}.db"
fi

# Decrypt using age private identity
log_info "Decrypting backup file using recovery key..."
if ! "$AGE_BIN" -d -i "$IDENTITY_FILE" "$BACKUP_FILE" > "$restored_db"; then
  log_error "age decryption failed for '$BACKUP_FILE'"
  rm -f "$restored_db"
  exit 1
fi
chmod 600 "$restored_db" 2>/dev/null || true

if [[ ! -s "$restored_db" ]]; then
  log_error "Decrypted database file is missing or empty"
  rm -f "$restored_db"
  exit 1
fi

# Verify SQLite integrity
log_info "Verifying SQLite integrity of decrypted database..."
integrity="unverified"
migrations="unknown"
dbtool_cmd=""
if command -v dbtool >/dev/null 2>&1; then
  dbtool_cmd="dbtool"
elif [[ -n "${DBTOOL_BIN:-}" && -x "${DBTOOL_BIN}" ]]; then
  dbtool_cmd="${DBTOOL_BIN}"
elif [[ -x "$SCRIPT_DIR/../cmd/dbtool/dbtool" ]]; then
  dbtool_cmd="$SCRIPT_DIR/../cmd/dbtool/dbtool"
elif [[ -x "$SCRIPT_DIR/../cmd/dbtool/dbtool.exe" ]]; then
  dbtool_cmd="$SCRIPT_DIR/../cmd/dbtool/dbtool.exe"
fi

if command -v sqlite3 >/dev/null 2>&1; then
  integrity="$(sqlite3 "$restored_db" "PRAGMA integrity_check;" 2>/dev/null || echo "failed")"
  if [[ "$integrity" != "ok" ]]; then
    log_error "Integrity check FAILED on decrypted database: ${integrity}"
    rm -f "$restored_db"
    exit 1
  fi
  migrations="$(sqlite3 "$restored_db" "SELECT count(*) FROM schema_migrations;" 2>/dev/null || echo "0")"
elif [[ -n "$dbtool_cmd" ]]; then
  if ! "$dbtool_cmd" -path "$restored_db" -readonly -check >/dev/null 2>&1; then
    log_error "dbtool integrity check FAILED on decrypted database"
    rm -f "$restored_db"
    exit 1
  fi
  integrity="ok"
else
  log_error "No SQLite integrity verification tool available. Restore verification fails closed."
  rm -f "$restored_db"
  exit 1
fi

# Verify against manifest if present
if [[ -n "$MANIFEST_FILE" && -f "$MANIFEST_FILE" ]]; then
  log_info "Verifying backup against manifest: ${MANIFEST_FILE}"
  expected_enc_sha="$(grep -o '"sha256": *"[^"]*"' "$MANIFEST_FILE" | head -n1 | cut -d'"' -f4 || echo "")"
  if [[ -n "$expected_enc_sha" ]]; then
    actual_enc_sha="$(sha256sum "$BACKUP_FILE" | cut -d' ' -f1)"
    if [[ "$expected_enc_sha" != "$actual_enc_sha" ]]; then
      log_error "Manifest encrypted sha256 mismatch! Expected: $expected_enc_sha, Actual: $actual_enc_sha"
      rm -f "$restored_db"
      exit 1
    fi
    log_info "Manifest ciphertext SHA-256 match verified: ${actual_enc_sha}"
  fi
fi

log_info "================================================================="
log_info "SUCCESS: Database restored and verified successfully."
log_info "Restored Database: ${restored_db}"
log_info "SQLite Integrity:  ${integrity}"
log_info "Schema Migrations: ${migrations}"
log_info "SAFETY: Live production database was NOT overwritten."
log_info "Follow docs/runbooks/RESTORE_RUNBOOK.md for production disaster promotion."
log_info "================================================================="

printf '%s\n' "$restored_db"
exit 0
