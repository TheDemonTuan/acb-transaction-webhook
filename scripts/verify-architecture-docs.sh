#!/usr/bin/env bash
set -euo pipefail

# scripts/verify-architecture-docs.sh
# Verifies repository documentation invariants:
# 1. Canonical specs and architecture documents exist.
# 2. Authoritative architecture does NOT specify a per-app Caddy/cloudflared stack.
# 3. Authoritative architecture specifies Traefik, Blue/Green gateways, singleton worker,
#    auth-browser, TTS, Bark, SQLite WAL, and 3 Docker networks.
# 4. README points to canonical specifications and does not treat older plans as current.
# 5. Tracked historical/older plans have superseded banners.

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

echo "==> Checking canonical architecture documents..."

CANONICAL_SPEC="docs/superpowers/specs/2026-09-14-acb-final-production-invariants.md"
ARCH_DOC="docs/architecture/PRODUCTION_ARCHITECTURE.md"
PLAN_DOC="docs/superpowers/plans/2026-09-14-acb-production-convergence-execution.md"
CONTRACTS_DOC="docs/architecture/COMPATIBILITY_CONTRACTS.md"
INVENTORY_DOC="docs/architecture/SYSTEM_INVENTORY.md"

for doc in "$CANONICAL_SPEC" "$ARCH_DOC" "$PLAN_DOC" "$CONTRACTS_DOC" "$INVENTORY_DOC"; do
  if [[ ! -f "$doc" ]]; then
    echo "ERROR: Missing required canonical document: $doc" >&2
    exit 1
  fi
done

echo "==> Verifying authoritative architecture topology in $ARCH_DOC..."

# Must NOT prescribe a per-app Caddy or per-app cloudflared stack
if grep -iE 'per-app[[:space:]]+(caddy|cloudflared)' "$ARCH_DOC" >/dev/null; then
  echo "ERROR: $ARCH_DOC mentions a forbidden per-app Caddy or cloudflared production stack." >&2
  exit 1
fi

# Must name all required platform components
required_components=(
  "Traefik"
  "Blue/Green"
  "acb-worker"
  "acb-auth-browser"
  "acb-tts-gateway"
  "acb-bark"
  "SQLite"
  "edge-acb"
  "acb-core"
  "acb-egress"
)

for comp in "${required_components[@]}"; do
  if ! grep -Fq "$comp" "$ARCH_DOC"; then
    echo "ERROR: $ARCH_DOC does not name required component/network: $comp" >&2
    exit 1
  fi
done

echo "==> Verifying README.md references..."

if ! grep -Fq "docs/superpowers/specs/2026-09-14-acb-final-production-invariants.md" README.md; then
  echo "ERROR: README.md does not reference canonical invariant spec." >&2
  exit 1
fi

if ! grep -Fq "docs/architecture/PRODUCTION_ARCHITECTURE.md" README.md; then
  echo "ERROR: README.md does not reference production architecture overview." >&2
  exit 1
fi

if ! grep -Fq "docs/superpowers/plans/2026-09-14-acb-production-convergence-execution.md" README.md; then
  echo "ERROR: README.md does not reference canonical execution plan." >&2
  exit 1
fi

# Ensure README does not describe an older plan as active/current
if grep -iE 'current[[:space:]]+plan.*(caddy|fix\.md|fix_2\.md)' README.md >/dev/null; then
  echo "ERROR: README.md points to an older plan as current." >&2
  exit 1
fi

echo "==> Verifying superseded banners in tracked historical plans..."

historical_docs=(
  "2026-09-12-caddy-progressive-blue-green-deployment.md"
  "2026-09-13-acb-secure-single-vps-platform-migration.md"
  "2026-09-13-single-vps-secure-container-platform-standard.md"
  "fix.md"
  "fix_2.md"
  "fix_3.md"
  "fix_4.md"
  "deploy_fix.md"
)

for hdoc in "${historical_docs[@]}"; do
  if [[ -f "$hdoc" ]]; then
    if ! grep -q "SUPERSEDED" "$hdoc"; then
      echo "ERROR: Historical document $hdoc lacks SUPERSEDED banner." >&2
      exit 1
    fi
  fi
done

echo "==> All architecture and documentation verification checks passed successfully."
exit 0
