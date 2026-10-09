#!/usr/bin/env python3
"""Install a proven source bundle and broker narrowly scoped GitHub calls locally.

The cross-repository credential is used only by local gh processes. The remote
root driver receives an ephemeral Docker credential and a line-oriented broker,
never a GitHub credential or a credential-bearing checkout.
"""

import argparse
import base64
import hashlib
import io
import json
import os
import re
import shlex
import signal
import stat
import subprocess
import sys
import tarfile
import tempfile
import zipfile
from pathlib import Path

SOURCE = 'TheDemonTuan/acb-transaction-webhook'
CENTRAL = 'TheDemonTuan/vps-deploy'
REGISTRY = 'cloudflare/registry/acb.json'
SHA = re.compile(r'[a-f0-9]{40}\Z')
NUMBER = re.compile(r'[1-9][0-9]*\Z')
UUID = re.compile(r'[a-fA-F0-9]{8}(?:-[a-fA-F0-9]{4}){3}-[a-fA-F0-9]{12}\Z')
FILES = frozenset('deploy.sh simple-lib.sh healthcheck.sh render-route.sh backup-db.sh restore-db.sh backup-secrets.sh migrate-payos-runtime.sh migrate-payos-runtime.py workflow-runtime.py acb-route-publish.py install-route-publisher.sh verify-frontend.py compose.prod.yaml bark-entrypoint.sh images.env source-run.env'.split())
LIMIT = 8 * 1024 * 1024


def require(condition, message):
    if not condition:
        raise ValueError(message)


def validate_request(args):
    """Accept GET-only proof/receipt reads, or the one ACB central workflow."""
    require(isinstance(args, list) and all(isinstance(a, str) for a in args), 'Invalid GitHub request')
    if len(args) == 2 and args[0] == 'api':
        endpoint = args[1]
        patterns = [
            rf'repos/{re.escape(SOURCE)}/actions/runs/[1-9][0-9]*(?:/(?:jobs|artifacts)\?per_page=100)?',
            rf'repos/{re.escape(SOURCE)}/actions/artifacts/[1-9][0-9]*/zip',
            rf'repos/{re.escape(CENTRAL)}/contents/{re.escape(REGISTRY)}',
            rf'repos/{re.escape(CENTRAL)}/actions/runs/[1-9][0-9]*(?:/artifacts\?per_page=100)?',
            rf'repos/{re.escape(CENTRAL)}/actions/artifacts/[1-9][0-9]*/zip',
            rf'repos/{re.escape(CENTRAL)}/actions/workflows/cloudflare-deploy\.yml/runs\?status=(?:success|completed)&per_page=100',
            rf'repos/{re.escape(CENTRAL)}/actions/workflows/cloudflare-deploy\.yml/runs\?event=workflow_dispatch&per_page=100&created=>=[0-9]{{4}}-[0-9]{{2}}-[0-9]{{2}}',
        ]
        require(any(re.fullmatch(p, endpoint) for p in patterns), 'GitHub API request out of scope')
        return
    prefix = ['workflow', 'run', 'cloudflare-deploy.yml', '--repo', CENTRAL]
    require(args[:5] == prefix, 'GitHub command out of scope')
    fields = {}
    for index in range(5, len(args), 2):
        require(index + 1 < len(args) and args[index] == '-f', 'Invalid workflow inputs')
        key, separator, value = args[index + 1].partition('=')
        require(separator and key not in fields, 'Duplicate or invalid workflow input')
        fields[key] = value
    require(fields.get('app') == 'acb' and fields.get('mode') in {'publish', 'rollback'}, 'Invalid deployment mode')
    selectors = set(fields) - {'app', 'mode'}
    expected = {'source_run_id'} if fields['mode'] == 'publish' else {'sha', 'version_id'}
    require(selectors == expected, 'Publication requires an exact source run; rollback requires exact SHA and version')
    validators = {'source_run_id': NUMBER, 'sha': SHA, 'version_id': UUID}
    require(all(validators[key].fullmatch(fields[key]) for key in selectors), 'Invalid publication selector')


def github(args):
    validate_request(args)
    result = subprocess.run(['gh', *args], stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                            env={**os.environ, 'GH_TOKEN': os.environ['DEPLOY_GITHUB_TOKEN']})
    require(result.returncode == 0, 'Scoped GitHub request failed')
    return result.stdout


def api(endpoint):
    return json.loads(github(['api', endpoint]))


def source_artifact(sha, run_id):
    run = api(f'repos/{SOURCE}/actions/runs/{run_id}')
    require(run.get('head_sha') == sha and run.get('head_branch') == 'main'
            and run.get('head_repository', {}).get('full_name') == SOURCE
            and run.get('path') == '.github/workflows/deploy.yml'
            and run.get('status') == 'completed' and run.get('conclusion') == 'success'
            and run.get('event') in {'push', 'workflow_dispatch'}, 'Source run is not an exact successful main build')
    jobs = api(f'repos/{SOURCE}/actions/runs/{run_id}/jobs?per_page=100')['jobs']
    deployments = [job for job in jobs if job.get('name') == 'Deploy to VPS']
    require(len(deployments) == 1, 'Source staging contract is missing')
    if deployments[0].get('conclusion') != 'skipped':
        require(run['event'] == 'workflow_dispatch', 'Push must not deploy in its source run')
        return None
    artifacts = api(f'repos/{SOURCE}/actions/runs/{run_id}/artifacts?per_page=100')['artifacts']
    backend = [a for a in artifacts if a.get('name') == 'verified-bundle' and not a.get('expired')]
    frontend = [a for a in artifacts if a.get('name') == 'frontend-dist-' + sha and not a.get('expired')]
    if run['event'] == 'workflow_dispatch' and (not backend or not frontend):
        return None  # Explicit single-component builds remain artifact-only.
    require(len(backend) == len(frontend) == 1, 'Exact backend/frontend artifact pair required')
    require(re.fullmatch(r'sha256:[a-f0-9]{64}', backend[0].get('digest', '')), 'GitHub archive digest required')
    require(type(backend[0].get('id')) is int and backend[0]['id'] > 0, 'Invalid artifact identifier')
    return backend[0]


def unpack_bundle(blob, artifact, destination, sha, run_id):
    require(len(blob) <= LIMIT and 'sha256:' + hashlib.sha256(blob).hexdigest() == artifact['digest'],
            'GitHub artifact archive digest mismatch')
    with zipfile.ZipFile(io.BytesIO(blob)) as archive:
        members = archive.infolist()
        require(len(members) == len(FILES) + 1 and {m.filename for m in members} == FILES | {'SHA256SUMS'},
                'Unexpected bundle archive entries')
        require(sum(m.file_size for m in members) <= LIMIT, 'Expanded bundle exceeds limit')
        for member in members:
            mode = member.external_attr >> 16
            require(not member.is_dir() and not stat.S_ISLNK(mode)
                    and stat.S_IFMT(mode) in {0, stat.S_IFREG}, 'Non-regular archive entry')
            (destination / member.filename).write_bytes(archive.read(member))
    entries = {}
    for line in (destination / 'SHA256SUMS').read_text(encoding='ascii').splitlines():
        match = re.fullmatch(r'([a-f0-9]{64})  ([A-Za-z0-9_.-]+)', line)
        require(match and match[2] in FILES and match[2] not in entries, 'Invalid bundle checksum manifest')
        entries[match[2]] = match[1]
    require(set(entries) == FILES, 'Incomplete bundle checksum manifest')
    for name, digest in entries.items():
        require(hashlib.sha256((destination / name).read_bytes()).hexdigest() == digest, 'Bundle file digest mismatch')
    receipt = dict(line.split('=', 1) for line in (destination / 'source-run.env').read_text(encoding='ascii').splitlines())
    require(receipt.get('RELEASE_SHA') == sha and receipt.get('SOURCE_RUN_ID') == run_id
            and receipt.get('PAYMENT_RUNTIME') == 'payos', 'Bundle source receipt mismatch')
    images = dict(line.split('=', 1) for line in (destination / 'images.env').read_text(encoding='ascii').splitlines())
    require(images.get('RELEASE_SHA') == sha and images.get('PAYMENT_RUNTIME') == 'payos', 'Bundle image release mismatch')


INSTALL = r'''set -euo pipefail
root="$1"; sha="$2"
[[ -d "$root" && ! -L "$root" ]] || exit 1
mkdir -p "$root/releases"
[[ ! -L "$root/releases" ]] || exit 1
stage="$(mktemp -d "$root/releases/.stage-$sha.XXXXXXXX")"
trap 'rm -rf -- "$stage"' EXIT
tar -xf - -C "$stage" --no-same-owner --no-same-permissions
(cd "$stage" && sha256sum -c SHA256SUMS >&2)
chmod 700 "$stage"/*.sh "$stage"/*.py
chmod 644 "$stage"/compose.prod.yaml "$stage"/bark-entrypoint.sh "$stage"/images.env "$stage"/source-run.env "$stage"/SHA256SUMS
target="$root/releases/$sha"
exec 9>"$root/.deploy.lock"
flock -w 60 9
if [[ -e "$target" || -L "$target" ]]; then
  [[ -d "$target" && ! -L "$target" ]] || exit 1
  cmp "$target/SHA256SUMS" "$stage/SHA256SUMS"
  (cd "$target" && sha256sum -c "$stage/SHA256SUMS" >&2)
else
  mv "$stage" "$target"
fi
chmod 700 "$target"/*.sh "$target"/*.py
chmod 644 "$target"/bark-entrypoint.sh
'''


class Remote:
    def __init__(self, environment):
        self.root = environment['DEPLOY_PATH']
        require(re.fullmatch(r'/[A-Za-z0-9._/-]+', self.root) and '..' not in self.root and self.root != '/', 'Invalid deployment root')
        host, user, port = (environment[k] for k in ('VPS_HOST', 'VPS_USER', 'VPS_PORT'))
        require(re.fullmatch(r'[A-Za-z0-9.-]+', host) and re.fullmatch(r'[A-Za-z_][A-Za-z0-9_-]*', user)
                and port.isdigit() and 1 <= int(port) <= 65535, 'Invalid SSH destination')
        self.command = ['ssh', '-i', str(Path.home() / '.ssh/id_ed25519'), '-p', port,
                        '-o', 'StrictHostKeyChecking=yes', '-o', 'BatchMode=yes', '-o', 'ConnectTimeout=15',
                        '-o', 'ServerAliveInterval=15', '-o', 'ServerAliveCountMax=10', f'{user}@{host}']

    def args(self, args):
        return [*self.command, shlex.join(args)]

    def run(self, args, data=None):
        result = subprocess.run(self.args(args), input=data, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                env=self.environment())
        # Do not allow SSH SendEnv/user configuration to transport runner secrets.
        require(result.returncode == 0, 'Remote production operation failed')
        return result.stdout

    @staticmethod
    def environment():
        return {key: value for key, value in os.environ.items()
                if key not in {'DEPLOY_GITHUB_TOKEN', 'REGISTRY_TOKEN', 'GH_TOKEN', 'GITHUB_TOKEN'}}

    def install(self, bundle, sha):
        with tempfile.TemporaryFile() as archive:
            with tarfile.open(fileobj=archive, mode='w') as tar:
                for name in sorted(FILES | {'SHA256SUMS'}):
                    tar.add(bundle / name, arcname=name, recursive=False)
            archive.seek(0)
            self.run(['sudo', '-n', 'bash', '-c', INSTALL, 'install-release', self.root, sha], archive.read())


def sanitized_summary(value, sha, run_id):
    require(isinstance(value, dict) and value.get('phase') == 'FRONTEND_READY'
            and value.get('release_sha') == sha and str(value.get('source_run_id')) == run_id
            and type(value.get('authoritative_backup')) is bool
            and type(value.get('awaiting_owner_configuration')) is bool, 'Invalid remote deployment summary')
    return {key: value[key] for key in ('phase', 'release_sha', 'source_run_id', 'authoritative_backup', 'awaiting_owner_configuration')}


def bridge(process, sha, run_id):
    summary = None
    try:
        for line in process.stdout:
            require(len(line) <= LIMIT * 2, 'Oversized remote bridge message')
            message = json.loads(line)
            if isinstance(message, dict) and set(message) == {'github_request'}:
                require(summary is None, 'GitHub request after final summary')
                try:
                    output = github(message['github_request'])
                    require(len(output) <= LIMIT, 'GitHub response exceeds limit')
                    response = {'github_response': base64.b64encode(output).decode('ascii')}
                except (ValueError, OSError):
                    response = {'github_error': True}
                process.stdin.write(json.dumps(response) + '\n')
                process.stdin.flush()
            else:
                require(summary is None, 'Duplicate remote summary')
                summary = sanitized_summary(message, sha, run_id)
        require(process.wait() == 0 and summary is not None, 'Remote automatic deployment failed')
        return summary
    finally:
        if process.poll() is None:
            process.terminate()
            process.wait()
        process.stdin.close()
        process.stdout.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--sha', required=True)
    parser.add_argument('--source-run-id', required=True)
    args = parser.parse_args()
    require(SHA.fullmatch(args.sha) and NUMBER.fullmatch(args.source_run_id), 'Invalid source identity')
    require(os.environ.get('DEPLOY_GITHUB_TOKEN'), 'Missing local DEPLOY_GITHUB_TOKEN')
    artifact = source_artifact(args.sha, args.source_run_id)
    if artifact is None:
        with open(os.environ['GITHUB_STEP_SUMMARY'], 'a', encoding='utf-8') as report:
            report.write('### Production followup\n\nArtifact-only/manual staging run; no production runtime changes.\n')
        return
    blob = github(['api', f'repos/{SOURCE}/actions/artifacts/{artifact["id"]}/zip'])
    remote = Remote(os.environ)
    with tempfile.TemporaryDirectory(prefix='acb-verified-') as directory:
        bundle = Path(directory)
        unpack_bundle(blob, artifact, bundle, args.sha, args.source_run_id)
        images = dict(line.split('=', 1) for line in (bundle / 'images.env').read_text(encoding='ascii').splitlines())
        keys = {'GATEWAY_IMAGE_REF', 'WORKER_IMAGE_REF', 'DBTOOL_IMAGE_REF', 'TTS_IMAGE_REF', 'BARK_IMAGE_REF'}
        require(set(images) == keys | {'RELEASE_SHA', 'PAYMENT_RUNTIME'}
                and all(re.fullmatch(r'ghcr\.io/[a-z0-9._/-]+@sha256:[a-f0-9]{64}', images[key]) for key in keys),
                'Invalid immutable candidate image references')
        candidate_images = [images[key] for key in sorted(keys)]
        remote.install(bundle, args.sha)
    auth = remote.run(['sudo', '-n', 'mktemp', '-d', '/tmp/acb-registry.XXXXXXXX']).decode('ascii').strip()
    require(re.fullmatch(r'/tmp/acb-registry\.[A-Za-z0-9]{8}', auth), 'Invalid temporary registry directory')
    try:
        actor = os.environ['GITHUB_ACTOR']
        require(re.fullmatch(r'[A-Za-z0-9_-]+(?:\[bot\])?', actor), 'Invalid registry actor')
        remote.run(['sudo', '-n', 'env', 'DOCKER_CONFIG=' + auth, 'docker', 'login', 'ghcr.io', '-u', actor, '--password-stdin'],
                   os.environ['REGISTRY_TOKEN'].encode('utf-8'))
        for image in candidate_images:
            remote.run(['sudo', '-n', 'env', 'DOCKER_CONFIG=' + auth, 'docker', 'pull', '--platform', 'linux/arm64', image])
        driver = f'{remote.root}/releases/{args.sha}/migrate-payos-runtime.py'
        command = ['sudo', '-n', 'env', '-i', 'PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin',
                   'DEPLOY_PATH=' + remote.root, 'DOCKER_CONFIG=' + auth, 'PAYOS_GITHUB_BRIDGE=1',
                   'python3', driver, '--automatic', args.sha, '--source-run-id', args.source_run_id]
        # Remote stderr can contain provider details: keep it off the runner log.
        with tempfile.TemporaryFile() as errors:
            process = subprocess.Popen(remote.args(command), stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                       stderr=errors, text=True, encoding='utf-8', env=remote.environment())
            summary = bridge(process, args.sha, args.source_run_id)
    finally:
        try:
            remote.run(['sudo', '-n', 'env', 'DOCKER_CONFIG=' + auth, 'docker', 'logout', 'ghcr.io'])
        finally:
            remote.run(['sudo', '-n', 'rm', '-rf', '--', auth])
    with open(os.environ['GITHUB_STEP_SUMMARY'], 'a', encoding='utf-8') as report:
        report.write('### Automatic production deployment\n\n```json\n' + json.dumps(summary, sort_keys=True) + '\n```\n')
    print(json.dumps(summary, sort_keys=True))


def interrupted(signum, frame):
    raise InterruptedError('Production runner interrupted')


if __name__ == '__main__':
    try:
        signal.signal(signal.SIGTERM, interrupted)
        main()
    except (ValueError, OSError, KeyError, zipfile.BadZipFile, UnicodeError, json.JSONDecodeError):
        print('AUTOMATIC_PRODUCTION_FAILED: deployment refused; remote evidence preserved', file=sys.stderr)
        sys.exit(1)
