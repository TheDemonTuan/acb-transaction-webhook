#!/usr/bin/env bash
set -Eeuo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
test_tmp="$(mktemp -d)"
trap 'rm -rf "$test_tmp"' EXIT

pass_count=0
fail_count=0

assert_success() {
  local desc="$1"
  shift
  if "$@"; then
    printf '  [PASS] %s\n' "$desc"
    pass_count=$((pass_count + 1))
  else
    printf '  [FAIL] %s (expected success, got exit code %d)\n' "$desc" "$?" >&2
    fail_count=$((fail_count + 1))
  fi
}

assert_failure() {
  local desc="$1"
  shift
  if "$@" >/dev/null 2>&1; then
    printf '  [FAIL] %s (expected failure, but succeeded)\n' "$desc" >&2
    fail_count=$((fail_count + 1))
  else
    printf '  [PASS] %s\n' "$desc"
    pass_count=$((pass_count + 1))
  fi
}

printf "========================================\n"
printf "Running Supply-Chain & Manifest Tests\n"
printf "========================================\n\n"

# ----------------------------------------------------
# 1. CVE Allowlist Validator Tests
# ----------------------------------------------------
printf "1. Testing CVE Allowlist Validator (validate-cve-allowlist.sh)...\n"

# 1.1 Production allowlist file
assert_success "Production deploy/cve-allowlist.json passes validation" \
  bash "$script_dir/validate-cve-allowlist.sh" --file "$script_dir/cve-allowlist.json"

# 1.2 Valid exception with future expiry
cat <<'EOF' > "$test_tmp/cve-valid.json"
{
  "version": 1,
  "exceptions": [
    {
      "cve": "CVE-2026-12345",
      "reason": "Test non-critical risk accepted with mitigation",
      "owner": "security@tuannguyenviet.site",
      "expiry": "2027-12-31"
    }
  ]
}
EOF
assert_success "Valid future exception passes" \
  bash "$script_dir/validate-cve-allowlist.sh" --file "$test_tmp/cve-valid.json" --reference-date "2026-09-13"

# 1.3 Expired exception
cat <<'EOF' > "$test_tmp/cve-expired.json"
{
  "version": 1,
  "exceptions": [
    {
      "cve": "CVE-2024-99999",
      "reason": "Old exception",
      "owner": "security@tuannguyenviet.site",
      "expiry": "2025-01-01"
    }
  ]
}
EOF
assert_failure "Expired exception is rejected" \
  bash "$script_dir/validate-cve-allowlist.sh" --file "$test_tmp/cve-expired.json" --reference-date "2026-09-13"

# 1.4 Missing CVE
cat <<'EOF' > "$test_tmp/cve-no-id.json"
{
  "version": 1,
  "exceptions": [
    {
      "reason": "Missing cve field",
      "owner": "security@tuannguyenviet.site",
      "expiry": "2027-12-31"
    }
  ]
}
EOF
assert_failure "Exception missing CVE ID is rejected" \
  bash "$script_dir/validate-cve-allowlist.sh" --file "$test_tmp/cve-no-id.json"

# 1.5 Missing reason
cat <<'EOF' > "$test_tmp/cve-no-reason.json"
{
  "version": 1,
  "exceptions": [
    {
      "cve": "CVE-2026-12345",
      "owner": "security@tuannguyenviet.site",
      "expiry": "2027-12-31"
    }
  ]
}
EOF
assert_failure "Exception missing reason is rejected" \
  bash "$script_dir/validate-cve-allowlist.sh" --file "$test_tmp/cve-no-reason.json"

# 1.6 Missing owner
cat <<'EOF' > "$test_tmp/cve-no-owner.json"
{
  "version": 1,
  "exceptions": [
    {
      "cve": "CVE-2026-12345",
      "reason": "Valid reason",
      "expiry": "2027-12-31"
    }
  ]
}
EOF
assert_failure "Exception missing owner is rejected" \
  bash "$script_dir/validate-cve-allowlist.sh" --file "$test_tmp/cve-no-owner.json"

# 1.7 Invalid date format
cat <<'EOF' > "$test_tmp/cve-bad-date.json"
{
  "version": 1,
  "exceptions": [
    {
      "cve": "CVE-2026-12345",
      "reason": "Valid reason",
      "owner": "security@tuannguyenviet.site",
      "expiry": "not-a-date"
    }
  ]
}
EOF
assert_failure "Exception with invalid expiry date format is rejected" \
  bash "$script_dir/validate-cve-allowlist.sh" --file "$test_tmp/cve-bad-date.json"

# 1.8 Output ignore file
assert_success "Output ignore file contains active CVE" \
  bash -c "bash '$script_dir/validate-cve-allowlist.sh' --file '$test_tmp/cve-valid.json' --reference-date '2026-09-13' --output-ignorefile '$test_tmp/.trivyignore' && grep -q 'CVE-2026-12345' '$test_tmp/.trivyignore'"

# 1.9 Component-specific allowlist filtering
cat <<'EOF' > "$test_tmp/cve-components.json"
{
  "version": 1,
  "exceptions": [
    {
      "cve": "CVE-2026-11111",
      "components": ["gateway"],
      "reason": "Gateway only risk accepted",
      "owner": "security@tuannguyenviet.site",
      "expiry": "2027-12-31"
    },
    {
      "cve": "CVE-2026-22222",
      "components": ["auth-browser"],
      "reason": "Browser only risk accepted",
      "owner": "security@tuannguyenviet.site",
      "expiry": "2027-12-31"
    }
  ]
}
EOF
assert_success "Component-specific ignorefile for gateway includes gateway CVE and excludes auth-browser CVE" \
  bash -c "bash '$script_dir/validate-cve-allowlist.sh' --file '$test_tmp/cve-components.json' --component gateway --reference-date '2026-09-13' --output-ignorefile '$test_tmp/.trivyignore-gw' && grep -q 'CVE-2026-11111' '$test_tmp/.trivyignore-gw' && ! grep -q 'CVE-2026-22222' '$test_tmp/.trivyignore-gw'"

printf "\n"

# ----------------------------------------------------
# 2. Third-Party Bark Policy Tests
# ----------------------------------------------------
printf "2. Testing Third-Party Bark Policy Validator (verify-third-party-policy.sh)...\n"

bark_approved_digest="sha256:32d65b07fa835c99b31a396b77727a04ed058377fc2482da3e9dc7397167ffc4"
bark_approved_image="ghcr.io/finb/bark-server@$bark_approved_digest"

# 2.1 Production allowlist check
assert_success "Production deploy/third-party-allowlist.json approves immutable Bark digest" \
  bash "$script_dir/verify-third-party-policy.sh" \
    --image "$bark_approved_image" \
    --allowlist "$script_dir/third-party-allowlist.json" \
    --reference-date "2026-09-13"

# 2.2 Reject mutable tag
assert_failure "Mutable image tag (latest) is rejected" \
  bash "$script_dir/verify-third-party-policy.sh" \
    --image "ghcr.io/finb/bark-server:latest" \
    --allowlist "$script_dir/third-party-allowlist.json"

# 2.3 Reject unapproved digest
assert_failure "Unapproved digest is rejected" \
  bash "$script_dir/verify-third-party-policy.sh" \
    --image "ghcr.io/finb/bark-server@sha256:0000000000000000000000000000000000000000000000000000000000000000" \
    --allowlist "$script_dir/third-party-allowlist.json"

# 2.4 Reject expired allowlist entry
assert_failure "Expired third-party allowlist entry is rejected" \
  bash "$script_dir/verify-third-party-policy.sh" \
    --image "$bark_approved_image" \
    --allowlist "$script_dir/third-party-allowlist.json" \
    --reference-date "2028-01-01"

printf "\n"

# ----------------------------------------------------
# 3. Release Manifest Generator & Verifier Tests
# ----------------------------------------------------
printf "3. Testing Release Manifest Generator & Verifier...\n"

manifest_test_dir="$test_tmp/bundle"
mkdir -p "$manifest_test_dir"

# Copy real deploy files to test bundle
cp -r "$script_dir"/* "$manifest_test_dir/"
mkdir -p "$manifest_test_dir/failover"
cp -r "$script_dir/../platform/failover"/. "$manifest_test_dir/failover/"
rm -f "$manifest_test_dir/release-manifest.json"* "$manifest_test_dir"/*.tmp*

manifest_out="$manifest_test_dir/release-manifest.json"
valid_sha="a4e71ffe29e97e88df6bf25e449c5a0d032a18cb"
dummy_frontend="ghcr.io/org/frontend@sha256:7777777777777777777777777777777777777777777777777777777777777777"
dummy_gw="ghcr.io/org/gateway@sha256:1111111111111111111111111111111111111111111111111111111111111111"
dummy_worker="ghcr.io/org/worker@sha256:2222222222222222222222222222222222222222222222222222222222222222"
dummy_dbtool="ghcr.io/org/dbtool@sha256:3333333333333333333333333333333333333333333333333333333333333333"
dummy_browser="ghcr.io/org/browser@sha256:4444444444444444444444444444444444444444444444444444444444444444"
dummy_tts="ghcr.io/org/tts@sha256:5555555555555555555555555555555555555555555555555555555555555555"
dummy_bark="$bark_approved_image"

# 3.1 Generate manifest
assert_success "Generate release manifest JSON" \
  bash "$script_dir/generate-release-manifest.sh" \
    --git-sha "$valid_sha" \
    --frontend-image "$dummy_frontend" \
    --gateway-image "$dummy_gw" \
    --worker-image "$dummy_worker" \
    --dbtool-image "$dummy_dbtool" \
    --auth-browser-image "$dummy_browser" \
    --tts-image "$dummy_tts" \
    --bark-image "$dummy_bark" \
    --deploy-dir "$manifest_test_dir" \
    --output "$manifest_out"

# 3.2 Verify generated manifest
assert_success "Verify valid release manifest and bundle checksums" \
  bash "$script_dir/verify-manifest.sh" \
    --manifest "$manifest_out" \
    --deploy-dir "$manifest_test_dir"

# 3.3 Verify with matching runtime gateway image
assert_success "Verify manifest with matching runtime image arguments" \
  bash "$script_dir/verify-manifest.sh" \
    --manifest "$manifest_out" \
    --deploy-dir "$manifest_test_dir" \
    --frontend-image "$dummy_frontend" \
    --gateway-image "$dummy_gw" \
    --worker-image "$dummy_worker" \
    --bark-image "$dummy_bark"

# 3.4 Reject mismatched runtime gateway image
assert_failure "Reject mismatched runtime image argument" \
  bash "$script_dir/verify-manifest.sh" \
    --manifest "$manifest_out" \
    --deploy-dir "$manifest_test_dir" \
    --gateway-image "ghcr.io/org/gateway@sha256:9999999999999999999999999999999999999999999999999999999999999999"

# 3.5 Tampered artifact file detected
printf "tampered content\n" >> "$manifest_test_dir/compose.prod.yaml"
assert_failure "Detect and reject tampered artifact file" \
  bash "$script_dir/verify-manifest.sh" \
    --manifest "$manifest_out" \
    --deploy-dir "$manifest_test_dir"
# Restore
cp "$script_dir/compose.prod.yaml" "$manifest_test_dir/"

# 3.6 Missing artifact file detected
rm "$manifest_test_dir/deploy-warm.sh"
assert_failure "Detect and reject missing artifact file" \
  bash "$script_dir/verify-manifest.sh" \
    --manifest "$manifest_out" \
    --deploy-dir "$manifest_test_dir"
# Restore
cp "$script_dir/deploy-warm.sh" "$manifest_test_dir/"

# 3.7 Verify worker RPC compatibility version
assert_success "Verify manifest with matching worker RPC compatibility version 2" \
  bash "$script_dir/verify-manifest.sh" \
    --manifest "$manifest_out" \
    --deploy-dir "$manifest_test_dir" \
    --expected-rpc-version 2

# 3.8 Reject mismatched worker RPC compatibility version
assert_failure "Reject manifest with mismatched worker RPC compatibility version" \
  bash "$script_dir/verify-manifest.sh" \
    --manifest "$manifest_out" \
    --deploy-dir "$manifest_test_dir" \
    --expected-rpc-version 1

# 3.9 Anti-replay: Reject candidate manifest older than currently deployed timestamp
assert_failure "Reject candidate manifest older than currently deployed timestamp (anti-replay)" \
  bash "$script_dir/verify-manifest.sh" \
    --manifest "$manifest_out" \
    --deploy-dir "$manifest_test_dir" \
    --current-deployed-time "2099-01-01T00:00:00Z"

# 3.10 Anti-replay: Reject candidate manifest replaying same commit without allow-redeploy
assert_failure "Reject replaying already deployed commit without --allow-redeploy" \
  bash "$script_dir/verify-manifest.sh" \
    --manifest "$manifest_out" \
    --deploy-dir "$manifest_test_dir" \
    --current-deployed-commit "$valid_sha"

# 3.11 Anti-replay: Allow redeploy with --allow-redeploy flag
assert_success "Allow redeploying commit when --allow-redeploy is explicitly specified" \
  bash "$script_dir/verify-manifest.sh" \
    --manifest "$manifest_out" \
    --deploy-dir "$manifest_test_dir" \
    --current-deployed-commit "$valid_sha" \
    --allow-redeploy

# 3.12 Promotion scope: Manifest with gateway promotion passes --require-promotion-scope gateway
assert_success "Verify manifest authorizes required gateway promotion scope" \
  bash "$script_dir/verify-manifest.sh" \
    --manifest "$manifest_out" \
    --deploy-dir "$manifest_test_dir" \
    --require-promotion-scope "gateway"

# 3.13 Promotion scope: Gateway-only manifest rejects --require-promotion-scope schema
gw_only_manifest="$test_tmp/manifest-gw-only.json"
assert_success "Generate gateway-only scoped release manifest" \
  bash "$script_dir/generate-release-manifest.sh" \
    --git-sha "$valid_sha" \
    --promotion-scope '{"promotion":{"frontend":false,"gateway":true,"worker":false,"schema":false,"auth_browser":false,"tts":false,"bark":false,"platform":false},"promotion_scope":["gateway"]}' \
    --frontend-image "$dummy_frontend" \
    --gateway-image "$dummy_gw" \
    --worker-image "$dummy_worker" \
    --dbtool-image "$dummy_dbtool" \
    --auth-browser-image "$dummy_browser" \
    --tts-image "$dummy_tts" \
    --bark-image "$dummy_bark" \
    --deploy-dir "$manifest_test_dir" \
    --output "$gw_only_manifest"

assert_failure "Reject promotion when required schema component is not authorized by signed manifest" \
  bash "$script_dir/verify-manifest.sh" \
    --manifest "$gw_only_manifest" \
    --deploy-dir "$manifest_test_dir" \
    --require-promotion-scope "schema"

# 3.14 Cosign verification: Missing Cosign binary fails closed when --require-cosign is set (GATE-15)
assert_failure "Missing Cosign on host causes signed manifest verification to fail closed (GATE-15)" \
  env PATH="/usr/bin:/bin" bash "$script_dir/verify-manifest.sh" \
    --manifest "$manifest_out" \
    --deploy-dir "$manifest_test_dir" \
    --require-cosign

# 3.15 Cosign verification: Wildcard expected certificate identity is rejected fail-closed
touch "$manifest_test_dir/release-manifest.bundle"
assert_failure "Wildcard certificate identity (.*) is rejected fail-closed in production verification" \
  bash "$script_dir/verify-manifest.sh" \
    --manifest "$manifest_out" \
    --bundle "$manifest_test_dir/release-manifest.bundle" \
    --deploy-dir "$manifest_test_dir" \
    --expected-identity ".*" \
    --require-cosign
rm -f "$manifest_test_dir/release-manifest.bundle"

printf "\n"

# ----------------------------------------------------
# 4. Actions Pinning & Promotion Scope Classifier Tests
# ----------------------------------------------------
printf "4. Testing Actions Pinning and Promotion Scope Classifiers...\n"

# 4.1 All repository workflow actions are pinned
assert_success "All repository workflow actions are pinned to 40-character SHAs" \
  bash "$script_dir/../scripts/verify-actions-pinned.sh"

# 4.2 Negative test: unpinned action fails verification
cat <<'EOF' > "$test_tmp/unpinned_workflow.yml"
name: Test
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
EOF
assert_failure "Unpinned action tag in workflow is detected and rejected" \
  bash "$script_dir/../scripts/verify-actions-pinned.sh" --file "$test_tmp/unpinned_workflow.yml"

# 4.3 Promotion scope classifier test suite passes
assert_success "Component promotion scope test suite passes (test-promotion-scope.sh)" \
  bash "$script_dir/../scripts/test-promotion-scope.sh"

printf "\n"

# ----------------------------------------------------
# 5. Compose Immutability & Missing Digest Fail-Closed (GATE-14)
# ----------------------------------------------------
printf "5. Testing Compose Immutability & Missing Digest Fail-Closed (GATE-14)...\n"

assert_success "Compose immutability policy tests pass (test_compose_policy.sh)" \
  bash "$script_dir/tests/test_compose_policy.sh"

assert_success "Compose runtime isolation policy tests pass (test_runtime_policy.sh)" \
  bash "$script_dir/tests/test_runtime_policy.sh"

printf "\n========================================\n"
printf "Results: %d passed, %d failed\n" "$pass_count" "$fail_count"
printf "========================================\n"

if [[ $fail_count -gt 0 ]]; then
  exit 1
fi
exit 0
