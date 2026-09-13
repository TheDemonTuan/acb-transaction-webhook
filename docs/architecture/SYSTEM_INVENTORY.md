# ACB Production System Inventory

**Document Version:** 1.0 (Canonical)
**Date:** 2026-09-14
**Authoritative Reference:** [`docs/superpowers/specs/2026-09-14-acb-final-production-invariants.md`](../superpowers/specs/2026-09-14-acb-final-production-invariants.md)
**Implementation Plan:** [`docs/superpowers/plans/2026-09-14-acb-production-convergence-execution.md`](../superpowers/plans/2026-09-14-acb-production-convergence-execution.md)

---

## 1. Inventory of Application Database Writes

All database modifications in SQLite WAL (`/data/gateway.db`) are cataloged below with their owning component, write path, and concurrency constraints:

| Table Name | Owning Component | Operation Type | Trigger / Description | Concurrency / Fencing Rule |
|---|---|---|---|---|
| `connections` | Gateway & Worker | `INSERT`, `UPDATE` | Updates bank connection state, active generation, account identity HMAC, encrypted account envelope. | Generation CAS check on updates (`WHERE id=? AND generation=?`). |
| `sessions` | Worker | `INSERT`, `UPDATE`, `DELETE` | Stores encrypted AES-256-GCM ACB session tokens, verification timestamps, and expiration times. | Tied to current connection ID and generation. Deleted on auth failure. |
| `auth_attempts` | Gateway & Worker | `INSERT`, `UPDATE` | Tracks browser authentication flow (`STARTING`, `IN_PROGRESS`, `EXPORTING`, `VERIFYING`, `SUCCESS`, `FAILED`, `EXPIRED`). | Strict unique index `one_active_auth_attempt` ensures only 1 active attempt at any time. |
| `poll_runs` | Worker | `INSERT`, `UPDATE` | Logs realtime poll attempts, page counts, rows seen, execution duration, and sanitized error messages. | Recorded per connection generation. |
| `checkpoints` | Worker | `INSERT`, `UPDATE` | Records contiguous transaction coverage intervals (`coverage_from`, `coverage_to`). | Updated only after a day's pages are completely ingested. |
| `coverage_gaps` | Worker | `INSERT`, `UPDATE` | Tracks detected missing transaction ranges following rate limits or unexpected network drops. | Resolved sequentially by catch-up tasks. |
| `transactions` | Worker | `INSERT` (UPSERT) | Stores deduplicated bank transactions with canonical hash, account, amount, and description. | Ingested atomically with events in `IngestTransactionsBatchWithSource`. |
| `transaction_events` | Worker | `INSERT` | Journal of transaction discovery events for SSE broadcasting and webhook triggering. | Inserted in same DB transaction as `transactions`. |
| `webhook_deliveries` | Worker | `INSERT`, `UPDATE` | Logs webhook dispatch attempts, HTTP status codes, latencies, and retry backoff counters. | Leased and updated by background notification dispatcher. |
| `history_sync_jobs` | Worker (via RPC) | `INSERT`, `UPDATE` | Durable queue for multi-day historical sync jobs (`QUEUED`, `RUNNING`, `COMPLETED`, `FAILED`, `CANCELED`). | Atomic CAS status updates. Fenced against stale generations. |
| `journal_events` | Gateway & Worker | `INSERT`, `DELETE` | Append-only sequence journal for multi-tab and Blue/Green SSE replay. Pruned by retention task. | Monotonic sequence IDs. Purged by worker retention task. |
| `monitor_settings` | Gateway | `INSERT`, `UPDATE` | Polling intervals (`default_poll_interval_sec`, `fast_poll_interval_sec`) and adaptive burst rules. | Single row per connection. Operator/Owner role required. |
| `voice_settings` | Gateway | `INSERT`, `UPDATE` | TTS voice synthesis preferences, selected voice name, speech rate, and volume. | Single row. Updated via settings UI. |
| `payment_qr_settings`| Gateway | `INSERT`, `UPDATE` | Static and dynamic VietQR parameters, template IDs, bank account numbers, bin codes. | Operator/Owner mutation. |
| `notification_channels`| Gateway & Worker | `INSERT`, `UPDATE`, `DELETE`| Configures webhook URLs, Bark keys, and Telegram targets. | Secrets encrypted with `app_master_key`. |
| `schema_migrations` | dbtool CLI | `INSERT` | Migration version, checksum, and applied timestamp. | Written only by `dbtool --migrate`. Never by runtime services. |

---

## 2. Inventory of ACB Upstream Callers

Direct HTTP calls to the official ACB banking endpoints (`online.acb.com.vn`) are strictly restricted.

| Caller Site | Invocation Trigger | Scheduler Priority | Permitted Runtime Role | Description |
|---|---|---|---|---|
| `internal/monitor/verifier.go` | Interactive operator login | `INTERACTIVE_VERIFY` | `worker` only | Verifies session validity immediately upon browser credential export. |
| `internal/monitor/poller.go` | Periodic timer (5s - 15s) | `REALTIME_POLL` | `worker` only | Queries recent transactions. Aborts immediately if active auth attempt exists. |
| `internal/monitor/sync.go` | Operator "Sync Now" button | `MANUAL_SYNC` | `worker` only | Immediate single-quantum poll requested via API. |
| `internal/monitor/keepalive.go` | Scheduled interval (120s) | `KEEPALIVE` | `worker` only | Low-priority session ping to keep bank session alive. |
| `internal/monitor/catchup.go` | Worker startup / post-downtime | `CATCH_UP` | `worker` only | Scans previous date ranges one page per quantum. Yields to realtime polls. |
| `internal/monitor/history_runner.go`| Durable history sync job | `FILTER_HISTORY` | `worker` only | Multi-day historical backfill. Yields execution after every single page. |
| **Monolith Dev Fallback** | Local developer testing | N/A | `monolith-dev` (non-prod only) | Forbidden when `APP_ENV=production`. |

> **Architectural Invariant:** Gateway containers (`acb-web-blue`, `acb-web-green`) **NEVER** make direct ACB calls. Any HTTP request requiring bank interaction is forwarded to `acb-worker` via Worker RPC.

---

## 3. Inventory of Shared Frontend Queries and Routes

All frontend API calls from `web/` are cataloged below:

| UI Route | Frontend Hook / Query | HTTP Endpoint | Required Auth Role | Purpose |
|---|---|---|---|---|
| `/` | `useStatusQuery` | `GET /api/v1/status` | `VIEWER`, `OPERATOR`, `OWNER` | Health, polling state, and bank session status. |
| `/transactions` | `useTransactionsQuery` | `GET /api/v1/transactions` | `VIEWER`, `OPERATOR`, `OWNER` | Paginated transaction ledger with filters. |
| `/transactions` | `useEnsureHistoryMutation` | `POST /api/v1/transactions/ensure-history` | `OPERATOR`, `OWNER` | Triggers durable historical synchronization. |
| `/transactions` | `useHistorySyncJobQuery` | `GET /api/v1/history-sync-jobs/{id}` | `VIEWER`, `OPERATOR`, `OWNER` | Polls progress of active history sync job. |
| `/transactions/{id}` | `useTransactionDetailQuery`| `GET /api/v1/transactions/{id}` | `VIEWER`, `OPERATOR`, `OWNER` | Detailed metadata for single transaction. |
| `/overview` | `useOverviewQuery` | `GET /api/v1/status` | `VIEWER`, `OPERATOR`, `OWNER` | Account balances, daily totals, system health. |
| `/activity` | `useDeliveriesQuery` | `GET /api/v1/deliveries` | `VIEWER`, `OPERATOR`, `OWNER` | Webhook delivery attempts and status log. |
| `/activity` | `useReplayDeliveryMutation`| `POST /api/v1/deliveries/{id}/replay` | `OPERATOR`, `OWNER` | Replays a failed webhook delivery. |
| `/bank-connection` | `useBankConnectionQuery` | `GET /api/v1/connection` | `VIEWER`, `OPERATOR`, `OWNER` | Bank configuration and credentials info. |
| `/bank-connection` | `useStartAuthMutation` | `POST /api/v1/connection/auth/start` | `OWNER` | Launches sandboxed auth-browser login flow. |
| `/bank-connection` | `useAuthScreenQuery` | `GET /api/v1/connection/auth/{id}/screen/*`| `OWNER` | Streams live browser VNC / canvas frames. |
| `/channels` | `useChannelsQuery` | `GET /api/v1/notification-channels` | `VIEWER`, `OPERATOR`, `OWNER` | Webhook & Bark notification endpoints. |
| Global Layout | `useEventsStream` (SSE) | `GET /api/v1/events/stream` | `VIEWER`, `OPERATOR`, `OWNER` | Realtime SSE event subscription. |

---

## 4. Inventory of Helper Images & Supply Chain Policies

Production requires pinned, immutable container image digests. Floating tags (`:latest`, `:main`) are strictly prohibited in production Compose manifests.

| Service | Base Image / Build Target | Digest Pinning Policy | Security Constraints & Seccomp |
|---|---|---|---|
| `gateway` | Multi-stage Go 1.27.1 + Distroless `nonroot` | Signed first-party digest via GHCR | Read-only rootfs, `no-new-privileges`, UID 1000, drop all capabilities. |
| `worker` | Multi-stage Go 1.27.1 + Distroless `nonroot` | Signed first-party digest via GHCR | Read-only rootfs, `no-new-privileges`, UID 1000, access to `/data` volume. |
| `auth-browser`| Chromium on Debian slim | Signed first-party digest via GHCR | Custom seccomp profile (`deploy/seccomp-auth-browser.json`), `IPC_LOCK`, isolated tmpfs. |
| `tts-gateway` | Python 3.12 slim (`tts-gateway/`) | Signed first-party digest via GHCR | Read-only rootfs, no outbound internet access except to Microsoft Edge TTS. |
| `bark` | `finab/bark-server` (Third-party) | Approved immutable SHA256 digest | Listed in `deploy/third-party-allowlist.json`. Isolated volume for APNs tokens. |
| `dbtool` | Ephemeral one-shot Go binary | Signed first-party digest via GHCR | Runs with `network_mode: none`. Mounts `/data` volume for migrations and backups. |

---

## 5. Secrets Inventory & Least-Privilege Access Matrix

All production secrets are mounted as read-only files under `/run/secrets/` with permissions `0400` or `0440`:

| Secret File | Environment Variable / Path | Gateway | Worker | Auth-Browser | TTS | Bark | Description |
|---|---|:---:|:---:|:---:|:---:|:---:|---|
| `app_master_key` | `APP_MASTER_KEY_FILE` (`/run/secrets/app_master_key`) | **READ** | **READ** | NO | NO | NO | 32-byte master key for AES-256-GCM encryption of database fields. |
| `worker_internal_token` | `WORKER_INTERNAL_TOKEN_FILE` | **READ** | **READ** | NO | NO | NO | Bearer token authenticating internal Worker RPC communication. |
| `tts_internal_token` | `TTS_INTERNAL_TOKEN_FILE` | **READ** | NO | NO | **READ** | NO | Internal token authenticating speech synthesis requests. |
| `bark_basic_auth_user` | `BARK_BASIC_AUTH_USER_FILE` | NO | **READ** | NO | NO | **READ** | Basic Auth username for self-hosted Bark push server. |
| `bark_basic_auth_password` | `BARK_BASIC_AUTH_PASSWORD_FILE` | NO | **READ** | NO | NO | **READ** | Basic Auth password for self-hosted Bark push server. |
| `age_recipient_key` | `AGE_RECIPIENT_KEY_FILE` | NO | NO | NO | NO | NO | Public age recipient key for database backup encryption (Host / deploy only). |

---

## 6. Deployment Entrypoints & Operator Commands

All production deployment operations are coordinated through verified bash scripts in `deploy/`:

| Script Path | Purpose | Execution Scope | Invariants & Pre-checks |
|---|---|---|---|
| `deploy/deploy-warm.sh` | Main deployment entrypoint for warm Blue/Green promotions. | Host / CI SSH | Verifies release manifest signature, checks active slot, performs warm standby cutover. |
| `deploy/deploy.sh` | Deployment wrapper orchestrating preflights and rollback traps. | Host / CI SSH | Enforces fail-closed traps on any unhandled error. |
| `deploy/switch-slot.sh` | Atomically points Traefik dynamic route to candidate slot. | Host | Validates YAML, replaces `/opt/edge/dynamic/acb.yml`, verifies identity ACK headers. |
| `deploy/rollback-warm.sh` | Rolls back Traefik dynamic route to previous healthy slot. | Host / Deploy Trap | Restores `/opt/edge/dynamic/acb.yml.prev` and verifies previous slot identity headers. |
| `deploy/backup.sh` | Creates encrypted SQLite database backup prior to migrations. | Host / dbtool | Uses `age` with public recipient. Plaintext snapshot securely unlinked. Manifest generated. |
| `deploy/verify-deployment.sh`| Post-deployment health, route identity, and RPC verification. | Host / CI | Asserts `X-Platform-Slot`, `X-Release-Commit`, and worker reachability. |
| `deploy/check-host.sh` | Validates host environment dependencies (Docker, Compose, age). | Host | Verifies versions, permissions, and `/opt/edge` presence. |

---

## 7. Rollback & Failover Commands

In case of runtime failure or deployment regression:

1. **Instant Traefik Route Rollback:**
   ```bash
   bash deploy/rollback-warm.sh
   ```
   *Restores `/opt/edge/dynamic/acb.yml.prev` atomically and verifies old slot response headers.*

2. **Emergency Gateway Failover via Failover Controller:**
   ```bash
   systemctl restart vps-failover-controller.service
   python3 platform/failover/vps-failover-controller.py --reconcile-now
   ```
   *Reconciles container state, verifies worker lock, and starts standby gateway if primary is unhealthy.*

3. **Database Disaster Recovery Restore:**
   ```bash
   # Decrypts age backup using private identity off-host and verifies integrity
   age --decrypt -i /path/to/recovery-key.txt backup_YYYYMMDD.db.age > restored.db
   sqlite3 restored.db "PRAGMA integrity_check;"
   ```
