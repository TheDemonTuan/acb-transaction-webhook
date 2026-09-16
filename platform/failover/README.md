# VPS Stateful Multi-App Failover Engine

Reliable, decoupled warm-standby (Blue/Green) and singleton failover engine with root-owned registry, canonical crash-safe state, per-app runtime locking, bounded event queueing, and periodic reconciliation.

## Architecture

```
Docker Daemon (/var/run/docker.sock)
       ???
       ??? (real-time container events)
??????????????????????????????????????????????????????????????????????????????????????????????????????????????????????????????????????????????????????????????????????????????
??? VPS Failover Controller                                ???
??? ?????? Event Ingestor (bounded queue + dirty reconcile)   ???
??? ?????? Worker (per-app coalescing + independent locks)     ???
??? ?????? Safety Net Reconciler (periodic scan every 60s)     ???
??????????????????????????????????????????????????????????????????????????????????????????????????????????????????????????????????????????????????????????????????????????????
       ???                              ???
       ???                              ???
/etc/vps-failover/apps.d/      /var/lib/vps-failover/apps/
(Root-owned trusted registry)  (Crash-safe state + CAS revisions)
```

## Directory Structure & Permissions

| Path | Owner | Mode | Description |
|---|---|---|---|
| `/etc/vps-failover/apps.d/*.json` | `root:root` | `0644` (dir `0755`) | Trusted root-owned application definitions |
| `/var/lib/vps-failover/apps/<app>/state.json` | `root:root` | `0640` (dir `0750`) | Canonical crash-safe runtime state |
| `/run/lock/vps-failover/<app>.lock` | `root:root` | `0640` (dir `0755`) | Runtime mutual exclusion lock per application |
| `/opt/platform/failover/` | `root:root` | `0755` | Engine script and systemd units |

## Security & Privilege Model

> **CRITICAL SECURITY NOTE ??? DOCKER PRIVILEGE EQUIVALENCE**:
> Any process with read/write access to `/var/run/docker.sock` has effective root-equivalent control over the host system. The controller must be treated as privileged code.
> - Registry files in `/etc/vps-failover/apps.d/` are strictly owned by `root:root` (`0644`).
> - The controller strictly ignores any executable paths or shell commands passed via Docker container labels.
> - Traffic switching is executed exclusively via trusted executable paths starting with `/usr/local/bin/`, `/opt/platform/bin/`, `/usr/bin/`, or `/bin/` without shell interpolation (`shell=False`).
> - Systemd sandboxing directives (`ProtectSystem=strict`, `ProtectHome=true`, `PrivateTmp=true`, `NoNewPrivileges=true`) enforce least filesystem exposure.

## Registry Format (`/etc/vps-failover/apps.d/<app>.json`)

### Blue/Green (Warm Standby) Workload
```json
{
  "app": "acb",
  "workload_class": "blue_green",
  "cooldown_seconds": 300,
  "max_restarts": 3,
  "health_timeout": 20,
  "switch_cmd": ["/usr/local/bin/platform-switch", "{app}", "{slot}"],
  "slots": {
    "blue": {
      "container_name": "acb-web-blue",
      "service": "acb-web"
    },
    "green": {
      "container_name": "acb-web-green",
      "service": "acb-web"
    }
  }
}
```

### Singleton Workload
```json
{
  "app": "auth-browser",
  "workload_class": "singleton",
  "container_name": "auth-browser",
  "max_restarts": 3,
  "cooldown_seconds": 60
}
```

## Canonical State Schema (`/var/lib/vps-failover/apps/<app>/state.json`)

```json
{
  "schema_version": 1,
  "app": "acb",
  "revision": 14,
  "active_slot": "blue",
  "operation_lease": null,
  "pending_route": null,
  "slots": {
    "blue": {
      "container_id": "9a1b2c3d...",
      "image_digest": "sha256:...",
      "status": "running",
      "health": "healthy",
      "last_transition": 1726200000.0
    },
    "green": {
      "container_id": "4e5f6a7b...",
      "image_digest": "sha256:...",
      "status": "stopped",
      "health": "",
      "last_transition": 1726195000.0
    }
  },
  "last_failover_time": 1726195000.0,
  "restarts_count": 0,
  "degraded": false,
  "degraded_reason": null
}
```

## Deployment Journal Coordination & Mutual Host Locking

The controller coordinates with transactional deployments (`deploy-gateway.sh`, `deploy-worker.sh`, `deploy-schema.sh`, `deploy-auth-browser.sh`, `deploy-tts.sh`, `deploy-bark.sh`):
1. **Mutual Host Lock (`/run/lock/vps-failover/<app>.lock`)**: Deployment scripts and the failover controller share the exact same per-app lock path. If deploy holds the lock, the failover engine catches lock contention, backs off safely, and never races or corrupts the route pointer.
2. **Deployment Journal Awareness (`deploy-journal.json`)**: When a deploy transaction is active (`TX_INITIALIZED`, `CANDIDATE_STARTING`, `VERIFYING_HEALTH`, `SWITCHING_ROUTE`, `VERIFYING_ACK`, `TX_SOAKING`, `TX_COMMITTED`), failover promotion is strictly inhibited. The controller treats candidate containers running during tests or soak as candidate state rather than committed state.
3. **Intentional Stop Markers (`intentional-stop-<slot>`)**: Prior to stopping old slots after deployment or during maintenance, deploy scripts place intentional stop markers only in the canonical `/var/lib/vps-failover/apps/<app>/` state directory. Docker container termination (`die`, `oom`) events matching an intentional stop marker are treated as expected transitions and do not trigger failovers.

## Exact Route Identity ACK & Bounded Recovery

- **Positive Edge Route ACK**: When failover activates warm standby, the controller executes the trusted switch command and verifies route ACK. If the route switch or edge ACK fails, the standby container is rolled back/stopped, and the app enters degraded mode without promoting a faulty target.
- **Bounded Cooldown & Exponential Backoff**: Failovers are bounded by `cooldown_seconds` and `max_restarts`. Repeated flapping triggers a degraded alert rather than endless restart storms.

## Split-Brain Prevention & Singleton Fencing

- **Singleton Workloads (`worker`, `auth-browser`)**: Registered singleton apps undergo bounded restarts on failure using exponential backoff (`min_restart_interval` and `backoff_factor`).
- **Strict Fencing**: The controller restarts only the existing singleton container name (`acb-worker`, `acb-auth-browser`) and never spawns parallel or duplicate containers. If an active deployment is in progress for the component, failover restarts are deferred.

## Edge Case Failure Handlers

| Failure Scenario | Controller Action |
|---|---|
| **Crash Between Phases** | State files use atomic tmp writes (`state.json.tmp.*`) with directory fsync. Orphan `.tmp` files left behind by crashes are cleaned up automatically on engine startup. |
| **Stale / Corrupt State** | Corrupt JSON in `state.json` is automatically backed up to `state.json.corrupt.<ts>`. The controller resets to initial safe degraded state and inspects actual live Docker containers rather than arbitrarily switching traffic. |
| **Rollback Failure** | If standby startup or switch ACK fails, the failed container is stopped, state is updated with `degraded=True` and explicit reason, and no active promotion occurs. |
| **Both Slots Degraded** | If the active slot dies and the standby slot fails start or readiness check, the controller halts failover, stops failed containers, and declares degraded state (`Both slots degraded`). |

## Operation Leases & Stale Lease Eviction

Deploy processes or maintenance tools may acquire an operation lease in `state.json`:
```json
"operation_lease": {
  "owner": "deploy",
  "operation": "deploy-warm",
  "acquired_at": 1726200000.0,
  "expires_at": 1726200600.0
}
```
- While the lease is active (`now < expires_at`), the controller suppresses automated failover switches to avoid deploy-watchdog races.
- If a deploy process crashes, the controller automatically detects when `now >= expires_at`, reaps the stale lease, and resumes normal recovery.

## Standby Behavior & Safe Degraded Handling

- **Intentional Standby Stopped**: When the standby peer is stopped intentionally by an operator or deploy process, the controller records the stopped status without triggering failover or starting containers.
- **Missing Active Slot**: If the active slot is unknown or unresolvable, the controller marks the application `degraded = True` with an alert; it **never defaults to blue**.

## Installation

The controller is a release-managed component. Do not copy the Python file, registry, or systemd units manually. Changes under `platform/failover/**` are classified as `failover_controller`, included in the signed release manifest, and installed transactionally by `deploy/deploy-failover-controller.sh` through `deploy/dispatch-rollout.sh`.

The transaction validates Python, JSON registries, systemd units, and checksums before installation. It snapshots the installed code, units, registry, and enablement state; restarts and verifies the candidate; and restores the verified previous controller if installation fails. The stable deployer provisions the shared `/run/lock/vps-failover` and `/var/lib/vps-failover/apps/acb` paths. There is no `/tmp` lock fallback because systemd `PrivateTmp` would split coordination between deployment and failover processes.

## Running Tests

```bash
pytest platform/failover/test_failover.py -v
```
