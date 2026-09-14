#!/usr/bin/env bash
# deploy/deploy-warm.sh
# RETIRED / HARD-FAIL COMPATIBILITY WRAPPER
#
# Monolithic / all-in-one deployment paths have been retired to eliminate
# deployment ambiguity, prevent accidental multi-component restarts, avoid
# shared SQLite WAL corruption, and ensure strict component isolation.
#
# Replacement commands:
#   - Gateway Blue/Green cutover:  deploy/deploy-gateway.sh <gateway-image-digest>
#   - Worker singleton upgrade:    deploy/deploy-worker.sh <worker-image-digest>
#   - Schema database migration:   deploy/deploy-schema.sh <dbtool-image-digest>
#   - Auxiliary sidecars:          deploy/deploy-auth-browser.sh | deploy/deploy-tts.sh | deploy/deploy-bark.sh
#   - Bounded rollout dispatcher:  deploy/dispatch-rollout.sh --manifest <manifest.json> ...
set -euo pipefail

echo "[ERROR] deploy/deploy-warm.sh is obsolete and has been retired." >&2
echo "[ERROR] Monolithic core upgrades (--upgrade-core / UPGRADE_CORE) are strictly forbidden." >&2
echo "[ERROR] Please execute component transaction scripts or dispatch-rollout.sh:" >&2
echo "        - Gateway: ./deploy/deploy-gateway.sh <gateway-image-digest>" >&2
echo "        - Worker:  ./deploy/deploy-worker.sh <worker-image-digest>" >&2
echo "        - Schema:  ./deploy/deploy-schema.sh <dbtool-image-digest>" >&2
echo "        - Rollout: ./deploy/dispatch-rollout.sh --manifest <manifest.json> ..." >&2
exit 1
