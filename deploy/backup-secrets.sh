#!/usr/bin/env bash
# Separate encrypted secret recovery backup
# Streams selected secret files directly into age without writing unencrypted secrets to disk.
set -euo pipefail
umask 077

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deploy/lib.sh
source "$SCRIPT_DIR/lib.sh"

BACKUP_DIR="${BACKUP_DIR:-$SCRIPT_DIR/data/backups}"
BACKUP_AGE_RECIPIENT="${BACKUP_AGE_RECIPIENT:-}"
OFFHOST_BACKUP_HOOK="${OFFHOST_BACKUP_HOOK:-}"
RELEASE_COMMIT="${RELEASE_COMMIT:-$(git rev-parse HEAD 2>/dev/null || echo "unknown")}"

# 1. Preflight tool, recipient, and secrets validation
if [[ -z "$BACKUP_AGE_RECIPIENT" ]]; then
  log_error "BACKUP_AGE_RECIPIENT is not configured. Secret backup encryption fails closed."
  exit 1
fi

AGE_BIN="age"
if ! command -v "$AGE_BIN" >/dev/null 2>&1; then
  if [[ -x "${HOME}/go/bin/age" ]]; then
    AGE_BIN="${HOME}/go/bin/age"
  elif [[ -x "/usr/local/bin/age" ]]; then
    AGE_BIN="/usr/local/bin/age"
  else
    log_error "age encryption utility is missing. Secret backup fails closed."
    exit 1
  fi
fi

check_required_secrets

mkdir -p "$BACKUP_DIR"
chmod 700 "$BACKUP_DIR" 2>/dev/null || true

ts="$(date -u +'%Y%m%d%H%M%S')"
STAGING_DIR="$(mktemp -d "${BACKUP_DIR}/.staging.secrets.${ts}.XXXXXX")"
chmod 700 "$STAGING_DIR" 2>/dev/null || true

cleanup() {
  local exit_code=$?
  if [[ -d "$STAGING_DIR" ]]; then
    rm -rf "$STAGING_DIR"
  fi
  exit "$exit_code"
}
trap cleanup EXIT HUP INT TERM

staging_enc="$STAGING_DIR/secrets-${ts}.tar.age"
staging_manifest="$STAGING_DIR/manifest-secrets-${ts}.json"

log_info "Streaming secrets directly into age encrypted tar archive..."
# Tar the secrets directory directly through pipe to age (Zero unencrypted secrets on backup disk)
tar -C "$SECRETS_DIR" -cf - . | "$AGE_BIN" -r "$BACKUP_AGE_RECIPIENT" -o "$staging_enc"

if [[ ! -s "$staging_enc" ]]; then
  log_error "Encrypted secret bundle was not created or is empty"
  exit 1
fi
chmod 600 "$staging_enc" 2>/dev/null || true

enc_sha256="$(sha256sum "$staging_enc" 2>/dev/null | cut -d' ' -f1 || echo "unknown")"
enc_size="$(wc -c < "$staging_enc" 2>/dev/null | tr -d ' ' || echo "0")"
recip_fp="$(printf '%s' "$BACKUP_AGE_RECIPIENT" | sha256sum 2>/dev/null | cut -d' ' -f1 || echo "unknown")"

# Secret manifest list (file names and individual hashes, NO secret values)
sec_entries=()
for f in "$SECRETS_DIR"/*; do
  if [[ -f "$f" ]]; then
    bname="$(basename "$f")"
    fsha="$(sha256sum "$f" 2>/dev/null | cut -d' ' -f1 || echo "")"
    sec_entries+=("{\"name\":\"$bname\",\"sha256\":\"$fsha\"}")
  fi
done
joined_entries="$(IFS=,; echo "${sec_entries[*]}")"

cat <<EOF > "$staging_manifest"
{
  "timestamp": "${ts}",
  "release_commit": "${RELEASE_COMMIT}",
  "recipient_fingerprint": "${recip_fp}",
  "secrets_bundle": {
    "file": "secrets-${ts}.tar.age",
    "sha256": "${enc_sha256}",
    "size_bytes": ${enc_size}
  },
  "secret_files": [${joined_entries}]
}
EOF
chmod 600 "$staging_manifest" 2>/dev/null || true

durable_enc="$BACKUP_DIR/secrets-${ts}.tar.age"
durable_manifest="$BACKUP_DIR/manifest-secrets-${ts}.json"
mv "$staging_enc" "$durable_enc"
mv "$staging_manifest" "$durable_manifest"

log_info "Published encrypted secret recovery bundle: ${durable_enc}"
log_info "Published secret recovery manifest: ${durable_manifest}"

if [[ -n "$OFFHOST_BACKUP_HOOK" && -x "$OFFHOST_BACKUP_HOOK" ]]; then
  log_info "Executing off-host hook for secret backup..."
  "$OFFHOST_BACKUP_HOOK" "$durable_enc" "$durable_manifest"
fi

printf '%s\n' "$durable_enc"
exit 0
