import base64
import hashlib
import importlib.util
import io
import json
import os
import stat
import subprocess
import tempfile
import unittest
import zipfile
from pathlib import Path
from unittest.mock import Mock, patch


SPEC = importlib.util.spec_from_file_location('automatic_production', Path(__file__).parents[1] / 'automatic-production.py')
production = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(production)
SHA = 'a' * 40
RUN = '1234'
VERSION = '12345678-1234-1234-1234-123456789abc'


def summary():
    return {'phase': 'FRONTEND_READY', 'release_sha': SHA, 'source_run_id': RUN,
            'authoritative_backup': False, 'awaiting_owner_configuration': True}


def artifact_blob(changes=None, extra=None):
    entries = {name: ('fixture ' + name + '\n').encode() for name in production.FILES}
    entries['images.env'] = ('RELEASE_SHA=' + SHA + '\nPAYMENT_RUNTIME=payos\n' + ''.join(
        key + '=ghcr.io/example/' + key.lower() + '@sha256:' + 'b' * 64 + '\n'
        for key in ('GATEWAY_IMAGE_REF', 'WORKER_IMAGE_REF', 'DBTOOL_IMAGE_REF', 'TTS_IMAGE_REF', 'BARK_IMAGE_REF'))).encode()
    entries['source-run.env'] = ('RELEASE_SHA=' + SHA + '\nSOURCE_RUN_ID=' + RUN + '\nSOURCE_RUN_ATTEMPT=1\nPAYMENT_RUNTIME=payos\n').encode()
    entries['SHA256SUMS'] = ''.join(hashlib.sha256(value).hexdigest() + '  ' + name + '\n'
                                     for name, value in sorted(entries.items())).encode()
    entries.update(changes or {})
    entries.update(extra or {})
    buffer = io.BytesIO()
    with zipfile.ZipFile(buffer, 'w') as archive:
        for name, value in entries.items():
            archive.writestr(name, value)
    blob = buffer.getvalue()
    return blob, {'id': 42, 'digest': 'sha256:' + hashlib.sha256(blob).hexdigest()}


class GitHubBrokerTests(unittest.TestCase):
    def test_scoped_get_endpoints_are_accepted(self):
        for endpoint in [
            f'repos/{production.SOURCE}/actions/runs/{RUN}',
            f'repos/{production.SOURCE}/actions/runs/{RUN}/jobs?per_page=100',
            f'repos/{production.SOURCE}/actions/runs/{RUN}/artifacts?per_page=100',
            f'repos/{production.SOURCE}/actions/artifacts/42/zip',
            f'repos/{production.CENTRAL}/contents/cloudflare/registry/acb.json',
            f'repos/{production.CENTRAL}/actions/runs/44',
            f'repos/{production.CENTRAL}/actions/runs/44/artifacts?per_page=100',
            f'repos/{production.CENTRAL}/actions/artifacts/45/zip',
            f'repos/{production.CENTRAL}/actions/workflows/cloudflare-deploy.yml/runs?status=success&per_page=100',
            f'repos/{production.CENTRAL}/actions/workflows/cloudflare-deploy.yml/runs?event=workflow_dispatch&per_page=100&created=>=2026-10-09',
        ]:
            with self.subTest(endpoint=endpoint):
                production.validate_request(['api', endpoint])

    def test_api_mutations_urls_and_scope_escapes_are_refused(self):
        for args in [
            ['api', f'repos/{production.SOURCE}/actions/runs/{RUN}', '--method', 'POST'],
            ['api', f'https://api.github.com/repos/{production.SOURCE}/actions/runs/{RUN}'],
            ['api', f'repos/{production.CENTRAL}/contents/cloudflare/registry/other.json'],
            ['api', f'repos/{production.CENTRAL}/contents/../../secrets'],
            ['api', 'repos/other/repo/actions/runs/1'],
            ['api', f'repos/{production.SOURCE}/actions/runs/1?token=secret'],
            ['auth', 'token'], ['api', '--input', '-'],
        ]:
            with self.subTest(args=args), self.assertRaises(ValueError):
                production.validate_request(args)

    def dispatch(self, *fields):
        return ['workflow', 'run', 'cloudflare-deploy.yml', '--repo', production.CENTRAL,
                '-f', 'app=acb', *[item for field in fields for item in ('-f', field)]]

    def test_only_exact_central_publication_or_rollback_can_dispatch(self):
        production.validate_request(self.dispatch('mode=publish', 'source_run_id=' + RUN))
        production.validate_request(self.dispatch('mode=rollback', 'sha=' + SHA, 'version_id=' + VERSION))
        for fields in [
            ('mode=bootstrap', 'source_run_id=' + RUN),
            ('mode=publish', 'source_run_id=' + RUN, 'app=other'),
            ('mode=publish', 'source_run_id=' + RUN, 'sha=' + SHA),
            ('mode=publish', 'source_run_id=12;id'),
            ('mode=rollback', 'version_id=latest'),
            ('mode=rollback', 'sha=' + SHA),
            ('mode=rollback', 'version_id=' + VERSION),
            ('mode=rollback', 'sha=' + SHA, 'version_id=latest'),
            ('mode=rollback', 'sha=main', 'version_id=' + VERSION),
            ('mode=publish', 'sha=' + SHA),
            ('mode=publish', 'sha=main'),
            ('mode=publish', 'source_run_id=' + RUN, 'ref=evil'),
        ]:
            with self.subTest(fields=fields), self.assertRaises(ValueError):
                production.validate_request(self.dispatch(*fields))

    def test_token_is_only_local_gh_environment_not_arguments(self):
        with patch.dict(os.environ, {'DEPLOY_GITHUB_TOKEN': 'cross-repo-secret'}), \
                patch.object(production.subprocess, 'run', return_value=subprocess.CompletedProcess([], 0, b'{}')) as run:
            production.github(['api', f'repos/{production.SOURCE}/actions/runs/{RUN}'])
        self.assertEqual(run.call_args.kwargs['env']['GH_TOKEN'], 'cross-repo-secret')
        self.assertNotIn('cross-repo-secret', ' '.join(run.call_args.args[0]))
        with patch.dict(os.environ, {'DEPLOY_GITHUB_TOKEN': 'cross', 'REGISTRY_TOKEN': 'registry', 'GH_TOKEN': 'local'}):
            environment = production.Remote.environment()
        self.assertTrue({'DEPLOY_GITHUB_TOKEN', 'REGISTRY_TOKEN', 'GH_TOKEN', 'GITHUB_TOKEN'}.isdisjoint(environment))


class BundleProofTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)

    def test_exact_archive_and_every_manifest_entry_are_verified(self):
        blob, artifact = artifact_blob()
        production.unpack_bundle(blob, artifact, self.root, SHA, RUN)
        self.assertEqual({p.name for p in self.root.iterdir()}, production.FILES | {'SHA256SUMS'})

    def test_archive_digest_is_checked_before_writes(self):
        blob, artifact = artifact_blob()
        artifact['digest'] = 'sha256:' + '0' * 64
        with self.assertRaises(ValueError):
            production.unpack_bundle(blob, artifact, self.root, SHA, RUN)
        self.assertEqual(list(self.root.iterdir()), [])

    def test_path_traversal_extra_files_and_corruption_are_refused(self):
        for changes, extra in [({}, {'../escape': b'bad'}), ({}, {'unexpected': b'bad'}),
                               ({'deploy.sh': b'corrupt'}, {}),
                               ({'SHA256SUMS': b'0' * 64 + b'  ../escape\n'}, {})]:
            with self.subTest(changes=changes, extra=extra):
                blob, artifact = artifact_blob(changes, extra)
                with self.assertRaises(ValueError):
                    production.unpack_bundle(blob, artifact, self.root, SHA, RUN)
        self.assertFalse((self.root.parent / 'escape').exists())

    def test_symlink_archive_entry_is_refused(self):
        blob, _ = artifact_blob()
        source = zipfile.ZipFile(io.BytesIO(blob))
        buffer = io.BytesIO()
        with zipfile.ZipFile(buffer, 'w') as archive:
            for name in source.namelist():
                member = zipfile.ZipInfo(name)
                member.create_system = 3
                member.external_attr = ((stat.S_IFLNK if name == 'deploy.sh' else stat.S_IFREG) | 0o600) << 16
                archive.writestr(member, source.read(name))
        source.close()
        blob = buffer.getvalue()
        with self.assertRaises(ValueError):
            production.unpack_bundle(blob, {'digest': 'sha256:' + hashlib.sha256(blob).hexdigest()}, self.root, SHA, RUN)

    def test_wrong_source_receipt_is_refused(self):
        blob, artifact = artifact_blob()
        with self.assertRaises(ValueError):
            production.unpack_bundle(blob, artifact, self.root, 'b' * 40, RUN)

    def test_source_success_and_artifact_pair_are_required(self):
        run = {'head_sha': SHA, 'head_branch': 'main', 'head_repository': {'full_name': production.SOURCE},
               'path': '.github/workflows/deploy.yml', 'status': 'completed', 'conclusion': 'success', 'event': 'push'}
        jobs = {'jobs': [{'name': 'Deploy to VPS', 'conclusion': 'skipped'}]}
        backend = {'id': 42, 'name': 'verified-bundle', 'digest': 'sha256:' + 'b' * 64}
        frontend = {'id': 43, 'name': 'frontend-dist-' + SHA}
        with patch.object(production, 'api', side_effect=[run, jobs, {'artifacts': [backend, frontend]}]):
            self.assertEqual(production.source_artifact(SHA, RUN), backend)
        with patch.object(production, 'api', side_effect=[run, jobs, {'artifacts': [backend]}]), self.assertRaises(ValueError):
            production.source_artifact(SHA, RUN)
        with patch.object(production, 'api', return_value={**run, 'conclusion': None}), self.assertRaises(ValueError):
            production.source_artifact(SHA, RUN)
        with patch.object(production, 'api', side_effect=[run, {'jobs': [{'name': 'Deploy to VPS', 'conclusion': 'success'}]}]), self.assertRaises(ValueError):
            production.source_artifact(SHA, RUN)
        with patch.object(production, 'api', side_effect=[{**run, 'event': 'workflow_dispatch'}, jobs, {'artifacts': [frontend]}]):
            self.assertIsNone(production.source_artifact(SHA, RUN))


class BridgeProtocolTests(unittest.TestCase):
    def process(self, messages, returncode=0):
        process = Mock()
        process.stdout = io.StringIO(''.join(json.dumps(message) + '\n' for message in messages))
        process.stdin = Mock()
        process.wait.return_value = returncode
        process.poll.return_value = returncode
        return process

    def test_binary_github_response_is_base64_and_summary_is_sanitized(self):
        process = self.process([{'github_request': ['api', f'repos/{production.SOURCE}/actions/artifacts/42/zip']},
                                {**summary(), 'unsafe_remote_output': 'secret'}])
        with patch.object(production, 'github', return_value=b'\x00\xffzip'):
            result = production.bridge(process, SHA, RUN)
        encoded = json.loads(process.stdin.write.call_args.args[0])
        self.assertEqual(base64.b64decode(encoded['github_response']), b'\x00\xffzip')
        self.assertEqual(result, summary())

    def test_configured_runtime_followup_preserves_completed_summary(self):
        completed = {**summary(), 'awaiting_owner_configuration': False}
        self.assertEqual(production.bridge(self.process([completed]), SHA, RUN), completed)

    def test_disallowed_request_receives_only_error(self):
        process = self.process([{'github_request': ['auth', 'token']}, summary()])
        with patch.object(production.subprocess, 'run') as run:
            production.bridge(process, SHA, RUN)
        run.assert_not_called()
        self.assertEqual(json.loads(process.stdin.write.call_args.args[0]), {'github_error': True})

    def test_invalid_or_missing_summary_and_nonzero_exit_fail(self):
        for messages, code in [([], 0), ([{**summary(), 'release_sha': 'b' * 40}], 0),
                               ([summary(), summary()], 0), ([summary()], 1),
                               ([{**summary(), 'phase': 'BACKEND_READY'}], 0)]:
            with self.subTest(messages=messages, code=code), self.assertRaises(ValueError):
                production.bridge(self.process(messages, code), SHA, RUN)

    def test_remote_commands_use_exact_root_and_no_runner_tokens(self):
        remote = production.Remote({'DEPLOY_PATH': '/opt/bank-event-gateway', 'VPS_HOST': 'vps.example',
                                    'VPS_USER': 'opc', 'VPS_PORT': '22'})
        with patch.dict(os.environ, {'DEPLOY_GITHUB_TOKEN': 'cross-secret', 'REGISTRY_TOKEN': 'registry-secret'}), \
                patch.object(production.subprocess, 'run', return_value=subprocess.CompletedProcess([], 0, b'')) as run:
            remote.run(['sudo', '-n', 'env', 'DEPLOY_PATH=' + remote.root, 'python3',
                        remote.root + '/releases/' + SHA + '/migrate-payos-runtime.py', '--automatic', SHA])
        self.assertNotIn('DEPLOY_GITHUB_TOKEN', run.call_args.kwargs['env'])
        self.assertNotIn('REGISTRY_TOKEN', run.call_args.kwargs['env'])
        self.assertIn('StrictHostKeyChecking=yes', run.call_args.args[0])
        self.assertIn('sudo -n env', run.call_args.args[0][-1])
        self.assertNotIn('cross-secret', run.call_args.args[0][-1])


class ProductionOrchestrationTests(unittest.TestCase):
    def invoke(self, failure=None):
        blob, artifact = artifact_blob()
        remote = Mock()
        remote.root = '/opt/bank-event-gateway'
        remote.environment.return_value = {'PATH': '/usr/bin'}
        remote.args.side_effect = lambda args: ['ssh', 'remote', ' '.join(args)]

        def operation(args, data=None):
            if args[2:4] == ['mktemp', '-d']:
                return b'/tmp/acb-registry.ABCDEFGH\n'
            if failure == 'login' and 'login' in args:
                raise ValueError('login refused')
            return b''

        remote.run.side_effect = operation
        with tempfile.TemporaryDirectory() as directory:
            environment = {'DEPLOY_GITHUB_TOKEN': 'cross-secret', 'REGISTRY_TOKEN': 'registry-secret',
                           'GITHUB_ACTOR': 'owner', 'GITHUB_STEP_SUMMARY': str(Path(directory) / 'summary')}
            with patch.dict(os.environ, environment), \
                    patch.object(production.sys, 'argv', ['automatic-production.py', '--sha', SHA, '--source-run-id', RUN]), \
                    patch.object(production, 'source_artifact', return_value=artifact), \
                    patch.object(production, 'github', return_value=blob), \
                    patch.object(production, 'Remote', return_value=remote), \
                    patch.object(production.subprocess, 'Popen') as popen, \
                    patch.object(production, 'bridge', side_effect=ValueError('driver refused') if failure == 'driver' else None,
                                 return_value=summary()), \
                    patch('builtins.print'):
                if failure:
                    with self.assertRaises(ValueError):
                        production.main()
                else:
                    production.main()
                    command = popen.call_args.args[0][-1]
                    self.assertIn('sudo -n env -i', command)
                    self.assertIn('PAYOS_GITHUB_BRIDGE=1', command)
                    self.assertIn(remote.root + '/releases/' + SHA + '/migrate-payos-runtime.py', command)
                    self.assertIn('--automatic ' + SHA + ' --source-run-id ' + RUN, command)
                    self.assertNotIn('cross-secret', command)
                    self.assertNotIn('registry-secret', command)
            calls = [call.args[0] for call in remote.run.call_args_list]
        return calls

    def test_registry_login_is_stdin_only_and_all_pulls_are_pinned(self):
        calls = self.invoke()
        login = next(args for args in calls if 'login' in args)
        self.assertEqual(login[-1], '--password-stdin')
        self.assertNotIn('registry-secret', login)
        pulls = [args for args in calls if 'pull' in args]
        self.assertEqual(len(pulls), 5)
        self.assertTrue(all('@sha256:' in args[-1] and 'linux/arm64' in args for args in pulls))
        self.assertEqual(calls[-2][-2:], ['logout', 'ghcr.io'])
        self.assertEqual(calls[-1], ['sudo', '-n', 'rm', '-rf', '--', '/tmp/acb-registry.ABCDEFGH'])

    def test_registry_logout_and_directory_cleanup_on_login_or_driver_failure(self):
        for failure in ('login', 'driver'):
            with self.subTest(failure=failure):
                calls = self.invoke(failure)
                self.assertEqual(calls[-2][-2:], ['logout', 'ghcr.io'])
                self.assertEqual(calls[-1][-1], '/tmp/acb-registry.ABCDEFGH')


if __name__ == '__main__':
    unittest.main()
