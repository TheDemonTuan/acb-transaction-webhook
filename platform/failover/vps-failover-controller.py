#!/usr/bin/env bash
# platform/failover/vps-failover-controller.py
"""
VPS Unified Stateful Multi-App Failover Engine.
Event-driven + periodic reconciliation for warm standby Blue/Green and singleton workloads.
Trusted root-owned registry: /etc/vps-failover/apps.d/<app>.json
Canonical crash-safe state:   /var/lib/vps-failover/apps/<app>/state.json
Runtime per-app locks:        /run/lock/vps-failover/<app>.lock
"""

from __future__ import annotations

import argparse
import contextlib
import json
import logging
import os
import queue
import re
import signal
import subprocess
import sys
import threading
import time
import uuid
from dataclasses import dataclass, field
from datetime import datetime
from pathlib import Path
from typing import Any, Callable, Dict, List, Optional, Set, Tuple

try:
    import fcntl
except ImportError:
    fcntl = None  # type: ignore

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(levelname)s] [%(name)s] %(message)s",
    datefmt="%Y-%m-%dT%H:%M:%S%z",
)
logger = logging.getLogger("vps-failover")

DEFAULT_REGISTRY_DIR = Path("/etc/vps-failover/apps.d")
DEFAULT_STATE_DIR = Path("/var/lib/vps-failover/apps")
DEFAULT_LOCK_DIR = Path("/run/lock/vps-failover")
DEFAULT_JOURNAL_PATH = Path("/opt/acb/deploy/data/deploy-journal.json")
DEFAULT_COOLDOWN_SECONDS = 300
DEFAULT_MAX_RESTARTS = 3
TRUSTED_PATH_PREFIXES = ("/usr/local/bin/", "/opt/platform/bin/", "/usr/bin/", "/bin/")
APP_NAME_REGEX = re.compile(r"^[a-zA-Z0-9_-]+$")

class CASConflictError(Exception):
    """Raised when state revision does not match expected CAS revision."""

@dataclass
class SlotConfig:
    slot_name: str
    container_name: str
    service: str = ""

@dataclass
class AppConfig:
    app: str
    workload_class: str  # "blue_green" or "singleton"
    slots: Dict[str, SlotConfig] = field(default_factory=dict)
    container_name: str = ""  # for singleton
    cooldown_seconds: int = DEFAULT_COOLDOWN_SECONDS
    max_restarts: int = DEFAULT_MAX_RESTARTS
    switch_cmd: Optional[List[str]] = None
    health_timeout: int = 20
    min_restart_interval: int = 0
    backoff_factor: float = 0.0

    @classmethod
    def from_dict(cls, data: dict, app_name: str) -> AppConfig:
        if not APP_NAME_REGEX.match(app_name):
            raise ValueError(f"Invalid app name '{app_name}'")

        cfg_app = data.get("app", app_name)
        if cfg_app != app_name:
            raise ValueError(f"Registry 'app' field ({cfg_app}) does not match filename ({app_name})")

        raw_class = data.get("workload_class", "blue_green").lower()
        if raw_class in ("http", "blue_green", "warm_standby"):
            workload_class = "blue_green"
        elif raw_class in ("singleton", "worker"):
            workload_class = "singleton"
        else:
            raise ValueError(f"Unsupported workload_class '{raw_class}' for app '{app_name}'")

        slots: Dict[str, SlotConfig] = {}
        container_name = ""

        if workload_class == "blue_green":
            raw_slots = data.get("slots", {})
            if not isinstance(raw_slots, dict) or len(raw_slots) < 2:
                raise ValueError(f"App '{app_name}' must configure at least 2 slots (e.g. blue and green)")
            for s_name, s_val in raw_slots.items():
                if not isinstance(s_val, dict) or not s_val.get("container_name"):
                    raise ValueError(f"Slot '{s_name}' for app '{app_name}' must specify 'container_name'")
                slots[s_name] = SlotConfig(
                    slot_name=s_name,
                    container_name=str(s_val["container_name"]),
                    service=str(s_val.get("service", "")),
                )
        else:
            container_name = str(data.get("container_name", ""))
            if not container_name:
                raise ValueError(f"Singleton app '{app_name}' must specify 'container_name'")

        switch_cmd = data.get("switch_cmd")
        if switch_cmd is not None:
            if not isinstance(switch_cmd, list) or not switch_cmd:
                raise ValueError(f"Invalid switch_cmd for app '{app_name}'")
            # Validate trusted executable path
            bin_path = switch_cmd[0]
            if not any(bin_path.startswith(prefix) for prefix in TRUSTED_PATH_PREFIXES):
                raise ValueError(f"switch_cmd binary '{bin_path}' not in trusted prefixes: {TRUSTED_PATH_PREFIXES}")

        return cls(
            app=app_name,
            workload_class=workload_class,
            slots=slots,
            container_name=container_name,
            cooldown_seconds=int(data.get("cooldown_seconds", DEFAULT_COOLDOWN_SECONDS)),
            max_restarts=int(data.get("max_restarts", DEFAULT_MAX_RESTARTS)),
            switch_cmd=switch_cmd,
            health_timeout=int(data.get("health_timeout", 20)),
            min_restart_interval=int(data.get("min_restart_interval", 0)),
            backoff_factor=float(data.get("backoff_factor", 0.0)),
        )

class AppLock:
    """Per-app runtime mutual exclusion file lock with timeout."""

    def __init__(self, app_name: str, lock_dir: Path | str = DEFAULT_LOCK_DIR, timeout: float = 5.0):
        if not APP_NAME_REGEX.match(app_name):
            raise ValueError(f"Invalid app name for lock: '{app_name}'")
        self.app_name = app_name
        self.lock_dir = Path(lock_dir)
        self.timeout = timeout
        self.lock_path = self.lock_dir / f"{app_name}.lock"
        self._fd: Optional[int] = None

    def __enter__(self) -> AppLock:
        self.acquire()
        return self

    def __exit__(self, exc_type, exc_val, exc_tb):
        self.release()

    def acquire(self) -> AppLock:
        self.lock_dir.mkdir(parents=True, exist_ok=True)
        self._fd = os.open(str(self.lock_path), os.O_RDWR | os.O_CREAT, 0o660)
        start_time = time.time()
        while True:
            try:
                if fcntl:
                    fcntl.flock(self._fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                elif sys.platform == "win32":
                    import msvcrt
                    msvcrt.locking(self._fd, msvcrt.LK_NBLCK, 1)
                return self
            except (BlockingIOError, OSError, IOError) as e:
                elapsed = time.time() - start_time
                if elapsed >= self.timeout:
                    if self._fd is not None:
                        try:
                            os.close(self._fd)
                        except OSError:
                            pass
                        self._fd = None
                    raise TimeoutError(f"AppLock timeout ({self.timeout}s) on {self.lock_path}: {e}")
                time.sleep(0.05)

    def release(self):
        if self._fd is not None:
            try:
                if fcntl:
                    fcntl.flock(self._fd, fcntl.LOCK_UN)
                elif sys.platform == "win32":
                    import msvcrt
                    try:
                        msvcrt.locking(self._fd, msvcrt.LK_UNLCK, 1)
                    except OSError:
                        pass
            except OSError:
                pass
            finally:
                try:
                    os.close(self._fd)
                except OSError:
                    pass
                self._fd = None

def make_initial_state(app_name: str) -> dict:
    return {
        "schema_version": 1,
        "app": app_name,
        "revision": 0,
        "active_slot": None,
        "operation_lease": None,
        "pending_route": None,
        "slots": {},
        "last_failover_time": None,
        "restarts_count": 0,
        "last_restart_time": None,
        "healthy_since": None,
        "degraded": False,
        "degraded_reason": None,
    }

def clean_stale_tmp_files(state_dir: Path | str = DEFAULT_STATE_DIR) -> None:
    """Cleans up orphan .tmp.* files left behind after ungraceful crashes between phases."""
    try:
        st_dir = Path(state_dir)
        if not st_dir.exists():
            return
        for tmp_file in st_dir.glob("*/*.tmp.*"):
            try:
                tmp_file.unlink()
                logger.info("Cleaned up orphan state tmp file: %s", tmp_file)
            except OSError:
                pass
    except Exception:
        pass

def load_state(app_name: str, state_dir: Path | str = DEFAULT_STATE_DIR) -> dict:
    target_file = Path(state_dir) / app_name / "state.json"
    if not target_file.exists():
        return make_initial_state(app_name)
    try:
        with open(target_file, "r", encoding="utf-8") as f:
            data = json.load(f)
            if not isinstance(data, dict):
                raise ValueError("State root must be a JSON object")
            return data
    except Exception as e:
        logger.error("State file %s corrupted: %s. Backing up and resetting state.", target_file, e)
        corrupt_backup = target_file.with_name(f"state.json.corrupt.{int(time.time())}")
        try:
            target_file.rename(corrupt_backup)
        except OSError:
            pass
        recovered = make_initial_state(app_name)
        recovered["degraded"] = True
        recovered["degraded_reason"] = f"Corrupt state file recovered: {e}"
        return recovered

def save_state_atomic(app_name: str, state: dict, state_dir: Path | str = DEFAULT_STATE_DIR) -> None:
    target_dir = Path(state_dir) / app_name
    target_dir.mkdir(parents=True, exist_ok=True)
    target_file = target_dir / "state.json"
    temp_file = target_dir / f"state.json.tmp.{os.getpid()}_{uuid.uuid4().hex}"

    content = json.dumps(state, indent=2, sort_keys=True)
    with open(temp_file, "w", encoding="utf-8") as f:
        f.write(content)
        f.flush()
        os.fsync(f.fileno())

    os.replace(temp_file, target_file)

    if hasattr(os, "O_DIRECTORY") and hasattr(os, "fsync"):
        try:
            dfd = os.open(str(target_dir), os.O_RDONLY | os.O_DIRECTORY)
            try:
                os.fsync(dfd)
            finally:
                os.close(dfd)
        except OSError:
            pass

def check_or_reap_lease(state: dict, now: float) -> Tuple[bool, str]:
    """
    Evaluates lease.
    Returns (is_active, message).
    If expired, mutates state dict to remove lease and returns False.
    """
    lease = state.get("operation_lease")
    if not lease:
        return False, ""
    expires_at = float(lease.get("expires_at", 0))
    if now >= expires_at:
        owner = lease.get("owner", "unknown")
        op = lease.get("operation", "unknown")
        state["operation_lease"] = None
        return False, f"Evicted stale lease (owner={owner}, op={op}, expired_at={expires_at})"
    return True, f"Active lease held by {lease.get('owner')} for {lease.get('operation')} until {expires_at}"

def get_deployment_journal_path() -> Path:
    env_path = os.environ.get("TX_JOURNAL_FILE")
    if env_path:
        return Path(env_path)
    local_path = Path(__file__).resolve().parent.parent.parent / "deploy" / "data" / "deploy-journal.json"
    if local_path.exists():
        return local_path
    return DEFAULT_JOURNAL_PATH

def check_deployment_in_progress(journal_path: Optional[Path] = None) -> Tuple[bool, str]:
    """
    Returns (is_in_progress, reason).
    Inhibits failover/promotion if a deploy transaction is active, uncommitted, or soaking.
    """
    path = journal_path or get_deployment_journal_path()
    if not path or not path.exists():
        return False, ""
    try:
        with open(path, "r", encoding="utf-8") as f:
            data = json.load(f)
        if not isinstance(data, dict):
            return False, ""
        state = data.get("state", "IDLE")
        tx_id = data.get("tx_id", "unknown")
        component = data.get("component", "unknown")
        active_states = {
            "TX_INITIALIZED",
            "CANDIDATE_STARTING",
            "VERIFYING_HEALTH",
            "SWITCHING_ROUTE",
            "VERIFYING_ACK",
            "TX_COMMITTED",
            "TX_SOAKING",
            "TX_ROLLING_BACK",
        }
        if state in active_states:
            return True, f"Deploy transaction {tx_id} ({component}) in progress (state={state})"
        return False, ""
    except Exception as e:
        logger.warning("Failed to parse deployment journal %s: %s", path, e)
        return False, ""

def is_intentional_stop(app_name: str, slot_or_container: str, state_dir: Path | str = DEFAULT_STATE_DIR) -> bool:
    """
    Checks canonical per-app state for an intentional stop marker.
    """
    st_dir = Path(state_dir)
    markers = [
        st_dir / app_name / f"intentional-stop-{slot_or_container}",
        st_dir / app_name / f".intentional-stop-{slot_or_container}",
    ]
    for m in markers:
        if m.exists():
            return True
    return False

class CommandRunner:
    """Fixed trusted command runner (never shell=True)."""

    def run(self, cmd: List[str], timeout: int = 15) -> Tuple[int, str, str]:
        try:
            res = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout)
            return res.returncode, res.stdout.strip(), res.stderr.strip()
        except Exception as e:
            return -1, "", str(e)

class DockerClient:
    """Interface to Docker CLI and Event Stream."""

    def __init__(self, runner: Optional[CommandRunner] = None):
        self.runner = runner or CommandRunner()

    def inspect_container(self, name_or_id: str) -> Optional[dict]:
        code, out, _ = self.runner.run(["docker", "inspect", name_or_id], timeout=10)
        if code != 0 or not out:
            return None
        try:
            data = json.loads(out)
            if isinstance(data, list) and data:
                return data[0]
        except Exception:
            pass
        return None

    def start_container(self, name_or_id: str) -> Tuple[int, str, str]:
        return self.runner.run(["docker", "start", name_or_id], timeout=20)

    def restart_container(self, name_or_id: str) -> Tuple[int, str, str]:
        return self.runner.run(["docker", "restart", "-t", "5", name_or_id], timeout=20)

    def exec_healthcheck(self, name_or_id: str, probe_cmd: Optional[List[str]] = None) -> bool:
        cmd = ["docker", "exec", name_or_id] + (probe_cmd or ["/gateway", "--healthcheck"])
        code, _, _ = self.runner.run(cmd, timeout=5)
        return code == 0

    def events_stream(self):
        """Yields JSON parsed event objects from docker events."""
        cmd = [
            "docker", "events",
            "--format", "{{json .}}",
            "--filter", "type=container",
            "--filter", "event=die",
            "--filter", "event=oom",
            "--filter", "event=health_status",
        ]
        proc = subprocess.Popen(cmd, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True)
        try:
            for line in proc.stdout:  # type: ignore
                line = line.strip()
                if not line:
                    continue
                try:
                    yield json.loads(line)
                except Exception:
                    continue
        finally:
            try:
                proc.kill()
            except Exception:
                pass

class SystemClock:
    def now(self) -> float:
        return time.time()

    def sleep(self, seconds: float):
        time.sleep(seconds)

class FailoverEngine:
    """
    Multi-app stateful failover and reconcile engine.
    Root-owned registry + canonical crash-safe state + runtime locking.
    Aware of deployment journal, intentional stops, and candidate vs committed states.
    """

    def __init__(
        self,
        registry_dir: Path | str = DEFAULT_REGISTRY_DIR,
        state_dir: Path | str = DEFAULT_STATE_DIR,
        lock_dir: Path | str = DEFAULT_LOCK_DIR,
        runner: Optional[CommandRunner] = None,
        docker_client: Optional[DockerClient] = None,
        clock: Optional[SystemClock] = None,
        journal_path: Optional[Path | str] = None,
        queue_maxsize: int = 500,
    ):
        self.registry_dir = Path(registry_dir)
        self.state_dir = Path(state_dir)
        self.lock_dir = Path(lock_dir)
        self.runner = runner or CommandRunner()
        self.docker_client = docker_client or DockerClient(self.runner)
        self.clock = clock or SystemClock()
        self.journal_path = Path(journal_path) if journal_path else None

        self.event_queue: queue.Queue = queue.Queue(maxsize=queue_maxsize)
        self._dirty_apps: Set[str] = set()
        self._queue_lock = threading.Lock()
        self.running = False
        self._stop_event = threading.Event()
        clean_stale_tmp_files(self.state_dir)

    def load_registry(self) -> Dict[str, AppConfig]:
        configs: Dict[str, AppConfig] = {}
        if not self.registry_dir.exists():
            return configs
        for path in self.registry_dir.glob("*.json"):
            app_name = path.stem
            try:
                with open(path, "r", encoding="utf-8") as f:
                    data = json.load(f)
                cfg = AppConfig.from_dict(data, app_name)
                configs[app_name] = cfg
            except Exception as e:
                logger.error("Failed to load registry config %s: %s", path, e)
        return configs

    def get_app_config(self, app_name: str) -> Optional[AppConfig]:
        path = self.registry_dir / f"{app_name}.json"
        if not path.exists():
            return None
        try:
            with open(path, "r", encoding="utf-8") as f:
                data = json.load(f)
            return AppConfig.from_dict(data, app_name)
        except Exception as e:
            logger.error("Failed to parse app config %s: %s", path, e)
            return None

    def execute_switch(self, app_cfg: AppConfig, target_slot: str) -> bool:
        """Executes fixed trusted switch command without shell interpolation."""
        if target_slot not in app_cfg.slots:
            logger.error("Refusing switch: target_slot '%s' not registered for app '%s'", target_slot, app_cfg.app)
            return False

        if app_cfg.switch_cmd:
            cmd = [arg.replace("{app}", app_cfg.app).replace("{slot}", target_slot) for arg in app_cfg.switch_cmd]
        else:
            cmd = ["/usr/local/bin/platform-switch", app_cfg.app, target_slot]

        # Path validation: executable must be absolute and within trusted prefixes
        bin_path = cmd[0]
        if not any(bin_path.startswith(prefix) for prefix in TRUSTED_PATH_PREFIXES):
            logger.error("Untrusted switch executable '%s' for app '%s'", bin_path, app_cfg.app)
            return False

        logger.info("Executing trusted switch: %s", " ".join(cmd))
        code, out, err = self.runner.run(cmd, timeout=30)
        if code == 0:
            logger.info("Switch succeeded for app %s to slot %s: %s", app_cfg.app, target_slot, out)
            return True
        logger.error("Switch failed for app %s to slot %s (code=%d): %s %s", app_cfg.app, target_slot, code, out, err)
        return False

    def handle_failover(
        self,
        app_name: str,
        event_slot: str,
        event_container_id: str,
        event_action: str,
        event_time: float = 0.0,
    ) -> None:
        """
        Active slot event evaluation under lock.
        Validates container id/generation, lease, cooldown, and deployment journal.
        Inhibits failover if a deployment transaction is active or candidate is uncommitted.
        """
        app_cfg = self.get_app_config(app_name)
        if not app_cfg:
            logger.warning("No registry config found for app '%s'. Ignoring event.", app_name)
            return

        try:
            with contextlib.ExitStack() as locks:
                locks.enter_context(AppLock("acb", self.lock_dir))
                if app_name != "acb":
                    locks.enter_context(AppLock(app_name, self.lock_dir))
                state = load_state(app_name, self.state_dir)
                now = self.clock.now()

                # 1. Deployment Journal check (no promotion during active transaction)
                in_progress, reason = check_deployment_in_progress(self.journal_path)
                if in_progress:
                    logger.info("App %s: deployment transaction in progress (%s). Skipping failover/promotion.", app_name, reason)
                    return

                # 2. Check operation lease
                is_active, lease_msg = check_or_reap_lease(state, now)
                if is_active:
                    logger.info("App %s: active operation lease in effect (%s). Skipping failover.", app_name, lease_msg)
                    save_state_atomic(app_name, state, self.state_dir)
                    return

                if state.get("operation_lease") is None and lease_msg:
                    # Stale lease was reaped; persist state
                    save_state_atomic(app_name, state, self.state_dir)

                # 3. Singleton workload
                if app_cfg.workload_class == "singleton":
                    self._handle_singleton_failure(app_cfg, state, event_container_id, now, event_time)
                    return

                # 4. Blue/Green workload
                active_slot = state.get("active_slot")
                if not active_slot:
                    logger.warning("App %s: active slot is missing/unknown. Safe degraded mode, no default blue.", app_name)
                    state["degraded"] = True
                    state["degraded_reason"] = "Active slot missing or unknown; failover aborted without default"
                    save_state_atomic(app_name, state, self.state_dir)
                    return

                # 5. Intentional stop check on event slot
                if is_intentional_stop(app_name, event_slot, self.state_dir):
                    logger.info(
                        "App %s: event on slot '%s' ignored due to intentional stop marker.",
                        app_name,
                        event_slot,
                    )
                    if "slots" not in state:
                        state["slots"] = {}
                    if event_slot not in state["slots"]:
                        state["slots"][event_slot] = {}
                    state["slots"][event_slot]["status"] = "stopped"
                    state["slots"][event_slot]["last_transition"] = now
                    save_state_atomic(app_name, state, self.state_dir)
                    return

                # 6. Intentional standby stopped check:
                # Only events affecting active slot trigger failover!
                if event_slot != active_slot:
                    logger.info(
                        "App %s: event on slot '%s' ignored (active slot is '%s'). Standby stopped/transition is normal.",
                        app_name,
                        event_slot,
                        active_slot,
                    )
                    if "slots" not in state:
                        state["slots"] = {}
                    if event_slot not in state["slots"]:
                        state["slots"][event_slot] = {}
                    state["slots"][event_slot]["status"] = "stopped"
                    state["slots"][event_slot]["last_transition"] = now
                    save_state_atomic(app_name, state, self.state_dir)
                    return

                # 7. Active slot event -> validate container ID / generation
                recorded_id = state.get("slots", {}).get(active_slot, {}).get("container_id")
                if recorded_id and event_container_id and recorded_id != event_container_id:
                    logger.warning(
                        "App %s: stale event on active slot '%s' (event id %s != recorded active id %s). Ignoring.",
                        app_name,
                        active_slot,
                        event_container_id,
                        recorded_id,
                    )
                    return

                # 8. Verify active container actual health status
                active_slot_cfg = app_cfg.slots.get(active_slot)
                if not active_slot_cfg:
                    state["degraded"] = True
                    state["degraded_reason"] = f"Active slot {active_slot} not defined in registry"
                    save_state_atomic(app_name, state, self.state_dir)
                    return

                inspect = self.docker_client.inspect_container(active_slot_cfg.container_name)
                if inspect:
                    c_state = inspect.get("State", {})
                    status = c_state.get("Status", "")
                    health = c_state.get("Health", {}).get("Status", "")
                    if status == "running" and (health == "healthy" or not health):
                        logger.info("App %s: container %s is running and healthy. Spurious event ignored.", app_name, active_slot_cfg.container_name)
                        return

                # 9. Cooldown check
                last_failover = state.get("last_failover_time")
                if last_failover and (now - float(last_failover)) < app_cfg.cooldown_seconds:
                    elapsed = now - float(last_failover)
                    logger.warning(
                        "App %s: active slot failed but failover blocked by cooldown (elapsed %.1fs < %ds).",
                        app_name,
                        elapsed,
                        app_cfg.cooldown_seconds,
                    )
                    state["degraded"] = True
                    state["degraded_reason"] = f"Active slot failed within cooldown ({elapsed:.0f}s < {app_cfg.cooldown_seconds}s)"
                    save_state_atomic(app_name, state, self.state_dir)
                    return

                # 10. Determine peer standby slot
                candidate_slots = [s for s in app_cfg.slots.keys() if s != active_slot]
                if not candidate_slots:
                    state["degraded"] = True
                    state["degraded_reason"] = "No standby slot configured in registry"
                    save_state_atomic(app_name, state, self.state_dir)
                    return
                target_slot = candidate_slots[0]
                target_slot_cfg = app_cfg.slots[target_slot]

                logger.critical(
                    "App %s: Active container %s failed. Activating warm standby slot %s (%s)...",
                    app_name,
                    active_slot_cfg.container_name,
                    target_slot,
                    target_slot_cfg.container_name,
                )

                # 11. Start standby container
                code, out, err = self.docker_client.start_container(target_slot_cfg.container_name)
                if code != 0:
                    logger.critical("App %s: Both slots degraded! Failed to start standby %s: %s %s", app_name, target_slot_cfg.container_name, out, err)
                    state["degraded"] = True
                    state["degraded_reason"] = f"Both slots degraded: active failed, standby {target_slot_cfg.container_name} start failed"
                    save_state_atomic(app_name, state, self.state_dir)
                    return

                # 12. Readiness probe
                ready = False
                for _ in range(app_cfg.health_timeout):
                    p_inspect = self.docker_client.inspect_container(target_slot_cfg.container_name)
                    if p_inspect:
                        p_state = p_inspect.get("State", {})
                        if p_state.get("Status") == "running":
                            health_stat = p_state.get("Health", {}).get("Status")
                            if health_stat == "healthy":
                                ready = True
                                break
                            if not health_stat and self.docker_client.exec_healthcheck(target_slot_cfg.container_name):
                                ready = True
                                break
                            if "Health" not in p_state:
                                ready = True
                                break
                    self.clock.sleep(1)

                if not ready:
                    logger.critical("App %s: Both slots degraded! Standby container %s failed readiness check within %ds", app_name, target_slot_cfg.container_name, app_cfg.health_timeout)
                    self.runner.run(["docker", "stop", "-t", "5", target_slot_cfg.container_name], timeout=10)
                    state["degraded"] = True
                    state["degraded_reason"] = f"Both slots degraded: active failed, standby {target_slot_cfg.container_name} readiness timeout"
                    save_state_atomic(app_name, state, self.state_dir)
                    return

                # 13. Perform atomic switch with exact route identity ACK
                if not self.execute_switch(app_cfg, target_slot):
                    logger.error("App %s: Route switch to slot %s failed. Entering degraded mode and rolling back standby container.", app_name, target_slot)
                    self.runner.run(["docker", "stop", "-t", "5", target_slot_cfg.container_name], timeout=10)
                    state["degraded"] = True
                    state["degraded_reason"] = f"Route switch or identity ACK failed for slot {target_slot}"
                    if "slots" not in state:
                        state["slots"] = {}
                    if target_slot not in state["slots"]:
                        state["slots"][target_slot] = {}
                    state["slots"][target_slot]["status"] = "failed"
                    save_state_atomic(app_name, state, self.state_dir)
                    return

                # 14. Update canonical state
                target_inspect = self.docker_client.inspect_container(target_slot_cfg.container_name)
                state["active_slot"] = target_slot
                state["last_failover_time"] = now
                state["degraded"] = False
                state["degraded_reason"] = None
                if "slots" not in state:
                    state["slots"] = {}
                state["slots"][target_slot] = {
                    "container_id": target_inspect.get("Id", "") if target_inspect else "",
                    "image_digest": target_inspect.get("Image", "") if target_inspect else "",
                    "status": "running",
                    "health": "healthy",
                    "last_transition": now,
                }
                if active_slot in state["slots"]:
                    state["slots"][active_slot]["status"] = "failed"
                    state["slots"][active_slot]["last_transition"] = now

                save_state_atomic(app_name, state, self.state_dir)
                logger.info("Failover completed successfully. App %s active slot is now %s.", app_name, target_slot)

        except TimeoutError as e:
            logger.warning("App %s: mutual host lock contention (%s). Skipping failover safely.", app_name, e)
            return

    def _handle_singleton_failure(
        self,
        app_cfg: AppConfig,
        state: dict,
        event_container_id: str,
        now: float,
        event_time: float = 0.0,
    ) -> None:
        """Restart the configured singleton only after fencing stale events and current health."""
        app_name = app_cfg.app
        c_name = app_cfg.container_name

        in_progress, reason = check_deployment_in_progress(self.journal_path)
        if in_progress:
            logger.info("App %s: deployment in progress (%s). Skipping singleton restart.", app_name, reason)
            return

        if is_intentional_stop(app_name, "singleton", self.state_dir) or is_intentional_stop(app_name, c_name, self.state_dir):
            logger.info("App %s: singleton container %s was intentionally stopped. Skipping restart.", app_name, c_name)
            return

        inspect = self.docker_client.inspect_container(c_name)
        if inspect:
            current_id = str(inspect.get("Id", ""))
            container_state = inspect.get("State", {})
            status = str(container_state.get("Status", ""))
            health = str(container_state.get("Health", {}).get("Status", ""))

            if event_container_id and current_id and event_container_id != current_id:
                logger.info(
                    "App %s: ignoring stale singleton event for container %s; current container is %s.",
                    app_name,
                    event_container_id,
                    current_id,
                )
                return
            if status == "running" and health == "healthy":
                logger.info("App %s: singleton recovered and is healthy; ignoring stale failure event.", app_name)
                return
            started_at = str(container_state.get("StartedAt", ""))
            if event_time and started_at:
                try:
                    started_epoch = datetime.fromisoformat(started_at.replace("Z", "+00:00")).timestamp()
                    if started_epoch > event_time:
                        logger.info("App %s: singleton started after the failure event; ignoring stale event.", app_name)
                        return
                except ValueError:
                    logger.warning("App %s: Docker returned an invalid StartedAt timestamp: %s", app_name, started_at)
            if status == "restarting" or (status == "running" and health == "starting"):
                logger.info("App %s: singleton is %s/%s; Docker recovery is still in progress.", app_name, status, health or "unknown")
                return

        restarts = int(state.get("restarts_count", 0))
        last_attempt = state.get("last_restart_attempt_time", state.get("last_restart_time"))
        factor = app_cfg.backoff_factor if app_cfg.backoff_factor > 0 else 2.0
        base = app_cfg.min_restart_interval if app_cfg.min_restart_interval > 0 else 5.0
        backoff = min(base * (factor ** max(0, restarts - 1)), float(app_cfg.cooldown_seconds))
        if restarts < app_cfg.max_restarts and last_attempt and (now - float(last_attempt)) < backoff:
            logger.warning(
                "App %s: singleton restart attempt in backoff (elapsed %.1fs < %.1fs).",
                app_name,
                now - float(last_attempt),
                backoff,
            )
            return

        if restarts >= app_cfg.max_restarts:
            logger.critical(
                "App %s: singleton container %s exceeded max acknowledged restarts (%d). Marked degraded without duplication.",
                app_name,
                c_name,
                app_cfg.max_restarts,
            )
            state["degraded"] = True
            state["degraded_reason"] = f"Singleton {c_name} exceeded max restarts ({app_cfg.max_restarts})"
            save_state_atomic(app_name, state, self.state_dir)
            return

        state["restart_attempts"] = int(state.get("restart_attempts", 0)) + 1
        state["last_restart_attempt_time"] = now
        save_state_atomic(app_name, state, self.state_dir)
        logger.warning(
            "App %s: singleton container %s failed. Requesting bounded emergency restart (%d/%d)...",
            app_name,
            c_name,
            restarts + 1,
            app_cfg.max_restarts,
        )
        code, _out, err = self.docker_client.restart_container(c_name)
        if code != 0:
            state["degraded"] = True
            state["degraded_reason"] = f"Singleton {c_name} restart request failed: {err or 'docker error'}"
            save_state_atomic(app_name, state, self.state_dir)
            logger.error("App %s: singleton restart request failed: %s", app_name, err or "docker error")
            return

        state["restarts_count"] = restarts + 1
        state["last_restart_time"] = now
        state["degraded"] = False
        state["degraded_reason"] = None
        save_state_atomic(app_name, state, self.state_dir)

    def reconcile_app(self, app_name: str) -> None:
        """Reconciles canonical state with reality for a single app."""
        app_cfg = self.get_app_config(app_name)
        if not app_cfg:
            return

        is_active_healthy = True
        cid = ""
        active_slot = None

        try:
            with contextlib.ExitStack() as locks:
                locks.enter_context(AppLock("acb", self.lock_dir))
                if app_name != "acb":
                    locks.enter_context(AppLock(app_name, self.lock_dir))
                state = load_state(app_name, self.state_dir)
                now = self.clock.now()

                # Check deployment journal
                in_progress, reason = check_deployment_in_progress(self.journal_path)
                if in_progress:
                    logger.info("Reconcile: App %s skipped (deployment in progress: %s)", app_name, reason)
                    save_state_atomic(app_name, state, self.state_dir)
                    return

                is_active, lease_msg = check_or_reap_lease(state, now)
                if is_active:
                    logger.info("Reconcile: App %s skipped (active lease: %s)", app_name, lease_msg)
                    save_state_atomic(app_name, state, self.state_dir)
                    return

                if state.get("operation_lease") is None and lease_msg:
                    save_state_atomic(app_name, state, self.state_dir)

                if app_cfg.workload_class == "singleton":
                    insp = self.docker_client.inspect_container(app_cfg.container_name)
                    status = insp.get("State", {}).get("Status") if insp else "missing"
                    health = insp.get("State", {}).get("Health", {}).get("Status", "") if insp else ""
                    if status != "running" or health != "healthy":
                        self._handle_singleton_failure(app_cfg, state, insp.get("Id", "") if insp else "", now)
                    else:
                        healthy_since = state.get("healthy_since")
                        previous_slot = state.get("slots", {}).get("singleton", {})
                        same_container = previous_slot.get("container_id") == (insp.get("Id", "") if insp else "")
                        if health == "healthy":
                            if not healthy_since or not same_container:
                                state["healthy_since"] = now
                            elif now - float(healthy_since) >= max(app_cfg.cooldown_seconds, 60):
                                state["restarts_count"] = 0
                                state["last_restart_time"] = None
                                state["degraded"] = False
                                state["degraded_reason"] = None
                        else:
                            state["healthy_since"] = None
                        state["slots"] = {
                            "singleton": {
                                "container_id": insp.get("Id", "") if insp else "",
                                "status": "running",
                                "health": health,
                                "last_transition": now,
                            }
                        }
                        save_state_atomic(app_name, state, self.state_dir)
                    return

                # Blue/Green reconcile
                active_slot = state.get("active_slot")
                if not active_slot:
                    # Check running containers
                    running_healthy = []
                    for s_name, s_cfg in app_cfg.slots.items():
                        insp = self.docker_client.inspect_container(s_cfg.container_name)
                        if insp:
                            c_st = insp.get("State", {})
                            if c_st.get("Status") == "running" and c_st.get("Health", {}).get("Status") != "unhealthy":
                                running_healthy.append((s_name, insp))

                    if len(running_healthy) == 1:
                        chosen_slot, insp = running_healthy[0]
                        state["active_slot"] = chosen_slot
                        state["degraded"] = False
                        state["degraded_reason"] = None
                        if "slots" not in state:
                            state["slots"] = {}
                        state["slots"][chosen_slot] = {
                            "container_id": insp.get("Id", ""),
                            "image_digest": insp.get("Image", ""),
                            "status": "running",
                            "health": "healthy",
                            "last_transition": now,
                        }
                        save_state_atomic(app_name, state, self.state_dir)
                        logger.info("Reconcile: Identified running active slot %s for app %s", chosen_slot, app_name)
                    else:
                        # Missing active: safe degraded mode, NEVER default to blue
                        state["degraded"] = True
                        if len(running_healthy) == 0:
                            state["degraded_reason"] = "Missing active slot: both slots degraded (0 healthy candidates running)"
                        else:
                            state["degraded_reason"] = f"Missing active slot: ambiguous ({len(running_healthy)} healthy candidates running)"
                        save_state_atomic(app_name, state, self.state_dir)
                        logger.warning("Reconcile: Active slot issue for app %s (%s). Degraded mode entered, no default blue.", app_name, state["degraded_reason"])
                        return
                    active_slot = state.get("active_slot")

                active_cfg = app_cfg.slots.get(active_slot)
                if not active_cfg:
                    state["degraded"] = True
                    state["degraded_reason"] = f"Active slot {active_slot} not configured in registry"
                    save_state_atomic(app_name, state, self.state_dir)
                    return

                # Check intentional stop marker on active slot
                if is_intentional_stop(app_name, active_slot, self.state_dir):
                    logger.info("Reconcile: Active slot %s for app %s was intentionally stopped. Skipping failover.", active_slot, app_name)
                    if "slots" not in state:
                        state["slots"] = {}
                    if active_slot not in state["slots"]:
                        state["slots"][active_slot] = {}
                    state["slots"][active_slot]["status"] = "stopped"
                    save_state_atomic(app_name, state, self.state_dir)
                    return

                active_insp = self.docker_client.inspect_container(active_cfg.container_name)
                is_active_healthy = (
                    active_insp is not None
                    and active_insp.get("State", {}).get("Status") == "running"
                    and active_insp.get("State", {}).get("Health", {}).get("Status") != "unhealthy"
                )

                if is_active_healthy:
                    # Active is healthy. Verify standby slot:
                    # Standby stopped is valid in warm standby!
                    if "slots" not in state:
                        state["slots"] = {}
                    state["slots"][active_slot] = {
                        "container_id": active_insp.get("Id", ""),
                        "image_digest": active_insp.get("Image", ""),
                        "status": "running",
                        "health": active_insp.get("State", {}).get("Health", {}).get("Status", "healthy"),
                        "last_transition": now,
                    }
                    for s_name, s_cfg in app_cfg.slots.items():
                        if s_name == active_slot:
                            continue
                        standby_insp = self.docker_client.inspect_container(s_cfg.container_name)
                        standby_status = standby_insp.get("State", {}).get("Status") if standby_insp else "stopped"
                        state["slots"][s_name] = {
                            "container_id": standby_insp.get("Id", "") if standby_insp else "",
                            "image_digest": standby_insp.get("Image", "") if standby_insp else "",
                            "status": standby_status,
                            "health": standby_insp.get("State", {}).get("Health", {}).get("Status", "") if standby_insp else "",
                            "last_transition": now,
                        }
                    save_state_atomic(app_name, state, self.state_dir)
                else:
                    logger.warning("Reconcile: Active slot container %s is unhealthy/stopped for app %s.", active_cfg.container_name, app_name)
                    cid = active_insp.get("Id", "") if active_insp else ""

        except TimeoutError as e:
            logger.warning("Reconcile: mutual host lock contention on app %s (%s). Skipping safely.", app_name, e)
            return

        if not is_active_healthy and active_slot:
            # Active container unhealthy: execute failover
            self.handle_failover(app_name, active_slot, cid, "reconcile_failure")

    def reconcile_all(self) -> None:
        """Initial / periodic reconcile across all registered apps."""
        configs = self.load_registry()
        for app_name in configs:
            try:
                self.reconcile_app(app_name)
            except Exception as e:
                logger.error("Reconcile failed for app %s: %s", app_name, e)

    def ingest_docker_event(self, event: dict) -> bool:
        """
        Decoupled bounded ingestion.
        Returns True if enqueued, False if dropped due to queue full (triggers dirty reconcile).
        """
        action = event.get("Action", "")
        actor = event.get("Actor", {})
        attributes = actor.get("Attributes", {})
        container_name = attributes.get("name", "")
        container_id = actor.get("ID", "")

        # Quick action filter
        if not (action in ("die", "oom") or action.startswith("health_status: unhealthy")):
            return True

        # Match container against registered apps
        configs = self.load_registry()
        matched_app: Optional[str] = None
        matched_slot: str = ""

        # Validate against registry config; NEVER trust arbitrary labels
        for app_name, app_cfg in configs.items():
            if app_cfg.workload_class == "singleton":
                if app_cfg.container_name == container_name:
                    matched_app = app_name
                    matched_slot = "singleton"
                    break
            else:
                for slot_name, slot_cfg in app_cfg.slots.items():
                    if slot_cfg.container_name == container_name:
                        matched_app = app_name
                        matched_slot = slot_name
                        break
                if matched_app:
                    break

        if not matched_app:
            return True

        try:
            event_time = float(event.get("timeNano", 0)) / 1_000_000_000 if event.get("timeNano") else float(event.get("time", 0) or 0)
        except (TypeError, ValueError):
            event_time = 0.0

        payload = {
            "app": matched_app,
            "slot": matched_slot,
            "container_name": container_name,
            "container_id": container_id,
            "action": action,
            "event_time": event_time,
        }

        try:
            self.event_queue.put_nowait(payload)
            return True
        except queue.Full:
            logger.warning(
                "Event queue full! Dropped event for app %s (container %s). Marking app for dirty reconcile.",
                matched_app,
                container_name,
            )
            with self._queue_lock:
                self._dirty_apps.add(matched_app)
            return False

    def process_event_batch(self, timeout: float = 1.0) -> bool:
        """
        Dequeues and coalesces events per app, executing failover or dirty reconcile.
        Returns True if work was processed, False if queue idle.
        """
        # Check dirty apps first
        dirty_app: Optional[str] = None
        with self._queue_lock:
            if self._dirty_apps:
                dirty_app = self._dirty_apps.pop()

        if dirty_app:
            logger.info("Processing dirty reconcile for app %s due to queue overflow", dirty_app)
            self.reconcile_app(dirty_app)
            return True

        try:
            item = self.event_queue.get(timeout=timeout)
        except queue.Empty:
            return False

        app = item["app"]
        coalesced = [item]

        # Coalesce any other pending events for the same app
        others: List[dict] = []
        while not self.event_queue.empty():
            try:
                nxt = self.event_queue.get_nowait()
                if nxt["app"] == app:
                    coalesced.append(nxt)
                else:
                    others.append(nxt)
            except queue.Empty:
                break

        # Put back events for other apps
        for other in others:
            try:
                self.event_queue.put_nowait(other)
            except queue.Full:
                with self._queue_lock:
                    self._dirty_apps.add(other["app"])

        # Execute failover evaluation on latest coalesced event
        latest = coalesced[-1]
        self.handle_failover(
            app_name=latest["app"],
            event_slot=latest["slot"],
            event_container_id=latest["container_id"],
            event_action=latest["action"],
            event_time=latest.get("event_time", 0.0),
        )
        return True

    def run_event_stream_loop(self, backoff_init: float = 1.0, backoff_max: float = 30.0):
        """Streams Docker events with reconnect EOF backoff and reconnect reconcile."""
        backoff = backoff_init
        while self.running:
            try:
                stream = self.docker_client.events_stream()
                logger.info("Connected to Docker event stream. Performing reconnect reconcile...")
                self.reconcile_all()
                backoff = backoff_init

                for event in stream:
                    if not self.running:
                        break
                    self.ingest_docker_event(event)
            except EOFError as e:
                logger.warning("Docker event stream encountered EOF (%s). Reconnecting in %.1fs...", e, backoff)
            except Exception as e:
                logger.error("Docker event stream error: %s. Reconnecting in %.1fs...", e, backoff)

            if self.running:
                self.clock.sleep(backoff)
                backoff = min(backoff * 2, backoff_max)

    def run_periodic_reconcile_loop(self, interval_seconds: int = 60):
        """Safety net periodic reconcile scan."""
        while self.running:
            self.clock.sleep(interval_seconds)
            if self.running:
                logger.info("Executing periodic safety net reconcile scan...")
                self.reconcile_all()

    def start_daemon(self):
        """Starts engine with initial reconcile, event stream, and worker thread."""
        self.running = True
        logger.info("VPS Failover Engine starting. Running initial reconcile...")
        self.reconcile_all()

        t_events = threading.Thread(target=self.run_event_stream_loop, daemon=True)
        t_reconcile = threading.Thread(target=self.run_periodic_reconcile_loop, daemon=True)
        t_events.start()
        t_reconcile.start()

        logger.info("VPS Failover Engine running. Listening for events and servicing queue...")
        while self.running:
            self.process_event_batch(timeout=1.0)

    def stop(self):
        self.running = False
        self._stop_event.set()

def parse_args():
    parser = argparse.ArgumentParser(description="VPS Multi-App Unified Failover Controller")
    parser.add_argument("--reconcile", action="store_true", help="Run one-shot safety net reconcile and exit")
    parser.add_argument("--reconcile-app", type=str, default="", help="Run one-shot reconcile for specific app and exit")
    parser.add_argument("--status", action="store_true", help="Print canonical state JSON for all registered apps")
    parser.add_argument("--registry-dir", type=str, default=str(DEFAULT_REGISTRY_DIR))
    parser.add_argument("--state-dir", type=str, default=str(DEFAULT_STATE_DIR))
    parser.add_argument("--lock-dir", type=str, default=str(DEFAULT_LOCK_DIR))
    parser.add_argument("--journal-path", type=str, default=None, help="Path to deployment transaction journal")
    return parser.parse_args()

def main():
    args = parse_args()
    engine = FailoverEngine(
        registry_dir=args.registry_dir,
        state_dir=args.state_dir,
        lock_dir=args.lock_dir,
        journal_path=args.journal_path,
    )

    if args.status:
        configs = engine.load_registry()
        result = {}
        for app in configs:
            result[app] = load_state(app, args.state_dir)
        print(json.dumps(result, indent=2))
        return

    if args.reconcile_app:
        engine.reconcile_app(args.reconcile_app)
        return

    if args.reconcile:
        engine.reconcile_all()
        return

    def sig_handler(sig, frame):
        logger.info("Terminating VPS Failover Controller...")
        engine.stop()
        sys.exit(0)

    signal.signal(signal.SIGINT, sig_handler)
    signal.signal(signal.SIGTERM, sig_handler)

    engine.start_daemon()

if __name__ == "__main__":
    main()
