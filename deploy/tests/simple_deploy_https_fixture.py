#!/usr/bin/env python3
"""Isolated HTTPS ingress: Access redirect and actual Traefik public API responses."""
import http.server
import ssl
import subprocess
import sys


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.headers.get('Host') == 'bank.tuannguyenviet.site':
            self.send_response(302)
            self.send_header('Location', 'https://thedemontuan.cloudflareaccess.com/cdn-cgi/access/login/bank.tuannguyenviet.site')
            self.end_headers()
            return
        if self.headers.get('Host') != 'transactions.tuannguyenviet.site':
            self.send_error(404)
            return
        try:
            response = subprocess.run([
                'docker', 'run', '--rm', '--network', 'container:edge-traefik',
                'curlimages/curl:8.12.1', '--silent', '--show-error', '--include',
                '--max-time', '5', '-H', 'Host: transactions.tuannguyenviet.site',
                '-H', 'Accept-Encoding: identity',
                'http://127.0.0.1:8080' + self.path,
            ], stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=15)
            if response.returncode:
                raise ValueError('origin request failed')
            head, body = response.stdout.split(b'\r\n\r\n', 1)
            lines = head.decode('iso-8859-1').split('\r\n')
            status = int(lines[0].split(' ', 2)[1])
            headers = [line.split(':', 1) for line in lines[1:] if ':' in line]
        except (subprocess.TimeoutExpired, ValueError, IndexError):
            self.send_error(502)
            return
        self.send_response(status)
        for name, value in headers:
            if name.lower() not in {'connection', 'transfer-encoding', 'content-length', 'server', 'date'}:
                self.send_header(name, value.strip())
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *_args):
        pass


server = http.server.ThreadingHTTPServer(('127.0.0.1', int(sys.argv[3])), Handler)
context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
context.minimum_version = ssl.TLSVersion.TLSv1_2
context.load_cert_chain(sys.argv[1], sys.argv[2])
server.socket = context.wrap_socket(server.socket, server_side=True)
server.serve_forever()
