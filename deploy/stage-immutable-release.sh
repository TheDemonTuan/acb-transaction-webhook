#!/usr/bin/env bash
# deploy/stage-immutable-release.sh
# Collision-safe atomic immutable release staging helper.
# Installs attempt-specific upload directory atomically. Reuses existing release
# only when signed manifest and complete artifact hashes match; refuses collision;
# cleans up only its own .upload-run-attempt directory without removing existing releases
# or generic .upload directories.
set -Eeuo pipefail
umask 027

# Re-exec if running from inside the staging directory being moved/cleaned
staging_arg=""
reexec_file=""
if [[ "${1:-}" == "--internal-reexec" ]]; then
  reexec_file="$2"
  shift 2
else
  for ((i=1; i<=$#; i++)); do
    arg="${!i}"
    if [[ "$arg" == "--staging-dir" ]]; then
      next_idx=$((i + 1))
      staging_arg="${!next_idx:-}"
      break
    elif [[ -z "$staging_arg" && "$arg" != --* && "$arg" != -* ]]; then
      staging_arg="$arg"
    fi
  done

  if [[ -n "$staging_arg" ]]; then
    script_canon="$(realpath "$0" 2>/dev/null || readlink -f "$0" 2>/dev/null || echo "$0")"
    staging_canon="$(realpath "$staging_arg" 2>/dev/null || readlink -f "$staging_arg" 2>/dev/null || echo "$staging_arg")"
    if [[ -n "$staging_canon" && "$script_canon" == "$staging_canon"/* ]]; then
      tmp_runner="$(mktemp "${TMPDIR:-/tmp}/stage-runner.XXXXXX.sh")"
      cp "$0" "$tmp_runner"
      chmod 700 "$tmp_runner"
      exec bash "$tmp_runner" --internal-reexec "$tmp_runner" "$@"
    fi
  fi
fi

STAGING_DIR=""
RELEASE_DIR=""
EXPECTED_IDENTITY=""
EXPECTED_ISSUER="https://token.actions.githubusercontent.com"
REQUIRE_COSIGN=0
SKIP_COSIGN=0
CLEANUP_ONLY=0

usage() {
  cat <<'EOF'
Usage: stage-immutable-release.sh [options]

Options:
  --staging-dir <dir>       Attempt-specific upload staging directory (required)
  --release-dir <dir>       Target canonical release directory (required)
  --expected-identity <id>  Expected Cosign certificate identity (optional)
  --expected-issuer <url>   Expected Cosign OIDC issuer (default: github actions)
  --require-cosign          Enforce Cosign signature verification (fails if missing)
  --skip-cosign             Bypass Cosign signature verification
  --cleanup-only            Safely clean only own staging directory and exit
  --help, -h                Show this help message
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --staging-dir)
      STAGING_DIR="$2"
      shift 2
      ;;
    --release-dir)
      RELEASE_DIR="$2"
      shift 2
      ;;
    --expected-identity)
      EXPECTED_IDENTITY="$2"
      shift 2
      ;;
    --expected-issuer)
      EXPECTED_ISSUER="$2"
      shift 2
      ;;
    --require-cosign)
      REQUIRE_COSIGN=1
      shift
      ;;
    --skip-cosign)
      SKIP_COSIGN=1
      shift
      ;;
    --cleanup-only|--clean-staging-only)
      CLEANUP_ONLY=1
      shift
      ;;
    --help|-h)
      usage
      exit 0
      ;;
    *)
      if [[ -z "$STAGING_DIR" && "$1" != --* ]]; then
        STAGING_DIR="$1"
        shift
      elif [[ -z "$RELEASE_DIR" && "$1" != --* ]]; then
        RELEASE_DIR="$1"
        shift
      else
        printf 'Unknown argument: %s\n' "$1" >&2
        usage >&2
        exit 1
      fi
      ;;
  esac
done

# shellcheck disable=SC2329
cleanup_self() {
  if [[ -n "$reexec_file" && -f "$reexec_file" ]]; then
    rm -f "$reexec_file" 2>/dev/null || true
  fi
}

installed=0
# shellcheck disable=SC2329
cleanup() {
  local exit_code=$?
  if [[ "$installed" -eq 0 && -n "$STAGING_DIR" && -d "$STAGING_DIR" && ! -L "$STAGING_DIR" ]]; then
    local sname
    sname="$(basename "$STAGING_DIR")"
    if [[ "$sname" != ".upload" && "$sname" != "$(basename "${RELEASE_DIR:-}")" && "$sname" =~ ^\..*\.upload- ]]; then
      rm -rf "$STAGING_DIR" 2>/dev/null || true
    fi
  fi
  cleanup_self
  exit "$exit_code"
}
trap cleanup EXIT

if [[ "$CLEANUP_ONLY" -eq 1 ]]; then
  [[ -n "$STAGING_DIR" ]] || { printf 'Error: --staging-dir is required for cleanup\n' >&2; exit 1; }
  sname="$(basename "$STAGING_DIR")"
  if [[ "$sname" == ".upload" ]]; then
    printf 'Refusing to clean generic .upload directory: %s\n' "$STAGING_DIR" >&2
    exit 1
  fi
  if [[ ! "$sname" =~ ^\..*\.upload- ]]; then
    printf 'Refusing cleanup: not an attempt-specific staging directory: %s\n' "$sname" >&2
    exit 1
  fi
  if [[ -d "$STAGING_DIR" && ! -L "$STAGING_DIR" ]]; then
    rm -rf "$STAGING_DIR"
    printf 'Cleaned up staging directory: %s\n' "$STAGING_DIR"
  fi
  exit 0
fi

[[ -n "$STAGING_DIR" ]] || { printf 'Error: --staging-dir is required\n' >&2; usage >&2; exit 1; }
[[ -n "$RELEASE_DIR" ]] || { printf 'Error: --release-dir is required\n' >&2; usage >&2; exit 1; }

[[ -e "$STAGING_DIR" ]] || { printf 'Error: staging directory does not exist: %s\n' "$STAGING_DIR" >&2; exit 1; }
[[ -d "$STAGING_DIR" && ! -L "$STAGING_DIR" ]] || { printf 'Error: staging directory is not a directory: %s\n' "$STAGING_DIR" >&2; exit 1; }

staging_canon="$(cd -- "$STAGING_DIR" && pwd -P)"
case "$staging_canon" in
  /|/tmp|/var/tmp)
    printf 'Error: unsafe staging directory: %s\n' "$staging_canon" >&2
    exit 1
    ;;
esac

sname="$(basename "$staging_canon")"
if [[ "$sname" == ".upload" ]]; then
  printf 'Error: generic .upload directory cannot be used as attempt staging directory: %s\n' "$sname" >&2
  exit 1
fi

if [[ ! "$sname" =~ ^\..*\.upload- ]]; then
  printf 'Error: staging directory must follow attempt-specific naming .<release_id>.upload-*: %s\n' "$sname" >&2
  exit 1
fi

manifest_file="$staging_canon/release-manifest.json"
bundle_file="$staging_canon/release-manifest.bundle"

[[ -f "$manifest_file" ]] || { printf 'Error: release-manifest.json missing in staging directory: %s\n' "$staging_canon" >&2; exit 1; }
[[ -f "$bundle_file" ]] || { printf 'Error: release-manifest.bundle missing in staging directory: %s\n' "$staging_canon" >&2; exit 1; }

release_id="$(python3 - "$manifest_file" <<'PY'
import json, sys
try:
    data = json.load(open(sys.argv[1], encoding='utf-8'))
    print(data.get('release_id', ''))
except Exception:
    sys.exit(1)
PY
)"

[[ -n "$release_id" && "$release_id" =~ ^[A-Za-z0-9._-]{1,128}$ ]] || {
  printf 'Error: invalid or missing release_id in manifest\n' >&2
  exit 1
}

release_target_name="$(basename "$RELEASE_DIR")"
[[ "$release_target_name" == "$release_id" ]] || {
  printf 'Error: release directory target %s does not match manifest release_id %s\n' "$release_target_name" "$release_id" >&2
  exit 1
}

if [[ "$sname" != ".${release_id}.upload-"* ]]; then
  printf 'Error: staging directory %s does not match release_id prefix .%s.upload-*\n' "$sname" "$release_id" >&2
  exit 1
fi

verify_cosign_signature() {
  local target_dir="$1"
  local m_path="$target_dir/release-manifest.json"
  local b_path="$target_dir/release-manifest.bundle"

  if [[ "$SKIP_COSIGN" -eq 1 ]]; then
    return 0
  fi

  if [[ -z "$EXPECTED_IDENTITY" && "$REQUIRE_COSIGN" -eq 0 ]]; then
    return 0
  fi

  if ! command -v cosign >/dev/null 2>&1; then
    printf 'Error: cosign executable not found in PATH\n' >&2
    return 1
  fi

  cosign verify-blob \
    --bundle "$b_path" \
    --certificate-identity "$EXPECTED_IDENTITY" \
    --certificate-oidc-issuer "$EXPECTED_ISSUER" \
    "$m_path" >/dev/null
}

verify_artifacts() {
  local target_dir="$1"
  local m_path="$2"

  python3 - "$target_dir" "$m_path" <<'PYEOF'
import hashlib, json, pathlib, re, sys

root = pathlib.Path(sys.argv[1]).resolve(strict=True)
manifest_path = pathlib.Path(sys.argv[2]).resolve(strict=True)

try:
    manifest = json.loads(manifest_path.read_text(encoding='utf-8'))
except Exception as e:
    sys.stderr.write(f"Invalid JSON in manifest {manifest_path}: {e}\n")
    sys.exit(1)

artifacts = manifest.get("artifacts")
if not isinstance(artifacts, dict) or not artifacts:
    sys.stderr.write(f"No artifacts dictionary in manifest {manifest_path}\n")
    sys.exit(1)

for name, digest in artifacts.items():
    if name in ("compose_bundle", "compose_bundle_sha256", "failover-bundle", "failover_bundle_sha256"):
        continue
    relative = pathlib.PurePosixPath(name)
    if relative.is_absolute() or ".." in relative.parts or not re.fullmatch(r"[0-9a-f]{64}", digest):
        sys.stderr.write(f"Invalid artifact entry: {name}\n")
        sys.exit(1)
    file_path = root.joinpath(*relative.parts)
    if file_path.is_symlink() or not file_path.is_file():
        sys.stderr.write(f"Missing or symlink artifact: {name}\n")
        sys.exit(1)
    try:
        resolved = file_path.resolve(strict=True)
    except Exception as e:
        sys.stderr.write(f"Cannot resolve artifact {name}: {e}\n")
        sys.exit(1)
    try:
        is_sub = resolved.is_relative_to(root)
    except AttributeError:
        is_sub = str(resolved).startswith(str(root) + "/") or str(resolved) == str(root)
    if not is_sub:
        sys.stderr.write(f"Artifact escapes root directory: {name}\n")
        sys.exit(1)
    actual_hash = hashlib.sha256(file_path.read_bytes()).hexdigest()
    if actual_hash != digest:
        sys.stderr.write(f"Artifact hash mismatch for {name}: expected {digest}, got {actual_hash}\n")
        sys.exit(1)
PYEOF
}

# 1. Verify candidate staging directory integrity and signature
if ! verify_cosign_signature "$staging_canon"; then
  printf 'Error: candidate manifest signature verification failed for %s\n' "$staging_canon" >&2
  exit 1
fi

if ! verify_artifacts "$staging_canon" "$manifest_file"; then
  printf 'Error: candidate artifact verification failed for %s\n' "$staging_canon" >&2
  exit 1
fi

# 2. Check if destination release already exists
if [[ -e "$RELEASE_DIR" || -L "$RELEASE_DIR" ]]; then
  if [[ ! -d "$RELEASE_DIR" || -L "$RELEASE_DIR" ]]; then
    printf 'RELEASE_ID_COLLISION: existing release path is not a directory: %s\n' "$RELEASE_DIR" >&2
    exit 1
  fi

  rel_manifest="$RELEASE_DIR/release-manifest.json"
  rel_bundle="$RELEASE_DIR/release-manifest.bundle"

  if [[ ! -f "$rel_manifest" || ! -f "$rel_bundle" ]]; then
    printf 'RELEASE_ID_COLLISION: existing release missing manifest or bundle: %s\n' "$RELEASE_DIR" >&2
    exit 1
  fi

  if ! verify_cosign_signature "$RELEASE_DIR"; then
    printf 'RELEASE_ID_COLLISION: existing release manifest signature is invalid: %s\n' "$RELEASE_DIR" >&2
    exit 1
  fi

  if ! cmp -s "$manifest_file" "$rel_manifest" || ! cmp -s "$bundle_file" "$rel_bundle"; then
    printf 'RELEASE_ID_COLLISION: signed bundle differs from existing release: %s\n' "$RELEASE_DIR" >&2
    exit 1
  fi

  if ! verify_artifacts "$RELEASE_DIR" "$manifest_file"; then
    printf 'RELEASE_ID_COLLISION: existing release artifact hash mismatch or missing artifact: %s\n' "$RELEASE_DIR" >&2
    exit 1
  fi

  printf 'Existing release %s is identical and verified. Reusing existing release.\n' "$release_id"
  exit 0
else
  # 3. Release absent: install atomically
  mkdir -p "$(dirname "$RELEASE_DIR")"
  if ! mv -T "$staging_canon" "$RELEASE_DIR" 2>/dev/null; then
    mv "$staging_canon" "$RELEASE_DIR"
  fi
  installed=1
  chmod -R go-w "$RELEASE_DIR" 2>/dev/null || true
  printf 'Successfully installed release %s to %s\n' "$release_id" "$RELEASE_DIR"
  exit 0
fi
