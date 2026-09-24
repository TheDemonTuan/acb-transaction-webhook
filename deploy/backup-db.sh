#!/usr/bin/env bash
# Online SQLite snapshot: plaintext owner-only for deployment, age-encrypted for operators.
set -euo pipefail
umask 077

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deploy/simple-lib.sh
source "$SCRIPT_DIR/simple-lib.sh"

BACKUP_DIR="${BACKUP_DIR:-${DEPLOY_PATH:-$SCRIPT_DIR}/data/backups}"
DATA_VOLUME_NAME="${DATA_VOLUME_NAME:-bank-event-gateway_gateway_data}"
BACKUP_AGE_RECIPIENT="${BACKUP_AGE_RECIPIENT:-}"
OFFHOST_BACKUP_HOOK="${OFFHOST_BACKUP_HOOK:-}"
REQUIRE_OFFHOST_BACKUP="${REQUIRE_OFFHOST_BACKUP:-0}"
ACTIVE_SLOT="${ACTIVE_SLOT:-unknown}"
RELEASE_COMMIT="${RELEASE_COMMIT:-${RELEASE_SHA:-unknown}}"
DBTOOL_IMAGE="${DBTOOL_IMAGE_REF:-}"

if [[ "${1:-}" == --snapshot && $# == 1 ]]; then
  [[ "$RELEASE_COMMIT" =~ ^[0-9a-f]{40}$ ]] || { log_error 'RELEASE_COMMIT must be a lowercase 40-character SHA'; exit 1; }
  [[ "$DATA_VOLUME_NAME" == bank-event-gateway_gateway_data ]] || { log_error 'Snapshot requires bank-event-gateway_gateway_data volume'; exit 1; }
  validate_digest "$DBTOOL_IMAGE" dbtool
  [[ "$BACKUP_DIR" == /* && ! -L "$BACKUP_DIR" ]] || { log_error 'BACKUP_DIR must be an existing absolute, non-symlink directory'; exit 1; }
  [[ -d "$BACKUP_DIR" && "$(stat -c '%a:%u:%g' "$BACKUP_DIR")" == '700:1000:1000' ]] || { log_error "BACKUP_DIR must exist with mode 0700 and owner 1000:1000: $BACKUP_DIR"; exit 1; }
  command -v docker >/dev/null && command -v sqlite3 >/dev/null && command -v python3 >/dev/null || { log_error 'docker, sqlite3 and python3 are required for snapshot'; exit 1; }
  docker volume inspect "$DATA_VOLUME_NAME" >/dev/null || { log_error "Missing data volume: $DATA_VOLUME_NAME"; exit 1; }
  docker image inspect "$DBTOOL_IMAGE" >/dev/null || { log_error "Missing immutable dbtool image: $DBTOOL_IMAGE"; exit 1; }
  ts="$(date -u +'%Y%m%dT%H%M%SZ')"
  snapshot_dir="$(mktemp -d "$BACKUP_DIR/${ts}-${RELEASE_COMMIT}-XXXXXXXX")"
  chmod 700 "$snapshot_dir"
  snapshot_ok=0
  snapshot_cleanup() { if (( ! snapshot_ok )); then rm -rf -- "$snapshot_dir"; fi; }
  trap snapshot_cleanup EXIT
  [[ "$(stat -c '%a:%u:%g' "$snapshot_dir")" == '700:1000:1000' ]] || { log_error 'Snapshot directory must be mode 0700 owner 1000:1000'; exit 1; }
  trap 'exit 130' INT
  trap 'exit 143' TERM
  docker run --rm --network none --user 1000:1000 \
    -v "${DATA_VOLUME_NAME}:/data:rw" -v "${snapshot_dir}:/backup:rw" \
    "$DBTOOL_IMAGE" -path /data/gateway.db -backup-to /backup/gateway.db >&2
  snapshot_file="$snapshot_dir/gateway.db"
  [[ -s "$snapshot_file" && ! -L "$snapshot_file" ]] || { log_error 'dbtool produced no SQLite snapshot'; exit 1; }
  chmod 600 "$snapshot_file"
  [[ "$(stat -c '%a:%u:%g' "$snapshot_file")" == '600:1000:1000' ]] || { log_error 'Snapshot owner or mode incorrect'; exit 1; }
  integrity="$(sqlite3 "$snapshot_file" 'PRAGMA integrity_check;')"
  [[ "$integrity" == ok ]] || { log_error "Snapshot integrity check failed: $integrity"; exit 1; }
  schema="$(sqlite3 "$snapshot_file" 'SELECT max(version) FROM schema_migrations;')"
  [[ "$schema" =~ ^[0-9]+$ ]] || { log_error 'Snapshot schema query returned no valid version'; exit 1; }
  digest="$(sha256sum "$snapshot_file")"
  digest="${digest%% *}"
  [[ "$digest" =~ ^[0-9a-f]{64}$ ]] || { log_error 'Snapshot SHA256 calculation failed'; exit 1; }
  receipt="$snapshot_dir/receipt.json"
  python3 - "$receipt" "$RELEASE_COMMIT" "$schema" "$digest" "$snapshot_file" "$ts" <<'PY'
import json
import os
import sys

receipt, sha, schema, digest, path, timestamp = sys.argv[1:]
with open(receipt, 'x', encoding='utf-8') as output:
    json.dump(dict(sha=sha, schema_version=int(schema), sha256=digest,
                   path=path, created_at=timestamp), output, separators=(',', ':'))
    output.write('\n')
    output.flush()
    os.fsync(output.fileno())
PY
  chmod 600 "$receipt"
  [[ "$(stat -c '%a:%u:%g' "$receipt")" == '600:1000:1000' ]] || { log_error 'Snapshot receipt owner or mode incorrect'; exit 1; }
  python3 - "$snapshot_dir" <<'PY'
import os
import sys
fd = os.open(sys.argv[1], os.O_RDONLY | os.O_DIRECTORY)
try:
    os.fsync(fd)
finally:
    os.close(fd)
PY
  snapshot_ok=1
  printf '%s\n' "$receipt"
  exit 0
fi
[[ $# == 0 ]] || { log_error 'Usage: backup-db.sh [--snapshot]'; exit 2; }

# 1. Preflight tool and recipient validation (Fail-closed)
if [[ -z "$BACKUP_AGE_RECIPIENT" ]]; then
  log_error "BACKUP_AGE_RECIPIENT is not configured. Backup encryption fails closed."
  exit 1
fi

AGE_BIN="age"
if ! command -v "$AGE_BIN" >/dev/null 2>&1; then
  # Fallback check for user go bin if not in standard PATH
  if [[ -x "${HOME}/go/bin/age" ]]; then
    AGE_BIN="${HOME}/go/bin/age"
  elif [[ -x "/usr/local/bin/age" ]]; then
    AGE_BIN="/usr/local/bin/age"
  else
    log_error "age encryption utility is missing. Backup encryption fails closed."
    exit 1
  fi
fi

if [[ "$REQUIRE_OFFHOST_BACKUP" == "1" ]]; then
  if [[ -z "$OFFHOST_BACKUP_HOOK" || ! -x "$OFFHOST_BACKUP_HOOK" ]]; then
    log_error "REQUIRE_OFFHOST_BACKUP is enabled but OFFHOST_BACKUP_HOOK is missing or not executable: '${OFFHOST_BACKUP_HOOK}'"
    exit 1
  fi
fi

[[ -d "$BACKUP_DIR" && ! -L "$BACKUP_DIR" && "$(stat -c '%a:%u:%g' "$BACKUP_DIR")" == '700:1000:1000' ]] || { log_error "BACKUP_DIR must exist mode 0700 owner 1000:1000: $BACKUP_DIR"; exit 1; }

ts="$(date -u +'%Y%m%d%H%M%S')"
STAGING_DIR="$(mktemp -d "${BACKUP_DIR}/.staging.${ts}.XXXXXX")"
chmod 700 "$STAGING_DIR" 2>/dev/null || true

# Signal trap ensures plaintext staging files and orphans are purged on failure
cleanup() {
  local exit_code=$?
  if [[ -d "$STAGING_DIR" ]]; then
    rm -rf "$STAGING_DIR"
  fi
  exit "$exit_code"
}
trap cleanup EXIT HUP INT TERM

log_info "Creating online consistent SQLite snapshot..."
staging_raw="$STAGING_DIR/gateway-staging.db"
staging_enc="$STAGING_DIR/gateway-${ts}.db.age"
staging_manifest="$STAGING_DIR/manifest-${ts}.json"

DATABASE_PATH="${DATABASE_PATH:-${DATA_DIR:-$SCRIPT_DIR/data}/gateway.db}"

DBTOOL_BIN="dbtool"
if ! command -v "$DBTOOL_BIN" >/dev/null 2>&1; then
  if [[ -x "${HOME}/go/bin/dbtool" ]]; then
    DBTOOL_BIN="${HOME}/go/bin/dbtool"
  elif [[ -x "/usr/local/bin/dbtool" ]]; then
    DBTOOL_BIN="/usr/local/bin/dbtool"
  fi
fi

if docker volume inspect "$DATA_VOLUME_NAME" >/dev/null 2>&1; then
  if [[ -z "$DBTOOL_IMAGE" ]]; then
    log_error "DBTOOL_IMAGE_REF immutable digest is required for volume backup."
    exit 1
  fi
  validate_digest "$DBTOOL_IMAGE" "dbtool"
  docker run --rm --network none --user 1000:1000 \
    -e APP_ENV=production -e DATA_DIR=/data -e DATABASE_PATH=/data/gateway.db \
    -v "${DATA_VOLUME_NAME}:/data:rw" \
    -v "${STAGING_DIR}:/backup:rw" \
    "$DBTOOL_IMAGE" -path /data/gateway.db -backup-to "/backup/gateway-staging.db"
elif [[ -f "$DATABASE_PATH" ]]; then
  if command -v sqlite3 >/dev/null 2>&1; then
    sqlite3 "$DATABASE_PATH" ".backup '$staging_raw'"
  elif command -v "$DBTOOL_BIN" >/dev/null 2>&1 || [[ -x "$DBTOOL_BIN" ]]; then
    "$DBTOOL_BIN" -path "$DATABASE_PATH" -backup-to "$staging_raw"
  else
    log_error "Neither docker volume, sqlite3 CLI, nor dbtool binary available to create SQLite snapshot"
    exit 1
  fi
fi

if [[ ! -s "$staging_raw" ]]; then
  log_error "Online SQLite backup failed: raw snapshot file is missing or empty"
  exit 1
fi
chmod 600 "$staging_raw" 2>/dev/null || true

# 2. SQLite Integrity and Schema Verification
log_info "Verifying SQLite integrity of staging snapshot..."
integrity=""
migrations="0"
if ! command -v sqlite3 >/dev/null 2>&1; then
  log_error "sqlite3 is required to verify a backup before publication."
  exit 1
fi
integrity="$(sqlite3 "$staging_raw" "PRAGMA integrity_check;" 2>/dev/null || echo "failed")"
if [[ "$integrity" != "ok" ]]; then
  log_error "SQLite integrity check failed on staging snapshot: ${integrity}"
  exit 1
fi
migrations="$(sqlite3 "$staging_raw" "SELECT max(version) FROM schema_migrations;")"
[[ "$migrations" =~ ^[0-9]+$ ]] || { log_error "Invalid schema version in backup snapshot."; exit 1; }
log_info "Staging snapshot integrity verified (${integrity}, schema migrations: ${migrations})"

raw_sha256="$(sha256sum "$staging_raw" | cut -d' ' -f1)"
raw_size="$(wc -c < "$staging_raw" | tr -d ' ')"
# 3. Encrypt snapshot with age recipient
log_info "Encrypting snapshot with age recipient..."
"$AGE_BIN" -r "$BACKUP_AGE_RECIPIENT" -o "$staging_enc" "$staging_raw"

if [[ ! -s "$staging_enc" ]]; then
  log_error "age encryption produced missing or empty ciphertext artifact"
  exit 1
fi
chmod 600 "$staging_enc" 2>/dev/null || true

# Immediately wipe raw plaintext staging snapshot
rm -f "$staging_raw"

enc_sha256="$(sha256sum "$staging_enc" 2>/dev/null | cut -d' ' -f1 || echo "unknown")"
enc_size="$(wc -c < "$staging_enc" 2>/dev/null | tr -d ' ' || echo "0")"
recip_fp="$(printf '%s' "$BACKUP_AGE_RECIPIENT" | sha256sum 2>/dev/null | cut -d' ' -f1 || echo "unknown")"

# 4. Generate JSON manifest
cat <<EOF > "$staging_manifest"
{
  "timestamp": "${ts}",
  "active_slot": "${ACTIVE_SLOT}",
  "release_commit": "${RELEASE_COMMIT}",
  "schema_version": ${migrations},
  "recipient_fingerprint": "${recip_fp}",
  "sqlite_backup": {
    "file": "gateway-${ts}.db.age",
    "sha256": "${enc_sha256}",
    "size_bytes": ${enc_size}
  },
  "raw_sqlite": {
    "sha256": "${raw_sha256}",
    "size_bytes": ${raw_size}
  },
  "raw_integrity": "${integrity}"
}
EOF
chmod 600 "$staging_manifest" 2>/dev/null || true

# 5. Atomic publish to durable backup directory
durable_enc="$BACKUP_DIR/gateway-${ts}.db.age"
durable_manifest="$BACKUP_DIR/manifest-${ts}.json"
atomic_write_file "$durable_enc" 600 < "$staging_enc"
atomic_write_file "$durable_manifest" 600 < "$staging_manifest"
rm -f "$staging_enc" "$staging_manifest"

log_info "Published encrypted backup artifact: ${durable_enc}"
log_info "Published backup manifest: ${durable_manifest}"

# 6. Optional or required off-host backup hook
if [[ -n "$OFFHOST_BACKUP_HOOK" ]]; then
  if [[ -x "$OFFHOST_BACKUP_HOOK" ]]; then
    log_info "Executing off-host backup hook: ${OFFHOST_BACKUP_HOOK}..."
    if ! "$OFFHOST_BACKUP_HOOK" "$durable_enc" "$durable_manifest"; then
      log_error "Off-host backup hook execution failed."
      exit 1
    fi
    log_info "Off-host backup hook completed successfully."
  else
    if [[ "$REQUIRE_OFFHOST_BACKUP" == "1" ]]; then
      log_error "Configured OFFHOST_BACKUP_HOOK is not executable: ${OFFHOST_BACKUP_HOOK}"
      exit 1
    else
      log_warn "OFFHOST_BACKUP_HOOK is set but not executable. Skipping off-host transfer."
    fi
  fi
fi

# Print final artifact path for caller scripts
printf '%s\n' "$durable_enc"
exit 0
