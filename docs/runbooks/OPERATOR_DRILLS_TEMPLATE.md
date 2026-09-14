# ACB Production Operator Drills Record Template

This document provides standardized drill logs and verification records for scheduled operational exercises. Drills must be performed monthly or quarterly to ensure disaster preparedness and validate invariant enforcement.

---

## Drill 1: Gateway Blue -> Green -> Blue Cutover & Rollback Drill

**Objective:** Validate zero-downtime Traefik slot cutover, positive header ACK, standby warm transition, and instantaneous route rollback.

| Parameter | Value |
|---|---|
| **Date & Time:** | `YYYY-MM-DD HH:MM:SS UTC` |
| **Operator Name:** | `@operator` |
| **Initial Active Slot:** | `blue` |
| **Promoted Candidate Slot:** | `green` |
| **Target Gateway Digest:** | `ghcr.io/thedemontuan/acb-transaction-webhook@sha256:...` |

### Execution Checklist:
1. [ ] Execute promotion: `deploy/deploy-gateway.sh <candidate-digest>`
2. [ ] Verify positive ACK: `X-Platform-Slot: green` received in HTTP response headers.
3. [ ] Verify old slot warm state: `docker ps` shows `acb-gateway-blue` stopped with `.intentional-stop-blue`.
4. [ ] Verify zero dropped requests during cutover.
5. [ ] Execute rollback: `deploy/rollback.sh`
6. [ ] Verify route reverted: `X-Platform-Slot: blue` received within 5 seconds.
7. [ ] Confirm `acb-gateway-green` stopped into warm standby.

**Result:** `[ ] PASS   [ ] FAIL`
**Notes / Observations:** _____________________________________________________

---

## Drill 2: Worker Singleton Upgrade & Candidate Rollback Drill

**Objective:** Validate graceful worker quiesce over RPC (`/rpc/quiesce`), zero concurrent lock holders, session persistence, and candidate rollback on error.

| Parameter | Value |
|---|---|
| **Date & Time:** | `YYYY-MM-DD HH:MM:SS UTC` |
| **Operator Name:** | `@operator` |
| **Running Worker Digest:** | `ghcr.io/...@sha256:current` |
| **Candidate Worker Digest:**| `ghcr.io/...@sha256:candidate` |

### Execution Checklist:
1. [ ] Execute worker upgrade: `deploy/deploy-worker.sh <candidate-digest>`
2. [ ] Verify `/rpc/quiesce` HTTP 200 response from old worker.
3. [ ] Confirm old container stopped only after quiesce completed.
4. [ ] Validate singleton invariant: `docker ps --filter name=acb-worker | wc -l` equals 2 (header + 1 container).
5. [ ] Check worker logs: scheduler resumes polling without duplicate session errors.
6. [ ] **Rollback Exercise:** Inject deliberate failure (e.g. invalid listen address in test run); verify script automatically restarts previous worker digest and restores polling.

**Result:** `[ ] PASS   [ ] FAIL`
**Notes / Observations:** _____________________________________________________

---

## Drill 3: Off-Host Disaster Recovery & Decryption Drill

**Objective:** Validate that the off-host asymmetric `age` private key successfully decrypts production database backups and passes SQLite integrity checks in an isolated environment.

| Parameter | Value |
|---|---|
| **Date & Time:** | `YYYY-MM-DD HH:MM:SS UTC` |
| **Operator Name:** | `@operator` |
| **Backup File Tested:** | `/var/backups/acb/gateway-YYYYMMDDHHMMSS.db.age` |
| **Key Identity Reference:** | `acb-recovery-identity.txt (Vault Reference: #12345)` |

### Execution Checklist:
1. [ ] Execute automated restore drill: `scripts/ops/restore-drill.sh`
2. [ ] Decrypt encrypted snapshot using isolated container.
3. [ ] Verify SQLite integrity: `PRAGMA integrity_check` returns `ok`.
4. [ ] Verify schema migrations: `SELECT count(*) FROM schema_migrations` matches production count.
5. [ ] Confirm no plaintext artifacts leaked onto host filesystem.
6. [ ] Confirm private key wiped from temporary staging memory immediately after drill.

**Result:** `[ ] PASS   [ ] FAIL`
**Notes / Observations:** _____________________________________________________

---

## Drill 4: Active-Auth Admission Gate Fail-Closed Drill

**Objective:** Verify that when a bank authentication or OTP session is actively in-flight, deployment scripts refuse to restart or modify worker and browser sidecars.

| Parameter | Value |
|---|---|
| **Date & Time:** | `YYYY-MM-DD HH:MM:SS UTC` |
| **Operator Name:** | `@operator` |
| **Simulated Active Sessions:**| `1 (in-flight OTP state)` |

### Execution Checklist:
1. [ ] Initiate simulated active auth lock or run integration test: `tests/integration/concurrent_auth_admission_test.go`.
2. [ ] Attempt worker deployment: `deploy/deploy-worker.sh <digest>`.
3. [ ] Confirm deployment aborts with exit code 1: "Active auth session in progress".
4. [ ] Attempt browser deployment: `deploy/deploy-auth-browser.sh <digest>`.
5. [ ] Confirm browser deployment aborts with exit code 1.
6. [ ] Clear simulated auth session; confirm subsequent deployment succeeds cleanly.

**Result:** `[ ] PASS   [ ] FAIL`
**Notes / Observations:** _____________________________________________________

---

## Drill 5: Standby Failover Controller Simulation Drill

**Objective:** Verify that simulated primary node failure triggers promotion on the standby node without data loss or dual-worker split-brain.

| Parameter | Value |
|---|---|
| **Date & Time:** | `YYYY-MM-DD HH:MM:SS UTC` |
| **Operator Name:** | `@operator` |
| **Controller Test:** | `platform/failover/test_failover.py` |

### Execution Checklist:
1. [ ] Run automated failover controller unit tests: `python3 -m pytest platform/failover/test_failover.py`.
2. [ ] Simulate heartbeat probe timeouts on standby node.
3. [ ] Confirm failover controller acquires `/run/lock/vps-failover/acb.lock`.
4. [ ] Verify standby promotion initiates stack launch and confirms route ACK.
5. [ ] Re-establish primary node; confirm failover controller avoids flapping back automatically.

**Result:** `[ ] PASS   [ ] FAIL`
**Notes / Observations:** _____________________________________________________
