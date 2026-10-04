#!/usr/bin/env python3
"""One-shot, metadata-only static-hosting cutover; never operates on database data.

A private snapshot is both the before-image evidence and the compare-before-write
journal. Interrupted operations must be restored, not resumed. Public proof is an
operator receipt, not authorization: route/account/browser checks stay off the VPS.
"""
from __future__ import annotations

import argparse
import contextlib
from datetime import datetime, timezone
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import sys
import tempfile
import urllib.error
import urllib.parse
import urllib.request
import uuid

import yaml


class MigrationError(Exception):
    pass


SHA = re.compile(r"[0-9a-f]{40}")
DIGEST = re.compile(r"[a-z0-9][a-z0-9./_-]*@sha256:[0-9a-f]{64}")
STATE_KEYS = {"RELEASE_SHA", "GATEWAY_SLOT"}
IMAGE_KEYS = {"RELEASE_SHA", "GATEWAY_IMAGE_REF", "WORKER_IMAGE_REF", "DBTOOL_IMAGE_REF",
              "BROWSER_IMAGE_REF", "TTS_IMAGE_REF", "BARK_IMAGE_REF"}
RUNTIME_KEYS = {"IMAGE_REF_BLUE", "IMAGE_REF_GREEN", "WORKER_IMAGE_REF", "BROWSER_IMAGE_REF",
                "TTS_IMAGE_REF", "BARK_IMAGE_REF", "DBTOOL_IMAGE_REF", "RELEASE_COMMIT_BLUE",
                "RELEASE_COMMIT_GREEN", "WORKER_RELEASE_COMMIT", "ENV_FILE", "SECRETS_DIR",
                "BARK_SECRET_GROUP"}
LEGACY_KEYS = {"state": {"FRONTEND_SLOT"}, "images": {"FRONTEND_IMAGE_REF"},
               "runtime": {"FRONTEND_IMAGE_REF_BLUE", "FRONTEND_IMAGE_REF_GREEN"}}
SCRIPTS = ("deploy.sh", "simple-lib.sh", "healthcheck.sh", "render-route.sh")
OBSOLETE = {"import-baseline.py", "baseline-frontend.sha256", "frontend-sws.toml"}
HOSTS = {"bank.tuannguyenviet.site", "transactions.tuannguyenviet.site"}
PROVENANCE = "static-hosting-provenance.json"


class UniqueLoader(yaml.SafeLoader):
    pass


def unique_mapping(loader, node):
    result = {}
    for key_node, value_node in node.value:
        key = loader.construct_object(key_node)
        if key in result:
            raise MigrationError("duplicate YAML key: " + str(key))
        result[key] = loader.construct_object(value_node)
    return result


UniqueLoader.add_constructor(yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, unique_mapping)


def unique_json(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise MigrationError("duplicate JSON metadata key: " + key)
        result[key] = value
    return result


def yaml_data(data):
    try:
        result = yaml.load(data, Loader=UniqueLoader)
    except (yaml.YAMLError, UnicodeError) as exc:
        raise MigrationError("invalid YAML metadata") from exc
    if not isinstance(result, dict):
        raise MigrationError("YAML metadata must be an object")
    return result


def digest(data):
    return hashlib.sha256(data).hexdigest()


def json_bytes(value):
    return (json.dumps(value, sort_keys=True, indent=2) + "\n").encode()


def regular(path):
    # Reject symlinks in every component, including parent directories.
    for component in (path, *path.parents):
        if component.is_symlink():
            raise MigrationError("symlink metadata path: " + str(path))
    if not path.is_file() or not stat.S_ISREG(path.stat().st_mode):
        raise MigrationError("missing regular metadata file: " + str(path))
    return path.read_bytes()


def sync_directory(path):
    fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def atomic_replace(path, data, mode, uid=None, gid=None):
    fd, temporary = tempfile.mkstemp(prefix=".static-hosting-", dir=path.parent)
    try:
        os.fchmod(fd, mode)
        if uid is not None:
            os.fchown(fd, uid, gid)
        with os.fdopen(fd, "wb") as stream:
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
        sync_directory(path.parent)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def private_new(path, data):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, "wb") as stream:
        os.fchmod(stream.fileno(), 0o600)
        stream.write(data)
        stream.flush()
        os.fsync(stream.fileno())
    sync_directory(path.parent)


def env_values(data, required=None, legacy=None):
    if data is None:
        raise MigrationError("missing environment metadata")
    try:
        text = data.decode("utf-8")
    except UnicodeError as exc:
        raise MigrationError("invalid metadata encoding") from exc
    values = {}
    for line in text.splitlines():
        if required is None and (not line.strip() or line.lstrip().startswith("#")):
            continue
        key, separator, value = line.partition("=")
        if (not separator or not re.fullmatch(r"[A-Z][A-Z0-9_]*", key) or key in values
                or "\r" in line or "\x00" in line):
            raise MigrationError("invalid or duplicate metadata key: " + key)
        if required is None:
            # dotenv permits optional blank values and quoted display strings;
            # they are not evaluated or rewritten by this migration.
            value = value.strip().strip("\"'")
        elif not value or any(c.isspace() for c in value):
            raise MigrationError("empty or unsafe metadata value: " + key)
        values[key] = value
    if required is not None:
        allowed = required | (legacy or set())
        if not required <= values.keys() or not values.keys() <= allowed:
            raise MigrationError("missing or unexpected metadata keys")
        if legacy and values.keys() & legacy and not legacy <= values.keys():
            raise MigrationError("incomplete legacy frontend metadata")
    return values


def backend_env(data):
    return b"".join(line for line in data.splitlines(keepends=True)
                    if not line.split(b"=", 1)[0].startswith(b"FRONTEND_"))


def manifest(data):
    if data is None:
        raise MigrationError("missing checksum manifest")
    entries = {}
    try:
        lines = data.decode("utf-8").splitlines()
    except UnicodeError as exc:
        raise MigrationError("invalid checksum manifest") from exc
    for line in lines:
        match = re.fullmatch(r"([0-9a-f]{64}) [ *](.+)", line)
        if not match:
            raise MigrationError("invalid checksum manifest entry")
        checksum, name = match.groups()
        path = Path(name)
        if (path.is_absolute() or ".." in path.parts or "\\" in name or name in entries
                or name in (".", "SHA256SUMS") or path.as_posix() != name):
            raise MigrationError("unsafe or duplicate checksum member")
        entries[name] = checksum
    if not entries:
        raise MigrationError("empty checksum manifest")
    return entries


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


class Migration:
    def __init__(self, root: Path, bundle: Path, snapshot: Path, proof: Path | None = None,
                 runner=subprocess.run):
        self.root, self.bundle, self.snapshot = map(Path, (root, bundle, snapshot))
        self.proof = Path(proof) if proof is not None else None
        for path in (self.root, self.bundle, self.snapshot):
            if not path.is_absolute():
                raise MigrationError("root, bundle and snapshot must be absolute")
            for part in (path, *path.parents):
                if part.is_symlink():
                    raise MigrationError("symlink migration directory")
        if self.bundle == self.root or self.bundle.is_relative_to(self.root / "releases"):
            raise MigrationError("verified bundle must be staged separately from live releases")
        if self.snapshot.is_relative_to(self.root / "releases") or self.snapshot == self.root:
            raise MigrationError("snapshot must be outside release metadata")
        self.runner = runner
        self.marker = self.root / ".static-hosting-pending"
        self.entries = {}
        self.environment = dict(os.environ)
        # No CLI/environment override of proof or operational paths.
        for key in ("PUBLIC_ORIGIN", "PUBLIC_VIEWER_ORIGIN", "ACB_ROUTE_FILE", "ACB_CONFIG"):
            self.environment.pop(key, None)

    def command(self, args, **kwargs):
        try:
            result = self.runner([str(a) for a in args], capture_output=True, text=True,
                                 timeout=180, env=self.environment, **kwargs)
        except (OSError, subprocess.SubprocessError) as exc:
            raise MigrationError("migration boundary command unavailable: " + str(args[0])) from exc
        if result.returncode:
            # Do not emit subprocess output: Compose/env/HTTP errors can disclose secrets.
            raise MigrationError("migration boundary command failed: " + str(args[0]))
        return result.stdout

    @contextlib.contextmanager
    def locks(self):
        with contextlib.ExitStack() as stack:
            for name in (".recovery-setup.lock", ".deploy.lock"):
                fd = os.open(self.root / name, os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
                stack.callback(os.close, fd)
                try:
                    fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                except BlockingIOError as exc:
                    raise MigrationError("deployment/recovery operation already running") from exc
            yield

    def remember(self, path):
        path = Path(path)
        key = str(path)
        if key not in self.entries:
            if path.exists() or path.is_symlink():
                data = regular(path)
                st = path.stat()
                self.entries[key] = {"path": key, "before": data, "after": data,
                                     "mode": stat.S_IMODE(st.st_mode), "uid": st.st_uid, "gid": st.st_gid}
            else:
                self.entries[key] = {"path": key, "before": None, "after": None,
                                     "mode": 0o600, "uid": os.getuid(), "gid": os.getgid()}
        return self.entries[key]["before"]

    def change(self, path, data):
        self.remember(path)
        self.entries[str(path)]["after"] = data

    def checked_manifest(self, directory, watch=False):
        data = self.remember(directory / "SHA256SUMS") if watch else regular(directory / "SHA256SUMS")
        members = manifest(data)
        for name, checksum in members.items():
            content = self.remember(directory / name) if watch else regular(directory / name)
            if content is None or digest(content) != checksum:
                raise MigrationError("missing or changed checksum member: " + str(directory / name))
        return data, members

    def load_settings(self):
        data = self.remember(self.root / "deploy/.env.production")
        self.settings = env_values(data)
        for key, default in (("PUBLIC_ORIGIN", "https://bank.tuannguyenviet.site"),
                             ("PUBLIC_VIEWER_ORIGIN", "https://transactions.tuannguyenviet.site")):
            self.environment[key] = self.settings.get(key, default)
        self.route = Path(self.settings.get("ACB_ROUTE_FILE", self.settings.get(
            "ACB_CONFIG", "/opt/platform/edge/dynamic/acb.yml")))
        if not self.route.is_absolute() or self.route.name != "acb.yml":
            raise MigrationError("invalid app-owned route path")
        if self.route == Path("/opt/platform/edge/dynamic/acb.yml"):
            regular(self.route)
            st = self.route.stat()
            if (stat.S_IMODE(st.st_mode), st.st_uid, st.st_gid) != (0o644, 0, 0):
                raise MigrationError("root-owned route publisher handoff required before metadata migration")
            for ancestor in self.route.parents:
                st = ancestor.stat()
                if not stat.S_ISDIR(st.st_mode) or st.st_uid != 0 or st.st_mode & 0o022:
                    raise MigrationError("root-owned route publisher ancestor handoff required: " + str(ancestor))

    def verify_bundle(self):
        self.bundle_checksum, members = self.checked_manifest(self.bundle)
        if not set(SCRIPTS) | {"images.env", "compose.prod.yaml"} <= members.keys():
            raise MigrationError("verified bundle lacks covered operational metadata")
        images = env_values(regular(self.bundle / "images.env"), IMAGE_KEYS)
        self.validate_images(images)
        self.migration_commit = images["RELEASE_SHA"]
        deploy = regular(self.bundle / "deploy.sh").decode("utf-8")
        for name, expected in (("STATE_KEYS", STATE_KEYS), ("IMAGES", IMAGE_KEYS), ("RUNTIME_KEYS", RUNTIME_KEYS)):
            matches = re.findall(r"^" + name + r"=['\"]([^'\"]+)['\"]\s*$", deploy, re.MULTILINE)
            if len(matches) != 1 or set(matches[0].split()) != expected:
                raise MigrationError("verified deployment script metadata schema mismatch: " + name)
        self.script_bytes = {name: regular(self.bundle / name) for name in SCRIPTS}
        composition = yaml_data(regular(self.bundle / "compose.prod.yaml"))
        if not isinstance(composition.get("services"), dict) or any(
                name.startswith("frontend-") for name in composition["services"]):
            raise MigrationError("verified bundle is not backend-only")

    @staticmethod
    def validate_images(images):
        if not SHA.fullmatch(images["RELEASE_SHA"]):
            raise MigrationError("invalid release SHA")
        for key, value in images.items():
            if key != "RELEASE_SHA" and not DIGEST.fullmatch(value):
                raise MigrationError("invalid immutable image reference: " + key)

    def state(self, path):
        data = self.remember(path)
        if data is None:
            raise MigrationError("BACKEND_STATE_MIGRATION_REQUIRED: canonical state missing")
        values = env_values(data, STATE_KEYS, LEGACY_KEYS["state"])
        if not SHA.fullmatch(values["RELEASE_SHA"]) or any(
                values[key] not in ("blue", "green") for key in values if key.endswith("_SLOT")):
            raise MigrationError("invalid canonical state")
        return values

    def runtime(self, release):
        values = env_values(self.remember(release / "runtime.env"), RUNTIME_KEYS, LEGACY_KEYS["runtime"])
        for key, value in values.items():
            if "IMAGE_REF" in key and not DIGEST.fullmatch(value):
                raise MigrationError("invalid runtime digest: " + key)
            if "COMMIT" in key and not SHA.fullmatch(value):
                raise MigrationError("invalid runtime commit: " + key)
        if (values["ENV_FILE"] != str(self.root / "deploy/.env.production")
                or values["SECRETS_DIR"] != str(self.root / "deploy/secrets")
                or values["BARK_SECRET_GROUP"] != "1000"):
            raise MigrationError("runtime paths/group mismatch")
        return values

    def render(self, slot):
        text = self.command(["bash", self.bundle / "render-route.sh", slot])
        data = text.encode()
        rendered = yaml_data(data)
        http = rendered.get("http", {})
        if (set(http.get("routers", {})) != {"acb-deny-internal", "acb-public-deny-private",
                "acb-public-sse-router", "acb-public-api-router", "acb-api-router", "acb-deploy-gateway"}
                or set(http.get("services", {})) != {"acb-service"}):
            raise MigrationError("verified renderer is not backend-only")
        return data

    def check_route(self, before, expected, legacy):
        actual, target = yaml_data(before), yaml_data(expected)
        if not legacy:
            if actual != target:
                raise MigrationError("backend route drift")
            return
        old_http = actual.get("http", {})
        wanted = target["http"]
        extras = {"routers": {"acb-credentials-router", "acb-public-frontend-router", "acb-frontend-router",
                              "acb-deploy-frontend"},
                  "services": {"acb-frontend-service"}, "middlewares": {"acb-credentials-security"}}
        if set(actual) != {"http"} or not set(old_http) <= {"routers", "services", "middlewares"}:
            raise MigrationError("legacy route has unknown topology")
        for section in ("routers", "services", "middlewares"):
            found, required = old_http.get(section, {}), wanted.get(section, {})
            if not isinstance(found, dict) or not set(found) <= set(required) | extras[section]:
                raise MigrationError("legacy route contains unexpected objects")
            if any(found.get(name) != value for name, value in required.items()):
                raise MigrationError("backend route policy/slot drift")

    def project_release(self, sha):
        release = self.root / "releases" / sha
        original_manifest, members = self.checked_manifest(release, watch=True)
        images = env_values(self.remember(release / "images.env"), IMAGE_KEYS, LEGACY_KEYS["images"])
        self.validate_images(images)
        if images["RELEASE_SHA"] != sha:
            raise MigrationError("release images manifest identity mismatch")
        runtime = self.runtime(release)
        composition = yaml_data(self.remember(release / "compose.prod.yaml"))
        services = composition.get("services")
        if not isinstance(services, dict) or not services:
            raise MigrationError("missing Compose services")
        for name in ("frontend-blue", "frontend-green"):
            services.pop(name, None)
        if any(name.startswith("frontend-") for name in services):
            raise MigrationError("unknown frontend service")
        for name in ("images.env", "runtime.env"):
            self.change(release / name, backend_env(self.entries[str(release / name)]["before"]))
        self.change(release / "compose.prod.yaml", yaml.safe_dump(composition, sort_keys=False).encode())
        for name in SCRIPTS:
            self.change(release / name, self.script_bytes[name])
            if self.entries[str(release / name)]["before"] is None:
                raise MigrationError("source release lacks operational script: " + name)
        for name in OBSOLETE:
            self.remember(release / name)
            self.change(release / name, None)
        provenance = {"source_release_sha": sha, "source_checksum_manifest_sha256": digest(original_manifest),
                      "migration_commit": self.migration_commit, "snapshot_path": str(self.snapshot)}
        if (release / PROVENANCE).exists():
            raise MigrationError("release already has migration provenance without matching receipt")
        self.change(release / PROVENANCE, json_bytes(provenance))
        names = (set(members) - OBSOLETE) | set(SCRIPTS) | {"images.env", "compose.prod.yaml", PROVENANCE}
        checksums = []
        for name in sorted(names):
            data = self.entries[str(release / name)]["after"]
            if data is None:
                raise MigrationError("missing projected checksum member")
            checksums.append(digest(data) + "  " + name + "\n")
        self.change(release / "SHA256SUMS", "".join(checksums).encode())
        return runtime, composition

    def inspect_backends(self, runtime, composition):
        slot = self.current["GATEWAY_SLOT"]
        if runtime["RELEASE_COMMIT_" + slot.upper()] != self.current["RELEASE_SHA"]:
            raise MigrationError("active runtime commit disagrees with canonical state")
        services = [("gateway-" + slot, runtime["IMAGE_REF_" + slot.upper()], self.current["RELEASE_SHA"]),
                    ("worker", runtime["WORKER_IMAGE_REF"], runtime["WORKER_RELEASE_COMMIT"]),
                    ("auth-browser", runtime["BROWSER_IMAGE_REF"], None),
                    ("tts-gateway", runtime["TTS_IMAGE_REF"], None),
                    ("bark", runtime["BARK_IMAGE_REF"], None)]
        if self.settings.get("AUTH_RECOVERY_ENABLED", "false") == "true":
            if "recovery-controller" not in composition["services"]:
                raise MigrationError("enabled recovery controller absent from Compose")
            services.append(("recovery-controller", runtime["WORKER_IMAGE_REF"], runtime["WORKER_RELEASE_COMMIT"]))
        identities = {}
        for name, reference, commit in services:
            if name not in composition["services"]:
                raise MigrationError("running backend absent from Compose: " + name)
            try:
                obj = json.loads(self.command(["docker", "inspect", "acb-" + name]))[0]
                image = json.loads(self.command(["docker", "image", "inspect", reference]))[0]
                if (not obj["State"]["Running"] or obj["State"]["Health"]["Status"] != "healthy"
                        or obj["Config"]["Image"] != reference or obj["Image"] != image["Id"]):
                    raise MigrationError("running backend image/health mismatch: " + name)
                variables = {}
                for line in obj["Config"].get("Env", []):
                    key, _, value = line.partition("=")
                    if key in variables:
                        raise MigrationError("duplicate running backend environment")
                    variables[key] = value
                if commit and variables.get("RELEASE_COMMIT") != commit:
                    raise MigrationError("running backend commit mismatch: " + name)
                labels = obj["Config"].get("Labels") or {}
                if labels and (labels.get("com.docker.compose.project") != "acb"
                               or labels.get("com.docker.compose.service") != name):
                    raise MigrationError("running backend ownership mismatch")
                if name.startswith("gateway-") and any(
                        variables.get(key, slot) != slot for key in ("GATEWAY_SLOT", "PLATFORM_SLOT")):
                    raise MigrationError("running gateway slot mismatch")
                identities[name] = {"id": obj["Id"], "image": obj["Image"], "reference": reference}
            except (ValueError, KeyError, IndexError, TypeError) as exc:
                raise MigrationError("invalid Docker identity response") from exc
        return identities

    def ack(self, state):
        self.command(["bash", self.bundle / "healthcheck.sh", "route", state["GATEWAY_SLOT"], state["RELEASE_SHA"]])

    def prepare(self):
        self.entries = {}
        if (self.root / ".deploy-pending").exists():
            raise MigrationError("deployment pending; finish deployment before migration")
        if self.marker.exists():
            raise MigrationError("STATIC_HOSTING_MIGRATION_PENDING: restore snapshot first")
        self.load_settings()
        self.verify_bundle()
        self.current = self.state(self.root / "state.env")
        states = [(self.root / "state.env", self.current)]
        rollback = self.root / "rollback"
        if rollback.exists():
            previous = self.state(rollback / "previous-state.env")
            target = self.state(rollback / "target-state.env")
            if self.current != previous and self.current != target:
                raise MigrationError("canonical/rollback snapshot state mismatch")
            states.extend(((rollback / "previous-state.env", previous), (rollback / "target-state.env", target)))
        projections = {}
        for _, state in states:
            sha = state["RELEASE_SHA"]
            if sha not in projections:
                projections[sha] = self.project_release(sha)
            if projections[sha][0]["RELEASE_COMMIT_" + state["GATEWAY_SLOT"].upper()] != sha:
                raise MigrationError("referenced rollback runtime identity mismatch")
        for path, state in states:
            self.change(path, backend_env(self.entries[str(path)]["before"]))
        for path, state in [(self.route, self.current)] + (
                [(rollback / "previous-acb.yml", previous)] if rollback.exists() else []):
            before = self.remember(path)
            rendered = self.render(state["GATEWAY_SLOT"])
            if before is None:
                raise MigrationError("missing live/rollback route metadata")
            self.check_route(before, rendered, "FRONTEND_SLOT" in state)
            self.change(path, rendered)
        self.current_runtime, self.current_composition = projections[self.current["RELEASE_SHA"]]
        self.identities = self.inspect_backends(self.current_runtime, self.current_composition)
        self.ack(self.current)
        self.projections = projections
        return {"release_sha": self.current["RELEASE_SHA"], "gateway_slot": self.current["GATEWAY_SLOT"],
                "migration_commit": self.migration_commit, "releases": sorted(projections)}

    def read_proof(self):
        if self.proof is None or not self.proof.is_absolute():
            raise MigrationError("apply requires absolute operator proof path")
        data = regular(self.proof)
        if stat.S_IMODE(self.proof.stat().st_mode) != 0o600:
            raise MigrationError("operator proof must have mode 0600")
        try:
            proof = json.loads(data, object_pairs_hook=unique_json)
            if not isinstance(proof, dict) or set(proof) != {"hosts"} or len(proof["hosts"]) != 2:
                raise ValueError()
            seen, releases, versions = set(), set(), set()
            for host in proof["hosts"]:
                if set(host) != {"hostname", "release_sha", "worker_version_id", "verified_at"}:
                    raise ValueError()
                if host["hostname"] not in HOSTS or host["hostname"] in seen:
                    raise ValueError()
                seen.add(host["hostname"])
                if not SHA.fullmatch(host["release_sha"]):
                    raise ValueError()
                version = uuid.UUID(host["worker_version_id"])
                if str(version) != host["worker_version_id"]:
                    raise ValueError()
                timestamp = host["verified_at"]
                if not re.fullmatch(r"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?(?:Z|\+00:00)", timestamp):
                    raise ValueError()
                datetime.fromisoformat(timestamp.replace("Z", "+00:00"))
                releases.add(host["release_sha"])
                versions.add(str(version))
            if seen != HOSTS or len(releases) != 1 or len(versions) != 1:
                raise ValueError()
        except (ValueError, KeyError, TypeError, AttributeError) as exc:
            raise MigrationError("invalid operator proof schema/identity/UTC timestamp") from exc
        return proof

    def verify_public(self, proof):
        opener = urllib.request.build_opener(NoRedirect())
        sha = proof["hosts"][0]["release_sha"]
        try:
            request = urllib.request.Request("https://transactions.tuannguyenviet.site/__release?smoke=" + sha,
                                             headers={"Accept": "text/plain"})
            with opener.open(request, timeout=15) as response:
                if (response.status != 200 or response.headers.get_content_type() != "text/plain"
                        or response.read(128) != (sha + "\n").encode()
                        or "no-store" not in response.headers.get("Cache-Control", "").lower()):
                    raise MigrationError("public viewer release is not verified Worker identity")
            try:
                with opener.open("https://bank.tuannguyenviet.site/", timeout=15):
                    raise MigrationError("bank Access redirect missing")
            except urllib.error.HTTPError as response:
                location = urllib.parse.urlsplit(response.headers.get("Location", ""))
                if (response.code != 302 or location.scheme != "https"
                        or location.netloc != "thedemontuan.cloudflareaccess.com"
                        or not location.path.startswith("/cdn-cgi/access/login/")):
                    raise MigrationError("bank Access redirect mismatch")
        except (urllib.error.URLError, OSError, ValueError) as exc:
            raise MigrationError("public acceptance unavailable; do not relax edge controls") from exc

    def compare(self, entry, expected):
        path = Path(entry["path"])
        if expected is None:
            if path.exists() or path.is_symlink():
                raise MigrationError("metadata file drift: " + str(path))
            return
        data = regular(path)
        st = path.stat()
        if (digest(data) != expected or stat.S_IMODE(st.st_mode) != entry["mode"]
                or st.st_uid != entry["uid"] or st.st_gid != entry["gid"]):
            raise MigrationError("metadata file drift: " + str(path))

    def check_drift(self):
        if (self.root / ".deploy-pending").exists():
            raise MigrationError("deployment pending")
        for entry in self.entries.values():
            self.compare(entry, digest(entry["before"]) if entry["before"] is not None else None)
        if regular(self.bundle / "SHA256SUMS") != self.bundle_checksum:
            raise MigrationError("verified bundle changed during migration")
        self.checked_manifest(self.bundle)

    def snapshot_plan(self, summary, proof):
        try:
            self.snapshot.mkdir(mode=0o700)
        except FileExistsError as exc:
            raise MigrationError("snapshot must be new and exclusive") from exc
        os.chmod(self.snapshot, 0o700)
        for name in ("originals", "staged", "validation"):
            (self.snapshot / name).mkdir(mode=0o700)
        files = []
        for index, entry in enumerate(self.entries.values()):
            record = {key: entry[key] for key in ("path", "mode", "uid", "gid")}
            for kind, directory in (("before", "originals"), ("after", "staged")):
                data = entry[kind]
                record[kind + "_sha256"] = digest(data) if data is not None else None
                record[kind + "_file"] = directory + "/" + f"{index:04d}" if data is not None else None
                if data is not None:
                    private_new(self.snapshot / record[kind + "_file"], data)
            record["changed"] = entry["before"] != entry["after"]
            files.append(record)
        # Keep canonical state publish last, even if dictionary insertion order changes.
        files.sort(key=lambda entry: entry["path"] == str(self.root / "state.env"))
        plan = {"schema": 1, "root": str(self.root), "snapshot": str(self.snapshot), "route": str(self.route),
                "bundle": str(self.bundle), "bundle_checksum_sha256": digest(self.bundle_checksum),
                "summary": summary, "state": self.current, "backend_identities": self.identities,
                "proof": proof, "files": files}
        private_new(self.snapshot / "manifest.json", json_bytes(plan))
        return plan

    def validate_staged(self, plan):
        for sha in self.projections:
            source = self.root / "releases" / sha
            projected = self.entries[str(source / "SHA256SUMS")]["after"]
            for name, checksum in manifest(projected).items():
                content = self.entries[str(source / name)]["after"]
                if content is None or digest(content) != checksum:
                    raise MigrationError("projected checksum manifest mismatch")
            env_values(self.entries[str(source / "images.env")]["after"], IMAGE_KEYS)
            env_values(self.entries[str(source / "runtime.env")]["after"], RUNTIME_KEYS)
            directory = self.snapshot / "validation" / sha
            directory.mkdir(mode=0o700)
            # Relative Compose support files keep their original release location;
            # project-directory also preserves relative mounts/env_file semantics.
            for name in ("runtime.env", "compose.prod.yaml"):
                private_new(directory / name, self.entries[str(source / name)]["after"])
            self.command(["docker", "compose", "--project-name", "acb", "--project-directory", source,
                          "--env-file", self.root / "deploy/.env.production", "--env-file", directory / "runtime.env",
                          "-f", directory / "compose.prod.yaml", "config", "--quiet"])
        for record in plan["files"]:
            data_path = self.snapshot / record["after_file"] if record["after_file"] else None
            if data_path is not None and digest(regular(data_path)) != record["after_sha256"]:
                raise MigrationError("staged checksum mismatch")
            if Path(record["path"]) == self.route or record["path"] == str(self.root / "rollback/previous-acb.yml"):
                slot = self.current["GATEWAY_SLOT"] if Path(record["path"]) == self.route else self.state(
                    self.root / "rollback/previous-state.env")["GATEWAY_SLOT"]
                self.command(["bash", "-c", 'source "$1/simple-lib.sh"; validate_route "$2" "$3"',
                              "migration", self.bundle, data_path, slot])

    def replace_file(self, entry, restoring=False):
        direction = "before" if restoring else "after"
        path = Path(entry["path"])
        stored = entry[direction + "_file"]
        if path == self.route:
            if stored is None:
                raise MigrationError("app-owned live route may not be removed")
            expected = entry["after_sha256" if restoring else "before_sha256"]
            self.command(["bash", "-c", 'source "$1/simple-lib.sh"; route_replace "$2" "$3" "$4"',
                          "migration", self.bundle, self.snapshot / stored, self.route, expected])
            return
        if stored is None:
            if path.exists():
                path.unlink()
                sync_directory(path.parent)
        else:
            atomic_replace(path, regular(self.snapshot / stored), entry["mode"], entry["uid"], entry["gid"])

    def check(self):
        if (self.snapshot / "receipt.json").exists():
            return self.completed()
        summary = self.prepare()
        self.check_drift()
        return summary

    def apply(self):
        with self.locks():
            if self.marker.exists():
                raise MigrationError("STATIC_HOSTING_MIGRATION_PENDING: restore snapshot first")
            if (self.snapshot / "receipt.json").exists():
                return self.completed()
            if self.snapshot.exists():
                raise MigrationError("snapshot already exists; never reuse cutover evidence")
            summary = self.prepare()
            proof = self.read_proof()
            self.verify_public(proof)
            self.check_drift()
            plan = self.snapshot_plan(summary, proof)
            self.validate_staged(plan)
            self.check_drift()
            journal = {"schema": 1, "snapshot": str(self.snapshot), "manifest_sha256": digest(regular(
                self.snapshot / "manifest.json")), "operation": "apply"}
            private_new(self.marker, json_bytes(journal))
            for entry in plan["files"]:
                if not entry["changed"]:
                    continue
                self.compare(entry, entry["before_sha256"])
                self.replace_file(entry)
            self.verify_post(plan)
            self.ack(self.current)
            if self.inspect_backends(self.current_runtime, self.current_composition) != self.identities:
                raise MigrationError("running backend changed during metadata-only migration")
            receipt = {"schema": 1, "status": "complete", "manifest_sha256": journal["manifest_sha256"],
                       "completed_at": datetime.now(timezone.utc).isoformat(), **summary}
            private_new(self.snapshot / "receipt.json", json_bytes(receipt))
            self.marker.unlink()
            sync_directory(self.root)
            return receipt

    def load_plan(self):
        if not self.snapshot.is_dir() or stat.S_IMODE(self.snapshot.stat().st_mode) != 0o700:
            raise MigrationError("private snapshot directory 0700 required")
        try:
            raw = regular(self.snapshot / "manifest.json")
            if stat.S_IMODE((self.snapshot / "manifest.json").stat().st_mode) != 0o600:
                raise MigrationError("snapshot manifest must be private")
            plan = json.loads(raw)
            if (plan["schema"] != 1 or plan["root"] != str(self.root)
                    or plan["snapshot"] != str(self.snapshot) or plan["bundle"] != str(self.bundle)):
                raise MigrationError("snapshot belongs to another migration")
            paths = set()
            for entry in plan["files"]:
                path = Path(entry["path"])
                if not path.is_absolute() or entry["path"] in paths or not (
                        path.is_relative_to(self.root) or entry["path"] == plan["route"]):
                    raise MigrationError("unsafe snapshot metadata path")
                paths.add(entry["path"])
                for kind, directory in (("before", "originals"), ("after", "staged")):
                    name = entry[kind + "_file"]
                    checksum = entry[kind + "_sha256"]
                    if name is None:
                        if checksum is not None:
                            raise MigrationError("invalid missing snapshot byte evidence")
                        continue
                    if not re.fullmatch(directory + r"/\d{4}", name):
                        raise MigrationError("unsafe snapshot evidence path")
                    evidence = self.snapshot / name
                    if (stat.S_IMODE(evidence.stat().st_mode) != 0o600
                            or digest(regular(evidence)) != checksum):
                        raise MigrationError("snapshot byte evidence drift")
            self.load_settings()
            self.verify_bundle()
            if (digest(self.bundle_checksum) != plan["bundle_checksum_sha256"]
                    or str(self.route) != plan["route"]):
                raise MigrationError("migration controller/route drift")
            return plan, digest(raw)
        except (OSError, ValueError, KeyError, TypeError) as exc:
            raise MigrationError("invalid or missing migration snapshot") from exc

    def verify_post(self, plan):
        for entry in plan["files"]:
            self.compare(entry, entry["after_sha256"])
        for sha in plan["summary"]["releases"]:
            self.checked_manifest(self.root / "releases" / sha)
        for path in (self.root / "state.env", self.root / "rollback/previous-state.env",
                     self.root / "rollback/target-state.env"):
            if path.exists():
                env_values(regular(path), STATE_KEYS)
        state = env_values(regular(self.root / "state.env"), STATE_KEYS)
        self.command(["bash", "-c", 'source "$1/simple-lib.sh"; validate_route "$2" "$3"',
                      "migration", self.bundle, plan["route"], state["GATEWAY_SLOT"]])

    def completed(self):
        if self.marker.exists() or (self.root / ".deploy-pending").exists():
            raise MigrationError("pending operation; completed receipt is not sufficient")
        plan, checksum = self.load_plan()
        try:
            receipt = json.loads(regular(self.snapshot / "receipt.json"))
            if receipt["status"] != "complete" or receipt["manifest_sha256"] != checksum:
                raise MigrationError("migration receipt mismatch")
        except (ValueError, KeyError, TypeError) as exc:
            raise MigrationError("invalid migration receipt") from exc
        self.verify_post(plan)
        self.current = env_values(regular(self.root / "state.env"), STATE_KEYS)
        release = self.root / "releases" / self.current["RELEASE_SHA"]
        self.entries = {}
        runtime = self.runtime(release)
        composition = yaml_data(regular(release / "compose.prod.yaml"))
        if self.inspect_backends(runtime, composition) != plan["backend_identities"]:
            raise MigrationError("backend identity drift since migration receipt")
        self.ack(self.current)
        return {**receipt, "noop": True}

    def restore(self):
        with self.locks():
            if (self.root / ".deploy-pending").exists():
                raise MigrationError("deployment pending; metadata restore prohibited")
            plan, checksum = self.load_plan()
            if (self.snapshot / "restore-receipt.json").exists() and not self.marker.exists():
                for entry in plan["files"]:
                    self.compare(entry, entry["before_sha256"])
                self.ack(plan["state"])
                return {"status": "restored", "noop": True}
            published = self.marker.exists() or (self.snapshot / "receipt.json").exists()
            if self.marker.exists():
                try:
                    journal = json.loads(regular(self.marker))
                    if journal["snapshot"] != str(self.snapshot) or journal["manifest_sha256"] != checksum:
                        raise MigrationError("pending journal belongs to another migration")
                except (ValueError, KeyError, TypeError) as exc:
                    raise MigrationError("invalid pending migration journal") from exc
            if not self.marker.exists() and (self.snapshot / "receipt.json").exists():
                try:
                    receipt = json.loads(regular(self.snapshot / "receipt.json"))
                    if receipt["status"] != "complete" or receipt["manifest_sha256"] != checksum:
                        raise MigrationError("migration receipt/evidence drift")
                except (ValueError, KeyError, TypeError) as exc:
                    raise MigrationError("invalid migration completion receipt") from exc
            # Preflight the whole set before restoring one byte. A crash between a
            # replace and journal update is safe: each file has only two legal hashes.
            changed = []
            for entry in plan["files"]:
                try:
                    self.compare(entry, entry["before_sha256"])
                except MigrationError:
                    self.compare(entry, entry["after_sha256"])
                    if entry["changed"]:
                        changed.append(entry)
            if not published and changed:
                raise MigrationError("metadata changed without a publication journal; retain evidence")
            state_entry = next(entry for entry in plan["files"] if entry["path"] == str(self.root / "state.env"))
            live_state = env_values(regular(self.root / "state.env"), STATE_KEYS, LEGACY_KEYS["state"])
            if any(live_state.get(key) != plan["state"].get(key) for key in STATE_KEYS):
                raise MigrationError("backend release changed; use normal backend rollback, not metadata restore")
            # Current running backend identity must still match; no database rollback.
            self.current = plan["state"]
            source = self.root / "releases" / self.current["RELEASE_SHA"]
            runtime_record = next(entry for entry in plan["files"] if entry["path"] == str(source / "runtime.env"))
            compose_record = next(entry for entry in plan["files"] if entry["path"] == str(source / "compose.prod.yaml"))
            runtime = env_values(regular(self.snapshot / runtime_record["before_file"]), RUNTIME_KEYS, LEGACY_KEYS["runtime"])
            composition = yaml_data(regular(self.snapshot / compose_record["before_file"]))
            if self.inspect_backends(runtime, composition) != plan["backend_identities"]:
                raise MigrationError("running backend drift; metadata restore prohibited")
            restore_journal = {"schema": 1, "snapshot": str(self.snapshot), "manifest_sha256": checksum,
                               "operation": "restore"}
            if self.marker.exists():
                atomic_replace(self.marker, json_bytes(restore_journal), 0o600)
            else:
                private_new(self.marker, json_bytes(restore_journal))
            changed.sort(key=lambda entry: entry is state_entry)
            for entry in changed:
                self.compare(entry, entry["after_sha256"])
                self.replace_file(entry, restoring=True)
            for entry in plan["files"]:
                self.compare(entry, entry["before_sha256"])
            self.ack(plan["state"])
            receipt = {"schema": 1, "status": "restored", "manifest_sha256": checksum,
                       "restored_at": datetime.now(timezone.utc).isoformat()}
            if (self.snapshot / "restore-receipt.json").exists():
                atomic_replace(self.snapshot / "restore-receipt.json", json_bytes(receipt), 0o600)
            else:
                private_new(self.snapshot / "restore-receipt.json", json_bytes(receipt))
            self.marker.unlink()
            sync_directory(self.root)
            return receipt


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", required=True, type=Path)
    parser.add_argument("--bundle", required=True, type=Path)
    parser.add_argument("--snapshot", required=True, type=Path)
    parser.add_argument("--proof", type=Path)
    operation = parser.add_mutually_exclusive_group(required=True)
    for name in ("check", "apply", "restore"):
        operation.add_argument("--" + name, action="store_true")
    args = parser.parse_args()
    try:
        migration = Migration(args.root, args.bundle, args.snapshot, args.proof)
        name = "check" if args.check else "apply" if args.apply else "restore"
        print(json.dumps(getattr(migration, name)(), sort_keys=True))
        return 0
    except (MigrationError, OSError) as exc:
        print("static-hosting migration: " + str(exc), file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
