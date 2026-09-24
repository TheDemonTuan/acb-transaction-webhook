#!/usr/bin/env bash
# deploy/preflight-scan-images.sh
# Scans pinned production images with Trivy to detect security drift before build decisions.
set -Eeuo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
state_file=""
output_dir="preflight-reports"
allowlist_file="$script_dir/cve-allowlist.json"
third_party_allowlist="$script_dir/third-party-allowlist.json"

usage() {
  cat <<'EOF'
Usage: preflight-scan-images.sh [options]

Options:
  --state <path>          Path to production-state.env (required)
  --output-dir <path>     Directory to save JSON scan reports (default: preflight-reports)
  --allowlist <path>      Path to cve-allowlist.json (default: deploy/cve-allowlist.json)
  --third-party <path>    Path to third-party-allowlist.json (default: deploy/third-party-allowlist.json)
  --help, -h              Show help
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --state)
      state_file="$2"
      shift 2
      ;;
    --output-dir)
      output_dir="$2"
      shift 2
      ;;
    --allowlist)
      allowlist_file="$2"
      shift 2
      ;;
    --third-party)
      third_party_allowlist="$2"
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

if [[ -z "$state_file" || ! -f "$state_file" ]]; then
  printf 'Error: Missing or invalid production state file: %s\n' "$state_file" >&2
  exit 1
fi

mkdir -p "$output_dir"

# Load image refs from production-state.env
declare -A images
while IFS='=' read -r key val; do
  [[ -z "$key" || "$key" =~ ^# ]] && continue
  case "$key" in
    frontend_image) images[frontend]="$val" ;;
    gateway_image) images[gateway]="$val" ;;
    worker_image) images[worker]="$val" ;;
    dbtool_image) images[schema]="$val" ;;
    auth_browser_image) images[auth_browser]="$val" ;;
    tts_image) images[tts]="$val" ;;
    bark_image) images[bark]="$val" ;;
  esac
done < "$state_file"

# Verify all 7 image digests are immutable
for comp in frontend gateway worker schema auth_browser tts bark; do
  img="${images[$comp]:-}"
  if [[ ! "$img" =~ ^[^[:space:]]+@sha256:[a-f0-9]{64}$ ]]; then
    printf 'Error: Invalid or missing immutable digest for %s: %s\n' "$comp" "$img" >&2
    exit 1
  fi
done

# Generate component ignorefiles from cve-allowlist.json
declare -A ignore_targets=(
  [frontend]="frontend"
  [gateway]="gateway"
  [worker]="worker"
  [schema]="dbtool"
  [auth_browser]="auth-browser"
  [tts]="tts-gateway"
)

for comp in frontend gateway worker schema auth_browser tts; do
  target="${ignore_targets[$comp]}"
  bash "$script_dir/validate-cve-allowlist.sh" \
    --file "$allowlist_file" \
    --component "$target" \
    --output-ignorefile "$output_dir/.trivyignore-${comp}"
done

# Prepare summary table if GITHUB_STEP_SUMMARY is available
summary_file="${GITHUB_STEP_SUMMARY:-}"
if [[ -n "$summary_file" ]]; then
  {
    echo "### Production Images Preflight Scan (Security Drift Detection)"
    echo ""
    echo "| Component | Pinned Production Digest | Status | Finding | Action |"
    echo "|---|---|---|---|---|"
  } >> "$summary_file"
fi

set_output() {
  local key="$1"
  local val="$2"
  if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
    echo "${key}=${val}" >> "$GITHUB_OUTPUT"
  fi
  printf 'PREFLIGHT_OUTPUT: %s=%s\n' "$key" "$val"
}

analyze_report() {
  local report_path="$1"
  local ignore_path="$2"
  node - "$report_path" "$ignore_path" <<'JSEOF'
const fs = require('fs');
const reportPath = process.argv[2];
const ignorePath = process.argv[3];

let ignored = new Set();
if (fs.existsSync(ignorePath)) {
  const lines = fs.readFileSync(ignorePath, 'utf8').split('\n');
  for (const line of lines) {
    const trimmed = line.trim();
    if (trimmed && !trimmed.startsWith('#')) {
      ignored.add(trimmed);
    }
  }
}

let data;
try {
  data = JSON.parse(fs.readFileSync(reportPath, 'utf8'));
} catch (err) {
  process.stderr.write(`Failed to parse Trivy report JSON: ${err.message}\n`);
  process.exit(2);
}

const actionable = [];
const unfixable = [];

if (data.Results) {
  for (const res of data.Results) {
    if (res.Vulnerabilities) {
      for (const v of res.Vulnerabilities) {
        if ((v.Severity === 'HIGH' || v.Severity === 'CRITICAL') && !ignored.has(v.VulnerabilityID)) {
          if (v.FixedVersion && v.FixedVersion.trim() !== '') {
            actionable.push({
              cve: v.VulnerabilityID,
              pkg: v.PkgName || '',
              installed: v.InstalledVersion || '',
              fixed: v.FixedVersion || '',
              severity: v.Severity || ''
            });
          } else {
            unfixable.push({
              cve: v.VulnerabilityID,
              pkg: v.PkgName || '',
              installed: v.InstalledVersion || '',
              severity: v.Severity || ''
            });
          }
        }
      }
    }
  }
}

process.stdout.write(JSON.stringify({
  actionable,
  unfixable,
  rebuild: actionable.length > 0
}));
JSEOF
}

blocking_exit=0

# Scan first-party images
for comp in frontend gateway worker schema auth_browser tts; do
  img="${images[$comp]}"
  report_json="$output_dir/preflight-${comp}.json"
  ignore_file="$output_dir/.trivyignore-${comp}"

  printf 'Scanning %s (%s)...\n' "$comp" "$img"
  if ! trivy image \
    --platform linux/arm64 \
    --scanners vuln \
    --format json \
    --output "$report_json" \
    "$img"; then
    printf 'Error: Trivy scan execution failed for %s (%s)\n' "$comp" "$img" >&2
    exit 1
  fi

  analysis="$(analyze_report "$report_json" "$ignore_file")"
  rebuild="$(node -e 'console.log(JSON.parse(process.argv[1]).rebuild)' "$analysis")"

  digest_short="${img##*@}"
  digest_short="${digest_short:0:19}..."

  if [[ "$rebuild" == "true" ]]; then
    set_output "security_rebuild_${comp}" "true"
    # shellcheck disable=SC2016
    cve_details="$(node -e '
      const act = JSON.parse(process.argv[1]).actionable;
      console.log(act.map(a => `${a.cve} (${a.pkg} ${a.installed} -> ${a.fixed})`).join(", "));
    ' "$analysis")"
    if [[ -n "$summary_file" ]]; then
      echo "| ${comp} | \`${digest_short}\` | ⚠️ DRIFT DETECTED | ${cve_details} | Auto-rebuild triggered (no-cache) |" >> "$summary_file"
    fi
  else
    set_output "security_rebuild_${comp}" "false"
    if [[ -n "$summary_file" ]]; then
      echo "| ${comp} | \`${digest_short}\` | ✅ CLEAN | 0 actionable CVEs | Retain production image |" >> "$summary_file"
    fi
  fi
done

# Scan third-party Bark image (governed by third-party-allowlist.json)
bark_img="${images[bark]}"
bark_report="$output_dir/preflight-bark.json"
printf 'Verifying Bark third-party policy (%s)...\n' "$bark_img"
bash "$script_dir/verify-third-party-policy.sh" \
  --image "$bark_img" \
  --allowlist "$third_party_allowlist"

printf 'Scanning Bark image with Trivy (%s)...\n' "$bark_img"
if ! trivy image \
  --platform linux/arm64 \
  --scanners vuln \
  --format json \
  --output "$bark_report" \
  "$bark_img"; then
  printf 'Error: Trivy scan execution failed for Bark (%s)\n' "$bark_img" >&2
  exit 1
fi

bark_analysis="$(analyze_report "$bark_report" "/dev/null")"
bark_critical="$(node -e '
  const data = JSON.parse(process.argv[1]);
  const crit = data.actionable.filter(v => v.severity === "CRITICAL").concat(data.unfixable.filter(v => v.severity === "CRITICAL"));
  console.log(crit.length > 0 ? "true" : "false");
' "$bark_analysis")"

bark_digest_short="${bark_img##*@}"
bark_digest_short="${bark_digest_short:0:19}..."

if [[ "$bark_critical" == "true" ]]; then
  set_output "bark_status" "vulnerable"
  blocking_exit=1
  if [[ -n "$summary_file" ]]; then
    echo "| bark | \`${bark_digest_short}\` | ❌ CRITICAL CVE | Unexempted CRITICAL in 3rd-party image | Block deployment (upstream fix required) |" >> "$summary_file"
  fi
else
  set_output "bark_status" "clean"
  if [[ -n "$summary_file" ]]; then
    echo "| bark | \`${bark_digest_short}\` | ✅ CLEAN | 0 CRITICAL CVEs | Retain 3rd-party image |" >> "$summary_file"
  fi
fi

exit "$blocking_exit"
