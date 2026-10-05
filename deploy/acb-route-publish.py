#!/usr/bin/python3
"""Root-owned fixed-target atomic publisher for the ACB Traefik route.

Install outside release directories at /usr/local/libexec/acb-route-publish.
Trusted canonical YAML templates live at /etc/acb-route-publisher/templates.
No CLI paths or environment overrides are accepted by the privileged entrypoint.
"""
import base64
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import sys
import tempfile
import yaml

DESTINATION = Path('/opt/platform/edge/dynamic/acb.yml')
TEMPLATES = Path('/etc/acb-route-publisher/templates')
LOCK = Path('/run/lock/acb-route-publisher.lock')
LIMIT = 128 * 1024


class PublishError(RuntimeError):
    pass


class UniqueLoader(yaml.SafeLoader):
    pass


def unique_mapping(loader, node):
    result = {}
    for key_node, value_node in node.value:
        key = loader.construct_object(key_node)
        if key in result:
            raise PublishError('Duplicate YAML key')
        result[key] = loader.construct_object(value_node)
    return result


UniqueLoader.add_constructor(yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, unique_mapping)


def trusted(path, directory=False):
    info = path.lstat()
    if info.st_uid != 0 or info.st_gid != 0 or stat.S_IMODE(info.st_mode) & 0o022:
        raise PublishError('Untrusted publisher policy or destination permissions')
    if not (stat.S_ISDIR(info.st_mode) if directory else stat.S_ISREG(info.st_mode)):
        raise PublishError('Publisher policy/destination must not be a symlink')


def scope(config):
    routers = {'acb-deny-internal', 'acb-public-deny-private', 'acb-public-sse-router',
               'acb-public-api-router', 'acb-api-router', 'acb-deploy-gateway'}
    if not isinstance(config, dict) or set(config) != {'http'}:
        raise PublishError('Only ACB HTTP topology may be published')
    http = config['http']
    if not isinstance(http, dict) or set(http) - {'routers', 'services', 'middlewares'}:
        raise PublishError('Unsupported route configuration objects')
    if not {'routers', 'services'} <= http.keys():
        raise PublishError('Missing ACB topology')
    if set(http['routers']) - routers or set(http['services']) - {'acb-service'}:
        raise PublishError('Route contains objects outside ACB ownership')
    if http.get('middlewares'):
        raise PublishError('Route contains foreign middleware definitions')
    for service in http['services'].values():
        servers = service.get('loadBalancer', {}).get('servers')
        if not isinstance(servers, list) or len(servers) != 1 or set(servers[0]) != {'url'}:
            raise PublishError('Unsupported ACB upstream topology')
        if not re.fullmatch(r'http://acb-web-(blue|green):8090', servers[0]['url']):
            raise PublishError('Upstream is outside ACB ownership')


def validate(route, templates):
    try:
        candidate = yaml.load(route, Loader=UniqueLoader)  # nosec B506: UniqueLoader subclasses yaml.SafeLoader
        scope(candidate)
        trusted(templates, directory=True)
        matches = False
        count = 0
        for template in templates.iterdir():
            trusted(template)
            if template.suffix not in ('.yml', '.yaml'):
                raise PublishError('Unexpected publisher policy file')
            data = template.read_bytes()
            if len(data) > LIMIT:
                raise PublishError('Publisher template too large')
            authorized = yaml.load(data, Loader=UniqueLoader)  # nosec B506: UniqueLoader subclasses yaml.SafeLoader
            scope(authorized)
            matches = matches or candidate == authorized
            count += 1
        if not count or not matches:
            raise PublishError('Route does not match an authorized canonical ACB topology')
    except (yaml.YAMLError, UnicodeError, TypeError, ValueError):
        raise PublishError('Invalid route YAML') from None


def publish(envelope, destination=DESTINATION, templates=TEMPLATES, lock=LOCK):
    if set(envelope) != {'expected_sha256', 'route_base64'}:
        raise PublishError('Invalid route publication envelope')
    expected = envelope['expected_sha256']
    if not isinstance(expected, str) or not re.fullmatch('[0-9a-f]{64}', expected):
        raise PublishError('Invalid expected route identity')
    try:
        route = base64.b64decode(envelope['route_base64'], validate=True)
    except (ValueError, TypeError):
        raise PublishError('Invalid encoded route') from None
    if not route or len(route) > LIMIT:
        raise PublishError('Route size limit exceeded')
    # Every parent is trusted; a deploy user must not replace the policy directory
    # or the fixed destination through a writable/symlink ancestor.
    for path in (destination.parent, *destination.parent.parents):
        trusted(path, directory=True)
    for path in (templates.parent, *templates.parent.parents):
        trusted(path, directory=True)
    validate(route, templates)
    descriptor = os.open(lock, os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
    try:
        info = os.fstat(descriptor)
        if info.st_uid != 0 or not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) & 0o077:
            raise PublishError('Untrusted publisher lock')
        fcntl.flock(descriptor, fcntl.LOCK_EX)
        trusted(destination)
        current = destination.read_bytes()
        if hashlib.sha256(current).hexdigest() != expected:
            raise PublishError('Route drift: compare-before-write refused')
        if current != route:
            fd, temporary = tempfile.mkstemp(prefix='.acb-publish-', suffix='.tmp', dir=destination.parent)
            try:
                with os.fdopen(fd, 'wb') as stream:
                    os.fchmod(stream.fileno(), 0o644)
                    os.fchown(stream.fileno(), 0, 0)
                    stream.write(route)
                    stream.flush()
                    os.fsync(stream.fileno())
                os.replace(temporary, destination)
                directory = os.open(destination.parent, os.O_DIRECTORY)
                try:
                    os.fsync(directory)
                finally:
                    os.close(directory)
            finally:
                if os.path.exists(temporary):
                    os.unlink(temporary)
        return hashlib.sha256(route).hexdigest()
    finally:
        os.close(descriptor)


def main():
    try:
        if len(sys.argv) != 1 or os.geteuid() != 0:
            raise PublishError('Publisher requires root and accepts no arguments')
        raw = sys.stdin.buffer.read(LIMIT * 2 + 1)
        if len(raw) > LIMIT * 2:
            raise PublishError('Publication envelope too large')
        envelope = json.loads(raw)
        if not isinstance(envelope, dict):
            raise PublishError('Publication envelope must be an object')
        print(publish(envelope))
        return 0
    except (PublishError, OSError, ValueError, KeyError, TypeError) as error:
        print('ACB route publication refused: ' + str(error), file=sys.stderr)
        return 1


if __name__ == '__main__':
    sys.exit(main())
