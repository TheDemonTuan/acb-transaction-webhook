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
    journal_path = Path(temp_dir) / "deploy-journal.json"

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
        journal_path=journal_path,
    )

    yield {
        "engine": engine,
        "reg_dir": reg_dir,
        "st_dir": st_dir,
        "lk_dir": lk_dir,
        "journal_path": journal_path,
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
    for k, v in kwargs.items():
        if k not in data:
            data[k] = v
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


# 13. Deployment Journal Blocks Failover & No Promotion During Transaction
def test_deployment_journal_blocks_failover(env):
    """When a deployment transaction is active, failover is strictly inhibited."""
    create_app_registry(env["reg_dir"], "acb")
    env["docker"].add_container("acb-web-blue", "cid-blue-1", status="running", health="healthy")
    env["docker"].add_container("acb-web-green", "cid-green-1", status="stopped", health="")

    st = ctrl.load_state("acb", env["st_dir"])
    st["active_slot"] = "blue"
    st["slots"] = {
        "blue": {"container_id": "cid-blue-1", "status": "running"},
        "green": {"container_id": "cid-green-1", "status": "stopped"},
    }
    ctrl.save_state_atomic("acb", st, env["st_dir"])

    # Active deployment journal written
    env["journal_path"].write_text(json.dumps({
        "tx_id": "tx-deploy-999",
        "component": "gateway",
        "state": "CANDIDATE_STARTING",
        "active_slot": "blue",
        "candidate_slot": "green",
    }), encoding="utf-8")

    # Active slot dies mid-deploy
    event = {
        "Action": "die",
        "Actor": {"ID": "cid-blue-1", "Attributes": {"name": "acb-web-blue"}},
    }
    env["engine"].ingest_docker_event(event)
    env["engine"].process_event_batch()

    # Verify: failover was inhibited by active transaction
    post_st = ctrl.load_state("acb", env["st_dir"])
    assert post_st["active_slot"] == "blue"
    assert "acb-web-green" not in env["docker"].started
    assert len(env["runner"].history) == 0


# 14. Candidate vs Committed State (No Premature Promotion)
def test_candidate_vs_committed_state_no_promotion(env):
    """Candidate container running during deployment is not promoted before commit."""
    create_app_registry(env["reg_dir"], "acb")
    # Candidate green is running for testing/soak, but active slot is blue
    env["docker"].add_container("acb-web-blue", "cid-blue-1", status="running", health="healthy")
    env["docker"].add_container("acb-web-green", "cid-green-1", status="running", health="healthy")

    st = ctrl.load_state("acb", env["st_dir"])
    st["active_slot"] = "blue"
    ctrl.save_state_atomic("acb", st, env["st_dir"])

    env["journal_path"].write_text(json.dumps({
        "tx_id": "tx-deploy-soak",
        "component": "gateway",
        "state": "TX_SOAKING",
        "active_slot": "blue",
        "candidate_slot": "green",
    }), encoding="utf-8")

    # Reconcile runs during soak
    env["engine"].reconcile_app("acb")

    post_st = ctrl.load_state("acb", env["st_dir"])
    assert post_st["active_slot"] == "blue"
    assert len(env["runner"].history) == 0


# 15. Mutual Host Lock Contention Safe Exit
def test_mutual_host_lock_contention_safe_exit(env):
    """When deploy holds the app lock, controller catches contention and exits safely without mutating route."""
    create_app_registry(env["reg_dir"], "acb")
    env["docker"].add_container("acb-web-blue", "cid-blue-1", status="running", health="healthy")
    env["docker"].add_container("acb-web-green", "cid-green-1", status="stopped", health="")

    st = ctrl.load_state("acb", env["st_dir"])
    st["active_slot"] = "blue"
    ctrl.save_state_atomic("acb", st, env["st_dir"])

    # Simulate deploy holding mutual lock with immediate timeout
    lock = ctrl.AppLock("acb", env["lk_dir"], timeout=0.05)
    lock.acquire()
    try:
        # Lower controller lock timeout for rapid test
        orig_app_lock = ctrl.AppLock
        class FastAppLock(orig_app_lock):
            def __init__(self, app_name, lock_dir, timeout=0.05):
                super().__init__(app_name, lock_dir, timeout=0.05)
        ctrl.AppLock = FastAppLock

        event = {
            "Action": "die",
            "Actor": {"ID": "cid-blue-1", "Attributes": {"name": "acb-web-blue"}},
        }
        env["engine"].ingest_docker_event(event)
        # Process event should not raise exception on lock contention
        env["engine"].process_event_batch()

        # Route switch was NOT attempted
        assert len(env["runner"].history) == 0
    finally:
        ctrl.AppLock = orig_app_lock
        lock.release()


# 16. Crash Between Phases: Orphan Tmp Cleanup
def test_crash_between_phases_tmp_cleanup(env):
    """Ungraceful crash between write phases leaves .tmp files; engine cleans them up."""
    acb_dir = env["st_dir"] / "acb"
    acb_dir.mkdir(parents=True, exist_ok=True)
    orphan_tmp = acb_dir / "state.json.tmp.9999_fakeuuid"
    orphan_tmp.write_text('{"incomplete": true}', encoding="utf-8")
    assert orphan_tmp.exists()

    ctrl.clean_stale_tmp_files(env["st_dir"])
    assert orphan_tmp.exists() is False


# 17. Corrupt State Safe Recovery: No Blind Switch
def test_corrupt_state_safe_recovery_no_blind_switch(env):
    """Corrupt state.json is safely backed up to .corrupt.<ts> and does not blindly promote default slot."""
    create_app_registry(env["reg_dir"], "acb")
    acb_dir = env["st_dir"] / "acb"
    acb_dir.mkdir(parents=True, exist_ok=True)
    state_file = acb_dir / "state.json"
    state_file.write_text("{ unclosed invalid json string ...", encoding="utf-8")

    st = ctrl.load_state("acb", env["st_dir"])
    assert st["degraded"] is True
    assert "corrupt state file recovered" in st["degraded_reason"].lower()

    corrupt_backups = list(acb_dir.glob("state.json.corrupt.*"))
    assert len(corrupt_backups) == 1

    # Reconcile with no healthy containers running
    env["engine"].reconcile_app("acb")
    post_st = ctrl.load_state("acb", env["st_dir"])
    assert post_st["active_slot"] is None
    assert post_st["degraded"] is True
    assert len(env["runner"].history) == 0


# 18. Intentional Stop Marker Prevents Failover
def test_intentional_stop_marker_prevents_failover(env):
    """When a slot has an intentional stop marker, die event is treated as expected transition."""
    create_app_registry(env["reg_dir"], "acb")
    env["docker"].add_container("acb-web-blue", "cid-blue-1", status="running", health="healthy")
    env["docker"].add_container("acb-web-green", "cid-green-1", status="stopped", health="")

    st = ctrl.load_state("acb", env["st_dir"])
    st["active_slot"] = "blue"
    st["slots"] = {"blue": {"container_id": "cid-blue-1", "status": "running"}}
    ctrl.save_state_atomic("acb", st, env["st_dir"])

    # Create intentional stop marker for blue
    acb_dir = env["st_dir"] / "acb"
    (acb_dir / "intentional-stop-blue").write_text('{"desired":"stopped"}', encoding="utf-8")

    event = {
        "Action": "die",
        "Actor": {"ID": "cid-blue-1", "Attributes": {"name": "acb-web-blue"}},
    }
    env["engine"].ingest_docker_event(event)
    env["engine"].process_event_batch()

    post_st = ctrl.load_state("acb", env["st_dir"])
    assert post_st["active_slot"] == "blue"
    assert post_st["slots"]["blue"]["status"] == "stopped"
    assert "acb-web-green" not in env["docker"].started
    assert len(env["runner"].history) == 0


# 19. Switch Command Failure: Rollback & Degraded
def test_switch_command_failure_rollback_and_degraded(env):
    """When switch command fails, standby container is stopped, state is marked degraded, no promotion."""
    create_app_registry(env["reg_dir"], "acb")
    env["docker"].add_container("acb-web-blue", "cid-blue-1", status="exited", health="")
    env["docker"].add_container("acb-web-green", "cid-green-1", status="stopped", health="")

    st = ctrl.load_state("acb", env["st_dir"])
    st["active_slot"] = "blue"
    st["slots"] = {"blue": {"container_id": "cid-blue-1", "status": "running"}}
    ctrl.save_state_atomic("acb", st, env["st_dir"])

    # Simulate switch command failing route ACK
    env["runner"].responses[("/usr/local/bin/platform-switch", "acb", "green")] = (
        1, "", "Route identity ACK failed: 502 Bad Gateway"
    )

    event = {
        "Action": "die",
        "Actor": {"ID": "cid-blue-1", "Attributes": {"name": "acb-web-blue"}},
    }
    env["engine"].ingest_docker_event(event)
    env["engine"].process_event_batch()

    post_st = ctrl.load_state("acb", env["st_dir"])
    assert post_st["active_slot"] == "blue"  # Green was NOT committed
    assert post_st["degraded"] is True
    assert "route switch or identity ack failed" in post_st["degraded_reason"].lower()
    # Confirm candidate was stopped/rolled back
    assert ["docker", "stop", "-t", "5", "acb-web-green"] in env["runner"].history


# 20. Both Slots Degraded: Primary Dies and Standby Fails to Start
def test_both_slots_degraded_standby_start_fails(env):
    """When primary dies and standby start fails, both slots degraded is declared, no switch."""
    create_app_registry(env["reg_dir"], "acb")
    env["docker"].add_container("acb-web-blue", "cid-blue-1", status="exited", health="")
    env["docker"].add_container("acb-web-green", "cid-green-1", status="stopped", health="")

    st = ctrl.load_state("acb", env["st_dir"])
    st["active_slot"] = "blue"
    st["slots"] = {"blue": {"container_id": "cid-blue-1", "status": "running"}}
    ctrl.save_state_atomic("acb", st, env["st_dir"])

    # Standby start fails (e.g. Docker daemon error / OOM)
    env["docker"].start_container = lambda name: (1, "", "Cannot start container: out of memory")

    event = {
        "Action": "die",
        "Actor": {"ID": "cid-blue-1", "Attributes": {"name": "acb-web-blue"}},
    }
    env["engine"].ingest_docker_event(event)
    env["engine"].process_event_batch()

    post_st = ctrl.load_state("acb", env["st_dir"])
    assert post_st["active_slot"] == "blue"
    assert post_st["degraded"] is True
    assert "both slots degraded" in post_st["degraded_reason"].lower()
    assert len(env["runner"].history) == 0


# 21. Both Slots Degraded: Standby Fails Readiness Probe
def test_both_slots_degraded_standby_readiness_timeout(env):
    """When primary dies and standby fails readiness probe within timeout, both degraded is declared."""
    create_app_registry(env["reg_dir"], "acb", health_timeout=2)
    env["docker"].add_container("acb-web-blue", "cid-blue-1", status="exited", health="")
    # Green container stays unhealthy
    env["docker"].add_container("acb-web-green", "cid-green-1", status="running", health="unhealthy")

    # Override start to leave it unhealthy
    env["docker"].start_container = lambda name: (0, "started", "")
    env["docker"].exec_healthcheck = lambda name, probe_cmd=None: False

    st = ctrl.load_state("acb", env["st_dir"])
    st["active_slot"] = "blue"
    st["slots"] = {"blue": {"container_id": "cid-blue-1", "status": "running"}}
    ctrl.save_state_atomic("acb", st, env["st_dir"])

    event = {
        "Action": "die",
        "Actor": {"ID": "cid-blue-1", "Attributes": {"name": "acb-web-blue"}},
    }
    env["engine"].ingest_docker_event(event)
    env["engine"].process_event_batch()

    post_st = ctrl.load_state("acb", env["st_dir"])
    assert post_st["active_slot"] == "blue"
    assert post_st["degraded"] is True
    assert "both slots degraded" in post_st["degraded_reason"].lower()
    assert ["docker", "stop", "-t", "5", "acb-web-green"] in env["runner"].history


# 22. Worker Singleton Recovery with Exponential Backoff and Fencing
def test_worker_singleton_recovery_with_backoff_and_fencing(env):
    """Worker singleton fails: restarts with backoff, never spawns duplicate, marks degraded on exhaustion."""
    create_app_registry(
        env["reg_dir"],
        "worker",
        workload_class="singleton",
        container_name="acb-worker",
        max_restarts=3,
        min_restart_interval=10,
        backoff_factor=2.0,
    )
    env["docker"].add_container("acb-worker", "cid-worker-1", status="exited", health="")

    st = ctrl.load_state("worker", env["st_dir"])
    ctrl.save_state_atomic("worker", st, env["st_dir"])

    event = {"Action": "die", "Actor": {"ID": "cid-worker-1", "Attributes": {"name": "acb-worker"}}}

    # Restart 1 at t=1000: succeeds
    env["engine"].ingest_docker_event(event)
    env["engine"].process_event_batch()
    st = ctrl.load_state("worker", env["st_dir"])
    assert st["restarts_count"] == 1
    assert "acb-worker" in env["docker"].restarted

    # Rapid failure at t=1002 (within 10s backoff): restart is blocked by backoff
    env["clock"].advance(2.0)
    env["docker"].containers["acb-worker"]["State"]["Status"] = "exited"
    restarts_before = len(env["docker"].restarted)
    env["engine"].ingest_docker_event(event)
    env["engine"].process_event_batch()
    assert len(env["docker"].restarted) == restarts_before  # no new restart

    # Advance clock to t=1015 (past 10s backoff): restart 2 succeeds
    env["clock"].advance(13.0)
    env["engine"].ingest_docker_event(event)
    env["engine"].process_event_batch()
    st = ctrl.load_state("worker", env["st_dir"])
    assert st["restarts_count"] == 2
    assert len(env["docker"].restarted) == restarts_before + 1

    # Advance clock past second backoff (20s) to t=1040: restart 3 succeeds
    env["clock"].advance(25.0)
    env["docker"].containers["acb-worker"]["State"]["Status"] = "exited"
    env["engine"].ingest_docker_event(event)
    env["engine"].process_event_batch()
    st = ctrl.load_state("worker", env["st_dir"])
    assert st["restarts_count"] == 3

    # Failure 4 at t=1090: exceeds max_restarts (3) -> marked degraded, no new restart
    env["clock"].advance(50.0)
    env["docker"].containers["acb-worker"]["State"]["Status"] = "exited"
    restarts_before_exhaust = len(env["docker"].restarted)
    env["engine"].ingest_docker_event(event)
    env["engine"].process_event_batch()
    st = ctrl.load_state("worker", env["st_dir"])
    assert st["degraded"] is True
    assert "exceeded max restarts" in st["degraded_reason"].lower()
    assert len(env["docker"].restarted) == restarts_before_exhaust


# 23. Worker Singleton: Deploy in Progress Inhibits Controller Restarts
def test_worker_deployment_in_progress_inhibits_restart(env):
    """When deployment transaction is active for worker, failover controller does not touch worker."""
    create_app_registry(
        env["reg_dir"],
        "worker",
        workload_class="singleton",
        container_name="acb-worker",
        max_restarts=3,
    )
    env["docker"].add_container("acb-worker", "cid-worker-1", status="exited", health="")

    st = ctrl.load_state("worker", env["st_dir"])
    ctrl.save_state_atomic("worker", st, env["st_dir"])

    env["journal_path"].write_text(json.dumps({
        "tx_id": "tx-worker-upgrade",
        "component": "worker",
        "state": "CANDIDATE_STARTING",
    }), encoding="utf-8")

    event = {"Action": "die", "Actor": {"ID": "cid-worker-1", "Attributes": {"name": "acb-worker"}}}
    env["engine"].ingest_docker_event(event)
    env["engine"].process_event_batch()

    assert len(env["docker"].restarted) == 0
    st = ctrl.load_state("worker", env["st_dir"])
    assert st["restarts_count"] == 0
