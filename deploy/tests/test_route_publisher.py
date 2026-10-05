"""Run with sudo Python on Linux: only private throwaway root-owned files change."""
import base64
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shutil
import stat
import subprocess  # nosec B404: isolated Linux fixture tests root helper
import tempfile
import unittest


@unittest.skipUnless(hasattr(os, 'geteuid') and os.geteuid() == 0, 'requires disposable root-owned Linux fixture')
class PublisherTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='acb-publisher-test-', dir='/root')
        self.root = Path(self.temp.name)
        self.dynamic = self.root / 'dynamic'
        self.dynamic.mkdir(mode=0o755)
        self.destination = self.dynamic / 'acb.yml'
        self.templates = self.root / 'policy/templates'
        self.templates.mkdir(parents=True)
        self.lock = self.root / 'publisher.lock'
        source = Path(__file__).parents[1] / 'acb-route-publish.py'
        spec = importlib.util.spec_from_file_location('route_publisher', source)
        self.publisher = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(self.publisher)
        self.blue = self.route('blue')
        self.green = self.route('green')
        for slot, data in [('blue', self.blue), ('green', self.green)]:
            (self.templates / (slot + '.yml')).write_bytes(data)
        self.destination.write_bytes(self.blue)
        self.destination.chmod(0o644)
        self.foreign = self.dynamic / '9router.yml'
        self.foreign.write_bytes(b'foreign application config must remain unchanged\n')
        self.original_foreign = self.foreign.read_bytes()
        self.original_stat = self.foreign.stat()

    def tearDown(self):
        self.assertEqual(self.foreign.read_bytes(), self.original_foreign)
        self.assertEqual((self.foreign.stat().st_ino, self.foreign.stat().st_mtime_ns),
                         (self.original_stat.st_ino, self.original_stat.st_mtime_ns))
        self.temp.cleanup()

    def route(self, slot):
        return ("http:\n  routers:\n    acb-deploy-gateway:\n      rule: Host(`gateway-deploy.acb.internal.invalid`)\n      service: acb-service\n  services:\n    acb-service:\n      loadBalancer:\n        servers:\n          - url: http://acb-web-" + slot + ":8090\n").encode()

    def envelope(self, data, expected=None):
        return {'expected_sha256': expected or hashlib.sha256(self.destination.read_bytes()).hexdigest(),
                'route_base64': base64.b64encode(data).decode()}

    def publish(self, data, expected=None):
        return self.publisher.publish(self.envelope(data, expected), self.destination, self.templates, self.lock)

    def test_publish_restore_atomic_identity_and_permissions(self):
        before = self.destination.stat().st_ino
        digest = self.publish(self.green)
        self.assertEqual(self.destination.read_bytes(), self.green)
        self.assertEqual(digest, hashlib.sha256(self.green).hexdigest())
        self.assertNotEqual(self.destination.stat().st_ino, before)
        self.assertEqual((self.destination.stat().st_uid, self.destination.stat().st_gid,
                          stat.S_IMODE(self.destination.stat().st_mode)), (0, 0, 0o644))
        self.publish(self.blue)
        self.assertEqual(self.destination.read_bytes(), self.blue)
        self.assertEqual(list(self.dynamic.glob('.acb-*')), [])

    def test_foreign_objects_and_policy_change_refused(self):
        for data in (self.green.replace(b'acb-deploy-gateway', b'9router-router'),
                     self.green.replace(b'acb-web-green:8090', b'other-app:8080'),
                     self.green.replace(b'Host(`gateway-deploy.acb.internal.invalid`)', b'Host(`evil.example`)')):
            with self.subTest(data=data):
                with self.assertRaises(self.publisher.PublishError):
                    self.publish(data)
                self.assertEqual(self.destination.read_bytes(), self.blue)

    def test_obsolete_frontend_topology_refused_even_in_root_policy(self):
        topology = self.publisher.yaml.safe_load(self.green)
        topology['http']['services']['acb-frontend-service'] = {
            'loadBalancer': {'servers': [{'url': 'http://acb-frontend-green:8080'}]}}
        legacy = self.publisher.yaml.safe_dump(topology).encode()
        (self.templates / 'legacy.yml').write_bytes(legacy)
        with self.assertRaises(self.publisher.PublishError):
            self.publish(legacy)
        self.assertEqual(self.destination.read_bytes(), self.blue)

    def test_compare_before_write_refuses_stale_release(self):
        expected = hashlib.sha256(self.blue).hexdigest()
        self.publish(self.green)
        with self.assertRaises(self.publisher.PublishError):
            self.publish(self.blue, expected)
        self.assertEqual(self.destination.read_bytes(), self.green)

    def test_symlink_and_deploy_writable_policy_refused(self):
        policy = self.templates / 'blue.yml'
        policy.chmod(0o666)
        with self.assertRaises(self.publisher.PublishError):
            self.publish(self.green)
        policy.chmod(0o644)
        self.destination.unlink()
        self.destination.symlink_to(self.foreign)
        with self.assertRaises(self.publisher.PublishError):
            self.publish(self.green)

    def test_duplicate_yaml_and_arbitrary_destination_envelope_refused(self):
        data = self.green + b'http: {}\n'
        with self.assertRaises(self.publisher.PublishError):
            self.publish(data)
        envelope = self.envelope(self.green)
        envelope['destination'] = str(self.foreign)
        with self.assertRaises(self.publisher.PublishError):
            self.publisher.publish(envelope, self.destination, self.templates, self.lock)

    def test_fixed_cli_rejects_arguments(self):
        source = Path(__file__).parents[1] / 'acb-route-publish.py'
        result = subprocess.run(['/usr/bin/python3', str(source), str(self.foreign)],  # nosec B603 B607
                                input=json.dumps(self.envelope(self.green)), text=True, capture_output=True, check=False)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.destination.read_bytes(), self.blue)

    def test_deploy_user_publish_and_restore_through_fixed_root_helper(self):
        import pwd
        runuser = shutil.which('runuser', path='/usr/sbin:/usr/bin:/sbin:/bin')
        self.assertIsNotNone(runuser, 'runuser is required for the root publisher integration test')
        prerequisite = 'run this test via sudo from a non-root passwordless-sudo test account'
        username = os.environ.get('SUDO_USER')
        self.assertTrue(username, prerequisite)
        try:
            user = pwd.getpwnam(username)
        except KeyError:
            self.fail(prerequisite)
        self.assertNotEqual(user.pw_uid, 0, prerequisite)
        source = (Path(__file__).parents[1] / 'acb-route-publish.py').read_text()
        # A root-owned throwaway installation changes only the fixed constants;
        # no production helper, policy, directory or sudoers file is touched.
        source = source.replace("Path('/opt/platform/edge/dynamic/acb.yml')", f'Path({str(self.destination)!r})')
        source = source.replace("Path('/etc/acb-route-publisher/templates')", f'Path({str(self.templates)!r})')
        source = source.replace("Path('/run/lock/acb-route-publisher.lock')", f'Path({str(self.lock)!r})')
        helper = self.root / 'acb-route-publish'
        helper.write_text(source)
        helper.chmod(0o755)
        direct = subprocess.run([runuser, '-u', username, '--', '/usr/bin/python3', '-c',  # nosec B603 B607
                                 'import pathlib,sys; pathlib.Path(sys.argv[1]).write_text("bad")',
                                 str(self.destination)], capture_output=True, text=True, check=False)
        self.assertNotEqual(direct.returncode, 0)
        for data in (self.green, self.blue):
            result = subprocess.run([runuser, '-u', username, '--', '/usr/bin/sudo', '-n', str(helper)],  # nosec B603 B607
                                    input=json.dumps(self.envelope(data)), capture_output=True, text=True, check=False)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(self.destination.read_bytes(), data)
            self.assertEqual(stat.S_IMODE(self.destination.stat().st_mode), 0o644)
        rejected = subprocess.run([runuser, '-u', username, '--', '/usr/bin/sudo', '-n', str(helper)],  # nosec B603 B607
                                  input=json.dumps(self.envelope(self.green.replace(b'acb-service', b'9router-service'))),
                                  capture_output=True, text=True, check=False)
        self.assertNotEqual(rejected.returncode, 0)
        self.assertEqual(self.destination.read_bytes(), self.blue)


if __name__ == '__main__':
    unittest.main()
