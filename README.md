# ACB Transaction Webhook

[![CI](https://github.com/TheDemonTuan/acb-transaction-webhook/actions/workflows/ci.yml/badge.svg)](https://github.com/TheDemonTuan/acb-transaction-webhook/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](https://opensource.org/licenses/MIT)
[![Go Version](https://img.shields.io/badge/Go-1.27.1-00ADD8?logo=go)](https://golang.org)
[![Bun Version](https://img.shields.io/badge/Bun-1.4.2-FBF0DF?logo=bun)](https://bun.sh)

A secure, high-performance service that monitors transaction history from Asia Commercial Bank (ACB ONE Web) and automatically dispatches signed webhooks to your applications in real-time.

---

## Authoritative Documentation & Specifications

All contributors, operators, and automated workers must refer to the canonical production architecture specifications:

- **Authoritative Specification:** [`docs/superpowers/specs/2026-09-14-acb-final-production-invariants.md`](docs/superpowers/specs/2026-09-14-acb-final-production-invariants.md) — The single source of truth for production invariants, failure domains, scheduling, and security contracts.
- **Production Architecture Overview:** [`docs/architecture/PRODUCTION_ARCHITECTURE.md`](docs/architecture/PRODUCTION_ARCHITECTURE.md) — High-level architecture, container roles, and Docker network topology.
- **Implementation & Convergence Plan:** [`docs/superpowers/plans/2026-09-14-acb-production-convergence-execution.md`](docs/superpowers/plans/2026-09-14-acb-production-convergence-execution.md) — Master implementation tracker for Tasks 1–51 and 16 acceptance gates.
- **Compatibility Contracts:** [`docs/architecture/COMPATIBILITY_CONTRACTS.md`](docs/architecture/COMPATIBILITY_CONTRACTS.md) — Interface schemas, async DTOs, scheduler interfaces, and route ACK protocols.
- **System Inventory:** [`docs/architecture/SYSTEM_INVENTORY.md`](docs/architecture/SYSTEM_INVENTORY.md) — Complete inventory of database writes, upstream ACB callers, secrets, and deploy entrypoints.

*(Note: All earlier planning documents, platform migration drafts, and incremental fix notes in the repository root are historical and have been superseded by the 2026-09-14 canonical specifications).*

---

## Highlights & Features

- **Automated Bank Transaction Monitoring**: Continuously monitors ACB bank accounts for incoming and outgoing transactions with adaptive polling.
- **Reliable Webhook Delivery**:
  - Signed webhooks with HMAC-SHA256 (`X-Signature-SHA256`) for tamper-proof validation.
  - Automatic retry policy with exponential backoff on delivery failures.
  - Webhook delivery history log with response status codes and replay capabilities.
  - Built-in **SSRF protection** preventing webhooks from targeting internal/private network IP ranges.
- **Zero-Trust Security**:
  - Sandboxed Chromium browser container (`auth-browser`) for interactive login and session capture without exposing raw credentials.
  - Cloudflare Access (Zero Trust) JWT authentication with role-based access control (`OWNER`, `OPERATOR`, `VIEWER`).
  - Stored credentials and tokens encrypted with **AES-256-GCM** using a master key.
  - Single-instance SQLite database running in WAL mode with strict process file locks.
- **Embedded Web Management Dashboard**:
  - Fast single-page application (SPA) built with React 19, TypeScript, and Tailwind CSS.
  - Compiled directly into the standalone Go binary — zero external static file dependencies.
  - Manage webhook endpoints, inspect transactions, monitor sync status, and launch authenticated bank sessions.
- **Container-First CI/CD**:
  - Independent, parallel image build jobs for the Gateway and Auth Browser in GitHub Actions.
  - Ready-to-use Docker Compose configurations for production deployment with Cloudflare Tunnel.

---

## Architecture

```
                                  +-----------------------------+
                                  |    Cloudflare Zero Trust    |
                                  |    (Access + Tunnel)        |
                                  +--------------+--------------+
                                                 |
                                                 v
+------------------+              +-----------------------------+              +----------------------+
|     ACB ONE      | <----------> |     acb-transaction-gateway | -----------> |   Webhook Endpoint   |
|   Online Bank    |  (Session)   |  (Go API + Embedded React)  |  (HMAC-SHA)  |  (Your Application)  |
+--------^---------+              +--------------+--------------+              +----------------------+
         |                                       |
         | (Auth handoff)                        | (Local RPC / VNC)
+--------+---------+                             |
|   auth-browser   | <---------------------------+
| (Isolated Chrome)|
+------------------+
```

---

## Operational Runbooks & Procedures

Authoritative operational guides for production deployment, failover, key management, and disaster recovery:

| Runbook | Scope & Purpose | Link |
|---|---|---|
| **Production Deployment Runbook** | Release scope, bootstrap transition, isolated component rollouts, route identity ACK, rollback | [`docs/runbooks/DEPLOYMENT_RUNBOOK.md`](docs/runbooks/DEPLOYMENT_RUNBOOK.md) |
| **Failover & Standby Runbook** | Host failover controller, shared release locking, split-brain fencing, standby promotion | [`docs/runbooks/FAILOVER_RUNBOOK.md`](docs/runbooks/FAILOVER_RUNBOOK.md) |
| **Secret Provisioning Runbook** | Secrets directory isolation, `app_master_key` generation, asymmetric `age` keypairs | [`docs/runbooks/SECRET_PROVISIONING_RUNBOOK.md`](docs/runbooks/SECRET_PROVISIONING_RUNBOOK.md) |
| **Encrypted Backup Runbook** | Online SQLite snapshotting (`VACUUM INTO`), age asymmetric encryption, WAL verification | [`docs/runbooks/BACKUP_RUNBOOK.md`](docs/runbooks/BACKUP_RUNBOOK.md) |
| **Restore Runbook** | Decrypting backups in staging via private recovery identity, integrity verification | [`docs/runbooks/RESTORE_RUNBOOK.md`](docs/runbooks/RESTORE_RUNBOOK.md) |
| **Disaster Recovery Runbook** | Catastrophic recovery protocol, cold-iron host provisioning, volume restoration | [`docs/runbooks/DISASTER_RECOVERY_RUNBOOK.md`](docs/runbooks/DISASTER_RECOVERY_RUNBOOK.md) |
| **Observability & Telemetry** | Prometheus metrics, system health probes, queue depth, scheduler telemetry | [`docs/runbooks/OBSERVABILITY.md`](docs/runbooks/OBSERVABILITY.md) |
| **Handoff & Release Checklist** | Pre-promotion checklist, verification gates, operator sign-off template | [`docs/runbooks/HANDOFF_RELEASE_CHECKLIST.md`](docs/runbooks/HANDOFF_RELEASE_CHECKLIST.md) |
| **Operator Drills Record** | Standard drill templates: Blue/Green rollback, worker quiesce, off-host recovery | [`docs/runbooks/OPERATOR_DRILLS_TEMPLATE.md`](docs/runbooks/OPERATOR_DRILLS_TEMPLATE.md) |

---

## Production Deployment: Bootstrap Transition

> **ARCHITECTURAL REQUIREMENT:** Whole-stack compose shortcuts (`docker compose up -d`) and monolithic deployments (`deploy-warm.sh`, `--upgrade-core`) are **RETIRED AND HARD-FAIL**. Production promotions execute via component transactions or the signed rollout dispatcher.

### 1. Host Preparation & Volume Setup
```bash
git clone https://github.com/TheDemonTuan/acb-transaction-webhook.git /opt/acb-transaction-webhook
cd /opt/acb-transaction-webhook

# Initialize data volumes with safety confirmation
deploy/init-fresh-data.sh --confirm-fresh-init
```

### 2. Secret Provisioning & Runtime Configuration
```bash
# Provision isolated secrets (0700 dir, 0600 files, app_master_key)
deploy/provision-secrets.sh --confirm-fresh-provision

cp .env.example deploy/.env.production
chmod 600 deploy/.env.production
# Edit deploy/.env.production with host domain, Cloudflare credentials, and ACB config
```

### 3. Initialize Release State & Immutable Image References
```bash
deploy/release-env.sh init \
  --gateway-blue ghcr.io/thedemontuan/acb-transaction-webhook@sha256:<gateway-digest> \
  --gateway-green ghcr.io/thedemontuan/acb-transaction-webhook@sha256:<gateway-digest> \
  --worker-image ghcr.io/thedemontuan/acb-transaction-webhook-worker@sha256:<worker-digest> \
  --dbtool-image ghcr.io/thedemontuan/acb-transaction-webhook-dbtool@sha256:<dbtool-digest> \
  --browser-image ghcr.io/thedemontuan/acb-transaction-webhook-auth-browser@sha256:<browser-digest> \
  --tts-image ghcr.io/thedemontuan/acb-transaction-webhook-tts-gateway@sha256:<tts-digest> \
  --bark-image ghcr.io/finb/bark-server@sha256:32d65b07fa835c99b31a396b77727a04ed058377fc2482da3e9dc7397167ffc4
```

### 4. Dependency-Ordered Stack Launch
```bash
# 1. Database schema migration
deploy/deploy-schema.sh ghcr.io/thedemontuan/acb-transaction-webhook-dbtool@sha256:<dbtool-digest>

# 2. Auxiliary sidecars
deploy/deploy-auth-browser.sh ghcr.io/thedemontuan/acb-transaction-webhook-auth-browser@sha256:<browser-digest>
deploy/deploy-tts.sh ghcr.io/thedemontuan/acb-transaction-webhook-tts-gateway@sha256:<tts-digest>
deploy/deploy-bark.sh ghcr.io/finb/bark-server@sha256:32d65b07fa835c99b31a396b77727a04ed058377fc2482da3e9dc7397167ffc4

# 3. Worker singleton
deploy/deploy-worker.sh ghcr.io/thedemontuan/acb-transaction-webhook-worker@sha256:<worker-digest>

# 4. Gateway Blue slot
deploy/deploy-gateway.sh ghcr.io/thedemontuan/acb-transaction-webhook@sha256:<gateway-digest>
```

For subsequent updates, execute component-specific scripts or use `deploy/dispatch-rollout.sh` with a signed release manifest. See [`docs/runbooks/DEPLOYMENT_RUNBOOK.md`](docs/runbooks/DEPLOYMENT_RUNBOOK.md).

---

## Local Development Setup

### Prerequisites

- **Go 1.27+**
- **Bun 1.4+**
- **Docker** (optional, for auth-browser testing)

### Build and Run

1. **Build the web frontend:**

```bash
cd web
bun install --frozen-lockfile
bun run build
cd ..
```

2. **Run backend unit and integration tests:**

```bash
go test -race ./...
```

3. **Start the gateway locally:**

```bash
export APP_ENV=development
export LISTEN_ADDR=127.0.0.1:8080
export DATA_DIR=/tmp/acb-transaction-webhook
go run ./cmd/gateway
```

Visit `http://127.0.0.1:8080` in your browser.

---

## Webhook Payload Specification

When a new transaction is detected, the gateway sends an HTTP POST request to all active webhook endpoints.

### Headers

| Header | Description |
|---|---|
| `Content-Type` | `application/json` |
| `X-Signature-SHA256` | Hex-encoded HMAC-SHA256 signature calculated over the raw request body with your webhook secret |
| `X-Event-ID` | Unique UUID representing the transaction delivery event |
| `User-Agent` | `ACB-Transaction-Webhook/2.0` |

### JSON Payload Example

```json
{
  "event_id": "9b1deb4d-3b7d-4bad-9bdd-2b0d7b3dcb6d",
  "event_type": "transaction.created",
  "timestamp": "2026-09-11T08:30:00Z",
  "data": {
    "account_number": "12345678",
    "transaction_id": "ACB1234567890",
    "amount": 500000.0,
    "currency": "VND",
    "transaction_type": "IN",
    "balance_after": 15500000.0,
    "description": "CHUYEN TIEN THANH TOAN DON HANG 9876",
    "transaction_time": "2026-09-11T08:28:15Z"
  }
}
```

### Verifying Signatures in Your Webhook Receiver

#### Node.js / Express Example

```javascript
import crypto from 'crypto';

function verifyWebhookSignature(req, secret) {
  const signature = req.headers['x-signature-sha256'];
  if (!signature) return false;

  const expectedSignature = crypto
    .createHmac('sha256', secret)
    .update(req.rawBody)
    .digest('hex');

  return crypto.timingSafeEqual(
    Buffer.from(signature, 'hex'),
    Buffer.from(expectedSignature, 'hex')
  );
}
```

#### Go Example

```go
package main

import (
    "crypto/hmac"
    "crypto/sha256"
    "encoding/hex"
)

func VerifySignature(payload []byte, secret, expectedSig string) bool {
    mac := hmac.New(sha256.New, []byte(secret))
    mac.Write(payload)
    actualSig := hex.EncodeToString(mac.Sum(nil))
    return hmac.Equal([]byte(actualSig), []byte(expectedSig))
}
```

---

## CI/CD Pipeline

The project includes GitHub Actions workflows configured with parallelization:

- **`.github/workflows/ci.yml`**:
  - Frontend typecheck & Vite build.
  - Go unit tests with race detection (`go test -race ./...`).
  - Parallel Docker smoke test jobs:
    - `docker-smoke-gateway`: verifies `/gateway --healthcheck`.
    - `docker-smoke-auth-browser`: verifies `/auth-browser --healthcheck` and container isolation.
- **`.github/workflows/deploy.yml`**:
  - Independent parallel GHCR image builds for Gateway and Auth-Browser (`build-gateway-image` & `build-auth-browser-image`).
  - Automated deployment over SSH to production VPS with verification and atomic rollback scripts.

---

## Environment Variables

| Variable | Default | Description |
|---|---|---|
| `APP_ENV` | `production` | Environment mode (`development` or `production`) |
| `LISTEN_ADDR` | `0.0.0.0:8080` | Bind address for the HTTP gateway |
| `PORT` | `8090` | Container exposed port |
| `PUBLIC_ORIGIN` | `https://bank.example.com` | Public base URL used for CORS and CSRF validation |
| `DATA_DIR` | `/data` | Directory for SQLite database and state files |
| `DATABASE_PATH` | `/data/gateway.db` | Path to SQLite database file |
| `APP_MASTER_KEY_FILE` | `/run/secrets/app_master_key` | Path to 32-byte encryption key |
| `DEFAULT_POLL_INTERVAL_SEC` | `15` | Default transaction sync interval in seconds |
| `FAST_POLL_INTERVAL_SEC` | `5` | Polling interval during active transaction bursts |
| `CLOUDFLARE_ACCESS_AUD` | - | Cloudflare Access Application Audience tag |
| `CF_ACCESS_ISSUER` | - | Cloudflare Access team issuer URL |
| `CF_ACCESS_JWKS_URL` | - | Cloudflare Access JWKS certificate URL |
| `OWNER_SUBJECTS` | - | Comma-separated list of admin email subjects |

---

## Security Best Practices

1. **Never commit secrets**: Master keys, Cloudflare tokens, and banking session data should never be committed into git.
2. **Use Cloudflare Access**: Always restrict gateway access behind Cloudflare Access or a VPN in production environments.
3. **SSRF Guard**: The gateway automatically blocks internal RFC1918 subnets, link-local, and loopback IPs when sending webhooks.
4. **Isolated Chromium Container**: The browser runs inside an unprivileged Docker sandbox with a custom seccomp profile (`deploy/seccomp-auth-browser.json`).

---

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.
