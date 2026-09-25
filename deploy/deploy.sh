#!/usr/bin/env bash
set -Eeuo pipefail
umask 077
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$HERE/simple-lib.sh"
DEPLOY_PATH="${DEPLOY_PATH:-/opt/bank-event-gateway}"
[[ "$DEPLOY_PATH" == /* && -d "$DEPLOY_PATH" ]] || { fail 'DEPLOY_PATH must be an existing absolute root'; exit 1; }
mode=deploy; [[ "${1:-}" == --check ]] && { mode=check; shift; }
[[ "${1:-}" == --rollback ]] && { mode=rollback; shift; }
[[ $# == 1 || ( "$mode" == rollback && $# == 0 ) ]] || { fail 'usage: deploy.sh [--check] SHA | --rollback'; exit 1; }
sha="${1:-}"
[[ "$mode" == rollback ]] || validate_sha "$sha"
if [[ "$mode" == check ]]; then
  if [[ -f "$DEPLOY_PATH/.deploy.lock" ]]; then
    exec 9<"$DEPLOY_PATH/.deploy.lock"
    flock -s -w 30 9 || { fail 'deploy lock unavailable'; exit 1; }
  fi
else
  exec 9>"$DEPLOY_PATH/.deploy.lock"
  flock -w 30 9 || { fail 'deploy lock unavailable'; exit 1; }
fi
STATE="$DEPLOY_PATH/state.env"
PENDING="$DEPLOY_PATH/.deploy-pending"
ROLLBACK="$DEPLOY_PATH/rollback"
ROUTE="${ACB_ROUTE_FILE:-$(route_file)}"
DYNAMIC="$(dirname "$ROUTE")"
[[ "$ROUTE" == "$DYNAMIC/acb.yml" && -f "$ROUTE" ]] || { fail 'missing app-owned dynamic route'; exit 1; }
RELEASE="$DEPLOY_PATH/releases/$sha"
IMAGES='RELEASE_SHA GATEWAY_IMAGE_REF FRONTEND_IMAGE_REF WORKER_IMAGE_REF DBTOOL_IMAGE_REF BROWSER_IMAGE_REF TTS_IMAGE_REF BARK_IMAGE_REF'
STATE_KEYS='RELEASE_SHA GATEWAY_SLOT FRONTEND_SLOT'
RUNTIME_KEYS='IMAGE_REF_BLUE IMAGE_REF_GREEN FRONTEND_IMAGE_REF_BLUE FRONTEND_IMAGE_REF_GREEN WORKER_IMAGE_REF BROWSER_IMAGE_REF TTS_IMAGE_REF BARK_IMAGE_REF DBTOOL_IMAGE_REF RELEASE_COMMIT_BLUE RELEASE_COMMIT_GREEN WORKER_RELEASE_COMMIT ENV_FILE SECRETS_DIR BARK_SECRET_GROUP'
validate_state() {
  load_keys "$1" "$STATE_KEYS"
  validate_sha "$RELEASE_SHA"
  [[ "$GATEWAY_SLOT" == blue || "$GATEWAY_SLOT" == green ]] || fail 'invalid gateway slot'
  [[ "$FRONTEND_SLOT" == blue || "$FRONTEND_SLOT" == green ]] || fail 'invalid frontend slot'
}
validate_bundle() {
  local requested="$1" bundle="$DEPLOY_PATH/releases/$1" key
  [[ -f "$bundle/compose.prod.yaml" && -f "$bundle/deploy.sh" && -f "$bundle/images.env" ]] || fail "incomplete bundle: $bundle"
  load_keys "$bundle/images.env" "$IMAGES"
  [[ "$RELEASE_SHA" == "$requested" ]] || fail 'bundle SHA mismatch'
  for key in GATEWAY_IMAGE_REF FRONTEND_IMAGE_REF WORKER_IMAGE_REF DBTOOL_IMAGE_REF BROWSER_IMAGE_REF TTS_IMAGE_REF BARK_IMAGE_REF; do validate_digest "${!key}" "$key"; done
  [[ -f "$bundle/SHA256SUMS" ]] && (cd "$bundle" && sha256sum -c SHA256SUMS >/dev/null) || [[ ! -f "$bundle/SHA256SUMS" ]] || fail 'bundle contents changed'
}
preflight() {
  local path component
  for component in docker python3 sqlite3 flock timeout; do command -v "$component" >/dev/null || fail "missing $component"; done
  python3 -c 'import yaml' || fail 'PyYAML required'
  for path in edge-acb acb-core acb-egress; do docker network inspect "$path" >/dev/null || fail "missing network $path"; done
  for path in bank-event-gateway_gateway_data bank-event-gateway_bark_data; do docker volume inspect "$path" >/dev/null || fail "missing volume $path"; done
  for path in acb worker auth-browser; do [[ ! -e "${FAILOVER_REGISTRY_DIR:-/etc/vps-failover/apps.d}/$path.json" ]] || fail "shared failover still owns $path"; done
  validate_permissions
  local origins
  origins="$(python3 - "$DEPLOY_PATH/deploy/.env.production" <<'PY'
import sys
values={}
for line in open(sys.argv[1],encoding='utf-8'):
    line=line.strip()
    if not line or line.startswith('#'): continue
    key,sep,value=line.partition('=')
    if key in ('PUBLIC_ORIGIN','PUBLIC_VIEWER_ORIGIN','PUBLIC_VIEWER_HOST'):
        if not sep or key in values or any(ch.isspace() for ch in value): raise SystemExit('invalid public origin setting')
        values[key]=value.strip('"\'')
bank=values.get('PUBLIC_ORIGIN','https://bank.tuannguyenviet.site')
viewer=values.get('PUBLIC_VIEWER_ORIGIN') or 'https://'+values.get('PUBLIC_VIEWER_HOST','transactions.tuannguyenviet.site')
print(bank+'\n'+viewer)
PY
)" || fail 'invalid canonical public origins'
  readarray -t PUBLIC_HOSTS <<< "$origins"
  [[ "${#PUBLIC_HOSTS[@]}" == 2 ]] || fail 'missing public origins'
  export PUBLIC_ORIGIN="${PUBLIC_HOSTS[0]}" PUBLIC_VIEWER_ORIGIN="${PUBLIC_HOSTS[1]}"
  "$HERE/render-route.sh" blue blue >/dev/null
  [[ "$(docker inspect -f '{{range .Mounts}}{{if eq .Destination "/etc/traefik/dynamic"}}{{.Source}}{{end}}{{end}}' edge-traefik)" == "$(realpath "$DYNAMIC")" ]] || fail 'Traefik dynamic mount mismatch'
  static="$(docker inspect -f '{{range .Mounts}}{{if eq .Destination "/etc/traefik/traefik.yml"}}{{.Source}}{{end}}{{end}}' edge-traefik)"
  [[ -f "$static" ]] || fail 'Traefik static config mount missing'
  python3 - "$static" <<'PY'
import sys,yaml
c=yaml.safe_load(open(sys.argv[1]))
assert c['entryPoints']['slot-probe']['address']=='127.0.0.1:18080'
assert c['providers']['file']['directory']=='/etc/traefik/dynamic'
assert c['providers']['file']['watch'] is True
PY
  docker inspect edge-traefik >/dev/null || fail 'missing edge-traefik'
}
load_runtime() {
  local dir="$1" key
  load_keys "$dir/runtime.env" "$RUNTIME_KEYS"
  [[ "$ENV_FILE" == "$DEPLOY_PATH/deploy/.env.production" && "$SECRETS_DIR" == "$DEPLOY_PATH/deploy/secrets" && "$BARK_SECRET_GROUP" == 1000 ]] || fail 'runtime paths/group mismatch'
  for key in IMAGE_REF_BLUE IMAGE_REF_GREEN FRONTEND_IMAGE_REF_BLUE FRONTEND_IMAGE_REF_GREEN WORKER_IMAGE_REF BROWSER_IMAGE_REF TTS_IMAGE_REF BARK_IMAGE_REF DBTOOL_IMAGE_REF; do validate_digest "${!key}" "$key"; done
  for key in RELEASE_COMMIT_BLUE RELEASE_COMMIT_GREEN WORKER_RELEASE_COMMIT; do validate_sha "${!key}"; done
}
compose() { compose_release "$1" "$1/runtime.env" "${@:2}"; }
write_runtime() {
  local dest="$1" gw="$2" fe="$3" previous="$4" candidate_gateway="$GATEWAY_IMAGE_REF" candidate_frontend="$FRONTEND_IMAGE_REF"
  local candidate_worker="$WORKER_IMAGE_REF" candidate_browser="$BROWSER_IMAGE_REF" candidate_tts="$TTS_IMAGE_REF" candidate_bark="$BARK_IMAGE_REF" candidate_dbtool="$DBTOOL_IMAGE_REF"
  [[ "$gw" == blue || "$gw" == green ]] || return 1
  [[ "$fe" == blue || "$fe" == green ]] || return 1
  load_runtime "$previous"
  local -A ref=([blue]="$IMAGE_REF_BLUE" [green]="$IMAGE_REF_GREEN")
  local -A fe_ref=([blue]="$FRONTEND_IMAGE_REF_BLUE" [green]="$FRONTEND_IMAGE_REF_GREEN")
  local -A commit=([blue]="$RELEASE_COMMIT_BLUE" [green]="$RELEASE_COMMIT_GREEN")
  ref[$gw]="$candidate_gateway"; fe_ref[$fe]="$candidate_frontend"; commit[$gw]="$sha"
  {
    printf 'IMAGE_REF_BLUE=%s\nIMAGE_REF_GREEN=%s\n' "${ref[blue]}" "${ref[green]}"
    printf 'FRONTEND_IMAGE_REF_BLUE=%s\nFRONTEND_IMAGE_REF_GREEN=%s\n' "${fe_ref[blue]}" "${fe_ref[green]}"
    printf 'RELEASE_COMMIT_BLUE=%s\nRELEASE_COMMIT_GREEN=%s\n' "${commit[blue]}" "${commit[green]}"
    printf 'WORKER_IMAGE_REF=%s\nBROWSER_IMAGE_REF=%s\nTTS_IMAGE_REF=%s\nBARK_IMAGE_REF=%s\nDBTOOL_IMAGE_REF=%s\n' "$candidate_worker" "$candidate_browser" "$candidate_tts" "$candidate_bark" "$candidate_dbtool"
    printf 'WORKER_RELEASE_COMMIT=%s\nENV_FILE=%s\nSECRETS_DIR=%s\nBARK_SECRET_GROUP=1000\n' "$sha" "$DEPLOY_PATH/deploy/.env.production" "$DEPLOY_PATH/deploy/secrets"
  } | atomic_write_file "$dest/runtime.env" 600
}
route_replace() {
  local src="$1"
  python3 - "$src" "$ROUTE" <<'PY'
import os,sys,tempfile
src,dst=sys.argv[1:]
with open(src,'rb') as f: data=f.read()
fd,tmp=tempfile.mkstemp(prefix='.acb-',suffix='.tmp',dir=os.path.dirname(dst))
try:
    with os.fdopen(fd,'wb') as f:
        os.fchmod(f.fileno(),0o644);f.write(data);f.flush();os.fsync(f.fileno())
    os.replace(tmp,dst)
    d=os.open(os.path.dirname(dst),os.O_DIRECTORY)
    try: os.fsync(d)
    finally: os.close(d)
finally:
    if os.path.exists(tmp): os.unlink(tmp)
PY
}
check_running() {
  local bundle="$1" gw="$2" fe="$3" id="$4" key ref
  load_runtime "$bundle"
  for key in gateway-$gw frontend-$fe worker auth-browser tts-gateway bark; do
    case "$key" in
      gateway-blue) ref="$IMAGE_REF_BLUE";; gateway-green) ref="$IMAGE_REF_GREEN";;
      frontend-blue) ref="$FRONTEND_IMAGE_REF_BLUE";; frontend-green) ref="$FRONTEND_IMAGE_REF_GREEN";;
      worker) ref="$WORKER_IMAGE_REF";; auth-browser) ref="$BROWSER_IMAGE_REF";;
      tts-gateway) ref="$TTS_IMAGE_REF";; bark) ref="$BARK_IMAGE_REF";;
    esac
    container_image_check "acb-$key" "$ref"
    EXPECTED_IMAGE_REF="$ref" EXPECTED_SLOT="${key##*-}" EXPECTED_RELEASE_SHA="$id" "$HERE/healthcheck.sh" container "acb-$key" 120
  done
  "$bundle/healthcheck.sh" route "$gw" "$id" "$id"
}
baseline_route_ack() {
  local slot="$1" commit="$2" hashfile="$3" actual headers deadline=$((SECONDS+60))
  [[ -f "$hashfile" ]] || fail 'missing imported baseline frontend identity'
  local expected; expected="$(cat "$hashfile")"
  [[ "$expected" =~ ^[a-f0-9]{64}$ ]] || fail 'invalid imported frontend hash'
  while (( SECONDS < deadline )); do
    headers="$(docker run --rm --network container:edge-traefik curlimages/curl:8.12.1 -fsS -D - -o /dev/null -H 'Host: gateway-deploy.acb.internal.invalid' --max-time 5 http://127.0.0.1:18080/readyz 2>/dev/null)" || headers=''
    if [[ "${headers,,}" == *"x-platform-slot: $slot"* && "${headers,,}" == *"x-release-commit: $commit"* ]]; then
      actual="$(docker run --rm --network container:edge-traefik curlimages/curl:8.12.1 -fsS --max-time 5 -H 'Host: frontend-deploy.acb.internal.invalid' -H 'Accept-Encoding: identity' http://127.0.0.1:18080/ 2>/dev/null | sha256sum)" || actual=''
      [[ "${actual%% *}" == "$expected" ]] && { log_info 'imported baseline ACK passed'; return 0; }
    fi
    sleep 1
  done
  fail 'imported baseline ACK failed'
}
public_smoke() {
  local response status location viewer release
  response="$(curl -sS --max-time 15 -D - -o /dev/null -w $'\n%{http_code}' "$PUBLIC_ORIGIN/")" || return 1
  status="${response##*$'\n'}"
  [[ "$status" == 200 || "$status" == 302 ]] || fail "bank public HTTP $status"
  if [[ "$status" == 302 ]]; then
    location="$(printf '%s' "$response" | python3 -c 'import sys;print(next((x.split(":",1)[1].strip() for x in sys.stdin if x.lower().startswith("location:")),""))')"
    [[ "$location" == https://thedemontuan.cloudflareaccess.com/cdn-cgi/access/login/* ]] || fail 'bank redirected outside expected Access login'
  fi
  viewer="$(curl -sS --max-time 15 -o /dev/null -w '%{http_code}' "$PUBLIC_VIEWER_ORIGIN/")"
  [[ "$viewer" == 200 ]] || fail "transactions public HTTP $viewer"
  release="$(curl -fsS --max-time 15 "$PUBLIC_VIEWER_ORIGIN/__release")"
  [[ "$release" == "$sha" ]] || fail 'public frontend identity mismatch'
  log_info 'public smoke passed'
}
renew() {
  [[ "${GATE_TOKEN:-}" ]] || return 0
  if (( SECONDS >= DEADLINE )); then fail 'mutation deadline expired'; return 1; fi
  dbtool rw -gate-renew -owner "$GATE_OWNER" -lease-token "$GATE_TOKEN" -lease-duration 15m >/dev/null
}
release_gate() {
  [[ "${GATE_TOKEN:-}" ]] || return 0
  dbtool rw -gate-release -owner "$GATE_OWNER" -lease-token "$GATE_TOKEN" >/dev/null
  GATE_TOKEN=''
}
quiesce_worker() {
  local old="$1" candidate="$2" report
  for image in "$old" "$candidate"; do
    if [[ "$image" == "$old" ]]; then report="$(docker exec acb-worker /worker -deploy-capabilities)"; else report="$(docker run --rm --entrypoint /worker "$image" -deploy-capabilities)"; fi
    printf '%s' "$report" | python3 -c 'import sys,json; d=json.load(sys.stdin); assert d["protocol"]>=2 and all(d.get(k) is True for k in ("quiesce","drain","resume","notificationDrain","sessionCheckpoint","journalCheckpoint"))'
  done
  report="$(timeout 45 docker exec -e WORKER_INTERNAL_TOKEN_FILE=/run/secrets/worker_internal_token acb-worker /worker -quiesce)"
  printf '%s' "$report" | python3 -c 'import sys,json; d=json.load(sys.stdin); assert d["status"]=="quiesced" and d["quiesced"] is True and d["dispatcher"]=="IDLE" and d["activeDeliveries"]==0 and d["activePoll"] is False and d["sessionCheckpointed"] is True and all(type(d[k]) is int and d[k]>=0 for k in ("generation","journalSeq"))'
  QUIESCED=1
}
worker_switch() {
  local source="$1" target="$2" old="$3" new="$4"
  quiesce_worker "$old" "$new"
  renew
  docker stop -t 30 acb-worker >/dev/null
  [[ "$(docker inspect -f '{{.State.Running}}' acb-worker)" == false ]] || fail 'worker still running'
  QUIESCED=0
  compose "$target" up -d --no-deps worker
  EXPECTED_IMAGE_REF="$new" "$HERE/healthcheck.sh" container acb-worker 120
  container_image_check acb-worker "$new"
}
restore_previous() {
  local snap="$1" prev_sha prev_gw prev_fe prev_bundle svc
  validate_state "$snap/previous-state.env"
  prev_sha="$RELEASE_SHA"; prev_gw="$GATEWAY_SLOT"; prev_fe="$FRONTEND_SLOT"
  prev_bundle="$DEPLOY_PATH/releases/$prev_sha"
  load_runtime "$prev_bundle"
  DBTOOL_IMAGE_REF="$DBTOOL_IMAGE_REF"
  for svc in worker auth-browser tts-gateway bark; do
    local ref
    case "$svc" in worker) ref="$WORKER_IMAGE_REF";; auth-browser) ref="$BROWSER_IMAGE_REF";; tts-gateway) ref="$TTS_IMAGE_REF";; bark) ref="$BARK_IMAGE_REF";; esac
    if [[ "$svc" == worker ]] && container_image_check acb-worker "$ref" >/dev/null 2>&1 && [[ "$(docker inspect -f '{{.State.Health.Status}}' acb-worker 2>/dev/null)" == healthy ]]; then
      docker exec -e WORKER_INTERNAL_TOKEN_FILE=/run/secrets/worker_internal_token acb-worker /worker -resume >/dev/null || return 1
      if EXPECTED_IMAGE_REF="$ref" "$HERE/healthcheck.sh" container acb-worker 30; then continue; fi
    fi
    if ! container_image_check "acb-$svc" "$ref" >/dev/null 2>&1 || [[ "$(docker inspect -f '{{.State.Health.Status}}' "acb-$svc" 2>/dev/null)" != healthy ]]; then
      if [[ "$svc" == worker ]]; then
        if [[ "$(docker inspect -f '{{.State.Running}} {{if .State.Health}}{{.State.Health.Status}}{{end}}' acb-worker 2>/dev/null)" == 'true healthy' ]]; then quiesce_worker "$(container_ref acb-worker)" "$ref"; fi
        docker stop -t 30 acb-worker >/dev/null || return 1
        QUIESCED=0
      fi
      renew
      compose "$prev_bundle" up -d --no-deps --force-recreate "$svc"
    fi
    EXPECTED_IMAGE_REF="$ref" "$HERE/healthcheck.sh" container "acb-$svc" 120
    container_image_check "acb-$svc" "$ref"
  done
  for svc in gateway-$prev_gw frontend-$prev_fe; do
    compose "$prev_bundle" up -d --no-deps --no-recreate "$svc"
    local ref="$IMAGE_REF_BLUE" slot="${svc##*-}"
    case "$svc" in gateway-green) ref="$IMAGE_REF_GREEN";; frontend-blue) ref="$FRONTEND_IMAGE_REF_BLUE";; frontend-green) ref="$FRONTEND_IMAGE_REF_GREEN";; esac
    if [[ "$svc" == frontend-* && -f "$prev_bundle/baseline-frontend.sha256" ]]; then
      container_image_check "acb-$svc" "$ref" || return 1
      local health_deadline=$((SECONDS+120))
      until [[ "$(docker inspect -f '{{.State.Health.Status}}' "acb-$svc" 2>/dev/null)" == healthy ]]; do
        if (( SECONDS >= health_deadline )); then log_error "restored $svc did not become healthy"; return 1; fi
        sleep 1
      done
    else
      EXPECTED_IMAGE_REF="$ref" EXPECTED_SLOT="$slot" EXPECTED_RELEASE_SHA="$prev_sha" "$HERE/healthcheck.sh" container "acb-$svc" 120
    fi
  done
  route_replace "$snap/previous-acb.yml"
  if [[ -f "$prev_bundle/baseline-frontend.sha256" ]]; then
    baseline_route_ack "$prev_gw" "$prev_sha" "$prev_bundle/baseline-frontend.sha256" || return 1
  else
    "$HERE/healthcheck.sh" route "$prev_gw" "$prev_sha" "$prev_sha" || return 1
  fi
  if [[ -f "$STATE" ]] && ! cmp -s "$STATE" "$snap/previous-state.env"; then cat "$snap/previous-state.env" | atomic_write_file "$STATE" 600; fi
  for svc in gateway frontend; do
    local slot="$prev_gw"; [[ "$svc" == frontend ]] && slot="$prev_fe"
    [[ "$slot" == blue ]] && slot=green || slot=blue
    if docker inspect "acb-$svc-$slot" >/dev/null 2>&1; then
      docker stop "acb-$svc-$slot" >/dev/null
    fi
  done
}
cleanup() {
  local code=$?
  trap - EXIT HUP INT TERM
  if (( code != 0 )) && [[ "${MUTATING:-0}" == 1 && "${COMMITTED:-0}" == 0 ]]; then
    DEADLINE=$((SECONDS+600))
    log_error "deploy failed ($code); restoring previous runtime"
    if ! restore_previous "$PENDING"; then
      log_error 'ROLLBACK_FAILED: retain both HTTP slots and pending evidence'
      code=1
    else
      if [[ "${GATE_TOKEN:-}" ]]; then
        if release_gate; then rm -rf "$PENDING"; else code=1; fi
      else
        rm -rf "$PENDING"
      fi
    fi
  elif (( code != 0 )) && [[ "${COMMITTED:-0}" == 1 ]]; then log_error 'RETIRE_FAILED: committed state retained; rerun to finish'; fi
  if [[ "${QUIESCED:-0}" == 1 ]]; then docker exec -e WORKER_INTERNAL_TOKEN_FILE=/run/secrets/worker_internal_token acb-worker /worker -resume >/dev/null || code=1; fi
  if [[ "${GATE_TOKEN:-}" ]]; then release_gate || code=1; fi
  exit "$code"
}
trap cleanup EXIT
trap 'code=$?; log_error "command failed at line $LINENO (exit $code)"' ERR
trap 'exit 130' INT
trap 'exit 143' TERM HUP
preflight
if [[ "$mode" == rollback ]]; then
  [[ -d "$ROLLBACK" && ! -d "$PENDING" ]] || fail 'rollback snapshot unavailable or deployment pending'
  validate_state "$ROLLBACK/previous-state.env"
  if [[ -f "$STATE" ]] && cmp -s "$STATE" "$ROLLBACK/previous-state.env"; then
    validate_baseline_route "$ROUTE" "$GATEWAY_SLOT" "$FRONTEND_SLOT"
    prev_bundle="$DEPLOY_PATH/releases/$RELEASE_SHA"
    if [[ -f "$prev_bundle/baseline-frontend.sha256" ]]; then
      baseline_route_ack "$GATEWAY_SLOT" "$RELEASE_SHA" "$prev_bundle/baseline-frontend.sha256"
    else
      "$HERE/healthcheck.sh" route "$GATEWAY_SLOT" "$RELEASE_SHA" "$RELEASE_SHA"
    fi
    log_info 'rollback already committed'; exit 0
  fi
  [[ -f "$STATE" ]] && cmp -s "$STATE" "$ROLLBACK/target-state.env" || fail 'rollback state mismatch'
  previous="$DEPLOY_PATH/releases/$RELEASE_SHA"
  [[ -f "$previous/runtime.env" ]] || fail 'rollback bundle missing'
  load_runtime "$previous"
  DBTOOL_IMAGE_REF="$DBTOOL_IMAGE_REF"
  for ref in "$WORKER_IMAGE_REF" "$BROWSER_IMAGE_REF" "$TTS_IMAGE_REF" "$BARK_IMAGE_REF"; do docker image inspect "$ref" >/dev/null || docker pull "$ref"; done
  auth="$(dbtool ro -readonly -active-auth-count)"
  [[ "$(printf '%s' "$auth" | json_field activeCount)" == 0 ]] || fail 'active authentication blocks rollback'
  dbtool ro -readonly -schema-compat -min-version 11 >/dev/null
  GATE_OWNER="rollback-$$"; GATE_TOKEN=''; DEADLINE=$((SECONDS+600))
  gate="$(dbtool rw -gate-acquire -owner "$GATE_OWNER" -reason rollback -lease-duration 15m)"
  GATE_TOKEN="$(printf '%s' "$gate" | json_field leaseToken)"
  [[ -n "$GATE_TOKEN" ]] || fail 'missing rollback lease token'
  dbtool ro -readonly -gate-check -owner "$GATE_OWNER" >/dev/null
  if ! restore_previous "$ROLLBACK"; then
    log_error 'ROLLBACK_FAILED: retain both HTTP slots and rollback evidence'
    exit 1
  fi
  release_gate
  log_info 'rollback committed'
  exit 0
fi
validate_bundle "$sha"
if [[ -f "$STATE" ]]; then
  validate_state "$STATE"
  current="$RELEASE_SHA"; gateway_slot="$GATEWAY_SLOT"; frontend_slot="$FRONTEND_SLOT"
  [[ -d "$PENDING" ]] || validate_baseline_route "$ROUTE" "$gateway_slot" "$frontend_slot"
else
  legacy="$DEPLOY_PATH/state/current-release.json"
  read -r current gateway_slot frontend_slot < <(python3 - "$legacy" <<'PY'
import json,sys
s=json.load(open(sys.argv[1]));assert s['status']=='COMPLETED'
a=s['active_slots'];assert a['gateway'] in ('blue','green') and a['frontend'] in ('blue','green')
print(s['git_sha'],a['gateway'],a['frontend'])
PY
)
  validate_sha "$current"
  validate_baseline_route "$ROUTE" "$gateway_slot" "$frontend_slot"
fi
if [[ ! -f "$STATE" ]]; then
  for svc in "acb-gateway-$gateway_slot" "acb-frontend-$frontend_slot"; do
    [[ "$(docker inspect -f '{{.State.Health.Status}}' "$svc")" == healthy ]] || fail "legacy active HTTP unhealthy: $svc"
  done
  [[ "$(docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "acb-gateway-$gateway_slot" | sed -n 's/^RELEASE_COMMIT=//p')" == "$current" ]] || fail 'gateway release differs from legacy state'
  python3 - "$legacy" "$(docker inspect -f '{{.Config.Image}}' "acb-gateway-$gateway_slot")" "$(docker inspect -f '{{.Config.Image}}' "acb-frontend-$frontend_slot")" "$gateway_slot" <<'PY'
import json,sys
s=json.load(open(sys.argv[1])); gateway,frontend,slot=sys.argv[2:]
assert s['images']['gateway'][slot]==gateway, 'gateway image disagrees with canonical state'
assert s['images']['frontend']==frontend, 'frontend image disagrees with canonical state'
PY
  for svc in acb-worker acb-auth-browser acb-tts-gateway acb-bark; do
    [[ "$(docker inspect -f '{{.State.Health.Status}}' "$svc")" == healthy ]] || fail "baseline singleton unhealthy: $svc"
    ref="$(container_ref "$svc")"; validate_digest "$ref" "$svc"
    container_image_check "$svc" "$ref"
  done
fi
if [[ "$mode" == check ]]; then
  [[ ! -d "$PENDING" ]] || fail 'pending deployment requires recovery before check'
  log_info "preflight passed: $current $gateway_slot/$frontend_slot target $sha"
  exit 0
fi
if [[ -d "$PENDING" ]]; then
  if cmp -s "$STATE" "$PENDING/target-state.env"; then
    validate_state "$STATE"
    check_running "$DEPLOY_PATH/releases/$RELEASE_SHA" "$GATEWAY_SLOT" "$FRONTEND_SLOT" "$RELEASE_SHA"
    public_smoke
    if [[ -f "$PENDING/gate-lease" ]]; then
      { IFS= read -r GATE_OWNER; IFS= read -r GATE_TOKEN; } < "$PENDING/gate-lease"
      load_runtime "$DEPLOY_PATH/releases/$RELEASE_SHA"
      DBTOOL_IMAGE_REF="$DBTOOL_IMAGE_REF"
      release_gate
    fi
    COMMITTED=1
  elif cmp -s "$STATE" "$PENDING/previous-state.env"; then
    if [[ -f "$PENDING/gate-lease" ]]; then
      { IFS= read -r GATE_OWNER; IFS= read -r GATE_TOKEN; } < "$PENDING/gate-lease"
      prev_dbtool="$(python3 - "$PENDING/target-state.env" "$DEPLOY_PATH" <<'PY'
import pathlib,sys
sha=dict(x.strip().split('=',1) for x in open(sys.argv[1]))['RELEASE_SHA']
env=dict(x.strip().split('=',1) for x in open(pathlib.Path(sys.argv[2])/'releases'/sha/'runtime.env'))
print(env['DBTOOL_IMAGE_REF'])
PY
)"
      DBTOOL_IMAGE_REF="$prev_dbtool"; DEADLINE=$((SECONDS+600))
    fi
    if ! restore_previous "$PENDING"; then
      log_error 'ROLLBACK_FAILED: pending evidence and HTTP slots retained'
      exit 1
    fi
    release_gate
    rm -rf "$PENDING"
  else fail 'pending deployment state disagrees with committed/previous; evidence retained'; exit 1; fi
fi
if [[ "${COMMITTED:-0}" == 1 ]]; then
  validate_state "$PENDING/previous-state.env"
  docker stop "acb-gateway-$GATEWAY_SLOT" "acb-frontend-$FRONTEND_SLOT" >/dev/null
  [[ ! -d "$ROLLBACK" ]] || mv "$ROLLBACK" "$DEPLOY_PATH/data/rollback-$(date -u +%Y%m%d%H%M%S)-$$"
  mv "$PENDING" "$ROLLBACK"
  log_info 'retire complete'; exit 0
fi
if [[ -f "$STATE" && "$current" == "$sha" ]]; then
  check_running "$RELEASE" "$gateway_slot" "$frontend_slot" "$sha"
  public_smoke
  log_info 'already committed; no mutation'; exit 0
fi
if [[ ! -f "$STATE" ]]; then
  if [[ ! -d "$DEPLOY_PATH/releases/$current" ]]; then
    python3 "$HERE/import-baseline.py" "$DEPLOY_PATH" "$current" >/dev/null
  else
    [[ -f "$DEPLOY_PATH/releases/$current/runtime.env" && -f "$DEPLOY_PATH/releases/$current/images.env" ]] || fail 'incomplete imported baseline'
  fi
  baseline_hash="$(docker run --rm --network edge-acb curlimages/curl:8.12.1 -fsS --max-time 5 -H 'Accept-Encoding: identity' "http://acb-frontend-$frontend_slot:8080/" | sha256sum)"
  [[ "${baseline_hash%% *}" =~ ^[a-f0-9]{64}$ ]] || fail 'cannot hash baseline frontend'
  printf '%s\n' "${baseline_hash%% *}" | atomic_write_file "$DEPLOY_PATH/releases/$current/baseline-frontend.sha256" 600
  # Adoption adds only internal routers; public policy and upstreams stay untouched.
  mkdir -p "$DEPLOY_PATH/data"
  if [[ ! -f "$DEPLOY_PATH/data/pre-simple-acb.yml" ]]; then
    cat "$ROUTE" | atomic_write_file "$DEPLOY_PATH/data/pre-simple-acb.yml" 600
  fi
  validate_baseline_route "$DEPLOY_PATH/data/pre-simple-acb.yml" "$gateway_slot" "$frontend_slot"
  python3 - "$DEPLOY_PATH/data/pre-simple-acb.yml" "$DEPLOY_PATH/data/adopted-acb.yml" <<'PY'
import sys,yaml
c=yaml.safe_load(open(sys.argv[1])); r=c['http']['routers']
for name,host,rule,service in (('acb-deploy-gateway','gateway',' && Path(`/readyz`)','acb-service'),('acb-deploy-frontend','frontend','','acb-frontend-service')):
    assert name not in r
    r[name]={'rule':'Host(`'+host+'-deploy.acb.internal.invalid`)'+rule,'entryPoints':['slot-probe'],'service':service}
with open(sys.argv[2],'w') as f: yaml.safe_dump(c,f,sort_keys=False)
PY
  if ! cmp -s "$ROUTE" "$DEPLOY_PATH/data/adopted-acb.yml"; then
    cmp -s "$ROUTE" "$DEPLOY_PATH/data/pre-simple-acb.yml" || fail 'live route changed during baseline adoption'
    route_replace "$DEPLOY_PATH/data/adopted-acb.yml"
  fi
  if ! baseline_route_ack "$gateway_slot" "$current" "$DEPLOY_PATH/releases/$current/baseline-frontend.sha256"; then route_replace "$DEPLOY_PATH/data/pre-simple-acb.yml"; exit 1; fi
  printf 'RELEASE_SHA=%s\nGATEWAY_SLOT=%s\nFRONTEND_SLOT=%s\n' "$current" "$gateway_slot" "$frontend_slot" | atomic_write_file "$STATE" 600
fi
previous="$DEPLOY_PATH/releases/$current"
next_gw=blue; [[ "$gateway_slot" == blue ]] && next_gw=green
next_fe=blue; [[ "$frontend_slot" == blue ]] && next_fe=green
write_runtime "$RELEASE" "$next_gw" "$next_fe" "$previous" "$gateway_slot" "$frontend_slot"
compose "$RELEASE" config --quiet
# Pull only services that may change. Rollback images must already be recoverable.
compose "$RELEASE" pull worker auth-browser tts-gateway bark "gateway-$next_gw" "frontend-$next_fe" dbtool
load_runtime "$previous"
for ref in "$WORKER_IMAGE_REF" "$BROWSER_IMAGE_REF" "$TTS_IMAGE_REF" "$BARK_IMAGE_REF"; do docker image inspect "$ref" >/dev/null || timeout 90 docker pull "$ref"; done
snapshot="$(mktemp -d "$DEPLOY_PATH/.deploy-snapshot.XXXXXXXX")"
cat "$STATE" | atomic_write_file "$snapshot/previous-state.env" 600
cat "$ROUTE" | atomic_write_file "$snapshot/previous-acb.yml" 600
printf 'RELEASE_SHA=%s\nGATEWAY_SLOT=%s\nFRONTEND_SLOT=%s\n' "$sha" "$next_gw" "$next_fe" | atomic_write_file "$snapshot/target-state.env" 600
printf '%s\n' "$previous" | atomic_write_file "$snapshot/previous-release" 600
mv "$snapshot" "$PENDING"
GATE_OWNER="deploy-$sha-$$"; GATE_TOKEN=''; QUIESCED=0; MUTATING=1; DEADLINE=$((SECONDS+600))
load_runtime "$RELEASE"
DBTOOL_IMAGE_REF="$DBTOOL_IMAGE_REF"
auth="$(dbtool ro -readonly -active-auth-count)"
[[ "$(printf '%s' "$auth" | json_field activeCount)" == 0 ]] || fail 'active authentication blocks deploy'
gate="$(dbtool rw -gate-acquire -owner "$GATE_OWNER" -reason deploy -lease-duration 15m)"
GATE_TOKEN="$(printf '%s' "$gate" | json_field leaseToken)"
[[ -n "$GATE_TOKEN" ]] || fail 'missing mutation lease token'
printf '%s\n%s\n' "$GATE_OWNER" "$GATE_TOKEN" | atomic_write_file "$PENDING/gate-lease" 600
dbtool ro -readonly -gate-check -owner "$GATE_OWNER" >/dev/null
dbtool ro -readonly -check >/dev/null
renew
remaining=$((DEADLINE-SECONDS)); (( remaining > 0 )) || fail 'snapshot deadline expired'
receipt="$(timeout --foreground --signal=TERM --kill-after=5s "$remaining" env RELEASE_COMMIT="$sha" DBTOOL_IMAGE_REF="$DBTOOL_IMAGE_REF" bash "$HERE/backup-db.sh" --snapshot)"
printf '%s\n' "$receipt" | atomic_write_file "$RELEASE/backup-receipt-path" 600
renew
remaining=$((DEADLINE-SECONDS)); (( remaining > 0 )) || fail 'migration deadline expired'
DBTOOL_TIMEOUT_SEC="$remaining" dbtool rw -migrate
dbtool ro -readonly -schema-compat -min-version 11
dbtool ro -readonly -check
for svc in auth-browser tts-gateway bark; do
  renew; compose "$RELEASE" up -d --no-deps "$svc"
  case "$svc" in auth-browser) ref="$BROWSER_IMAGE_REF";; tts-gateway) ref="$TTS_IMAGE_REF";; bark) ref="$BARK_IMAGE_REF";; esac
  EXPECTED_IMAGE_REF="$ref" "$HERE/healthcheck.sh" container "acb-$svc" 120
done
for svc in "frontend-$next_fe" "gateway-$next_gw"; do
  renew; compose "$RELEASE" up -d --no-deps "$svc"
  if [[ "$svc" == frontend-* ]]; then ref="$FRONTEND_IMAGE_REF"; else ref="$GATEWAY_IMAGE_REF"; fi
  EXPECTED_IMAGE_REF="$ref" EXPECTED_SLOT="${svc##*-}" EXPECTED_RELEASE_SHA="$sha" "$HERE/healthcheck.sh" container "acb-$svc" 120
done
"$HERE/render-route.sh" "$next_gw" "$next_fe" > "$PENDING/candidate-acb.yml"
validate_route "$PENDING/candidate-acb.yml" "$next_gw" "$next_fe"
renew; route_replace "$PENDING/candidate-acb.yml"
"$HERE/healthcheck.sh" route "$next_gw" "$sha" "$sha"
renew
load_runtime "$previous"; old_worker="$WORKER_IMAGE_REF"
load_runtime "$RELEASE"; worker_switch "$previous" "$RELEASE" "$old_worker" "$WORKER_IMAGE_REF"
renew
check_running "$RELEASE" "$next_gw" "$next_fe" "$sha"
renew
public_smoke
cat "$PENDING/target-state.env" | atomic_write_file "$STATE" 600
COMMITTED=1; MUTATING=0
release_gate
for svc in "gateway-$gateway_slot" "frontend-$frontend_slot"; do docker stop "acb-$svc" >/dev/null; done
[[ ! -d "$ROLLBACK" ]] || mv "$ROLLBACK" "$DEPLOY_PATH/data/rollback-$(date -u +%Y%m%d%H%M%S)-$$"
mv "$PENDING" "$ROLLBACK"
log_info "committed $sha gateway=$next_gw frontend=$next_fe"
