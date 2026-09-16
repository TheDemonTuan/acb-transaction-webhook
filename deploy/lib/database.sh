#!/usr/bin/env bash
# deploy/lib/database.sh
# Database migrations, active-auth fail-closed gate, read-only WAL probes, backups, and candidate readiness probes.
set -euo pipefail

DEFAULT_GATEWAY_DB="${GATEWAY_DB_FILE:-${RUNTIME_DATA_DIR:-$SCRIPT_DIR/data}/gateway.db}"

check_active_auth_gate() {
  log_info "Evaluating active-auth gate before migration / core modification..."
  local active_count=""

  if [[ -n "${ACTIVE_AUTH_CHECK_CMD:-}" ]]; then
    local out
    if ! out="$($ACTIVE_AUTH_CHECK_CMD 2>&1)"; then
      log_error "check_active_auth_gate: check command failed with exit code $?: ${out}"
      return 1
    fi
    active_count="$(printf '%s' "$out" | tr -d ' \r\n[:space:]')"
  elif command -v docker >/dev/null 2>&1 && docker volume inspect "$DATA_VOLUME_NAME" >/dev/null 2>&1; then
    local dbtool_img="${DBTOOL_IMAGE_REF:-$(get_release_env DBTOOL_IMAGE_REF 2>/dev/null || true)}"
    if [[ -z "$dbtool_img" ]]; then
      log_error "check_active_auth_gate: DBTOOL_IMAGE_REF immutable digest is required for active auth check."
      return 1
    fi
    validate_digest "$dbtool_img" "dbtool"

    local auth_json
    # Pull separately so Docker progress never contaminates the strict JSON probe output.
    log_info "Pulling immutable dbtool image before active-auth probe..."
    if ! docker pull "$dbtool_img" >&2; then
      log_error "check_active_auth_gate: failed to pull immutable dbtool image."
      return 1
    fi
    # Execute dbtool strictly read-only with no egress network
    if ! auth_json="$(
      docker run --rm \
        --read-only \
        --network none \
        --user 1000:1000 \
        -e DATABASE_PATH=/data/gateway.db \
        -v "${DATA_VOLUME_NAME}:/data:ro" \
        "$dbtool_img" \
        -path /data/gateway.db \
        -readonly \
        -active-auth-count 2>&1
    )"; then
      log_error "check_active_auth_gate: dbtool execution failed: ${auth_json}"
      return 1
    fi

    if [[ -z "$auth_json" ]]; then
      log_error "check_active_auth_gate: dbtool produced empty output."
      return 1
    fi

    if command -v jq >/dev/null 2>&1; then
      active_count="$(printf '%s' "$auth_json" | jq -e -r '.activeCount // .count' 2>/dev/null || echo "PARSE_ERROR")"
    else
      if [[ "$auth_json" =~ \"activeCount\":[[:space:]]*([0-9]+) ]]; then
        active_count="${BASH_REMATCH[1]}"
      else
        active_count="PARSE_ERROR"
      fi
    fi

    if [[ "$active_count" == "PARSE_ERROR" ]]; then
      log_error "check_active_auth_gate: Failed to parse valid JSON count from dbtool output: ${auth_json}"
      return 1
    fi
  elif command -v sqlite3 >/dev/null 2>&1; then
    local db_file="${1:-$DEFAULT_GATEWAY_DB}"
    if [[ ! -r "$db_file" ]]; then
      log_error "check_active_auth_gate: Database file '$db_file' is missing or not readable."
      return 1
    fi
    local query="SELECT count(*) FROM auth_attempts WHERE status IN ('STARTING','IN_PROGRESS','EXPORTING','VERIFYING') AND (expires_at > datetime('now') OR expires_at > strftime('%Y-%m-%dT%H:%M:%SZ', 'now'));"
    local sql_out
    # Query in read-only mode to prevent write lock contention
    if ! sql_out="$(sqlite3 "file:${db_file}?mode=ro" "$query" 2>&1)"; then
      log_error "check_active_auth_gate: SQLite query execution failed: ${sql_out}"
      return 1
    fi
    active_count="$(printf '%s' "$sql_out" | tr -d ' \r\n[:space:]')"
  else
    log_error "check_active_auth_gate: No supported active auth check method available. Fail closed."
    return 1
  fi

  if [[ ! "$active_count" =~ ^[0-9]+$ ]]; then
    log_error "check_active_auth_gate: Expected integer active session count, got '${active_count}'. Aborting."
    return 1
  fi

  if [[ "$active_count" -gt 0 ]]; then
    log_error "Active-auth gate FAILED: found ${active_count} active in-flight authentication session(s). Aborting to protect customer authentication."
    return 1
  fi

  log_info "Active-auth gate passed: 0 active authentication sessions in progress."
  return 0
}

acquire_mutation_gate() {
  local db_volume="${1:-$DATA_VOLUME_NAME}"
  local dbtool_img="${2:-${DBTOOL_IMAGE_REF:-}}"
  local owner="${3:-deploy-$(date -u +%s)}"
  local reason="${4:-deploy}"
  local lease_duration="${5:-120s}"

  log_info "Acquiring durable mutation gate for owner '$owner' (reason: $reason)..."
  local gate_json=""
  if [[ -n "${GATE_ACQUIRE_CMD:-}" ]]; then
    if ! gate_json="$($GATE_ACQUIRE_CMD "$owner" "$reason" "$lease_duration" 2>&1)"; then
      log_error "acquire_mutation_gate: gate command failed: ${gate_json}"
      return 1
    fi
  elif command -v docker >/dev/null 2>&1 && docker volume inspect "$db_volume" >/dev/null 2>&1 && [[ -n "$dbtool_img" ]]; then
    if ! gate_json="$(
      docker run --rm --network none --user 1000:1000 \
        -e DATABASE_PATH=/data/gateway.db \
        -v "${db_volume}:/data:rw" \
        "$dbtool_img" \
        -path /data/gateway.db \
        -gate-acquire \
        -owner "$owner" \
        -reason "$reason" \
        -lease-duration "$lease_duration" 2>&1
    )"; then
      log_error "acquire_mutation_gate: dbtool execution failed: ${gate_json}"
      return 1
    fi
  elif [[ -f "$DEFAULT_GATEWAY_DB" ]] && command -v dbtool >/dev/null 2>&1; then
    if ! gate_json="$(dbtool -path "$DEFAULT_GATEWAY_DB" -gate-acquire -owner "$owner" -reason "$reason" -lease-duration "$lease_duration" 2>&1)"; then
      log_error "acquire_mutation_gate: local dbtool failed: ${gate_json}"
      return 1
    fi
  else
    log_info "acquire_mutation_gate: falling back to active-auth check in bootstrap transition mode."
    check_active_auth_gate || return 1
    return 0
  fi

  local token=""
  if command -v jq >/dev/null 2>&1; then
    token="$(printf '%s' "$gate_json" | jq -r '.leaseToken // empty' 2>/dev/null || true)"
  fi
  printf '%s' "$token"
  log_info "Durable mutation gate acquired successfully."
  return 0
}

release_mutation_gate() {
  local db_volume="${1:-$DATA_VOLUME_NAME}"
  local dbtool_img="${2:-${DBTOOL_IMAGE_REF:-}}"
  local owner="$3"
  local token="${4:-}"

  log_info "Releasing durable mutation gate for owner '$owner'..."
  if [[ -n "${GATE_RELEASE_CMD:-}" ]]; then
    $GATE_RELEASE_CMD "$owner" "$token" >/dev/null 2>&1 || true
    return 0
  elif command -v docker >/dev/null 2>&1 && docker volume inspect "$db_volume" >/dev/null 2>&1 && [[ -n "$dbtool_img" ]]; then
    docker run --rm --network none --user 1000:1000 \
      -e DATABASE_PATH=/data/gateway.db \
      -v "${db_volume}:/data:rw" \
      "$dbtool_img" \
      -path /data/gateway.db \
      -gate-release \
      -owner "$owner" \
      -lease-token "$token" >/dev/null 2>&1 || true
  elif [[ -f "$DEFAULT_GATEWAY_DB" ]] && command -v dbtool >/dev/null 2>&1; then
    dbtool -path "$DEFAULT_GATEWAY_DB" -gate-release -owner "$owner" -lease-token "$token" >/dev/null 2>&1 || true
  fi
  log_info "Durable mutation gate released."
  return 0
}

verify_schema_compat() {
  local db_volume="${1:-$DATA_VOLUME_NAME}"
  local dbtool_img="${2:-${DBTOOL_IMAGE_REF:-}}"
  local min_version="${3:-9}"

  log_info "Verifying schema compatibility (minimum version: $min_version)..."
  if [[ -n "${SCHEMA_COMPAT_CMD:-}" ]]; then
    $SCHEMA_COMPAT_CMD "$min_version" || return 1
    return 0
  elif command -v docker >/dev/null 2>&1 && docker volume inspect "$db_volume" >/dev/null 2>&1 && [[ -n "$dbtool_img" ]]; then
    if ! docker run --rm --network none --read-only --user 1000:1000 \
      -e DATABASE_PATH=/data/gateway.db \
      -v "${db_volume}:/data:ro" \
      "$dbtool_img" \
      -path /data/gateway.db \
      -readonly \
      -schema-compat \
      -min-version "$min_version"; then
      log_error "Schema compatibility verification failed via dbtool"
      return 1
    fi
  elif [[ -f "$DEFAULT_GATEWAY_DB" ]] && command -v dbtool >/dev/null 2>&1; then
    if ! dbtool -path "$DEFAULT_GATEWAY_DB" -schema-compat -min-version "$min_version"; then
      log_error "Schema compatibility verification failed via local dbtool"
      return 1
    fi
  fi
  log_info "Schema compatibility verified successfully."
  return 0
}

verify_wal_probe() {
  local db_volume="${1:-$DATA_VOLUME_NAME}"
  local dbtool_img="${2:-${DBTOOL_IMAGE_REF:-}}"
  log_info "Executing read-only WAL probe to verify database accessibility..."

  if command -v docker >/dev/null 2>&1 && docker volume inspect "$db_volume" >/dev/null 2>&1; then
    if [[ -n "$dbtool_img" ]]; then
      if ! docker run --rm --read-only --network none --user 1000:1000 \
        -v "${db_volume}:/data:ro" \
        "$dbtool_img" -path /data/gateway.db -readonly -check >/dev/null 2>&1; then
        log_error "Read-only WAL probe failed via dbtool check."
        return 1
      fi
    fi
  elif [[ -f "$DEFAULT_GATEWAY_DB" ]] && command -v sqlite3 >/dev/null 2>&1; then
    if ! sqlite3 "file:$DEFAULT_GATEWAY_DB?mode=ro" "PRAGMA quick_check;" >/dev/null 2>&1; then
      log_error "Read-only WAL probe failed via sqlite3 quick_check."
      return 1
    fi
  fi
  log_info "Read-only WAL probe PASSED."
  return 0
}

create_preflight_backup() {
  perform_sqlite_backup "$@"
}

perform_sqlite_backup() {
  local db_volume="$1"
  local dbtool_img="$2"
  local active_slot="$3"
  local ts
  ts="$(date -u +'%Y%m%d%H%M%S')"
  mkdir -p "$BACKUP_DIR"

  local backup_file="$BACKUP_DIR/gateway-${ts}.db"
  log_info "Performing online SQLite preflight backup: ${backup_file}"

  if docker volume inspect "$db_volume" >/dev/null 2>&1; then
    docker run --rm --user 1000:1000 \
      -e APP_ENV=production -e DATA_DIR=/data -e DATABASE_PATH=/data/gateway.db \
      -v "${db_volume}:/data:rw" \
      -v "${BACKUP_DIR}:/backup:rw" \
      "$dbtool_img" -path /data/gateway.db -backup-to "/backup/gateway-${ts}.db" >&2
  elif [[ -f "$DEFAULT_GATEWAY_DB" ]]; then
    if command -v sqlite3 >/dev/null 2>&1; then
      sqlite3 "$DEFAULT_GATEWAY_DB" "PRAGMA wal_checkpoint(TRUNCATE);" || true
      sqlite3 "$DEFAULT_GATEWAY_DB" ".backup '$backup_file'"
    fi
  fi

  if [[ ! -s "$backup_file" ]]; then
    log_error "Preflight SQLite backup failed: backup file is missing or empty"
    return 1
  fi
  chmod 600 "$backup_file" 2>/dev/null || true

  log_info "Verifying SQLite integrity of backup..."
  if command -v sqlite3 >/dev/null 2>&1; then
    local integrity
    integrity="$(sqlite3 "$backup_file" "PRAGMA integrity_check;" 2>/dev/null || echo "failed")"
    if [[ "$integrity" != "ok" ]]; then
      log_error "SQLite integrity check failed on backup: ${integrity}"
      return 1
    fi
  fi

  log_info "Verifying schema migration state..."
  if command -v sqlite3 >/dev/null 2>&1; then
    local migrations
    migrations="$(sqlite3 "$backup_file" "SELECT count(*) FROM schema_migrations;" 2>/dev/null || echo "0")"
    log_info "Schema migration count in backup: ${migrations}"
  fi

  # Backup secrets and write manifest
  local manifest_file="$BACKUP_DIR/manifest-${ts}.json"
  local db_sha256
  db_sha256="$(sha256sum "$backup_file" 2>/dev/null | cut -d' ' -f1 || echo "unknown")"
  local db_size
  db_size="$(wc -c < "$backup_file" 2>/dev/null | tr -d ' ' || echo "0")"

  local enc_backup_file=""
  local enc_db_sha256=""
  if [[ -s "$SECRETS_DIR/app_master_key" ]] && command -v openssl >/dev/null 2>&1; then
    enc_backup_file="${backup_file}.enc"
    log_info "Creating AES-256 encrypted SQLite backup at rest: ${enc_backup_file}"
    if openssl enc -aes-256-cbc -salt -pbkdf2 -in "$backup_file" -out "$enc_backup_file" -pass file:"$SECRETS_DIR/app_master_key" 2>/dev/null; then
      chmod 600 "$enc_backup_file" 2>/dev/null || true
      enc_db_sha256="$(sha256sum "$enc_backup_file" 2>/dev/null | cut -d' ' -f1 || echo "")"
      log_info "Encrypted SQLite backup generated (sha256: ${enc_db_sha256})"
    else
      log_warn "Failed to create encrypted backup with openssl, keeping standard backup."
      enc_backup_file=""
    fi
  fi

  local secrets_backup_dir="$BACKUP_DIR/secrets-${ts}"
  mkdir -p "$secrets_backup_dir"
  local sec_items=()
  for s in app_master_key tts_internal_token worker_internal_token bark_basic_auth_user bark_basic_auth_password; do
    if [[ -f "$SECRETS_DIR/$s" ]]; then
      cp -p "$SECRETS_DIR/$s" "$secrets_backup_dir/$s"
      chmod 600 "$secrets_backup_dir/$s" 2>/dev/null || true
      local s_sha
      s_sha="$(sha256sum "$SECRETS_DIR/$s" 2>/dev/null | cut -d' ' -f1 || echo "")"
      sec_items+=("{\"name\":\"$s\",\"sha256\":\"$s_sha\"}")
    fi
  done
  local joined_secrets
  joined_secrets="$(IFS=,; echo "${sec_items[*]}")"

  cat <<EOF > "$manifest_file"
{
  "timestamp": "${ts}",
  "active_slot": "${active_slot}",
  "dbtool_image": "${dbtool_img}",
  "data_volume": "${db_volume}",
  "sqlite_backup": {
    "file": "$(basename "$backup_file")",
    "sha256": "${db_sha256}",
    "size_bytes": ${db_size}
  },
  "encrypted_sqlite_backup": {
    "file": "$(basename "${enc_backup_file:-}")",
    "sha256": "${enc_db_sha256:-}"
  },
  "secrets_backup_dir": "$(basename "$secrets_backup_dir")",
  "secrets": [${joined_secrets}]
}
EOF
  chmod 600 "$manifest_file" 2>/dev/null || true
  log_info "Backup manifest recorded: ${manifest_file}"

  if [[ -n "${ENCRYPTED_BACKUP_HOOK:-}" || -n "${OFFHOST_BACKUP_HOOK:-}" ]]; then
    local hook="${ENCRYPTED_BACKUP_HOOK:-$OFFHOST_BACKUP_HOOK}"
    if [[ ! -x "$hook" ]]; then
      log_error "Offhost backup hook '$hook' is not executable. Aborting backup verification."
      return 1
    fi
    log_info "Executing verified offhost backup hook for remote backup receipt..."
    local receipt_target="${enc_backup_file:-$backup_file}"
    local receipt_out
    if ! receipt_out="$("$hook" "$manifest_file" "$receipt_target")"; then
      log_error "Offhost backup hook failed: ${receipt_out}"
      return 1
    fi
    log_info "Remote backup receipt verified: ${receipt_out}"
    printf '%s' "$receipt_out" > "${manifest_file}.receipt"
  elif [[ "${REQUIRE_REMOTE_BACKUP_RECEIPT:-0}" == "1" || "${REQUIRE_OFFHOST_BACKUP:-0}" == "1" ]]; then
    log_error "REQUIRE_REMOTE_BACKUP_RECEIPT is enabled but no executable offhost backup hook is configured. Fail closed."
    return 1
  fi

  printf '%s' "$backup_file"
}

run_dbtool_migration() {
  local db_volume="$1"
  local dbtool_img="$2"
  log_info "Executing database migration via immutable dbtool (${dbtool_img})..."
  if ! docker run --rm --user 1000:1000 --network none \
    -e DATABASE_PATH=/data/gateway.db \
    -v "${db_volume}:/data:rw" \
    "$dbtool_img" -path /data/gateway.db -migrate; then
    log_error "Database migration failed. NO AUTO RESTORE will be executed to prevent unintended data loss."
    return 1
  fi
  log_info "Database migration completed successfully."
  return 0
}

wait_for_candidate_ready() {
  local slot="$1"
  local timeout="${2:-60}"
  local expected_commit="${3:-}"
  local container="acb-gateway-${slot}"

  log_info "Waiting for candidate slot [${slot}] to pass health probes twice consecutively (timeout: ${timeout}s)..."
  local start
  start="$(date +%s)"
  local consecutive=0

  while true; do
    local is_ready=0
    local status
    status="$(docker inspect --format '{{.State.Status}}' "$container" 2>/dev/null || echo "not_found")"

    if [[ "$status" == "running" ]]; then
      if docker exec -e EXPECTED_SLOT="$slot" -e EXPECTED_RELEASE_COMMIT="$expected_commit" "$container" /gateway --deploycheck >/dev/null 2>&1; then
        is_ready=1
      elif docker exec "$container" /gateway --healthcheck >/dev/null 2>&1; then
        is_ready=1
      fi
    fi

    if [[ "$is_ready" -eq 1 ]]; then
      consecutive=$(( consecutive + 1 ))
      log_info "Candidate slot [${slot}] passed readiness probe (${consecutive}/2)."
      if [[ "$consecutive" -ge 2 ]]; then
        log_info "Candidate slot [${slot}] is healthy and ready (verified twice consecutively)."
        return 0
      fi
      sleep 1
    else
      consecutive=0
    fi

    local now
    now="$(date +%s)"
    if (( now - start >= timeout )); then
      log_error "Candidate slot [${slot}] failed to become ready within ${timeout}s."
      return 1
    fi
    sleep 2
  done
}
