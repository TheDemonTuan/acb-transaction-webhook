#!/usr/bin/env python3
"""Isolated HTTPS ingress: Access redirect + actual internal Traefik frontend response."""
import http.server
import os
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
        response = subprocess.run([
            'docker', 'run', '--rm', '--network', 'container:edge-traefik',
            'curlimages/curl:8.12.1', '--silent', '--show-error', '--fail',
            '--max-time', '5', '-H', 'Host: frontend-deploy.acb.internal.invalid',
            '-H', 'Accept-Encoding: identity',
            'http://127.0.0.1:18080' + self.path,
        ], stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=15)
        if response.returncode:
            self.send_error(502)
            return
        self.send_response(200)
        self.send_header('Content-Type', 'text/plain' if self.path == '/__release' else 'text/html')
        self.send_header('Content-Length', str(len(response.stdout)))
        self.end_headers()
        self.wfile.write(response.stdout)

    def log_message(self, *_args):
        pass


server = http.server.ThreadingHTTPServer(('127.0.0.1', int(sys.argv[3])), Handler)
context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
context.load_cert_chain(sys.argv[1], sys.argv[2])
server.socket = context.wrap_socket(server.socket, server_side=True)
server.serve_forever()
