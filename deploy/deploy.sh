#!/usr/bin/env bash
set -Eeuo pipefail
umask 077
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$HERE/simple-lib.sh"
DEPLOY_PATH="${DEPLOY_PATH:-/opt/bank-event-gateway}"
[[ "$DEPLOY_PATH" == /* && -d "$DEPLOY_PATH" ]] || { fail 'DEPLOY_PATH must be an existing absolute root'; exit 1; }
[[ ! -e "$DEPLOY_PATH/.static-hosting-pending" ]] || { fail 'STATIC_HOSTING_MIGRATION_PENDING'; exit 1; }
[[ -f "$DEPLOY_PATH/state.env" ]] || { fail 'BACKEND_STATE_MIGRATION_REQUIRED'; exit 1; }
mode=deploy; [[ "${1:-}" == --check ]] && { mode=check; shift; }
[[ "${1:-}" == --rollback ]] && { mode=rollback; shift; }
[[ "${1:-}" == --reconcile ]] && { mode=reconcile; shift; }
[[ $# == 1 || ( "$mode" == rollback && $# == 0 ) ]] || { fail 'usage: deploy.sh [--check | --reconcile] SHA | --rollback'; exit 1; }
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
[[ ! -e "$DEPLOY_PATH/.static-hosting-pending" ]] || { fail 'STATIC_HOSTING_MIGRATION_PENDING'; exit 1; }
STATE="$DEPLOY_PATH/state.env"
PENDING="$DEPLOY_PATH/.deploy-pending"
ROLLBACK="$DEPLOY_PATH/rollback"
ROUTE="${ACB_ROUTE_FILE:-$(route_file)}"
DYNAMIC="$(dirname "$ROUTE")"
[[ "$ROUTE" == "$DYNAMIC/acb.yml" && -f "$ROUTE" ]] || { fail 'missing app-owned dynamic route'; exit 1; }
RELEASE="$DEPLOY_PATH/releases/$sha"
IMAGES='RELEASE_SHA GATEWAY_IMAGE_REF WORKER_IMAGE_REF DBTOOL_IMAGE_REF BROWSER_IMAGE_REF TTS_IMAGE_REF BARK_IMAGE_REF'
STATE_KEYS='RELEASE_SHA GATEWAY_SLOT'
RUNTIME_KEYS='IMAGE_REF_BLUE IMAGE_REF_GREEN WORKER_IMAGE_REF BROWSER_IMAGE_REF TTS_IMAGE_REF BARK_IMAGE_REF DBTOOL_IMAGE_REF RELEASE_COMMIT_BLUE RELEASE_COMMIT_GREEN WORKER_RELEASE_COMMIT ENV_FILE SECRETS_DIR BARK_SECRET_GROUP'
validate_state() {
  load_keys "$1" "$STATE_KEYS"
  validate_sha "$RELEASE_SHA"
  [[ "$GATEWAY_SLOT" == blue || "$GATEWAY_SLOT" == green ]] || fail 'invalid gateway slot'
}
validate_bundle() {
  local requested="$1" bundle="$DEPLOY_PATH/releases/$1" key
  [[ -f "$bundle/compose.prod.yaml" && -f "$bundle/deploy.sh" && -f "$bundle/images.env" ]] || fail "incomplete bundle: $bundle"
  load_keys "$bundle/images.env" "$IMAGES"
  [[ "$RELEASE_SHA" == "$requested" ]] || fail 'bundle SHA mismatch'
  for key in GATEWAY_IMAGE_REF WORKER_IMAGE_REF DBTOOL_IMAGE_REF BROWSER_IMAGE_REF TTS_IMAGE_REF BARK_IMAGE_REF; do validate_digest "${!key}" "$key"; done
  [[ -f "$bundle/SHA256SUMS" ]] && (cd "$bundle" && sha256sum -c SHA256SUMS >/dev/null) || [[ ! -f "$bundle/SHA256SUMS" ]] || fail 'bundle contents changed'
  load_recovery_flags "$bundle" || return 1
}
preflight() {
  local path component
  for component in docker python3 sqlite3 flock timeout; do command -v "$component" >/dev/null || fail "missing $component"; done
  python3 -c 'import yaml' || fail 'PyYAML required'
  for path in edge-acb acb-core acb-egress; do docker network inspect "$path" >/dev/null || fail "missing network $path"; done
  for path in bank-event-gateway_gateway_data bank-event-gateway_bark_data; do docker volume inspect "$path" >/dev/null || fail "missing volume $path"; done
  for path in acb worker auth-browser; do [[ ! -e "${FAILOVER_REGISTRY_DIR:-/etc/vps-failover/apps.d}/$path.json" ]] || fail "shared failover still owns $path"; done
  local permission_bundle="$RELEASE"
  if [[ "$mode" == rollback ]]; then
    validate_state "$ROLLBACK/previous-state.env"
    permission_bundle="$DEPLOY_PATH/releases/$RELEASE_SHA"
  fi
  validate_permissions "$permission_bundle"
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
  "$HERE/render-route.sh" blue >/dev/null
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
  for key in IMAGE_REF_BLUE IMAGE_REF_GREEN WORKER_IMAGE_REF BROWSER_IMAGE_REF TTS_IMAGE_REF BARK_IMAGE_REF DBTOOL_IMAGE_REF; do validate_digest "${!key}" "$key"; done
  for key in RELEASE_COMMIT_BLUE RELEASE_COMMIT_GREEN WORKER_RELEASE_COMMIT; do validate_sha "${!key}"; done
}
compose() { compose_release "$1" "$1/runtime.env" "${@:2}"; }
write_runtime() {
  local dest="$1" gw="$2" previous="$3" candidate_gateway="$GATEWAY_IMAGE_REF"
  local candidate_worker="$WORKER_IMAGE_REF" candidate_browser="$BROWSER_IMAGE_REF" candidate_tts="$TTS_IMAGE_REF" candidate_bark="$BARK_IMAGE_REF" candidate_dbtool="$DBTOOL_IMAGE_REF"
  [[ "$gw" == blue || "$gw" == green ]] || return 1
  load_runtime "$previous"
  local -A ref=([blue]="$IMAGE_REF_BLUE" [green]="$IMAGE_REF_GREEN")
  local -A commit=([blue]="$RELEASE_COMMIT_BLUE" [green]="$RELEASE_COMMIT_GREEN")
  ref[$gw]="$candidate_gateway"; commit[$gw]="$sha"
  {
    printf 'IMAGE_REF_BLUE=%s\nIMAGE_REF_GREEN=%s\n' "${ref[blue]}" "${ref[green]}"
    printf 'RELEASE_COMMIT_BLUE=%s\nRELEASE_COMMIT_GREEN=%s\n' "${commit[blue]}" "${commit[green]}"
    printf 'WORKER_IMAGE_REF=%s\nBROWSER_IMAGE_REF=%s\nTTS_IMAGE_REF=%s\nBARK_IMAGE_REF=%s\nDBTOOL_IMAGE_REF=%s\n' "$candidate_worker" "$candidate_browser" "$candidate_tts" "$candidate_bark" "$candidate_dbtool"
    printf 'WORKER_RELEASE_COMMIT=%s\nENV_FILE=%s\nSECRETS_DIR=%s\nBARK_SECRET_GROUP=1000\n' "$sha" "$DEPLOY_PATH/deploy/.env.production" "$DEPLOY_PATH/deploy/secrets"
  } | atomic_write_file "$dest/runtime.env" 600
}
start_recovery_controller() {
  local bundle="$1"
  load_recovery_flags "$bundle" || return 1
  [[ "$AUTH_RECOVERY_ENABLED" == true ]] || return 0
  [[ -z "${GATE_TOKEN:-}" ]] || fail 'release deploy gate before starting recovery controller' || return 1
  load_runtime "$bundle"
  dbtool ro -readonly -schema-compat -min-version 13 >/dev/null || return 1
  import_recovery_credentials "$bundle" "$bundle/runtime.env" || return 1
  compose "$bundle" up -d --no-deps --force-recreate recovery-controller || return 1
  EXPECTED_IMAGE_REF="$WORKER_IMAGE_REF" "$HERE/healthcheck.sh" container acb-recovery-controller 120 || return 1
  container_image_check acb-recovery-controller "$WORKER_IMAGE_REF"
}
# Fence every running-controller reconfiguration, not just disabling.
reconcile_runtime_recovery() {
  local bundle="$1"
  load_recovery_flags "$bundle" || return 1
  if [[ "$(docker inspect -f '{{.State.Running}}' acb-recovery-controller 2>/dev/null || true)" == true ]]; then
    load_runtime "$bundle"
    GATE_OWNER="recovery-reconfigure-$$"; DEADLINE=$((SECONDS+600))
    local gate
    gate="$(dbtool rw -gate-acquire -owner "$GATE_OWNER" -reason recovery-reconfigure -lease-duration 15m)" || { fail 'active authentication blocks recovery reconfiguration; use /acb_pause or wait for expiry'; return 1; }
    GATE_TOKEN="$(printf '%s' "$gate" | json_field leaseToken)"
    [[ -n "$GATE_TOKEN" ]] || fail 'missing recovery-reconfigure lease token' || return 1
    stop_recovery_controller || return 1
    release_gate || return 1
  fi
  start_recovery_controller "$bundle"
}
check_running() {
  local bundle="$1" gw="$2" id="$3" key ref
  local -a services=(gateway-$gw worker auth-browser tts-gateway bark)
  load_runtime "$bundle"
  load_recovery_flags "$bundle" || return 1
  if [[ "${4:-}" != skip-recovery && "$AUTH_RECOVERY_ENABLED" == true ]]; then services+=(recovery-controller); fi
  if [[ "${4:-}" != skip-recovery && "$AUTH_RECOVERY_ENABLED" != true ]] && [[ "$(docker inspect -f '{{.State.Running}}' acb-recovery-controller 2>/dev/null || true)" == true ]]; then
    fail 'disabled recovery controller is still running'; return 1
  fi
  for key in "${services[@]}"; do
    case "$key" in
      gateway-blue) ref="$IMAGE_REF_BLUE";; gateway-green) ref="$IMAGE_REF_GREEN";;
      worker|recovery-controller) ref="$WORKER_IMAGE_REF";; auth-browser) ref="$BROWSER_IMAGE_REF";;
      tts-gateway) ref="$TTS_IMAGE_REF";; bark) ref="$BARK_IMAGE_REF";;
    esac
    container_image_check "acb-$key" "$ref"
    EXPECTED_IMAGE_REF="$ref" EXPECTED_SLOT="${key##*-}" EXPECTED_RELEASE_SHA="$id" "$HERE/healthcheck.sh" container "acb-$key" 120
  done
  "$HERE/healthcheck.sh" route "$gw" "$id"
}
public_smoke() {
  local bank_status viewer_status status
  PUBLIC_SMOKE_TMP="$(mktemp -d "$DEPLOY_PATH/.public-smoke.XXXXXXXX")" || return 1
  if ! bank_status="$(curl -sS --max-time 15 -D "$PUBLIC_SMOKE_TMP/bank.headers" -o /dev/null -w '%{http_code}' "${PUBLIC_ORIGIN%/}/api/v1/connection")" ||
     ! viewer_status="$(curl -sS --max-time 15 -H 'Accept: application/json' -D "$PUBLIC_SMOKE_TMP/viewer.headers" -o "$PUBLIC_SMOKE_TMP/viewer.json" -w '%{http_code}' "${PUBLIC_VIEWER_ORIGIN%/}/api/public/v1/transactions?limit=1")"; then
    rm -rf "$PUBLIC_SMOKE_TMP"; PUBLIC_SMOKE_TMP=''
    fail 'public backend smoke transport failed'; return 1
  fi
  if python3 - "$PUBLIC_SMOKE_TMP" "$bank_status" "$viewer_status" <<'PY'
import email.parser,json,pathlib,sys,urllib.parse
root=pathlib.Path(sys.argv[1])
def headers(name):
    lines=[]
    for line in (root/name).read_text(encoding='iso-8859-1').splitlines():
        if line.startswith('HTTP/'): lines=[]
        else: lines.append(line)
    return email.parser.HeaderParser().parsestr('\n'.join(lines))
try:
    bank=headers('bank.headers')
    location=urllib.parse.urlsplit(bank.get('Location',''))
    if sys.argv[2]!='302' or location.scheme!='https' or location.netloc!='thedemontuan.cloudflareaccess.com' or not location.path.startswith('/cdn-cgi/access/login/'):
        raise ValueError('bank API did not redirect to expected Access login')
    viewer=headers('viewer.headers')
    if sys.argv[3]!='200' or viewer.get_content_type()!='application/json' or viewer.get('cf-mitigated'):
        raise ValueError('viewer API did not return unchallenged HTTP 200 JSON')
    try:
        with (root/'viewer.json').open(encoding='utf-8') as response: page=json.load(response)
    except (ValueError,UnicodeError): raise ValueError('viewer API JSON decode failed') from None
    if not isinstance(page,dict) or not isinstance(page.get('items'),list) or not isinstance(page.get('nextCursor'),str) or not isinstance(page.get('summary'),dict):
        raise ValueError('viewer API JSON contract mismatch')
except (OSError,ValueError) as error:
    # Do not log response bodies, transaction data, or redirect query values.
    print('public backend smoke failed: '+str(error),file=sys.stderr)
    raise SystemExit(1)
PY
  then status=0; else status=1; fi
  rm -rf "$PUBLIC_SMOKE_TMP"; PUBLIC_SMOKE_TMP=''
  [[ "$status" == 0 ]] || return "$status"
  log_info 'public backend API/Access smoke passed'
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
ensure_restore_gate() {
  local gate
  DEADLINE=$((SECONDS+600))
  if [[ -n "${GATE_TOKEN:-}" && -n "${GATE_OWNER:-}" ]] && dbtool rw -gate-renew -owner "$GATE_OWNER" -lease-token "$GATE_TOKEN" -lease-duration 15m >/dev/null 2>&1; then return 0; fi
  # A crash may leave a snapshot before admission, or an expired lease. Never stop
  # a controller until a fresh transactional admission rejects any active login.
  GATE_OWNER="restore-$$"; GATE_TOKEN=''
  gate="$(dbtool rw -gate-acquire -owner "$GATE_OWNER" -reason restore -lease-duration 15m)" || { fail 'restore admission failed; use /acb_pause or wait for active authentication to expire'; return 1; }
  GATE_TOKEN="$(printf '%s' "$gate" | json_field leaseToken)"
  [[ -n "$GATE_TOKEN" ]] || fail 'missing restore lease token'
}
restore_previous() {
  local snap="$1" prev_sha prev_gw prev_bundle svc
  validate_state "$snap/previous-state.env"
  prev_sha="$RELEASE_SHA"; prev_gw="$GATEWAY_SLOT"
  prev_bundle="$DEPLOY_PATH/releases/$prev_sha"
  RESTORED_BUNDLE="$prev_bundle"
  load_runtime "$prev_bundle"
  DBTOOL_IMAGE_REF="$DBTOOL_IMAGE_REF"
  ensure_restore_gate || return 1
  printf '%s\n%s\n' "$GATE_OWNER" "$GATE_TOKEN" | atomic_write_file "$snap/gate-lease" 600 || return 1
  stop_recovery_controller || return 1
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
  svc="gateway-$prev_gw"
  compose "$prev_bundle" up -d --no-deps --no-recreate "$svc" || return 1
  local ref="$IMAGE_REF_BLUE"
  [[ "$prev_gw" != green ]] || ref="$IMAGE_REF_GREEN"
  EXPECTED_IMAGE_REF="$ref" EXPECTED_SLOT="$prev_gw" EXPECTED_RELEASE_SHA="$prev_sha" "$HERE/healthcheck.sh" container "acb-$svc" 120 || return 1
  validate_baseline_route "$snap/previous-acb.yml" "$prev_gw" || return 1
  route_replace "$snap/previous-acb.yml" || return 1
  "$HERE/healthcheck.sh" route "$prev_gw" "$prev_sha" || return 1
  if [[ -f "$STATE" ]] && ! cmp -s "$STATE" "$snap/previous-state.env"; then cat "$snap/previous-state.env" | atomic_write_file "$STATE" 600 || return 1; fi
  local slot=blue; [[ "$prev_gw" != blue ]] || slot=green
  if docker inspect "acb-gateway-$slot" >/dev/null 2>&1; then
    docker stop "acb-gateway-$slot" >/dev/null || return 1
  fi
}
cleanup() {
  local code=$?
  trap - EXIT HUP INT TERM
  [[ -z "${PUBLIC_SMOKE_TMP:-}" ]] || rm -rf "$PUBLIC_SMOKE_TMP"
  if (( code != 0 )) && [[ "${MUTATING:-0}" == 1 && "${COMMITTED:-0}" == 0 ]]; then
    DEADLINE=$((SECONDS+600))
    log_error "deploy failed ($code); restoring previous runtime"
    if ! restore_previous "$PENDING"; then
      log_error 'ROLLBACK_FAILED: retain both HTTP slots and pending evidence'
      code=1
    else
      if [[ "${GATE_TOKEN:-}" ]]; then
        if release_gate && start_recovery_controller "$RESTORED_BUNDLE"; then rm -rf "$PENDING"; else code=1; fi
      else
        if start_recovery_controller "$RESTORED_BUNDLE"; then rm -rf "$PENDING"; else code=1; fi
      fi
    fi
  elif (( code != 0 )) && [[ "${COMMITTED:-0}" == 1 ]]; then log_error 'RETIRE_FAILED: committed state retained; rerun to finish'; fi
  if (( code != 0 )) && [[ "${PREPARED:-0}" == 1 && "${MUTATING:-0}" == 0 && "${COMMITTED:-0}" == 0 ]]; then rm -rf "$PENDING"; fi
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
    validate_baseline_route "$ROUTE" "$GATEWAY_SLOT"
    prev_bundle="$DEPLOY_PATH/releases/$RELEASE_SHA"
    reconcile_runtime_recovery "$prev_bundle"
    "$HERE/healthcheck.sh" route "$GATEWAY_SLOT" "$RELEASE_SHA"
    log_info 'rollback already committed'; exit 0
  fi
  [[ -f "$STATE" ]] && cmp -s "$STATE" "$ROLLBACK/target-state.env" || fail 'rollback state mismatch'
  previous="$DEPLOY_PATH/releases/$RELEASE_SHA"
  [[ -f "$previous/runtime.env" ]] || fail 'rollback bundle missing'
  load_runtime "$previous"
  DBTOOL_IMAGE_REF="$DBTOOL_IMAGE_REF"
  for ref in "$WORKER_IMAGE_REF" "$BROWSER_IMAGE_REF" "$TTS_IMAGE_REF" "$BARK_IMAGE_REF"; do docker image inspect "$ref" >/dev/null || docker pull "$ref"; done
  auth="$(dbtool ro -readonly -active-auth-count)"
  [[ "$(printf '%s' "$auth" | json_field activeCount)" == 0 ]] || fail 'active authentication blocks rollback; use /acb_pause or wait for the attempt to expire'
  dbtool ro -readonly -schema-compat -min-version 13 >/dev/null
  GATE_OWNER="rollback-$$"; GATE_TOKEN=''; DEADLINE=$((SECONDS+600))
  gate="$(dbtool rw -gate-acquire -owner "$GATE_OWNER" -reason rollback -lease-duration 15m)" || { fail 'rollback admission failed; use /acb_pause or wait for active authentication to expire'; exit 1; }
  GATE_TOKEN="$(printf '%s' "$gate" | json_field leaseToken)"
  [[ -n "$GATE_TOKEN" ]] || fail 'missing rollback lease token'
  dbtool ro -readonly -gate-check -owner "$GATE_OWNER" >/dev/null
  if ! restore_previous "$ROLLBACK"; then
    log_error 'ROLLBACK_FAILED: retain both HTTP slots and rollback evidence'
    exit 1
  fi
  release_gate
  start_recovery_controller "$RESTORED_BUNDLE"
  log_info 'rollback committed'
  exit 0
fi
validate_bundle "$sha"
validate_state "$STATE"
current="$RELEASE_SHA"; gateway_slot="$GATEWAY_SLOT"
[[ -d "$PENDING" ]] || validate_baseline_route "$ROUTE" "$gateway_slot"
if [[ "$mode" == reconcile ]]; then
  [[ -f "$STATE" && "$current" == "$sha" && ! -d "$PENDING" ]] || { fail 'setup release changed or pending deployment exists; rerun setup after deploy completes'; exit 1; }
fi
if [[ "$mode" == check ]]; then
  [[ ! -d "$PENDING" ]] || fail 'pending deployment requires recovery before check'
  log_info "preflight passed: $current gateway=$gateway_slot target $sha"
  exit 0
fi
if [[ -d "$PENDING" ]]; then
  if cmp -s "$STATE" "$PENDING/target-state.env"; then
    validate_state "$STATE"
    check_running "$DEPLOY_PATH/releases/$RELEASE_SHA" "$GATEWAY_SLOT" "$RELEASE_SHA" skip-recovery
    public_smoke
    if [[ -f "$PENDING/gate-lease" ]]; then
      { IFS= read -r GATE_OWNER; IFS= read -r GATE_TOKEN; } < "$PENDING/gate-lease"
      load_runtime "$DEPLOY_PATH/releases/$RELEASE_SHA"
      DBTOOL_IMAGE_REF="$DBTOOL_IMAGE_REF"
      release_gate
    fi
    reconcile_runtime_recovery "$DEPLOY_PATH/releases/$RELEASE_SHA"
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
    start_recovery_controller "$RESTORED_BUNDLE"
    rm -rf "$PENDING"
  else fail 'pending deployment state disagrees with committed/previous; evidence retained'; exit 1; fi
fi
if [[ "${COMMITTED:-0}" == 1 ]]; then
  validate_state "$PENDING/previous-state.env"
  docker stop "acb-gateway-$GATEWAY_SLOT" >/dev/null
  [[ ! -d "$ROLLBACK" ]] || mv "$ROLLBACK" "$DEPLOY_PATH/data/rollback-$(date -u +%Y%m%d%H%M%S)-$$"
  mv "$PENDING" "$ROLLBACK"
  log_info 'retire complete'; exit 0
fi
if [[ -f "$STATE" && "$current" == "$sha" ]]; then
  reconcile_runtime_recovery "$RELEASE"
  check_running "$RELEASE" "$gateway_slot" "$sha"
  public_smoke
  log_info 'already committed; no mutation'; exit 0
fi
previous="$DEPLOY_PATH/releases/$current"
next_gw=blue; [[ "$gateway_slot" == blue ]] && next_gw=green
write_runtime "$RELEASE" "$next_gw" "$previous"
compose "$RELEASE" config --quiet
# Pull only services that may change. Rollback images must already be recoverable.
compose "$RELEASE" pull worker auth-browser tts-gateway bark "gateway-$next_gw" dbtool
load_runtime "$previous"
for ref in "$WORKER_IMAGE_REF" "$BROWSER_IMAGE_REF" "$TTS_IMAGE_REF" "$BARK_IMAGE_REF"; do docker image inspect "$ref" >/dev/null || timeout 90 docker pull "$ref"; done
snapshot="$(mktemp -d "$DEPLOY_PATH/.deploy-snapshot.XXXXXXXX")"
cat "$STATE" | atomic_write_file "$snapshot/previous-state.env" 600
cat "$ROUTE" | atomic_write_file "$snapshot/previous-acb.yml" 600
printf 'RELEASE_SHA=%s\nGATEWAY_SLOT=%s\n' "$sha" "$next_gw" | atomic_write_file "$snapshot/target-state.env" 600
printf '%s\n' "$previous" | atomic_write_file "$snapshot/previous-release" 600
mv "$snapshot" "$PENDING"
GATE_OWNER="deploy-$sha-$$"; GATE_TOKEN=''; QUIESCED=0; PREPARED=1; MUTATING=0; DEADLINE=$((SECONDS+600))
load_runtime "$RELEASE"
DBTOOL_IMAGE_REF="$DBTOOL_IMAGE_REF"
auth="$(dbtool ro -readonly -active-auth-count)"
[[ "$(printf '%s' "$auth" | json_field activeCount)" == 0 ]] || fail 'active authentication blocks deploy; use /acb_pause or wait for the attempt to expire'
gate="$(dbtool rw -gate-acquire -owner "$GATE_OWNER" -reason deploy -lease-duration 15m)" || { fail 'deploy admission failed; use /acb_pause or wait for active authentication to expire'; exit 1; }
GATE_TOKEN="$(printf '%s' "$gate" | json_field leaseToken)"
[[ -n "$GATE_TOKEN" ]] || fail 'missing mutation lease token'
printf '%s\n%s\n' "$GATE_OWNER" "$GATE_TOKEN" | atomic_write_file "$PENDING/gate-lease" 600
dbtool ro -readonly -gate-check -owner "$GATE_OWNER" >/dev/null
MUTATING=1
stop_recovery_controller
dbtool ro -readonly -check >/dev/null
renew
remaining=$((DEADLINE-SECONDS)); (( remaining > 0 )) || fail 'snapshot deadline expired'
receipt="$(timeout --foreground --signal=TERM --kill-after=5s "$remaining" env RELEASE_COMMIT="$sha" DBTOOL_IMAGE_REF="$DBTOOL_IMAGE_REF" bash "$HERE/backup-db.sh" --snapshot)"
printf '%s\n' "$receipt" | atomic_write_file "$RELEASE/backup-receipt-path" 600
renew
remaining=$((DEADLINE-SECONDS)); (( remaining > 0 )) || fail 'migration deadline expired'
DBTOOL_TIMEOUT_SEC="$remaining" dbtool rw -migrate
dbtool ro -readonly -schema-compat -min-version 13
dbtool ro -readonly -check
for svc in auth-browser tts-gateway bark; do
  renew; compose "$RELEASE" up -d --no-deps "$svc"
  case "$svc" in auth-browser) ref="$BROWSER_IMAGE_REF";; tts-gateway) ref="$TTS_IMAGE_REF";; bark) ref="$BARK_IMAGE_REF";; esac
  EXPECTED_IMAGE_REF="$ref" "$HERE/healthcheck.sh" container "acb-$svc" 120
done
renew; compose "$RELEASE" up -d --no-deps "gateway-$next_gw"
EXPECTED_IMAGE_REF="$GATEWAY_IMAGE_REF" EXPECTED_SLOT="$next_gw" EXPECTED_RELEASE_SHA="$sha" "$HERE/healthcheck.sh" container "acb-gateway-$next_gw" 120
"$HERE/render-route.sh" "$next_gw" > "$PENDING/candidate-acb.yml"
validate_route "$PENDING/candidate-acb.yml" "$next_gw"
renew; route_replace "$PENDING/candidate-acb.yml"
"$HERE/healthcheck.sh" route "$next_gw" "$sha"
renew
load_runtime "$previous"; old_worker="$WORKER_IMAGE_REF"
load_runtime "$RELEASE"; worker_switch "$previous" "$RELEASE" "$old_worker" "$WORKER_IMAGE_REF"
renew
check_running "$RELEASE" "$next_gw" "$sha" skip-recovery
renew
public_smoke
cat "$PENDING/target-state.env" | atomic_write_file "$STATE" 600
COMMITTED=1; MUTATING=0
release_gate
start_recovery_controller "$RELEASE"
docker stop "acb-gateway-$gateway_slot" >/dev/null
[[ ! -d "$ROLLBACK" ]] || mv "$ROLLBACK" "$DEPLOY_PATH/data/rollback-$(date -u +%Y%m%d%H%M%S)-$$"
mv "$PENDING" "$ROLLBACK"
log_info "committed $sha gateway=$next_gw"
