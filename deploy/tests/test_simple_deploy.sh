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
[[ "${1:-}" == --images-env && $# == 4 && "$3" == --bundle && "$2" == /* && "$4" == /* ]] || fail 'Usage: test_simple_deploy.sh --images-env /absolute/images.env --bundle /absolute/bundle'
images_env="$2"; bundle="$4"
[[ -f "$images_env" && -f "$bundle/deploy.sh" && -f "$bundle/import-baseline.py" && -f "$bundle/compose.prod.yaml" ]] || fail 'Missing images.env or complete bundle'
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
  git -C "$repo" cat-file -e a6bc2a4739c7cd4776188d30d71cba6ecb79b970^{commit} || fail 'Missing historical baseline commit; checkout must fetch full history'
  archive_dir="$(mktemp -d "${TMPDIR:-/tmp}/acb-history.XXXXXXXX")"
  git -C "$repo" archive a6bc2a4739c7cd4776188d30d71cba6ecb79b970 deploy/compose deploy/bark-entrypoint.sh deploy/seccomp-auth-browser.json > "$archive_dir/baseline.tar"
  cp "$repo/platform/edge/dynamic/acb.yml" "$archive_dir/acb.yml"
  cp "$repo/deploy/tests/simple_deploy_https_fixture.py" "$archive_dir/https_fixture.py"
  chmod 755 "$archive_dir"
  chmod 644 "$archive_dir/"*
  export REHEARSAL_ARCHIVE="$archive_dir/baseline.tar" REHEARSAL_ROUTE_SOURCE="$archive_dir/acb.yml" REHEARSAL_HTTPS_FIXTURE="$archive_dir/https_fixture.py"
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
  sudo -E -u "$account" -g '#1000' bash "$0" "$@" || uid_rc=$?
  rm -rf -- "$archive_dir"
  sudo rm -rf -- "$auth_dir"
  exit "$uid_rc"
fi
base_sha=a6bc2a4739c7cd4776188d30d71cba6ecb79b970
if [[ -z "${REHEARSAL_ARCHIVE:-}" ]]; then
  git -C "$repo" -c safe.directory="$repo" cat-file -e "$base_sha^{commit}" || fail "Missing historical baseline commit: $base_sha"
else
  [[ -s "$REHEARSAL_ARCHIVE" ]] || fail 'Historical fixture archive unavailable to UID 1000'
fi
root="$(mktemp -d "${TMPDIR:-/tmp}/acb-rehearsal.XXXXXXXX")"
cleanup() {
  rc=$?
  trap - EXIT
  if [[ "$rc" != 0 ]]; then printf 'Rehearsal failed; fixture root: %s\n' "$root" >&2; fi
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
mkdir -p "$root/releases/rel-$base_sha-fixture/compose" "$root/state" "$root/data/backups" "$root/deploy/secrets" "$root/edge/dynamic"
# The runner is UID 1000; Docker-mounted secrets and backup directory are owner-only.
chmod 700 "$root/deploy/secrets" "$root/data/backups"
legacy="$root/releases/rel-$base_sha-fixture"
if [[ -n "${REHEARSAL_ARCHIVE:-}" ]]; then
  tar -xf "$REHEARSAL_ARCHIVE" -C "$root"
else
  git -C "$repo" -c safe.directory="$repo" archive "$base_sha" deploy/compose deploy/bark-entrypoint.sh deploy/seccomp-auth-browser.json | tar -x -C "$root"
fi
cp "$root/deploy/compose/"*.yaml "$legacy/compose/"
cp "$root/deploy/bark-entrypoint.sh" "$root/deploy/seccomp-auth-browser.json" "$legacy/"
chmod 644 "$legacy/bark-entrypoint.sh"
# Bundle must execute from releases/<sha>, not from a staging directory.
release_sha="$(python3 - "$images_env" <<'PY'
import re,sys
values={}
for line in open(sys.argv[1],encoding='utf8'):
    if not line.strip(): continue
    key,sep,value=line.strip().partition('=')
    assert sep and key not in values and value and re.fullmatch(r'[A-Z_]+',key),(key,value)
    values[key]=value
assert set(values)=={'RELEASE_SHA','GATEWAY_IMAGE_REF','FRONTEND_IMAGE_REF','WORKER_IMAGE_REF','DBTOOL_IMAGE_REF','BROWSER_IMAGE_REF','TTS_IMAGE_REF','BARK_IMAGE_REF'}
assert re.fullmatch('[a-f0-9]{40}',values['RELEASE_SHA'])
for key,value in values.items():
    if key.endswith('_IMAGE_REF'): assert re.fullmatch(r'[^\s@]+@sha256:[a-f0-9]{64}',value),(key,value)
print(values['RELEASE_SHA'])
PY
)"
target="$root/releases/$release_sha"
mkdir -p "$target"
cp "$bundle/"{deploy.sh,simple-lib.sh,healthcheck.sh,render-route.sh,backup-db.sh,compose.prod.yaml,seccomp-auth-browser.json,bark-entrypoint.sh,import-baseline.py} "$target/"
chmod 644 "$target/bark-entrypoint.sh"
cp "$images_env" "$target/images.env"
[[ ! -f "$bundle/SHA256SUMS" ]] || cp "$bundle/SHA256SUMS" "$target/"
# Synthetic credentials: never mount production secrets or use production sessions.
python3 - "$root" <<'PY'
import os,secrets,sys
root=sys.argv[1]
for name in ('worker_internal_token','auth_browser_internal_token','tts_internal_token','bark_basic_auth_user','bark_basic_auth_password','app_master_key'):
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
EOF
chmod 600 "$root/deploy/.env.production"
ln -s "$root/deploy/.env.production" "$legacy/.env.production"
ln -s "$root/deploy/secrets" "$legacy/secrets"
for name in edge-acb acb-core acb-egress; do
  if [[ "$name" == acb-core ]]; then docker network create --internal "$name" >/dev/null; else docker network create "$name" >/dev/null; fi
done
cp "${REHEARSAL_ROUTE_SOURCE:-$repo/platform/edge/dynamic/acb.yml}" "$root/edge/dynamic/acb.yml"
if ACB_ROUTE_FILE="$root/edge/dynamic/acb.yml" FAILOVER_REGISTRY_DIR="$root/registry" DEPLOY_PATH="$root" \
  bash "$target/deploy.sh" --check "$release_sha" >"$root/missing-volume.log" 2>&1; then
  fail 'Missing gateway data volume did not block preflight'
fi
[[ ! -e "$root/state.env" ]] || fail 'Missing-volume preflight wrote state'
for name in bank-event-gateway_gateway_data bank-event-gateway_bark_data; do docker volume create "$name" >/dev/null; done
gateway_volume_id="$(docker volume inspect --format '{{.Name}}:{{.CreatedAt}}' bank-event-gateway_gateway_data)"
# The historical image identities are fixtures, never resolved from a production volume.
cat > "$root/base-images.env" <<'EOF'
GATEWAY_IMAGE_REF=ghcr.io/thedemontuan/acb-transaction-webhook@sha256:95c6bc22a74b7027e3af1ef49499b1159dfc542ddf228216f2d694c99d488569
WORKER_IMAGE_REF=ghcr.io/thedemontuan/acb-transaction-webhook-worker@sha256:a948557ff7f56cb7ed2344e6986b4e855a251770d07afd69395d1e449b0c8152
FRONTEND_IMAGE_REF=ghcr.io/thedemontuan/acb-transaction-webhook-frontend@sha256:cef773c35a1936948fe4848fc5b00e679c6c5e3454f30b0fd91a67bcf4238714
DBTOOL_IMAGE_REF=ghcr.io/thedemontuan/acb-transaction-webhook-dbtool@sha256:89ab28eae8fabe777a686d7035667eefd1cd226ec19292986e1cbde8e56906ab
BROWSER_IMAGE_REF=ghcr.io/thedemontuan/acb-transaction-webhook-auth-browser@sha256:f79d5ca60ba93512caf8902f02d8c2bda4c0d220865ad9f08c4ac75dae8058bc
TTS_IMAGE_REF=ghcr.io/thedemontuan/acb-transaction-webhook-tts-gateway@sha256:27edff9e9b1f1019dda85fa31de9b04649e1f5553d59d1af3c65ee0c1be447d3
BARK_IMAGE_REF=ghcr.io/finb/bark-server@sha256:32d65b07fa835c99b31a396b77727a04ed058377fc2482da3e9dc7397167ffc4
EOF
set -a
# Whitelisted constant fixture, not remotely supplied configuration.
source "$root/base-images.env"
{ cat "$root/base-images.env"; printf 'IMAGE_REF_BLUE=%s\nIMAGE_REF_GREEN=%s\nRELEASE_COMMIT=%s\nBARK_SECRET_GROUP=1000\n' "$GATEWAY_IMAGE_REF" "$GATEWAY_IMAGE_REF" "$base_sha"; } > "$legacy/.release.env"
set +a
for ref in "$GATEWAY_IMAGE_REF" "$WORKER_IMAGE_REF" "$FRONTEND_IMAGE_REF" "$DBTOOL_IMAGE_REF" "$BROWSER_IMAGE_REF" "$TTS_IMAGE_REF" "$BARK_IMAGE_REF"; do docker pull --platform linux/arm64 "$ref" >/dev/null; done
docker pull busybox:1.37.0 >/dev/null
docker run --rm --network none -v bank-event-gateway_gateway_data:/data busybox:1.37.0 chown 1000:1000 /data
# Seed the fixture DB using the real historical dbtool and named volume.
docker run --rm --network none --user 1000:1000 -v bank-event-gateway_gateway_data:/data "$DBTOOL_IMAGE_REF" -path /data/gateway.db -migrate
docker pull python:3.13-alpine >/dev/null
docker run --rm --network none --user 1000:1000 -v bank-event-gateway_gateway_data:/data python:3.13-alpine \
  python -c 'import sqlite3; db=sqlite3.connect("/data/gateway.db"); db.execute("CREATE TABLE rehearsal_sentinel (id INTEGER PRIMARY KEY, value TEXT NOT NULL)"); db.execute("INSERT INTO rehearsal_sentinel VALUES (1, ?)", ("retain-after-migrate",)); db.execute("INSERT INTO connections(id,state,created_at,updated_at) VALUES (?,?,?,?)", ("rehearsal-connection","UNCONFIGURED","2026-01-01T00:00:00Z","2026-01-01T00:00:00Z")); db.commit()'
docker compose --project-name acb --project-directory "$legacy" --env-file "$root/deploy/.env.production" --env-file "$legacy/.release.env" \
  -f "$legacy/compose/base.yaml" -f "$legacy/compose/gateway.yaml" -f "$legacy/compose/frontend.yaml" \
  -f "$legacy/compose/worker.yaml" -f "$legacy/compose/auth-browser.yaml" -f "$legacy/compose/tts.yaml" \
  -f "$legacy/compose/bark.yaml" up -d --no-deps gateway-blue frontend-blue worker auth-browser tts-gateway bark

for service in gateway-blue frontend-blue worker auth-browser tts-gateway bark; do
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
cp "${REHEARSAL_ROUTE_SOURCE:-$repo/platform/edge/dynamic/acb.yml}" "$root/edge/dynamic/acb.yml"
python3 - "$root/edge/dynamic/acb.yml" <<'PY'
import sys,yaml
path=sys.argv[1]
with open(path) as f:data=yaml.safe_load(f)
data['http']['services']['acb-frontend-service']['loadBalancer']['servers']=[{'url':'http://acb-frontend-blue:8080'}]
with open(path,'w') as f:yaml.safe_dump(data,f,sort_keys=False)
PY
cat > "$root/edge/dynamic/middlewares.yml" <<'EOF'
http:
  middlewares:
    tunnel-only:
      ipAllowList:
        sourceRange: ["192.0.2.1/32"]
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
  wrong-frontend)
    if [[ " $* " == *' up -d --no-deps frontend-green '* ]]; then
      args=("$@")
      for ((i=0; i<${#args[@]}; i++)); do
        if [[ "${args[i]}" == up ]]; then
          exec "$REAL_DOCKER" "${args[@]:0:i}" -f "${WRONG_FRONTEND_OVERRIDE:?}" "${args[@]:i}"
        fi
      done
    fi ;;
  wrong-gateway)
    if [[ " $* " == *' up -d --no-deps gateway-green '* ]]; then
      python3 - "${WRONG_GATEWAY_RUNTIME:?}" "${WRONG_GATEWAY_SHA:?}" <<'PY'
import pathlib,sys
p=pathlib.Path(sys.argv[1]); lines=p.read_text().splitlines()
assert sum(s.startswith('RELEASE_COMMIT_GREEN=') for s in lines)==1
p.write_text('\n'.join('RELEASE_COMMIT_GREEN='+sys.argv[2] if s.startswith('RELEASE_COMMIT_GREEN=') else s for s in lines)+'\n')
PY
      "$REAL_DOCKER" "$@"
      exit 0
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
    if [[ " $* " == *' --network container:edge-traefik '* && " $* " == *' frontend-deploy.acb.internal.invalid '* && ! -e "${STALE_SERVICE_APPLIED:?}" ]]; then
      : > "$STALE_SERVICE_APPLIED"
      python3 - "$ACB_ROUTE_FILE" <<'PY'
import os,sys,yaml
p=sys.argv[1]
with open(p) as f: data=yaml.safe_load(f)
data['http']['services']['acb-frontend-service']['loadBalancer']['servers']=[{'url':'http://acb-frontend-blue:8080'}]
t=p+'.stale-service.tmp'
with open(t,'w') as f: yaml.safe_dump(data,f,sort_keys=False)
os.chmod(t,0o644);os.replace(t,p)
PY
      ready=0
      for ((n=0;n<20;n++)); do
        code="$("$REAL_DOCKER" run --rm --network container:edge-traefik curlimages/curl:8.12.1 -s -o /dev/null -w '%{http_code}' -H 'Host: frontend-deploy.acb.internal.invalid' http://127.0.0.1:18080/__release)" || code=000
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
for _ in {1..30}; do
  if "$real_curl" -fsS --cacert "$FIXTURE_CA" --connect-to bank.tuannguyenviet.site:443:127.0.0.1:19443 \
    -o /dev/null https://bank.tuannguyenviet.site/; then break; fi
  kill -0 "$https_pid" 2>/dev/null || fail 'Fixture HTTPS server exited'
  sleep 1
done
[[ "$(docker run --rm --network edge-acb curlimages/curl:8.12.1 -s -o /dev/null -w '%{http_code}' \
    -H 'Host: transactions.tuannguyenviet.site' http://edge-traefik:8080/)" == 403 ]] || fail 'Public web did not reject fixture source with 403'
cat > "$root/state/current-release.json" <<EOF
{"schema_version":2,"status":"COMPLETED","git_sha":"$base_sha","release_dir":"$legacy","active_slots":{"gateway":"blue","frontend":"blue"},"images":{"gateway":{"blue":"$GATEWAY_IMAGE_REF","green":"$GATEWAY_IMAGE_REF"},"frontend":"$FRONTEND_IMAGE_REF","worker":"$WORKER_IMAGE_REF","auth_browser":"$BROWSER_IMAGE_REF","tts":"$TTS_IMAGE_REF","bark":"$BARK_IMAGE_REF","dbtool":"$DBTOOL_IMAGE_REF"}}
EOF
for path in /api/private /internal /admin /readyz; do
  code="$(docker run --rm --network edge-acb curlimages/curl:8.12.1 -s -o /dev/null -w '%{http_code}' \
    -H 'Host: transactions.tuannguyenviet.site' "http://edge-traefik:8080$path")"
  [[ "$code" == 403 ]] || fail "Public private path $path unexpectedly returned $code"
done
[[ "$(docker run --rm --network edge-acb curlimages/curl:8.12.1 -s -o /dev/null -w '%{http_code}' \
  -H 'Host: gateway-deploy.acb.internal.invalid' http://edge-traefik:8080/readyz)" == 404 ]] || fail 'Internal probe host exposed on public web'
old_route_sha="$(sha256sum "$root/edge/dynamic/acb.yml")"
old_worker_id="$(docker inspect --format '{{.Id}}' acb-worker)"
secret_before="$(sha256sum "$root/deploy/secrets/"* | sha256sum)"
expect_unchanged_failure() {
  local label="$1"; shift
  if "$@" >"$root/negative-output.log" 2>&1; then fail "$label unexpectedly succeeded"; fi
  [[ ! -e "$root/state.env" ]] || fail "$label created state.env"
  [[ "$(sha256sum "$root/edge/dynamic/acb.yml")" == "$old_route_sha" ]] || fail "$label changed live route"
  [[ "$(docker inspect --format '{{.Id}}' acb-worker)" == "$old_worker_id" ]] || fail "$label recreated worker"
  printf 'PASS: %s fails without route/state/worker mutation\n' "$label"
}
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
# The deployment itself must import legacy state and prove ACK against actual Traefik.
DEPLOY_PATH="$root" bash "$target/deploy.sh" --check "$release_sha"
worker_events_since="$(date -u +'%Y-%m-%dT%H:%M:%S.%NZ')"
DEPLOY_PATH="$root" bash "$target/deploy.sh" "$release_sha"
[[ "$(sed -n 's/^RELEASE_SHA=//p' "$root/state.env")" == "$release_sha" ]] || fail 'Full deployment failed to commit SHA'
[[ "$(sed -n 's/^GATEWAY_SLOT=//p' "$root/state.env")" == green ]] || fail 'Gateway did not flip to green'
[[ "$(docker volume inspect --format '{{.Name}}:{{.CreatedAt}}' bank-event-gateway_gateway_data)" == "$gateway_volume_id" ]] || fail 'Gateway data volume changed during deployment'
[[ "$(sed -n 's/^FRONTEND_SLOT=//p' "$root/state.env")" == green ]] || fail 'Frontend did not flip to green'
DEPLOY_PATH="$root" bash "$target/healthcheck.sh" route green "$release_sha" "$release_sha"
cp "$root/edge/dynamic/acb.yml" "$root/valid-acb.yml"
python3 - "$root/edge/dynamic/acb.yml" <<'PY'
import os,sys,yaml
path=sys.argv[1]
with open(path) as stream: route=yaml.safe_load(stream)
route['http']['services']['acb-frontend-service']['loadBalancer']['servers']=[{'url':'http://acb-frontend-blue:8080'}]
tmp=path+'.fixture.tmp'
with open(tmp,'w') as stream: yaml.safe_dump(route,stream,sort_keys=False)
os.chmod(tmp,0o644)
os.replace(tmp,path)
PY
stale_status=200
for _ in {1..30}; do
  stale_status="$(docker run --rm --network container:edge-traefik curlimages/curl:8.12.1 -s -o /dev/null -w '%{http_code}' \
    -H 'Host: frontend-deploy.acb.internal.invalid' http://127.0.0.1:18080/)" || stale_status=000
  [[ "$stale_status" != 200 ]] && break
  sleep 1
done
[[ "$stale_status" =~ ^50[234]$ ]] || fail "Traefik did not serve stale frontend upstream failure (HTTP $stale_status)"
ack_rc=0
timeout 68 bash "$target/healthcheck.sh" route green "$release_sha" "$release_sha" >"$root/stale-route.log" 2>&1 || ack_rc=$?
[[ "$ack_rc" != 0 && "$ack_rc" != 124 ]] || fail "Internal ACK did not reject stale frontend service (exit $ack_rc)"
cp "$root/valid-acb.yml" "$root/edge/dynamic/.acb-fixture.tmp"
chmod 644 "$root/edge/dynamic/.acb-fixture.tmp"
mv "$root/edge/dynamic/.acb-fixture.tmp" "$root/edge/dynamic/acb.yml"
DEPLOY_PATH="$root" bash "$target/healthcheck.sh" route green "$release_sha" "$release_sha"
for name in acb-gateway-green acb-frontend-green acb-worker acb-auth-browser acb-tts-gateway acb-bark; do
  [[ "$(docker inspect --format '{{.State.Health.Status}}' "$name")" == healthy ]] || fail "$name unhealthy after deployment"
done
for name in acb-gateway-blue acb-frontend-blue; do
  [[ "$(docker inspect --format '{{.State.Running}}' "$name")" == false ]] || fail "Retired container $name still running"
done
[[ "$(curl -fsS https://transactions.tuannguyenviet.site/__release)" == "$release_sha" ]] || fail 'Public HTTPS frontend SHA mismatch'
[[ "$(docker run --rm --network edge-acb curlimages/curl:8.12.1 -s -o /dev/null -w '%{http_code}' \
  -H 'Host: gateway-deploy.acb.internal.invalid' http://edge-traefik:8080/readyz)" == 404 ]] || fail 'Internal deploy router exposed via public web'
[[ "$(docker run --rm --network container:edge-traefik curlimages/curl:8.12.1 -s -o /dev/null -w '%{http_code}' \
  -H 'Host: gateway-deploy.acb.internal.invalid' http://127.0.0.1:18080/internal/private)" == 404 ]] || fail 'Internal gateway probe leaks private API'
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
backup_count="$(find "$root/data/backups" -name receipt.json | wc -l)"
state_before="$(sha256sum "$root/state.env")"
route_before="$(sha256sum "$root/edge/dynamic/acb.yml")"
worker_before="$(docker inspect --format '{{.Id}}' acb-worker)"
DEPLOY_PATH="$root" bash "$target/deploy.sh" "$release_sha"
[[ "$(docker inspect --format '{{.Id}}' acb-worker)" == "$worker_before" ]] || fail 'Same SHA restarted worker'
[[ "$(find "$root/data/backups" -name receipt.json | wc -l)" == "$backup_count" ]] || fail 'Same SHA created another snapshot'
[[ "$(sha256sum "$root/state.env")" == "$state_before" ]] || fail 'Same SHA rewrote committed state'
[[ "$(sha256sum "$root/edge/dynamic/acb.yml")" == "$route_before" ]] || fail 'Same SHA rewrote app route'
DEPLOY_PATH="$root" bash "$target/deploy.sh" --rollback
[[ "$(curl -s -o /dev/null -w '%{http_code}' https://transactions.tuannguyenviet.site/)" == 200 ]] || fail 'Public transactions fixture not healthy after rollback'
[[ "$(sed -n 's/^RELEASE_SHA=//p' "$root/state.env")" == "$base_sha" ]] || fail 'Rollback did not restore baseline'
[[ "$(docker inspect --format '{{.Config.Image}}' acb-worker)" == "$WORKER_IMAGE_REF" ]] || fail 'Rollback did not restore worker digest'
DEPLOY_PATH="$root" bash "$target/deploy.sh" --rollback
state_before="$(sha256sum "$root/state.env")"
route_before="$(sha256sum "$root/edge/dynamic/acb.yml")"
aux_before="$(docker inspect -f '{{.Id}}' acb-auth-browser acb-tts-gateway acb-bark)"
worker_before="$(docker inspect --format '{{.Id}}' acb-worker)"
for mode in corrupt-backup quiesce-timeout unhealthy-worker wrong-frontend wrong-gateway stale-service; do
  if [[ "$mode" == wrong-frontend ]]; then
    printf '%s\n' "$base_sha" > "$root/wrong-release"
    chmod 644 "$root/wrong-release"
    printf 'services:\n  frontend-green:\n    volumes:\n      - %s:/usr/share/nginx/html/__release:ro\n' "$root/wrong-release" > "$root/wrong-frontend.yaml"
  fi
  fault_rc=0
  fault_started=$SECONDS
  DOCKER_FAULT_MODE="$mode" WRONG_FRONTEND_OVERRIDE="$root/wrong-frontend.yaml" WRONG_GATEWAY_RUNTIME="$target/runtime.env" WRONG_GATEWAY_SHA="$base_sha" STALE_SERVICE_APPLIED="$root/stale-service-applied" \
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
  if [[ "$mode" == stale-service ]]; then [[ -f "$root/stale-service-applied" ]] || fail 'Stale service fault never reached Traefik ACK'; fi
  if [[ "$mode" == wrong-gateway ]]; then
    python3 - "$target/runtime.env" "$release_sha" <<'PY'
import pathlib,sys
p=pathlib.Path(sys.argv[1]); lines=p.read_text().splitlines()
p.write_text('\n'.join('RELEASE_COMMIT_GREEN='+sys.argv[2] if s.startswith('RELEASE_COMMIT_GREEN=') else s for s in lines)+'\n')
PY
  fi
  if [[ "$mode" == corrupt-backup ]]; then
    [[ "$(docker inspect -f '{{.Id}}' acb-auth-browser acb-tts-gateway acb-bark)" == "$aux_before" ]] || fail 'Backup corruption restarted auxiliaries'
  fi
  [[ ! -d "$root/.deploy-pending" ]] || fail "$mode left recoverable deployment pending"
  [[ "$(sha256sum "$root/state.env")" == "$state_before" ]] || fail "$mode committed wrong state"
  [[ "$(sha256sum "$root/edge/dynamic/acb.yml")" == "$route_before" ]] || fail "$mode did not restore route"
  [[ "$(docker inspect -f '{{.State.Health.Status}}' acb-worker)" == healthy ]] || fail "$mode left worker unhealthy"
  [[ "$(docker inspect -f '{{.Config.Image}}' acb-worker)" == "$WORKER_IMAGE_REF" ]] || fail "$mode lost original worker digest"
  [[ "$(docker inspect -f '{{.State.Running}}' acb-gateway-blue)" == true ]] || fail "$mode stopped serving gateway"
done
# Each fault delays a real Docker operation, never fabricates an inspect, health or HTTP result.
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
for signal in TERM KILL; do
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
  kill -s "$signal" "$deploy_child"
  if [[ "$signal" == KILL ]]; then kill -KILL "$(cat "$marker")" 2>/dev/null || :; else : > "$release"; fi
  interrupt_rc=0; wait "$deploy_pid" || interrupt_rc=$?
  active_deploy_pid=''; marker=''
  [[ "$interrupt_rc" != 0 && "$interrupt_rc" != 124 ]] || fail "$signal interruption did not exit safely ($interrupt_rc)"
  if [[ "$signal" == KILL ]]; then
    [[ -d "$root/.deploy-pending" ]] || fail 'SIGKILL lost pending recovery evidence'
    timeout 240 bash "$target/deploy.sh" "$release_sha" >"$root/recovery-kill.log" 2>&1
    [[ "$(sed -n 's/^RELEASE_SHA=//p' "$root/state.env")" == "$release_sha" ]] || fail 'SIGKILL recovery did not deploy target'
    bash "$target/deploy.sh" --rollback >"$root/recovery-rollback.log" 2>&1
  fi
  verify_baseline "$signal recovery"
done
marker="$root/postcommit.marker" release="$root/postcommit.release"
DOCKER_FAULT_MODE=after-commit DOCKER_FAULT_MARKER="$marker" DOCKER_FAULT_RELEASE="$release" \
  timeout 240 bash "$target/deploy.sh" "$release_sha" >"$root/postcommit.log" 2>&1 &
deploy_pid=$!
active_deploy_pid="$deploy_pid"
wait_marker "$marker" "$deploy_pid"
[[ "$(sed -n 's/^RELEASE_SHA=//p' "$root/state.env")" == "$release_sha" ]] || fail 'Post-commit fault preceded state commit'
deploy_child="$(ps -o pid= --ppid "$deploy_pid" | xargs)"
[[ "$deploy_child" =~ ^[0-9]+$ ]] || fail 'Cannot identify post-commit deploy process'
kill -KILL "$deploy_child" "$(cat "$marker")"
postcommit_rc=0; wait "$deploy_pid" || postcommit_rc=$?
active_deploy_pid=''; marker=''
[[ "$postcommit_rc" != 0 ]] || fail 'SIGKILL after commit returned success'
[[ -d "$root/.deploy-pending" ]] || fail 'SIGKILL after commit lost pending snapshot'
timeout 240 bash "$target/deploy.sh" "$release_sha" >"$root/postcommit-recovery.log" 2>&1
[[ ! -d "$root/.deploy-pending" && -d "$root/rollback" ]] || fail 'Committed recovery did not finish retire'
[[ "$(sed -n 's/^RELEASE_SHA=//p' "$root/state.env")" == "$release_sha" ]] || fail 'Committed recovery changed release'
for name in acb-gateway-blue acb-frontend-blue; do
  [[ "$(docker inspect -f '{{.State.Running}}' "$name")" == false ]] || fail "Committed recovery left $name running"
done
docker stop edge-traefik >/dev/null
rollback_ack_rc=0
timeout 150 bash "$target/deploy.sh" --rollback >"$root/rollback-ack-outage.log" 2>&1 || rollback_ack_rc=$?
[[ "$rollback_ack_rc" != 0 && "$rollback_ack_rc" != 124 ]] || fail "Rollback ACK outage did not fail boundedly ($rollback_ack_rc)"
[[ "$(cat "$root/rollback-ack-outage.log")" == *ROLLBACK_FAILED* ]] || fail 'Rollback ACK outage omitted ROLLBACK_FAILED evidence'
[[ "$(sed -n 's/^RELEASE_SHA=//p' "$root/state.env")" == "$release_sha" ]] || fail 'Rollback ACK outage changed committed state'
for name in acb-gateway-blue acb-frontend-blue acb-gateway-green acb-frontend-green; do
  [[ "$(docker inspect -f '{{.State.Running}}' "$name")" == true ]] || fail "Rollback ACK outage stopped $name"
done
docker start edge-traefik >/dev/null
bash "$target/deploy.sh" --rollback >"$root/postcommit-rollback.log" 2>&1
verify_baseline 'post-commit recovery'
docker run --rm --network none --user 1000:1000 -v bank-event-gateway_gateway_data:/data python:3.13-alpine python -c '
import datetime,sqlite3
db=sqlite3.connect("/data/gateway.db")
now=datetime.datetime.now(datetime.timezone.utc)
expiry=(now+datetime.timedelta(hours=2)).isoformat()
db.execute("INSERT INTO connections(id,created_at,updated_at) VALUES(?,?,?)",("rehearsal-auth",now.isoformat(),now.isoformat()))
db.execute("INSERT INTO auth_attempts(id,connection_id,generation,owner_subject,status,expires_at,created_at) VALUES(?,?,?,?,?,?,?)",("rehearsal-attempt","rehearsal-auth",0,"deploy-smoke@example.invalid","IN_PROGRESS",expiry,now.isoformat()))
db.commit()'
if bash "$target/deploy.sh" "$release_sha" >"$root/active-auth.log" 2>&1; then fail 'Active authentication did not block deployment'; fi
[[ "$(sha256sum "$root/state.env")" == "$state_before" && "$(sha256sum "$root/edge/dynamic/acb.yml")" == "$route_before" && "$(docker inspect --format '{{.Id}}' acb-worker)" == "$worker_before" ]] || fail 'Active-auth failure mutated runtime'
docker run --rm --network none --user 1000:1000 -v bank-event-gateway_gateway_data:/data python:3.13-alpine python -c '
import sqlite3
db=sqlite3.connect("/data/gateway.db")
db.execute("DELETE FROM auth_attempts WHERE id=?",("rehearsal-attempt",))
db.execute("DELETE FROM connections WHERE id=?",("rehearsal-auth",))
db.commit()'
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
unpullable_sha=ffffffffffffffffffffffffffffffffffffffff
unpullable="$root/releases/$unpullable_sha"
mkdir "$unpullable"
cp "$target/"{deploy.sh,simple-lib.sh,healthcheck.sh,render-route.sh,backup-db.sh,compose.prod.yaml,seccomp-auth-browser.json,bark-entrypoint.sh,import-baseline.py} "$unpullable/"
python3 - "$target/images.env" "$unpullable/images.env" "$unpullable_sha" <<'PY'
import sys
src,dst,sha=sys.argv[1:]
text=open(src).read()
text='\n'.join(('RELEASE_SHA='+sha if line.startswith('RELEASE_SHA=') else 'GATEWAY_IMAGE_REF=ghcr.io/thedemontuan/acb-transaction-webhook@sha256:'+'f'*64 if line.startswith('GATEWAY_IMAGE_REF=') else line) for line in text.splitlines())+'\n'
open(dst,'w').write(text)
PY
pull_rc=0
timeout 120 bash "$unpullable/deploy.sh" "$unpullable_sha" >"$root/image-pull.log" 2>&1 || pull_rc=$?
[[ "$pull_rc" != 0 && "$pull_rc" != 124 ]] || fail "Missing image did not fail promptly (exit $pull_rc)"
[[ "$(sha256sum "$root/state.env")" == "$state_before" && "$(sha256sum "$root/edge/dynamic/acb.yml")" == "$route_before" && "$(docker inspect --format '{{.Id}}' acb-worker)" == "$worker_before" ]] || fail 'Image pull failure mutated runtime'
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
(cd "$root/edge/dynamic" && sha256sum -c "$root/unrelated.sha256") || fail 'Deployment modified unrelated Traefik files'
printf 'PASS: real Docker baseline, deploy, verified snapshot, no-op rerun, rollback, unrelated routes\n'
