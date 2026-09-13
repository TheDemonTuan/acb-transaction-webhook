#!/usr/bin/env bash
# Online consistent SQLite snapshot with age ciphertext-only durable backup
# Never writes or retains plaintext snapshots in durable backup directories.
set -euo pipefail
umask 077

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deploy/lib.sh
source "$SCRIPT_DIR/lib.sh"

BACKUP_DIR="${BACKUP_DIR:-$SCRIPT_DIR/data/backups}"
BACKUP_AGE_RECIPIENT="${BACKUP_AGE_RECIPIENT:-}"
OFFHOST_BACKUP_HOOK="${OFFHOST_BACKUP_HOOK:-}"
REQUIRE_OFFHOST_BACKUP="${REQUIRE_OFFHOST_BACKUP:-0}"
ACTIVE_SLOT="${ACTIVE_SLOT:-$(get_active_slot 2>/dev/null || echo "monolith")}"
RELEASE_COMMIT="${RELEASE_COMMIT:-$(git rev-parse HEAD 2>/dev/null || echo "unknown")}"
DBTOOL_IMAGE="${DBTOOL_IMAGE_REF:-ghcr.io/thedemontuan/acb-transaction-webhook-dbtool:latest}"

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

mkdir -p "$BACKUP_DIR"
chmod 700 "$BACKUP_DIR" 2>/dev/null || true

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
  docker run --rm --user 1000:1000 \
    -e APP_ENV=production -e DATA_DIR=/data -e DATABASE_PATH=/data/gateway.db \
    -v "${DATA_VOLUME_NAME}:/data:rw" \
    -v "${STAGING_DIR}:/backup:rw" \
    "$DBTOOL_IMAGE" -path /data/gateway.db -backup-to "/backup/gateway-staging.db"
elif [[ -f "$DATABASE_PATH" ]]; then
  if command -v sqlite3 >/dev/null 2>&1; then
    sqlite3 "$DATABASE_PATH" "PRAGMA wal_checkpoint(TRUNCATE);" 2>/dev/null || true
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
integrity="ok"
migrations="0"
if command -v sqlite3 >/dev/null 2>&1; then
  integrity="$(sqlite3 "$staging_raw" "PRAGMA integrity_check;" 2>/dev/null || echo "failed")"
  if [[ "$integrity" != "ok" ]]; then
    log_error "SQLite integrity check failed on staging snapshot: ${integrity}"
    exit 1
  fi
  migrations="$(sqlite3 "$staging_raw" "SELECT count(*) FROM schema_migrations;" 2>/dev/null || echo "0")"
fi
log_info "Staging snapshot integrity verified (${integrity}, schema migrations: ${migrations})"

raw_sha256="$(sha256sum "$staging_raw" 2>/dev/null | cut -d' ' -f1 || echo "unknown")"
raw_size="$(wc -c < "$staging_raw" 2>/dev/null | tr -d ' ' || echo "0")"

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
mv "$staging_enc" "$durable_enc"
mv "$staging_manifest" "$durable_manifest"

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
