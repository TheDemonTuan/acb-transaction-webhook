"""Disposable filesystem/SQLite drills with strict docker/GitHub command fixtures.

No daemon, provider, production path, or user bank transfer is accessed. Assertions
observe durable state and command effects, not script source/wording.
"""
import contextlib
import email.message
import importlib.util
import io
import json
import os
from pathlib import Path
import sqlite3
import tempfile
import unittest
from unittest import mock
import zipfile

SPEC = importlib.util.spec_from_file_location('payos_migration', Path(__file__).parents[1] / 'migrate-payos-runtime.py')
migration = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(migration)
OLD = 'a' * 40
NEW = 'b' * 40
FRONTEND = 'c' * 40
VERSION = '12345678-1234-1234-1234-123456789abc'
IMAGE = lambda name, value: 'ghcr.io/test/' + name + '@sha256:' + value * 64
LEGACY_TOOL = IMAGE('dbtool', '1')
CANDIDATE_TOOL = IMAGE('dbtool', '2')


def encoded(value):
    return json.dumps(value).encode()


def zipped(files):
    buffer = io.BytesIO()
    with zipfile.ZipFile(buffer, 'w') as archive:
        for name, value in files.items():
            archive.writestr(name, value)
    return buffer.getvalue()


class Fixture(migration.Migration):
    def __init__(self, root):
        super().__init__(root, NEW, '99', 'cloudflare/registry/acb.json')
        self.route = self.root / 'dynamic/acb.yml'
        self.calls = []
        self.fail_action = None
        self.active_auth = False
        self.gate_token = None
        self.current_frontend = FRONTEND
        self.payment_counts = {'orders': 0, 'receipts': 0, 'journalSeq': 5}
        self.containers = {}
        self.dispatches = []
        self.prior = self.root / 'prior.json'
        self.db = self.root / 'volume/gateway.db'
        self.db.parent.mkdir()
        self.root.joinpath('deploy/secrets').mkdir(parents=True)
        self.root.joinpath('data/backups').mkdir(parents=True)
        self.route.parent.mkdir()
        self.route.write_bytes(b'legacy-route\n')
        self.root.joinpath('deploy/.env.production').write_text('AUTH_RECOVERY_ENABLED=true\nPUBLIC_ORIGIN=' + migration.BANK + '\n')
        self.root.joinpath('state.env').write_text('RELEASE_SHA=' + OLD + '\nGATEWAY_SLOT=green\n')
        self.prior.write_bytes(encoded({'app': 'acb', 'sha': FRONTEND, 'version_id': VERSION,
                                      'central_run_id': '88', 'bank_access_headers_file': str(self.root / 'access.json')}))
        (self.root / 'access.json').write_bytes(encoded({'CF-Access-Client-Id': 'fixture-id', 'CF-Access-Client-Secret': 'fixture-secret'}))
        os.chmod(self.root / 'access.json', 0o600)
        self.old_images = self.bundle(OLD, False)
        self.new_images = self.bundle(NEW, True)
        old_runtime = {'IMAGE_REF_BLUE': self.old_images['GATEWAY_IMAGE_REF'], 'IMAGE_REF_GREEN': self.old_images['GATEWAY_IMAGE_REF'],
                       'WORKER_IMAGE_REF': self.old_images['WORKER_IMAGE_REF'], 'BROWSER_IMAGE_REF': self.old_images['BROWSER_IMAGE_REF'],
                       'DBTOOL_IMAGE_REF': LEGACY_TOOL, 'TTS_IMAGE_REF': self.old_images['TTS_IMAGE_REF'],
                       'BARK_IMAGE_REF': self.old_images['BARK_IMAGE_REF']}
        old_bundle = self.root / 'releases' / OLD
        (old_bundle / 'runtime.env').write_text(''.join(k + '=' + v + '\n' for k, v in old_runtime.items()))
        for service, ref in [('worker', old_runtime['WORKER_IMAGE_REF']), ('auth-browser', old_runtime['BROWSER_IMAGE_REF']),
                             ('recovery-controller', old_runtime['WORKER_IMAGE_REF']), ('gateway-green', old_runtime['IMAGE_REF_GREEN']),
                             ('gateway-blue', old_runtime['IMAGE_REF_BLUE'])]:
            self.containers['acb-' + service] = {'State': {'Running': True}, 'Config': {'Image': ref}}
        with contextlib.closing(sqlite3.connect(self.db)) as db, db:
            db.executescript('''CREATE TABLE transactions(id TEXT,credit INTEGER,debit INTEGER,connection_id TEXT);
                INSERT INTO transactions VALUES('old-credit',10000,0,'acb');
                INSERT INTO transactions VALUES('old-debit',0,5000,'acb');
                CREATE TABLE event_journal(seq INTEGER,event_type TEXT,aggregate_id TEXT,payload_json TEXT);
                INSERT INTO event_journal VALUES(5,'bank.transaction.credit','old-credit','{}');
                CREATE TABLE deliveries(id TEXT,event_id TEXT,endpoint_id TEXT,status TEXT);
                INSERT INTO deliveries VALUES('legacy-pending','old-event','hook','PENDING');
                CREATE TABLE auth_attempts(id TEXT,status TEXT);
                CREATE TABLE connections(id TEXT,state TEXT);
                INSERT INTO connections VALUES('acb','MONITORING');
                CREATE TABLE audit_logs(action TEXT);
                CREATE TABLE events(id TEXT,transaction_id TEXT,event_type TEXT);
                CREATE TABLE webhook_endpoints(id TEXT,provider TEXT);''')
        self.archives = {70: zipped({p.name: p.read_bytes() for p in self.release.iterdir() if p.is_file()})}
        self.receipts = {'88': self.receipt(FRONTEND, 'publish', '80'), '100': self.receipt(NEW, 'publish', '99'),
                         '101': self.receipt(FRONTEND, 'rollback', '80')}
        for run_id, receipt in self.receipts.items():
            self.archives[int(run_id) + 1000] = zipped({'receipt.json': encoded(receipt)})

    def bundle(self, sha, payos):
        root = self.root / 'releases' / sha
        root.mkdir(parents=True)
        images = {'RELEASE_SHA': sha, 'GATEWAY_IMAGE_REF': IMAGE('gateway', '3' if payos else '4'),
                  'WORKER_IMAGE_REF': IMAGE('worker', '5' if payos else '6'), 'DBTOOL_IMAGE_REF': CANDIDATE_TOOL if payos else LEGACY_TOOL,
                  'TTS_IMAGE_REF': IMAGE('tts', '7'), 'BARK_IMAGE_REF': IMAGE('bark', '8')}
        if payos:
            images['PAYMENT_RUNTIME'] = 'payos'
        else:
            images['BROWSER_IMAGE_REF'] = IMAGE('browser', '9')
        (root / 'images.env').write_text(''.join(k + '=' + v + '\n' for k, v in images.items()))
        for name in ('simple-lib.sh', 'compose.prod.yaml', 'healthcheck.sh', 'backup-db.sh', 'render-route.sh',
                     'migrate-payos-runtime.sh', 'migrate-payos-runtime.py'):
            (root / name).write_text('external-command-fixture\n')
        self.manifest(root)
        return images

    def manifest(self, root):
        (root / 'SHA256SUMS').write_text(''.join(migration.digest(p.read_bytes()) + '  ' + p.name + '\n'
                                               for p in sorted(root.iterdir()) if p.is_file() and p.name != 'SHA256SUMS'))

    def receipt(self, sha, mode, source_run):
        return {'app': 'acb', 'status': 'passed', 'public_checks_passed': True, 'source_repository': migration.SOURCE,
                'requested_sha': sha, 'source_run_id': source_run, 'mode': mode,
                'active_deployment': {'version_id': VERSION}, 'artifact_checksum': 'e' * 64,
                'source_archive_digest': 'sha256:' + 'f' * 64}


    @contextlib.contextmanager
    def lease_heartbeat(self, image):
        self.dbtool(image, '-gate-renew', '-owner', 'payos-cutover', '-lease-token', self.plan['gate_token'], '-lease-duration', '15m')
        yield
        self.dbtool(image, '-gate-renew', '-owner', 'payos-cutover', '-lease-token', self.plan['gate_token'], '-lease-duration', '15m')

    def action(self, name):
        if self.fail_action == name:
            self.fail_action = None
            raise migration.MigrationError('injected external failure')

    def command(self, args, timeout=120, env=None):
        args = [str(a) for a in args]
        self.calls.append(tuple(args))
        if args[:3] == ['docker', 'volume', 'inspect']:
            return encoded([{'Driver': 'local', 'Mountpoint': str(self.db.parent)}])
        if args[:3] == ['docker', 'image', 'inspect']:
            return encoded([{'Id': args[-1].partition('@')[2]}])
        if args[:2] == ['docker', 'inspect']:
            return encoded([self.containers[args[2]]])
        if args[:3] == ['docker', 'ps', '-aq']:
            name = args[-1].removeprefix('name=^/').removesuffix('$')
            return (name if name in self.containers else '').encode()
        if args[:2] == ['docker', 'stop']:
            self.action('stop-' + args[-1])
            self.containers[args[-1]]['State']['Running'] = False
            return b''
        if args[:2] == ['docker', 'exec']:
            legacy = self.containers['acb-worker']['Config']['Image'] == self.old_images['WORKER_IMAGE_REF']
            self.action('quiesce')
            return encoded({'status': 'quiesced', 'quiesced': True, 'dispatcher': 'IDLE', 'activeDeliveries': 0,
                            'activePoll': False, 'sessionCheckpointed': True, 'activePaymentRequests': 0, 'journalSeq': 5})
        if args[:2] == ['docker', 'run']:
            image = next(a for a in args if '@sha256:' in a)
            flags = args[args.index(image) + 1:]
            action = next(a for a in flags if a.startswith('-') and a not in ('-path', '-readonly'))
            if action == '-gate-status':
                return encoded({'gateState': 'OPEN'})
            if action == '-gate-acquire':
                if self.active_auth and image == LEGACY_TOOL:
                    raise migration.MigrationError('legacy active auth admission denied')
                self.gate_token = 'private-lease'
                return encoded({'gateState': 'LOCKED', 'leaseToken': self.gate_token})
            if action == '-gate-renew':
                if self.gate_token != flags[flags.index('-lease-token') + 1]:
                    raise migration.MigrationError('expired lease')
                return b'{}'
            if action == '-gate-release':
                self.gate_token = None
                return b'{}'
            if action == '-payment-counts':
                return encoded(self.payment_counts)
            if action == '-payos-cutover':
                self.action('cutover')
                if not self.gate_token:
                    raise migration.MigrationError('no lease')
                with contextlib.closing(sqlite3.connect(self.db)) as db, db:
                    db.execute("UPDATE connections SET state='PAUSED'")
                    db.execute("INSERT INTO audit_logs VALUES('PAYOS_CUTOVER')")
                return b'{}'
            if action == '-migrate':
                self.action('migrate')
                with contextlib.closing(sqlite3.connect(self.db)) as db, db:
                    db.executescript('''CREATE TABLE IF NOT EXISTS payment_orders(id TEXT,order_code INTEGER,status TEXT,origin TEXT);
                                       CREATE TABLE IF NOT EXISTS payment_receipts(reference TEXT,amount_vnd INTEGER,order_id TEXT,transaction_id TEXT);''')
                return b'{}'
            if action == '-schema-version':
                with contextlib.closing(sqlite3.connect(self.db)) as db:
                    current = db.execute("SELECT 1 FROM sqlite_master WHERE name='payment_orders'").fetchone()
                return encoded({'version': 15 if current else 14})
            if action == '-check':
                return b'{}'
            raise AssertionError('unhandled dbtool action: ' + action)
        if args[:2] == ['gh', 'api']:
            endpoint = args[2]
            if '/contents/' in endpoint:
                registry = {'app': 'acb', 'repository': migration.SOURCE, 'workflow': 'deploy.yml', 'workers': ['acb-web'], 'automatic': False}
                import base64
                return encoded({'sha': 'registry-pin', 'content': base64.b64encode(encoded(registry)).decode()})
            if '/actions/workflows/cloudflare-deploy.yml/runs?' in endpoint:
                run_ids = [88]
                if 'created=' in endpoint:
                    run_ids = [101 if 'mode=rollback' in self.dispatches[-1] else 100] if self.dispatches else []
                return encoded({'workflow_runs': [{'id': run_id, 'event': 'workflow_dispatch',
                                                   'status': 'completed', 'conclusion': 'success',
                                                   'created_at': '2999-01-01T00:00:00Z'} for run_id in run_ids]})
            if endpoint.endswith('/zip'):
                return self.archives[int(endpoint.split('/')[-2])]
            if '/artifacts?' in endpoint:
                run_id = endpoint.split('/runs/')[1].split('/')[0]
                if run_id == '99':
                    return encoded({'artifacts': [{'id': 70, 'name': 'verified-bundle', 'expired': False,
                                                   'digest': 'sha256:' + migration.digest(self.archives[70])},
                                                  {'id': 71, 'name': 'frontend-dist-' + NEW, 'expired': False,
                                                   'digest': 'sha256:' + 'f' * 64}]})
                archive_id = int(run_id) + 1000
                return encoded({'artifacts': [{'id': archive_id, 'name': 'cloudflare-receipt-acb-' + run_id,
                                               'expired': False, 'digest': 'sha256:' + migration.digest(self.archives[archive_id])}]})
            if '/jobs?' in endpoint:
                return encoded({'jobs': [{'name': 'Deploy to VPS', 'conclusion': 'skipped'}]})
            if '/actions/runs/' in endpoint:
                run_id = endpoint.split('/runs/')[1]
                source = endpoint.startswith('repos/' + migration.SOURCE + '/')
                return encoded({'path': '.github/workflows/deploy.yml' if source else '.github/workflows/cloudflare-deploy.yml',
                                'head_sha': NEW, 'conclusion': 'success', 'status': 'completed', 'event': 'workflow_dispatch',
                                'head_branch': 'main', 'head_repository': {'full_name': migration.SOURCE if source else migration.CENTRAL},
                                'created_at': '2999-01-01T00:00:00Z'})
            raise AssertionError('unhandled gh endpoint: ' + endpoint)
        if args[:3] == ['gh', 'workflow', 'run']:
            self.action('dispatch')
            self.dispatches.append(tuple(args))
            self.current_frontend = FRONTEND if 'mode=rollback' in args else NEW
            return b''
        if args[0] == 'bash' and args[-1] == '--snapshot':
            self.action('backup')
            if any(c['State']['Running'] for name, c in self.containers.items()
                   if name in ('acb-worker', 'acb-auth-browser', 'acb-recovery-controller', 'acb-gateway-blue', 'acb-gateway-green')):
                raise AssertionError('backup attempted with live writer')
            if env['DBTOOL_IMAGE_REF'] != LEGACY_TOOL:
                raise AssertionError('authoritative backup must use running legacy tool')
            root = self.root / 'data/backups/drained'
            root.mkdir()
            with contextlib.closing(sqlite3.connect(self.db)) as source, contextlib.closing(sqlite3.connect(root / 'gateway.db')) as target:
                source.backup(target)
            receipt = {'sha': OLD, 'schema_version': 14, 'path': str(root / 'gateway.db'),
                       'sha256': migration.digest((root / 'gateway.db').read_bytes()), 'created_at': '2026-10-09'}
            (root / 'receipt.json').write_bytes(encoded(receipt))
            self.action('backup-after-write')
            return str(root / 'receipt.json').encode()
        if args[0] == 'bash' and args[1].endswith('render-route.sh'):
            return ('candidate-' + args[-1] + '\n').encode()
        if args[0] == 'bash' and args[1].endswith('healthcheck.sh'):
            self.action('health')
            return b''
        if args[0] == 'bash' and args[1].endswith('deploy.sh'):
            state = migration.keys(self.root / 'state.env')
            state['RELEASE_SHA'] = args[2]
            (self.root / 'state.env').write_text(''.join(k + '=' + v + '\n' for k, v in state.items()))
            return b''
        if args[:2] == ['bash', '-Eeuo']:
            function = args[4].split('; ')[-1].split()[0]
            bundle = Path(args[6])
            values = args[7:]
            if function == 'compose_release':
                candidate = bundle.name == NEW
                for service in values[values.index('--no-deps') + 1:]:
                    if service.startswith('--'):
                        continue
                    ref = (self.new_images if candidate else self.old_images).get('WORKER_IMAGE_REF' if service in ('worker', 'recovery-controller') else
                          'BROWSER_IMAGE_REF' if service == 'auth-browser' else 'GATEWAY_IMAGE_REF' if service.startswith('gateway-') else
                          'TTS_IMAGE_REF' if service == 'tts-gateway' else 'BARK_IMAGE_REF')
                    self.containers['acb-' + service] = {'State': {'Running': True}, 'Config': {'Image': ref}}
                    if service == 'recovery-controller':
                        self.action('controller-after-start')
                self.action('compose-after-start')
                return b''
            if function == 'container_image_check':
                if self.containers[values[0]]['Config']['Image'] != values[1]:
                    raise AssertionError('image mismatch')
                return b''
            if function == 'route_replace':
                source, target, expected = values
                if migration.digest(Path(target).read_bytes()) != expected:
                    raise migration.MigrationError('route drift')
                Path(target).write_bytes(Path(source).read_bytes())
                return b''
            if function == 'load_recovery_flags':
                return b''
            raise AssertionError('unhandled library operation: ' + function)
        raise AssertionError('unhandled command: ' + repr(args))

    def http(self, origin, path, method='GET', headers=None, body=None, first_frame=False):
        self.calls.append(('HTTP', origin, path, method))
        self.action('http')
        hdr = email.message.Message()
        hdr['Content-Type'] = 'application/json'
        if path == '/__release':
            if origin == migration.BANK and not headers:
                hdr['Location'] = 'https://fixture.cloudflareaccess.com/cdn-cgi/access/login/bank'
                return 302, hdr, b''
            hdr.replace_header('Content-Type', 'text/plain')
            return 200, hdr, (self.current_frontend + '\n').encode()
        if path.startswith('/pay'):
            hdr.replace_header('Content-Type', 'text/html')
            return 200, hdr, b'<html>fixture SPA</html>'
        if path == '/api/integrations/payos/webhook' and method == 'POST':
            return (503 if self.gate_token else 400), hdr, b'{"code":"INVALID_JSON"}'
        if path == '/api/public/v1/transactions?limit=1':
            return 200, hdr, b'{"items":[]}'
        if path == '/api/public/v1/payment-config':
            return 200, hdr, b'{"provider":"PAYOS","ready":false}'
        if path == '/api/public/v1/events':
            hdr.replace_header('Content-Type', 'text/event-stream')
            return 200, hdr, b'event: initial_state\ndata: {}\n\n'
        return 403, hdr, b'{}'


class MigrationDrill(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.driver = Fixture(Path(self.tmp.name))

    def stage_to_backend(self):
        with mock.patch.object(self.driver, 'discover_central_run', side_effect=migration.MigrationError('fixture publication paused')):
            with self.assertRaises(migration.MigrationError):
                self.driver.apply(self.driver.prior)
        self.assertEqual(self.driver.plan['phase'], 'BACKEND_READY')

    def test_readonly_check_no_lock_marker_snapshot_or_mutating_command(self):
        before = {p.relative_to(self.driver.root): p.read_bytes() for p in self.driver.root.rglob('*') if p.is_file()}
        self.driver.prepare(self.driver.prior)
        after = {p.relative_to(self.driver.root): p.read_bytes() for p in self.driver.root.rglob('*') if p.is_file()}
        self.assertEqual(before, after)
        self.assertFalse(any('-gate-acquire' in call or call[:2] == ('docker', 'stop') for call in self.driver.calls))

    def test_active_auth_rejected_by_legacy_before_stopping_any_service(self):
        self.driver.active_auth = True
        with self.assertRaises(migration.MigrationError):
            self.driver.apply(self.driver.prior)
        self.assertEqual(self.driver.plan['phase'], 'STAGED')
        self.assertTrue(all(c['State']['Running'] for c in self.driver.containers.values()))
        acquire = [c for c in self.driver.calls if '-gate-acquire' in c]
        self.assertEqual(len(acquire), 1)
        self.assertIn(LEGACY_TOOL, acquire[0])
        self.assertFalse(any(c[:2] == ('docker', 'stop') for c in self.driver.calls))

    def test_authoritative_backup_is_after_every_writer_stopped_and_pre_schema(self):
        self.stage_to_backend()
        backup_index = next(i for i, c in enumerate(self.driver.calls) if c[-1:] == ('--snapshot',))
        cutover_index = next(i for i, c in enumerate(self.driver.calls) if '-payos-cutover' in c)
        self.assertLess(backup_index, cutover_index)
        for service in ('acb-recovery-controller', 'acb-auth-browser', 'acb-worker', 'acb-gateway-blue', 'acb-gateway-green'):
            self.assertTrue(any(c[:2] == ('docker', 'stop') and c[-1] == service for c in self.driver.calls[:backup_index]))
        self.assertTrue(any('-gate-renew' in c for c in self.driver.calls))
        snapshot = self.driver.plan['backup']
        self.assertEqual(snapshot['history'], self.driver.database_summary(Path(snapshot['receipt']['path'])))
        self.assertFalse(self.driver.containers['acb-auth-browser']['State']['Running'])
        self.assertIsNone(self.driver.gate_token)

    def test_stop_crash_resume_keeps_before_images_and_uses_legacy_gate(self):
        self.driver.fail_action = 'stop-acb-worker'
        with self.assertRaises(migration.MigrationError):
            self.driver.apply(self.driver.prior)
        manifest = (self.driver.snapshot / 'manifest.json').read_bytes()
        self.assertEqual(self.driver.plan['phase'], 'LEGACY_ADMITTED')
        self.assertFalse(self.driver.containers['acb-auth-browser']['State']['Running'])
        self.driver.gate_token = None  # simulate lease expiry while offline
        self.stage_to_backend()
        self.assertEqual(manifest, (self.driver.snapshot / 'manifest.json').read_bytes())
        acquire = [c for c in self.driver.calls if '-gate-acquire' in c]
        self.assertTrue(all(LEGACY_TOOL in c for c in acquire))

    def test_candidate_migration_fault_keeps_authoritative_backup_and_resumes_once(self):
        self.driver.fail_action = 'migrate'
        with self.assertRaises(migration.MigrationError):
            self.driver.apply(self.driver.prior)
        self.assertEqual(self.driver.plan['phase'], 'LEGACY_DRAINED')
        receipt = encoded(self.driver.plan['backup'])
        manifest = (self.driver.snapshot / 'manifest.json').read_bytes()
        self.assertTrue(all(not c['State']['Running'] for c in self.driver.containers.values()))
        self.stage_to_backend()
        self.assertEqual(json.loads(receipt), self.driver.plan['backup'])
        self.assertEqual(manifest, (self.driver.snapshot / 'manifest.json').read_bytes())
        self.assertEqual(sum(c[-1:] == ('--snapshot',) for c in self.driver.calls), 1)
        self.assertEqual(sum('-payos-cutover' in c for c in self.driver.calls), 1)

    def test_dispatch_fault_is_not_repeated_and_requires_exact_central_receipt(self):
        self.driver.fail_action = 'dispatch'
        self.stage_to_backend()
        self.assertFalse(self.driver.plan['central_publish']['dispatched'])
        self.driver.current_frontend = NEW  # externally completed ambiguous dispatch
        self.driver.apply(central_run_id='100')
        self.assertEqual(self.driver.plan['phase'], 'FRONTEND_READY')
        self.assertEqual(sum(c[:3] == ('gh', 'workflow', 'run') for c in self.driver.calls), 1)
        self.assertFalse(any('-gate-acquire' in c and CANDIDATE_TOOL in c for c in self.driver.calls))

    def test_gate_denies_webhook_during_schema_failure_then_receiver_recovers(self):
        self.driver.fail_action = 'migrate'
        with self.assertRaises(migration.MigrationError):
            self.driver.apply(self.driver.prior)
        self.assertEqual(self.driver.http(migration.PUBLIC, '/api/integrations/payos/webhook', 'POST')[0], 503)
        self.stage_to_backend()
        self.assertEqual(self.driver.http(migration.PUBLIC, '/api/integrations/payos/webhook', 'POST')[0], 400)

    def test_issued_order_or_receipt_blocks_rollback_without_changes(self):
        self.stage_to_backend()
        for field in ('orders', 'receipts'):
            self.driver.payment_counts[field] = 1
            before = (self.driver.db.read_bytes(), self.driver.route.read_bytes(), (self.driver.root / 'state.env').read_bytes())
            old_calls = len(self.driver.calls)
            with self.assertRaises(migration.MigrationError):
                self.driver.rollback()
            self.assertEqual(before, (self.driver.db.read_bytes(), self.driver.route.read_bytes(), (self.driver.root / 'state.env').read_bytes()))
            self.assertFalse(any(c[:2] == ('docker', 'stop') or '-gate-acquire' in c for c in self.driver.calls[old_calls:]))
            self.driver.payment_counts[field] = 0

    def test_automatic_deploy_reaches_frontend_ready_and_awaits_owner_configuration(self):
        self.driver.automatic = True
        summary = self.driver.apply()
        self.assertEqual(summary['phase'], 'FRONTEND_READY')
        self.assertTrue(summary['awaiting_owner_configuration'])
        self.assertTrue(summary['authoritative_backup'])
        self.assertTrue(self.driver.done.exists())
        self.assertFalse(self.driver.marker.exists())
        self.assertEqual(self.driver.http(migration.PUBLIC, '/api/integrations/payos/webhook', 'POST')[0], 400)
        cfg = self.driver.http(migration.PUBLIC, '/api/public/v1/payment-config')[2]
        self.assertFalse(json.loads(cfg)['ready'])

    def test_automatic_subsequent_deploy_publishes_matching_frontend(self):
        self.driver.automatic = True
        self.driver.apply()
        # Post-cutover state is now payos. A new release deploys via standard deploy.sh
        # and records central publication without repeating authoritative DB backup.
        other_sha = 'd' * 40
        self.driver.bundle(other_sha, True)
        self.driver.sha = other_sha
        self.driver.release = self.driver.root / 'releases' / other_sha
        self.driver.current_frontend = FRONTEND
        self.driver.receipts['100'] = self.driver.receipt(other_sha, 'publish', '99')
        self.driver.archives[1100] = zipped({'receipt.json': encoded(self.driver.receipts['100'])})
        original = self.driver.command
        def commands(args, **kwargs):
            result = original(args, **kwargs)
            if args[:3] == ['gh', 'workflow', 'run']:
                self.driver.current_frontend = other_sha
            return result
        with mock.patch.object(self.driver, 'source_proof', return_value=self.driver.plan['source']), \
                mock.patch.object(self.driver, 'command', side_effect=commands):
            result = self.driver.deploy_and_publish()
        self.assertEqual(result['phase'], 'FRONTEND_READY')
        self.assertEqual(result['release_sha'], other_sha)
        self.assertFalse(result['authoritative_backup'])
        self.assertFalse(result['awaiting_owner_configuration'])

    def test_unresolved_dispatch_times_out_without_redispatch(self):
        self.stage_to_backend()
        count = len(self.driver.dispatches)
        with mock.patch.object(self.driver, 'gh', return_value={'workflow_runs': []}), \
                mock.patch.object(migration.time, 'monotonic', side_effect=[0, 1501]):
            with self.assertRaises(migration.MigrationError):
                self.driver.apply()
        self.assertEqual(len(self.driver.dispatches), count)
        self.assertEqual(self.driver.plan['phase'], 'BACKEND_READY')
        self.assertTrue(self.driver.plan['central_publish']['dispatched'])

    def test_conflicting_exact_receipts_require_review_without_redispatch(self):
        self.stage_to_backend()
        original = self.driver.gh
        def api(endpoint):
            if 'created=' in endpoint:
                return {'workflow_runs': [{'id': run, 'event': 'workflow_dispatch', 'status': 'completed',
                                          'conclusion': 'success', 'created_at': '2999-01-01T00:00:00Z'}
                                         for run in (100, 102)]}
            if '/runs/102/artifacts?' in endpoint:
                return {'artifacts': [{'name': 'cloudflare-receipt-acb-102', 'expired': False}]}
            return original(endpoint)
        receipt = self.driver.receipts['100']
        with mock.patch.object(self.driver, 'gh', side_effect=api), \
                mock.patch.object(self.driver, 'receipt_artifact', return_value=receipt):
            with self.assertRaises(migration.MigrationError):
                self.driver.apply()
        self.assertEqual(len(self.driver.dispatches), 1)
        self.assertEqual(self.driver.plan['phase'], 'BACKEND_READY')

    def test_partial_snapshot_crash_resumes_without_overwriting_before_images(self):
        original_atomic = migration.atomic
        def crash(path, data):
            original_atomic(path, data)
            if Path(path).name == 'previous-env.production':
                raise migration.MigrationError('filesystem interruption after fsync')
        with mock.patch.object(migration, 'atomic', side_effect=crash):
            with self.assertRaises(migration.MigrationError):
                self.driver.stage(self.driver.prior)
        self.assertFalse(self.driver.marker.exists())
        original = (self.driver.snapshot / 'previous-state.env').read_bytes()
        seed = (self.driver.snapshot / 'staged-plan.json').read_bytes()
        self.stage_to_backend()
        self.assertEqual(original, (self.driver.snapshot / 'previous-state.env').read_bytes())
        self.assertEqual(seed, (self.driver.snapshot / 'staged-plan.json').read_bytes())

    def test_backup_response_crash_recovers_same_completed_authoritative_receipt(self):
        self.driver.fail_action = 'backup-after-write'
        with self.assertRaises(migration.MigrationError):
            self.driver.apply(self.driver.prior)
        self.assertEqual(self.driver.plan['phase'], 'LEGACY_ADMITTED')
        receipt = self.driver.root / 'data/backups/drained/receipt.json'
        original = receipt.read_bytes()
        self.assertIsNone(self.driver.plan['backup'])
        self.stage_to_backend()
        self.assertEqual(original, receipt.read_bytes())
        self.assertEqual(self.driver.plan['backup']['receipt_path'], str(receipt))
        self.assertEqual(sum(call[-1:] == ('--snapshot',) for call in self.driver.calls), 1)

    def test_worker_start_fault_keeps_gate_closed_and_resumes_candidate_not_legacy(self):
        self.driver.fail_action = 'compose-after-start'
        with self.assertRaises(migration.MigrationError):
            self.driver.apply(self.driver.prior)
        self.assertEqual(self.driver.plan['phase'], 'SCHEMA_READY')
        self.assertIsNotNone(self.driver.gate_token)
        self.assertTrue(self.driver.containers['acb-worker']['State']['Running'])
        self.assertEqual(self.driver.containers['acb-worker']['Config']['Image'], self.driver.new_images['WORKER_IMAGE_REF'])
        receipt = encoded(self.driver.plan['backup'])
        self.stage_to_backend()
        self.assertEqual(json.loads(receipt), self.driver.plan['backup'])
        self.assertIsNone(self.driver.gate_token)
        self.assertFalse(self.driver.containers['acb-auth-browser']['State']['Running'])


    def test_rollback_completion_after_controller_failure_preserves_new_legacy_money(self):
        self.stage_to_backend()
        self.driver.fail_action = 'controller-after-start'
        with self.assertRaises(migration.MigrationError):
            self.driver.rollback(central_run_id='101')
        self.assertTrue(self.driver.plan['rollback_legacy_starting'])
        self.assertIsNone(self.driver.gate_token)
        with contextlib.closing(sqlite3.connect(self.driver.db)) as db, db:
            db.execute("INSERT INTO transactions VALUES('new-legacy-credit',7000,0,'acb')")
        old_calls = len(self.driver.calls)
        result = self.driver.rollback()
        self.assertEqual(result['phase'], 'ROLLED_BACK')
        with contextlib.closing(sqlite3.connect(self.driver.db)) as db:
            self.assertEqual(db.execute("SELECT credit FROM transactions WHERE id='new-legacy-credit'").fetchone()[0], 7000)
        self.assertFalse(any(CANDIDATE_TOOL in call for call in self.driver.calls[old_calls:]))

    def test_expired_rollback_completion_lease_rejects_active_auth_with_legacy_tool(self):
        self.stage_to_backend()
        self.driver.fail_action = 'compose-after-start'
        with self.assertRaises(migration.MigrationError):
            self.driver.rollback(central_run_id='101')
        self.driver.gate_token = None
        self.driver.active_auth = True
        old_calls = len(self.driver.calls)
        with self.assertRaises(migration.MigrationError):
            self.driver.rollback()
        acquire = [call for call in self.driver.calls[old_calls:] if '-gate-acquire' in call]
        self.assertEqual(len(acquire), 1)
        self.assertIn(LEGACY_TOOL, acquire[0])
        self.assertFalse(any(call[:2] == ('docker', 'stop') for call in self.driver.calls[old_calls:]))

    def test_order_racing_initial_rollback_check_keeps_receiver_and_releases_gate(self):
        self.stage_to_backend()
        original_gate = self.driver.gate
        def issue_before_admission(image):
            original_gate(image)
            self.driver.payment_counts['orders'] = 1
        with mock.patch.object(self.driver, 'gate', side_effect=issue_before_admission):
            with self.assertRaises(migration.MigrationError):
                self.driver.rollback()
        self.assertIsNone(self.driver.gate_token)
        self.assertTrue(self.driver.containers['acb-worker']['State']['Running'])
        self.assertTrue(self.driver.containers['acb-gateway-blue']['State']['Running'])
        self.assertFalse(self.driver.plan.get('rollback_requested'))

    def test_preorder_rollback_restores_exact_legacy_history_routes_env_and_central_version(self):
        self.stage_to_backend()
        original_db = self.driver.plan['backup']['history']
        result = self.driver.rollback()
        self.assertEqual(result['phase'], 'ROLLED_BACK')
        self.assertEqual(self.driver.database_summary(self.driver.db), original_db)
        self.assertEqual(self.driver.route.read_bytes(), b'legacy-route\n')
        self.assertEqual(migration.keys(self.driver.root / 'state.env'), {'RELEASE_SHA': OLD, 'GATEWAY_SLOT': 'green'})
        self.assertEqual(self.driver.containers['acb-worker']['Config']['Image'], self.driver.old_images['WORKER_IMAGE_REF'])
        self.assertTrue(self.driver.containers['acb-auth-browser']['State']['Running'])
        self.assertTrue(self.driver.containers['acb-recovery-controller']['State']['Running'])
        rollback = [c for c in self.driver.dispatches if 'mode=rollback' in c]
        self.assertEqual(len(rollback), 1)
        self.assertIn('sha=' + FRONTEND, rollback[0])
        self.assertIn('version_id=' + VERSION, rollback[0])


if __name__ == '__main__':
    unittest.main()
