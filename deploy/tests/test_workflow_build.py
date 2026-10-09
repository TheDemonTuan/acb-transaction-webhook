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

    def test_pending_cutover_or_publication_cannot_bypass_driver(self):
        self.write('PAYMENT_RUNTIME=payos\n', 'PAYMENT_RUNTIME=payos\n')
        for name in ('.payos-cutover-pending', '.payos-production-pending'):
            marker = self.root / name
            marker.write_text('{}\n')
            self.assertEqual(runtime.deployment_mode(self.root), 'stage')
            marker.unlink()

    def test_invalid_pending_marker_is_rejected(self):
        (self.root / '.payos-cutover-pending').mkdir()
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

    def test_source_deployment_is_manual_candidate_staging_only(self):
        job = self.document['jobs']['deploy']
        self.assertIn("github.event_name == 'workflow_dispatch'", job['if'])
        self.assertIn('inputs.deploy == true', job['if'])
        script = step(self.document, 'deploy', 'Stage immutable bundle without production runtime changes')['run']
        self.assertIn('sudo -n bash', script)
        self.assertIn('sha256sum -c', script)
        self.assertNotIn('docker login', script)
        self.assertNotIn('DEPLOY_GITHUB_TOKEN', script)
        self.assertNotIn('bash "$target/deploy.sh"', script)

    def test_successful_followup_checks_out_exact_source_without_credentials(self):
        document = workflow('production.yml')
        trigger = document.get('on', document.get(True))['workflow_run']
        self.assertEqual(trigger['workflows'], ['Build and deploy gateway'])
        self.assertEqual(trigger['types'], ['completed'])
        job = document['jobs']['production']
        self.assertIn("conclusion == 'success'", job['if'])
        self.assertIn("head_branch == 'main'", job['if'])
        self.assertEqual(job['permissions']['packages'], 'read')
        checkout = next(item for item in job['steps'] if 'checkout@' in item.get('uses', ''))
        self.assertEqual(checkout['with']['ref'], '${{ github.event.workflow_run.head_sha }}')
        self.assertFalse(checkout['with']['persist-credentials'])
        deploy = step(document, 'production', 'Install verified bundle and broker automatic migration and exact publication')
        self.assertEqual(deploy['env']['DEPLOY_GITHUB_TOKEN'], '${{ secrets.DEPLOY_GITHUB_TOKEN }}')
        self.assertEqual(deploy['env']['REGISTRY_TOKEN'], '${{ secrets.GITHUB_TOKEN }}')
        self.assertNotIn('PAYOS_API_KEY', deploy['env'])
        self.assertNotIn('CF_ACCESS_CLIENT_SECRET', deploy['env'])
        self.assertEqual(document['concurrency'], self.document['concurrency'])


if __name__ == '__main__':
    unittest.main()
