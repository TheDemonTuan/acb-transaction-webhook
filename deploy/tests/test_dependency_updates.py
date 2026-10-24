import re
import unittest
from pathlib import Path

import yaml


ROOT = Path(__file__).resolve().parents[2]


class DependencyUpdateTests(unittest.TestCase):
    def test_pr_gate_requires_reusable_ci_without_deployment(self):
        workflow = yaml.safe_load((ROOT / '.github/workflows/pull-request.yml').read_text())
        triggers = workflow.get('on', workflow.get(True))
        self.assertEqual(triggers['pull_request']['branches'], ['main'])
        self.assertNotIn('pull_request_target', triggers)
        self.assertIn('merge_group', triggers)
        self.assertEqual(workflow['permissions'], {'contents': 'read'})
        jobs = workflow['jobs']
        self.assertEqual(jobs['verify']['uses'], './.github/workflows/ci.yml')
        self.assertNotIn('secrets', jobs['verify'])
        gate = jobs['dependency-update-gate']
        self.assertEqual(gate['name'], 'Dependency Update Gate')
        self.assertEqual(gate['if'], 'always()')
        self.assertEqual(gate['needs'], ['verify'])
        self.assertEqual(gate['steps'][0]['env']['RESULT'], '${{ needs.verify.result }}')
        self.assertEqual(gate['steps'][0]['run'], 'test "$RESULT" = success')
        self.assertNotIn('deploy', jobs)

    def test_development_lock_follows_runtime_constraints(self):
        runtime = (ROOT / 'tts-gateway/requirements.lock').read_text()
        development = (ROOT / 'tts-gateway/requirements-dev.lock').read_text()
        self.assertIn('--constraint=tts-gateway/requirements.lock', development.splitlines()[1])
        pattern = r'^([A-Za-z0-9_.-]+)==([^\s\\]+)'
        runtime_versions = dict(re.findall(pattern, runtime, re.MULTILINE))
        development_versions = dict(re.findall(pattern, development, re.MULTILINE))
        for package in runtime_versions.keys() & development_versions.keys():
            self.assertEqual(runtime_versions[package], development_versions[package], package)

    def test_all_runtime_images_are_scanned_on_arm_before_merge(self):
        ci = yaml.safe_load((ROOT / '.github/workflows/ci.yml').read_text())
        jobs = ci['jobs']
        gateway = jobs['docker-smoke-gateway']
        self.assertEqual(gateway['runs-on'], 'ubuntu-24.04-arm')
        scan = next(step['run'] for step in gateway['steps']
                    if step.get('name') == 'Scan all Go runtime images before merge')
        self.assertIn('for component in gateway worker dbtool', scan)
        self.assertIn('--platform linux/arm64', scan)
        self.assertIn('--exit-code 1 --severity HIGH,CRITICAL', scan)
        self.assertIn('deploy/validate-cve-allowlist.sh', scan)
        tts = jobs['docker-smoke-tts-gateway']
        self.assertEqual(tts['runs-on'], 'ubuntu-24.04-arm')
        self.assertTrue(any('--severity HIGH,CRITICAL' in step.get('run', '')
                            for step in tts['steps']))

    def test_ci_validates_renovate_and_action_pins(self):
        ci = yaml.safe_load((ROOT / '.github/workflows/ci.yml').read_text())
        self.assertFalse((ROOT / '.github/dependabot.yml').exists())
        commands = '\n'.join(step.get('run', '') for step in ci['jobs']['verify']['steps'])
        self.assertRegex(commands, r'renovate@\d+\.\d+\.\d+')
        self.assertIn('renovate-config-validator --strict .github/renovate.json', commands)
        self.assertIn('node scripts/verify-renovate-policy.mjs', commands)
        self.assertIn('bash scripts/verify-actions-pinned.sh', commands)
        for filename in ['ci.yml', 'pull-request.yml']:
            text = (ROOT / '.github/workflows' / filename).read_text()
            for action in re.findall(r'uses:\s*([^\s]+)', text):
                self.assertTrue(action.startswith('./') or re.search(r'@[a-f0-9]{40}$', action), action)


if __name__ == '__main__':
    unittest.main()
