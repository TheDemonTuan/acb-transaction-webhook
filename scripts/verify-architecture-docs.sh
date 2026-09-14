#!/usr/bin/env bash
set -euo pipefail

# scripts/verify-architecture-docs.sh
# Verifies repository documentation invariants and enforces strict CI guardrails:
# 1. Canonical specs, architecture documents, and ops runbooks exist and are non-empty.
# 2. Authoritative architecture does NOT specify a per-app Caddy/cloudflared stack.
# 3. Authoritative architecture specifies Traefik, Blue/Green gateways, singleton worker,
#    auth-browser, TTS, Bark, SQLite WAL, and 3 Docker networks.
# 4. README points to canonical specifications and operations runbooks.
# 5. Obsolete deployment entrypoints (deploy-warm.sh, --upgrade-core) are hard-failed.
# 6. Active documentation does NOT contain forbidden legacy commands or contradictory guidance.
# 7. Historical/superseded documents in docs/archive/ have mandatory SUPERSEDED banners.

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

echo "==> Checking canonical architecture documents & runbooks..."

CANONICAL_DOCS=(
  "docs/superpowers/specs/2026-09-14-acb-final-production-invariants.md"
  "docs/architecture/PRODUCTION_ARCHITECTURE.md"
  "docs/superpowers/plans/2026-09-14-acb-production-convergence-execution.md"
  "docs/architecture/COMPATIBILITY_CONTRACTS.md"
  "docs/architecture/SYSTEM_INVENTORY.md"
  "docs/archive/README.md"
  "docs/runbooks/DEPLOYMENT_RUNBOOK.md"
  "docs/runbooks/FAILOVER_RUNBOOK.md"
  "docs/runbooks/SECRET_PROVISIONING_RUNBOOK.md"
  "docs/runbooks/BACKUP_RUNBOOK.md"
  "docs/runbooks/RESTORE_RUNBOOK.md"
  "docs/runbooks/DISASTER_RECOVERY_RUNBOOK.md"
  "docs/runbooks/OBSERVABILITY.md"
  "docs/runbooks/HANDOFF_RELEASE_CHECKLIST.md"
  "docs/runbooks/OPERATOR_DRILLS_TEMPLATE.md"
)

for doc in "${CANONICAL_DOCS[@]}"; do
  if [[ ! -f "$doc" || ! -s "$doc" ]]; then
    echo "ERROR: Missing or empty canonical document: $doc" >&2
    exit 1
  fi
done

echo "==> Verifying authoritative architecture topology in docs/architecture/PRODUCTION_ARCHITECTURE.md..."
ARCH_DOC="docs/architecture/PRODUCTION_ARCHITECTURE.md"

if grep -iE 'per-app[[:space:]]+(caddy|cloudflared)' "$ARCH_DOC" >/dev/null; then
  echo "ERROR: $ARCH_DOC mentions a forbidden per-app Caddy or cloudflared production stack." >&2
  exit 1
fi

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

echo "==> Verifying README.md references and runbooks..."

for ref in \
  "docs/superpowers/specs/2026-09-14-acb-final-production-invariants.md" \
  "docs/architecture/PRODUCTION_ARCHITECTURE.md" \
  "docs/superpowers/plans/2026-09-14-acb-production-convergence-execution.md" \
  "docs/runbooks/DEPLOYMENT_RUNBOOK.md" \
  "docs/runbooks/FAILOVER_RUNBOOK.md" \
  "docs/runbooks/SECRET_PROVISIONING_RUNBOOK.md" \
  "docs/runbooks/BACKUP_RUNBOOK.md" \
  "docs/runbooks/DISASTER_RECOVERY_RUNBOOK.md"; do
  if ! grep -Fq "$ref" README.md; then
    echo "ERROR: README.md does not reference required canonical doc: $ref" >&2
    exit 1
  fi
done

if grep -iE 'current[[:space:]]+plan.*(caddy|fix\.md|fix_2\.md)' README.md >/dev/null; then
  echo "ERROR: README.md points to an older plan as current." >&2
  exit 1
fi

echo "==> Verifying retirement and hard-fail enforcement of obsolete deploy paths..."

# 1. deploy/deploy-warm.sh must hard-fail safely
if [[ ! -f "deploy/deploy-warm.sh" ]]; then
  echo "ERROR: deploy/deploy-warm.sh must exist as a compatibility wrapper." >&2
  exit 1
fi
if ! grep -q "exit 1" "deploy/deploy-warm.sh"; then
  echo "ERROR: deploy/deploy-warm.sh must unconditionally exit 1." >&2
  exit 1
fi
if ! grep -qiE "(retired|obsolete)" "deploy/deploy-warm.sh"; then
  echo "ERROR: deploy/deploy-warm.sh must state it has been retired/obsolete." >&2
  exit 1
fi

# 2. deploy/deploy.sh must forbid --upgrade-core
if grep -q "upgrade_core=1" "deploy/deploy.sh"; then
  echo "ERROR: deploy/deploy.sh still enables upgrade_core." >&2
  exit 1
fi

# 3. .github/workflows/deploy.yml must not contain unconditional UPGRADE_CORE or obsolete inputs
if grep -q "UPGRADE_CORE" ".github/workflows/deploy.yml"; then
  echo "ERROR: .github/workflows/deploy.yml contains forbidden UPGRADE_CORE reference." >&2
  exit 1
fi
if grep -q "upgrade_core:" ".github/workflows/deploy.yml"; then
  echo "ERROR: .github/workflows/deploy.yml contains forbidden upgrade_core workflow input." >&2
  exit 1
fi

echo "==> Verifying that active docs do NOT instruct forbidden legacy commands..."

ACTIVE_DOCS=(
  "README.md"
  "deploy/README.md"
  "docs/runbooks/DEPLOYMENT_RUNBOOK.md"
  "docs/runbooks/FAILOVER_RUNBOOK.md"
  "docs/runbooks/SECRET_PROVISIONING_RUNBOOK.md"
  "docs/runbooks/DISASTER_RECOVERY_RUNBOOK.md"
)

for doc in "${ACTIVE_DOCS[@]}"; do
  # Disallow active instructions containing UPGRADE_CORE=1
  if grep -E "^[[:space:]]*(\./|bash[[:space:]]+)?deploy/deploy-warm\.sh[[:space:]]+--upgrade-core" "$doc" >/dev/null 2>&1; then
    echo "ERROR: $doc actively recommends forbidden command: deploy-warm.sh --upgrade-core" >&2
    exit 1
  fi
  if grep -E "^[[:space:]]*(\./|bash[[:space:]]+)?deploy/deploy\.sh[[:space:]]+--upgrade-core" "$doc" >/dev/null 2>&1; then
    echo "ERROR: $doc actively recommends forbidden command: deploy.sh --upgrade-core" >&2
    exit 1
  fi
  if grep -iE 'caddy[[:space:]]+reload' "$doc" >/dev/null 2>&1; then
    echo "ERROR: $doc contains forbidden command: caddy reload" >&2
    exit 1
  fi
done

echo "==> Verifying superseded banners in docs/archive/..."

for f in docs/archive/*.md; do
  [[ -f "$f" ]] || continue
  base="$(basename "$f")"
  if [[ "$base" == "README.md" ]]; then
    continue
  fi
  if ! head -n 15 "$f" | grep -q "SUPERSEDED"; then
    echo "ERROR: Archived document $f lacks SUPERSEDED banner in first 15 lines." >&2
    exit 1
  fi
done

echo "==> All architecture, runbook, and retirement verification checks passed successfully."
exit 0
