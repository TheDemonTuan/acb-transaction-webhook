#!/usr/bin/env python3
"""Import only the actual live Compose service definitions for first cutover rollback."""
import json
import os
import pathlib
import re
import subprocess
import sys

import yaml

root = pathlib.Path(sys.argv[1]).resolve()
sha = sys.argv[2]
state = json.loads((root / 'state/current-release.json').read_text())
assert state['status'] == 'COMPLETED' and state['git_sha'] == sha
slots = state['active_slots']
assert slots['gateway'] in ('blue', 'green') and slots['frontend'] in ('blue', 'green')
release = root / 'releases' / sha
assert not release.exists(), 'baseline bundle already exists'

def command(*args):
    return subprocess.check_output(args, text=True)

def live(name):
    obj = json.loads(command('docker', 'inspect', name))[0]
    assert obj['State']['Running'] and obj['State']['Health']['Status'] == 'healthy', name
    assert obj['Config']['Labels'].get('com.docker.compose.project') == 'acb', name
    return obj

names = ['gateway-' + slots['gateway'], 'frontend-' + slots['frontend'], 'worker', 'auth-browser', 'tts-gateway', 'bark']
refs = {}
services = {}
networks = {}
volumes = {}
secrets = {}
for name in names:
    obj = live('acb-' + name)
    labels = obj['Config']['Labels']
    svc = labels['com.docker.compose.service']
    assert svc == name, (svc, name)
    paths = [pathlib.Path(p).resolve() for p in labels['com.docker.compose.project.config_files'].split(',')]
    assert paths and all(p.is_file() and p.is_relative_to(root) for p in paths), name
    base = pathlib.Path(labels['com.docker.compose.project.working_dir']).resolve()
    assert base.is_relative_to(root), base
    assert (base / '.release.env').is_file(), 'missing legacy release env'
    cmd = ['docker', 'compose', '--project-name', 'acb', '--project-directory', str(base), '--env-file', str(root / 'deploy/.env.production'), '--env-file', str(base / '.release.env')]
    # Compose labels capture exact source files, including split service files.
    for path in paths:
        cmd += ['-f', str(path)]
    cfg = json.loads(command(*cmd, 'config', '--no-env-resolution', '--format', 'json'))
    definition = cfg['services'][svc]
    ref = obj['Config']['Image']
    assert re.fullmatch(r'[a-z0-9][a-z0-9./_-]*@sha256:[a-f0-9]{64}', ref), name
    assert json.loads(command('docker', 'image', 'inspect', ref))[0]['Id'] == obj['Image'], name
    definition['image'] = ref
    for entry in definition.get('env_file', []):
        candidate = pathlib.Path(entry if isinstance(entry, str) else entry['path']).resolve()
        assert candidate == (root / 'deploy/.env.production').resolve(), (name, 'unexpected env_file')
    # Do not permit a relative asset to silently resolve under the new bundle.
    for mount in definition.get('volumes', []):
        if mount['type'] == 'bind':
            source = pathlib.Path(mount['source']).resolve()
            assert source.is_relative_to(root) and source.exists(), source
            mount['source'] = str(source)
    for index, opt in enumerate(definition.get('security_opt', [])):
        if opt.startswith('seccomp:'):
            path = pathlib.Path(opt[8:])
            path = (path if path.is_absolute() else base / path).resolve()
            assert path.is_relative_to(root) and path.is_file(), path
            definition['security_opt'][index] = 'seccomp:' + str(path)
    services[svc] = definition
    refs[svc] = ref
    for key, dest in (('networks', networks), ('volumes', volumes), ('secrets', secrets)):
        for item, value in cfg.get(key, {}).items():
            if key == 'secrets':
                path = pathlib.Path(value['file']).resolve()
                canonical = root / 'deploy/secrets' / path.name
                assert canonical.is_file() and path.samefile(canonical), 'unexpected secret path'
                value['file'] = str(canonical)
            if item in dest:
                assert dest[item] == value, (key, item)
            else:
                dest[item] = value

# Keep exactly the running service configuration, never substitute a different release's template.
identity = obj = live('acb-gateway-' + slots['gateway'])
commit = [v.split('=', 1)[1] for v in identity['Config']['Env'] if v.startswith('RELEASE_COMMIT=')]
assert commit == [sha], 'gateway identity disagrees with legacy state'
assert volumes['gateway_data']['name'] == 'bank-event-gateway_gateway_data'
assert volumes['bark_data']['name'] == 'bank-event-gateway_bark_data'
for network in ('edge-acb','acb-core','acb-egress'):
    assert networks[network]['name'] == network, network
release.mkdir(mode=0o700)
composition = {'name': 'acb', 'services': services, 'volumes': volumes, 'networks': networks, 'secrets': secrets}
compose = release / 'compose.prod.yaml'
compose.write_text(yaml.safe_dump(composition, sort_keys=False))
compose.chmod(0o600)
# Persist actual references (auxiliary services may have been recovered from older releases).
values = {
    'IMAGE_REF_BLUE': refs.get('gateway-blue', state['images']['gateway']['blue']),
    'IMAGE_REF_GREEN': refs.get('gateway-green', state['images']['gateway']['green']),
    'FRONTEND_IMAGE_REF_BLUE': refs.get('frontend-blue', state['images']['frontend']),
    'FRONTEND_IMAGE_REF_GREEN': refs.get('frontend-green', state['images']['frontend']),
    'WORKER_IMAGE_REF': refs['worker'], 'BROWSER_IMAGE_REF': refs['auth-browser'],
    'TTS_IMAGE_REF': refs['tts-gateway'], 'BARK_IMAGE_REF': refs['bark'],
    'DBTOOL_IMAGE_REF': state['images']['dbtool'],
    'RELEASE_COMMIT_BLUE': sha, 'RELEASE_COMMIT_GREEN': sha,
    'WORKER_RELEASE_COMMIT': sha,
    'ENV_FILE': str(root / 'deploy/.env.production'),
    'SECRETS_DIR': str(root / 'deploy/secrets'), 'BARK_SECRET_GROUP': '1000',
}
with (release / 'runtime.env').open('w') as stream:
    for key, value in values.items():
        assert '\n' not in value and '=' not in key
        stream.write(f'{key}={value}\n')
(release / 'runtime.env').chmod(0o600)
image_names = ('GATEWAY_IMAGE_REF', 'FRONTEND_IMAGE_REF', 'WORKER_IMAGE_REF', 'DBTOOL_IMAGE_REF', 'BROWSER_IMAGE_REF', 'TTS_IMAGE_REF', 'BARK_IMAGE_REF')
image_refs = (refs['gateway-' + slots['gateway']], refs['frontend-' + slots['frontend']], refs['worker'], values['DBTOOL_IMAGE_REF'], refs['auth-browser'], refs['tts-gateway'], refs['bark'])
(release / 'images.env').write_text('RELEASE_SHA=' + sha + '\n' + ''.join(f'{k}={v}\n' for k,v in zip(image_names,image_refs)))
(release / 'images.env').chmod(0o600)
print(str(release))
