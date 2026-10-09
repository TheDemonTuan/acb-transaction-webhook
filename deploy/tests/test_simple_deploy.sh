#!/usr/bin/env bash
# Disposable-daemon deployment rehearsal. Never run against a daemon with production names.
set -Eeuo pipefail
umask 077
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
require() { command -v "$1" >/dev/null 2>&1 || fail "Missing required dependency: $1"; }
for tool in docker python3 sqlite3 git curl openssl tar flock timeout sha256sum realpath xargs ps pkill; do require "$tool"; done
repo="$(dirname "$(dirname "$(dirname "$(realpath -- "${BASH_SOURCE[0]}")")")")"
python3 -c 'import yaml' >/dev/null 2>&1 || fail 'Missing required dependency: python3 PyYAML'
docker compose version >/dev/null 2>&1 || fail 'Missing required dependency: docker compose'
docker info >/dev/null 2>&1 || fail 'Docker daemon unavailable'

log_ts() {
  date -u +'%Y-%m-%dT%H:%M:%SZ'
}

log_step() {
  printf '[%s] [REHEARSAL] %s\n' "$(log_ts)" "$*"
}

log_test_start() {
  printf '[%s] [TEST-START] %s\n' "$(log_ts)" "$*"
}

log_test_pass() {
  printf '[%s] [TEST-PASS] %s\n' "$(log_ts)" "$*"
}

log_test_note() {
  printf '[%s] [TEST-NOTE] %s\n' "$(log_ts)" "$*"
}

orig_args=("$@")
images_env=""
bundle=""
target_suite="all"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --images-env)
      [[ $# -ge 2 && "$2" == /* ]] || fail 'Invalid --images-env: requires absolute path'
      images_env="$2"
      shift 2
      ;;
    --bundle)
      [[ $# -ge 2 && "$2" == /* ]] || fail 'Invalid --bundle: requires absolute path'
      bundle="$2"
      shift 2
      ;;
    --suite|--shard)
      [[ $# -ge 2 ]] || fail "Missing value for $1"
      case "$2" in
        all|lifecycle|fault-matrix)
          target_suite="$2"
          ;;
        *)
          fail "Unknown suite/shard '$2'. Expected: all | lifecycle | fault-matrix"
          ;;
      esac
      shift 2
      ;;
    *)
      fail "Unknown argument: $1. Usage: test_simple_deploy.sh --images-env /absolute/images.env --bundle /absolute/bundle [--suite all|lifecycle|fault-matrix]"
      ;;
  esac
done

[[ -n "$images_env" && -n "$bundle" ]] || fail 'Usage: test_simple_deploy.sh --images-env /absolute/images.env --bundle /absolute/bundle [--suite all|lifecycle|fault-matrix]'
[[ -f "$images_env" && -f "$bundle/deploy.sh" && -f "$bundle/migrate-payos-runtime.sh" && -f "$bundle/compose.prod.yaml" && -s "$bundle/SHA256SUMS" ]] || fail 'Missing images.env or complete checksummed bundle'
[[ -z "$(docker ps -aq --filter name='^/acb-' --filter name='^/edge-traefik$')" ]] || fail 'Unsafe Docker daemon: ACB or edge-traefik container exists'
for name in bank-event-gateway_gateway_data bank-event-gateway_bark_data; do
  ! docker volume inspect "$name" >/dev/null 2>&1 || fail "Unsafe Docker daemon: $name exists"
done
for name in edge-acb acb-core acb-egress; do
  ! docker network inspect "$name" >/dev/null 2>&1 || fail "Unsafe Docker daemon: $name exists"
done

if (( EUID != 1000 )); then
  require sudo
  command -v getent >/dev/null || fail 'Missing required dependency: getent'
  account="$(getent passwd 1000 | cut -d: -f1)"
  [[ -n "$account" ]] || fail 'Disposable runner requires a UID 1000 account'
  sock="${DOCKER_HOST:-unix:///var/run/docker.sock}"
  sock="${sock#unix://}"
  [[ -S "$sock" ]] || fail "Missing local Docker socket: $sock"
  archive_dir="$(mktemp -d "${TMPDIR:-/tmp}/acb-fixture.XXXXXXXX")"
  cp "$repo/platform/edge/dynamic/acb.yml" "$archive_dir/acb.yml"
  cp "$repo/deploy/tests/simple_deploy_https_fixture.py" "$archive_dir/https_fixture.py"
  chmod 755 "$archive_dir"
  chmod 644 "$archive_dir/"*
  export REHEARSAL_ROUTE_SOURCE="$archive_dir/acb.yml" REHEARSAL_HTTPS_FIXTURE="$archive_dir/https_fixture.py"
  docker_group="$(stat -c '%G' "$sock")"
  [[ "$docker_group" != root && "$docker_group" != UNKNOWN ]] || fail 'Docker socket has no dedicated group for fixture UID 1000'
  sudo usermod -aG "$docker_group" "$account"
  registry_config="${DOCKER_CONFIG:-$HOME/.docker}/config.json"
  [[ -f "$registry_config" ]] || fail 'Missing GHCR Docker credential config for UID 1000 rehearsal'
  auth_dir="$(mktemp -d "${TMPDIR:-/tmp}/acb-ghcr.XXXXXXXX")"
  sudo chown 1000:1000 "$auth_dir"
  sudo install -m 600 -o 1000 -g 1000 "$registry_config" "$auth_dir/config.json"
  export DOCKER_CONFIG="$auth_dir"
  uid_rc=0
  sudo -E -u "$account" -g '#1000' bash "$0" "${orig_args[@]}" || uid_rc=$?
  rm -rf -- "$archive_dir"
  sudo rm -rf -- "$auth_dir"
  exit "$uid_rc"
fi

base_sha=a6bc2a4739c7cd4776188d30d71cba6ecb79b970
root="$(mktemp -d "${TMPDIR:-/tmp}/acb-rehearsal.XXXXXXXX")"
cleanup() {
  rc=$?
  trap - EXIT
  if [[ "$rc" != 0 ]]; then printf '[%s] [REHEARSAL] Rehearsal failed; fixture root: %s\n' "$(log_ts)" "$root" >&2; fi
  if [[ -n "${active_deploy_pid:-}" ]] && kill -0 "$active_deploy_pid" 2>/dev/null; then
    pkill -TERM -P "$active_deploy_pid" 2>/dev/null || :
    kill "$active_deploy_pid" 2>/dev/null || :
  fi
  if [[ -n "${marker:-}" && -f "$marker" ]]; then kill "$(cat "$marker")" 2>/dev/null || :; fi
  [[ -z "${https_pid:-}" ]] || kill "$https_pid" >/dev/null 2>&1 || :
  docker ps -aq --filter name='^/acb-' --filter name='^/edge-traefik$' | xargs -r docker rm -f >/dev/null 2>&1 || :
  for name in bank-event-gateway_gateway_data bank-event-gateway_bark_data; do docker volume rm "$name" >/dev/null 2>&1 || :; done
  for name in edge-acb acb-core acb-egress; do docker network rm "$name" >/dev/null 2>&1 || :; done
  if [[ "$rc" == 0 ]]; then rm -rf -- "$root"; fi
  exit "$rc"
}
trap cleanup EXIT

log_step "Initializing fixture directories at $root (selected suite: $target_suite)..."
mkdir -p "$root/releases/$base_sha" "$root/data/backups" "$root/deploy/secrets" "$root/edge/dynamic"
chmod 700 "$root/deploy/secrets" "$root/data/backups"
baseline="$root/releases/$base_sha"

release_sha="$(python3 - "$images_env" <<'PY'
import re,sys
values={}
for line in open(sys.argv[1],encoding='utf8'):
    if not line.strip(): continue
    key,sep,value=line.strip().partition('=')
    assert sep and key not in values and value and re.fullmatch(r'[A-Z_]+',key),(key,value)
    values[key]=value
assert set(values)=={'RELEASE_SHA','PAYMENT_RUNTIME','GATEWAY_IMAGE_REF','WORKER_IMAGE_REF','DBTOOL_IMAGE_REF','TTS_IMAGE_REF','BARK_IMAGE_REF'}
assert values['PAYMENT_RUNTIME']=='payos'
assert re.fullmatch('[a-f0-9]{40}',values['RELEASE_SHA'])
for key,value in values.items():
    if key.endswith('_IMAGE_REF'): assert re.fullmatch(r'[^\s@]+@sha256:[a-f0-9]{64}',value),(key,value)
print(values['RELEASE_SHA'])
PY
)"
target="$root/releases/$release_sha"
mkdir -p "$target"
cp -a "$bundle/." "$target/"
chmod 644 "$target/bark-entrypoint.sh" "$target/compose.prod.yaml"
cp "$images_env" "$target/images.env"
cp "$bundle/SHA256SUMS" "$target/"
log_test_start "Uploaded release contains complete checksummed setup bundle"
python3 - "$target/SHA256SUMS" <<'PY'
import sys
expected={'deploy.sh','simple-lib.sh','healthcheck.sh','render-route.sh','backup-db.sh','restore-db.sh','backup-secrets.sh','migrate-payos-runtime.sh','migrate-payos-runtime.py','workflow-runtime.py','acb-route-publish.py','install-route-publisher.sh','verify-frontend.py','compose.prod.yaml','bark-entrypoint.sh','images.env','source-run.env'}
names=[line.rstrip('\n').split('  ',1)[1] for line in open(sys.argv[1])]
assert len(names)==len(expected) and set(names)==expected, ('incomplete uploaded checksum manifest',names)
PY
(cd "$target" && sha256sum -c SHA256SUMS) || fail 'Uploaded release checksum verification failed'
cp "$target/migrate-payos-runtime.py" "$root/migration.py.saved"
printf '\n# deliberate upload corruption\n' >> "$target/migrate-payos-runtime.py"
if (cd "$target" && sha256sum -c SHA256SUMS) >"$root/corrupt-bundle.log" 2>&1; then fail 'Corrupted uploaded migration passed checksum verification'; fi
cp "$root/migration.py.saved" "$target/migrate-payos-runtime.py"
(cd "$target" && sha256sum -c SHA256SUMS) || fail 'Restored upload checksum verification failed'
log_test_pass "Packaged migration companion detects corruption"

python3 - "$root" <<'PY'
import os,secrets,sys
root=sys.argv[1]
for name in ('worker_internal_token','tts_internal_token','bark_basic_auth_user','bark_basic_auth_password','app_master_key'):
    path=f'{root}/deploy/secrets/{name}'
    with open(path,'w') as f:f.write(secrets.token_hex(32)+'\n')
    os.chown(path,1000,1000)
    os.chmod(path,0o640 if name.startswith('bark_') else 0o600)
PY
cat > "$root/deploy/.env.production" <<'EOF'
PUBLIC_ORIGIN=https://bank.tuannguyenviet.site
PUBLIC_VIEWER_ORIGIN=https://transactions.tuannguyenviet.site
OWNER_SUBJECTS=deploy-smoke@example.invalid
CF_ACCESS_ISSUER=https://deploy-smoke.cloudflareaccess.com
CF_ACCESS_AUDIENCE=deploy-smoke-audience
CF_ACCESS_JWKS_URL=https://deploy-smoke.cloudflareaccess.com/cdn-cgi/access/certs
PAYMENT_PUBLIC_ORIGIN=https://transactions.tuannguyenviet.site
PAYMENTS_ENABLED=false
PAYOS_WEBHOOK_CONFIRMED=false
EOF
chmod 600 "$root/deploy/.env.production"
ln -s "$root/deploy/.env.production" "$baseline/.env.production"
ln -s "$root/deploy/secrets" "$baseline/secrets"
for name in edge-acb acb-core acb-egress; do
  if [[ "$name" == acb-core ]]; then docker network create --internal "$name" >/dev/null; else docker network create "$name" >/dev/null; fi
done
cp "${REHEARSAL_ROUTE_SOURCE:-$repo/platform/edge/dynamic/acb.yml}" "$root/edge/dynamic/acb.yml"
log_step "Verifying missing canonical state blocks preflight..."
if ACB_ROUTE_FILE="$root/edge/dynamic/acb.yml" FAILOVER_REGISTRY_DIR="$root/registry" DEPLOY_PATH="$root" \
  bash "$target/deploy.sh" --check "$release_sha" >"$root/missing-volume.log" 2>&1; then
  fail 'Missing canonical state did not block preflight'
fi
[[ "$(cat "$root/missing-volume.log")" == *BACKEND_STATE_MIGRATION_REQUIRED* ]] || fail 'Missing canonical state omitted migration requirement'
[[ ! -e "$root/state.env" ]] || fail 'Missing-volume preflight wrote state'
for name in bank-event-gateway_gateway_data bank-event-gateway_bark_data; do docker volume create "$name" >/dev/null; done
gateway_volume_id="$(docker volume inspect --format '{{.Name}}:{{.CreatedAt}}' bank-event-gateway_gateway_data)"

cp "$images_env" "$root/base-images.env"
set -a
# shellcheck source=/dev/null
source "$root/base-images.env"
set +a
# Baseline is the candidate payOS contract with a different release identity.
# Legacy-tool admission/drain phases belong to the one-time cutover drill.
cp "$target/compose.prod.yaml" "$target/bark-entrypoint.sh" "$baseline/"
python3 - "$root" "$baseline" "$base_sha" <<'PY'
import pathlib,sys
root,baseline=map(pathlib.Path,sys.argv[1:3])
sha=sys.argv[3]
refs=dict(line.split('=',1) for line in (root/'base-images.env').read_text().splitlines())
refs.pop('RELEASE_SHA')
(baseline/'images.env').write_text('RELEASE_SHA='+sha+'\n'+''.join(k+'='+v+'\n' for k,v in refs.items()))
runtime={
    'IMAGE_REF_BLUE':refs['GATEWAY_IMAGE_REF'],'IMAGE_REF_GREEN':refs['GATEWAY_IMAGE_REF'],
    **{key:refs[key] for key in ('WORKER_IMAGE_REF','TTS_IMAGE_REF','BARK_IMAGE_REF','DBTOOL_IMAGE_REF')},
    'RELEASE_COMMIT_BLUE':sha,'RELEASE_COMMIT_GREEN':sha,'WORKER_RELEASE_COMMIT':sha,
    'ENV_FILE':str(root/'deploy/.env.production'),'SECRETS_DIR':str(root/'deploy/secrets'),'BARK_SECRET_GROUP':'1000','PAYMENT_RUNTIME':'payos',
}
(baseline/'runtime.env').write_text(''.join(k+'='+v+'\n' for k,v in runtime.items()))
for name in ('compose.prod.yaml','images.env','runtime.env'):
    (baseline/name).chmod(0o600)
(root/'state.env').write_text('RELEASE_SHA='+sha+'\nGATEWAY_SLOT=blue\nPAYMENT_RUNTIME=payos\n')
(root/'state.env').chmod(0o600)
PY
cp "$target/"{deploy.sh,simple-lib.sh,healthcheck.sh,render-route.sh} "$baseline/"
(cd "$baseline" && sha256sum compose.prod.yaml images.env runtime.env deploy.sh simple-lib.sh healthcheck.sh render-route.sh bark-entrypoint.sh > SHA256SUMS)
docker volume rm bank-event-gateway_gateway_data >/dev/null
if ACB_ROUTE_FILE="$root/edge/dynamic/acb.yml" FAILOVER_REGISTRY_DIR="$root/registry" DEPLOY_PATH="$root" \
  bash "$target/deploy.sh" --check "$release_sha" >"$root/missing-volume.log" 2>&1; then
  fail 'Missing gateway data volume did not block preflight'
fi
[[ "$(cat "$root/missing-volume.log")" == *'missing volume bank-event-gateway_gateway_data'* ]] || fail 'Missing-volume preflight failed for another reason'
docker volume create bank-event-gateway_gateway_data >/dev/null
gateway_volume_id="$(docker volume inspect --format '{{.Name}}:{{.CreatedAt}}' bank-event-gateway_gateway_data)"
log_step "Pulling baseline images and preparing volumes..."
for ref in "$GATEWAY_IMAGE_REF" "$WORKER_IMAGE_REF" "$DBTOOL_IMAGE_REF" "$TTS_IMAGE_REF" "$BARK_IMAGE_REF"; do docker pull "$ref" >/dev/null; done
docker pull busybox:1.37.0 >/dev/null
docker run --rm --network none -v bank-event-gateway_gateway_data:/data busybox:1.37.0 chown 1000:1000 /data
docker run --rm --network none --user 1000:1000 -v bank-event-gateway_gateway_data:/data "$DBTOOL_IMAGE_REF" -path /data/gateway.db -migrate
docker pull python:3.13-alpine >/dev/null
docker run --rm --network none --user 1000:1000 -v bank-event-gateway_gateway_data:/data python:3.13-alpine \
  python -c 'import sqlite3; db=sqlite3.connect("/data/gateway.db"); db.execute("CREATE TABLE rehearsal_sentinel (id INTEGER PRIMARY KEY, value TEXT NOT NULL)"); db.execute("INSERT INTO rehearsal_sentinel VALUES (1, ?)", ("retain-after-migrate",)); db.execute("INSERT INTO connections(id,state,created_at,updated_at) VALUES (?,?,?,?)", ("rehearsal-connection","UNCONFIGURED","2026-01-01T00:00:00Z","2026-01-01T00:00:00Z")); db.executemany("INSERT INTO transactions(id,connection_id,semantic_key,canonical_hash,transaction_date,effective_date,credit,parser_version,first_seen_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)", [("rehearsal-txn-"+str(n),"rehearsal-connection","rehearsal-key-"+str(n),"rehearsal-hash-"+str(n),"2026-01-01T00:00:00Z","2026-01-01T00:00:00Z",100,"v1","2026-01-01T00:00:0"+str(n)+"Z") for n in (1,2)]); db.commit()'

log_step "Starting canonical baseline containers..."
docker compose --project-name acb --project-directory "$baseline" --env-file "$root/deploy/.env.production" --env-file "$baseline/runtime.env" \
  -f "$baseline/compose.prod.yaml" up -d --no-deps gateway-blue worker tts-gateway bark

log_step "Waiting for baseline containers to become healthy..."
for service in gateway-blue worker tts-gateway bark; do
  deadline=$((SECONDS+120))
  until [[ "$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{end}}' "acb-$service")" == healthy ]]; do
    if (( SECONDS >= deadline )); then
      docker inspect -f 'fixture status={{.State.Status}} exit={{.State.ExitCode}} health={{if .State.Health}}{{.State.Health.Status}}{{else}}missing{{end}}' "acb-$service" >&2
      if [[ "$service" == bark ]]; then
        python3 - "$root/deploy/secrets" <<'PY' >&2
import pathlib, subprocess, sys
result = subprocess.run(['docker', 'logs', '--tail', '20', 'acb-bark'], capture_output=True, text=True)
text = result.stdout + result.stderr
secrets = pathlib.Path(sys.argv[1])
for path in secrets.iterdir():
    text = text.replace(path.read_text().strip(), '[redacted]')
print(text)
PY
      fi
      fail "Baseline service acb-$service never became healthy"
    fi
    sleep 1
  done
done
log_step "All baseline containers healthy."

cp "${REHEARSAL_ROUTE_SOURCE:-$repo/platform/edge/dynamic/acb.yml}" "$root/edge/dynamic/acb.yml"
cat > "$root/edge/dynamic/middlewares.yml" <<'EOF'
http:
  middlewares:
    tunnel-only:
      ipAllowList:
        sourceRange: ["192.0.2.1/32", "127.0.0.1/32"]
    deny-internal:
      ipAllowList:
        sourceRange: ["192.0.2.1/32"]
    security-headers:
      headers:
        frameDeny: true
    public-sse-rate-limit:
      rateLimit: {average: 100, burst: 100}
    public-api-rate-limit:
      rateLimit: {average: 100, burst: 100}
    public-sse-inflight-ip:
      inFlightReq: {amount: 100}
    public-sse-inflight-global:
      inFlightReq: {amount: 100}
    public-api-inflight-ip:
      inFlightReq: {amount: 100}
    public-api-inflight-global:
      inFlightReq: {amount: 100}
EOF
for file in portfolio bark 9router; do printf '# unrelated fixture %s\n' "$file" > "$root/edge/dynamic/$file.yml"; done
sha256sum "$root/edge/dynamic/"{middlewares,portfolio,bark,9router}.yml > "$root/unrelated.sha256"
cat > "$root/edge/traefik.yml" <<'EOF'
accessLog:
  format: json
entryPoints:
  web:
    address: ':8080'
  slot-probe:
    address: '127.0.0.1:18080'
providers:
  file:
    directory: /etc/traefik/dynamic
    watch: true
EOF
log_step "Starting edge-traefik router..."
docker run -d --name edge-traefik --network edge-acb -v "$root/edge/traefik.yml:/etc/traefik/traefik.yml:ro" -v "$root/edge/dynamic:/etc/traefik/dynamic:ro" traefik:v3.7.13 >/dev/null
mkdir -p "$root/registry" "$root/bin"
docker pull curlimages/curl:8.12.1 >/dev/null
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj '/CN=bank.tuannguyenviet.site' \
  -addext 'subjectAltName=DNS:bank.tuannguyenviet.site,DNS:transactions.tuannguyenviet.site' \
  -keyout "$root/edge/fixture.key" -out "$root/edge/fixture.crt" >/dev/null 2>&1
python3 "${REHEARSAL_HTTPS_FIXTURE:-$repo/deploy/tests/simple_deploy_https_fixture.py}" "$root/edge/fixture.crt" "$root/edge/fixture.key" 19443 &
https_pid=$!
real_curl="$(command -v curl)"
cat > "$root/bin/curl" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
for arg; do
  case "$arg" in
    https://bank.tuannguyenviet.site*) exec "$REAL_CURL" --noproxy '*' --cacert "$FIXTURE_CA" --connect-to bank.tuannguyenviet.site:443:127.0.0.1:19443 "$@" ;;
    https://transactions.tuannguyenviet.site*) exec "$REAL_CURL" --noproxy '*' --cacert "$FIXTURE_CA" --connect-to transactions.tuannguyenviet.site:443:127.0.0.1:19443 "$@" ;;
  esac
done
exec "$REAL_CURL" "$@"
SH
chmod +x "$root/bin/curl"
real_docker="$(command -v docker)"
cat > "$root/bin/docker" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  pull|inspect|exec|stop|start|compose|run)
    if [[ " $* " == *frontend* ]]; then
      printf 'Backend rehearsal forbids frontend image/container operations\n' >&2
      exit 1
    fi ;;
esac
case "${DOCKER_FAULT_MODE:-}" in
  before-worker)
    if [[ " $* " == *' /worker -deploy-capabilities '* && " $* " == *' exec '* ]]; then
      printf '%s\n' "$BASHPID" > "$DOCKER_FAULT_MARKER"
      for ((n=0;n<600;n++)); do [[ -e "$DOCKER_FAULT_RELEASE" ]] && break; sleep .1; done
      [[ -e "$DOCKER_FAULT_RELEASE" ]] || exit 1
    fi ;;
  after-commit)
    if [[ " $* " == *' stop acb-gateway-blue '* || " $* " == *' stop acb-gateway-green '* ]]; then
      printf '%s\n' "$BASHPID" > "$DOCKER_FAULT_MARKER"
      for ((n=0;n<600;n++)); do [[ -e "$DOCKER_FAULT_RELEASE" ]] && break; sleep .1; done
      [[ -e "$DOCKER_FAULT_RELEASE" ]] || exit 1
    fi ;;
  unhealthy-worker)
    if [[ " $* " == *' up -d --no-deps worker '* ]]; then
      "$REAL_DOCKER" "$@"
      "$REAL_DOCKER" stop -t 1 acb-worker >/dev/null
      exit 0
    fi ;;
  quiesce-timeout)
    if [[ " $* " == *' /worker -quiesce '* ]]; then sleep 50; fi ;;
  legacy-worker-capabilities|candidate-missing-payment-drain)
    if [[ " $* " == *' /worker -deploy-capabilities '* ]] && { [[ "$DOCKER_FAULT_MODE" == legacy-worker-capabilities && " $* " == *' exec '* ]] || [[ "$DOCKER_FAULT_MODE" == candidate-missing-payment-drain && " $* " == *' run '* ]]; }; then
      report="$("$REAL_DOCKER" "$@")"
      printf '%s\n' "$DOCKER_FAULT_MODE" > "${IDENTITY_FAULT_MARKER:?}"
      printf '%s' "$report" | python3 -c 'import json,os,sys; d=json.load(sys.stdin); d.pop("paymentDrain"); d.update(protocol=2,sessionCheckpoint=True) if os.environ["DOCKER_FAULT_MODE"]=="legacy-worker-capabilities" else None; json.dump(d,sys.stdout)'
      exit 0
    fi ;;
  quiesce-active-payments|quiesce-active-deliveries|quiesce-dispatcher-active|quiesce-invalid-journal|quiesce-legacy|quiesce-counter-boolean)
    if [[ " $* " == *' /worker -quiesce '* ]]; then
      report="$("$REAL_DOCKER" "$@")"
      printf '%s\n' "$DOCKER_FAULT_MODE" > "${IDENTITY_FAULT_MARKER:?}"
      printf '%s' "$report" | python3 -c '
import json,os,sys
d=json.load(sys.stdin)
mode=os.environ["DOCKER_FAULT_MODE"]
if mode=="quiesce-active-payments": d["activePaymentRequests"]=1
elif mode=="quiesce-active-deliveries": d["activeDeliveries"]=1
elif mode=="quiesce-dispatcher-active": d["dispatcher"]="DRAINING"
elif mode=="quiesce-invalid-journal": d["journalSeq"]=-1
elif mode=="quiesce-counter-boolean": d["activePaymentRequests"]=False
elif mode=="quiesce-legacy":
    # Legacy fields intentionally appear only in this rejected reply fixture.
    d.pop("activePaymentRequests")
    d.update(activePoll=False,sessionCheckpointed=True,generation=0)
json.dump(d,sys.stdout)
'
      exit 0
    fi ;;
  wrong-gateway|wrong-gateway-slot)
    if [[ " $* " == *' up -d --no-deps gateway-green '* ]]; then
      args=("$@")
      for ((i=0; i<${#args[@]}; i++)); do
        if [[ "${args[i]}" == up ]]; then
          printf '%s\n' "$DOCKER_FAULT_MODE" > "${IDENTITY_FAULT_MARKER:?}"
          exec "$REAL_DOCKER" "${args[@]:0:i}" -f "${WRONG_GATEWAY_OVERRIDE:?}" "${args[@]:i}"
        fi
      done
    fi ;;
  corrupt-backup)
    if [[ " $* " == *' -backup-to /backup/gateway.db '* ]]; then
      "$REAL_DOCKER" "$@"
      for arg; do
        if [[ "$arg" == *:/backup:rw ]]; then
          printf 'corrupted external snapshot\n' > "${arg%%:/backup:rw}/gateway.db"
          exit 0
        fi
      done
      exit 1
    fi ;;
  stale-service)
    if [[ " $* " == *' --network container:edge-traefik '* && " $* " == *' gateway-deploy.acb.internal.invalid '* && ! -e "${STALE_SERVICE_APPLIED:?}" ]]; then
      : > "$STALE_SERVICE_APPLIED"
      python3 - "$ACB_ROUTE_FILE" <<'PY'
import os,sys,yaml
p=sys.argv[1]
with open(p) as f: data=yaml.safe_load(f)
data['http']['services']['acb-service']['loadBalancer']['servers']=[{'url':'http://acb-web-stale:8090'}]
t=p+'.stale-service.tmp'
with open(t,'w') as f: yaml.safe_dump(data,f,sort_keys=False)
os.chmod(t,0o644);os.replace(t,p)
PY
      ready=0
      for ((n=0;n<20;n++)); do
        code="$("$REAL_DOCKER" run --rm --network container:edge-traefik curlimages/curl:8.12.1 -s -o /dev/null -w '%{http_code}' -H 'Host: gateway-deploy.acb.internal.invalid' http://127.0.0.1:18080/readyz)" || code=000
        if [[ "$code" != 200 ]]; then ready=1; break; fi
        sleep 1
      done
      ((ready)) || exit 1
    fi ;;
  retire-failure)
    if [[ " $* " == *' stop acb-gateway-blue '* ]]; then
      "$REAL_DOCKER" pause acb-gateway-blue >/dev/null
      exit 1
    fi ;;
esac
exec "$REAL_DOCKER" "$@"
SH
chmod +x "$root/bin/docker"
export REAL_DOCKER="$real_docker"
export REAL_CURL="$real_curl" FIXTURE_CA="$root/edge/fixture.crt" CURL_CA_BUNDLE="$root/edge/fixture.crt"
export PATH="$root/bin:$PATH" ACB_ROUTE_FILE="$root/edge/dynamic/acb.yml" FAILOVER_REGISTRY_DIR="$root/registry"
export DEPLOY_PATH="$root"

log_step "Waiting for HTTPS fixture and Traefik edge to become responsive..."
for _ in {1..30}; do
  if "$real_curl" -fsS --cacert "$FIXTURE_CA" --connect-to bank.tuannguyenviet.site:443:127.0.0.1:19443 \
    -o /dev/null https://bank.tuannguyenviet.site/; then break; fi
  kill -0 "$https_pid" 2>/dev/null || fail 'Fixture HTTPS server exited'
  sleep 1
done
[[ "$(docker run --rm --network edge-acb curlimages/curl:8.12.1 -s -o /dev/null -w '%{http_code}' \
    -H 'Host: transactions.tuannguyenviet.site' http://edge-traefik:8080/api/public/v1/transactions)" == 403 ]] || fail 'Public API did not reject fixture source with 403'
for path in /api/private /internal /admin /admin/acb-credentials /api/v1/connection/credentials /readyz; do
  code="$(docker run --rm --network edge-acb curlimages/curl:8.12.1 -s -o /dev/null -w '%{http_code}' \
    -H 'Host: transactions.tuannguyenviet.site' "http://edge-traefik:8080$path")"
  [[ "$code" == 403 ]] || fail "Public private path $path unexpectedly returned $code"
done
[[ "$(docker run --rm --network edge-acb curlimages/curl:8.12.1 -s -o /dev/null -w '%{http_code}' \
  -H 'Host: gateway-deploy.acb.internal.invalid' http://edge-traefik:8080/readyz)" == 404 ]] || fail 'Internal probe host exposed on public web'
old_route_sha="$(sha256sum "$root/edge/dynamic/acb.yml")"
initial_state_sha="$(sha256sum "$root/state.env")"
old_worker_id="$(docker inspect --format '{{.Id}}' acb-worker)"
secret_before="$(sha256sum "$root/deploy/secrets/"* | sha256sum)"
log_step "Base environment initialization complete."

expect_unchanged_failure() {
  local label="$1"; shift
  if "$@" >"$root/negative-output.log" 2>&1; then fail "$label unexpectedly succeeded"; fi
  [[ "$(sha256sum "$root/state.env")" == "$initial_state_sha" ]] || fail "$label changed state.env"
  [[ "$(sha256sum "$root/edge/dynamic/acb.yml")" == "$old_route_sha" ]] || fail "$label changed live route"
  [[ "$(docker inspect --format '{{.Id}}' acb-worker)" == "$old_worker_id" ]] || fail "$label recreated worker"
  printf '[%s] [TEST-PASS] %s fails without route/state/worker mutation\n' "$(log_ts)" "$label"
}

wait_marker() {
  local marker="$1" pid="$2" n
  for ((n=0;n<600;n++)); do
    [[ -e "$marker" ]] && return 0
    kill -0 "$pid" 2>/dev/null || fail "Deploy exited before fault marker: $marker"
    sleep .1
  done
  fail "Timed out waiting for fault marker: $marker"
}

verify_baseline() {
  [[ "$(sha256sum "$root/state.env")" == "$state_before" ]] || fail "$1 changed baseline state"
  [[ "$(sha256sum "$root/edge/dynamic/acb.yml")" == "$route_before" ]] || fail "$1 changed baseline route"
  [[ "$(docker inspect -f '{{.State.Health.Status}}' acb-worker)" == healthy ]] || fail "$1 left worker unhealthy"
  [[ "$(docker inspect -f '{{.Config.Image}}' acb-worker)" == "$WORKER_IMAGE_REF" ]] || fail "$1 changed worker image"
}


run_suite_lifecycle() {
  log_test_start "Legacy manifests reject deployment before lockfile mutation"
  cp "$root/state.env" "$root/payOS-state.saved"
  printf 'RELEASE_SHA=%s\nGATEWAY_SLOT=blue\n' "$base_sha" > "$root/state.env"
  rm -f "$root/.deploy.lock"
  for operation in --check --reconcile ''; do
    if bash "$target/deploy.sh" ${operation:+"$operation"} "$release_sha" >"$root/legacy.log" 2>&1; then fail 'Legacy state admitted by normal deploy'; fi
    [[ "$(cat "$root/legacy.log")" == *PAYOS_RUNTIME_MIGRATION_REQUIRED* ]] || fail 'Missing legacy diagnostic'
    [[ ! -e "$root/.deploy.lock" && ! -e "$root/.deploy-pending" ]] || fail 'Legacy rejection mutated deployment files'
  done
  cp "$root/payOS-state.saved" "$root/state.env"
  log_test_pass "Legacy state requires explicit migration"
  log_test_start "Incomplete static-hosting migration blocks all backend operations"
  mkdir "$root/.static-hosting-pending"
  for operation in --check --reconcile ''; do
    if [[ -n "$operation" ]]; then
      expect_unchanged_failure "pending migration $operation" bash "$target/deploy.sh" "$operation" "$release_sha"
    else
      expect_unchanged_failure 'pending migration deploy' bash "$target/deploy.sh" "$release_sha"
    fi
    [[ "$(cat "$root/negative-output.log")" == *STATIC_HOSTING_MIGRATION_PENDING* ]] || fail 'Pending migration omitted required evidence'
  done
  expect_unchanged_failure 'pending migration rollback' bash "$target/deploy.sh" --rollback
  [[ "$(cat "$root/negative-output.log")" == *STATIC_HOSTING_MIGRATION_PENDING* ]] || fail 'Pending migration rollback omitted required evidence'
  rmdir "$root/.static-hosting-pending"
  log_test_pass "Pending migration prevents backend mutation"

  log_step "Starting suite: lifecycle"

  log_test_start "Preflight negative checks (invalid SHA, digest, missing/world-readable secret, failover)"
  expect_unchanged_failure 'invalid SHA' bash "$target/deploy.sh" abc
  cp "$target/images.env" "$root/valid-images.env"
  printf 'GATEWAY_IMAGE_REF=invalid\n' >> "$target/images.env"
  expect_unchanged_failure 'invalid image digest' bash "$target/deploy.sh" --check "$release_sha"
  cp "$root/valid-images.env" "$target/images.env"
  mv "$root/deploy/secrets/worker_internal_token" "$root/worker-token.tmp"
  expect_unchanged_failure 'missing secret' bash "$target/deploy.sh" --check "$release_sha"
  mv "$root/worker-token.tmp" "$root/deploy/secrets/worker_internal_token"
  chmod 644 "$root/deploy/secrets/worker_internal_token"
  expect_unchanged_failure 'world-readable secret' bash "$target/deploy.sh" --check "$release_sha"
  chmod 600 "$root/deploy/secrets/worker_internal_token"
  [[ "$(sha256sum "$root/deploy/secrets/"* | sha256sum)" == "$secret_before" ]] || fail 'Preflight failures changed credential bytes'
  printf '{"app":"acb"}\n' > "$root/registry/acb.json"
  expect_unchanged_failure 'shared failover registration' bash "$target/deploy.sh" --check "$release_sha"
  rm "$root/registry/acb.json"
  log_test_pass "Preflight negative checks passed"

  log_test_start "Baseline deploy and candidate promotion ($release_sha)"
  DEPLOY_PATH="$root" bash "$target/deploy.sh" --check "$release_sha"
  worker_events_since="$(date -u +'%Y-%m-%dT%H:%M:%S.%NZ')"
  DEPLOY_PATH="$root" bash "$target/deploy.sh" "$release_sha"
  [[ "$(sed -n 's/^RELEASE_SHA=//p' "$root/state.env")" == "$release_sha" ]] || fail 'Full deployment failed to commit SHA'
  [[ "$(sed -n 's/^GATEWAY_SLOT=//p' "$root/state.env")" == green ]] || fail 'Gateway did not flip to green'
  [[ "$(docker volume inspect --format '{{.Name}}:{{.CreatedAt}}' bank-event-gateway_gateway_data)" == "$gateway_volume_id" ]] || fail 'Gateway data volume changed during deployment'
  DEPLOY_PATH="$root" bash "$target/healthcheck.sh" route green "$release_sha"
  log_test_pass "Baseline deploy committed successfully to green slot"
  [[ "$(docker inspect -f '{{.State.Running}}' acb-recovery-controller 2>/dev/null || true)" != true ]] || fail 'Default-off deployment started recovery controller'


  log_test_start "Stale gateway service rejection"
  cp "$root/edge/dynamic/acb.yml" "$root/valid-acb.yml"
  python3 - "$root/edge/dynamic/acb.yml" <<'PY'
import os,sys,yaml
path=sys.argv[1]
with open(path) as stream: route=yaml.safe_load(stream)
route['http']['services']['acb-service']['loadBalancer']['servers']=[{'url':'http://acb-web-blue:8090'}]
tmp=path+'.fixture.tmp'
with open(tmp,'w') as stream: yaml.safe_dump(route,stream,sort_keys=False)
os.chmod(tmp,0o644)
os.replace(tmp,path)
PY
  stale_status=200
  for _ in {1..30}; do
    stale_status="$(docker run --rm --network container:edge-traefik curlimages/curl:8.12.1 -s -o /dev/null -w '%{http_code}' \
      -H 'Host: gateway-deploy.acb.internal.invalid' http://127.0.0.1:18080/readyz)" || stale_status=000
    [[ "$stale_status" != 200 ]] && break
    sleep 1
  done
  [[ "$stale_status" =~ ^50[234]$ ]] || fail "Traefik did not serve stale gateway upstream failure (HTTP $stale_status)"
  ack_rc=0
  timeout 68 bash "$target/healthcheck.sh" route green "$release_sha" >"$root/stale-route.log" 2>&1 || ack_rc=$?
  [[ "$ack_rc" != 0 && "$ack_rc" != 124 ]] || fail "Internal ACK did not reject stale gateway service (exit $ack_rc)"
  cp "$root/valid-acb.yml" "$root/edge/dynamic/.acb-fixture.tmp"
  chmod 644 "$root/edge/dynamic/.acb-fixture.tmp"
  mv "$root/edge/dynamic/.acb-fixture.tmp" "$root/edge/dynamic/acb.yml"
  DEPLOY_PATH="$root" bash "$target/healthcheck.sh" route green "$release_sha"
  log_test_pass "Stale gateway service rejection passed"

  log_test_start "Runtime service health, public endpoint and probe isolation checks"
  for name in acb-gateway-green acb-worker acb-tts-gateway acb-bark; do
    [[ "$(docker inspect --format '{{.State.Health.Status}}' "$name")" == healthy ]] || fail "$name unhealthy after deployment"
  done
  for name in acb-gateway-blue; do
    [[ "$(docker inspect --format '{{.State.Running}}' "$name")" == false ]] || fail "Retired container $name still running"
  done
  curl -fsS 'https://transactions.tuannguyenviet.site/api/public/v1/transactions?limit=1' | python3 -c 'import json,sys; page=json.load(sys.stdin); assert isinstance(page.get("items"),list) and len(page["items"])==1; assert isinstance(page.get("nextCursor"),str) and page["nextCursor"]; assert isinstance(page.get("summary"),dict)'
  for host in bank.tuannguyenviet.site transactions.tuannguyenviet.site; do
    for path in / /index.html /__release /assets/missing.js /t/fixture /health-anything /ready-anything; do
      [[ "$(docker run --rm --network container:edge-traefik curlimages/curl:8.12.1 -s -o /dev/null -w '%{http_code}' \
        -H "Host: $host" "http://127.0.0.1:8080$path")" == 404 ]] || fail "Origin serves unexpected static/catch-all path $host$path"
    done
  done
  [[ "$(docker run --rm --network container:edge-traefik curlimages/curl:8.12.1 -s -o /dev/null -w '%{http_code}' \
    -H 'Host: bank.tuannguyenviet.site' http://127.0.0.1:8080/admin/acb-credentials)" == 404 ]] || fail 'Origin still serves credentials SPA'
  [[ -z "$(docker ps -aq --filter name='^/acb-frontend-')" ]] || fail 'Backend deployment created frontend containers'
  [[ "$(docker run --rm --network edge-acb curlimages/curl:8.12.1 -s -o /dev/null -w '%{http_code}' \
    -H 'Host: gateway-deploy.acb.internal.invalid' http://edge-traefik:8080/readyz)" == 404 ]] || fail 'Internal deploy router exposed via public web'
  [[ "$(docker run --rm --network container:edge-traefik curlimages/curl:8.12.1 -s -o /dev/null -w '%{http_code}' \
    -H 'Host: gateway-deploy.acb.internal.invalid' http://127.0.0.1:18080/internal/private)" == 404 ]] || fail 'Internal gateway probe leaks private API'
  log_test_pass "Runtime service health and boundary isolation checks passed"
  python3 "$repo/deploy/tests/payment_ingress_fixture.py"

  log_test_start "Verified snapshot receipt, schema migration and sentinel check"
  receipt="$(find "$root/data/backups" -name receipt.json -print -quit)"
  [[ -n "$receipt" ]] || fail 'No verified snapshot receipt'
  python3 - "$receipt" <<'PY'
import hashlib,json,os,sqlite3,sys
r=json.load(open(sys.argv[1])); p=r['path']; assert os.stat(p).st_mode&0o777==0o600
assert hashlib.sha256(open(p,'rb').read()).hexdigest()==r['sha256']
directory=os.stat(os.path.dirname(p)); assert directory.st_mode&0o777==0o700
assert (directory.st_uid,directory.st_gid)==(1000,1000)
assert (os.stat(p).st_uid,os.stat(p).st_gid)==(1000,1000)
con=sqlite3.connect(f'file:{p}?mode=ro',uri=True)
assert con.execute('pragma integrity_check').fetchone()[0]=='ok'
assert con.execute('select max(version) from schema_migrations').fetchone()[0]==r['schema_version']
PY
  docker run --rm --network none --user 1000:1000 -v bank-event-gateway_gateway_data:/data:ro python:3.13-alpine \
    python -c 'import sqlite3; db=sqlite3.connect("file:/data/gateway.db?mode=ro",uri=True); assert db.execute("SELECT value FROM rehearsal_sentinel WHERE id=1").fetchone()[0] == "retain-after-migrate"'
  log_test_pass "Verified snapshot receipt and DB sentinel validated"

  log_test_start "No-op deployment rerun idempotency check"
  backup_count="$(find "$root/data/backups" -name receipt.json | wc -l)"
  state_before="$(sha256sum "$root/state.env")"
  route_before="$(sha256sum "$root/edge/dynamic/acb.yml")"
  worker_before="$(docker inspect --format '{{.Id}}' acb-worker)"
  DEPLOY_PATH="$root" bash "$target/deploy.sh" "$release_sha"
  [[ "$(docker inspect --format '{{.Id}}' acb-worker)" == "$worker_before" ]] || fail 'Same SHA restarted worker'
  [[ "$(find "$root/data/backups" -name receipt.json | wc -l)" == "$backup_count" ]] || fail 'Same SHA created another snapshot'
  [[ "$(sha256sum "$root/state.env")" == "$state_before" ]] || fail 'Same SHA rewrote committed state'
  [[ "$(sha256sum "$root/edge/dynamic/acb.yml")" == "$route_before" ]] || fail 'Same SHA rewrote app route'
  log_test_pass "No-op deployment rerun passed (no mutation)"
  log_test_start "Production-parity unconfigured runtime and retained secret hardening"
  for name in acb-worker acb-gateway-green; do
    docker inspect "$name" | python3 -c 'import json,sys; c=json.load(sys.stdin)[0]; assert c["HostConfig"]["ReadonlyRootfs"]; assert c["Config"]["User"]=="1000:1000"; assert "ALL" in c["HostConfig"]["CapDrop"]; assert "no-new-privileges:true" in c["HostConfig"]["SecurityOpt"]; mounts={m["Destination"]:m for m in c["Mounts"]}; assert all(not mounts["/run/secrets/"+n]["RW"] for n in ("app_master_key","worker_internal_token")); assert not any("/run/secrets/"+n in mounts for n in ("payos_client_id","payos_api_key","payos_checksum_key")); env=dict(x.split("=",1) for x in c["Config"]["Env"]); assert not any("AUTH_BROWSER" in k or "POLL_MIN" in k for k in env)'
  done
  config="$(docker run --rm --network container:edge-traefik curlimages/curl:8.12.1 -fsS -H 'Host: transactions.tuannguyenviet.site' http://127.0.0.1:8080/api/public/v1/payment-config)"
  printf '%s' "$config" | python3 -c 'import json,sys; c=json.load(sys.stdin); assert c["provider"]=="PAYOS" and c["ready"] is False and c["status"]=="UNCONFIGURED"'
  [[ -z "$(docker ps -aq --filter name='^/acb-auth-browser$' --filter name='^/acb-recovery-controller$')" ]] || fail 'Retired runtime containers created'
  log_test_pass "payOS runtime is hardened and cannot issue orders before confirmation"

  log_test_start "Deployment rollback check"
  DEPLOY_PATH="$root" bash "$target/deploy.sh" --rollback
  [[ "$(curl -s -o /dev/null -w '%{http_code}' 'https://transactions.tuannguyenviet.site/api/public/v1/transactions?limit=1')" == 200 ]] || fail 'Public transactions API fixture not healthy after rollback'
  [[ "$(sed -n 's/^RELEASE_SHA=//p' "$root/state.env")" == "$base_sha" ]] || fail 'Rollback did not restore baseline'
  [[ "$(docker inspect --format '{{.Config.Image}}' acb-worker)" == "$WORKER_IMAGE_REF" ]] || fail 'Rollback did not restore worker digest'
  [[ -z "$(docker ps -aq --filter name='^/acb-auth-browser$' --filter name='^/acb-recovery-controller$')" ]] || fail 'Compatible rollback created retired services'
  DEPLOY_PATH="$root" bash "$target/deploy.sh" --rollback
  log_test_pass "Deployment rollback and no-op rollback passed"

  state_before="$(sha256sum "$root/state.env")"
  route_before="$(sha256sum "$root/edge/dynamic/acb.yml")"
  worker_before="$(docker inspect --format '{{.Id}}' acb-worker)"

  log_test_start "Mutation gate admission blocks concurrent deployment"
  blocker="$(docker run --rm --network none --user 1000:1000 -v bank-event-gateway_gateway_data:/data "$DBTOOL_IMAGE_REF" -path /data/gateway.db -gate-acquire -owner rehearsal-blocker -reason rehearsal -lease-duration 15m)"
  blocker_token="$(printf '%s' "$blocker" | python3 -c 'import json,sys; print(json.load(sys.stdin)["leaseToken"])')"
  if bash "$target/deploy.sh" "$release_sha" >"$root/blocked-gate.log" 2>&1; then fail 'Locked gate admitted deployment'; fi
  [[ "$(sha256sum "$root/state.env")" == "$state_before" && "$(sha256sum "$root/edge/dynamic/acb.yml")" == "$route_before" && "$(docker inspect --format '{{.Id}}' acb-worker)" == "$worker_before" ]] || fail 'Rejected gate mutated runtime'
  docker run --rm --network none --user 1000:1000 -v bank-event-gateway_gateway_data:/data "$DBTOOL_IMAGE_REF" -path /data/gateway.db -gate-release -owner rehearsal-blocker -lease-token "$blocker_token"
  log_test_pass "Concurrent mutation gate preserves receiver and database"

  log_test_start "Schema migration failure and snapshot preservation check"
  backup_before_migration="$(find "$root/data/backups" -name receipt.json | wc -l)"
  migration_checksum="$(docker run --rm --network none --user 1000:1000 -v bank-event-gateway_gateway_data:/data python:3.13-alpine python -c '
import sqlite3
db=sqlite3.connect("/data/gateway.db")
checksum=db.execute("SELECT checksum FROM schema_migrations WHERE version=1").fetchone()[0]
db.execute("UPDATE schema_migrations SET checksum=? WHERE version=1",("rehearsal-invalid-checksum",))
db.commit()
print(checksum)')"
  migration_rc=0
  timeout 180 bash "$target/deploy.sh" "$release_sha" >"$root/migration-failure.log" 2>&1 || migration_rc=$?
  [[ "$migration_rc" != 0 && "$migration_rc" != 124 ]] || fail "Migration failure did not finish safely (exit $migration_rc)"
  [[ "$(sha256sum "$root/state.env")" == "$state_before" && "$(sha256sum "$root/edge/dynamic/acb.yml")" == "$route_before" ]] || fail 'Migration failure changed state or route'
  [[ "$(find "$root/data/backups" -name receipt.json | wc -l)" -gt "$backup_before_migration" ]] || fail 'Migration failure did not preserve pre-migration snapshot'
  docker run --rm --network none --user 1000:1000 -v bank-event-gateway_gateway_data:/data python:3.13-alpine python -c '
import sqlite3,sys
db=sqlite3.connect("/data/gateway.db")
assert db.execute("SELECT value FROM rehearsal_sentinel WHERE id=1").fetchone()[0] == "retain-after-migrate"
db.execute("UPDATE schema_migrations SET checksum=? WHERE version=1",(sys.argv[1],))
db.commit()' "$migration_checksum"
  log_test_pass "Schema migration failure safety passed"

  log_test_start "Unpullable image ref prompt failure check"
  unpullable_sha=ffffffffffffffffffffffffffffffffffffffff
  unpullable="$root/releases/$unpullable_sha"
  mkdir "$unpullable"
  cp -a "$target/." "$unpullable/"
  rm "$unpullable/SHA256SUMS"
  python3 - "$target/images.env" "$unpullable/images.env" "$unpullable_sha" <<'PY'
import sys
src,dst,sha=sys.argv[1:]
text=open(src).read()
text='\n'.join(('RELEASE_SHA='+sha if line.startswith('RELEASE_SHA=') else 'WORKER_IMAGE_REF=ghcr.io/thedemontuan/acb-transaction-webhook-worker@sha256:'+'f'*64 if line.startswith('WORKER_IMAGE_REF=') else line) for line in text.splitlines())+'\n'
open(dst,'w').write(text)
PY
  (cd "$unpullable" && sha256sum deploy.sh simple-lib.sh healthcheck.sh render-route.sh backup-db.sh restore-db.sh backup-secrets.sh migrate-payos-runtime.sh migrate-payos-runtime.py workflow-runtime.py acb-route-publish.py install-route-publisher.sh verify-frontend.py compose.prod.yaml bark-entrypoint.sh images.env source-run.env > SHA256SUMS)
  pull_rc=0
  timeout 180 bash "$unpullable/deploy.sh" "$unpullable_sha" >"$root/image-pull.log" 2>&1 || pull_rc=$?
  [[ "$pull_rc" != 0 && "$pull_rc" != 124 ]] || fail "Missing image did not fail promptly (exit $pull_rc)"
  [[ "$(sha256sum "$root/state.env")" == "$state_before" && "$(sha256sum "$root/edge/dynamic/acb.yml")" == "$route_before" && "$(docker inspect --format '{{.Id}}' acb-worker)" == "$worker_before" ]] || fail 'Image pull failure mutated runtime'
  log_test_pass "Unpullable image failure prompt rejection passed"

  log_test_start "Retire failure and retry recovery check"
  retire_rc=0
  DOCKER_FAULT_MODE=retire-failure timeout 240 bash "$target/deploy.sh" "$release_sha" >"$root/retire-failure.log" 2>&1 || retire_rc=$?
  [[ "$retire_rc" != 0 && "$retire_rc" != 124 ]] || fail "Retire failure did not report error ($retire_rc)"
  [[ -d "$root/.deploy-pending" && "$(sed -n 's/^RELEASE_SHA=//p' "$root/state.env")" == "$release_sha" ]] || fail 'Retire failure lost committed state/evidence'
  docker unpause acb-gateway-blue >/dev/null
  timeout 240 bash "$target/deploy.sh" "$release_sha" >"$root/retire-retry.log" 2>&1
  [[ ! -d "$root/.deploy-pending" ]] || fail 'Retire retry left pending evidence'
  [[ "$(docker inspect -f '{{.State.Running}}' acb-gateway-blue)" == false ]] || fail 'Retire retry left old gateway running'
  [[ "$(sed -n 's/^RELEASE_SHA=//p' "$root/state.env")" == "$release_sha" ]] || fail 'Redeploy after rollback did not commit target'
  DEPLOY_PATH="$root" bash "$target/deploy.sh" "$release_sha"
  [[ "$(sed -n 's/^GATEWAY_SLOT=//p' "$root/state.env")" == green ]] || fail 'Post-rollback no-op changed gateway slot'
  log_test_pass "Retire failure and retry recovery passed"

  log_test_start "Worker singleton events verification"
  worker_events_until="$(date -u +'%Y-%m-%dT%H:%M:%S.%NZ')"
  timeout 20 docker events --since "$worker_events_since" --until "$worker_events_until" \
    --filter container=acb-worker --format '{{.Action}}' >"$root/worker-events.log"
  python3 - "$root/worker-events.log" <<'PY'
import sys
events=open(sys.argv[1]).read().splitlines()
assert 'start' in events and 'die' in events, ('missing worker handoff events',events)
running=1
for event in events:
    if event=='die':
        assert running==1, ('worker died without running',events)
        running=0
    elif event=='start':
        assert running==0, ('singleton overlap',events)
        running=1
assert running==1, ('worker not running',events)
PY
  log_test_pass "Worker singleton lifecycle events verified"

  log_test_start "Unrelated Traefik route preservation check"
  (cd "$root/edge/dynamic" && sha256sum -c "$root/unrelated.sha256") || fail 'Deployment modified unrelated Traefik files'
  log_test_pass "Unrelated Traefik dynamic routes preserved"
  log_step "Suite lifecycle completed successfully."
}

init_fault_matrix_baseline() {
  local cur_sha
  [[ -f "$root/state.env" ]] || fail 'Fault matrix requires canonical baseline state'
  cur_sha="$(sed -n 's/^RELEASE_SHA=//p' "$root/state.env" 2>/dev/null || echo '')"
  if [[ "$cur_sha" != "$base_sha" ]]; then
    log_step "Initializing fault-matrix baseline (deploy candidate followed by rollback)..."
    DEPLOY_PATH="$root" bash "$target/deploy.sh" --rollback >"$root/fault-init-rollback.log" 2>&1 || fail "Fault baseline rollback failed"
  fi
  state_before="$(sha256sum "$root/state.env")"
  route_before="$(sha256sum "$root/edge/dynamic/acb.yml")"
  aux_before="$(docker inspect -f '{{.Id}}' acb-tts-gateway acb-bark)"
  worker_before="$(docker inspect --format '{{.Id}}' acb-worker)"
  log_step "Fault-matrix baseline established: $base_sha (blue slot active)."
}

run_suite_fault_matrix() {
  log_step "Starting suite: fault-matrix"
  init_fault_matrix_baseline

  for mode in corrupt-backup quiesce-timeout legacy-worker-capabilities candidate-missing-payment-drain quiesce-active-payments quiesce-active-deliveries quiesce-dispatcher-active quiesce-invalid-journal quiesce-legacy quiesce-counter-boolean unhealthy-worker wrong-gateway wrong-gateway-slot stale-service; do
    log_test_start "Fault matrix test: $mode"
    if [[ "$mode" == quiesce-timeout ]]; then
      log_test_note "Executing quiesce-timeout test (sleep 50 in worker quiesce, outer timeout 480)"
    fi
    if [[ "$mode" == wrong-gateway ]]; then
      printf 'services:\n  gateway-green:\n    environment:\n      RELEASE_COMMIT: %s\n' "$base_sha" > "$root/wrong-gateway.yaml"
    fi
    if [[ "$mode" == wrong-gateway-slot ]]; then
      printf 'services:\n  gateway-green:\n    environment:\n      PLATFORM_SLOT: blue\n' > "$root/wrong-gateway.yaml"
    fi
    worker_before_fault="$(docker inspect --format '{{.Id}}' acb-worker)"
    fault_rc=0
    fault_started=$SECONDS
    DOCKER_FAULT_MODE="$mode" WRONG_GATEWAY_OVERRIDE="$root/wrong-gateway.yaml" IDENTITY_FAULT_MARKER="$root/$mode.injected" STALE_SERVICE_APPLIED="$root/stale-service-applied" \
      timeout 480 bash "$target/deploy.sh" "$release_sha" >"$root/$mode.log" 2>&1 || fault_rc=$?
    if [[ "$fault_rc" == 124 && $((SECONDS-fault_started)) -ge 480 ]]; then
      python3 - "$root/$mode.log" <<'PY'
import sys
for line in open(sys.argv[1], errors='replace'):
    if any(phrase in line for phrase in ('deploy ERROR:', 'healthcheck:', 'container timeout:', 'route ACK:', 'worker rpc returned status')):
        print(line.rstrip(), file=sys.stderr)
PY
      fail "$mode exceeded outer deployment deadline"
    fi
    [[ "$fault_rc" != 0 ]] || fail "$mode unexpectedly succeeded"
    if [[ "$mode" == wrong-gateway || "$mode" == wrong-gateway-slot ]]; then [[ -f "$root/$mode.injected" ]] || fail "$mode never reached candidate service"; fi
    case "$mode" in
      legacy-worker-capabilities|candidate-missing-payment-drain|quiesce-active-payments|quiesce-active-deliveries|quiesce-dispatcher-active|quiesce-invalid-journal|quiesce-legacy|quiesce-counter-boolean)
        [[ -f "$root/$mode.injected" ]] || fail "$mode never reached the worker contract boundary"
        [[ "$(docker inspect --format '{{.Id}}' acb-worker)" == "$worker_before_fault" ]] || fail "$mode replaced the worker despite an invalid drain contract"
        ;;
    esac
    if [[ "$mode" == stale-service ]]; then [[ -f "$root/stale-service-applied" ]] || fail 'Stale service fault never reached Traefik ACK'; fi
    if [[ "$mode" == corrupt-backup ]]; then
      [[ "$(docker inspect -f '{{.Id}}' acb-tts-gateway acb-bark)" == "$aux_before" ]] || fail 'Backup corruption restarted auxiliaries'
    fi
    [[ ! -d "$root/.deploy-pending" ]] || fail "$mode left recoverable deployment pending"
    [[ "$(sha256sum "$root/state.env")" == "$state_before" ]] || fail "$mode committed wrong state"
    [[ "$(sha256sum "$root/edge/dynamic/acb.yml")" == "$route_before" ]] || fail "$mode did not restore route"
    [[ "$(docker inspect -f '{{.State.Health.Status}}' acb-worker)" == healthy ]] || fail "$mode left worker unhealthy"
    [[ "$(docker inspect -f '{{.Config.Image}}' acb-worker)" == "$WORKER_IMAGE_REF" ]] || fail "$mode lost original worker digest"
    [[ "$(docker inspect -f '{{.State.Running}}' acb-gateway-blue)" == true ]] || fail "$mode stopped serving gateway"
    log_test_pass "Fault matrix test passed: $mode"
  done

  for signal in TERM KILL; do
    log_test_start "INTENTIONAL FAULT TEST: Interruption via SIG$signal before-worker switch"
    log_test_note "Simulating sudden process death via SIG$signal. Any 'Killed' or termination log here is an INTENTIONAL FAULT INJECTION, NOT a crash."
    marker="$root/fault-$signal.marker" release="$root/fault-$signal.release"
    DOCKER_FAULT_MODE=before-worker DOCKER_FAULT_MARKER="$marker" DOCKER_FAULT_RELEASE="$release" \
      timeout 240 bash "$target/deploy.sh" "$release_sha" >"$root/interrupted-$signal.log" 2>&1 &
    deploy_pid=$!
    active_deploy_pid="$deploy_pid"
    wait_marker "$marker" "$deploy_pid"
    [[ "$(sha256sum "$root/state.env")" == "$state_before" ]] || fail "$signal committed before interruption"
    [[ "$(sha256sum "$root/edge/dynamic/acb.yml")" != "$route_before" ]] || fail "$signal did not reach switched route"
    deploy_child="$(ps -o pid= --ppid "$deploy_pid" | xargs)"
    [[ "$deploy_child" =~ ^[0-9]+$ ]] || fail "Cannot identify deploy process for $signal"
    log_step "Injecting INTENTIONAL SIG$signal to deploy child PID $deploy_child..."
    kill -s "$signal" "$deploy_child"
    if [[ "$signal" == KILL ]]; then
      kill -KILL "$(cat "$marker")" 2>/dev/null || :
    else
      : > "$release"
    fi
    interrupt_rc=0
    wait "$deploy_pid" 2>/dev/null || interrupt_rc=$?
    active_deploy_pid=''; marker=''
    log_step "Process terminated as expected by INTENTIONAL SIG$signal (exit code: $interrupt_rc)"
    [[ "$interrupt_rc" != 0 && "$interrupt_rc" != 124 ]] || fail "$signal interruption did not exit safely ($interrupt_rc)"
    if [[ "$signal" == KILL ]]; then
      [[ -d "$root/.deploy-pending" ]] || fail 'SIGKILL lost pending recovery evidence'
      log_step "Validating automatic recovery from intentional SIGKILL interruption..."
      timeout 240 bash "$target/deploy.sh" "$release_sha" >"$root/recovery-kill.log" 2>&1
      [[ "$(sed -n 's/^RELEASE_SHA=//p' "$root/state.env")" == "$release_sha" ]] || fail 'SIGKILL recovery did not deploy target'
      bash "$target/deploy.sh" --rollback >"$root/recovery-rollback.log" 2>&1
    fi
    verify_baseline "$signal recovery"
    log_test_pass "INTENTIONAL FAULT TEST: Interruption via SIG$signal before-worker switch and recovery passed"
  done

  log_test_start "INTENTIONAL FAULT TEST: Interruption via SIGKILL after-commit"
  log_test_note "Simulating crash via SIGKILL immediately after state commit. Any 'Killed' log here is an INTENTIONAL FAULT INJECTION, NOT a crash."
  marker="$root/postcommit.marker" release="$root/postcommit.release"
  DOCKER_FAULT_MODE=after-commit DOCKER_FAULT_MARKER="$marker" DOCKER_FAULT_RELEASE="$release" \
    timeout 240 bash "$target/deploy.sh" "$release_sha" >"$root/postcommit.log" 2>&1 &
  deploy_pid=$!
  active_deploy_pid="$deploy_pid"
  wait_marker "$marker" "$deploy_pid"
  [[ "$(sed -n 's/^RELEASE_SHA=//p' "$root/state.env")" == "$release_sha" ]] || fail 'Post-commit fault preceded state commit'
  deploy_child="$(ps -o pid= --ppid "$deploy_pid" | xargs)"
  [[ "$deploy_child" =~ ^[0-9]+$ ]] || fail 'Cannot identify post-commit deploy process'
  log_step "Injecting INTENTIONAL SIGKILL to deploy child PID $deploy_child and marker..."
  kill -KILL "$deploy_child" "$(cat "$marker")"
  postcommit_rc=0
  wait "$deploy_pid" 2>/dev/null || postcommit_rc=$?
  active_deploy_pid=''; marker=''
  log_step "Process terminated as expected by INTENTIONAL SIGKILL (exit code: $postcommit_rc)"
  [[ "$postcommit_rc" != 0 ]] || fail 'SIGKILL after commit returned success'
  [[ -d "$root/.deploy-pending" ]] || fail 'SIGKILL after commit lost pending snapshot'
  log_step "Validating recovery from intentional post-commit SIGKILL..."
  timeout 240 bash "$target/deploy.sh" "$release_sha" >"$root/postcommit-recovery.log" 2>&1
  [[ ! -d "$root/.deploy-pending" && -d "$root/rollback" ]] || fail 'Committed recovery did not finish retire'
  [[ "$(sed -n 's/^RELEASE_SHA=//p' "$root/state.env")" == "$release_sha" ]] || fail 'Committed recovery changed release'
  for name in acb-gateway-blue; do
    [[ "$(docker inspect -f '{{.State.Running}}' "$name")" == false ]] || fail "Committed recovery left $name running"
  done
  log_test_pass "INTENTIONAL FAULT TEST: Interruption via SIGKILL after-commit and recovery passed"

  log_test_start "Rollback ACK outage test (Traefik stopped during rollback)"
  docker stop edge-traefik >/dev/null
  rollback_ack_rc=0
  timeout 150 bash "$target/deploy.sh" --rollback >"$root/rollback-ack-outage.log" 2>&1 || rollback_ack_rc=$?
  [[ "$rollback_ack_rc" != 0 && "$rollback_ack_rc" != 124 ]] || fail "Rollback ACK outage did not fail boundedly ($rollback_ack_rc)"
  [[ "$(cat "$root/rollback-ack-outage.log")" == *ROLLBACK_FAILED* ]] || fail 'Rollback ACK outage omitted ROLLBACK_FAILED evidence'
  [[ "$(sed -n 's/^RELEASE_SHA=//p' "$root/state.env")" == "$release_sha" ]] || fail 'Rollback ACK outage changed committed state'
  for name in acb-gateway-blue acb-gateway-green; do
    [[ "$(docker inspect -f '{{.State.Running}}' "$name")" == true ]] || fail "Rollback ACK outage stopped $name"
  done
  docker start edge-traefik >/dev/null
  bash "$target/deploy.sh" --rollback >"$root/postcommit-rollback.log" 2>&1
  verify_baseline 'post-commit recovery'
  log_test_pass "Rollback ACK outage test and recovery passed"

  log_test_start "Unrelated Traefik route preservation check across fault matrix"
  (cd "$root/edge/dynamic" && sha256sum -c "$root/unrelated.sha256") || fail 'Deployment modified unrelated Traefik files'
  log_test_pass "Unrelated Traefik dynamic routes preserved across fault matrix"
  log_step "Suite fault-matrix completed successfully."
}

case "$target_suite" in
  lifecycle)
    run_suite_lifecycle
    printf '[%s] [REHEARSAL] PASS: suite lifecycle (baseline deploy, verified snapshot, no-op rerun, rollback, active auth, migration failure, image pull, retire failure, worker events, unrelated routes)\n' "$(log_ts)"
    ;;
  fault-matrix)
    run_suite_fault_matrix
    printf '[%s] [REHEARSAL] PASS: suite fault-matrix (corrupt-backup, quiesce-timeout, incompatible drain capabilities/replies, unhealthy-worker, wrong-gateway, wrong-gateway-slot, stale-service, TERM/KILL before-worker and recovery, KILL after-commit and recovery, rollback-ack-outage, unrelated routes)\n' "$(log_ts)"
    ;;
  all)
    run_suite_lifecycle
    run_suite_fault_matrix
    printf '[%s] [REHEARSAL] PASS: all suites completed successfully (lifecycle + fault-matrix)\n' "$(log_ts)"
    ;;
esac
