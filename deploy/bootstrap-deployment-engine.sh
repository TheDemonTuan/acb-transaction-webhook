#!/usr/bin/env bash
# One-time / bootstrap deployment engine updater.
# Updates trusted deployment-engine files from a signed release bundle
# without touching containers, routes, database, secrets, or application release state.
set -Eeuo pipefail
umask 077

RELEASE_DIR=""
RUNTIME_DEPLOY_DIR=""
EXPECTED_IDENTITY=""
EXPECTED_ISSUER="https://token.actions.githubusercontent.com"
REQUIRE_COSIGN=0
SKIP_MANIFEST_CHECK=0
DO_RECONCILE=0
SKIP_SEMANTIC_CHECK=0
ALLOW_PARTIAL=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --release-dir) RELEASE_DIR="$2"; shift 2 ;;
    --runtime-deploy-dir) RUNTIME_DEPLOY_DIR="$2"; shift 2 ;;
    --expected-identity) EXPECTED_IDENTITY="$2"; shift 2 ;;
    --expected-issuer) EXPECTED_ISSUER="$2"; shift 2 ;;
    --require-cosign) REQUIRE_COSIGN=1; shift ;;
    --skip-manifest-check) SKIP_MANIFEST_CHECK=1; shift ;;
    --reconcile) DO_RECONCILE=1; shift ;;
    --no-reconcile) DO_RECONCILE=0; shift ;;
    --skip-semantic-check) SKIP_SEMANTIC_CHECK=1; shift ;;
    --allow-partial) ALLOW_PARTIAL=1; shift ;;
    *) printf 'Unknown bootstrap argument: %s\n' "$1" >&2; exit 1 ;;
  esac
done

[[ -n "$RELEASE_DIR" && -d "$RELEASE_DIR" ]] || { printf 'Release directory is required and must exist.\n' >&2; exit 1; }
[[ -n "$RUNTIME_DEPLOY_DIR" && -d "$RUNTIME_DEPLOY_DIR" ]] || { printf 'Runtime deploy directory is required and must exist.\n' >&2; exit 1; }

RELEASE_DIR="$(cd -- "$RELEASE_DIR" && pwd -P)"
RUNTIME_DEPLOY_DIR="$(cd -- "$RUNTIME_DEPLOY_DIR" && pwd -P)"

manifest_file="$RELEASE_DIR/release-manifest.json"
bundle_file="$RELEASE_DIR/release-manifest.bundle"

[[ -f "$manifest_file" ]] || { printf 'Manifest file missing in release directory: %s\n' "$manifest_file" >&2; exit 1; }

# Step 1: Verify signed candidate manifest using Cosign blob verification (schema-light)
# No untrusted candidate scripts are executed prior to signature and artifact verification.
if [[ "$SKIP_MANIFEST_CHECK" -eq 0 ]]; then
  if [[ -f "$bundle_file" ]] || [[ "$REQUIRE_COSIGN" -eq 1 ]]; then
    [[ -f "$bundle_file" ]] || { printf 'Error: Cosign bundle file missing: %s\n' "$bundle_file" >&2; exit 1; }
    command -v cosign >/dev/null 2>&1 || { printf 'Error: cosign binary not found in PATH.\n' >&2; exit 1; }
    [[ -n "$EXPECTED_IDENTITY" && "$EXPECTED_IDENTITY" != '*' && "$EXPECTED_IDENTITY" != '.*' ]] || {
      printf 'Error: Exact signing identity is required.\n' >&2
      exit 1
    }
    cosign_args=(
      verify-blob
      --bundle "$bundle_file"
      --certificate-identity "$EXPECTED_IDENTITY"
      --certificate-oidc-issuer "$EXPECTED_ISSUER"
      "$manifest_file"
    )
    if ! cosign "${cosign_args[@]}"; then
      printf 'Error: Cosign manifest signature verification failed for %s\n' "$manifest_file" >&2
      exit 1
    fi
    printf 'Cosign manifest blob signature verified successfully with identity: %s\n' "$EXPECTED_IDENTITY"
  fi
fi

# Step 2: Schema-light manifest validation
# Only extracts release_id, git_sha, and artifacts dictionary.
# Does not depend on application image keys (e.g. tts vs tts_gateway).
manifest_meta="$(python3 - "$manifest_file" <<'PY'
import json, sys
try:
    with open(sys.argv[1], 'r', encoding='utf-8') as f:
        data = json.load(f)
except Exception as e:
    sys.stderr.write(f"Invalid manifest JSON: {e}\n")
    sys.exit(1)

release_id = data.get("release_id", "")
git_sha = data.get("git_sha", "")
artifacts = data.get("artifacts", {})

if not release_id:
    sys.stderr.write("Missing release_id in manifest\n")
    sys.exit(1)

if not isinstance(artifacts, dict):
    sys.stderr.write("manifest.artifacts must be a dictionary\n")
    sys.exit(1)

print(release_id)
print(git_sha)
PY
)" || { printf 'Error: Failed to parse candidate manifest metadata.\n' >&2; exit 1; }

release_id="$(printf '%s\n' "$manifest_meta" | sed -n '1p')"
git_sha="$(printf '%s\n' "$manifest_meta" | sed -n '2p')"

# Step 3: Required engine artifacts & full engine allowlist
REQUIRED_ENGINE_ARTIFACTS=(
  "stable-deployer.sh"
  "verify-manifest.sh"
  "runtime-layout.sh"
  "reconcile-release.sh"
  "release-state.py"
)

ENGINE_ALLOWLIST=(
  "stable-deployer.sh"
  "verify-manifest.sh"
  "runtime-layout.sh"
  "reconcile-release.sh"
  "release-state.py"
  "verify-runtime-drift.sh"
  "release-env.sh"
  "edge-probe.sh"
  "lib.sh"
  "lib/common.sh"
  "lib/database.sh"
  "lib/images.sh"
  "lib/recovery.sh"
  "lib/rollout-journal.sh"
  "lib/state.sh"
  "lib/traefik.sh"
  "preflight-vps.sh"
  "preflight-runtime.sh"
  "bootstrap-deployment-engine.sh"
)

# If full manifest check is active and partial artifacts are not explicitly permitted,
# verify that all required core engine artifacts are present in manifest.artifacts.
if [[ "$SKIP_MANIFEST_CHECK" -eq 0 && "$ALLOW_PARTIAL" -eq 0 ]]; then
  for req in "${REQUIRED_ENGINE_ARTIFACTS[@]}"; do
    req_file="$RELEASE_DIR/$req"
    if [[ ! -f "$req_file" || -L "$req_file" ]]; then
      printf 'Error: Required engine artifact missing or symlink: %s\n' "$req" >&2
      exit 1
    fi
    has_hash="$(python3 - "$manifest_file" "$req" <<'PY'
import json, sys
data = json.load(open(sys.argv[1], encoding="utf-8"))
artifacts = data.get("artifacts", {})
print(1 if sys.argv[2] in artifacts else 0)
PY
)"
    if [[ "$has_hash" -ne 1 ]]; then
      printf 'Error: Required engine artifact %s missing from manifest artifacts.\n' "$req" >&2
      exit 1
    fi
  done
fi

# Step 4: Verify candidate deployment-engine file hashes against signed manifest
verified_files=()
for file in "${ENGINE_ALLOWLIST[@]}"; do
  candidate_path="$RELEASE_DIR/$file"
  [[ -f "$candidate_path" ]] || continue

  # Verify file is not a symlink
  [[ ! -L "$candidate_path" ]] || { printf 'Engine file %s must not be a symlink.\n' "$file" >&2; exit 1; }

  # Check hash against manifest artifacts
  if [[ "$SKIP_MANIFEST_CHECK" -eq 0 ]]; then
    expected_hash="$(python3 - "$manifest_file" "$file" <<'PY'
import json, sys
data = json.load(open(sys.argv[1], encoding="utf-8"))
artifacts = data.get("artifacts", {})
print(artifacts.get(sys.argv[2], ""))
PY
)"
    if [[ -z "$expected_hash" ]]; then
      printf 'Engine file %s not present in manifest artifacts.\n' "$file" >&2
      exit 1
    fi

    actual_hash="$(sha256sum "$candidate_path" | awk '{print $1}')"
    if [[ "$expected_hash" != "$actual_hash" ]]; then
      printf 'Artifact hash mismatch for %s (expected %s, got %s)\n' "$file" "$expected_hash" "$actual_hash" >&2
      exit 1
    fi
  fi

  # Self-test candidate engine syntax
  case "$file" in
    *.sh)
      bash -n "$candidate_path" || { printf 'Syntax check failed for %s\n' "$candidate_path" >&2; exit 1; }
      ;;
    *.py)
      python3 -m py_compile "$candidate_path" || { printf 'Syntax check failed for %s\n' "$candidate_path" >&2; exit 1; }
      ;;
  esac

  verified_files+=("$file")
done

[[ ${#verified_files[@]} -gt 0 ]] || {
  printf 'Error: No valid deployment engine files found to bootstrap.\n' >&2
  exit 1
}

# Step 5: Version the entire verified engine atomically
runtime_root="${RUNTIME_ROOT:-$(cd -- "$RUNTIME_DEPLOY_DIR/.." && pwd -P)}"
engines_dir="$runtime_root/engines"
version_dir="$engines_dir/$release_id"
mkdir -p "$version_dir"

for file in "${verified_files[@]}"; do
  candidate_path="$RELEASE_DIR/$file"
  target_ver_path="$version_dir/$file"
  mkdir -p "$(dirname "$target_ver_path")"
  mode="0755"
  [[ "$file" != *.py ]] || mode="0644"
  install -m "$mode" "$candidate_path" "$target_ver_path"
done

# Atomically switch current symlink in engines directory
current_link="$engines_dir/current"
tmp_link="$engines_dir/.current.tmp.$$"
if ln -sfn "$version_dir" "$tmp_link" 2>/dev/null || ln -sfn "$release_id" "$tmp_link" 2>/dev/null; then
  mv -f "$tmp_link" "$current_link" 2>/dev/null || ln -sfn "$version_dir" "$current_link" 2>/dev/null || {
    printf 'Error: Could not switch trusted engine current link.\n' >&2
    exit 1
  }
fi
if [[ "${OS:-}" != "Windows_NT" ]]; then
  [[ -L "$current_link" ]] || { printf 'Error: Trusted engine current link was not installed.\n' >&2; exit 1; }
fi

# Step 6: Staged atomic installation into RUNTIME_DEPLOY_DIR with rollback on failure
staged_files=()
installed_backups=()
cleanup_engine_install() {
  local code=$?
  if [[ "$code" -ne 0 ]]; then
    for backup in "${installed_backups[@]}"; do
      orig="${backup%.previous}"
      if [[ -f "$backup" ]]; then
        mv -f "$backup" "$orig" || true
      fi
    done
  fi
  for f in "${staged_files[@]}"; do
    rm -f "$f" || true
  done
}
trap cleanup_engine_install EXIT

for file in "${verified_files[@]}"; do
  candidate_path="$RELEASE_DIR/$file"
  target_path="$RUNTIME_DEPLOY_DIR/$file"
  flat_name="${file//\//_}"
  next_path="$RUNTIME_DEPLOY_DIR/.${flat_name}.next.$$"
  prev_path="$RUNTIME_DEPLOY_DIR/.${flat_name}.previous"

  mkdir -p "$(dirname "$target_path")"
  mode="0755"
  [[ "$file" != *.py ]] || mode="0644"

  install -m "$mode" "$candidate_path" "$next_path"
  staged_files+=("$next_path")

  # Fsync file and parent dir
  python3 - "$next_path" "$RUNTIME_DEPLOY_DIR" <<'PY'
import os, sys
path, parent = sys.argv[1:3]
try:
    with open(path, 'r+b') as f:
        os.fsync(f.fileno())
except OSError:
    pass
try:
    pfd = os.open(parent, os.O_RDONLY | getattr(os, 'O_DIRECTORY', 0))
    try:
        os.fsync(pfd)
    finally:
        os.close(pfd)
except OSError:
    pass
PY

  # Backup existing if present
  if [[ -f "$target_path" ]]; then
    mv -f "$target_path" "$prev_path"
    installed_backups+=("$prev_path")
  fi

  # Atomic rename
  mv -f "$next_path" "$target_path"

  # Fsync parent dir
  python3 - "$RUNTIME_DEPLOY_DIR" <<'PY'
import os, sys
try:
    pfd = os.open(sys.argv[1], os.O_RDONLY | getattr(os, 'O_DIRECTORY', 0))
    try:
        os.fsync(pfd)
    finally:
        os.close(pfd)
except OSError:
    pass
PY
done

# Clear trap so installed engine files are preserved
trap - EXIT

# Step 7: Execute the new semantic verifier only AFTER the engine switch has succeeded
if [[ "$SKIP_MANIFEST_CHECK" -eq 0 && "$SKIP_SEMANTIC_CHECK" -eq 0 ]]; then
  new_verifier="$RUNTIME_DEPLOY_DIR/verify-manifest.sh"
  if [[ -x "$new_verifier" ]]; then
    printf 'Executing new semantic verifier after engine upgrade...\n'
    verify_cmd=(
      "$new_verifier"
      --manifest "$manifest_file"
      --bundle "$bundle_file"
      --deploy-dir "$RELEASE_DIR"
      --expected-issuer "$EXPECTED_ISSUER"
    )
    if [[ -n "$EXPECTED_IDENTITY" ]]; then
      verify_cmd+=(--expected-identity "$EXPECTED_IDENTITY")
    fi
    if [[ "$REQUIRE_COSIGN" -eq 1 ]]; then
      verify_cmd+=(--require-cosign)
    fi
    "${verify_cmd[@]}"
  fi
fi

# Step 8: Optional recovery-only reconcile using the newly installed engine
if [[ "$DO_RECONCILE" -eq 1 && -f "$RUNTIME_DEPLOY_DIR/reconcile-release.sh" ]]; then
  printf 'Executing recovery-only reconcile with updated deployment engine...\n'
  export SKIP_MANIFEST_CHECK
  bash "$RUNTIME_DEPLOY_DIR/reconcile-release.sh" --runtime-root "$runtime_root" --recovery-only
fi

printf 'Deployment engine bootstrap completed successfully.\n'
exit 0
