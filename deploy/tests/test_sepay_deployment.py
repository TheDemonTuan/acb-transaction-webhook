"""Local filesystem checks for the SePay deployment secret contract."""
import concurrent.futures
import hashlib
import io
import json
import os
from pathlib import Path
import shutil
import stat
import subprocess
import tarfile
import tempfile
import unittest
import yaml

DEPLOY = Path(__file__).resolve().parents[1]


@unittest.skipUnless(os.name == 'posix' and shutil.which('bash') and
                     (os.geteuid() == 0 or (os.geteuid(), os.getegid()) == (1000, 1000)),
                     'POSIX root or deployment uid/gid 1000 required')
class SePayBootstrapTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.secrets = self.root / 'secrets'
        self.secrets.mkdir(mode=0o700)
        if os.geteuid() == 0:
            os.chown(self.secrets, 1000, 1000)
        self.config = self.secrets / 'sepay_store_config'

    def bootstrap(self):
        return subprocess.run(['bash', '-Eeuo', 'pipefail', '-c',
                               'source "$1"; bootstrap_sepay_store_config "$2"',
                               'fixture', str(DEPLOY / 'simple-lib.sh'), str(self.secrets)],
                              capture_output=True, text=True)

    def supply(self, payload, mode=0o600):
        self.config.write_bytes(payload)
        self.config.chmod(mode)
        if os.geteuid() == 0:
            os.chown(self.config, 1000, 1000)

    def test_missing_config_is_disabled_owned_and_durable_without_scaffolding(self):
        result = self.bootstrap()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(self.config.read_bytes()), {'mode': 'disabled'})
        info = self.config.lstat()
        self.assertEqual((stat.S_IMODE(info.st_mode), info.st_uid, info.st_gid),
                         (0o600, 1000, 1000))
        self.assertEqual(list(self.secrets.iterdir()), [self.config])

    def test_operator_config_is_never_overwritten_even_if_invalid(self):
        for payload in (b'{"mode":"observe","webhookSecret":"fixture-private"}\n',
                        b'{"mode":"active"}\n', b'invalid operator config\n'):
            with self.subTest(payload=payload):
                self.supply(payload)
                before = self.config.stat()
                result = self.bootstrap()
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(self.config.read_bytes(), payload)
                self.assertEqual(self.config.stat().st_ino, before.st_ino)
                self.assertNotIn('fixture-private', result.stdout + result.stderr)

    def test_unsafe_existing_config_fails_without_repair(self):
        self.supply(b'{"mode":"disabled"}\n', mode=0o644)
        result = self.bootstrap()
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(stat.S_IMODE(self.config.stat().st_mode), 0o644)
        self.assertEqual(self.config.read_bytes(), b'{"mode":"disabled"}\n')

    def test_symlink_is_rejected_without_touching_target(self):
        target = self.root / 'operator-config'
        target.write_bytes(b'private config\n')
        self.config.symlink_to(target)
        result = self.bootstrap()
        self.assertNotEqual(result.returncode, 0)
        self.assertTrue(self.config.is_symlink())
        self.assertEqual(target.read_bytes(), b'private config\n')

    def test_concurrent_missing_bootstraps_publish_one_complete_file(self):
        with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
            results = list(pool.map(lambda _: self.bootstrap(), range(16)))
        for result in results:
            self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.config.read_bytes(), b'{"mode":"disabled"}\n')
        self.assertEqual(list(self.secrets.iterdir()), [self.config])

    @unittest.skipUnless(shutil.which('age') and shutil.which('age-keygen'),
                         'age tools required for encrypted backup contract')
    def test_legacy_secret_backup_bootstraps_and_encrypts_config(self):
        identity = self.root / 'identity'
        generated = subprocess.run(['age-keygen', '-o', str(identity)], capture_output=True, text=True)
        self.assertEqual(generated.returncode, 0, generated.stderr)
        recipient = subprocess.run(['age-keygen', '-y', str(identity)],
                                   check=True, capture_output=True, text=True).stdout.strip()
        names = {'app_master_key', 'worker_internal_token', 'tts_internal_token',
                 'bark_basic_auth_user', 'bark_basic_auth_password', 'sepay_store_config'}
        for name in names - {'sepay_store_config'}:
            path = self.secrets / name
            path.write_bytes(('fixture-secret-' + name + '\n').encode())
            path.chmod(0o640 if name.startswith('bark_') else 0o600)
        backups = self.root / 'backups'
        result = subprocess.run(['bash', str(DEPLOY / 'backup-secrets.sh')],
                                env=dict(os.environ, SECRETS_DIR=str(self.secrets),
                                         BACKUP_DIR=str(backups), BACKUP_AGE_RECIPIENT=recipient,
                                         OFFHOST_BACKUP_HOOK='', RELEASE_COMMIT='a' * 40),
                                capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(self.config.read_bytes()), {'mode': 'disabled'})
        manifests = list(backups.glob('manifest-secrets-*.json'))
        self.assertEqual(len(manifests), 1)
        manifest = json.loads(manifests[0].read_bytes())
        self.assertEqual({item['name'] for item in manifest['secret_files']}, names)
        encrypted = backups / manifest['secrets_bundle']['file']
        plaintext = subprocess.run(['age', '-d', '-i', str(identity), str(encrypted)],
                                   check=True, capture_output=True).stdout
        with tarfile.open(fileobj=io.BytesIO(plaintext)) as archive:
            self.assertEqual(set(archive.getnames()), names)
            for item in manifest['secret_files']:
                expected = (self.secrets / item['name']).read_bytes()
                self.assertEqual(archive.extractfile(item['name']).read(), expected)
                self.assertEqual(item['sha256'], hashlib.sha256(expected).hexdigest())
                self.assertNotIn(expected.decode().strip(), result.stdout + result.stderr)


@unittest.skipUnless(shutil.which('bash'), 'bash required for route validation')
class BaselineRouteTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        sha = 'a' * 40
        self.bundle = self.root / 'releases' / sha
        self.bundle.mkdir(parents=True)
        (self.root / 'state.env').write_text(
            f'RELEASE_SHA={sha}\nGATEWAY_SLOT=blue\nPAYMENT_RUNTIME=payos\n', encoding='utf-8', newline='\n')
        rendered = subprocess.run(['bash', (DEPLOY / 'render-route.sh').as_posix(), 'blue'],
                                  check=True, capture_output=True, text=True)
        self.policy = yaml.safe_load(rendered.stdout)
        del self.policy['http']['routers']['acb-sepay-telegram-router']
        for key in ('sepay-telegram-rate-limit', 'sepay-telegram-body-limit'):
            del self.policy['http']['middlewares'][key]
        self.route = self.root / 'acb.yml'
        self.route.write_text(yaml.safe_dump(self.policy), encoding='utf-8')
        (self.bundle / 'simple-lib.sh').write_bytes((DEPLOY / 'simple-lib.sh').read_bytes())
        (self.bundle / 'render-route.sh').write_text(
            "#!/usr/bin/env bash\ncat <<'BASELINE_YAML'\n" + yaml.safe_dump(self.policy) +
            'BASELINE_YAML\n', encoding='utf-8', newline='\n')
        (self.bundle / 'SHA256SUMS').write_text(''.join(
            hashlib.sha256((self.bundle / name).read_bytes()).hexdigest() + '  ' + name + '\n'
            for name in ('simple-lib.sh', 'render-route.sh')), encoding='utf-8', newline='\n')

    def validate(self, function='validate_baseline_route'):
        return subprocess.run(['bash', '-Eeuo', 'pipefail', '-c',
                               'source "$1"; DEPLOY_PATH="$2"; "$3" "$4" blue',
                               'fixture', (DEPLOY / 'simple-lib.sh').as_posix(), self.root.as_posix(),
                               function, self.route.as_posix()], capture_output=True, text=True)

    def test_prior_policy_remains_valid_only_as_committed_baseline(self):
        current = self.validate('validate_route')
        self.assertNotEqual(current.returncode, 0)
        baseline = self.validate()
        self.assertEqual(baseline.returncode, 0, baseline.stderr)

    def test_unexpected_public_route_is_rejected(self):
        self.policy['http']['routers']['unexpected-public'] = {
            'rule': 'PathPrefix(`/api/v1`)', 'service': 'acb-service'}
        self.route.write_text(yaml.safe_dump(self.policy), encoding='utf-8')
        self.assertNotEqual(self.validate().returncode, 0)

    def test_baseline_renderer_tampering_is_rejected(self):
        with (self.bundle / 'render-route.sh').open('a', encoding='utf-8') as stream:
            stream.write('# modified after bundle publication\n')
        result = self.validate()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('baseline bundle changed', result.stderr)


if __name__ == '__main__':
    unittest.main()
