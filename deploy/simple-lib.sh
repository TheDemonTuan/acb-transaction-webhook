#!/usr/bin/env bash
# Small, deliberately non-executable deployment utilities.
log_info() { printf 'deploy: %s\n' "$*" >&2; }
log_error() { printf 'deploy ERROR: %s\n' "$*" >&2; }
log_warn() { printf 'deploy WARNING: %s\n' "$*" >&2; }
fail() { log_error "$*"; return 1; }
validate_sha() { [[ "$1" =~ ^[a-f0-9]{40}$ ]] || fail "invalid release SHA: $1"; }
validate_digest() { [[ "$1" =~ ^[a-z0-9][a-z0-9./_-]*@sha256:[a-f0-9]{64}$ ]] || fail "invalid ${2:-image} digest: $1"; }
# No shell evaluation: keys and values are separately checked before export.
load_keys() {
  local file="$1" allowed="$2" key value line
  [[ -f "$file" ]] || fail "missing file: $file" || return 1
  local -A seen=()
  while IFS= read -r line || [[ -n "$line" ]]; do
    [[ -n "$line" && "$line" != *$'\r'* && "$line" == *=* ]] || { fail "invalid line in $file"; return 1; }
    key="${line%%=*}"; value="${line#*=}"
    [[ " $allowed " == *" $key "* && -n "$value" && "$value" != *$'\n'* && "$value" != *[[:space:]]* && ! -v seen[$key] ]] || { fail "invalid or duplicate key $key in $file"; return 1; }
    seen[$key]=1
    printf -v "$key" '%s' "$value"
    export "$key"
  done < "$file"
  for key in $allowed; do [[ -v seen[$key] ]] || { fail "missing $key in $file"; return 1; }; done
}
atomic_write_file() {
  local target="$1" mode="${2:-600}" dir tmp
  dir="$(dirname "$target")"
  [[ -d "$dir" ]] || fail "missing directory: $dir" || return 1
  tmp="$(mktemp "$dir/.acb-XXXXXXXX.tmp")" || return 1
  if ! cat > "$tmp" || ! chmod "$mode" "$tmp" || ! python3 - "$tmp" "$target" <<'PY'
import os,sys
src,dst=sys.argv[1:]
with open(src,'rb') as f: os.fsync(f.fileno())
os.replace(src,dst)
fd=os.open(os.path.dirname(dst),os.O_DIRECTORY)
try: os.fsync(fd)
finally: os.close(fd)
PY
  then rm -f "$tmp"; return 1; fi
}
PAYOS_IMAGE_KEYS='RELEASE_SHA PAYMENT_RUNTIME GATEWAY_IMAGE_REF WORKER_IMAGE_REF DBTOOL_IMAGE_REF TTS_IMAGE_REF BARK_IMAGE_REF'
PAYOS_STATE_KEYS='RELEASE_SHA GATEWAY_SLOT PAYMENT_RUNTIME'
PAYOS_RUNTIME_KEYS='IMAGE_REF_BLUE IMAGE_REF_GREEN WORKER_IMAGE_REF TTS_IMAGE_REF BARK_IMAGE_REF DBTOOL_IMAGE_REF RELEASE_COMMIT_BLUE RELEASE_COMMIT_GREEN WORKER_RELEASE_COMMIT ENV_FILE SECRETS_DIR BARK_SECRET_GROUP PAYMENT_RUNTIME'
require_payos_runtime() {
  python3 - "$1" "$PAYOS_STATE_KEYS" "$PAYOS_RUNTIME_KEYS" "$PAYOS_IMAGE_KEYS" <<'PY'
import pathlib,sys
root=pathlib.Path(sys.argv[1])
def values(path):
    result={}
    for line in path.read_text().splitlines():
        key,sep,value=line.partition('=')
        if not sep or key in result: raise ValueError('invalid manifest')
        result[key]=value
    return result
try:
    state=values(root/'state.env')
    if state.get('PAYMENT_RUNTIME')!='payos': raise ValueError('legacy state')
    if set(state)!=set(sys.argv[2].split()): raise ValueError('invalid state keys')
    release=root/'releases'/state['RELEASE_SHA']
    for name,keys in (('runtime.env',sys.argv[3]),('images.env',sys.argv[4])):
        manifest=values(release/name)
        if manifest.get('PAYMENT_RUNTIME')!='payos' or set(manifest)!=set(keys.split()):
            raise ValueError('legacy runtime')
except (OSError,ValueError,KeyError):
    raise SystemExit('PAYOS_RUNTIME_MIGRATION_REQUIRED')
PY
}
payment_issuance_check() {
  local counts
  counts="$(dbtool ro -payment-counts)" || return 1
  printf '%s' "$counts" | python3 -c 'import json,sys; d=json.load(sys.stdin); assert all(type(d[k]) is int and d[k]>=0 for k in ("orders","receipts","journalSeq")); sys.exit("PAYOS_ROLLBACK_REQUIRES_PAYMENT_DRAIN" if d["orders"] or d["receipts"] else 0)'
}
provider_rollback_check() {
  local target="$1" runtime
  [[ -f "$target" ]] || return 0
  if python3 - "$target" <<'PY'
import sys
values=dict(line.rstrip('\n').split('=',1) for line in open(sys.argv[1]))
sys.exit(0 if values.get('PAYMENT_RUNTIME')=='payos' else 1)
PY
  then return 0; fi
  runtime="$(python3 - "$DEPLOY_PATH" <<'PY'
import pathlib,sys
root=pathlib.Path(sys.argv[1])
state=dict(line.rstrip('\n').split('=',1) for line in open(root/'state.env'))
print(root/'releases'/state['RELEASE_SHA']/'runtime.env')
PY
)" || return 1
  load_keys "$runtime" "$PAYOS_RUNTIME_KEYS" || return 1
  validate_digest "$DBTOOL_IMAGE_REF" dbtool || return 1
  payment_issuance_check || return 1
  fail 'PAYOS_RUNTIME_MIGRATION_REQUIRED: legacy rollback requires explicit cutover tool'
}
compose_release() {
  local release="$1" runtime="$2"; shift 2
  docker compose --project-name acb --project-directory "$release" --env-file "$DEPLOY_PATH/deploy/.env.production" --env-file "$runtime" -f "$release/compose.prod.yaml" "$@"
}
# Actual file ownership is significant: do not silently chmod live secrets.
check_secret_permissions() {
  local target_path="$1"
  [[ -e "$target_path" ]] || return 0
  [[ "$(uname -s 2>/dev/null)" =~ MINGW|MSYS|CYGWIN ]] && return 0
  if command -v stat >/dev/null 2>&1; then
    local mode
    mode="$(stat -c '%a' "$target_path" 2>/dev/null || stat -f '%Lp' "$target_path" 2>/dev/null || true)"
    if [[ -n "$mode" ]]; then
      local last_two="${mode: -2}"
      local base_name
      base_name="$(basename "$target_path")"
      if [[ "$base_name" == "bark_basic_auth_user" || "$base_name" == "bark_basic_auth_password" ]]; then
        if [[ "${mode: -1}" != "0" ]]; then
          fail "Secret file $target_path has unsafe world permissions (mode $mode)"
          return 1
        fi
      else
        if [[ "$last_two" != "00" ]]; then
          fail "Secret file $target_path has unsafe permissions (mode $mode, expected ending in 00)"
          return 1
        fi
      fi
    fi
  fi
  return 0
}
validate_permissions() {
  python3 - "$DEPLOY_PATH/deploy" "$DEPLOY_PATH/data/backups" <<'PY'
import os,stat,sys
root,backup=sys.argv[1:]
checks=[(root+'/.env.production',0o600,1000,1000),(root+'/secrets',0o700,1000,1000),(backup,0o700,1000,1000)]
checks += [(root+'/secrets/'+n,0o600,1000,1000) for n in ('app_master_key','worker_internal_token','tts_internal_token','payos_client_id','payos_api_key','payos_checksum_key')]
checks += [(root+'/secrets/'+n,0o640,1000,1000) for n in ('bark_basic_auth_user','bark_basic_auth_password')]
for path,mode,uid,gid in checks:
    st=os.stat(path)
    actual=(stat.S_IMODE(st.st_mode),st.st_uid,st.st_gid)
    if actual!=(mode,uid,gid): raise SystemExit(f'{path}: expected {mode:04o} {uid}:{gid}, actual {actual[0]:04o} {actual[1]}:{actual[2]}')
PY
}
dbtool() {
  local access="$1"; shift
  local -a flags=()
  local name="acb-deploy-dbtool-$$" code
  if [[ "$access" == ro ]]; then flags+=(--read-only); access=ro; else access=rw; fi
  if [[ -n "${DBTOOL_TIMEOUT_SEC:-}" ]]; then
    if timeout --foreground --signal=TERM --kill-after=5s "$DBTOOL_TIMEOUT_SEC" docker run --rm --name "$name" --network none --user 1000:1000 "${flags[@]}" -v "bank-event-gateway_gateway_data:/data:$access" "$DBTOOL_IMAGE_REF" -path /data/gateway.db "$@"; then return 0; else code=$?; fi
    docker rm -f "$name" >/dev/null 2>&1 || true
    return "$code"
  fi
  docker run --rm --network none --user 1000:1000 "${flags[@]}" -v "bank-event-gateway_gateway_data:/data:$access" "$DBTOOL_IMAGE_REF" -path /data/gateway.db "$@"
}
json_field() { python3 -c 'import json,sys; v=json.load(sys.stdin); x=v; [None for p in sys.argv[1].split(".") if (x:=x[p]) is None]; print(x)' "$1"; }
container_ref() { docker inspect -f '{{.Config.Image}}' "$1"; }
container_image_check() {
  local actual expected
  expected="$(docker image inspect -f '{{.Id}}' "$2")" || return 1
  actual="$(docker inspect -f '{{.Image}}' "$1")" || return 1
  [[ "$expected" == "$actual" ]] || fail "$1 image mismatch"
}
route_file() { printf '%s\n' "${ACB_CONFIG:-/opt/platform/edge/dynamic/acb.yml}"; }
route_replace() {
  local src="$1" dst="${2:-${ROUTE:-$(route_file)}}" expected="${3:-}"
  if [[ "$dst" == /opt/platform/edge/dynamic/acb.yml ]]; then
    python3 - "$src" "$dst" "$expected" <<'PY' | sudo -n /usr/local/libexec/acb-route-publish
import base64,hashlib,json,sys
from pathlib import Path
src,dst=map(Path,sys.argv[1:3]);expected=sys.argv[3]
data=src.read_bytes()
if not data or len(data)>131072: raise SystemExit('route publication size limit exceeded')
print(json.dumps({'expected_sha256':expected or hashlib.sha256(dst.read_bytes()).hexdigest(),
                  'route_base64':base64.b64encode(data).decode('ascii')}))
PY
  else
    # Disposable rehearsal uses its own route path and never calls a privileged
    # writer. Production has exactly one destination, regardless of ownership.
    python3 - "$src" "$dst" "$expected" <<'PY'
import hashlib,os,stat,sys,tempfile
src,dst,expected=sys.argv[1:]
with open(src,'rb') as stream: data=stream.read()
metadata=os.stat(dst,follow_symlinks=False)
if not stat.S_ISREG(metadata.st_mode): raise SystemExit('route must be a regular file')
if expected:
    with open(dst,'rb') as stream: current=hashlib.sha256(stream.read()).hexdigest()
    if current!=expected: raise SystemExit('route drift: compare-before-write refused')
fd,tmp=tempfile.mkstemp(prefix='.acb-',suffix='.tmp',dir=os.path.dirname(dst))
try:
    with os.fdopen(fd,'wb') as stream:
        os.fchmod(stream.fileno(),stat.S_IMODE(metadata.st_mode))
        os.fchown(stream.fileno(),metadata.st_uid,metadata.st_gid)
        stream.write(data);stream.flush();os.fsync(stream.fileno())
    os.replace(tmp,dst)
    directory=os.open(os.path.dirname(dst),os.O_DIRECTORY)
    try: os.fsync(directory)
    finally: os.close(directory)
finally:
    if os.path.exists(tmp): os.unlink(tmp)
PY
  fi
}
validate_route() {
  [[ $# == 2 && ( "$2" == blue || "$2" == green ) ]] || fail 'usage: validate_route <file> <gateway-slot>' || return 1
  python3 - "$1" "$2" "$(dirname "${BASH_SOURCE[0]}")/render-route.sh" <<'PY'
import subprocess,sys,yaml
class UniqueLoader(yaml.SafeLoader):
    pass
def unique_mapping(loader,node):
    result={}
    for key_node,value_node in node.value:
        key=loader.construct_object(key_node)
        if key in result: raise ValueError('duplicate route key: '+str(key))
        result[key]=loader.construct_object(value_node)
    return result
UniqueLoader.add_constructor(yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG,unique_mapping)
p,gw,renderer=sys.argv[1:]
with open(p,encoding='utf-8') as f: actual=yaml.load(f,Loader=UniqueLoader)
expected=yaml.load(subprocess.check_output(['bash',renderer,gw],text=True),Loader=UniqueLoader)
if actual!=expected: raise SystemExit('backend route topology/policy/slot mismatch')
PY
}
validate_baseline_route() {
  validate_route "$@"
}
