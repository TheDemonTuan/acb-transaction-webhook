#!/usr/bin/env python3
"""Select independent releases against the last successful main push of this workflow."""

import json
import os
import re
import subprocess
import sys
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path


FRONTEND_PATHS = {
    'deploy/cloudflare-routes.py',
    'deploy/verify-frontend.py',
    'deploy/tests/test_cloudflare_routes.py',
    'deploy/tests/test_verify_frontend.py',
}
SHA = re.compile(r'[0-9a-f]{40}\Z')


def classify_paths(paths):
    frontend = backend = False
    for path in paths:
        if path.startswith('.github/workflows/') or path == 'scripts/verify.sh':
            frontend = backend = True
        elif path.startswith('web/') or path in FRONTEND_PATHS:
            frontend = True
        else:
            backend = True
    return frontend, backend


class ActionsAPI:
    def __init__(self, repository, token, base='https://api.github.com'):
        if not re.fullmatch(r'[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+', repository):
            raise ValueError('Invalid GitHub repository')
        if not token:
            raise ValueError('Missing GH_TOKEN')
        self.prefix = base.rstrip('/') + '/repos/' + repository
        self.token = token

    def get(self, path):
        request = urllib.request.Request(self.prefix + path, headers={
            'Authorization': 'Bearer ' + self.token,
            'Accept': 'application/vnd.github+json',
            'X-GitHub-Api-Version': '2022-11-28',
            'User-Agent': 'acb-release-classifier',
        })
        try:
            with urllib.request.urlopen(request, timeout=30) as response:
                return json.load(response)
        except urllib.error.HTTPError as error:
            raise ValueError(f'Actions API HTTP {error.code}; component selection refused') from None
        except (OSError, json.JSONDecodeError):
            raise ValueError('Actions API unavailable or invalid; component selection refused') from None

    def previous_success(self, run_id):
        current = self.get(f'/actions/runs/{run_id}')
        workflow_id = current.get('workflow_id')
        if type(workflow_id) is not int:
            raise ValueError('Actions API did not identify the current workflow')
        page = 1
        while True:
            query = urllib.parse.urlencode({
                'branch': 'main', 'event': 'push', 'status': 'success',
                'per_page': 100, 'page': page,
            })
            result = self.get(f'/actions/workflows/{workflow_id}/runs?{query}')
            runs = result.get('workflow_runs')
            if not isinstance(runs, list):
                raise ValueError('Actions API returned an invalid workflow run list')
            for run in runs:
                if (run.get('id') != run_id and run.get('workflow_id') == workflow_id
                        and run.get('event') == 'push' and run.get('head_branch') == 'main'
                        and run.get('conclusion') == 'success'):
                    sha = run.get('head_sha', '')
                    if not isinstance(sha, str) or not SHA.fullmatch(sha):
                        raise ValueError('Successful workflow run has invalid head SHA')
                    return sha
            if len(runs) < 100:
                return None
            page += 1


def automatic_selection(sha, api, run_id):
    base = api.previous_success(run_id)
    if base is None:
        return True, True, 'No successful main push of this workflow; both components selected', None
    ancestor = subprocess.run(['git', 'merge-base', '--is-ancestor', base, sha],
                              stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    if ancestor.returncode != 0:
        return True, True, 'Previous successful SHA is unavailable or not an ancestor; both components selected', base
    diff = subprocess.run(['git', 'diff', '--name-only', '--no-renames', '-z', base, sha, '--'],
                          check=True, stdout=subprocess.PIPE)
    paths = [path.decode('utf-8', errors='surrogateescape') for path in diff.stdout.split(b'\0') if path]
    frontend, backend = classify_paths(paths)
    return frontend, backend, f'Classified {len(paths)} changed paths since successful main push', base


def select(event_name, inputs, auto):
    target = inputs.get('target', 'all') if event_name == 'workflow_dispatch' else 'auto'
    if target not in {'auto', 'all', 'frontend', 'backend'}:
        raise ValueError('Invalid component target')
    deploy = inputs.get('deploy', False)
    rehearse = inputs.get('rehearse', False)
    for name, value in [('deploy', deploy), ('rehearse', rehearse)]:
        if type(value) is not bool:
            raise ValueError(f'{name} must be a boolean')
    account = inputs.get('import_account', '')
    if not isinstance(account, str):
        raise ValueError('Invalid dispatch string input')
    if account and not re.fullmatch(r'[0-9]{1,32}', account):
        raise ValueError('INVALID_IMPORT_ACCOUNT')
    if target == 'auto':
        frontend, backend, reason, base = auto()
    else:
        frontend, backend = target in {'all', 'frontend'}, target in {'all', 'backend'}
        reason, base = f'Explicit dispatch target: {target}', None
    if account or rehearse:
        backend = True
        reason += '; backend forced by import_account or rehearse'
    return {'frontend': str(frontend).lower(), 'backend': str(backend).lower(),
            'reason': reason, 'base_sha': base}


def main():
    event_name = os.environ['GITHUB_EVENT_NAME']
    if event_name not in {'push', 'workflow_dispatch'}:
        raise ValueError('Unsupported workflow event')
    sha = os.environ['GITHUB_SHA']
    if not SHA.fullmatch(sha):
        raise ValueError('Invalid checked-out release SHA')
    event = json.loads(Path(os.environ['GITHUB_EVENT_PATH']).read_text(encoding='utf-8'))
    inputs = event.get('inputs') or {}
    # workflow_dispatch webhook inputs are strings, unlike the Actions inputs context.
    inputs = dict(inputs)
    for name in ('deploy', 'rehearse'):
        value = inputs.get(name, False)
        if isinstance(value, str):
            if value not in {'true', 'false'}:
                raise ValueError(f'Invalid boolean input: {name}')
            value = value == 'true'
        inputs[name] = value
    account = inputs.get('import_account', '')
    if account:
        if not isinstance(account, str) or not re.fullmatch(r'[0-9]{1,32}', account):
            raise ValueError('INVALID_IMPORT_ACCOUNT')
        print('::add-mask::' + account)

    def auto():
        api = ActionsAPI(os.environ['GITHUB_REPOSITORY'], os.environ.get('GH_TOKEN', ''),
                         os.environ.get('GITHUB_API_URL', 'https://api.github.com'))
        return automatic_selection(sha, api, int(os.environ['GITHUB_RUN_ID']))

    result = select(event_name, inputs, auto)
    with open(os.environ['GITHUB_OUTPUT'], 'a', encoding='utf-8') as output:
        for name in ('frontend', 'backend'):
            output.write(f'{name}={result[name]}\n')
    with open(os.environ['GITHUB_STEP_SUMMARY'], 'a', encoding='utf-8') as summary:
        summary.write('### Release component selection\n')
        summary.write(f"Frontend artifact: {result['frontend']}; backend: {result['backend']}\n\n")
        summary.write(result['reason'] + '\n')
        if result['base_sha']:
            summary.write(f"Successful main push base: `{result['base_sha']}`\n")


if __name__ == '__main__':
    try:
        main()
    except (ValueError, OSError, KeyError, subprocess.CalledProcessError) as error:
        print(f'CHANGE_SELECTION_FAILED: {error}', file=sys.stderr)
        sys.exit(1)
