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
# Legacy bundles never receive recovery profiles, overrides, or secret requirements.
load_recovery_flags() {
  local release="${1:-}" settings
  settings="$(python3 - "$DEPLOY_PATH/deploy/.env.production" "$release" <<'PY'
import sys
env,release=sys.argv[1:]
supported=True
if release:
    import yaml
    supported='recovery-controller' in (yaml.safe_load(open(release+'/compose.prod.yaml')).get('services') or {})
values={'AUTH_RECOVERY_ENABLED':'false','AI_CAPTCHA_ENABLED':'false'}
seen=set()
if supported:
    for raw in open(env,encoding='utf-8'):
        line=raw.strip()
        if not line or line.startswith('#'): continue
        key,sep,value=line.partition('=')
        if key.strip() not in values: continue
        if not sep or key!=key.strip() or key in seen or value not in ('true','false'):
            raise SystemExit('invalid or duplicate recovery flag: '+key)
        seen.add(key); values[key]=value
    if release and values['AUTH_RECOVERY_ENABLED']=='true' and values['AI_CAPTCHA_ENABLED']=='true':
        import os
        if not os.path.isfile(release+'/compose.auth-recovery-ai.yaml'):
            raise SystemExit('bundle-invalid: missing compose.auth-recovery-ai.yaml')
print(str(supported).lower(),values['AUTH_RECOVERY_ENABLED'],values['AI_CAPTCHA_ENABLED'])
PY
)" || return 1
  read -r RECOVERY_SERVICE_SUPPORTED AUTH_RECOVERY_ENABLED AI_CAPTCHA_ENABLED <<< "$settings"
  export AUTH_RECOVERY_ENABLED AI_CAPTCHA_ENABLED
}
stop_recovery_controller() {
  if docker inspect acb-recovery-controller >/dev/null 2>&1; then
    docker stop -t 30 acb-recovery-controller >/dev/null || return 1
    [[ "$(docker inspect -f '{{.State.Running}}' acb-recovery-controller)" == false ]] || fail 'recovery controller still running'
  fi
}
compose_release() {
  local release="$1" runtime="$2"; shift 2
  local -a recovery_args=()
  load_recovery_flags "$release" || return 1
  if [[ "$AUTH_RECOVERY_ENABLED" == true ]]; then
    recovery_args+=(--profile auth-recovery)
    if [[ "$AI_CAPTCHA_ENABLED" == true ]]; then recovery_args+=(-f "$release/compose.auth-recovery-ai.yaml"); fi
  fi
  docker compose --project-name acb --project-directory "$release" --env-file "$DEPLOY_PATH/deploy/.env.production" --env-file "$runtime" -f "$release/compose.prod.yaml" "${recovery_args[@]}" "$@"
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
  load_recovery_flags "${1:-}" || return 1
  python3 - "$DEPLOY_PATH/deploy" "$DEPLOY_PATH/data/backups" "$AUTH_RECOVERY_ENABLED" "$AI_CAPTCHA_ENABLED" <<'PY'
import os,stat,sys
root,backup,recovery,ai=sys.argv[1:]
checks=[(root+'/.env.production',0o600,1000,1000),(root+'/secrets',0o700,1000,1000),(backup,0o700,1000,1000)]
checks += [(root+'/secrets/'+n,0o600,1000,1000) for n in ('app_master_key','worker_internal_token','auth_browser_internal_token','tts_internal_token')]
checks += [(root+'/secrets/'+n,0o640,1000,1000) for n in ('bark_basic_auth_user','bark_basic_auth_password')]
if recovery=='true':
    checks += [(root+'/secrets/telegram_bot_token',0o600,1000,1000)]
    if ai=='true': checks.append((root+'/secrets/ninerouter_api_key',0o600,1000,1000))
for path,mode,uid,gid in checks:
    st=os.stat(path)
    actual=(stat.S_IMODE(st.st_mode),st.st_uid,st.st_gid)
    if actual!=(mode,uid,gid): raise SystemExit(f'{path}: expected {mode:04o} {uid}:{gid}, actual {actual[0]:04o} {actual[1]}:{actual[2]}')
PY
}
# Import is a one-shot, never a runtime source of credentials. The controller's
# importer checks the v13 checksum, mutation gate and shared singleton lock.
validate_recovery_controller_bundle() {
  python3 - "$1/compose.prod.yaml" <<'PY'
import sys,yaml
labels=yaml.safe_load(open(sys.argv[1]))['services']['recovery-controller'].get('labels',[])
if 'platform.auth-control.schema=13' not in labels:
    raise SystemExit('RECOVERY_SCHEMA_REQUIRED: refuse legacy auto-login controller; disable recovery and deploy forward')
PY
}
import_recovery_credentials() {
  local release="$1" runtime="$2" require_files="${3:-false}" available
  [[ -z "${GATE_TOKEN:-}" ]] || fail 'release deploy gate before importing credentials' || return 1
  validate_recovery_controller_bundle "$release" || return 1
  available="$(python3 - "$DEPLOY_PATH/deploy/secrets" <<'PY'
import os,stat,sys
paths=[os.path.join(sys.argv[1],n) for n in ('acb_username','acb_password','acb_account')]
missing=False
for path in paths:
    try: st=os.lstat(path)
    except FileNotFoundError:
        missing=True
        continue
    if not stat.S_ISREG(st.st_mode) or (stat.S_IMODE(st.st_mode),st.st_uid,st.st_gid)!=(0o600,1000,1000):
        raise SystemExit('CREDENTIAL_IMPORT_FILE_INVALID: expected regular 0600 file owned by 1000:1000, no symlink')
print('false' if missing else 'true')
PY
)" || return 1
  if [[ "$available" != true ]]; then
    [[ "$require_files" != true ]] || fail 'CREDENTIAL_IMPORT_FILE_INVALID: provide all three legacy files before explicit import' || return 1
    log_warn 'Legacy import files missing; encrypted DB credentials remain authoritative. If unconfigured, bot starts degraded; run setup-recovery.sh --import-credentials.'
    return 0
  fi
  compose_release "$release" "$runtime" run -T --rm --no-deps \
    -v "$DEPLOY_PATH/deploy/secrets/acb_username:/run/import/acb_username:ro" \
    -v "$DEPLOY_PATH/deploy/secrets/acb_password:/run/import/acb_password:ro" \
    -v "$DEPLOY_PATH/deploy/secrets/acb_account:/run/import/acb_account:ro" \
    recovery-controller --import-credentials
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
validate_route() {
  python3 - "$1" "${2:-}" "${3:-}" <<'PY'
import sys,yaml
p,gw,fe=sys.argv[1:]
cfg=yaml.safe_load(open(p))['http']; r=cfg['routers']; s=cfg['services']
assert s['acb-service']['loadBalancer']['servers']==[{'url':f'http://acb-web-{gw}:8090'}]
assert s['acb-frontend-service']['loadBalancer']['servers']==[{'url':f'http://acb-frontend-{fe}:8080'}]
assert s['acb-service']['loadBalancer']['responseForwarding']['flushInterval']=='100ms'
for service in ('acb-service','acb-frontend-service'):
    assert s[service]['loadBalancer']['healthCheck']['path']=='/readyz'
for name,service,rule in (('acb-deploy-gateway','acb-service','Host(`gateway-deploy.acb.internal.invalid`) && Path(`/readyz`)'),('acb-deploy-frontend','acb-frontend-service','Host(`frontend-deploy.acb.internal.invalid`)')):
    assert r[name]['service']==service and r[name]['entryPoints']==['slot-probe'] and r[name]['rule']==rule
    assert not r[name].get('middlewares')
expected={'acb-deny-internal':1000,'acb-public-deny-private':1000,'acb-public-sse-router':1200,'acb-public-api-router':1100,'acb-api-router':200,'acb-credentials-router':250,'acb-public-frontend-router':100,'acb-frontend-router':100}
for name,priority in expected.items(): assert r[name]['priority']==priority and r[name]['entryPoints']==['web']
credential=r['acb-credentials-router']
owner_rule=r['acb-frontend-router']['rule']
assert credential['rule']==owner_rule+' && Path(`/admin/acb-credentials`)'
assert credential['middlewares']==['tunnel-only','acb-credentials-security']
assert credential['service']=='acb-frontend-service'
assert cfg['middlewares']['acb-credentials-security']['headers']['customResponseHeaders']=={
    'Cache-Control':'no-store','Referrer-Policy':'no-referrer','X-Frame-Options':'DENY',
    'Content-Security-Policy':"default-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'; object-src 'none'; script-src 'self'; script-src-attr 'none'; connect-src 'self'; img-src 'self' data:; font-src 'self'; style-src 'self' 'unsafe-inline'"}
PY
}
validate_baseline_route() {
  python3 - "$1" "$2" "$3" <<'PY'
import sys,yaml
c=yaml.safe_load(open(sys.argv[1]))['http']['services']
for service,url in (('acb-service','http://acb-web-'+sys.argv[2]+':8090'),('acb-frontend-service','http://acb-frontend-'+sys.argv[3]+':8080')):
    assert c[service]['loadBalancer']['servers']==[{'url':url}],(service,c[service])
PY
}
