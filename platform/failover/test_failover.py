"""
Unit test suite for VPS Multi-App Failover Engine.
Uses fake docker, fake clock, fake fs, and fake runner (no real Docker daemon).
Covers:
1. manual standby stop
2. stale event
3. two apps
4. cooldown
5. missing active
6. queue full dirty reconcile
7. EOF & backoff reconnect
8. crash-safe state & CAS
9. singleton bounded restart
10. stale deploy lease
11. trusted switch command interface
"""

import importlib.util
import json
import os
import re
import shutil
import tempfile
from pathlib import Path
from typing import Dict, List, Optional, Tuple

import pytest

# Dynamically import vps-failover-controller.py
module_name = "vps_failover_controller"
spec = importlib.util.spec_from_file_location(
    module_name,
    Path(__file__).parent / "vps-failover-controller.py"
)
ctrl = importlib.util.module_from_spec(spec)
import sys
sys.modules[module_name] = ctrl
spec.loader.exec_module(ctrl)


class FakeClock:
    def __init__(self, start_time: float = 10000.0):
        self._time = start_time

    def now(self) -> float:
        return self._time

    def advance(self, seconds: float):
        self._time += seconds

    def sleep(self, seconds: float):
        self._time += seconds


class FakeRunner:
    def __init__(self):
        self.history: List[List[str]] = []
        self.responses: Dict[Tuple[str, ...], Tuple[int, str, str]] = {}
        self.default_response: Tuple[int, str, str] = (0, "ok", "")

    def run(self, cmd: List[str], timeout: int = 15) -> Tuple[int, str, str]:
        self.history.append(list(cmd))
        key = tuple(cmd)
        if key in self.responses:
            return self.responses[key]
        return self.default_response


class FakeDockerClient:
    def __init__(self, runner: Optional[FakeRunner] = None):
        self.runner = runner or FakeRunner()
        self.containers: Dict[str, dict] = {}
        self.started: List[str] = []
        self.restarted: List[str] = []
        self.stream_generator = None

    def add_container(
        self,
        name: str,
        cid: str,
        status: str = "running",
        health: str = "healthy",
        image: str = "sha256:dummy",
    ):
        self.containers[name] = {
            "Id": cid,
            "Name": f"/{name}",
            "Image": image,
            "State": {
                "Status": status,
                "Health": {"Status": health} if health else {},
            },
        }

    def inspect_container(self, name_or_id: str) -> Optional[dict]:
        if name_or_id in self.containers:
            return self.containers[name_or_id]
        for c in self.containers.values():
            if c["Id"] == name_or_id:
                return c
        return None

    def start_container(self, name_or_id: str) -> Tuple[int, str, str]:
        self.started.append(name_or_id)
        if name_or_id in self.containers:
            self.containers[name_or_id]["State"]["Status"] = "running"
            self.containers[name_or_id]["State"]["Health"] = {"Status": "healthy"}
        return 0, "started", ""

    def restart_container(self, name_or_id: str) -> Tuple[int, str, str]:
        self.restarted.append(name_or_id)
        if name_or_id in self.containers:
            self.containers[name_or_id]["State"]["Status"] = "running"
        return 0, "restarted", ""

    def exec_healthcheck(self, name_or_id: str, probe_cmd: Optional[List[str]] = None) -> bool:
        return True

    def events_stream(self):
        if self.stream_generator:
            yield from self.stream_generator()


@pytest.fixture
def env():
    temp_dir = tempfile.mkdtemp(prefix="test_failover_")
    reg_dir = Path(temp_dir) / "apps.d"
    st_dir = Path(temp_dir) / "apps"
    lk_dir = Path(temp_dir) / "lock"
    reg_dir.mkdir(parents=True)
    st_dir.mkdir(parents=True)
    lk_dir.mkdir(parents=True)

    clock = FakeClock(1000.0)
    runner = FakeRunner()
    docker = FakeDockerClient(runner)

    engine = ctrl.FailoverEngine(
        registry_dir=reg_dir,
        state_dir=st_dir,
        lock_dir=lk_dir,
        runner=runner,
        docker_client=docker,
        clock=clock,
    )

    yield {
        "engine": engine,
        "reg_dir": reg_dir,
        "st_dir": st_dir,
        "lk_dir": lk_dir,
        "clock": clock,
        "runner": runner,
        "docker": docker,
        "temp_dir": temp_dir,
    }

    shutil.rmtree(temp_dir, ignore_errors=True)


def create_app_registry(reg_dir: Path, app: str, slots: dict = None, switch_cmd: list = None, **kwargs) -> Path:
    if slots is None:
        slots = {
            "blue": {"container_name": f"{app}-web-blue"},
            "green": {"container_name": f"{app}-web-green"},
        }
    data = {
        "app": app,
        "workload_class": kwargs.get("workload_class", "blue_green"),
        "slots": slots,
        "cooldown_seconds": kwargs.get("cooldown_seconds", 300),
        "max_restarts": kwargs.get("max_restarts", 3),
        "switch_cmd": switch_cmd or ["/usr/local/bin/platform-switch", "{app}", "{slot}"],
    }
    if kwargs.get("workload_class") == "singleton":
        data["container_name"] = kwargs.get("container_name", app)
        data.pop("slots", None)

    p = reg_dir / f"{app}.json"
    p.write_text(json.dumps(data), encoding="utf-8")
    return p


# 1. Manual Standby Stop
def test_manual_standby_stop(env):
    """Standby stopped intentionally: controller leaves standby stopped, no failover."""
    create_app_registry(env["reg_dir"], "acb")
    env["docker"].add_container("acb-web-blue", "cid-blue-1", status="running", health="healthy")
    env["docker"].add_container("acb-web-green", "cid-green-1", status="exited", health="")

    # Set initial state
    state = ctrl.load_state("acb", env["st_dir"])
    state["active_slot"] = "blue"
    state["slots"] = {
        "blue": {"container_id": "cid-blue-1", "status": "running"},
        "green": {"container_id": "cid-green-1", "status": "running"},
    }
    ctrl.save_state_atomic("acb", state, env["st_dir"])

    # Simulate manual standby stop: docker emits die event for green
    event = {
        "Action": "die",
        "Actor": {
            "ID": "cid-green-1",
            "Attributes": {"name": "acb-web-green"},
        },
    }
    env["engine"].ingest_docker_event(event)
    env["engine"].process_event_batch()

    # Verify: active remains blue, green was NOT restarted, no switch executed
    post_state = ctrl.load_state("acb", env["st_dir"])
    assert post_state["active_slot"] == "blue"
    assert post_state["degraded"] is False
    assert post_state["slots"]["green"]["status"] == "stopped"
    assert "acb-web-green" not in env["docker"].started
    assert len(env["runner"].history) == 0


# 2. Stale Event
def test_stale_event(env):
    """Event from old container generation is rejected after lock check."""
    create_app_registry(env["reg_dir"], "acb")
    # Current active is cid-blue-v2
    env["docker"].add_container("acb-web-blue", "cid-blue-v2", status="running", health="healthy")
    env["docker"].add_container("acb-web-green", "cid-green-v2", status="stopped", health="")

    state = ctrl.load_state("acb", env["st_dir"])
    state["active_slot"] = "blue"
    state["slots"] = {
        "blue": {"container_id": "cid-blue-v2", "status": "running"},
    }
    ctrl.save_state_atomic("acb", state, env["st_dir"])

    # Event arrives for an OLD container cid-blue-v1
    event = {
        "Action": "die",
        "Actor": {
            "ID": "cid-blue-v1",
            "Attributes": {"name": "acb-web-blue"},
        },
    }
    env["engine"].ingest_docker_event(event)
    env["engine"].process_event_batch()

    # Active remains blue, no failover performed
    post_state = ctrl.load_state("acb", env["st_dir"])
    assert post_state["active_slot"] == "blue"
    assert "acb-web-green" not in env["docker"].started
    assert len(env["runner"].history) == 0


# 3. Two Apps
def test_two_apps_independent_failover(env):
    """Events for app1 and app2 processed independently without starvation."""
    create_app_registry(env["reg_dir"], "acb")
    create_app_registry(env["reg_dir"], "bark", slots={
        "blue": {"container_name": "bark-blue"},
        "green": {"container_name": "bark-green"},
    })

    # Both start on blue
    env["docker"].add_container("acb-web-blue", "cid-acb-b", status="exited", health="")
    env["docker"].add_container("acb-web-green", "cid-acb-g", status="stopped", health="")
    env["docker"].add_container("bark-blue", "cid-bark-b", status="exited", health="")
    env["docker"].add_container("bark-green", "cid-bark-g", status="stopped", health="")

    for app in ("acb", "bark"):
        st = ctrl.load_state(app, env["st_dir"])
        st["active_slot"] = "blue"
        st["slots"] = {"blue": {"container_id": f"cid-{app}-b", "status": "running"}}
        ctrl.save_state_atomic(app, st, env["st_dir"])

    # Both active containers die
    e1 = {"Action": "die", "Actor": {"ID": "cid-acb-b", "Attributes": {"name": "acb-web-blue"}}}
    e2 = {"Action": "die", "Actor": {"ID": "cid-bark-b", "Attributes": {"name": "bark-blue"}}}
    env["engine"].ingest_docker_event(e1)
    env["engine"].ingest_docker_event(e2)

    # Process both events
    env["engine"].process_event_batch()
    env["engine"].process_event_batch()

    st_acb = ctrl.load_state("acb", env["st_dir"])
    st_bark = ctrl.load_state("bark", env["st_dir"])

    assert st_acb["active_slot"] == "green"
    assert st_bark["active_slot"] == "green"
    assert "acb-web-green" in env["docker"].started
    assert "bark-green" in env["docker"].started


# 4. Cooldown
def test_cooldown_blocks_rapid_failover(env):
    """Active slot fails again within cooldown window -> blocked, marked degraded."""
    create_app_registry(env["reg_dir"], "acb", cooldown_seconds=300)
    env["docker"].add_container("acb-web-blue", "cid-blue-1", status="exited", health="")
    env["docker"].add_container("acb-web-green", "cid-green-1", status="stopped", health="")

    # Initial state on blue
    st = ctrl.load_state("acb", env["st_dir"])
    st["active_slot"] = "blue"
    st["slots"] = {"blue": {"container_id": "cid-blue-1", "status": "running"}}
    ctrl.save_state_atomic("acb", st, env["st_dir"])

    # Failover 1: blue fails at t=1000
    e1 = {"Action": "die", "Actor": {"ID": "cid-blue-1", "Attributes": {"name": "acb-web-blue"}}}
    env["engine"].ingest_docker_event(e1)
    env["engine"].process_event_batch()

    st = ctrl.load_state("acb", env["st_dir"])
    assert st["active_slot"] == "green"
    assert st["last_failover_time"] == 1000.0

    # Advance clock by 30 seconds (still within 300s cooldown)
    env["clock"].advance(30.0)
    # Green (now active) fails
    env["docker"].containers["acb-web-green"]["State"]["Status"] = "exited"
    e2 = {"Action": "die", "Actor": {"ID": "cid-green-1", "Attributes": {"name": "acb-web-green"}}}
    env["engine"].ingest_docker_event(e2)
    env["engine"].process_event_batch()

    st2 = ctrl.load_state("acb", env["st_dir"])
    # Green remains the active slot because switch was blocked by cooldown
    assert st2["active_slot"] == "green"
    assert st2["degraded"] is True
    assert "cooldown" in st2["degraded_reason"].lower()


# 5. Missing Active (No Default Blue)
def test_missing_active_safe_degraded(env):
    """Active slot missing / unknown -> safe degraded mode, never defaults to blue."""
    create_app_registry(env["reg_dir"], "acb")
    # Both containers stopped or missing
    env["docker"].add_container("acb-web-blue", "cid-blue-1", status="exited", health="")
    env["docker"].add_container("acb-web-green", "cid-green-1", status="exited", health="")

    st = ctrl.load_state("acb", env["st_dir"])
    st["active_slot"] = None  # missing active
    ctrl.save_state_atomic("acb", st, env["st_dir"])

    # Reconcile runs
    env["engine"].reconcile_app("acb")

    post_st = ctrl.load_state("acb", env["st_dir"])
    assert post_st["active_slot"] is None
    assert post_st["degraded"] is True
    assert "missing active slot" in post_st["degraded_reason"].lower()
    # Confirm no arbitrary switch or start
    assert len(env["runner"].history) == 0


# 6. Queue Full Dirty Reconcile
def test_queue_full_triggers_dirty_reconcile(env):
    """Bounded queue overflows -> drops event, marks dirty, processes reconcile."""
    create_app_registry(env["reg_dir"], "acb")
    # Override queue with tiny bounded size
    env["engine"].event_queue = ctrl.queue.Queue(maxsize=2)

    # Fill queue to capacity
    env["docker"].add_container("acb-web-blue", "cid-blue-1", status="running", health="healthy")
    env["docker"].add_container("acb-web-green", "cid-green-1", status="stopped", health="")

    e1 = {"Action": "die", "Actor": {"ID": "cid-blue-1", "Attributes": {"name": "acb-web-blue"}}}
    e2 = {"Action": "die", "Actor": {"ID": "cid-blue-1", "Attributes": {"name": "acb-web-blue"}}}
    e3 = {"Action": "die", "Actor": {"ID": "cid-blue-1", "Attributes": {"name": "acb-web-blue"}}}

    assert env["engine"].ingest_docker_event(e1) is True
    assert env["engine"].ingest_docker_event(e2) is True
    # 3rd event overflows queue
    assert env["engine"].ingest_docker_event(e3) is False
    assert "acb" in env["engine"]._dirty_apps

    # Worker picks up dirty app and runs full reconcile
    processed = env["engine"].process_event_batch()
    assert processed is True
    assert "acb" not in env["engine"]._dirty_apps


# 7. EOF & Backoff Reconnect
def test_eof_backoff_and_reconnect(env):
    """Docker event stream encounters EOF -> backs off, reconnects, reconciles."""
    create_app_registry(env["reg_dir"], "acb")
    env["docker"].add_container("acb-web-blue", "cid-blue-1", status="running", health="healthy")
    env["docker"].add_container("acb-web-green", "cid-green-1", status="stopped", health="")

    call_count = 0
    reconciled_apps = []

    def mock_reconcile_all():
        reconciled_apps.append("reconciled")

    env["engine"].reconcile_all = mock_reconcile_all

    def generator():
        nonlocal call_count
        call_count += 1
        if call_count == 1:
            raise EOFError("Connection reset by Docker daemon")
        # Second connection succeeds
        yield {"Action": "die", "Actor": {"ID": "cid-blue-1", "Attributes": {"name": "acb-web-blue"}}}
        env["engine"].stop()

    env["docker"].stream_generator = generator
    env["engine"].running = True
    env["engine"].run_event_stream_loop(backoff_init=1.0, backoff_max=4.0)

    assert call_count == 2
    assert "reconciled" in reconciled_apps
    assert env["clock"].now() > 1000.0  # sleep was invoked during backoff


# 8. Crash-Safe State Storage & CAS Revisions
def test_crash_safe_state_and_corrupt_recovery(env):
    """State writes are atomic; corrupt state files are backed up safely."""
    st = ctrl.load_state("acb", env["st_dir"])
    st["revision"] = 5
    st["active_slot"] = "green"
    ctrl.save_state_atomic("acb", st, env["st_dir"])

    target_file = env["st_dir"] / "acb" / "state.json"
    assert target_file.exists()

    loaded = ctrl.load_state("acb", env["st_dir"])
    assert loaded["revision"] == 5
    assert loaded["active_slot"] == "green"

    # Corrupt file simulation
    target_file.write_text("{ corrupt json ...", encoding="utf-8")
    recovered = ctrl.load_state("acb", env["st_dir"])
    assert recovered["degraded"] is True
    assert "corrupt" in recovered["degraded_reason"].lower()

    # Check that corrupt backup was saved
    corrupt_files = list(target_file.parent.glob("state.json.corrupt.*"))
    assert len(corrupt_files) == 1


# 9. Singleton Bounded Restart (Never Duplicate)
def test_singleton_bounded_restart(env):
    """Singleton workload bounded restarts up to limit, never duplicates instance."""
    create_app_registry(
        env["reg_dir"],
        "auth-browser",
        workload_class="singleton",
        container_name="auth-browser",
        max_restarts=2,
    )
    env["docker"].add_container("auth-browser", "cid-auth-1", status="exited", health="")

    st = ctrl.load_state("auth-browser", env["st_dir"])
    ctrl.save_state_atomic("auth-browser", st, env["st_dir"])

    event = {"Action": "die", "Actor": {"ID": "cid-auth-1", "Attributes": {"name": "auth-browser"}}}

    # Restart 1
    env["engine"].ingest_docker_event(event)
    env["engine"].process_event_batch()
    st = ctrl.load_state("auth-browser", env["st_dir"])
    assert st["restarts_count"] == 1
    assert "auth-browser" in env["docker"].restarted

    # Restart 2
    env["docker"].containers["auth-browser"]["State"]["Status"] = "exited"
    env["engine"].ingest_docker_event(event)
    env["engine"].process_event_batch()
    st = ctrl.load_state("auth-browser", env["st_dir"])
    assert st["restarts_count"] == 2

    # Restart 3 -> exceeds max_restarts (2) -> marked degraded, no new restart
    env["docker"].containers["auth-browser"]["State"]["Status"] = "exited"
    restart_count_before = len(env["docker"].restarted)
    env["engine"].ingest_docker_event(event)
    env["engine"].process_event_batch()
    st = ctrl.load_state("auth-browser", env["st_dir"])
    assert st["degraded"] is True
    assert len(env["docker"].restarted) == restart_count_before  # no new restart


# 10. Stale Deploy Lease
def test_stale_deploy_lease_reaping(env):
    """Active lease blocks failover; expired lease is reaped and failover proceeds."""
    create_app_registry(env["reg_dir"], "acb")
    env["docker"].add_container("acb-web-blue", "cid-blue-1", status="exited", health="")
    env["docker"].add_container("acb-web-green", "cid-green-1", status="stopped", health="")

    st = ctrl.load_state("acb", env["st_dir"])
    st["active_slot"] = "blue"
    st["slots"] = {"blue": {"container_id": "cid-blue-1", "status": "running"}}
    # Lease valid until t=1050
    st["operation_lease"] = {
        "owner": "deploy",
        "operation": "deploy-warm",
        "acquired_at": 1000.0,
        "expires_at": 1050.0,
    }
    ctrl.save_state_atomic("acb", st, env["st_dir"])

    event = {"Action": "die", "Actor": {"ID": "cid-blue-1", "Attributes": {"name": "acb-web-blue"}}}

    # At t=1010, lease is active -> failover blocked
    env["clock"].advance(10.0)
    env["engine"].ingest_docker_event(event)
    env["engine"].process_event_batch()
    st = ctrl.load_state("acb", env["st_dir"])
    assert st["active_slot"] == "blue"
    assert st["operation_lease"] is not None

    # Advance clock to t=1100 (lease expired at 1050)
    env["clock"].advance(90.0)
    env["engine"].ingest_docker_event(event)
    env["engine"].process_event_batch()

    # Expired lease reaped and failover proceeds to green
    st = ctrl.load_state("acb", env["st_dir"])
    assert st["operation_lease"] is None
    assert st["active_slot"] == "green"
    assert "acb-web-green" in env["docker"].started


# 11. Trusted Switch Interface
def test_trusted_switch_interface_validation(env):
    """Executable must begin with trusted prefix; invalid paths are rejected."""
    # Valid trusted prefix
    valid_cfg = ctrl.AppConfig.from_dict({
        "app": "acb",
        "workload_class": "blue_green",
        "slots": {
            "blue": {"container_name": "acb-b"},
            "green": {"container_name": "acb-g"},
        },
        "switch_cmd": ["/usr/local/bin/platform-switch", "{app}", "{slot}"],
    }, "acb")
    assert valid_cfg.switch_cmd == ["/usr/local/bin/platform-switch", "{app}", "{slot}"]

    # Untrusted relative/tmp path rejected by schema validation
    with pytest.raises(ValueError, match="not in trusted prefixes"):
        ctrl.AppConfig.from_dict({
            "app": "acb",
            "workload_class": "blue_green",
            "slots": {
                "blue": {"container_name": "acb-b"},
                "green": {"container_name": "acb-g"},
            },
            "switch_cmd": ["/tmp/malicious-switch", "{app}", "{slot}"],
        }, "acb")


# 12. App Registry matches Docker Compose
def test_apps_registry_matches_compose():
    """All container_name declarations in apps.d/*.json must exist in deploy/compose.prod.yaml."""
    base_dir = Path(__file__).resolve().parent.parent.parent
    apps_d = base_dir / "platform" / "failover" / "apps.d"
    compose_file = base_dir / "deploy" / "compose.prod.yaml"

    if not apps_d.exists() or not compose_file.exists():
        pytest.skip("apps.d or compose.prod.yaml not found at expected path")

    compose_text = compose_file.read_text(encoding="utf-8")
    compose_containers = set(re.findall(r"container_name:\s*([^\s#]+)", compose_text))

    assert len(compose_containers) > 0, "Failed to parse any container_name from compose.prod.yaml"

    for json_file in apps_d.glob("*.json"):
        data = json.loads(json_file.read_text(encoding="utf-8"))
        if data.get("workload_class") == "singleton":
            cname = data.get("container_name")
            assert cname in compose_containers, (
                f"{json_file.name}: singleton container '{cname}' not found in compose.prod.yaml ({compose_containers})"
            )
        elif data.get("workload_class") == "blue_green":
            for slot_name, slot_cfg in data.get("slots", {}).items():
                cname = slot_cfg.get("container_name")
                assert cname in compose_containers, (
                    f"{json_file.name}: slot '{slot_name}' container '{cname}' not found in compose.prod.yaml ({compose_containers})"
                )
