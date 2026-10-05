"""One-shot hosting metadata migration, with real isolated file transitions.

Only subprocess/network boundaries are injected. These tests never need Docker,
Cloudflare credentials, a bank session, or a production deployment directory.
"""
import copy
import hashlib
import importlib.util
import json
from pathlib import Path
import stat
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import yaml

spec = importlib.util.spec_from_file_location(
    "static_hosting_migration", Path(__file__).parents[1] / "migrate-static-hosting.py")
migration = importlib.util.module_from_spec(spec)
spec.loader.exec_module(migration)

CURRENT = "a" * 40
PREVIOUS = "b" * 40
HISTORICAL = "c" * 40
MIGRATION_COMMIT = "f" * 40
WORKER_COMMIT = "e" * 40
VERSION = "12345678-1234-4234-8234-123456789abc"
SCRIPTS = ("deploy.sh", "simple-lib.sh", "healthcheck.sh", "render-route.sh")
BANK = "bank.tuannguyenviet.site"
VIEWER = "transactions.tuannguyenviet.site"


def digest(label):
    return "registry.example/acb/" + label + "@sha256:" + hashlib.sha256(label.encode()).hexdigest()


def sha256(data):
    return hashlib.sha256(data).hexdigest()


def env_bytes(values):
    return "".join(key + "=" + value + "\n" for key, value in values.items()).encode()


def env_values(path):
    values = {}
    for raw in path.read_text().splitlines():
        if not raw or raw.startswith("#"):
            continue
        key, value = raw.split("=", 1)
        if key in values:
            raise AssertionError("duplicate projected key: " + key)
        values[key] = value
    return values


def backend_route(slot):
    service = "acb-service"
    routers = {
        "acb-deny-internal": {
            "rule": f"(Host(`{BANK}`) || Host(`{VIEWER}`)) && PathPrefix(`/internal`)",
            "entryPoints": ["web"], "priority": 1000,
            "middlewares": ["deny-internal"], "service": service},
        "acb-public-deny-private": {
            "rule": f"Host(`{VIEWER}`) && (PathPrefix(`/api`) || PathPrefix(`/internal`) || PathPrefix(`/admin`) || Path(`/health`) || Path(`/healthz`) || Path(`/ready`) || Path(`/readyz`))",
            "entryPoints": ["web"], "priority": 1000,
            "middlewares": ["deny-internal"], "service": service},
        "acb-public-sse-router": {
            "rule": f"Host(`{VIEWER}`) && (Path(`/api/public/v1/events`) || Path(`/api/public/v1/events/stream`))",
            "entryPoints": ["web"], "priority": 1200,
            "middlewares": ["tunnel-only", "public-sse-rate-limit", "public-sse-inflight-ip",
                            "public-sse-inflight-global", "security-headers"], "service": service},
        "acb-public-api-router": {
            "rule": f"Host(`{VIEWER}`) && (Path(`/api/public/v1`) || PathPrefix(`/api/public/v1/`))",
            "entryPoints": ["web"], "priority": 1100,
            "middlewares": ["tunnel-only", "public-api-rate-limit", "public-api-inflight-ip",
                            "public-api-inflight-global", "security-headers"], "service": service},
        "acb-api-router": {
            "rule": f"Host(`{BANK}`) && (PathPrefix(`/api`) || Path(`/health`) || Path(`/healthz`) || Path(`/ready`) || Path(`/readyz`))",
            "entryPoints": ["web"], "priority": 200,
            "middlewares": ["tunnel-only", "security-headers"], "service": service},
        "acb-deploy-gateway": {
            "rule": "Host(`gateway-deploy.acb.internal.invalid`) && Path(`/readyz`)",
            "entryPoints": ["slot-probe"], "service": service},
    }
    return {"http": {"routers": routers, "services": {
        service: {"loadBalancer": {
            "passHostHeader": True, "responseForwarding": {"flushInterval": "100ms"},
            "servers": [{"url": f"http://acb-web-{slot}:8090"}],
            "healthCheck": {"path": "/readyz", "interval": "5s", "timeout": "2s"}}}}}}


def legacy_route(slot, frontend_slot):
    route = backend_route(slot)
    route["http"]["routers"].update({
        "acb-frontend-router": {"rule": f"Host(`{BANK}`)", "entryPoints": ["web"],
                                "service": "acb-frontend-service"},
        "acb-public-frontend-router": {"rule": f"Host(`{VIEWER}`)", "entryPoints": ["web"],
                                       "service": "acb-frontend-service"},
        "acb-credentials-router": {"rule": f"Host(`{BANK}`) && Path(`/admin/acb-credentials`)",
                                   "service": "acb-frontend-service",
                                   "middlewares": ["acb-credentials-security"]},
        "acb-deploy-frontend": {"rule": "Host(`frontend-deploy.acb.internal.invalid`)",
                                "entryPoints": ["slot-probe"], "service": "acb-frontend-service"},
    })
    route["http"]["services"]["acb-frontend-service"] = {
        "loadBalancer": {"servers": [{"url": f"http://acb-frontend-{frontend_slot}:8080"}]}}
    route["http"]["middlewares"] = {"acb-credentials-security": {
        "headers": {"customResponseHeaders": {"Cache-Control": "no-store"}}}}
    return route


class MigrationTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.private = Path(self.temp.name)
        self.private.chmod(0o700)
        self.root = self.private / "deployment"
        self.root.mkdir(mode=0o700)
        self.bundle = self.private / "verified-bundle"
        self.bundle.mkdir(mode=0o700)
        self.snapshot = self.private / "cutover-snapshot"
        self.proof = self.private / "proof.json"
        self.route = self.root / "edge/dynamic/acb.yml"
        self.calls = []
        self.acks = []
        self.identity_bad = False
        self.ack_bad = False
        self.route_boundary_drift = None
        self.public_calls = []
        self.runtime_before = {}
        self.images_before = {}
        self.compose_before = {}
        self.source_manifest_hashes = {}
        self.write(self.root / "deploy/.env.production",
                   f"PUBLIC_ORIGIN=https://{BANK}\nPUBLIC_VIEWER_ORIGIN=https://{VIEWER}\n"
                   f"ACB_ROUTE_FILE={self.route}\nAUTH_RECOVERY_ENABLED=false\n")
        (self.root / "deploy/secrets").mkdir(mode=0o700)
        self.write(self.root / "deploy/secrets/app_master_key", b"synthetic-secret\x00\n")
        self.write(self.root / "data/gateway.db", b"SQLite fixture bytes: never migrate me\x00\xff")
        self.write(self.root / "data/gateway.db-wal", b"unchanged WAL\x00\x01")
        self.write(self.root / "state.env", env_bytes({
            "RELEASE_SHA": CURRENT, "GATEWAY_SLOT": "blue", "FRONTEND_SLOT": "green"}))
        self.write(self.root / "rollback/previous-state.env", env_bytes({
            "RELEASE_SHA": PREVIOUS, "GATEWAY_SLOT": "green", "FRONTEND_SLOT": "blue"}))
        self.write(self.root / "rollback/target-state.env", (self.root / "state.env").read_bytes())
        self.write(self.root / "rollback/previous-acb.yml",
                   yaml.safe_dump(legacy_route("green", "blue")), mode=0o640)
        self.write(self.route, yaml.safe_dump(legacy_route("blue", "green")), mode=0o644)
        for release in (CURRENT, PREVIOUS, HISTORICAL):
            self.make_release(release)
        self.make_verified_bundle()
        self.write(self.proof, json.dumps({"hosts": [{
            "hostname": hostname, "release_sha": MIGRATION_COMMIT,
            "worker_version_id": VERSION, "verified_at": "2026-10-04T12:00:00Z",
        } for hostname in (BANK, VIEWER)]}))
        self.before = self.tree(self.root)
        self.historical_before = self.tree(self.root / "releases" / HISTORICAL)
        self.driver = migration.Migration(self.root, self.bundle, self.snapshot,
                                          proof=self.proof, runner=self.runner)
        self.public_patch = patch.object(self.driver, "verify_public", side_effect=self.verify_public)
        self.public_patch.start()
        self.addCleanup(self.public_patch.stop)

    def write(self, path, data, mode=0o600):
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(data.encode() if isinstance(data, str) else data)
        path.chmod(mode)

    @staticmethod
    def tree(directory):
        return {str(path.relative_to(directory)): (
            path.read_bytes(), stat.S_IMODE(path.stat().st_mode),
            path.stat().st_uid, path.stat().st_gid)
            for path in directory.rglob("*") if path.is_file()}

    def manifest(self, directory):
        rows = [sha256(path.read_bytes()) + "  " + str(path.relative_to(directory))
                for path in sorted(directory.rglob("*"))
                if path.is_file() and path.name != "SHA256SUMS"]
        self.write(directory / "SHA256SUMS", "\n".join(rows) + "\n")

    def make_release(self, release):
        directory = self.root / "releases" / release
        directory.mkdir(parents=True, mode=0o700)
        images = {"RELEASE_SHA": release, "GATEWAY_IMAGE_REF": digest("gateway-" + release),
                  "FRONTEND_IMAGE_REF": digest("frontend-" + release),
                  "WORKER_IMAGE_REF": digest("worker-build-" + release),
                  "DBTOOL_IMAGE_REF": digest("dbtool-" + release),
                  "BROWSER_IMAGE_REF": digest("browser-" + release),
                  "TTS_IMAGE_REF": digest("tts-" + release), "BARK_IMAGE_REF": digest("bark-" + release)}
        runtime = {
            "IMAGE_REF_BLUE": images["GATEWAY_IMAGE_REF"],
            "IMAGE_REF_GREEN": images["GATEWAY_IMAGE_REF"],
            "FRONTEND_IMAGE_REF_BLUE": images["FRONTEND_IMAGE_REF"],
            "FRONTEND_IMAGE_REF_GREEN": digest("frontend-independent-" + release),
            "WORKER_IMAGE_REF": digest("worker-runtime-recovery-" + release),
            "BROWSER_IMAGE_REF": images["BROWSER_IMAGE_REF"], "TTS_IMAGE_REF": images["TTS_IMAGE_REF"],
            "BARK_IMAGE_REF": images["BARK_IMAGE_REF"], "DBTOOL_IMAGE_REF": images["DBTOOL_IMAGE_REF"],
            "RELEASE_COMMIT_BLUE": release, "RELEASE_COMMIT_GREEN": release,
            "WORKER_RELEASE_COMMIT": WORKER_COMMIT,
            "ENV_FILE": str(self.root / "deploy/.env.production"),
            "SECRETS_DIR": str(self.root / "deploy/secrets"), "BARK_SECRET_GROUP": "1000",
        }
        services = {}
        for name, image_key in (("gateway-blue", "IMAGE_REF_BLUE"), ("gateway-green", "IMAGE_REF_GREEN"),
                                ("worker", "WORKER_IMAGE_REF"), ("recovery-controller", "WORKER_IMAGE_REF"),
                                ("auth-browser", "BROWSER_IMAGE_REF"), ("tts-gateway", "TTS_IMAGE_REF"),
                                ("bark", "BARK_IMAGE_REF")):
            services[name] = {
                "image": "${" + image_key + ":?required}",
                "container_name": "acb-" + name,
                "restart": "unless-stopped", "user": "1000:1000", "read_only": True,
                "cap_drop": ["ALL"], "security_opt": ["no-new-privileges:true"],
                "environment": {"RELEASE_COMMIT": ("${RELEASE_COMMIT_" + name.split("-")[-1].upper() + "}"
                                                    if name.startswith("gateway-") else "${WORKER_RELEASE_COMMIT}"),
                                "CUSTOM_BASELINE_VALUE": "must survive " + release},
                "volumes": ["gateway_data:/data", "./operator.conf:/etc/operator.conf:ro"],
                "networks": ["acb-core"], "mem_limit": "517M", "pids_limit": 91,
                "healthcheck": {"test": ["CMD", "/health-fixture"], "interval": "13s"},
            }
        for slot in ("blue", "green"):
            services["frontend-" + slot] = {
                "image": "${FRONTEND_IMAGE_REF_" + slot.upper() + "}",
                "container_name": "acb-frontend-" + slot, "networks": ["acb-core"]}
        compose = {"name": "acb", "services": services,
                   "volumes": {"gateway_data": {"external": True}, "bark_data": {"external": True}},
                   "networks": {"acb-core": {"external": True}},
                   "secrets": {"app_master_key": {"file": "${SECRETS_DIR}/app_master_key"}}}
        self.images_before[release], self.runtime_before[release] = images, runtime
        self.compose_before[release] = copy.deepcopy(compose)
        self.write(directory / "images.env", env_bytes(images))
        self.write(directory / "runtime.env", env_bytes(runtime))
        self.write(directory / "compose.prod.yaml", yaml.safe_dump(compose, sort_keys=False), mode=0o640)
        for script in SCRIPTS:
            self.write(directory / script, "#!/bin/bash\n# original " + release + " " + script + "\n", mode=0o750)
        self.write(directory / "import-baseline.py", "# original legacy importer\n", mode=0o750)
        self.write(directory / "baseline-frontend.sha256", "1" * 64 + "\n")
        self.write(directory / "seccomp-auth-browser.json", '{"defaultAction":"SCMP_ACT_ERRNO"}\n', mode=0o644)
        self.write(directory / "operator.conf", "custom backend baseline\n", mode=0o640)
        self.manifest(directory)
        self.source_manifest_hashes[release] = sha256((directory / "SHA256SUMS").read_bytes())

    def make_verified_bundle(self):
        images = {key: value for key, value in self.images_before[CURRENT].items()
                  if not key.startswith("FRONTEND_")}
        images["RELEASE_SHA"] = MIGRATION_COMMIT
        self.write(self.bundle / "images.env", env_bytes(images))
        compose = copy.deepcopy(self.compose_before[CURRENT])
        for slot in ("blue", "green"):
            del compose["services"]["frontend-" + slot]
        # Deliberately different: projection must use the OLD backend definitions.
        compose["services"]["worker"]["mem_limit"] = "999M"
        self.write(self.bundle / "compose.prod.yaml", yaml.safe_dump(compose))
        source = Path(__file__).parents[1]
        for script in SCRIPTS:
            self.write(self.bundle / script, (source / script).read_bytes(), mode=0o755)
        self.write(self.bundle / "migrate-static-hosting.py", (source / "migrate-static-hosting.py").read_bytes(), mode=0o755)
        self.write(self.bundle / "seccomp-auth-browser.json", '{"defaultAction":"SCMP_ACT_ERRNO"}\n')
        self.manifest(self.bundle)

    def verify_public(self, proof):
        self.public_calls.append(proof)

    def runner(self, args, **kwargs):
        args = [str(arg) for arg in args]
        self.calls.append(args)
        stdout = ""
        if args[0] == "bash" and Path(args[1]).name == "render-route.sh":
            self.assertEqual(len(args), 3)
            self.assertIn(args[2], ("blue", "green"))
            stdout = yaml.safe_dump(backend_route(args[2]))
        elif args[0] == "bash" and args[1] == "-c":
            if args[2] == 'source "$1/simple-lib.sh"; route_replace "$2" "$3" "$4"':
                self.assertEqual(args[3:5], ["migration", str(self.bundle)])
                self.assertEqual(len(args), 8)
                source, destination, expected = Path(args[-3]), Path(args[-2]), args[-1]
                self.assertTrue(source.is_relative_to(self.snapshot))
                self.assertEqual(destination, self.route)
                plan = json.loads((self.snapshot / "manifest.json").read_text())
                entry = next(record for record in plan["files"] if record["path"] == str(destination))
                journal = json.loads((self.root / ".static-hosting-pending").read_text())
                applying = journal["operation"] == "apply"
                self.assertEqual(expected, entry["before_sha256" if applying else "after_sha256"])
                self.assertEqual(source, self.snapshot / entry["after_file" if applying else "before_file"])
                existing = destination.stat()
                if self.route_boundary_drift is not None:
                    self.write(destination, self.route_boundary_drift, stat.S_IMODE(existing.st_mode))
                    self.route_boundary_drift = None
                if sha256(destination.read_bytes()) != expected:
                    return subprocess.CompletedProcess(args, 1, stdout="", stderr="route drift")
                migration.atomic_replace(destination, source.read_bytes(),
                                         stat.S_IMODE(existing.st_mode), existing.st_uid, existing.st_gid)
            else:
                self.assertEqual(args[2], 'source "$1/simple-lib.sh"; validate_route "$2" "$3"')
                route, slot = Path(args[-2]), args[-1]
                self.assertEqual(yaml.safe_load(route.read_text()), backend_route(slot))
        elif args[0] == "bash" and Path(args[1]).name == "healthcheck.sh":
            self.assertEqual(args[2], "route")
            self.assertEqual(len(args), 5)
            self.acks.append(tuple(args[3:]))
            if self.ack_bad and (self.root / ".static-hosting-pending").exists():
                raise subprocess.CalledProcessError(1, args, stderr="gateway ACK mismatch")
        elif args[:3] == ["docker", "image", "inspect"]:
            stdout = json.dumps([{"Id": "sha256:" + args[-1].split("@sha256:")[-1]}])
        elif args[:2] == ["docker", "inspect"]:
            runtime = self.runtime_before[CURRENT]
            names = [arg for arg in args[2:] if not arg.startswith("-")]
            objects = []
            for name in names:
                image_key = {"acb-web-blue": "IMAGE_REF_BLUE", "acb-web-green": "IMAGE_REF_GREEN",
                             "acb-gateway-blue": "IMAGE_REF_BLUE", "acb-gateway-green": "IMAGE_REF_GREEN",
                             "acb-worker": "WORKER_IMAGE_REF", "acb-recovery-controller": "WORKER_IMAGE_REF",
                             "acb-auth-browser": "BROWSER_IMAGE_REF", "acb-tts-gateway": "TTS_IMAGE_REF",
                             "acb-bark": "BARK_IMAGE_REF"}[name]
                ref = runtime[image_key]
                commit = CURRENT if "BLUE" in image_key or "GREEN" in image_key else WORKER_COMMIT
                objects.append({"Id": "fixture-container-" + name, "Name": "/" + name,
                                "Image": "sha256:" + ("0" * 64 if self.identity_bad else ref.split("@sha256:")[-1]),
                                "Config": {"Image": ref, "Env": ["RELEASE_COMMIT=" + commit,
                                                                      "DEPLOY_SLOT=" + ("green" if name.endswith("green") else "blue")]},
                                "State": {"Running": True, "Health": {"Status": "healthy"}}})
            stdout = json.dumps(objects)
        elif args[:2] == ["docker", "compose"]:
            self.assertEqual(args[-2:], ["config", "--quiet"])
            candidate = Path(args[args.index("-f") + 1])
            compose = yaml.safe_load(candidate.read_text())
            self.assertFalse(set(compose["services"]) & {"frontend-blue", "frontend-green"})
            self.assertNotIn("FRONTEND_", candidate.with_name("runtime.env").read_text())
        else:
            self.fail("Unexpected or mutating subprocess: " + repr(args))
        return subprocess.CompletedProcess(args, 0, stdout=stdout, stderr="")

    def assert_original_metadata(self):
        actual = self.tree(self.root)
        for name, original in self.before.items():
            self.assertEqual(actual.get(name), original, name)
        self.assertFalse(any(name.endswith("static-hosting-provenance.json") for name in actual))

    def assert_checksums(self, directory):
        entries = {}
        for line in (directory / "SHA256SUMS").read_text().splitlines():
            checksum, filename = line.split(None, 1)
            filename = filename.lstrip("*")
            self.assertNotIn(filename, entries)
            self.assertFalse(Path(filename).is_absolute())
            self.assertNotIn("..", Path(filename).parts)
            self.assertEqual(sha256((directory / filename).read_bytes()), checksum, filename)
            entries[filename] = checksum
        self.assertTrue(set(SCRIPTS) | {"images.env", "runtime.env", "compose.prod.yaml"} <= set(entries))
        self.assertNotIn("import-baseline.py", entries)
        self.assertNotIn("baseline-frontend.sha256", entries)

    def test_check_is_read_only_and_does_not_create_snapshot_or_locks(self):
        self.driver.check()
        self.assertEqual(self.tree(self.root), self.before)
        self.assertFalse(self.snapshot.exists())
        self.assertFalse((self.root / ".static-hosting-pending").exists())
        self.assertFalse(self.public_calls)

    def test_old_canonical_and_rollback_project_preserving_backend_values_and_permissions(self):
        replacements = []
        original_replace = migration.atomic_replace

        def record_replace(path, *args, **kwargs):
            path = Path(path)
            if path.is_relative_to(self.root):
                replacements.append(path)
            return original_replace(path, *args, **kwargs)

        with patch.object(migration, "atomic_replace", side_effect=record_replace):
            self.driver.apply()
        self.assertTrue(self.public_calls)
        self.assertFalse((self.root / ".static-hosting-pending").exists())
        self.assertTrue((self.snapshot / "receipt.json").is_file())
        self.assertIn(("blue", CURRENT), self.acks)
        state_writes = [path for path in replacements if path == self.root / "state.env"]
        self.assertEqual(len(state_writes), 1)
        metadata_writes = [path for path in replacements if path.name != ".static-hosting-pending"]
        self.assertEqual(metadata_writes[-1], self.root / "state.env")
        for name, release, slot in (("state.env", CURRENT, "blue"),
                                    ("rollback/previous-state.env", PREVIOUS, "green"),
                                    ("rollback/target-state.env", CURRENT, "blue")):
            self.assertEqual(env_values(self.root / name), {"RELEASE_SHA": release, "GATEWAY_SLOT": slot})
            self.assertEqual(stat.S_IMODE((self.root / name).stat().st_mode), self.before[name][1])
        for release in (CURRENT, PREVIOUS):
            directory = self.root / "releases" / release
            for filename, old in (("runtime.env", self.runtime_before[release]),
                                  ("images.env", self.images_before[release])):
                self.assertEqual(env_values(directory / filename), {
                    key: value for key, value in old.items() if not key.startswith("FRONTEND_")})
            expected = copy.deepcopy(self.compose_before[release])
            for slot in ("blue", "green"):
                del expected["services"]["frontend-" + slot]
            self.assertEqual(yaml.safe_load((directory / "compose.prod.yaml").read_text()), expected)
            for script in SCRIPTS:
                self.assertEqual((directory / script).read_bytes(), (self.bundle / script).read_bytes())
            for filename in ("images.env", "runtime.env", "compose.prod.yaml", "SHA256SUMS", *SCRIPTS):
                relative = str((directory / filename).relative_to(self.root))
                self.assertEqual(stat.S_IMODE((directory / filename).stat().st_mode), self.before[relative][1])
            self.assertFalse((directory / "import-baseline.py").exists())
            self.assertFalse((directory / "baseline-frontend.sha256").exists())
            self.assert_checksums(directory)
            provenance = json.loads((directory / "static-hosting-provenance.json").read_text())
            self.assertEqual(provenance["source_release_sha"], release)
            self.assertEqual(provenance["source_checksum_manifest_sha256"], self.source_manifest_hashes[release])
            self.assertEqual(provenance["migration_commit"], MIGRATION_COMMIT)
            self.assertEqual(provenance["snapshot_path"], str(self.snapshot))
        self.assertEqual(yaml.safe_load(self.route.read_text()), backend_route("blue"))
        self.assertEqual(yaml.safe_load((self.root / "rollback/previous-acb.yml").read_text()), backend_route("green"))
        self.assertEqual(self.tree(self.root / "releases" / HISTORICAL), self.historical_before)
        for name in ("data/gateway.db", "data/gateway.db-wal", "deploy/.env.production", "deploy/secrets/app_master_key"):
            self.assertEqual(self.tree(self.root)[name], self.before[name], name)
        projected = self.tree(self.root)
        for name, original in self.before.items():
            if name in projected:
                self.assertEqual(projected[name][1:], original[1:], name)
        self.assertEqual(stat.S_IMODE(self.snapshot.stat().st_mode), 0o700)
        for path in self.snapshot.rglob("*"):
            self.assertFalse(path.is_symlink())
            self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o700 if path.is_dir() else 0o600, str(path))

    def test_second_apply_validates_and_is_noop(self):
        self.driver.apply()
        before = self.tree(self.root)
        snapshot_before = self.tree(self.snapshot)
        mtimes = {path: path.stat().st_mtime_ns for path in self.root.rglob("*") if path.is_file()}
        self.driver.apply()
        self.assertEqual(self.tree(self.root), before)
        self.assertEqual(self.tree(self.snapshot), snapshot_before)
        for path, mtime in mtimes.items():
            self.assertEqual(path.stat().st_mtime_ns, mtime, str(path))

    def test_interrupt_leaves_marker_blocks_apply_and_restores_exact_original_bytes(self):
        replace = self.driver.replace_file
        calls = []

        def interrupted(*args, **kwargs):
            result = replace(*args, **kwargs)
            calls.append(args)
            if len(calls) == 1:
                raise KeyboardInterrupt("fixture power loss after first live metadata write")
            return result

        with patch.object(self.driver, "replace_file", side_effect=interrupted):
            with self.assertRaises(KeyboardInterrupt):
                self.driver.apply()
        self.assertTrue((self.root / ".static-hosting-pending").is_file())
        self.assertTrue((self.snapshot / "manifest.json").is_file())
        self.assertNotEqual(self.tree(self.root), self.before)
        self.assertEqual((self.root / "state.env").read_bytes(), self.before["state.env"][0])
        with self.assertRaises(migration.MigrationError):
            self.driver.apply()
        self.driver.restore()
        self.assert_original_metadata()
        self.assertFalse((self.root / ".static-hosting-pending").exists())
        self.assertIn(("blue", CURRENT), self.acks)

    def test_completed_migration_restore_reinstates_originals_and_removes_provenance(self):
        self.driver.apply()
        self.driver.restore()
        self.assert_original_metadata()
        self.assertFalse((self.root / ".static-hosting-pending").exists())

    def test_restore_refuses_live_drift_without_overwriting_any_metadata(self):
        self.driver.apply()
        path = self.root / "releases" / CURRENT / "runtime.env"
        self.write(path, path.read_bytes().replace(WORKER_COMMIT.encode(), ("9" * 40).encode()))
        drifted = self.tree(self.root)
        with self.assertRaises(migration.MigrationError):
            self.driver.restore()
        self.assertEqual(self.tree(self.root), drifted)
        self.assertTrue((self.snapshot / "manifest.json").exists())

    def test_route_drift_at_apply_publisher_boundary_is_not_overwritten(self):
        drift = b"# external route update between migration compare and publisher\n"
        self.route_boundary_drift = drift
        with self.assertRaises(migration.MigrationError):
            self.driver.apply()
        self.assertEqual(self.route.read_bytes(), drift)
        self.assertTrue((self.root / ".static-hosting-pending").exists())
        self.assertEqual((self.root / "state.env").read_bytes(), self.before["state.env"][0])
        failed = self.tree(self.root)
        with self.assertRaises(migration.MigrationError):
            self.driver.restore()
        self.assertEqual(self.tree(self.root), failed)
        # Operator resolves the external route change before requesting restore.
        relative = str(self.route.relative_to(self.root))
        self.write(self.route, self.before[relative][0], self.before[relative][1])
        self.driver.restore()
        self.assert_original_metadata()

    def test_route_drift_at_restore_publisher_boundary_is_not_overwritten(self):
        self.driver.apply()
        drift = b"# external route update during metadata restore\n"
        self.route_boundary_drift = drift
        with self.assertRaises(migration.MigrationError):
            self.driver.restore()
        self.assertEqual(self.route.read_bytes(), drift)
        self.assertTrue((self.root / ".static-hosting-pending").exists())
        failed = self.tree(self.root)
        with self.assertRaises(migration.MigrationError):
            self.driver.restore()
        self.assertEqual(self.tree(self.root), failed)

    def test_restore_refuses_new_backend_release_state(self):
        self.driver.apply()
        self.write(self.root / "state.env", env_bytes({"RELEASE_SHA": HISTORICAL, "GATEWAY_SLOT": "green"}))
        drifted = self.tree(self.root)
        with self.assertRaises(migration.MigrationError):
            self.driver.restore()
        self.assertEqual(self.tree(self.root), drifted)

    def test_restore_without_snapshot_fails_without_mutation(self):
        with self.assertRaises(migration.MigrationError):
            self.driver.restore()
        self.assert_original_metadata()
        self.assertFalse(self.snapshot.exists())

    def assert_rejected_before_write(self):
        before = self.tree(self.root)
        for operation in (self.driver.check, self.driver.apply):
            with self.assertRaises(migration.MigrationError):
                operation()
            actual = self.tree(self.root)
            for filename, value in before.items():
                self.assertEqual(actual.get(filename), value, filename)
            self.assertFalse((self.root / ".static-hosting-pending").exists())
            self.assertFalse(self.snapshot.exists())

    def test_invalid_duplicate_and_missing_state_keys_reject_before_canonical_publish(self):
        cases = {
            "duplicate": "RELEASE_SHA=" + CURRENT + "\nRELEASE_SHA=" + PREVIOUS + "\nGATEWAY_SLOT=blue\nFRONTEND_SLOT=green\n",
            "missing": "RELEASE_SHA=" + CURRENT + "\nFRONTEND_SLOT=green\n",
            "unknown": "RELEASE_SHA=" + CURRENT + "\nGATEWAY_SLOT=blue\nFRONTEND_SLOT=green\nUNEXPECTED=bad\n",
            "bad_slot": "RELEASE_SHA=" + CURRENT + "\nGATEWAY_SLOT=orange\nFRONTEND_SLOT=green\n",
            "bad_frontend_slot": "RELEASE_SHA=" + CURRENT + "\nGATEWAY_SLOT=blue\nFRONTEND_SLOT=orange\n",
            "bad_sha": "RELEASE_SHA=not-a-sha\nGATEWAY_SLOT=blue\nFRONTEND_SLOT=green\n",
        }
        state = self.root / "state.env"
        for name, text in cases.items():
            with self.subTest(name=name):
                self.write(state, text)
                self.assert_rejected_before_write()
        self.write(state, self.before["state.env"][0])

    def test_invalid_duplicate_and_missing_runtime_keys_reject_before_canonical_publish(self):
        directory = self.root / "releases" / CURRENT
        original = (directory / "runtime.env").read_bytes()
        cases = {
            "duplicate": original + ("WORKER_IMAGE_REF=" + digest("duplicate") + "\n").encode(),
            "missing": env_bytes({key: value for key, value in self.runtime_before[CURRENT].items()
                                   if key != "WORKER_RELEASE_COMMIT"}),
            "unknown": original + b"UNEXPECTED_RUNTIME_KEY=bad\n",
            "invalid_digest": original.replace(digest("worker-runtime-recovery-" + CURRENT).encode(), b"registry/worker:latest"),
            "invalid_commit": original.replace(WORKER_COMMIT.encode(), b"bad-sha"),
        }
        for name, data in cases.items():
            with self.subTest(name=name):
                self.write(directory / "runtime.env", data)
                self.manifest(directory)
                self.assert_rejected_before_write()
        self.write(directory / "runtime.env", original)
        self.manifest(directory)

    def test_invalid_rollback_metadata_is_not_silently_ignored(self):
        self.write(self.root / "rollback/previous-state.env", "RELEASE_SHA=" + PREVIOUS + "\n")
        self.assert_rejected_before_write()

    def test_missing_canonical_state_does_not_adopt_legacy_json(self):
        (self.root / "state.env").unlink()
        self.write(self.root / "state/current-release.json", json.dumps({
            "release_sha": CURRENT, "gateway_slot": "blue", "frontend_slot": "green"}))
        self.assert_rejected_before_write()

    def test_deployment_pending_rejects_migration(self):
        self.write(self.root / ".deploy-pending", "existing deployment evidence\n")
        self.assert_rejected_before_write()

    def test_backend_identity_mismatch_rejects_before_snapshot_and_publish(self):
        self.identity_bad = True
        self.assert_rejected_before_write()

    def test_route_drift_rejects_before_snapshot_and_publish(self):
        route = legacy_route("blue", "green")
        route["http"]["routers"]["acb-public-api-router"]["middlewares"].remove("tunnel-only")
        self.write(self.route, yaml.safe_dump(route), mode=0o644)
        self.assert_rejected_before_write()

    def test_verified_bundle_checksum_tampering_rejects_before_publish(self):
        self.write(self.bundle / "deploy.sh", b"#!/bin/bash\n# tampered after verification\n", mode=0o755)
        self.assert_rejected_before_write()

    def test_apply_requires_private_valid_two_host_receipt(self):
        original = self.proof.read_bytes()
        cases = [None, {"hosts": []}, {"hosts": [{"hostname": BANK,
                 "release_sha": MIGRATION_COMMIT, "worker_version_id": VERSION,
                 "verified_at": "2026-10-04T12:00:00Z"}]}]
        valid = json.loads(original)
        for field, value in (("hostname", BANK), ("release_sha", PREVIOUS),
                             ("worker_version_id", "87654321-4321-4321-8321-cba987654321"),
                             ("verified_at", "not-a-timestamp")):
            inconsistent = copy.deepcopy(valid)
            inconsistent["hosts"][1][field] = value
            cases.append(inconsistent)
        for proof in cases:
            with self.subTest(proof=proof):
                if proof is None:
                    self.proof.unlink()
                else:
                    self.write(self.proof, json.dumps(proof))
                with self.assertRaises(migration.MigrationError):
                    self.driver.apply()
                self.assert_original_metadata()
                self.assertFalse(self.snapshot.exists())
        self.write(self.proof, original, mode=0o644)
        with self.assertRaises(migration.MigrationError):
            self.driver.apply()
        self.assert_original_metadata()
        self.assertFalse(self.snapshot.exists())

    def test_public_verification_failure_keeps_old_metadata(self):
        with patch.object(self.driver, "verify_public", side_effect=migration.MigrationError("challenge instead of release")):
            with self.assertRaises(migration.MigrationError):
                self.driver.apply()
        self.assert_original_metadata()
        self.assertFalse(self.snapshot.exists())

    def test_ack_failure_retains_journal_and_snapshot_for_restore(self):
        self.ack_bad = True
        with self.assertRaises(migration.MigrationError):
            self.driver.apply()
        self.assertTrue((self.root / ".static-hosting-pending").exists())
        self.assertTrue((self.snapshot / "manifest.json").exists())
        self.ack_bad = False
        self.driver.restore()
        self.assert_original_metadata()

    def test_snapshot_is_exclusive_and_existing_evidence_is_untouched(self):
        self.snapshot.mkdir(mode=0o700)
        evidence = self.snapshot / "operator-evidence"
        self.write(evidence, b"do not overwrite snapshot\x00")
        with self.assertRaises(migration.MigrationError):
            self.driver.apply()
        self.assertEqual(evidence.read_bytes(), b"do not overwrite snapshot\x00")
        self.assert_original_metadata()


if __name__ == "__main__":
    unittest.main()
