#!/usr/bin/env bash
# Root/operator installation only; never run this as part of an app deployment.
set -euo pipefail
[[ $EUID == 0 && $# == 1 && "$1" == /* && -d "$1" ]] || { echo 'usage (root): install-route-publisher.sh /absolute/canonical-template-directory' >&2; exit 2; }
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
