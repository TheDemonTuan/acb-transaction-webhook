#!/usr/bin/env bash
# Isolated decryption and integrity verification for age SQLite backups
# Fails closed on integrity/manifest errors; NEVER overwrites live database.
set -euo pipefail
umask 077

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deploy/simple-lib.sh
source "$SCRIPT_DIR/simple-lib.sh"

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

# Resolve aliases/symlinks before admitting any output; this tool is isolated only.
python3 - "$SCRIPT_DIR" "${DEPLOY_PATH:-/opt/bank-event-gateway}" "$OUTPUT_FILE" "$TARGET_DIR" <<'PY'
import pathlib,sys
script,root,output,target=sys.argv[1:]
live=[pathlib.Path(p).resolve() for p in (script+'/data',root+'/data','/data')]
for value in (output,target):
    if not value: continue
    path=pathlib.Path(value).resolve()
    if any(path==base or base in path.parents for base in live):
        raise SystemExit('RESTORE REFUSED: isolated output required; live database promotion forbidden')
if output and pathlib.Path(output).exists():
    raise SystemExit('RESTORE REFUSED: output already exists')
PY

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
restore_verified=0
trap 'if [[ "$restore_verified" != 1 ]]; then rm -f -- "$restored_db"; fi' EXIT

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
  python3 - "$MANIFEST_FILE" "$BACKUP_FILE" "$restored_db" <<'PY'
import hashlib,json,sys
manifest,encrypted,raw=sys.argv[1:]
data=json.load(open(manifest))
for section,path in (('sqlite_backup',encrypted),('raw_sqlite',raw)):
    expected=data[section]['sha256']
    with open(path,'rb') as stream: actual=hashlib.file_digest(stream,'sha256').hexdigest()
    if actual!=expected: raise SystemExit('Manifest '+section+' sha256 mismatch')
PY
fi
restore_verified=1

log_info "================================================================="
log_info "SUCCESS: Database restored and verified successfully."
log_info "Restored Database: ${restored_db}"
log_info "SQLite Integrity:  ${integrity}"
log_info "Schema Migrations: ${migrations}"
log_info "SAFETY: Live production database was NOT overwritten."
log_info "SAFETY: Promote only via the explicit cutover policy; an issued payOS order forbids restoring a legacy database."
log_info "================================================================="

printf '%s\n' "$restored_db"
exit 0
