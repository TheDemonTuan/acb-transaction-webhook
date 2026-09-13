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

```bash
# 1. Create directories with strict permissions
sudo mkdir -p /etc/vps-failover/apps.d
sudo mkdir -p /var/lib/vps-failover/apps
sudo mkdir -p /run/lock/vps-failover
sudo mkdir -p /opt/platform/failover

sudo chmod 0755 /etc/vps-failover /etc/vps-failover/apps.d
sudo chmod 0750 /var/lib/vps-failover /var/lib/vps-failover/apps
sudo chmod 0755 /run/lock/vps-failover
sudo chmod 0755 /opt/platform/failover

# 2. Install controller script and units
sudo cp platform/failover/vps-failover-controller.py /opt/platform/failover/
sudo chmod 0755 /opt/platform/failover/vps-failover-controller.py

sudo cp platform/failover/vps-failover-controller.service /etc/systemd/system/
sudo cp platform/failover/vps-failover-reconcile.service /etc/systemd/system/
sudo cp platform/failover/vps-failover-reconcile.timer /etc/systemd/system/

# 3. Reload systemd and start service
sudo systemctl daemon-reload
sudo systemctl enable --now vps-failover-controller.service
sudo systemctl enable --now vps-failover-reconcile.timer
```

## Running Tests

```bash
pytest platform/failover/test_failover.py -v
```
