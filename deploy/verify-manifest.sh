#!/usr/bin/env bash
set -Eeuo pipefail

# verify-manifest.sh
# Validates release manifest schema, image digest immutability, deployed artifact checksums,
# Cosign cryptographic signatures with exact signer identity, and anti-replay constraints.

manifest_file=""
bundle_file=""
deploy_dir=""

expected_gateway=""
expected_worker=""
expected_dbtool=""
expected_browser=""
expected_tts=""
expected_bark=""

expected_rpc_version=""
expected_schema_version=""
expected_identity=""
expected_issuer="https://token.actions.githubusercontent.com"
require_cosign=0

current_deployed_commit=""
current_deployed_time=""
max_age_seconds=0
allow_redeploy=0
required_scope=""
output_env_file=""

usage() {
  cat <<'EOF'
Usage: verify-manifest.sh [options]

Options:
  --manifest <path>               Path to release-manifest.json
  --bundle <path>                 Path to release-manifest.bundle
  --deploy-dir <dir>              Directory containing deployed files (default: manifest directory)
  --gateway-image <ref>           Expected gateway image digest (optional)
  --worker-image <ref>            Expected worker image digest (optional)
  --dbtool-image <ref>            Expected dbtool image digest (optional)
  --browser-image <ref>           Expected auth-browser image digest (optional)
  --tts-image <ref>               Expected tts-gateway image digest (optional)
  --bark-image <ref>              Expected bark image digest (optional)
  --expected-rpc-version <ver>    Expected worker RPC version (optional)
  --expected-schema-version <ver> Expected schema version (optional)
  --expected-identity <id>        Exact expected Cosign certificate identity (required when Cosign used)
  --expected-issuer <issuer>      Expected Cosign OIDC issuer (default: github actions)
  --require-cosign                Require Cosign signature verification to succeed (fails closed if missing)
  --current-deployed-commit <sha> Commit SHA of currently deployed release (anti-replay check)
  --current-deployed-time <iso>   Timestamp of currently deployed release (anti-replay check)
  --max-age-seconds <sec>         Reject manifests generated more than <sec> ago (anti-stale check)
  --allow-redeploy                Allow re-deploying the currently active commit
  --require-promotion-scope <sc>  Require manifest to authorize promotion of component <sc>
  --output-env <path>             Write verified manifest variables to file
  --help, -h                      Show help
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --manifest)
      manifest_file="$2"
      shift 2
      ;;
    --bundle)
      bundle_file="$2"
      shift 2
      ;;
    --deploy-dir)
      deploy_dir="$2"
      shift 2
      ;;
    --gateway-image)
      expected_gateway="$2"
      shift 2
      ;;
    --worker-image)
      expected_worker="$2"
      shift 2
      ;;
    --dbtool-image)
      expected_dbtool="$2"
      shift 2
      ;;
    --browser-image)
      expected_browser="$2"
      shift 2
      ;;
    --tts-image)
      expected_tts="$2"
      shift 2
      ;;
    --bark-image)
      expected_bark="$2"
      shift 2
      ;;
    --expected-rpc-version)
      expected_rpc_version="$2"
      shift 2
      ;;
    --expected-schema-version)
      expected_schema_version="$2"
      shift 2
      ;;
    --expected-identity)
      expected_identity="$2"
      shift 2
      ;;
    --expected-issuer)
      expected_issuer="$2"
      shift 2
      ;;
    --require-cosign)
      require_cosign=1
      shift
      ;;
    --current-deployed-commit)
      current_deployed_commit="$2"
      shift 2
      ;;
    --current-deployed-time)
      current_deployed_time="$2"
      shift 2
      ;;
    --max-age-seconds)
      max_age_seconds="$2"
      shift 2
      ;;
    --allow-redeploy)
      allow_redeploy=1
      shift
      ;;
    --require-promotion-scope)
      required_scope="$2"
      shift 2
      ;;
    --output-env)
      output_env_file="$2"
      shift 2
      ;;
    --help|-h)
      usage
      exit 0
      ;;
    *)
      printf 'Unknown argument: %s\n' "$1" >&2
      usage >&2
      exit 1
      ;;
  esac
done

if [[ -z "$manifest_file" ]]; then
  if [[ -f "release-manifest.json" ]]; then
    manifest_file="release-manifest.json"
  elif [[ -f "deploy/release-manifest.json" ]]; then
    manifest_file="deploy/release-manifest.json"
  else
    printf 'Error: --manifest file path is required.\n' >&2
    exit 1
  fi
fi

if [[ ! -f "$manifest_file" ]]; then
  printf 'Error: release manifest file not found: %s\n' "$manifest_file" >&2
  exit 1
fi

if [[ -z "$deploy_dir" ]]; then
  deploy_dir="$(cd -- "$(dirname -- "$manifest_file")" && pwd)"
fi

if [[ -z "$bundle_file" ]]; then
  candidate_bundle="${manifest_file%.json}.bundle"
  if [[ -f "$candidate_bundle" ]]; then
    bundle_file="$candidate_bundle"
  fi
fi

compute_hash() {
  local target="$1"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$target" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$target" | awk '{print $1}'
  else
    printf 'Error: neither sha256sum nor shasum found\n' >&2
    exit 1
  fi
}

# 1. Cosign Signature Verification
if [[ $require_cosign -eq 1 ]] || [[ -n "$bundle_file" && -f "$bundle_file" ]]; then
  if ! command -v cosign >/dev/null 2>&1; then
    printf 'Error: Cosign verification required but cosign binary not found in PATH\n' >&2
    exit 1
  fi

  if [[ -z "$bundle_file" || ! -f "$bundle_file" ]]; then
    printf 'Error: Cosign verification required but signature bundle not found: %s\n' "$bundle_file" >&2
    exit 1
  fi

  # Reject empty, wildcard or regex identity in production mode
  if [[ -z "$expected_identity" || "$expected_identity" == ".*" || "$expected_identity" == "*" ]]; then
    printf 'Error: exact expected certificate identity is required (wildcard identity rejected in production)\n' >&2
    exit 1
  fi

  printf 'Verifying Cosign signature bundle: %s\n' "$bundle_file"
  cosign_args=(
    verify-blob
    --bundle "$bundle_file"
    --certificate-identity "$expected_identity"
    --certificate-oidc-issuer "$expected_issuer"
    "$manifest_file"
  )

  if ! cosign "${cosign_args[@]}"; then
    printf 'Error: Cosign manifest signature verification failed for %s\n' "$manifest_file" >&2
    exit 1
  fi
  printf 'Cosign manifest blob signature verified successfully with identity: %s\n' "$expected_identity"
fi

# 2. Manifest JSON Schema and Content Verification
validate_and_extract() {
  if command -v node >/dev/null 2>&1; then
    node - "$manifest_file" \
      "$expected_gateway" "$expected_worker" "$expected_dbtool" "$expected_browser" "$expected_tts" "$expected_bark" \
      "$current_deployed_commit" "$current_deployed_time" "$max_age_seconds" "$allow_redeploy" "$required_scope" <<'JSEOF'
const fs = require('fs');

const [
  ,, manifestPath,
  expGw, expWk, expDb, expBr, expTts, expBk,
  curDepCommit, curDepTime, maxAgeSecStr, allowRedeployStr, reqScope
] = process.argv;

const maxAgeSec = parseInt(maxAgeSecStr, 10) || 0;
const allowRedeploy = (allowRedeployStr === '1');

let manifest;
try {
  manifest = JSON.parse(fs.readFileSync(manifestPath, 'utf8'));
} catch (e) {
  console.error(`Error: invalid JSON in ${manifestPath}: ${e.message}`);
  process.exit(1);
}

// 2.1 Git SHA format check
if (!manifest.git_sha || !/^[0-9a-fA-F]{40}$/.test(manifest.git_sha)) {
  console.error(`Error: invalid or missing git_sha in manifest: ${manifest.git_sha}`);
  process.exit(1);
}

// 2.2 Timestamp and Anti-replay check
if (!manifest.created_at || isNaN(Date.parse(manifest.created_at))) {
  console.error(`Error: invalid or missing created_at timestamp in manifest: ${manifest.created_at}`);
  process.exit(1);
}

const manifestTime = new Date(manifest.created_at).getTime();
const now = Date.now();

// Reject future timestamps (> 300s clock skew)
if (manifestTime > now + 300 * 1000) {
  console.error(`Error: manifest created_at is in the future (${manifest.created_at}); potential clock skew or tampering`);
  process.exit(1);
}

// Reject stale manifests if max-age-seconds is set
if (maxAgeSec > 0) {
  const ageSec = (now - manifestTime) / 1000;
  if (ageSec > maxAgeSec) {
    console.error(`Error: manifest is stale (age: ${Math.round(ageSec)}s > max allowed: ${maxAgeSec}s)`);
    process.exit(1);
  }
}

// Anti-replay: compare against current deployed commit
if (curDepCommit && curDepCommit === manifest.git_sha && !allowRedeploy) {
  console.error(`Error: anti-replay rejected: candidate git_sha ${manifest.git_sha} is already currently deployed`);
  process.exit(1);
}

// Anti-replay: compare against current deployed timestamp
if (curDepTime) {
  const depTime = new Date(curDepTime).getTime();
  if (!isNaN(depTime) && manifestTime <= depTime && !allowRedeploy) {
    console.error(`Error: anti-replay rejected: candidate created_at (${manifest.created_at}) is older than or equal to current deployment (${curDepTime})`);
    process.exit(1);
  }
}

// 2.3 Compatibility section
if (!manifest.compatibility || typeof manifest.compatibility !== 'object') {
  console.error('Error: missing compatibility section in manifest');
  process.exit(1);
}

if (typeof manifest.compatibility.schema_version !== 'number') {
  console.error('Error: missing or invalid schema_version in compatibility section');
  process.exit(1);
}

if (typeof manifest.compatibility.worker_rpc_version !== 'number') {
  console.error('Error: missing or invalid worker_rpc_version in compatibility section');
  process.exit(1);
}

// 2.4 Promotion scope section
if (manifest.promotion && typeof manifest.promotion !== 'object') {
  console.error('Error: invalid promotion section in manifest');
  process.exit(1);
}

if (reqScope) {
  const isAuthorized = (manifest.promotion && manifest.promotion[reqScope] === true) ||
                       (Array.isArray(manifest.promotion_scope) && manifest.promotion_scope.includes(reqScope));
  if (!isAuthorized) {
    console.error(`Error: promotion scope '${reqScope}' is NOT authorized by the signed manifest`);
    process.exit(1);
  }
}

// 2.5 Images section
if (!manifest.images || typeof manifest.images !== 'object') {
  console.error('Error: missing images section in manifest');
  process.exit(1);
}

const requiredImages = ['gateway', 'worker', 'dbtool', 'auth_browser', 'tts_gateway', 'bark'];
const expectedMap = {
  gateway: expGw,
  worker: expWk,
  dbtool: expDb,
  auth_browser: expBr,
  tts_gateway: expTts,
  bark: expBk
};
const digestRe = /^[^ \t\r\n]+@sha256:[a-f0-9]{64}$/;

for (const key of requiredImages) {
  const img = manifest.images[key];
  if (!img || !digestRe.test(img)) {
    console.error(`Error: image '${key}' is missing or not an immutable sha256 digest: ${img}`);
    process.exit(1);
  }
  const exp = expectedMap[key];
  if (exp && exp !== img) {
    console.error(`Error: runtime image mismatch for '${key}': expected=${exp}, manifest=${img}`);
    process.exit(1);
  }
}

// 2.6 Artifacts section
if (!manifest.artifacts || typeof manifest.artifacts !== 'object') {
  console.error('Error: missing artifacts section in manifest');
  process.exit(1);
}

// Output parsed values for bash consumer
console.log(`GIT_SHA=${manifest.git_sha}`);
console.log(`RELEASE_ID=${manifest.release_id || ''}`);
console.log(`CREATED_AT=${manifest.created_at}`);
console.log(`COMPAT_SCHEMA=${manifest.compatibility.schema_version}`);
console.log(`COMPAT_RPC=${manifest.compatibility.worker_rpc_version}`);

if (manifest.promotion) {
  for (const [k, v] of Object.entries(manifest.promotion)) {
    console.log(`PROMOTION_${k.toUpperCase()}=${v === true ? 'true' : 'false'}`);
  }
}
if (Array.isArray(manifest.promotion_scope)) {
  console.log(`PROMOTION_SCOPE=${manifest.promotion_scope.join(',')}`);
}
const isDocOnly = (manifest.promotion_doc_only === true) ||
  (Array.isArray(manifest.promotion_scope) && manifest.promotion_scope.length === 0) ||
  (manifest.promotion && Object.values(manifest.promotion).every(v => v === false));
console.log(`PROMOTION_DOC_ONLY=${isDocOnly ? 'true' : 'false'}`);

for (const key of requiredImages) {
  console.log(`IMAGE_${key.toUpperCase()}=${manifest.images[key]}`);
}

for (const [artFile, artHash] of Object.entries(manifest.artifacts)) {
  console.log(`ARTIFACT:${artFile}=${artHash}`);
}
JSEOF
  elif command -v python3 >/dev/null 2>&1; then
    python3 - "$manifest_file" \
      "$expected_gateway" "$expected_worker" "$expected_dbtool" "$expected_browser" "$expected_tts" "$expected_bark" \
      "$current_deployed_commit" "$current_deployed_time" "$max_age_seconds" "$allow_redeploy" "$required_scope" <<'PYEOF'
import sys, json, re, datetime

(
  manifest_path,
  exp_gw, exp_wk, exp_db, exp_br, exp_tts, exp_bk,
  cur_dep_commit, cur_dep_time, max_age_sec_str, allow_redeploy_str, req_scope
) = sys.argv[1:13]

max_age_sec = int(max_age_sec_str) if max_age_sec_str.isdigit() else 0
allow_redeploy = (allow_redeploy_str == "1")

try:
    with open(manifest_path, "r", encoding="utf-8") as f:
        manifest = json.load(f)
except Exception as e:
    print(f"Error: invalid JSON in {manifest_path}: {e}", file=sys.stderr)
    sys.exit(1)

git_sha = manifest.get("git_sha", "")
if not re.match(r"^[0-9a-fA-F]{40}$", git_sha):
    print(f"Error: invalid or missing git_sha in manifest: {git_sha}", file=sys.stderr)
    sys.exit(1)

created_at_str = manifest.get("created_at", "")
try:
    # Accept ISO 8601 UTC
    clean_ts = created_at_str.replace("Z", "+00:00")
    created_dt = datetime.datetime.fromisoformat(clean_ts)
    if created_dt.tzinfo is None:
        created_dt = created_dt.replace(tzinfo=datetime.timezone.utc)
except Exception as e:
    print(f"Error: invalid or missing created_at timestamp in manifest: {created_at_str}", file=sys.stderr)
    sys.exit(1)

now = datetime.datetime.now(datetime.timezone.utc)
if created_dt > now + datetime.timedelta(seconds=300):
    print(f"Error: manifest created_at is in the future ({created_at_str}); clock skew or tampering", file=sys.stderr)
    sys.exit(1)

if max_age_sec > 0:
    age = (now - created_dt).total_seconds()
    if age > max_age_sec:
        print(f"Error: manifest is stale (age: {int(age)}s > max allowed: {max_age_sec}s)", file=sys.stderr)
        sys.exit(1)

if cur_dep_commit and cur_dep_commit == git_sha and not allow_redeploy:
    print(f"Error: anti-replay rejected: candidate git_sha {git_sha} is already currently deployed", file=sys.stderr)
    sys.exit(1)

if cur_dep_time:
    try:
        clean_dep = cur_dep_time.replace("Z", "+00:00")
        dep_dt = datetime.datetime.fromisoformat(clean_dep)
        if dep_dt.tzinfo is None:
            dep_dt = dep_dt.replace(tzinfo=datetime.timezone.utc)
        if created_dt <= dep_dt and not allow_redeploy:
            print(f"Error: anti-replay rejected: candidate created_at ({created_at_str}) is older than or equal to current deployment ({cur_dep_time})", file=sys.stderr)
            sys.exit(1)
    except Exception as e:
        pass

compat = manifest.get("compatibility")
if not isinstance(compat, dict):
    print("Error: missing compatibility section in manifest", file=sys.stderr)
    sys.exit(1)

if not isinstance(compat.get("schema_version"), int) or not isinstance(compat.get("worker_rpc_version"), int):
    print("Error: missing or invalid schema_version or worker_rpc_version", file=sys.stderr)
    sys.exit(1)

promotion = manifest.get("promotion", {})
promotion_scope = manifest.get("promotion_scope", [])
if req_scope:
    auth = (isinstance(promotion, dict) and promotion.get(req_scope) is True) or (isinstance(promotion_scope, list) and req_scope in promotion_scope)
    if not auth:
        print(f"Error: promotion scope '{req_scope}' is NOT authorized by the signed manifest", file=sys.stderr)
        sys.exit(1)

images = manifest.get("images")
if not isinstance(images, dict):
    print("Error: missing images section in manifest", file=sys.stderr)
    sys.exit(1)

required = ["gateway", "worker", "dbtool", "auth_browser", "tts_gateway", "bark"]
expected_map = {
    "gateway": exp_gw,
    "worker": exp_wk,
    "dbtool": exp_db,
    "auth_browser": exp_br,
    "tts_gateway": exp_tts,
    "bark": exp_bk
}
digest_re = re.compile(r"^[^ \t\r\n]+@sha256:[a-f0-9]{64}$")

for k in required:
    img = images.get(k, "")
    if not digest_re.match(img):
        print(f"Error: image '{k}' is missing or not an immutable sha256 digest: {img}", file=sys.stderr)
        sys.exit(1)
    exp = expected_map.get(k)
    if exp and exp != img:
        print(f"Error: runtime image mismatch for '{k}': expected={exp}, manifest={img}", file=sys.stderr)
        sys.exit(1)

artifacts = manifest.get("artifacts")
if not isinstance(artifacts, dict):
    print("Error: missing artifacts section in manifest", file=sys.stderr)
    sys.exit(1)

print(f"GIT_SHA={git_sha}")
print(f"RELEASE_ID={manifest.get('release_id', '')}")
print(f"CREATED_AT={created_at_str}")
print(f"COMPAT_SCHEMA={compat.get('schema_version')}")
print(f"COMPAT_RPC={compat.get('worker_rpc_version')}")

if isinstance(promotion, dict):
    for k, v in promotion.items():
        print(f"PROMOTION_{k.upper()}={'true' if v is True else 'false'}")
if isinstance(promotion_scope, list):
    print(f"PROMOTION_SCOPE={','.join(promotion_scope)}")
is_doc_only = (
    manifest.get("promotion_doc_only") is True or
    (isinstance(promotion_scope, list) and len(promotion_scope) == 0) or
    (isinstance(promotion, dict) and all(v is False for v in promotion.values()))
)
print(f"PROMOTION_DOC_ONLY={'true' if is_doc_only else 'false'}")

for k in required:
    print(f"IMAGE_{k.upper()}={images[k]}")

for art_file, art_hash in artifacts.items():
    print(f"ARTIFACT:{art_file}={art_hash}")
PYEOF
  else
    printf 'Error: node or python3 required to parse release manifest\n' >&2
    exit 1
  fi
}

parsed_output="$(validate_and_extract)"

# 3. Check compatibility arguments against parsed values
if [[ -n "$expected_rpc_version" ]]; then
  actual_rpc_version="$(printf '%s\n' "$parsed_output" | grep '^COMPAT_RPC=' | cut -d'=' -f2)"
  if [[ "$actual_rpc_version" != "$expected_rpc_version" ]]; then
    printf 'Error: worker RPC compatibility version mismatch: expected=%s actual=%s (gateway and worker must be promoted together)\n' "$expected_rpc_version" "$actual_rpc_version" >&2
    exit 1
  fi
fi

if [[ -n "$expected_schema_version" ]]; then
  actual_schema_version="$(printf '%s\n' "$parsed_output" | grep '^COMPAT_SCHEMA=' | cut -d'=' -f2)"
  if [[ "$actual_schema_version" != "$expected_schema_version" ]]; then
    printf 'Error: schema compatibility version mismatch: expected=%s actual=%s\n' "$expected_schema_version" "$actual_schema_version" >&2
    exit 1
  fi
fi

# 4. Verify artifact checksums against files on disk
artifact_count=0
while IFS= read -r line; do
  if [[ "$line" =~ ^ARTIFACT:(.*)=(.*)$ ]]; then
    art_file="${BASH_REMATCH[1]}"
    expected_hash="${BASH_REMATCH[2]}"

    target_path=""
    if [[ -f "$deploy_dir/$art_file" ]]; then
      target_path="$deploy_dir/$art_file"
    elif [[ -f "$art_file" ]]; then
      target_path="$art_file"
    elif [[ -f "$deploy_dir/$(basename "$art_file")" ]]; then
      target_path="$deploy_dir/$(basename "$art_file")"
    else
      printf 'Error: artifact file listed in manifest not found on disk: %s (checked in %s)\n' "$art_file" "$deploy_dir" >&2
      exit 1
    fi

    actual_hash="$(compute_hash "$target_path")"
    if [[ "$actual_hash" != "$expected_hash" ]]; then
      printf 'Error: artifact checksum mismatch for %s: expected=%s actual=%s\n' "$art_file" "$expected_hash" "$actual_hash" >&2
      exit 1
    fi
    artifact_count=$((artifact_count + 1))
  fi
done <<< "$parsed_output"

if [[ -n "$output_env_file" ]]; then
  mkdir -p "$(dirname "$output_env_file")"
  printf '%s\n' "$parsed_output" | grep -v '^ARTIFACT:' > "$output_env_file"
fi

manifest_git_sha="$(printf '%s\n' "$parsed_output" | grep '^GIT_SHA=' | cut -d'=' -f2)"
printf 'Release manifest preflight OK: git_sha=%s, 6 images verified, %d artifact checksums verified.\n' "$manifest_git_sha" "$artifact_count"
exit 0
