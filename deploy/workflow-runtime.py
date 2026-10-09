#!/usr/bin/env python3
"""Read the remote payment runtime before any workflow staging or deployment."""

import re
import sys
from pathlib import Path


def deployment_mode(root):
    markers = []
    for name in ('state.env', 'runtime.env'):
        path = Path(root) / name
        if not path.exists():
            markers.append(None)
            continue
        if path.is_symlink() or not path.is_file():
            raise ValueError('PAYMENT_RUNTIME_STATE_INVALID')
        lines = path.read_text(encoding='utf-8').splitlines()
        values = [line.split('=', 1)[1] for line in lines if line.startswith('PAYMENT_RUNTIME=')]
        if len(values) > 1 or (values and values[0] != 'payos'):
            raise ValueError('PAYMENT_RUNTIME_STATE_INVALID')
        markers.append(values[0] if values else None)
    if any(markers) and markers != ['payos', 'payos']:
        raise ValueError('PAYMENT_RUNTIME_STATE_INCOMPLETE')
    return 'deploy' if markers == ['payos', 'payos'] else 'stage'


if __name__ == '__main__':
    try:
        if len(sys.argv) != 2 or not re.fullmatch(r'/[A-Za-z0-9._/-]+', sys.argv[1]) or '..' in sys.argv[1]:
            raise ValueError('DEPLOY_PATH_INVALID')
        print(deployment_mode(sys.argv[1]))
    except (ValueError, OSError, UnicodeError) as error:
        print(str(error) if isinstance(error, ValueError) else 'PAYMENT_RUNTIME_READ_FAILED', file=sys.stderr)
        sys.exit(1)
