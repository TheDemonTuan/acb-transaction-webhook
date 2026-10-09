import importlib.util
import os
import re
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

import yaml


ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location('workflow_runtime', ROOT / 'deploy/workflow-runtime.py')
runtime = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(runtime)
SHA = 'a' * 40
DIGEST = 'b' * 64


def workflow(name):
    return yaml.safe_load((ROOT / '.github/workflows' / name).read_text(encoding='utf-8'))


def step(document, job, name):
    return next(item for item in document['jobs'][job]['steps'] if item.get('name') == name)


class RuntimePreflightTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)

    def write(self, state='', running=''):
        (self.root / 'state.env').write_text(state)
        (self.root / 'runtime.env').write_text(running)

    def test_legacy_is_staging_only_without_writes(self):
        self.write('RELEASE_SHA=' + SHA + '\nGATEWAY_SLOT=green\n',
                   'BROWSER_IMAGE_REF=historical-digest\n')
        before = {p.name: p.read_bytes() for p in self.root.iterdir()}
        self.assertEqual(runtime.deployment_mode(self.root), 'stage')
        self.assertEqual({p.name: p.read_bytes() for p in self.root.iterdir()}, before)
        self.assertFalse((self.root / '.deploy.lock').exists())

    def test_missing_runtime_is_staging_only(self):
        self.assertEqual(runtime.deployment_mode(self.root), 'stage')
        self.assertEqual(list(self.root.iterdir()), [])

    def test_only_both_payos_markers_admit_normal_deployment(self):
        self.write('PAYMENT_RUNTIME=payos\n', 'PAYMENT_RUNTIME=payos\n')
        self.assertEqual(runtime.deployment_mode(self.root), 'deploy')
        for state, running in [('PAYMENT_RUNTIME=payos\n', ''),
                               ('', 'PAYMENT_RUNTIME=payos\n'),
                               ('PAYMENT_RUNTIME=acb\n', 'PAYMENT_RUNTIME=payos\n'),
                               ('PAYMENT_RUNTIME=payos\nPAYMENT_RUNTIME=payos\n', 'PAYMENT_RUNTIME=payos\n')]:
            with self.subTest(state=state, running=running):
                self.write(state, running)
                with self.assertRaises(ValueError):
                    runtime.deployment_mode(self.root)


class WorkflowBuildTests(unittest.TestCase):
    def setUp(self):
        self.document = workflow('deploy.yml')
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)

    def assemble_images(self, components, bark=None):
        refs = self.root / 'image-refs'
        refs.mkdir(exist_ok=True)
        for component, key in components.items():
            (refs / ('image-ref-' + component)).write_text(key + '=ghcr.io/example/' + component + '@sha256:' + DIGEST + '\n')
        script = step(self.document, 'scan', 'Assemble immutable images.env')['run']
        program = script.split("<<'PY'\n", 1)[1].rsplit('\nPY', 1)[0]
        return subprocess.run([sys.executable, '-c', program, SHA,
                               bark or 'ghcr.io/example/bark@sha256:' + DIGEST],
                              cwd=self.root, text=True, capture_output=True)

    def test_built_digest_artifacts_produce_exact_payos_manifest(self):
        components = {entry['component']: entry['key'] for entry in self.document['jobs']['build']['strategy']['matrix']['include']}
        self.assertEqual(set(components), {'gateway', 'worker', 'dbtool', 'tts-gateway'})
        result = self.assemble_images(components)
        self.assertEqual(result.returncode, 0, result.stderr)
        values = dict(line.split('=', 1) for line in (self.root / 'images.env').read_text().splitlines())
        self.assertEqual(set(values), {'RELEASE_SHA', 'PAYMENT_RUNTIME', 'GATEWAY_IMAGE_REF',
                                      'WORKER_IMAGE_REF', 'DBTOOL_IMAGE_REF', 'TTS_IMAGE_REF', 'BARK_IMAGE_REF'})
        self.assertEqual(values['PAYMENT_RUNTIME'], 'payos')
        self.assertEqual(values['RELEASE_SHA'], SHA)
        self.assertTrue(all(re.fullmatch(r'ghcr\.io/[a-z0-9._/-]+@sha256:[a-f0-9]{64}', value)
                            for key, value in values.items() if key.endswith('_IMAGE_REF')))

    def test_missing_image_or_unpinned_bark_refuses_manifest(self):
        components = {'gateway': 'GATEWAY_IMAGE_REF', 'worker': 'WORKER_IMAGE_REF',
                      'dbtool': 'DBTOOL_IMAGE_REF', 'tts-gateway': 'TTS_IMAGE_REF'}
        missing = dict(components)
        del missing['worker']
        self.assertNotEqual(self.assemble_images(missing).returncode, 0)
        self.assertFalse((self.root / 'images.env').exists())
        self.assertNotEqual(self.assemble_images(components, 'ghcr.io/example/bark:latest').returncode, 0)
        self.assertFalse((self.root / 'images.env').exists())

    @unittest.skipIf(os.name == 'nt' or not shutil.which('bash'), 'POSIX runner required for bundle execution')
    def test_bundle_packages_exact_source_receipt_and_checksum_covers_every_file(self):
        script = step(self.document, 'verified-bundle', 'Assemble verified bundle')['run']
        names = re.search(r'files=\(([^)]+)\)', script).group(1).split()
        (self.root / 'bundle').mkdir()
        (self.root / 'deploy').mkdir()
        for name in names:
            if name == 'images.env':
                (self.root / 'bundle' / name).write_text('PAYMENT_RUNTIME=payos\n')
            elif name != 'source-run.env':
                (self.root / 'deploy' / name).write_text('fixture ' + name + '\n')
        result = subprocess.run(['bash', '-c', script], cwd=self.root,
                                env={**os.environ, 'GITHUB_SHA': SHA, 'GITHUB_RUN_ID': '1234', 'GITHUB_RUN_ATTEMPT': '2'},
                                text=True, capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        bundle = self.root / 'bundle'
        self.assertEqual((bundle / 'source-run.env').read_text(),
                         'RELEASE_SHA=' + SHA + '\nSOURCE_RUN_ID=1234\nSOURCE_RUN_ATTEMPT=2\nPAYMENT_RUNTIME=payos\n')
        self.assertEqual({line.split('  ', 1)[1] for line in (bundle / 'SHA256SUMS').read_text().splitlines()}, set(names))
        self.assertTrue({'migrate-payos-runtime.sh', 'migrate-payos-runtime.py', 'workflow-runtime.py',
                         'restore-db.sh', 'backup-secrets.sh', 'verify-frontend.py'} <= set(names))
        self.assertFalse(any('recovery' in name or 'auth-browser' in name for name in names))
        verification = subprocess.run(['sha256sum', '-c', 'SHA256SUMS'], cwd=bundle, capture_output=True)
        self.assertEqual(verification.returncode, 0, verification.stderr)

    @unittest.skipIf(os.name == 'nt' or not shutil.which('bash'), 'POSIX runner required for remote staging execution')
    def test_remote_legacy_staging_never_logs_in_or_invokes_normal_deploy(self):
        root = self.root / 'runtime'
        stage_dir = root / 'releases' / 'stage'
        stage_dir.mkdir(parents=True)
        (root / 'state.env').write_text('RELEASE_SHA=' + SHA + '\nGATEWAY_SLOT=green\n')
        (root / 'runtime.env').write_text('BROWSER_IMAGE_REF=historical-digest\n')
        legacy = {name: (root / name).read_bytes() for name in ('state.env', 'runtime.env')}
        bundle_script = step(self.document, 'verified-bundle', 'Assemble verified bundle')['run']
        names = re.search(r'files=\(([^)]+)\)', bundle_script).group(1).split()
        for name in names:
            (stage_dir / name).write_text('fixture\n')
        (stage_dir / 'workflow-runtime.py').write_bytes((ROOT / 'deploy/workflow-runtime.py').read_bytes())
        (stage_dir / 'deploy.sh').write_text('touch "$DEPLOY_PATH/normal-deploy-called"\nexit 99\n')
        checksum = subprocess.run(['sha256sum', *names], cwd=stage_dir, text=True, capture_output=True)
        self.assertEqual(checksum.returncode, 0, checksum.stderr)
        (stage_dir / 'SHA256SUMS').write_text(checksum.stdout)
        workflow_script = step(self.document, 'deploy', 'Stage and deploy one immutable bundle')['run']
        remote = workflow_script.split("cat <<'REMOTE'\n", 1)[1].split('\nREMOTE\n', 1)[0]
        remote = remote.replace('${{ github.actor }}', 'fixture-user')
        result = subprocess.run(['bash', '-c', remote], cwd=self.root, text=True, capture_output=True,
                                env={**os.environ, 'DEPLOY_PATH': str(root), 'STAGE': str(stage_dir),
                                     'SHA': SHA, 'MODE': 'stage', 'SOURCE_RUN_ID': '1234', 'GH_TOKEN': 'fixture-token'})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('PAYOS_RUNTIME_MIGRATION_REQUIRED', result.stdout)
        self.assertNotIn('fixture-token', result.stdout + result.stderr)
        self.assertEqual({name: (root / name).read_bytes() for name in legacy}, legacy)
        self.assertFalse((root / 'normal-deploy-called').exists())
        self.assertFalse((root / '.deploy.lock').exists())
        self.assertFalse(list(root.glob('.docker-auth.*')))
        self.assertTrue((root / 'releases' / SHA / 'workflow-runtime.py').is_file())

    @unittest.skipIf(os.name == 'nt' or not shutil.which('bash'), 'POSIX runner required for smoke command execution')
    def test_image_smoke_uses_three_secret_files_and_keeps_provider_gates_closed(self):
        bin_dir = self.root / 'bin'
        bin_dir.mkdir()
        docker = bin_dir / 'docker'
        docker.write_text('#!' + sys.executable + '\n' + '''import json, os, pathlib, sys
args = sys.argv[1:]
if args[0] == 'run':
    settings = [args[index + 1] for index, value in enumerate(args) if value == '-e']
    mount = args[args.index('--mount') + 1]
    directory = pathlib.Path(mount.split('src=', 1)[1].split(',', 1)[0])
    pathlib.Path(os.environ['SMOKE_RECEIPT']).write_text(json.dumps({
        'settings': settings, 'mount': mount,
        'secrets': {p.name: p.read_text() for p in directory.iterdir()}}))
elif args[0] == 'exec':
    assert args == ['exec', 'smoke-test', '/gateway', '--healthcheck']
elif args[0] != 'rm':
    raise SystemExit('Unexpected smoke operation')
''')
        docker.chmod(0o755)
        receipt = self.root / 'smoke.json'
        script = step(workflow('ci.yml'), 'docker-smoke-gateway',
                      'Run gateway boot and storage health smoke (not provider confirmation)')['run']
        result = subprocess.run(['bash', '-c', script], cwd=self.root, text=True, capture_output=True,
                                env={**os.environ, 'PATH': str(bin_dir) + os.pathsep + os.environ['PATH'],
                                     'RUNNER_TEMP': str(self.root), 'SMOKE_RECEIPT': str(receipt)})
        self.assertEqual(result.returncode, 0, result.stderr)
        import json
        recorded = json.loads(receipt.read_text())
        settings = dict(value.split('=', 1) for value in recorded['settings'])
        self.assertEqual(settings['PAYMENTS_ENABLED'], 'false')
        self.assertEqual(settings['PAYOS_WEBHOOK_CONFIRMED'], 'false')
        self.assertEqual(set(recorded['secrets']), {'payos_client_id', 'payos_api_key', 'payos_checksum_key'})
        for key, name in [('PAYOS_CLIENT_ID_FILE', 'payos_client_id'),
                          ('PAYOS_API_KEY_FILE', 'payos_api_key'), ('PAYOS_CHECKSUM_KEY_FILE', 'payos_checksum_key')]:
            self.assertEqual(settings[key], '/run/secrets/' + name)
        self.assertTrue(recorded['mount'].endswith(',readonly'))
        self.assertFalse((self.root / 'payos-smoke-secrets').exists())


if __name__ == '__main__':
    unittest.main()
