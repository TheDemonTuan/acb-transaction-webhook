#!/usr/bin/env python3
"""
VPS Unified Failover Controller (Event-Driven + Reconcile Safety Net)
Manages multi-app warm standby Blue/Green failover across the entire VPS.
Listens to Docker events realtime (die, oom, health_status: unhealthy).
"""

import json
import logging
import os
import signal
import subprocess
import sys
import time
from pathlib import Path

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(levelname)s] %(message)s",
    datefmt="%Y-%m-%dT%H:%M:%S%z",
)
logger = logging.getLogger("vps-failover")

COOLDOWN_SECONDS = 300  # 5 minutes per-app cooldown
STATE_DIR = Path("/tmp/vps-failover")
STATE_DIR.mkdir(parents=True, exist_ok=True)


def run_cmd(cmd, timeout=15):
    try:
        res = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout)
        return res.returncode, res.stdout.strip(), res.stderr.strip()
    except Exception as e:
        return -1, "", str(e)


def inspect_container(name):
    code, out, _ = run_cmd(["docker", "inspect", name])
    if code != 0 or not out:
        return None
    try:
        data = json.loads(out)
        if data and isinstance(data, list):
            return data[0]
    except Exception:
        pass
    return None


def is_on_cooldown(app_name):
    lock_file = STATE_DIR / f"{app_name}.cooldown"
    if lock_file.exists():
        age = time.time() - lock_file.stat().st_mtime
        if age < COOLDOWN_SECONDS:
            logger.info("App %s is on failover cooldown (elapsed %.0fs / %ds)", app_name, age, COOLDOWN_SECONDS)
            return True
        lock_file.unlink(missing_ok=True)
    return False


def set_cooldown(app_name):
    lock_file = STATE_DIR / f"{app_name}.cooldown"
    lock_file.touch()


def switch_traefik_slot(app_name, target_slot):
    # 1. Check if an app-specific switch script exists (e.g. /opt/bank-event-gateway/deploy/switch-slot.sh)
    app_dirs = [
        Path("/opt/bank-event-gateway"),
        Path(f"/opt/{app_name}"),
    ]
    for d in app_dirs:
        script = d / "deploy" / "switch-slot.sh"
        if script.exists():
            code, out, err = run_cmd(["/bin/bash", str(script), target_slot], timeout=15)
            if code == 0:
                logger.info("Successfully executed %s for slot %s", script, target_slot)
                return True
            else:
                logger.warning("Script %s failed: %s %s", script, out, err)

    # 2. Fallback: Directly update Traefik dynamic config file
    dynamic_files = [
        Path(f"/opt/platform/edge/dynamic/{app_name}.yml"),
        Path(f"/opt/edge/dynamic/{app_name}.yml"),
    ]
    for cfg in dynamic_files:
        if cfg.exists():
            try:
                content = cfg.read_text()
                # Replace acb-web-blue with acb-web-green or vice versa
                other_slot = "green" if target_slot == "blue" else "blue"
                old_str = f"acb-web-{other_slot}"
                new_str = f"acb-web-{target_slot}"
                if old_str in content:
                    content = content.replace(old_str, new_str)
                    tmp = cfg.with_suffix(".tmp")
                    tmp.write_text(content)
                    tmp.replace(cfg)
                    logger.info("Atomically updated %s upstream to %s", cfg, new_str)
                    return True
            except Exception as e:
                logger.error("Failed to update dynamic config %s: %s", cfg, e)
    return False


def handle_failover(container_name, labels):
    app = labels.get("platform.failover.app", "")
    current_slot = labels.get("platform.failover.slot", "")
    peer_name = labels.get("platform.failover.peer", "")
    workload_class = labels.get("platform.workload.class", "http")

    if not app or not peer_name:
        return

    if workload_class == "singleton":
        logger.warning("Container %s is a singleton workload. Never auto-failover to duplicate instance.", container_name)
        return

    if is_on_cooldown(app):
        return

    logger.warning("Initiating failover evaluation for app=%s (primary=%s, peer=%s)", app, container_name, peer_name)

    # Verify primary failure (double check with 2s delay to avoid transient spikes)
    time.sleep(2)
    inspect = inspect_container(container_name)
    if inspect:
        state = inspect.get("State", {})
        status = state.get("Status", "")
        health = state.get("Health", {}).get("Status", "")
        # If primary recovered on its own, cancel failover
        if status == "running" and (health == "healthy" or not health):
            logger.info("Primary %s recovered on its own. Failover aborted.", container_name)
            return

    # Attempt 1 bounded emergency restart of primary if container died
    logger.info("Attempting single emergency restart of primary container %s...", container_name)
    run_cmd(["docker", "restart", "-t", "5", container_name], timeout=10)
    time.sleep(3)
    inspect = inspect_container(container_name)
    if inspect:
        state = inspect.get("State", {})
        if state.get("Status") == "running" and state.get("Health", {}).get("Status") != "unhealthy":
            logger.info("Primary %s recovered after restart. Failover aborted.", container_name)
            return

    # Primary still failing -> Activate Peer (Warm Standby)
    logger.critical("Primary %s failed. Activating warm standby peer %s...", container_name, peer_name)
    set_cooldown(app)

    code, out, err = run_cmd(["docker", "start", peer_name], timeout=15)
    if code != 0:
        logger.error("Failed to start peer container %s: %s %s", peer_name, out, err)
        return

    # Wait for peer readiness (up to 20 seconds)
    peer_ready = False
    peer_slot = "green" if current_slot == "blue" else "blue"
    for _ in range(10):
        time.sleep(2)
        p_inspect = inspect_container(peer_name)
        if not p_inspect:
            continue
        p_state = p_inspect.get("State", {})
        if p_state.get("Status") == "running":
            # Check internal health probe if available
            code, _, _ = run_cmd(["docker", "exec", peer_name, "/gateway", "--healthcheck"], timeout=3)
            if code == 0:
                peer_ready = True
                break
            # If no healthcheck binary, running status is accepted
            if "Health" not in p_state:
                peer_ready = True
                break

    if peer_ready:
        logger.info("Peer container %s is READY. Switching routing pointer to %s...", peer_name, peer_slot)
        if switch_traefik_slot(app, peer_slot):
            logger.info("SUCCESS: Failover completed. App %s is now served by %s", app, peer_name)
        else:
            logger.error("Failed to update route pointer for app %s to slot %s", app, peer_slot)
    else:
        logger.error("ERROR: Peer container %s failed to become healthy within timeout", peer_name)


def reconcile():
    """Safety net scan every 60s: inspects all failover-enabled containers"""
    logger.info("Running reconcile safety net scan...")
    code, out, _ = run_cmd(["docker", "ps", "-a", "--filter", "label=platform.failover.enabled=true", "--format", "{{json .}}"])
    if code != 0 or not out:
        return

    apps = {}
    for line in out.splitlines():
        try:
            c = json.loads(line)
            inspect = inspect_container(c.get("ID", c.get("Names", "")))
            if not inspect:
                continue
            labels = inspect.get("Config", {}).get("Labels", {})
            app = labels.get("platform.failover.app")
            if not app:
                continue
            if app not in apps:
                apps[app] = []
            apps[app].append({
                "name": inspect.get("Name", "").lstrip("/"),
                "state": inspect.get("State", {}),
                "labels": labels,
            })
        except Exception:
            pass

    for app, containers in apps.items():
        running = [c for c in containers if c["state"].get("Status") == "running"]
        unhealthy = [c for c in running if c["state"].get("Health", {}).get("Status") == "unhealthy"]

        if not running and containers:
            logger.warning("Reconcile: App %s has NO running containers. Starting first peer %s", app, containers[0]["name"])
            run_cmd(["docker", "start", containers[0]["name"]])
        elif unhealthy and len(running) == 1:
            bad_c = unhealthy[0]
            logger.warning("Reconcile: App %s active container %s is unhealthy", app, bad_c["name"])
            handle_failover(bad_c["name"], bad_c["labels"])


def listen_events():
    """Event-driven fast path: streams Docker events realtime"""
    logger.info("VPS Failover Controller started. Listening to realtime Docker events...")
    cmd = [
        "docker", "events",
        "--format", "{{json .}}",
        "--filter", "type=container",
        "--filter", "event=die",
        "--filter", "event=oom",
        "--filter", "event=health_status",
    ]

    while True:
        proc = None
        try:
            proc = subprocess.Popen(cmd, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True)
            for line in proc.stdout:
                line = line.strip()
                if not line:
                    continue
                try:
                    event = json.loads(line)
                    action = event.get("Action", "")
                    actor = event.get("Actor", {})
                    attributes = actor.get("Attributes", {})
                    container_name = attributes.get("name", "")

                    if attributes.get("platform.failover.enabled") != "true":
                        continue

                    # Process triggers: die, oom, or health_status: unhealthy
                    if action in ("die", "oom") or action.startswith("health_status: unhealthy"):
                        logger.warning("Detected event: action=%s on container=%s", action, container_name)
                        handle_failover(container_name, attributes)
                except Exception as e:
                    logger.debug("Error parsing event: %s", e)
        except Exception as e:
            logger.error("Docker events stream interrupted: %s. Retrying in 5s...", e)
            time.sleep(5)
        finally:
            if proc:
                try:
                    proc.kill()
                except Exception:
                    pass


def main():
    if "--reconcile" in sys.argv:
        reconcile()
        return

    def sig_handler(sig, frame):
        logger.info("Terminating VPS Failover Controller...")
        sys.exit(0)

    signal.signal(signal.SIGINT, sig_handler)
    signal.signal(signal.SIGTERM, sig_handler)

    listen_events()


if __name__ == "__main__":
    main()
