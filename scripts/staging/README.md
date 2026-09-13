# Staging Fault-Injection Harness & Cutover Verification Runbook

Automated staging resilience, cutover failure validation, and Recovery Time Objective (RTO) benchmark suite for the ACB Transaction Webhook platform.

---

## 1. Safety Architecture & Destructive Safeguards

Destructive operations (killing containers, simulating disk exhaustion, dropping event sockets, corrupting configuration) can cause severe service interruption. The harness implements a **triple-gate safety safeguard**:

### Triple Gate Validation
1. **Explicit Authorization Flag**:
   - The harness requires `ALLOW_DESTRUCTIVE=1` in environment or `--allow-destructive` via CLI.
   - Without this flag, the harness executes in dry-run/simulation mode.
2. **Nonproduction Marker Contract**:
   - The test host must contain a nonproduction marker file:
     - `/etc/nonproduction`, OR
     - `/tmp/nonproduction-marker`, OR
     - Environment flag `NONPRODUCTION_CONFIRMED=1` / `STAGING_ENV=1`.
   - If no valid marker is detected, the script terminates immediately with exit code `1` and emits `[FATAL-SAFETY]`.
3. **Production Blacklist Shield**:
   - Hostname validation: Hostname cannot match `*prod*`, `*production*`, or `*acb-primary*`.
   - Domain validation: Target URL cannot match live production domains (e.g. `*.tuannguyenviet.site` without `staging`).

---

## 2. Scenario Catalog

The test suite systematically exercises all 10 failure domains:

| # | Scenario | Failure Injected | Expected Recovery Behavior | SLA Target RTO |
|---|---|---|---|---|
| **1** | `transaction_kill_phases` | SIGKILL at pre-commit, post-commit, or dispatch | SQLite WAL rolls back cleanly; worker restarts and replays pending queue | **<= 15s** |
| **2** | `failed_candidate` | Standby slot candidate container fails `/readyz` probe | Deploy script aborts cutover; Traefik dynamic router remains pinned to primary | **<= 5s** (0 downtime) |
| **3** | `migration` | dbtool preflight schema validation returns non-zero | Container startup aborted; database schema left untouched; primary unaffected | **<= 5s** |
| **4** | `token_newline` | Session tokens / secrets contain trailing `\r\n` | String sanitization trims whitespace/CRLF; HMAC signatures remain valid | **<= 1s** |
| **5** | `daemon_eof` | Docker daemon socket event stream drops | Failover controller applies backoff reconnect and executes dirty reconcile | **<= 10s** |
| **6** | `two_apps` | Catastrophic failure on App A (`acb` slot) | App B (`bark`) remains healthy; per-app runtime locks prevent cascade | **<= 5s** |
| **7** | `disk_full` | Data volume reaches 100% capacity (ENOSPC) | SQLite enters read-only WAL mode; gateway returns 503 rather than corrupting DB | **<= 5s** |
| **8** | `auth_active` | Primary gateway crashes while user authenticates ACB | Session marked `AUTH_SESSION_SUPERSEDED`; no orphaned browser processes | **<= 5s** |
| **9** | `core_outage` | Traefik edge platform or core container crashes | Systemd watchdog timer detects failure and restores core services | **<= 90s** |
| **10** | `soak_reboot` | Host reboots during 15-minute post-cutover soak | Controller reads `state.json`, restores correct primary, keeps standby stopped | **<= 5s** |

---

## 3. Measurable Recovery Time Objective (RTO)

The test harness measures Recovery Time Objective with automated timers:

$$\text{RTO} = T_{\text{recovered}} - T_{\text{failure\_injected}}$$

- **$T_{\text{failure\_injected}}$**: Timestamp when container is terminated or fault is applied.
- **$T_{\text{recovered}}$**: Timestamp when `/healthz` or `/readyz` returns HTTP 200 with all dependencies reporting `READY`.

### Benchmark Thresholds
- **Warm Standby Blue/Green Failover**: $\text{RTO} \le 5\text{s}$
- **Docker Daemon EOF Reconnect**: $\text{RTO} \le 10\text{s}$
- **Worker Singleton Restart & Queue Replay**: $\text{RTO} \le 15\text{s}$
- **Host Watchdog Platform Recovery**: $\text{RTO} \le 90\text{s}$

---

## 4. Execution Runbook

### A. Dry-Run / CI Simulation
Runs the full logic without modifying containers or touching disks:
```bash
bash scripts/staging/fault-injection-harness.sh --dry-run --all
```
Or via automated Bun test runner:
```bash
bun test ./scripts/staging/test-fault-injection.ts
```

### B. Live Staging Execution
To run on confirmed staging hardware:
```bash
# 1. Create nonproduction marker on the host
touch /tmp/nonproduction-marker

# 2. Execute with destructive authorization
ALLOW_DESTRUCTIVE=1 bash scripts/staging/fault-injection-harness.sh --all
```

### C. Individual Scenario Execution
```bash
# Test single failure scenario
ALLOW_DESTRUCTIVE=1 bash scripts/staging/fault-injection-harness.sh --scenario daemon_eof
```
