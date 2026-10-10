#!/usr/bin/env python3
"""Real-Traefik boundary drill, called only inside the disposable deployment fixture.

Uses invalid JSON, never fake payment credentials or provider network requests.
The caller supplies an isolated edge-traefik with access logs enabled and only
loopback allowed as its simulated tunnel. No production host is contacted.
"""
import json
import subprocess
import time
import uuid

PUBLIC = 'transactions.tuannguyenviet.site'
ADMIN = 'bank.tuannguyenviet.site'
CALLBACK = '/api/integrations/payos/webhook'
SEPAY_CALLBACK = '/api/integrations/sepay/telegram'
IMAGE = 'curlimages/curl:8.12.1'


def request(method, path, body=None, host=PUBLIC, tunnel=True):
    network = 'container:edge-traefik' if tunnel else 'edge-acb'
    address = '127.0.0.1:8080' if tunnel else 'edge-traefik:8080'
    command = ['docker', 'run', '--rm', '-i', '--network', network, IMAGE,
               '--silent', '--show-error', '--max-time', '15', '--include',
               '--request', method, '-H', 'Host: ' + host,
               '-H', 'Content-Type: application/json',
               '--write-out', '\n__STATUS__:%{http_code}', 'http://' + address + path]
    if method == 'HEAD':
        command += ['--head']
    if body is not None:
        command += ['--data-binary', '@-']
    result = subprocess.run(command, input=body, capture_output=True, check=True, timeout=40)
    payload, status = result.stdout.rsplit(b'\n__STATUS__:', 1)
    header_block, response_body = payload.split(b'\r\n\r\n', 1)
    headers = {}
    for line in header_block.split(b'\r\n')[1:]:
        key, value = line.decode().split(':', 1)
        headers.setdefault(key.lower(), []).append(value.strip())
    return int(status), headers, response_body


def main():
    status, headers, body = request('POST', CALLBACK, b'[')
    assert status == 400, ('exact callback must reach JSON validation', status)
    assert isinstance(json.loads(body), dict), body
    assert 'location' not in headers, headers
    assert headers['referrer-policy'] == ['no-referrer'], headers
    assert headers['cache-control'] == ['no-store'], headers

    for method, path in (('GET', CALLBACK), ('HEAD', CALLBACK), ('OPTIONS', CALLBACK),
                         ('PUT', CALLBACK), ('POST', CALLBACK + '/'),
                         ('POST', CALLBACK + '/child'),
                         ('POST', '/api/integrations/payos/other'),
                         ('GET', '/api/v1/status'), ('GET', '/internal/private'),
                         ('GET', '/admin/connection'), ('GET', '/health'),
                         ('GET', '/healthz'), ('GET', '/ready'), ('GET', '/readyz')):
        status, headers, _ = request(method, path)
        assert status == 403, ('private/non-exact route admitted', method, path, status)
        assert 'location' not in headers, (method, path, headers)

    status, _, _ = request('POST', CALLBACK, b'[' + b' ' * 65535)
    assert status == 400, ('64KiB provider body incorrectly rejected by edge', status)
    status, _, _ = request('POST', CALLBACK, b'[' + b' ' * 65536)
    assert status == 413, ('oversized provider body reached backend', status)
    status, _, _ = request('POST', CALLBACK, b'[', tunnel=False)
    assert status == 403, ('provider callback accepted outside tunnel', status)

    status, headers, body = request('POST', SEPAY_CALLBACK, b'{}')
    assert status == 503, ('disabled SePay callback must reach gateway fail-closed response', status)
    assert isinstance(json.loads(body), dict), body
    assert 'location' not in headers, headers
    assert headers['referrer-policy'] == ['no-referrer'], headers
    assert headers['cache-control'] == ['no-store'], headers
    for method, path in (('GET', SEPAY_CALLBACK), ('HEAD', SEPAY_CALLBACK),
                         ('OPTIONS', SEPAY_CALLBACK), ('PUT', SEPAY_CALLBACK),
                         ('POST', SEPAY_CALLBACK + '/'), ('POST', SEPAY_CALLBACK + '/child'),
                         ('POST', '/api/integrations/sepay/other'), ('GET', '/api/v1/sepay-reviews')):
        status, headers, _ = request(method, path)
        assert status == 403, ('non-exact SePay route admitted', method, path, status)
        assert 'location' not in headers, headers
    status, _, _ = request('POST', SEPAY_CALLBACK, b'[' + b' ' * 65535)
    assert status == 503, ('64KiB SePay body incorrectly rejected by edge', status)
    status, _, _ = request('POST', SEPAY_CALLBACK, b'[' + b' ' * 65536)
    assert status == 413, ('oversized SePay body reached backend', status)
    status, _, _ = request('POST', SEPAY_CALLBACK, b'{}', tunnel=False)
    assert status == 403, ('SePay callback accepted outside tunnel', status)

    capability = 'ingress-capability-' + uuid.uuid4().hex
    for host, prefix in ((PUBLIC, '/api/public/v1/payments/'),
                         (ADMIN, '/api/public/v1/payments/'),
                         (ADMIN, '/api/v1/payments/')):
        status, headers, _ = request('GET', prefix + capability, host=host)
        assert 400 <= status < 500, ('unknown capability unexpected response', host, status)
        assert headers['referrer-policy'] == ['no-referrer'], (host, headers)
        assert headers['cache-control'] == ['no-store'], (host, headers)

    control = 'ingress-log-control-' + uuid.uuid4().hex
    status, _, _ = request('GET', '/api/v1/' + control)
    assert status == 403, status
    deadline = time.monotonic() + 10
    while True:
        logs = subprocess.run(['docker', 'logs', 'edge-traefik'],
                              capture_output=True, check=True, timeout=15)
        text = (logs.stdout + logs.stderr).decode()
        if control in text:
            break
        assert time.monotonic() < deadline, 'Traefik access-log control request missing'
        time.sleep(0.1)
    assert capability not in text, 'Payment capability leaked into Traefik logs'
    assert 'acb-sepay-telegram-router' not in text, 'SePay callback leaked into Traefik access logs'

    # One parallel curl process avoids container startup skew that would refill
    # the token bucket between requests. All bodies fail locally before provider IO.
    transfer = ('url = "http://127.0.0.1:8080' + CALLBACK + '"\n'
                'header = "Host: ' + PUBLIC + '"\n'
                'header = "Content-Type: application/json"\n'
                'data = "["\noutput = "/dev/null"\n'
                'silent\nshow-error\nmax-time = 15\nwrite-out = "%{http_code}\\n"\n')
    config = '\nnext\n'.join([transfer] * 400).encode()
    burst = subprocess.run(['docker', 'run', '--rm', '-i', '--network', 'container:edge-traefik',
                            IMAGE, '--parallel', '--parallel-max', '200', '--config', '-'],
                           input=config, capture_output=True, check=True, timeout=40)
    codes = burst.stdout.decode().splitlines()
    assert len(codes) == 400, ('missing burst responses', len(codes))
    assert '429' in codes, 'Provider burst never triggered its dedicated rate limit'
    assert set(codes) <= {'400', '429'}, ('unexpected provider burst status', set(codes))
    sepay_config = config.replace(CALLBACK.encode(), SEPAY_CALLBACK.encode())
    burst = subprocess.run(['docker', 'run', '--rm', '-i', '--network', 'container:edge-traefik',
                            IMAGE, '--parallel', '--parallel-max', '200', '--config', '-'],
                           input=sepay_config, capture_output=True, check=True, timeout=40)
    codes = burst.stdout.decode().splitlines()
    assert len(codes) == 400 and '429' in codes, ('SePay dedicated rate limit missing', set(codes))
    assert set(codes) <= {'503', '429'}, ('unexpected disabled SePay burst status', set(codes))
    status, _, _ = request('POST', CALLBACK, b'[')
    assert status == 400, ('SePay rate limit consumed payOS quota', status)
    print('Payment/SePay ingress: exact POST, deny boundaries, tunnel, 64KiB, privacy/logs and isolated burst verified')


if __name__ == '__main__':
    main()
