import importlib.util
import subprocess
import unittest
from pathlib import Path
from unittest.mock import patch


SPEC = importlib.util.spec_from_file_location('classify_changes', Path(__file__).parents[1] / 'classify-changes.py')
classifier = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(classifier)
SHA_A = 'a' * 40
SHA_B = 'b' * 40
VERSION = '12345678-1234-1234-1234-123456789abc'


class ClassifyChangesTests(unittest.TestCase):
    def test_path_contract(self):
        for path in ['web/src/app.tsx', 'deploy/cloudflare-routes.py',
                     'deploy/verify-frontend.py', 'deploy/tests/test_cloudflare_routes.py']:
            with self.subTest(path=path):
                self.assertEqual(classifier.classify_paths([path]), (True, False))
        for path in ['.github/workflows/deploy.yml', '.github/workflows/ci.yml', 'scripts/verify.sh']:
            with self.subTest(path=path):
                self.assertEqual(classifier.classify_paths([path]), (True, True))
        self.assertEqual(classifier.classify_paths(['internal/httpapi/server.go']), (False, True))
        self.assertEqual(classifier.classify_paths(['unknown-new-file']), (False, True))
        self.assertEqual(classifier.classify_paths([]), (False, False))

    def test_dispatch_backend_forcing(self):
        for inputs in [{'target': 'frontend', 'rehearse': True},
                       {'target': 'frontend', 'import_account': '123'}]:
            with self.subTest(inputs=inputs):
                result = classifier.select('workflow_dispatch', inputs, lambda: self.fail('Unexpected auto'))
                self.assertEqual((result['frontend'], result['backend']), ('true', 'true'))
        result = classifier.select('workflow_dispatch', {'target': 'backend', 'deploy': False}, lambda: None)
        self.assertEqual((result['frontend'], result['backend']), ('false', 'true'))

    def test_rollback_and_bootstrap_are_frontend_only(self):
        for special, mode in [({'frontend_version_id': VERSION}, 'rollback'),
                              ({'bootstrap_frontend': True}, 'bootstrap')]:
            inputs = {'target': 'frontend', 'deploy': True, **special}
            result = classifier.select('workflow_dispatch', inputs, lambda: self.fail('Unexpected auto'))
            self.assertEqual(result['frontend_mode'], mode)
            self.assertEqual(result['backend'], 'false')
            for change in [{'target': 'all'}, {'deploy': False}, {'rehearse': True}, {'import_account': '123'}]:
                with self.subTest(change=change, mode=mode), self.assertRaises(ValueError):
                    classifier.select('workflow_dispatch', {**inputs, **change}, lambda: None)
        with self.assertRaises(ValueError):
            classifier.select('workflow_dispatch', {'target': 'frontend', 'deploy': True,
                              'frontend_version_id': VERSION, 'bootstrap_frontend': True}, lambda: None)
        with self.assertRaises(ValueError):
            classifier.select('workflow_dispatch', {'frontend_version_id': 'latest'}, lambda: None)

    def test_auto_selected_on_push_and_dispatch(self):
        auto = lambda: (True, False, 'auto', SHA_A)
        for event, inputs in [('push', {}), ('workflow_dispatch', {'target': 'auto'})]:
            result = classifier.select(event, inputs, auto)
            self.assertEqual((result['frontend'], result['backend'], result['base_sha']), ('true', 'false', SHA_A))

    def test_api_uses_current_workflow_and_paginates(self):
        api = classifier.ActionsAPI('owner/repo', 'not-a-real-token')
        calls = []
        def get(path):
            calls.append(path)
            if path == '/actions/runs/900':
                return {'workflow_id': 77}
            if path.endswith('&page=1'):
                return {'workflow_runs': [{'id': 900, 'workflow_id': 77, 'event': 'push',
                         'head_branch': 'main', 'conclusion': 'success', 'head_sha': SHA_B}] * 100}
            return {'workflow_runs': [
                {'id': 899, 'workflow_id': 88, 'event': 'push', 'head_branch': 'main', 'conclusion': 'success', 'head_sha': SHA_B},
                {'id': 898, 'workflow_id': 77, 'event': 'workflow_dispatch', 'head_branch': 'main', 'conclusion': 'success', 'head_sha': SHA_B},
                {'id': 897, 'workflow_id': 77, 'event': 'push', 'head_branch': 'main', 'conclusion': 'success', 'head_sha': SHA_A},
            ]}
        with patch.object(api, 'get', side_effect=get):
            self.assertEqual(api.previous_success(900), SHA_A)
        self.assertEqual(len(calls), 3)
        self.assertIn('/actions/workflows/77/runs?', calls[1])
        self.assertIn('event=push', calls[1])
        self.assertIn('status=success', calls[1])
        self.assertIn('branch=main', calls[1])

    def test_missing_or_nonancestor_base_selects_both(self):
        api = classifier.ActionsAPI('owner/repo', 'not-a-real-token')
        with patch.object(api, 'previous_success', return_value=None):
            self.assertEqual(classifier.automatic_selection(SHA_B, api, 900)[:2], (True, True))
        with patch.object(api, 'previous_success', return_value=SHA_A), \
                patch.object(classifier.subprocess, 'run', return_value=subprocess.CompletedProcess([], 1)):
            self.assertEqual(classifier.automatic_selection(SHA_B, api, 900)[:2], (True, True))

    def test_diff_covers_failed_release_gap_and_deleted_files(self):
        api = classifier.ActionsAPI('owner/repo', 'not-a-real-token')
        responses = [subprocess.CompletedProcess([], 0),
                     subprocess.CompletedProcess([], 0, stdout=b'web/deleted.ts\0internal/changed.go\0')]
        with patch.object(api, 'previous_success', return_value=SHA_A), \
                patch.object(classifier.subprocess, 'run', side_effect=responses) as run:
            result = classifier.automatic_selection(SHA_B, api, 900)
        self.assertEqual(result[:2], (True, True))
        self.assertEqual(run.call_args_list[1].args[0], ['git', 'diff', '--name-only', '--no-renames', '-z', SHA_A, SHA_B, '--'])


if __name__ == '__main__':
    unittest.main()
