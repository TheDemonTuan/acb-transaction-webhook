"""Exercise router selection from the shipped and rendered Traefik configuration."""
import ast
import re
import shutil
import subprocess
from pathlib import Path
import unittest

import yaml

ROOT = Path(__file__).resolve().parents[2]
PUBLIC = 'transactions.tuannguyenviet.site'
ADMIN = 'bank.tuannguyenviet.site'
CALLBACK = '/api/integrations/payos/webhook'
SEPAY_CALLBACK = '/api/integrations/sepay/telegram'


def matches(rule, host, path, method):
    def matcher(match):
        name, argument = match.groups()
        actual = {'Host': host, 'Path': path, 'PathPrefix': path, 'Method': method}[name]
        return str(actual.startswith(argument) if name == 'PathPrefix' else actual == argument)

    expression = re.sub(r'(Host|Path|PathPrefix|Method)\(`([^`]*)`\)', matcher, rule)
    expression = expression.replace('&&', ' and ').replace('||', ' or ').replace('!', ' not ')
    tree = ast.parse(expression.strip(), mode='eval')

    def boolean(node):
        if isinstance(node, ast.Constant) and isinstance(node.value, bool):
            return node.value
        if isinstance(node, ast.BoolOp):
            values = [boolean(value) for value in node.values]
            if isinstance(node.op, ast.And):
                return all(values)
            if isinstance(node.op, ast.Or):
                return any(values)
        if isinstance(node, ast.UnaryOp) and isinstance(node.op, ast.Not):
            return not boolean(node.operand)
        raise ValueError('Unsupported routing expression')

    return boolean(tree.body)


def selected(config, host, path, method):
    routers = [(name, router) for name, router in config['http']['routers'].items()
               if 'web' in router.get('entryPoints', [])
               and matches(router['rule'], host, path, method)]
    return max(routers, key=lambda item: item[1].get('priority', len(item[1]['rule']))) if routers else (None, None)


class PaymentIngressTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.configurations = [('shipped', yaml.safe_load((ROOT / 'platform/edge/dynamic/acb.yml').read_text()))]
        bash = shutil.which('bash')
        if not bash:
            candidate = Path('C:/Program Files/Git/bin/bash.exe')
            bash = str(candidate) if candidate.exists() else None
        if not bash:
            raise RuntimeError('bash is required to exercise the release route renderer')
        for slot in ('blue', 'green'):
            result = subprocess.run([bash, str(ROOT / 'deploy/render-route.sh'), slot],
                                    check=True, capture_output=True, text=True)
            cls.configurations.append((slot, yaml.safe_load(result.stdout)))
        cls.shared = yaml.safe_load((ROOT / 'platform/edge/dynamic/middlewares.yml').read_text())['http']['middlewares']

    def test_only_exact_public_post_opens_provider_callback(self):
        for label, config in self.configurations:
            for method, path, allowed in (
                    ('POST', CALLBACK, True), ('GET', CALLBACK, False),
                    ('HEAD', CALLBACK, False), ('OPTIONS', CALLBACK, False),
                    ('PUT', CALLBACK, False), ('POST', CALLBACK + '/', False),
                    ('POST', CALLBACK + '/child', False),
                    ('POST', '/api/integrations/payos/other', False),
                    ('POST', '/api/integrations/other/webhook', False),
                    ('GET', '/api/v1/status', False), ('GET', '/internal/private', False),
                    ('GET', '/admin/connection', False), ('GET', '/health', False),
                    ('GET', '/healthz', False), ('GET', '/ready', False), ('GET', '/readyz', False)):
                with self.subTest(config=label, method=method, path=path):
                    _, router = selected(config, PUBLIC, path, method)
                    self.assertIsNotNone(router)
                    self.assertEqual('deny-internal' not in router['middlewares'], allowed)
                    if allowed:
                        self.assertEqual(router['priority'], 1150)
                        self.assertEqual(router['service'], 'acb-service')
                    else:
                        self.assertEqual(router['priority'], 1000)

    def test_callback_tunnel_security_body_and_rate_contract(self):
        for label, config in self.configurations:
            with self.subTest(config=label):
                _, router = selected(config, PUBLIC, CALLBACK, 'POST')
                policies = dict(self.shared, **config['http'].get('middlewares', {}))
                chain = [policies[name] for name in router['middlewares']]
                self.assertIn({'sourceRange': ['172.31.250.2/32'], 'rejectStatusCode': 403},
                              [item['ipAllowList'] for item in chain if 'ipAllowList' in item])
                self.assertIn({'maxRequestBodyBytes': 65536, 'memRequestBodyBytes': 65536},
                              [item['buffering'] for item in chain if 'buffering' in item])
                self.assertIn({'average': 60, 'period': '1s', 'burst': 120,
                               'sourceCriterion': {'requestHost': True}},
                              [item['rateLimit'] for item in chain if 'rateLimit' in item])
                headers = {}
                for item in chain:
                    self.assertNotIn('redirectRegex', item)
                    self.assertNotIn('redirectScheme', item)
                    headers.update(item.get('headers', {}).get('customResponseHeaders', {}))
                self.assertEqual(headers['X-Content-Type-Options'], 'nosniff')
                self.assertEqual(headers['X-Frame-Options'], 'DENY')
                self.assertEqual(headers['Referrer-Policy'], 'no-referrer')
                self.assertEqual(headers['Cache-Control'], 'no-store')

    def test_capability_headers_and_access_logs_are_private(self):
        for label, config in self.configurations:
            policies = dict(self.shared, **config['http'].get('middlewares', {}))
            for host, prefix in ((PUBLIC, '/api/public/v1/payments/'),
                                 (ADMIN, '/api/v1/payments/'),
                                 (ADMIN, '/api/public/v1/payments/')):
                with self.subTest(config=label, host=host, prefix=prefix):
                    _, router = selected(config, host, prefix + 'opaque-capability', 'GET')
                    self.assertIs(router['observability']['accessLogs'], False)
                    headers = {}
                    for name in router['middlewares']:
                        headers.update(policies[name].get('headers', {}).get('customResponseHeaders', {}))
                    self.assertEqual(headers['Referrer-Policy'], 'no-referrer')
                    self.assertEqual(headers['Cache-Control'], 'no-store')
            _, public_admin = selected(config, PUBLIC, '/api/v1/payments/opaque-capability', 'GET')
            self.assertIn('deny-internal', public_admin['middlewares'])
            self.assertEqual(selected(config, 'other.example', CALLBACK, 'POST'), (None, None))

    def test_sepay_exact_callback_and_isolated_limits(self):
        for label, config in self.configurations:
            for method, path in (('GET', SEPAY_CALLBACK), ('HEAD', SEPAY_CALLBACK),
                                 ('OPTIONS', SEPAY_CALLBACK), ('PUT', SEPAY_CALLBACK),
                                 ('POST', SEPAY_CALLBACK + '/'), ('POST', SEPAY_CALLBACK + '/child'),
                                 ('POST', '/api/integrations/sepay/other'),
                                 ('GET', '/api/v1/sepay-reviews')):
                with self.subTest(config=label, method=method, path=path):
                    _, router = selected(config, PUBLIC, path, method)
                    self.assertIn('deny-internal', router['middlewares'])
            with self.subTest(config=label):
                name, router = selected(config, PUBLIC, SEPAY_CALLBACK, 'POST')
                self.assertEqual(name, 'acb-sepay-telegram-router')
                self.assertEqual(router['priority'], 1150)
                self.assertEqual(router['service'], 'acb-service')
                self.assertIs(router['observability']['accessLogs'], False)
                self.assertEqual(router['middlewares'], [
                    'tunnel-only', 'security-headers', 'payment-privacy',
                    'sepay-telegram-rate-limit', 'sepay-telegram-body-limit'])
                policies = dict(self.shared, **config['http'].get('middlewares', {}))
                self.assertEqual(policies['sepay-telegram-rate-limit'], {'rateLimit': {
                    'average': 60, 'period': '1s', 'burst': 120,
                    'sourceCriterion': {'requestHost': True}}})
                self.assertEqual(policies['sepay-telegram-body-limit'], {'buffering': {
                    'maxRequestBodyBytes': 65536, 'memRequestBodyBytes': 65536}})
                headers = {}
                for policy in (policies[name] for name in router['middlewares']):
                    self.assertNotIn('redirectRegex', policy)
                    self.assertNotIn('redirectScheme', policy)
                    self.assertNotIn('forwardAuth', policy)
                    request_headers = policy.get('headers', {}).get('customRequestHeaders', {})
                    self.assertNotIn('x-telegram-bot-api-secret-token',
                                     {name.lower() for name in request_headers})
                    headers.update(policy.get('headers', {}).get('customResponseHeaders', {}))
                self.assertEqual(headers['Referrer-Policy'], 'no-referrer')
                self.assertEqual(headers['Cache-Control'], 'no-store')
                self.assertEqual(headers['X-Content-Type-Options'], 'nosniff')
                self.assertEqual(headers['X-Frame-Options'], 'DENY')
                expected_slot = 'blue' if label == 'shipped' else label
                upstream = config['http']['services']['acb-service']['loadBalancer']['servers']
                self.assertEqual(upstream, [{'url': f'http://acb-web-{expected_slot}:8090'}])
                self.assertNotEqual(selected(config, ADMIN, SEPAY_CALLBACK, 'POST')[0], name)
                self.assertEqual(selected(config, 'other.example', SEPAY_CALLBACK, 'POST'), (None, None))
                _, payos = selected(config, PUBLIC, CALLBACK, 'POST')
                self.assertIn('payos-webhook-rate-limit', payos['middlewares'])
                self.assertNotIn('sepay-telegram-rate-limit', payos['middlewares'])

    def test_assets_payment_pages_remove_inherited_referrer_policy(self):
        # Cloudflare _headers replaces the inherited value only with its explicit
        # removal directive; appending another value would produce a joined header.
        blocks, current = {}, None
        for line in (ROOT / 'web/public/_headers').read_text().splitlines():
            if not line.strip() or line.startswith('#'):
                continue
            if not line.startswith(' '):
                current = line.strip()
                blocks[current] = []
            else:
                blocks[current].append(line.strip())
        for path in ('/pay', '/pay/opaque-capability'):
            headers = {}
            for pattern, entries in blocks.items():
                if pattern == path or pattern.endswith('*') and path.startswith(pattern[:-1]):
                    for entry in entries:
                        if entry.startswith('! '):
                            headers.pop(entry[2:], None)
                        else:
                            key, value = entry.split(':', 1)
                            headers[key] = headers[key] + ', ' + value.strip() if key in headers else value.strip()
            self.assertEqual(headers['Referrer-Policy'], 'no-referrer', path)
            self.assertEqual(headers['Cache-Control'], 'no-store', path)


if __name__ == '__main__':
    unittest.main()
