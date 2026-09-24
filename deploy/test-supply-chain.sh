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

printf 'Running Supply-Chain Policy Tests\n\n'

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

printf "\n========================================\n"
printf "Results: %d passed, %d failed\n" "$pass_count" "$fail_count"
printf "========================================\n"

if [[ $fail_count -gt 0 ]]; then
  exit 1
fi
exit 0
