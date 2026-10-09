import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

import yaml

DEPLOY = Path(__file__).resolve().parents[1]
SHA = 'a' * 40
DIGEST = 'ghcr.io/example/runtime@sha256:' + 'b' * 64

@unittest.skipUnless(os.name == 'posix' and shutil.which('bash'), 'POSIX filesystem and bash required for deployment shell contracts')
class RuntimeAdmissionTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.release = self.root / 'releases' / SHA
        self.release.mkdir(parents=True)
        self.runtime = {
            'IMAGE_REF_BLUE': DIGEST, 'IMAGE_REF_GREEN': DIGEST,
            'WORKER_IMAGE_REF': DIGEST, 'TTS_IMAGE_REF': DIGEST,
            'BARK_IMAGE_REF': DIGEST, 'DBTOOL_IMAGE_REF': DIGEST,
            'RELEASE_COMMIT_BLUE': SHA, 'RELEASE_COMMIT_GREEN': SHA,
            'WORKER_RELEASE_COMMIT': SHA,
            'ENV_FILE': str(self.root / 'deploy/.env.production'),
            'SECRETS_DIR': str(self.root / 'deploy/secrets'),
            'BARK_SECRET_GROUP': '1000', 'PAYMENT_RUNTIME': 'payos',
        }
        self.write_env(self.release / 'runtime.env', self.runtime)
        self.write_env(self.root / 'state.env', {'RELEASE_SHA': SHA, 'GATEWAY_SLOT': 'blue', 'PAYMENT_RUNTIME': 'payos'})
        self.write_env(self.release / 'images.env', dict(RELEASE_SHA=SHA, PAYMENT_RUNTIME='payos', **{key: DIGEST for key in ('GATEWAY_IMAGE_REF', 'WORKER_IMAGE_REF', 'DBTOOL_IMAGE_REF', 'TTS_IMAGE_REF', 'BARK_IMAGE_REF')}))
        self.bin = self.root / 'bin'
        self.bin.mkdir()
        docker = self.bin / 'docker'
        docker.write_text('#!/usr/bin/env bash\nprintf "%s\\n" "$*" >> "$DOCKER_CALLS"\nprintf "%s\\n" "$PAYMENT_COUNTS"\nexit "${DOCKER_EXIT:-0}"\n')
        docker.chmod(0o755)
        self.env = dict(os.environ, DEPLOY_PATH=str(self.root),
                        PATH=str(self.bin) + os.pathsep + os.environ['PATH'],
                        DOCKER_CALLS=str(self.root / 'docker.calls'),
                        PAYMENT_COUNTS=json.dumps({'orders': 0, 'receipts': 0, 'journalSeq': 0}))

    @staticmethod
    def write_env(path, values):
        path.write_text(''.join(f'{key}={value}\n' for key, value in values.items()))

    def run_deploy(self, *args):
        return subprocess.run(['bash', str(DEPLOY / 'deploy.sh'), *args],
                              env=self.env, text=True, capture_output=True)

    def snapshot(self):
        return {str(path.relative_to(self.root)): path.read_bytes()
                for path in self.root.rglob('*') if path.is_file()}

    def test_legacy_rejected_without_any_mutation(self):
        self.write_env(self.root / 'state.env', {'RELEASE_SHA': SHA, 'GATEWAY_SLOT': 'blue'})
        for args in ((SHA,), ('--check', SHA), ('--reconcile', SHA), ('--rollback',)):
            with self.subTest(args=args):
                before = self.snapshot()
                result = self.run_deploy(*args)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('PAYOS_RUNTIME_MIGRATION_REQUIRED', result.stderr)
                self.assertEqual(self.snapshot(), before)

    def test_pending_cutover_rejected_without_lock(self):
        (self.root / '.payos-cutover-pending').write_text('{"phase":"BACKEND_READY"}\n')
        before = self.snapshot()
        result = self.run_deploy(SHA)
        self.assertIn('PAYOS_RUNTIME_MIGRATION_REQUIRED', result.stderr)
        self.assertEqual(self.snapshot(), before)

    def test_runtime_marker_missing_rejected_without_mutation(self):
        self.runtime.pop('PAYMENT_RUNTIME')
        self.write_env(self.release / 'runtime.env', self.runtime)
        before = self.snapshot()
        result = self.run_deploy(SHA)
        self.assertIn('PAYOS_RUNTIME_MIGRATION_REQUIRED', result.stderr)
        self.assertEqual(self.snapshot(), before)

    def legacy_rollback(self):
        rollback = self.root / 'rollback'
        rollback.mkdir(exist_ok=True)
        self.write_env(rollback / 'previous-state.env', {'RELEASE_SHA': 'c' * 40, 'GATEWAY_SLOT': 'green'})
        return self.run_deploy('--rollback')

    def test_issued_orders_or_receipts_block_provider_rollback_before_lock(self):
        for orders, receipts in ((1, 0), (0, 1), (2, 3)):
            with self.subTest(orders=orders, receipts=receipts):
                self.env['PAYMENT_COUNTS'] = json.dumps({'orders': orders, 'receipts': receipts, 'journalSeq': 9})
                state = (self.root / 'state.env').read_bytes()
                result = self.legacy_rollback()
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('PAYOS_ROLLBACK_REQUIRES_PAYMENT_DRAIN', result.stderr)
                self.assertEqual((self.root / 'state.env').read_bytes(), state)
                self.assertFalse((self.root / '.deploy.lock').exists())
                calls = (self.root / 'docker.calls').read_text()
                self.assertIn('--read-only', calls)
                self.assertIn('/data:ro', calls)
                self.assertIn('-payment-counts', calls)
                self.assertNotIn(' stop ', calls)

    def test_legacy_rollback_without_issuance_requires_explicit_tool(self):
        result = self.legacy_rollback()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('PAYOS_RUNTIME_MIGRATION_REQUIRED', result.stderr)
        self.assertFalse((self.root / '.deploy.lock').exists())

    def test_count_failure_never_admits_restore(self):
        for payload, code in (('not-json', '0'), ('{}', '0'), ('{"orders":false,"receipts":0,"journalSeq":0}', '0'), ('', '1')):
            with self.subTest(payload=payload, code=code):
                self.env.update(PAYMENT_COUNTS=payload, DOCKER_EXIT=code)
                result = self.legacy_rollback()
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse((self.root / '.deploy.lock').exists())


class ComposeRuntimeTests(unittest.TestCase):
    def test_runtime_hardening_and_payment_mounts(self):
        compose = yaml.safe_load((DEPLOY / 'compose.prod.yaml').read_text())
        self.assertEqual(set(compose['services']), {'worker', 'gateway-blue', 'gateway-green', 'dbtool', 'tts-gateway', 'bark'})
        self.assertEqual(compose['volumes']['gateway_data']['name'], '${DATA_VOLUME_NAME:-bank-event-gateway_gateway_data}')
        expected_secrets = {'app_master_key', 'worker_internal_token', 'tts_internal_token', 'payos_client_id', 'payos_api_key', 'payos_checksum_key', 'bark_basic_auth_user', 'bark_basic_auth_password'}
        self.assertEqual(set(compose['secrets']), expected_secrets)
        for name in ('worker', 'gateway-blue', 'gateway-green'):
            service = compose['services'][name]
            self.assertTrue(service['read_only'])
            self.assertEqual(service['user'], '1000:1000')
            self.assertEqual(service['cap_drop'], ['ALL'])
            self.assertIn('no-new-privileges:true', service['security_opt'])
            self.assertIn('gateway_data:/data', service['volumes'])
            self.assertIn('acb-core', service['networks'])
            env = service['environment']
            self.assertEqual(env['PAYMENTS_ENABLED'], '${PAYMENTS_ENABLED:-false}')
            self.assertEqual(env['PAYOS_WEBHOOK_CONFIRMED'], '${PAYOS_WEBHOOK_CONFIRMED:-false}')
            self.assertEqual(env['PAYMENT_MAX_AMOUNT_VND'], '${PAYMENT_MAX_AMOUNT_VND:-500000000}')
            for secret, key in (('payos_client_id', 'PAYOS_CLIENT_ID_FILE'), ('payos_api_key', 'PAYOS_API_KEY_FILE'), ('payos_checksum_key', 'PAYOS_CHECKSUM_KEY_FILE')):
                self.assertIn(secret, service['secrets'])
                self.assertEqual(env[key], '/run/secrets/' + secret)
            self.assertFalse(any(key.startswith(('POLL_', 'AUTH_BROWSER', 'AI_CAPTCHA', 'AUTH_RECOVERY')) for key in env))


if __name__ == '__main__':
    unittest.main()
