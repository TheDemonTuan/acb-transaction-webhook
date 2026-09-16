#!/usr/bin/env bash
# Validate staged release acceptance uses the exact production bundle layout.
set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR="$(cd -- "$SCRIPT_DIR/.." && pwd)"
REPO_ROOT="$(cd -- "$DEPLOY_DIR/.." && pwd)"

grep -Fq 'verify-staged-release.sh --deploy-dir deploy' "$REPO_ROOT/.github/workflows/deploy.yml" || {
  printf 'FAIL: deploy workflow does not accept the staged bundle before SSH\n' >&2
  exit 1
}
grep -Fq 'name: Accept exact signed staged release before SSH' "$REPO_ROOT/.github/workflows/deploy.yml" || {
  printf 'FAIL: acceptance gate is not ordered before SSH setup\n' >&2
  exit 1
}

grep -Fq 'compose_prod config --quiet' "$DEPLOY_DIR/verify-staged-release.sh" || {
  printf 'FAIL: staged acceptance does not render production Compose\n' >&2
  exit 1
}
grep -Fq 'RELEASE_CONTEXT_DIR="$DEPLOY_DIR"' "$DEPLOY_DIR/verify-staged-release.sh" || {
  printf 'FAIL: staged acceptance does not use release-root project directory\n' >&2
  exit 1
}

# The complete behavioral render is covered by the production-layout regression.
bash "$SCRIPT_DIR/test_staged_release_compose.sh"
printf 'PASS: staged release bundle runtime contract\n'
