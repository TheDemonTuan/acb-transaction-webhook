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

while [[ $# -gt 0 ]]; do
  case "$1" in
    --release-dir) RELEASE_DIR="$2"; shift 2 ;;
    --runtime-deploy-dir) RUNTIME_DEPLOY_DIR="$2"; shift 2 ;;
    --expected-identity) EXPECTED_IDENTITY="$2"; shift 2 ;;
    --expected-issuer) EXPECTED_ISSUER="$2"; shift 2 ;;
    --require-cosign) REQUIRE_COSIGN=1; shift ;;
    --skip-manifest-check) SKIP_MANIFEST_CHECK=1; shift ;;
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

# Step 1: Verify signed candidate manifest using trusted verifier if available
trusted_verifier="$RUNTIME_DEPLOY_DIR/verify-manifest.sh"
if [[ "$SKIP_MANIFEST_CHECK" -eq 0 ]]; then
  verifier="$trusted_verifier"
  if [[ ! -x "$verifier" ]]; then
    verifier="$RELEASE_DIR/verify-manifest.sh"
  fi
  [[ -x "$verifier" ]] || { printf 'No executable manifest verifier found.\n' >&2; exit 1; }

  verify_cmd=(
    "$verifier"
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

# Step 2: Strict allowlist of engine files to bootstrap
ENGINE_ALLOWLIST=(
  "stable-deployer.sh"
  "verify-manifest.sh"
  "runtime-layout.sh"
  "reconcile-release.sh"
  "release-state.py"
)

# Step 3: Verify candidate deployment-engine file hashes against signed manifest
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
    if [[ -n "$expected_hash" ]]; then
      actual_hash="$(sha256sum "$candidate_path" | awk '{print $1}')"
      if [[ "$expected_hash" != "$actual_hash" ]]; then
        printf 'Artifact hash mismatch for %s (expected %s, got %s)\n' "$file" "$expected_hash" "$actual_hash" >&2
        exit 1
      fi
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
done

# Step 4: Staged atomic installation with rollback on pre-recovery failure
staged_files=()
installed_backups=()
cleanup_engine_install() {
  local code=$?
  if [[ "$code" -ne 0 ]]; then
    # If failure happened before reconcile started, rollback installed files
    for backup in "${installed_backups[@]}"; do
      orig="${backup%.previous}"
      if [[ -f "$backup" ]]; then
        mv -f "$backup" "$orig" || true
      fi
    done
  fi
  # Clean up any leftover .next files
  for f in "${staged_files[@]}"; do
    rm -f "$f" || true
  done
}
trap cleanup_engine_install EXIT

for file in "${ENGINE_ALLOWLIST[@]}"; do
  candidate_path="$RELEASE_DIR/$file"
  [[ -f "$candidate_path" ]] || continue

  target_path="$RUNTIME_DEPLOY_DIR/$file"
  next_path="$RUNTIME_DEPLOY_DIR/.${file}.next.$$"
  prev_path="$RUNTIME_DEPLOY_DIR/.${file}.previous"

  mode="0755"
  [[ "$file" != *.py ]] || mode="0644"

  install -m "$mode" "$candidate_path" "$next_path"
  staged_files+=("$next_path")

  # Fsync file and parent dir
  python3 - "$next_path" "$RUNTIME_DEPLOY_DIR" <<'PY'
import os, sys
path, parent = sys.argv[1:3]
with open(path, 'rb') as f:
    os.fsync(f.fileno())
pfd = os.open(parent, os.O_RDONLY | getattr(os, 'O_DIRECTORY', 0))
try:
    os.fsync(pfd)
finally:
    os.close(pfd)
PY

  # Backup existing if present
  if [[ -f "$target_path" ]]; then
    cp -p "$target_path" "$prev_path"
    installed_backups+=("$prev_path")
  fi

  # Atomic rename into runtime deploy dir
  mv -f "$next_path" "$target_path"

  # Fsync parent dir
  python3 - "$RUNTIME_DEPLOY_DIR" <<'PY'
import os, sys
pfd = os.open(sys.argv[1], os.O_RDONLY | getattr(os, 'O_DIRECTORY', 0))
try:
    os.fsync(pfd)
finally:
    os.close(pfd)
PY
done

# Clear trap so installed engine files are preserved during recovery
trap - EXIT

# Step 5: Run recovery-only reconcile using the newly installed engine
runtime_root="$(cd -- "$RUNTIME_DEPLOY_DIR/.." && pwd -P)"
if [[ -f "$RUNTIME_DEPLOY_DIR/reconcile-release.sh" ]]; then
  printf 'Executing recovery-only reconcile with updated deployment engine...\n'
  bash "$RUNTIME_DEPLOY_DIR/reconcile-release.sh" --runtime-root "$runtime_root" --recovery-only
fi

printf 'Deployment engine bootstrap completed successfully.\n'
exit 0
