#!/usr/bin/env python3
"""Lease-fenced, resumable one-time migration; CLI documentation is in --help.

The pending journal is durable before every irreversible operation. Failures keep
it and the receiver/database in place; there is deliberately no automatic restore.
Operator evidence acknowledges external owner/bank actions, never performs them.
"""
from __future__ import annotations

import argparse
import base64
import contextlib
import hashlib
import io
import json
import os
from pathlib import Path
import re
import sqlite3
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
import zipfile

SOURCE = 'TheDemonTuan/acb-transaction-webhook'
CENTRAL = 'TheDemonTuan/vps-deploy'
VOLUME = 'bank-event-gateway_gateway_data'
BANK = 'https://bank.tuannguyenviet.site'
PUBLIC = 'https://transactions.tuannguyenviet.site'
PHASES = ('STAGED', 'LEGACY_ADMITTED', 'LEGACY_DRAINED', 'SCHEMA_READY',
          'BACKEND_READY', 'FRONTEND_READY', 'WEBHOOK_CONFIRMED', 'LIVE')
IMAGES = {'RELEASE_SHA', 'PAYMENT_RUNTIME', 'GATEWAY_IMAGE_REF', 'WORKER_IMAGE_REF',
          'DBTOOL_IMAGE_REF', 'TTS_IMAGE_REF', 'BARK_IMAGE_REF'}
DIGEST = re.compile(r'[a-z0-9][a-z0-9./_-]*@sha256:[a-f0-9]{64}\Z')
SHA = re.compile(r'[a-f0-9]{40}\Z')
UUID = re.compile(r'[a-f0-9]{8}(?:-[a-f0-9]{4}){3}-[a-f0-9]{12}\Z')


class MigrationError(Exception):
    pass


def require(ok, message):
    if not ok:
        raise MigrationError(message)


def digest(data):
    return hashlib.sha256(data).hexdigest()


def decode(data):
    def unique(pairs):
        result = {}
        for key, value in pairs:
            require(key not in result, 'duplicate metadata key')
            result[key] = value
        return result
    try:
        return json.loads(data, object_pairs_hook=unique)
    except (ValueError, UnicodeError):
        raise MigrationError('invalid JSON metadata') from None


def regular(path):
    path = Path(path)
    require(path.is_file() and not path.is_symlink(), 'missing regular migration input: ' + path.name)
    return path.read_bytes()


def keys(path):
    result = {}
    for line in regular(path).decode().splitlines():
        if not line or line.startswith('#'):
            continue
        key, sep, value = line.partition('=')
        require(sep and key not in result and value and not any(c.isspace() for c in value),
                'invalid environment manifest: ' + Path(path).name)
        result[key] = value
    return result


def fsync_directory(path):
    # Production CLI requires Linux; portable unit fixtures cannot fsync a Windows directory.
    if os.name == 'posix':
        fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(fd)
        finally:
            os.close(fd)


def atomic(path, data):
    path = Path(path)
    if isinstance(data, dict):
        data = (json.dumps(data, sort_keys=True, indent=2) + '\n').encode()
    fd, temporary = tempfile.mkstemp(prefix='.payos-', dir=path.parent)
    try:
        with os.fdopen(fd, 'wb') as stream:
            if hasattr(os, 'fchmod'):
                os.fchmod(stream.fileno(), 0o600)
            else:
                os.chmod(temporary, 0o600)
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
        fsync_directory(path.parent)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)

def file_digest(path):
    path = Path(path)
    require(path.is_file() and not path.is_symlink(), 'backup must be a regular file')
    hasher = hashlib.sha256()
    with path.open('rb') as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b''):
            hasher.update(chunk)
    return hasher.hexdigest()


def atomic_copy(source, target, expected_digest):
    target = Path(target)
    metadata = target.stat()
    fd, temporary = tempfile.mkstemp(prefix='.payos-restore-', dir=target.parent)
    try:
        with os.fdopen(fd, 'wb') as output, Path(source).open('rb') as input_file:
            if hasattr(os, 'fchmod'):
                os.fchmod(output.fileno(), 0o600)
            else:
                os.chmod(temporary, 0o600)
            if hasattr(os, 'fchown'):
                os.fchown(output.fileno(), metadata.st_uid, metadata.st_gid)
            hasher = hashlib.sha256()
            for chunk in iter(lambda: input_file.read(1024 * 1024), b''):
                output.write(chunk)
                hasher.update(chunk)
            require(hasher.hexdigest() == expected_digest, 'restore source changed during copy')
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, target)
        fsync_directory(target.parent)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


class Migration:
    def __init__(self, root, sha, source_run_id=None, registry_path=None):
        self.root = Path(root)
        require(self.root.is_absolute() and self.root.is_dir() and not self.root.is_symlink(),
                'DEPLOY_PATH must be an existing absolute root')
        self.sha = sha
        self.source_run_id = source_run_id
        self.registry_path = registry_path
        self.release = self.root / 'releases' / sha if sha else None
        self.marker = self.root / '.payos-cutover-pending'
        self.done = self.root / '.payos-cutover-done'
        self.route = Path(os.environ.get('ACB_ROUTE_FILE', '/opt/platform/edge/dynamic/acb.yml'))
        self.plan = None
        self.snapshot = None
        self.gate_image = None
        self.renewing = False

    def command(self, args, timeout=120, env=None):
        if self.plan and self.plan.get('gate_token') and self.gate_image and not self.renewing \
                and not any(str(arg) in ('-gate-renew', '-gate-acquire', '-gate-release') for arg in args):
            self.renewing = True
            try:
                self.dbtool(self.gate_image, '-gate-renew', '-owner', 'payos-cutover',
                            '-lease-token', self.plan['gate_token'], '-lease-duration', '15m')
            finally:
                self.renewing = False
        try:
            result = subprocess.run([str(a) for a in args], stdout=subprocess.PIPE,
                                    stderr=subprocess.PIPE, timeout=timeout,
                                    env={**os.environ, **(env or {})})
        except (OSError, subprocess.TimeoutExpired):
            raise MigrationError('command unavailable or timed out: ' + str(args[0])) from None
        # Arguments/output may contain lease tokens, Access credentials, or data.
        require(result.returncode == 0, 'command failed: ' + str(args[0]))
        return result.stdout

    def gh(self, endpoint):
        return decode(self.command(['gh', 'api', endpoint]))

    def shell(self, bundle, function, args=(), env=None, timeout=120):
        # Values are positional arguments, never interpolated into shell source.
        require(re.fullmatch(r'[a-z_]+', function), 'invalid library function')
        script = 'source "$1/simple-lib.sh"; shift; ' + function + ' "$@"'
        return self.command(['bash', '-Eeuo', 'pipefail', '-c', script, 'cutover', bundle, *args],
                            timeout, {'DEPLOY_PATH': str(self.root), **(env or {})})

    def dbtool(self, image, *args, readonly=False):
        require(DIGEST.fullmatch(image), 'dbtool must be digest pinned')
        return self.command(['docker', 'run', '--rm', '--network', 'none', '--user', '1000:1000',
                             *(['--read-only'] if readonly else []), '-v',
                             VOLUME + ':/data:' + ('ro' if readonly else 'rw'), image,
                             '-path', '/data/gateway.db', *args])

    def inspect(self, name):
        objects = decode(self.command(['docker', 'inspect', name]))
        require(len(objects) == 1, 'ambiguous container inspection')
        return objects[0]

    def volume_database(self):
        info = decode(self.command(['docker', 'volume', 'inspect', VOLUME]))
        require(len(info) == 1 and info[0].get('Driver') == 'local', 'local SQLite volume required')
        return Path(info[0]['Mountpoint']) / 'gateway.db'

    def volume_permissions(self):
        path = self.volume_database()
        require(path.is_file() and not path.is_symlink()
                and os.access(path, os.R_OK | os.W_OK)
                and os.access(path.parent, os.R_OK | os.W_OK | os.X_OK)
                and getattr(os, 'geteuid', lambda: path.stat().st_uid)() in (0, path.stat().st_uid),
                'PAYOS_VOLUME_ACCESS_REQUIRED: deployment operator needs existing SQLite volume read/write permission (root or database owner UID); no broad sudo grant is installed')

    def database_summary(self, path):
        # Only used after all legacy writers have stopped, or on immutable backup.
        with contextlib.closing(sqlite3.connect(Path(path).as_uri() + '?mode=ro', uri=True)) as db:
            require(db.execute('PRAGMA integrity_check').fetchone()[0] == 'ok', 'SQLite integrity check failed')
            result = {}
            for table in ('transactions', 'event_journal', 'deliveries'):
                require(db.execute('SELECT 1 FROM sqlite_master WHERE type=\'table\' AND name=?',
                                   (table,)).fetchone(), 'missing historical table: ' + table)
                hasher = hashlib.sha256()
                count = 0
                for row in db.execute('SELECT * FROM ' + table + ' ORDER BY rowid'):
                    hasher.update(json.dumps(row, separators=(',', ':'), default=lambda v:
                                  {'bytes': base64.b64encode(v).decode()}).encode() + b'\n')
                    count += 1
                result[table] = {'count': count, 'sha256': hasher.hexdigest()}
            result['financial_totals'] = dict(zip(('credit_vnd', 'debit_vnd'), db.execute(
                'SELECT COALESCE(SUM(credit),0),COALESCE(SUM(debit),0) FROM transactions').fetchone()))
            result['journal_seq'] = db.execute('SELECT COALESCE(MAX(seq),0) FROM event_journal').fetchone()[0]
            result['delivery_states'] = dict(db.execute('SELECT status,COUNT(*) FROM deliveries GROUP BY status'))
            return result

    def verified_bundle(self, bundle, expected_sha, payos):
        values = keys(bundle / 'images.env')
        require(values.get('RELEASE_SHA') == expected_sha, 'bundle SHA mismatch')
        if payos:
            require(set(values) == IMAGES and values.get('PAYMENT_RUNTIME') == 'payos',
                    'candidate manifest is not the payOS contract')
        for key, value in values.items():
            if key.endswith('_IMAGE_REF'):
                require(DIGEST.fullmatch(value), 'image is not digest pinned')
        manifest = regular(bundle / 'SHA256SUMS')
        entries = {}
        for line in manifest.decode().splitlines():
            checksum, sep, name = line.partition('  ')
            require(sep and re.fullmatch('[a-f0-9]{64}', checksum) and name not in entries
                    and '/' not in name and name not in ('.', '..'), 'unsafe bundle checksum manifest')
            require(digest(regular(bundle / name)) == checksum, 'bundle checksum mismatch: ' + name)
            entries[name] = checksum
        required = {'images.env', 'simple-lib.sh', 'compose.prod.yaml', 'healthcheck.sh', 'backup-db.sh'}
        if payos:
            required |= {'migrate-payos-runtime.sh', 'migrate-payos-runtime.py', 'render-route.sh'}
        require(required <= entries.keys(), 'bundle checksum coverage incomplete')
        return values, entries, digest(manifest)

    def source_proof(self, sha, run_id, entries):
        require(run_id and str(run_id).isdigit(), '--source-run-id of successful staging run required')
        run = self.gh(f'repos/{SOURCE}/actions/runs/{run_id}')
        require(run.get('head_sha') == sha and run.get('conclusion') == 'success'
                and run.get('status') == 'completed' and run.get('event') == 'workflow_dispatch'
                and run.get('head_branch') == 'main' and run.get('head_repository', {}).get('full_name') == SOURCE
                and run.get('path') == '.github/workflows/deploy.yml', 'source run is not successful staged deploy.yml on main')
        jobs = self.gh(f'repos/{SOURCE}/actions/runs/{run_id}/jobs?per_page=100')['jobs']
        deploy = [job for job in jobs if job.get('name') == 'Deploy to VPS']
        require(len(deploy) == 1 and deploy[0].get('conclusion') == 'skipped', 'source run must be staging-only (deploy=false)')
        if 'source-run.env' in entries:
            staged = keys(self.release / 'source-run.env')
            require(staged == {'RELEASE_SHA': sha, 'SOURCE_RUN_ID': str(run_id),
                              'SOURCE_RUN_ATTEMPT': str(run.get('run_attempt')),
                              'PAYMENT_RUNTIME': 'payos'}, 'source-run manifest differs from exact successful staging run')
        artifacts = self.gh(f'repos/{SOURCE}/actions/runs/{run_id}/artifacts?per_page=100')['artifacts']
        matches = [a for a in artifacts if a.get('name') == 'verified-bundle' and not a.get('expired')]
        frontend = [a for a in artifacts if a.get('name') == 'frontend-dist-' + sha and not a.get('expired')]
        require(len(matches) == len(frontend) == 1, 'successful backend/frontend artifacts required')
        require(re.fullmatch(r'sha256:[a-f0-9]{64}', frontend[0].get('digest', '')), 'frontend artifact digest missing')
        require(str(matches[0].get('digest', '')).startswith('sha256:'), 'source artifact has no GitHub digest')
        archive = self.command(['gh', 'api', f'repos/{SOURCE}/actions/artifacts/{matches[0]["id"]}/zip'])
        require('sha256:' + digest(archive) == matches[0]['digest'], 'source artifact archive digest mismatch')
        require(len(archive) <= 8 * 1024 * 1024, 'backend artifact exceeds limit')
        try:
            with zipfile.ZipFile(io.BytesIO(archive)) as zipped:
                require(len(zipped.namelist()) == len(set(zipped.namelist())), 'duplicate artifact entries')
                for name, checksum in entries.items():
                    require(zipped.getinfo(name).file_size == (self.release / name).stat().st_size,
                            'source artifact member size differs from candidate')
                    require(digest(zipped.read(name)) == checksum, 'candidate differs from successful source artifact')
        except (zipfile.BadZipFile, KeyError):
            raise MigrationError('invalid verified backend artifact') from None
        return {'source_run_id': str(run_id), 'backend_artifact_id': matches[0]['id'],
                'frontend_artifact_id': frontend[0]['id'], 'backend_archive_sha256': digest(archive),
                'frontend_archive_digest': frontend[0]['digest']}

    def central_current(self):
        require(self.registry_path, '--registry-path of central acb registry required')
        item = self.gh(f'repos/{CENTRAL}/contents/{self.registry_path}')
        registry = decode(base64.b64decode(item['content']))
        # Explicitly require maintenance, never silently change another app or registry.
        require(registry.get('automatic') is False, 'CENTRAL_AUTOMATIC_PUBLISH_MUST_BE_DISABLED')
        require(registry.get('app', registry.get('id', 'acb')) == 'acb', 'wrong central app registry')
        require(registry.get('repository') == SOURCE and registry.get('workflow') == 'deploy.yml'
                and registry.get('workers') == ['acb-web'], 'central registry source/worker mismatch')
        return {'registry_path': self.registry_path, 'registry_sha': item['sha'], 'registry': registry}

    def prepare(self, prior):
        require(SHA.fullmatch(self.sha or ''), 'invalid release SHA')
        require(prior is not None, '--prior-frontend private exact-version evidence is required for first check/apply')
        require(self.source_run_id and str(self.source_run_id).isdigit(),
                '--source-run-id of successful staging-only source run is required for first check/apply')
        require(not (self.root / '.deploy-pending').exists() and not (self.root / '.static-hosting-pending').exists(),
                'another migration/deployment is pending')
        state = keys(self.root / 'state.env')
        require(set(state) == {'RELEASE_SHA', 'GATEWAY_SLOT'} and SHA.fullmatch(state['RELEASE_SHA'])
                and state['GATEWAY_SLOT'] in ('blue', 'green'), 'legacy state required')
        legacy = self.root / 'releases' / state['RELEASE_SHA']
        old, _, old_manifest = self.verified_bundle(legacy, state['RELEASE_SHA'], False)
        candidate, entries, manifest = self.verified_bundle(self.release, self.sha, True)
        runtime = keys(legacy / 'runtime.env')
        require(runtime.get('DBTOOL_IMAGE_REF') == old['DBTOOL_IMAGE_REF'], 'legacy runtime/dbtool mismatch')
        for name, key in [('acb-worker', 'WORKER_IMAGE_REF'), ('acb-auth-browser', 'BROWSER_IMAGE_REF'),
                          ('acb-gateway-' + state['GATEWAY_SLOT'], 'IMAGE_REF_' + state['GATEWAY_SLOT'].upper())]:
            container = self.inspect(name)
            require(container['State']['Running'] is True and container['Config']['Image'] == runtime[key],
                    'actual legacy VPS runtime does not match pinned state')
            self.shell(legacy, 'container_image_check', [name, runtime[key]])
        frontend = decode(regular(prior))
        require(frontend.get('app') == 'acb' and SHA.fullmatch(frontend.get('sha', ''))
                and UUID.fullmatch(frontend.get('version_id', '')), 'prior central SHA/version evidence required')
        prior_receipt = self.receipt_artifact(frontend.get('central_run_id'))
        self.latest_central_publication(frontend['central_run_id'])
        require(prior_receipt.get('requested_sha') == frontend['sha']
                and prior_receipt.get('active_deployment', {}).get('version_id') == frontend['version_id'],
                'prior central receipt differs from requested exact frontend snapshot')
        frontend['receipt'] = prior_receipt
        self.frontend_release(frontend['sha'], frontend.get('bank_access_headers_file'), False)
        central = self.central_current()
        self.publisher_ready()
        source = self.source_proof(self.sha, self.source_run_id, entries)
        self.secrets_ready()
        self.dbtool(old['DBTOOL_IMAGE_REF'], '-readonly', '-gate-status', readonly=True)
        self.volume_permissions()
        for key, reference in candidate.items():
            if key.endswith('_IMAGE_REF'):
                self.command(['docker', 'image', 'inspect', reference])
        route_hash = digest(regular(self.route))
        return {'schema': 1, 'phase': 'STAGED', 'sha': self.sha, 'legacy_sha': state['RELEASE_SHA'],
                'legacy_slot': state['GATEWAY_SLOT'], 'legacy_bundle': str(legacy),
                'legacy_dbtool': old['DBTOOL_IMAGE_REF'], 'candidate_images': candidate,
                'candidate_manifest_sha256': manifest, 'legacy_manifest_sha256': old_manifest,
                'prior_frontend': frontend, 'central': central, 'source': source,
                'route_sha256': route_hash, 'gate_token': None, 'gate_owner': 'payos-cutover',
                'backup': None, 'state_sha256': digest(regular(self.root / 'state.env')),
                'runtime_sha256': digest(regular(legacy / 'runtime.env')),
                'env_sha256': digest(regular(self.root / 'deploy/.env.production'))}

    def secrets_ready(self):
        for name in ('payos_client_id', 'payos_api_key', 'payos_checksum_key'):
            path = self.root / 'deploy/secrets' / name
            require(path.is_file() and not path.is_symlink() and path.stat().st_size > 0,
                    'real payOS secret file required: ' + name)
            st = path.stat()
            require(st.st_mode & 0o777 == 0o600 and st.st_uid == st.st_gid == 1000,
                    'payOS secret permissions must be 0600 1000:1000')

    def publisher_ready(self):
        if self.route != Path('/opt/platform/edge/dynamic/acb.yml'):
            return  # Only isolated drills use an app-owned unprivileged route.
        import importlib.util
        installed = Path('/usr/local/libexec/acb-route-publish')
        require(installed.is_file() and not installed.is_symlink() and installed.stat().st_uid == 0
                and not installed.stat().st_mode & 0o022
                and digest(regular(installed)) == digest(regular(self.release / 'acb-route-publish.py')),
                'PAYOS_ROUTE_PUBLISHER_UPGRADE_REQUIRED: root operator must install reviewed candidate publisher')
        # The installed helper has no extension, so load with the source loader.
        from importlib.machinery import SourceFileLoader
        spec = importlib.util.spec_from_loader('cutover_route_policy', SourceFileLoader('cutover_route_policy', str(installed)))
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        for data in [regular(self.route), *(self.command(['bash', self.release / 'render-route.sh', slot]) for slot in ('blue', 'green'))]:
            try:
                module.validate(data, Path('/etc/acb-route-publisher/templates'))
            except module.PublishError:
                raise MigrationError('PAYOS_ROUTE_POLICY_UPGRADE_REQUIRED: preserve legacy and authorize both candidate slot templates') from None

    def load(self):
        self.plan = decode(regular(self.marker if self.marker.exists() else self.done))
        require(self.plan.get('schema') == 1 and self.plan.get('phase') in (*PHASES, 'ROLLED_BACK'), 'invalid cutover journal')
        require(not self.sha or self.sha == self.plan['sha'], 'pending migration SHA differs')
        self.sha = self.plan['sha']
        self.release = self.root / 'releases' / self.sha
        self.snapshot = self.root / ('.payos-cutover-' + self.sha)
        require(self.plan['snapshot_manifest_sha256'] == digest(regular(self.snapshot / 'manifest.json')),
                'immutable cutover snapshot metadata changed')
        immutable = decode(regular(self.snapshot / 'manifest.json'))
        for name, checksum in immutable['files'].items():
            require(digest(regular(self.snapshot / name)) == checksum, 'immutable before-image changed')
        _, _, manifest = self.verified_bundle(self.release, self.sha, True)
        require(manifest == self.plan['candidate_manifest_sha256'], 'candidate bundle changed after staging')
        legacy = Path(self.plan['legacy_bundle'])
        _, _, manifest = self.verified_bundle(legacy, self.plan['legacy_sha'], False)
        require(manifest == self.plan['legacy_manifest_sha256'], 'legacy bundle changed after staging')
        require(digest(regular(legacy / 'runtime.env')) == self.plan['runtime_sha256'], 'pinned legacy runtime changed')
        return self.plan

    def save(self, phase=None):
        if phase:
            self.plan['phase'] = phase
        atomic(self.marker, self.plan)

    def stage(self, prior):
        self.plan = self.prepare(prior)
        self.snapshot = self.root / ('.payos-cutover-' + self.sha)
        require(not self.done.exists(), 'completed migration evidence exists')
        require(not self.snapshot.is_symlink(), 'cutover snapshot cannot be a symlink')
        if (self.snapshot / 'manifest.json').exists():
            manifest = decode(regular(self.snapshot / 'manifest.json'))
            require(manifest['plan'] == self.plan, 'orphaned snapshot differs from current readonly staging inputs')
            for name, checksum in manifest['files'].items():
                require(digest(regular(self.snapshot / name)) == checksum, 'orphaned snapshot changed')
            self.plan['snapshot_manifest_sha256'] = digest(regular(self.snapshot / 'manifest.json'))
            self.save()
            return
        if not self.snapshot.exists():
            self.snapshot.mkdir(mode=0o700)
        seed = self.snapshot / 'staged-plan.json'
        if seed.exists():
            require(decode(regular(seed)) == self.plan, 'partial snapshot staging inputs changed')
        else:
            require(not any(self.snapshot.iterdir()), 'partial snapshot has no immutable staging plan')
            atomic(seed, self.plan)
        files = {'previous-state.env': self.root / 'state.env', 'previous-runtime.env': Path(self.plan['legacy_bundle']) / 'runtime.env',
                 'previous-env.production': self.root / 'deploy/.env.production', 'previous-acb.yml': self.route}
        hashes = {'staged-plan.json': digest(regular(seed))}
        for name, source in files.items():
            data = regular(source)
            if (self.snapshot / name).exists():
                require(regular(self.snapshot / name) == data, 'partial before-image differs from current staging inputs')
            else:
                atomic(self.snapshot / name, data)
            hashes[name] = digest(data)
        manifest = {'files': hashes, 'plan': self.plan}
        atomic(self.snapshot / 'manifest.json', manifest)
        self.plan['snapshot_manifest_sha256'] = digest(regular(self.snapshot / 'manifest.json'))
        self.save()

    def gate(self, image):
        self.gate_image = image
        token = self.plan.get('gate_token')
        if token:
            try:
                self.dbtool(image, '-gate-renew', '-owner', 'payos-cutover', '-lease-token', token, '-lease-duration', '15m')
                return
            except MigrationError:
                pass
        # Reacquisition uses the LEGACY image while legacy writers could be alive.
        result = decode(self.dbtool(image, '-gate-acquire', '-owner', 'payos-cutover',
                                   '-reason', 'payos-cutover', '-lease-duration', '15m'))
        require(isinstance(result.get('leaseToken'), str) and result['leaseToken'], 'gate admission returned no lease')
        self.plan['gate_token'] = result['leaseToken']
        self.save()

    @contextlib.contextmanager
    def lease_heartbeat(self, image):
        # Dedicated subprocess keeps the lease renewed during health/central waits.
        token = self.plan['gate_token']
        args = ['docker', 'run', '--rm', '--network', 'none', '--user', '1000:1000', '-v', VOLUME + ':/data:rw',
                image, '-path', '/data/gateway.db', '-gate-renew', '-owner', 'payos-cutover',
                '-lease-token', token, '-lease-duration', '15m']
        script = 'while sleep 30; do "$@" >/dev/null 2>&1 || exit 1; done'
        process = subprocess.Popen(['bash', '-c', script, 'cutover-renew', *args], stdout=subprocess.DEVNULL,
                                   stderr=subprocess.DEVNULL, start_new_session=True)
        try:
            yield
            require(process.poll() is None, 'cutover gate renewal failed; stop and retain journal')
            self.dbtool(image, '-gate-renew', '-owner', 'payos-cutover', '-lease-token', token, '-lease-duration', '15m')
        finally:
            import signal
            if process.poll() is None:
                os.killpg(process.pid, signal.SIGTERM)
            process.wait()

    def stopped(self, names):
        for name in names:
            # Existing singleton services are expected; absent stopped slots are fine.
            result = self.command(['docker', 'ps', '-aq', '--filter', 'name=^/' + name + '$']).strip()
            if result:
                require(self.inspect(name)['State']['Running'] is False, 'writer still running: ' + name)

    def stop(self, names):
        for name in names:
            if self.command(['docker', 'ps', '-aq', '--filter', 'name=^/' + name + '$']).strip():
                self.command(['docker', 'stop', '-t', '45', name], timeout=70)
        self.stopped(names)

    def quiesce(self, legacy):
        if self.inspect('acb-worker')['State']['Running']:
            report = decode(self.command(['docker', 'exec', '-e', 'WORKER_INTERNAL_TOKEN_FILE=/run/secrets/worker_internal_token',
                                          'acb-worker', '/worker', '-quiesce'], timeout=60))
            require(report.get('status') == 'quiesced' and report.get('quiesced') is True
                    and report.get('dispatcher') == 'IDLE' and type(report.get('activeDeliveries')) is int
                    and report['activeDeliveries'] == 0, 'worker did not drain dispatcher')
            if legacy:
                require(report.get('activePoll') is False and report.get('sessionCheckpointed') is True,
                        'legacy worker did not checkpoint and drain polling')
            else:
                require(type(report.get('activePaymentRequests')) is int and report['activePaymentRequests'] == 0,
                        'candidate worker did not drain payments')
            self.plan['drain_report'] = report
            self.save()

    def drain_legacy(self):
        self.stop(['acb-recovery-controller', 'acb-auth-browser'])
        self.quiesce(True)
        self.stop(['acb-worker', 'acb-gateway-blue', 'acb-gateway-green'])
        self.stopped(['acb-recovery-controller', 'acb-auth-browser', 'acb-worker', 'acb-gateway-blue', 'acb-gateway-green'])
        if not self.plan.get('backup'):
            # No precautionary snapshot can be used here: this is after ALL writers.
            if not self.plan.get('backup_intent'):
                self.plan['backup_intent'] = {
                    'history': self.database_summary(self.volume_database()),
                    'existing_receipts': sorted(str(p) for p in (self.root / 'data/backups').glob('*/receipt.json'))}
                self.save()
            intent = self.plan['backup_intent']
            require(self.database_summary(self.volume_database()) == intent['history'], 'drained financial state changed during backup')
            recovered = [p for p in (self.root / 'data/backups').glob('*/receipt.json') if str(p) not in intent['existing_receipts']]
            require(len(recovered) <= 1, 'ambiguous authoritative backup receipts; retain snapshots for operator review')
            if recovered:
                receipt_path = str(recovered[0])
            else:
                receipt_path = self.command(['bash', Path(self.plan['legacy_bundle']) / 'backup-db.sh', '--snapshot'], env={
                    'DEPLOY_PATH': str(self.root), 'DBTOOL_IMAGE_REF': self.plan['legacy_dbtool'],
                    'RELEASE_COMMIT': self.plan['legacy_sha'], 'ACTIVE_SLOT': self.plan['legacy_slot']}).decode().strip()
            receipt = decode(regular(receipt_path))
            backup_path = Path(receipt['path'])
            require(backup_path.is_relative_to(self.root / 'data/backups') and receipt.get('sha') == self.plan['legacy_sha']
                    and receipt.get('schema_version') == 13 and receipt['sha256'] == file_digest(backup_path),
                    'authoritative backup receipt mismatch')
            require(self.database_summary(backup_path) == intent['history'], 'backup differs from drained authoritative database')
            self.plan['backup'] = {'receipt': receipt, 'receipt_path': receipt_path, 'history': intent['history']}
            self.save()  # A durable receipt is never replaced on resume.
        self.save('LEGACY_DRAINED')

    def schema_ready(self):
        self.stopped(['acb-recovery-controller', 'acb-auth-browser', 'acb-worker', 'acb-gateway-blue', 'acb-gateway-green'])
        backup = self.plan['backup']
        require(file_digest(backup['receipt']['path']) == backup['receipt']['sha256'], 'backup checksum changed')
        image = self.plan['candidate_images']['DBTOOL_IMAGE_REF']
        if not self.plan.get('legacy_retired'):
            self.dbtool(image, '-payos-cutover', '-lease-token', self.plan['gate_token'])
            self.plan['legacy_retired'] = True
            self.save()
        self.dbtool(image, '-migrate')
        self.dbtool(image, '-readonly', '-check', readonly=True)
        require(self.database_summary(self.volume_database()) == backup['history'], 'migration changed financial history/journal/deliveries')
        self.save('SCHEMA_READY')

    def set_flags(self, enabled, confirmed):
        path = self.root / 'deploy/.env.production'
        lines = regular(path).decode().splitlines()
        values = {'PAYMENTS_ENABLED': str(enabled).lower(), 'PAYOS_WEBHOOK_CONFIRMED': str(confirmed).lower()}
        counts = {key: 0 for key in values}
        output = []
        for line in lines:
            key = line.partition('=')[0]
            if key in values:
                counts[key] += 1
                line = key + '=' + values[key]
            output.append(line)
        require(all(n <= 1 for n in counts.values()), 'duplicate operational payment gate')
        output += [key + '=' + value for key, value in values.items() if not counts[key]]
        atomic(path, ('\n'.join(output) + '\n').encode())

    def compose(self, legacy, *args):
        bundle = Path(self.plan['legacy_bundle']) if legacy else self.release
        flags = {}
        for line in regular(self.root / 'deploy/.env.production').decode().splitlines():
            key, sep, value = line.partition('=')
            if key in ('PAYMENTS_ENABLED', 'PAYOS_WEBHOOK_CONFIRMED'):
                require(sep and key not in flags and value in ('true', 'false'), 'invalid operational payment flag')
                flags[key] = value
        return self.shell(bundle, 'compose_release', [bundle, bundle / 'runtime.env', *args], env=flags, timeout=180)

    def health(self, legacy=False):
        bundle = Path(self.plan['legacy_bundle']) if legacy else self.release
        images = keys(bundle / 'runtime.env')
        sha = self.plan['legacy_sha'] if legacy else self.sha
        slot = self.plan['legacy_slot'] if legacy else self.plan['candidate_slot']
        for service, ref in [('worker', images['WORKER_IMAGE_REF']), ('gateway-' + slot, images['IMAGE_REF_' + slot.upper()])]:
            self.shell(bundle, 'container_image_check', ['acb-' + service, ref])
            self.command(['bash', bundle / 'healthcheck.sh', 'container', 'acb-' + service, '120'], timeout=150,
                         env={'DEPLOY_PATH': str(self.root), 'EXPECTED_IMAGE_REF': ref,
                              'EXPECTED_RELEASE_SHA': sha, 'EXPECTED_SLOT': slot})
        self.command(['bash', bundle / 'healthcheck.sh', 'route', slot, sha], env={'DEPLOY_PATH': str(self.root)}, timeout=150)

    def counts(self):
        values = decode(self.dbtool(self.plan['candidate_images']['DBTOOL_IMAGE_REF'], '-payment-counts', readonly=True))
        require(all(type(values.get(k)) is int and values[k] >= 0 for k in ('orders', 'receipts', 'journalSeq')), 'invalid payment counts')
        return values

    def no_payments(self):
        values = self.counts()
        require(values['orders'] == values['receipts'] == 0, 'PAYOS_ROLLBACK_REQUIRES_PAYMENT_DRAIN')
        return values

    def backend_ready(self):
        self.set_flags(False, False)
        images = self.plan['candidate_images']
        slot = 'blue' if self.plan['legacy_slot'] == 'green' else 'green'
        self.plan['candidate_slot'] = slot
        self.save()
        runtime = {'IMAGE_REF_BLUE': images['GATEWAY_IMAGE_REF'], 'IMAGE_REF_GREEN': images['GATEWAY_IMAGE_REF'],
                   'RELEASE_COMMIT_BLUE': self.sha, 'RELEASE_COMMIT_GREEN': self.sha,
                   **{key: images[key] for key in ('WORKER_IMAGE_REF', 'TTS_IMAGE_REF', 'BARK_IMAGE_REF', 'DBTOOL_IMAGE_REF')},
                   'WORKER_RELEASE_COMMIT': self.sha, 'ENV_FILE': str(self.root / 'deploy/.env.production'),
                   'SECRETS_DIR': str(self.root / 'deploy/secrets'), 'BARK_SECRET_GROUP': '1000', 'PAYMENT_RUNTIME': 'payos'}
        data = ''.join(key + '=' + value + '\n' for key, value in runtime.items()).encode()
        path = self.release / 'runtime.env'
        if path.exists():
            require(regular(path) == data, 'candidate runtime drift')
        else:
            atomic(path, data)
        self.compose(False, 'up', '-d', '--no-deps', 'tts-gateway', 'bark', 'worker', 'gateway-' + slot)
        route = self.command(['bash', self.release / 'render-route.sh', slot])
        atomic(self.snapshot / 'candidate-acb.yml', route)
        self.plan['candidate_route_sha256'] = digest(route)
        self.save()
        current = digest(regular(self.route))
        require(current in (self.plan['route_sha256'], digest(route)), 'route drift before cutover')
        if current != digest(route):
            self.shell(self.release, 'route_replace', [self.snapshot / 'candidate-acb.yml', self.route, current])
        self.health()
        if self.plan.get('gate_token'):
            self.dbtool(images['DBTOOL_IMAGE_REF'], '-gate-release', '-owner', 'payos-cutover', '-lease-token', self.plan['gate_token'])
            self.plan['gate_token'] = None
            self.save()
        self.backend_smoke()
        state = f'RELEASE_SHA={self.sha}\nGATEWAY_SLOT={slot}\nPAYMENT_RUNTIME=payos\n'.encode()
        atomic(self.root / 'state.env', state)
        self.plan['baseline_counts'] = self.no_payments()
        self.save('BACKEND_READY')

    def http(self, origin, path, method='GET', headers=None, body=None, first_frame=False):
        class NoRedirect(urllib.request.HTTPRedirectHandler):
            def redirect_request(self, req, fp, code, msg, hdrs, newurl):
                return None
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
        request = urllib.request.Request(origin + path, data=body, method=method, headers=headers or {})
        try:
            try:
                response = opener.open(request, timeout=15)
            except urllib.error.HTTPError as error:
                response = error
            with response:
                if first_frame:
                    data = bytearray()
                    deadline = time.monotonic() + 15
                    while len(data) < 65536 and not data.endswith(b'\n\n'):
                        require(time.monotonic() < deadline, 'SSE initial frame timed out')
                        chunk = response.read(1)
                        require(chunk, 'SSE ended before initial frame')
                        data.extend(chunk)
                    data = bytes(data)
                    require(data.endswith(b'\n\n'), 'SSE initial frame exceeds limit')
                else:
                    data = response.read(1024 * 1024 + 1)
                require(len(data) <= 1024 * 1024, 'public response exceeds limit')
                require(not response.headers.get('cf-mitigated'), 'public endpoint was challenged')
                return response.status, response.headers, data
        except (OSError, urllib.error.URLError):
            raise MigrationError('public HTTP verification failed') from None

    def backend_smoke(self):
        status, headers, data = self.http(PUBLIC, '/api/integrations/payos/webhook', 'POST',
                                          {'Content-Type': 'application/json'}, b'{}')
        require(status == 400 and headers.get_content_type() == 'application/json' and isinstance(decode(data), dict),
                'exact callback must return JSON 400, not login/challenge/SPA')
        for path in ('/api/v1/status', '/internal', '/api/integrations/payos/webhook'):
            status, _, _ = self.http(PUBLIC, path)
            require(status in (403, 404), 'public private/wrong-method route exposed')
        status, headers, data = self.http(PUBLIC, '/api/public/v1/transactions?limit=1')
        require(status == 200 and headers.get_content_type() == 'application/json' and isinstance(decode(data).get('items'), list),
                'historical transaction API unavailable')
        status, _, data = self.http(PUBLIC, '/api/public/v1/payment-config')
        cfg = decode(data)
        require(status == 200 and cfg.get('provider') == 'PAYOS' and cfg.get('ready') is False,
                'new-order gate unexpectedly open')
        status, headers, data = self.http(PUBLIC, '/api/public/v1/events', first_frame=True)
        require(status == 200 and headers.get_content_type() == 'text/event-stream' and b'event:' in data,
                'public SSE initial frame unavailable')

    def access_headers(self, path):
        require(path, 'bank Access service-token headers file required for release evidence')
        headers = decode(regular(path))
        require(set(headers) == {'CF-Access-Client-Id', 'CF-Access-Client-Secret'}
                and all(isinstance(v, str) and v and '\n' not in v and '\r' not in v for v in headers.values()),
                'invalid bank Access headers file')
        require(os.name != 'posix' or Path(path).stat().st_mode & 0o077 == 0, 'Access headers file must be private')
        return headers

    def frontend_release(self, sha, access_file, deep_link=True):
        for origin in (BANK, PUBLIC):
            status, headers, body = self.http(origin, '/__release', headers=self.access_headers(access_file) if origin == BANK else None)
            require(status == 200 and headers.get_content_type() == 'text/plain'
                    and body == (sha + '\n').encode(), 'actual frontend __release SHA mismatch')
        if deep_link:
            # Deliberately nonexistent capability: SPA loading, not order creation.
            for path in ('/pay', '/pay/' + 'A' * 43):
                status, headers, body = self.http(PUBLIC, path)
                require(status == 200 and headers.get_content_type() == 'text/html' and b'<html' in body.lower(), 'payment deep-link SPA unavailable')

    def central_operation(self, mode):
        key = 'central_' + mode
        if not self.plan.get(key):
            self.central_current()
            if mode == 'publish':
                prior = self.plan['prior_frontend']
                self.frontend_release(prior['sha'], prior.get('bank_access_headers_file'), False)
            inputs = ['-f', 'app=acb', '-f', 'mode=' + mode]
            if mode == 'publish':
                inputs += ['-f', 'source_run_id=' + self.plan['source']['source_run_id']]
            else:
                prior = self.plan['prior_frontend']
                inputs += ['-f', 'sha=' + prior['sha'], '-f', 'version_id=' + prior['version_id']]
            # Persist intent BEFORE dispatch. Ambiguous dispatch is never repeated;
            # operator supplies the exact run ID on resume via --central-run-id.
            self.plan[key] = {'dispatched': False, 'requested_at': time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime())}
            self.save()
            self.command(['gh', 'workflow', 'run', 'cloudflare-deploy.yml', '--repo', CENTRAL, *inputs])
            self.plan[key]['dispatched'] = True
            self.save()
        return key

    def latest_central_publication(self, expected_run):
        runs = self.gh(f'repos/{CENTRAL}/actions/workflows/cloudflare-deploy.yml/runs?status=success&per_page=100')['workflow_runs']
        for run in runs:
            if run.get('event') != 'workflow_dispatch':
                continue
            artifacts = self.gh(f'repos/{CENTRAL}/actions/runs/{run["id"]}/artifacts?per_page=100')['artifacts']
            if not any(a.get('name') == 'cloudflare-receipt-acb-' + str(run['id']) for a in artifacts):
                continue  # A different app's deployment is not ACB publication.
            receipt = self.receipt_artifact(run['id'], publication=False)
            if receipt.get('mode') not in ('publish', 'rollback', 'bootstrap', 'cutover'):
                continue
            require(receipt.get('status') in ('passed', 'already_current') and receipt.get('public_checks_passed') is True,
                    'latest central ACB publication is not healthy exact-version evidence')
            require(str(run['id']) == str(expected_run), 'prior frontend receipt is not the latest successful central ACB publication')
            return
        raise MigrationError('current central ACB publication receipt unavailable in recent successful runs')

    def receipt_artifact(self, run_id, publication=True):
        require(run_id and str(run_id).isdigit(), 'exact central receipt run ID required')
        run = self.gh(f'repos/{CENTRAL}/actions/runs/{run_id}')
        require(run.get('path') == '.github/workflows/cloudflare-deploy.yml' and run.get('event') == 'workflow_dispatch'
                and run.get('conclusion') == 'success' and run.get('status') == 'completed',
                'central workflow has not successfully completed')
        artifacts = self.gh(f'repos/{CENTRAL}/actions/runs/{run_id}/artifacts?per_page=100')['artifacts']
        receipts = [a for a in artifacts if a.get('name') == 'cloudflare-receipt-acb-' + str(run_id) and not a.get('expired')]
        require(len(receipts) == 1, 'exact successful central ACB deployment receipt required')
        blob = self.command(['gh', 'api', f'repos/{CENTRAL}/actions/artifacts/{receipts[0]["id"]}/zip'])
        require(len(blob) <= 1024 * 1024 and 'sha256:' + digest(blob) == receipts[0].get('digest'),
                'central receipt archive size/digest mismatch')
        try:
            with zipfile.ZipFile(io.BytesIO(blob)) as archive:
                require(archive.namelist().count('receipt.json') == 1
                        and len(archive.namelist()) == len(set(archive.namelist())), 'ambiguous central deployment receipt')
                receipt = decode(archive.read('receipt.json'))
        except (zipfile.BadZipFile, KeyError):
            raise MigrationError('invalid central receipt archive') from None
        require(receipt.get('app') == 'acb', 'central receipt belongs to a different app')
        if publication:
            require(receipt.get('status') in ('passed', 'already_current') and receipt.get('public_checks_passed') is True
                    and receipt.get('source_repository') == SOURCE
                    and UUID.fullmatch(receipt.get('active_deployment', {}).get('version_id', '')),
                    'central receipt did not prove successful exact-version publication')
        return receipt

    def central_receipt(self, mode, run_id):
        key = self.central_operation(mode)
        run_id = run_id or self.plan[key].get('run_id')
        require(run_id and str(run_id).isdigit(), 'central dispatch recorded; resume with exact --central-run-id')
        run = self.gh(f'repos/{CENTRAL}/actions/runs/{run_id}')
        require(run.get('created_at', '') >= self.plan[key]['requested_at'], 'central receipt predates this cutover request')
        receipt = self.receipt_artifact(run_id)
        expected = self.sha if mode == 'publish' else self.plan['prior_frontend']['sha']
        require(receipt.get('requested_sha') == expected and receipt.get('mode') == mode,
                'central receipt is not the exact requested app/mode/SHA')
        if mode == 'publish':
            require(str(receipt.get('source_run_id')) == self.plan['source']['source_run_id'], 'central receipt source-run mismatch')
            require(receipt.get('source_archive_digest') == self.plan['source']['frontend_archive_digest']
                    and re.fullmatch(r'[a-f0-9]{64}', receipt.get('artifact_checksum', '')),
                    'central published frontend artifact does not match the pinned successful source archive')
        else:
            require(receipt['active_deployment']['version_id'] == self.plan['prior_frontend']['version_id'],
                    'central rollback version mismatch')
        self.plan[key]['run_id'] = str(run_id)
        self.plan[key]['receipt'] = receipt
        self.save()

    def frontend_ready(self, run_id):
        self.central_receipt('publish', run_id)
        self.frontend_release(self.sha, self.plan['prior_frontend'].get('bank_access_headers_file'))
        self.save('FRONTEND_READY')

    def webhook_confirmed(self, evidence_path):
        require(evidence_path, 'owner must confirm webhook while new orders disabled; resume with --webhook-evidence')
        evidence = decode(regular(evidence_path))
        require(evidence.get('app') == 'acb' and evidence.get('sha') == self.sha
                and evidence.get('owner_confirmed') is True and evidence.get('sample_acknowledged') is True
                and evidence.get('callback_url') == PUBLIC + '/api/integrations/payos/webhook',
                'real owner confirm-webhook evidence required')
        require(self.no_payments() == self.plan['baseline_counts'], 'confirm sample changed payment/journal counts')
        self.plan['webhook_evidence_sha256'] = digest(regular(evidence_path))
        self.save()
        self.set_flags(False, True)
        self.compose(False, 'up', '-d', '--no-deps', '--force-recreate', 'worker', 'gateway-' + self.plan['candidate_slot'])
        self.health()
        self.save('WEBHOOK_CONFIRMED')

    def live(self, evidence_path, enable):
        if not self.plan.get('payments_enabled_at'):
            require(enable, 'webhook confirmed; --enable-payments explicitly opens live acceptance')
            self.plan['payments_enabled_at'] = time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime())
            self.save()
        self.set_flags(True, True)
        self.compose(False, 'up', '-d', '--no-deps', '--force-recreate', 'worker', 'gateway-' + self.plan['candidate_slot'])
        self.health()
        require(evidence_path, 'new orders enabled; user must transfer money externally; resume with --live-evidence')
        evidence = decode(regular(evidence_path))
        require(evidence.get('app') == 'acb' and evidence.get('sha') == self.sha and evidence.get('provider') == 'PAYOS'
                and evidence.get('bank') == 'KienlongBank' and evidence.get('user_transferred') is True
                and all(evidence.get(k) is True for k in ('same_amount_out_of_order', 'sse', 'tts_once', 'outbound_deliveries',
                                                         'lost_sse_recovered', 'replay_deduplicated')),
                'complete real-bank live acceptance evidence required')
        references = evidence.get('references')
        require(isinstance(references, list) and len(references) == 3 and len(set(references)) == 3
                and all(isinstance(v, str) and v for v in references), 'three actual provider receipt references required')
        expected_total = evidence.get('amount_vnd')
        require(type(expected_total) is int and expected_total > 0, 'live expected total must be integer VND')
        # Independently validate financial receipt evidence against current DB.
        db_path = self.volume_database()
        with contextlib.closing(sqlite3.connect(db_path.as_uri() + '?mode=ro', uri=True)) as db:
            db.execute('BEGIN')  # Consistent acceptance evidence while receiver remains live.
            expected_amounts = evidence.get('expected_amounts_vnd', [2000, 3000, 3000])
            require(isinstance(expected_amounts, list) and len(expected_amounts) == 3
                    and all(type(v) is int and v > 0 for v in expected_amounts)
                    and expected_amounts[1] == expected_amounts[2] and sum(expected_amounts) == expected_total,
                    'acceptance amounts must be static plus two equal operator orders')
            if expected_amounts != [2000, 3000, 3000]:
                require(evidence.get('minimum_amount_approved') is True
                        and isinstance(evidence.get('minimum_amount_evidence'), str) and evidence['minimum_amount_evidence'],
                        'non-default real-bank amounts require approved provider minimum evidence')
            rows = db.execute('SELECT r.reference,r.amount_vnd,o.status,o.origin,r.transaction_id,o.order_code, '
                              't.credit,t.debit,t.connection_id FROM payment_receipts r '
                              'JOIN payment_orders o ON o.id=r.order_id JOIN transactions t ON t.id=r.transaction_id '
                              'WHERE r.reference IN (?,?,?)', references).fetchall()
            ordered = {row[0]: row for row in rows}
            require(len(ordered) == 3 and all(ordered[reference][1] == amount for reference, amount in zip(references, expected_amounts))
                    and all(row[2] == 'PAID' and row[6] == row[1] and row[7] == 0 and row[8] == 'payos-klb' for row in rows),
                    'live references/amounts are not committed payOS credits')
            require([ordered[r][3] for r in references] == ['STATIC_URL', 'OPERATOR_DYNAMIC', 'OPERATOR_DYNAMIC'],
                    'live acceptance requires fixed URL and two separate operator orders')
            for row in rows:
                events = db.execute("SELECT id FROM events WHERE transaction_id=? AND event_type='bank.transaction.credit'", (row[4],)).fetchall()
                journal = db.execute("SELECT payload_json FROM event_journal WHERE aggregate_id=? AND event_type='bank.transaction.credit'", (row[4],)).fetchall()
                require(len(events) == len(journal) == 1, 'receipt has duplicate/missing credit event or journal')
                payload = decode(journal[0][0])
                require(payload.get('provider') == 'PAYOS' and str(payload.get('orderCode')) == str(row[5]),
                        'journal credit does not correlate to the settled payOS order')
                deliveries = db.execute('SELECT e.provider,d.status FROM deliveries d JOIN webhook_endpoints e ON e.id=d.endpoint_id '
                                        'WHERE d.event_id=?', (events[0][0],)).fetchall()
                require(deliveries and all(status == 'DELIVERED' for _, status in deliveries)
                        and {'BARK', 'WEBHOOK'} <= {provider for provider, _ in deliveries},
                        'actual Bark and webhook deliveries must complete for every live receipt')
            credited = db.execute('SELECT COALESCE(SUM(credit),0) FROM transactions').fetchone()[0]
            require(credited - self.plan['backup']['history']['financial_totals']['credit_vnd'] == expected_total,
                    'actual incoming total delta differs from approved live acceptance total')
        self.stopped(['acb-auth-browser', 'acb-recovery-controller'])
        self.plan['live_evidence_sha256'] = digest(regular(evidence_path))
        self.plan['final_counts'] = self.counts()
        self.save('LIVE')
        atomic(self.done, self.plan)
        self.marker.unlink()
        fsync_directory(self.root)

    def apply(self, prior=None, central_run_id=None, webhook_evidence=None, live_evidence=None, enable=False):
        if self.marker.exists() or self.done.exists():
            self.load()
            require(self.plan['phase'] != 'ROLLED_BACK', 'cutover rolled back; stage a new release')
            require(not self.plan.get('rollback_requested'), 'PAYOS_ROLLBACK_PENDING: resume --rollback-before-payments')
            self.volume_permissions()
        else:
            self.stage(prior)
        if self.plan['phase'] == 'LIVE':
            return self.summary()
        index = PHASES.index(self.plan['phase'])
        if index <= PHASES.index('LEGACY_DRAINED'):
            for target, checksum in [(self.root / 'state.env', self.plan['state_sha256']),
                                     (Path(self.plan['legacy_bundle']) / 'runtime.env', self.plan['runtime_sha256']),
                                     (self.root / 'deploy/.env.production', self.plan['env_sha256']),
                                     (self.route, self.plan['route_sha256'])]:
                require(digest(regular(target)) == checksum, 'legacy inputs drifted after staging')
        if index < PHASES.index('SCHEMA_READY'):
            self.gate(self.plan['legacy_dbtool'])
            with self.lease_heartbeat(self.plan['legacy_dbtool']):
                if self.plan['phase'] == 'STAGED':
                    self.save('LEGACY_ADMITTED')
                if self.plan['phase'] == 'LEGACY_ADMITTED':
                    self.drain_legacy()
                if self.plan['phase'] == 'LEGACY_DRAINED':
                    self.schema_ready()
        if self.plan['phase'] == 'SCHEMA_READY':
            self.gate(self.plan['candidate_images']['DBTOOL_IMAGE_REF'])
            # backend_ready releases the gate; renewal must not race after release.
            self.backend_ready()
        if self.plan['phase'] == 'BACKEND_READY':
            self.frontend_ready(central_run_id)
        if self.plan['phase'] == 'FRONTEND_READY':
            self.webhook_confirmed(webhook_evidence)
        if self.plan['phase'] == 'WEBHOOK_CONFIRMED':
            self.live(live_evidence, enable)
        return self.summary()

    def rollback_inputs(self):
        state = keys(self.root / 'state.env')
        allowed = [{'RELEASE_SHA': self.plan['legacy_sha'], 'GATEWAY_SLOT': self.plan['legacy_slot']}]
        if self.plan.get('candidate_slot'):
            allowed.append({'RELEASE_SHA': self.sha, 'GATEWAY_SLOT': self.plan['candidate_slot'], 'PAYMENT_RUNTIME': 'payos'})
        require(state in allowed, 'PAYOS_CUTOVER_STATE_DRIFT: refuse overwriting another deployment')
        route_hash = digest(regular(self.route))
        require(route_hash in (self.plan['route_sha256'], self.plan.get('candidate_route_sha256')),
                'PAYOS_CUTOVER_ROUTE_DRIFT: refuse overwriting external route changes')
        def without_flags(path):
            return [line for line in regular(path).decode().splitlines()
                    if line.partition('=')[0] not in ('PAYMENTS_ENABLED', 'PAYOS_WEBHOOK_CONFIRMED')]
        require(without_flags(self.root / 'deploy/.env.production') == without_flags(self.snapshot / 'previous-env.production'),
                'PAYOS_CUTOVER_ENV_DRIFT: refuse overwriting external environment changes')

    def rollback(self, central_run_id=None):
        self.load()
        if self.plan['phase'] == 'ROLLED_BACK':
            return self.summary()
        if self.plan.get('rollback_legacy_starting'):
            # Legacy may already be taking money after a successful gate release.
            # Never replay a database restore or use the auth-independent candidate gate.
            version = decode(self.dbtool(self.plan['legacy_dbtool'], '-readonly', '-schema-version', readonly=True))
            require(version.get('version') == 13, 'legacy rollback completion requires unchanged schema 13')
            self.volume_permissions()
            self.gate(self.plan['legacy_dbtool'])
            return self.rollback_finish()
        # Boundary check BEFORE changing gates, flags, services, routes, or DB.
        if PHASES.index(self.plan['phase']) >= PHASES.index('SCHEMA_READY'):
            self.no_payments()
        self.volume_permissions()
        self.rollback_inputs()
        image = self.plan['legacy_dbtool'] if PHASES.index(self.plan['phase']) < PHASES.index('SCHEMA_READY') else self.plan['candidate_images']['DBTOOL_IMAGE_REF']
        self.gate(image)
        if PHASES.index(self.plan['phase']) >= PHASES.index('SCHEMA_READY'):
            try:
                self.no_payments()  # Fence closed; reject a create that raced first check.
            except MigrationError:
                self.dbtool(image, '-gate-release', '-owner', 'payos-cutover', '-lease-token', self.plan['gate_token'])
                self.plan['gate_token'] = None
                self.save()
                raise
        self.plan['rollback_requested'] = True
        self.save()
        self.set_flags(False, self.plan['phase'] in ('WEBHOOK_CONFIRMED', 'LIVE'))
        worker = self.inspect('acb-worker')
        legacy = worker['Config']['Image'] == keys(Path(self.plan['legacy_bundle']) / 'runtime.env')['WORKER_IMAGE_REF']
        self.stop(['acb-recovery-controller', 'acb-auth-browser'])
        self.quiesce(legacy)
        self.stop(['acb-worker', 'acb-gateway-blue', 'acb-gateway-green'])
        if self.plan.get('backup'):
            self.no_payments()  # second check with every writer drained
            receipt = self.plan['backup']['receipt']
            require(file_digest(receipt['path']) == receipt['sha256'], 'rollback backup checksum mismatch')
            db = self.volume_database()
            require(db.is_file() and not db.is_symlink(), 'database restore target invalid')
            # Volume writers stopped. Copy with fsync + atomic replacement; no live restore.
            self.plan['rollback_restore_intent'] = True
            self.save()
            with contextlib.closing(sqlite3.connect(db)) as checkpoint:
                require(checkpoint.execute('PRAGMA wal_checkpoint(TRUNCATE)').fetchone()[0] == 0,
                        'rollback SQLite checkpoint busy; writers must remain stopped')
            # Remove only checkpointed sidecars BEFORE replacing the main file;
            # no crash window may replay candidate WAL against the restored legacy DB.
            for suffix in ('-wal', '-shm'):
                path = Path(str(db) + suffix)
                if path.exists():
                    path.unlink()
            fsync_directory(db.parent)
            atomic_copy(receipt['path'], db, receipt['sha256'])
            require(self.database_summary(db) == self.plan['backup']['history'], 'restored history mismatch')
            # Restored snapshot contains legacy gate; admit with LEGACY tool only.
            self.gate(self.plan['legacy_dbtool'])
        if self.plan.get('central_publish'):
            self.central_receipt('rollback', central_run_id)
            prior = self.plan['prior_frontend']
            self.frontend_release(prior['sha'], prior.get('bank_access_headers_file'), False)
        for name, target in [('previous-env.production', self.root / 'deploy/.env.production'),
                             ('previous-state.env', self.root / 'state.env')]:
            atomic(target, regular(self.snapshot / name))
        self.shell(self.release, 'route_replace', [self.snapshot / 'previous-acb.yml', self.route, digest(regular(self.route))])
        self.plan['rollback_legacy_starting'] = True
        self.save()  # Durable before any legacy producer can restart.
        return self.rollback_finish()

    def rollback_finish(self):
        self.compose(True, 'up', '-d', '--no-deps', 'auth-browser', 'tts-gateway', 'bark', 'worker', 'gateway-' + self.plan['legacy_slot'])
        self.health(True)
        self.dbtool(self.plan['legacy_dbtool'], '-gate-release', '-owner', 'payos-cutover', '-lease-token', self.plan['gate_token'])
        self.plan['gate_token'] = None
        self.save()
        # Preserve disabled legacy profiles; start the controller only after admission release.
        old_env = regular(self.snapshot / 'previous-env.production').decode().splitlines()
        if any(line.strip() == 'AUTH_RECOVERY_ENABLED=true' for line in old_env):
            self.compose(True, 'up', '-d', '--no-deps', 'recovery-controller')
        self.save('ROLLED_BACK')
        atomic(self.done, self.plan)
        self.marker.unlink()
        fsync_directory(self.root)
        return self.summary()

    def summary(self):
        # Never include token, secrets, receipt payload, capabilities or paths.
        return {'phase': self.plan['phase'], 'release_sha': self.sha,
                'source_run_id': self.plan['source']['source_run_id'],
                'authoritative_backup': bool(self.plan.get('backup')),
                'operation': 'rollback' if self.plan.get('rollback_requested') else 'apply',
                'rollback_step': 'LEGACY_STARTING' if self.plan.get('rollback_legacy_starting') else
                                 'DATABASE_RESTORING' if self.plan.get('rollback_restore_intent') else None,
                'payment_counts': self.plan.get('final_counts', self.plan.get('baseline_counts'))}


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument('--check', metavar='SHA', help='readonly actual VPS/bundle/GitHub/secret preflight; does not acquire a gate')
    mode.add_argument('--apply', metavar='SHA', help='resume durable phases; never repeats ambiguous central dispatch')
    mode.add_argument('--rollback-before-payments', action='store_true', help='restore only before any provider order/receipt exists')
    parser.add_argument('--source-run-id', help='successful staging-only deploy.yml run (target=all,deploy=false,rehearse=false)')
    parser.add_argument('--registry-path', default='cloudflare/registry/acb.json', help='central acb registry JSON path; automatic must be false')
    parser.add_argument('--prior-frontend', help='private JSON {app:acb,sha,version_id,central_run_id,bank_access_headers_file}; verified prior central version')
    parser.add_argument('--central-run-id', help='successful exact central publish/rollback run ID, provided after dispatch')
    parser.add_argument('--webhook-evidence', help='owner JSON {app,sha,owner_confirmed:true,sample_acknowledged:true,callback_url}')
    parser.add_argument('--enable-payments', action='store_true', help='explicitly enable real-bank acceptance after owner confirms')
    parser.add_argument('--live-evidence', help='real-bank JSON: app,sha,provider,bank,user_transferred, references[static,operator1,operator2], amount_vnd; booleans same_amount_out_of_order,sse,tts_once,outbound_deliveries,lost_sse_recovered,replay_deduplicated. DB independently proves receipts, journal and delivered Bark/webhooks; expected_amounts_vnd defaults [2000,3000,3000].')
    args = parser.parse_args(argv)
    os.umask(0o077)
    try:
        require(os.name == 'posix', 'Linux deployment host required for readonly check and lease-fenced mutation; --help is platform independent')
        driver = Migration(os.environ.get('DEPLOY_PATH', '/opt/bank-event-gateway'), args.check or args.apply,
                           args.source_run_id, args.registry_path)
        if args.check:
            if driver.marker.exists() or driver.done.exists():
                driver.load()
                driver.volume_permissions()
                result = driver.summary()
            else:
                plan = driver.prepare(args.prior_frontend)
                result = {'phase': 'CHECKED', 'release_sha': plan['sha'], 'source_run_id': plan['source']['source_run_id']}
        else:
            import fcntl
            with open(driver.root / '.deploy.lock', 'a') as lock:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
                if args.rollback_before_payments:
                    result = driver.rollback(args.central_run_id)
                else:
                    result = driver.apply(args.prior_frontend, args.central_run_id, args.webhook_evidence,
                                          args.live_evidence, args.enable_payments)
        print(json.dumps(result, sort_keys=True))
        return 0
    except (MigrationError, OSError, KeyError, TypeError, ValueError, AttributeError, sqlite3.Error) as error:
        # OSError/DB exception text can contain sensitive paths/content.
        message = str(error) if isinstance(error, MigrationError) else 'migration input/IO/database failure'
        print('payos-cutover ERROR: ' + message, file=sys.stderr)
        return 1


if __name__ == '__main__':
    sys.exit(main())
