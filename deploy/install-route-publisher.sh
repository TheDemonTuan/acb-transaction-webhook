#!/usr/bin/env bash
# Root installation; verified automatic releases upgrade only this app's fixed policy destinations.
set -euo pipefail
upgrade=false
if [[ ${1:-} == --upgrade ]]; then upgrade=true; shift; fi
[[ $EUID == 0 && $# == 1 && "$1" == /* && -d "$1" ]] || { echo 'usage (root): install-route-publisher.sh [--upgrade] /absolute/canonical-template-directory' >&2; exit 2; }
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
python3 - "$HERE/acb-route-publish.py" "$1" <<'PY'
import importlib.util,os,pathlib,stat,sys
source,templates=map(pathlib.Path,sys.argv[1:])
spec=importlib.util.spec_from_file_location('publisher',source)
module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module)
files=list(templates.iterdir())
if not files: raise SystemExit('Canonical route templates are required')
for path in files:
    if not path.is_file() or path.is_symlink() or path.suffix not in ('.yml','.yaml'):
        raise SystemExit('Only regular YAML templates are allowed')
    data=path.read_bytes()
    if len(data)>module.LIMIT: raise SystemExit('Template exceeds size limit')
    module.scope(module.yaml.load(data,Loader=module.UniqueLoader))
for path in (pathlib.Path('/usr/local/libexec'),pathlib.Path('/etc/acb-route-publisher')):
    if path.exists() and (path.is_symlink() or path.stat().st_uid!=0 or stat.S_IMODE(path.stat().st_mode)&0o022):
        raise SystemExit('Existing helper/policy directory is not root-controlled')
PY
if [[ "$upgrade" == true ]]; then
  # Reviewed release upgrade runs under the deployment lock after source proof.
  # Content-addressed additions preserve all authorized rollback policies.
  python3 - "$HERE/acb-route-publish.py" "$1" <<'PY'
import fcntl,hashlib,importlib.util,os,pathlib,stat,sys,tempfile
source,incoming=map(pathlib.Path,sys.argv[1:])
spec=importlib.util.spec_from_file_location('publisher',source)
module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module)
helper=pathlib.Path('/usr/local/libexec/acb-route-publish')
templates=pathlib.Path('/etc/acb-route-publisher/templates')
route=pathlib.Path('/opt/platform/edge/dynamic/acb.yml')
lock=pathlib.Path('/run/lock/acb-route-publisher.lock')
sudoers=pathlib.Path('/etc/sudoers.d/acb-route-publisher')
for path in (helper,route,sudoers):
    module.trusted(path)
    for parent in path.parents: module.trusted(parent,directory=True)
module.trusted(templates,directory=True)
for parent in templates.parents: module.trusted(parent,directory=True)
descriptor=os.open(lock,os.O_RDWR|os.O_CREAT|os.O_NOFOLLOW,0o600)
try:
    info=os.fstat(descriptor)
    if info.st_uid!=0 or info.st_gid!=0 or not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode)&0o077:
        raise SystemExit('Untrusted publisher lock')
    fcntl.flock(descriptor,fcntl.LOCK_EX)
    existing=list(templates.iterdir())
    if not existing: raise SystemExit('Upgrade requires installed canonical policies')
    for path in existing:
        module.trusted(path)
        if path.suffix not in ('.yml','.yaml') or path.stat().st_size>module.LIMIT:
            raise SystemExit('Invalid installed canonical policy')
        module.scope(module.yaml.load(path.read_bytes(),Loader=module.UniqueLoader))
    additions={}
    for path in incoming.iterdir():
        if path.is_symlink() or not path.is_file() or path.suffix not in ('.yml','.yaml'):
            raise SystemExit('Only regular YAML templates are allowed')
        data=path.read_bytes()
        if len(data)>module.LIMIT: raise SystemExit('Template exceeds size limit')
        module.scope(module.yaml.load(data,Loader=module.UniqueLoader))
        additions['payos-'+hashlib.sha256(data).hexdigest()+'.yml']=data
    if not additions: raise SystemExit('Canonical route templates are required')
    legacy=route.read_bytes()
    if len(legacy)>module.LIMIT: raise SystemExit('Current route exceeds size limit')
    module.scope(module.yaml.load(legacy,Loader=module.UniqueLoader))
    additions['legacy-'+hashlib.sha256(legacy).hexdigest()+'.yml']=legacy
    for name,data in additions.items():
        destination=templates/name
        if destination.exists() or destination.is_symlink():
            module.trusted(destination)
            if destination.read_bytes()!=data: raise SystemExit('Canonical policy identity collision')
    helper_data=source.read_bytes()
    def replace(destination,data,mode):
        fd,temporary=tempfile.mkstemp(prefix='.publisher-upgrade-',dir=destination.parent)
        try:
            with os.fdopen(fd,'wb') as stream:
                os.fchmod(fd,mode);os.fchown(fd,0,0)
                stream.write(data);stream.flush();os.fsync(fd)
            os.replace(temporary,destination)
            directory=os.open(destination.parent,os.O_DIRECTORY)
            try: os.fsync(directory)
            finally: os.close(directory)
        finally:
            if os.path.exists(temporary): os.unlink(temporary)
    for name,data in additions.items():
        destination=templates/name
        if not destination.exists(): replace(destination,data,0o644)
    replace(helper,helper_data,0o755)
finally:
    os.close(descriptor)
print('Publisher upgraded; legacy templates/current route preserved; services and sudoers unchanged.')
PY
  exit 0
fi
install -d -o root -g root -m 0755 /usr/local/libexec /etc/acb-route-publisher
[[ ! -e /etc/acb-route-publisher/templates ]] || { echo 'Policy already installed; operator must review replacement explicitly' >&2; exit 1; }
install -d -o root -g root -m 0755 /etc/acb-route-publisher/templates
for file in "$1"/*.yml "$1"/*.yaml; do
  [[ -f "$file" ]] || continue
  install -o root -g root -m 0644 "$file" "/etc/acb-route-publisher/templates/$(basename "$file")"
done
install -o root -g root -m 0755 "$HERE/acb-route-publish.py" /usr/local/libexec/acb-route-publish
# Empty argument string prevents arbitrary helper CLI arguments. No shell/Python
# command or arbitrary destination is granted by this rule.
sudoers=$(mktemp)
trap 'rm -f "$sudoers"' EXIT
printf 'opc ALL=(root) NOPASSWD: /usr/local/libexec/acb-route-publish ""\n' > "$sudoers"
visudo -cf "$sudoers"
install -o root -g root -m 0440 "$sudoers" /etc/sudoers.d/acb-route-publisher
printf 'Publisher installed; shared route directory permissions and all services unchanged.\n'
